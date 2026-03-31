package main

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"pr-agent-go/internal/config"
	"pr-agent-go/internal/conflict"
	"pr-agent-go/internal/github"
	"pr-agent-go/internal/orchestrator"
	"pr-agent-go/internal/processor"
	"pr-agent-go/internal/review"
	"pr-agent-go/internal/status"
	"pr-agent-go/internal/storage"
)

func main() {
	cfg := config.Load()

	store, err := storage.New(cfg.DataDir)
	if err != nil {
		log.Fatalf("init storage: %v", err)
	}

	switch command := resolveCommand(os.Args[1:]); command {
	case "serve":
		if err := runServer(cfg, store); err != nil {
			log.Fatal(err)
		}
	case "status":
		if err := runStatus(cfg, store, hasJSONFlag(os.Args[1:])); err != nil {
			log.Fatal(err)
		}
	case "stats":
		if err := runStats(cfg, store, hasJSONFlag(os.Args[1:])); err != nil {
			log.Fatal(err)
		}
	case "doctor":
		if err := runDoctor(cfg, hasJSONFlag(os.Args[1:])); err != nil {
			log.Fatal(err)
		}
	case "logs":
		if err := runLogs(cfg, store, hasJSONFlag(os.Args[1:])); err != nil {
			log.Fatal(err)
		}
	case "review":
		if err := runReviewPR(cfg, store, os.Args[1:], hasJSONFlag(os.Args[1:])); err != nil {
			log.Fatal(err)
		}
	case "check":
		if err := runIntervenePR(cfg, store, os.Args[1:], hasJSONFlag(os.Args[1:])); err != nil {
			log.Fatal(err)
		}
	case "add":
		if err := runRegisterWebhook(cfg, os.Args[1:], hasJSONFlag(os.Args[1:])); err != nil {
			log.Fatal(err)
		}
	default:
		printUsage()
		os.Exit(1)
	}
}

func resolveCommand(args []string) string {
	if len(args) == 0 {
		return "serve"
	}
	switch args[0] {
	case "serve", "status", "stats", "doctor", "logs", "review", "check", "add":
		return args[0]
	case "review-pr":
		return "review"
	case "intervene-pr":
		return "check"
	case "register-webhook":
		return "add"
	default:
		return args[0]
	}
}

func hasJSONFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--json" {
			return true
		}
	}
	return false
}

func runServer(cfg config.Config, store *storage.FileStorage) error {
	service := &orchestrator.Service{
		Storage:             store,
		GitHub:              github.New(cfg.GitHub.APIBaseURL, cfg.GitHub.Token, cfg.GitHub.ReviewCommentMarker),
		Agent:               review.NewAgent(cfg.LLM.APIBaseURL, cfg.LLM.APIKey, cfg.LLM.Model),
		ConflictResolver:    conflict.NewGitResolver(cfg.GitHub.Token, cfg.Git.TempDir, cfg.Git.UserName, cfg.Git.UserEmail, review.NewAgent(cfg.LLM.APIBaseURL, cfg.LLM.APIKey, cfg.LLM.Model)),
		ReviewCommentMarker: cfg.GitHub.ReviewCommentMarker,
	}
	queue := processor.New(service, cfg.Server.WorkerCount, cfg.Server.QueueSize)
	statusCache := status.NewCheckCache(cfg)
	statusCache.Start(context.Background(), 30*time.Second)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"service": "pr-agent-go",
			"now":     time.Now().UTC().Format(time.RFC3339),
		})
	})

	mux.HandleFunc("/internal/daily-summary", func(w http.ResponseWriter, r *http.Request) {
		summary, err := store.DailySummary(time.Now())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal_error", "message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, summary)
	})

	mux.HandleFunc("/internal/status", func(w http.ResponseWriter, r *http.Request) {
		overview, err := status.BuildOverview(cfg, store, queue.Snapshot(), statusCache.Snapshot())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal_error", "message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, overview)
	})

	mux.HandleFunc("/webhooks/github", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
			return
		}

		if !verifyGitHubSignature(body, r.Header.Get("X-Hub-Signature-256"), cfg.GitHub.WebhookSecret) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_signature"})
			return
		}

		if r.Header.Get("X-GitHub-Event") == "ping" {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":      true,
				"message": "pong",
			})
			return
		}

		decodedPayload, err := decodeGitHubPayload(body, r.Header.Get("Content-Type"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
			return
		}

		var payload orchestrator.PullRequestEvent
		if err := json.Unmarshal(decodedPayload, &payload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
			return
		}

		envelope := orchestrator.WebhookEnvelope{
			EventName:  r.Header.Get("X-GitHub-Event"),
			DeliveryID: firstNonEmpty(r.Header.Get("X-GitHub-Delivery"), time.Now().UTC().Format("20060102150405.000000000")),
			Payload:    payload,
		}
		if err := queue.Enqueue(envelope); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error":   "queue_full",
				"message": "webhook queued capacity reached",
			})
			return
		}

		writeJSON(w, http.StatusAccepted, map[string]any{
			"accepted":   true,
			"deliveryId": envelope.DeliveryID,
			"queued":     queue.Snapshot().Queued,
		})
	})

	log.Printf("pr-agent-go listening on :%d dataDir=%s llmEnabled=%t workers=%d queue=%d", cfg.Port, cfg.DataDir, cfg.LLM.APIKey != "", cfg.Server.WorkerCount, cfg.Server.QueueSize)
	return http.ListenAndServe(":"+strconv.Itoa(cfg.Port), mux)
}

func runStatus(cfg config.Config, store *storage.FileStorage, jsonOutput bool) error {
	overview, err := status.FetchRemoteOverview(cfg.Port)
	if err != nil {
		overview, err = status.BuildOverview(cfg, store, processor.Snapshot{}, status.RunChecks(cfg))
		if err != nil {
			return err
		}
	}

	if jsonOutput {
		return printJSON(overview)
	}

	fmt.Printf("Service: %s\n", overview.Service)
	fmt.Printf("Time: %s\n", overview.Now)
	fmt.Printf("Port: %d\n", overview.Config.Port)
	fmt.Printf("Workers: %d\n", overview.Config.WorkerCount)
	fmt.Printf("Queue: queued=%d active=%d completed=%d failed=%d\n", overview.Queue.Queued, overview.Queue.Active, overview.Queue.Completed, overview.Queue.Failed)
	fmt.Printf("GitHub: %s\n", summarizeCheck(overview.Checks, "github"))
	fmt.Printf("Model: %s\n", summarizeCheck(overview.Checks, "model"))
	fmt.Printf("Webhook Secret: %t\n", overview.Config.WebhookSecured)
	fmt.Printf("Today: reviews=%d failed_events=%d repos=%s\n", overview.Daily.ReviewCount, overview.Daily.FailedEvents, strings.Join(overview.Daily.Repos, ", "))
	if overview.ReviewMetrics.LatestRunAt != "" {
		fmt.Printf("Latest PR: %s #%d at %s\n", overview.ReviewMetrics.LatestRepo, overview.ReviewMetrics.LatestPR, overview.ReviewMetrics.LatestRunAt)
	}
	if len(overview.Queue.RecentFailures) > 0 {
		fmt.Printf("Recent Failures:\n")
		for _, failure := range overview.Queue.RecentFailures {
			fmt.Printf("- %s\n", failure)
		}
	}
	return nil
}

func runStats(cfg config.Config, store *storage.FileStorage, jsonOutput bool) error {
	overview, err := status.FetchRemoteOverview(cfg.Port)
	if err != nil {
		overview, err = status.BuildOverview(cfg, store, processor.Snapshot{}, status.RunChecks(cfg))
		if err != nil {
			return err
		}
	}

	if jsonOutput {
		return printJSON(map[string]any{
			"daily":         overview.Daily,
			"reviewMetrics": overview.ReviewMetrics,
			"eventMetrics":  overview.EventMetrics,
			"queue":         overview.Queue,
			"recentRuns":    overview.RecentRuns,
		})
	}

	fmt.Printf("Daily Reviews: %d\n", overview.Daily.ReviewCount)
	fmt.Printf("Risk Counts: low=%d medium=%d high=%d unknown=%d\n",
		overview.Daily.RiskCounts["low"],
		overview.Daily.RiskCounts["medium"],
		overview.Daily.RiskCounts["high"],
		overview.Daily.RiskCounts["unknown"],
	)
	fmt.Printf("All-time Reviews: %d\n", overview.ReviewMetrics.Total)
	fmt.Printf("Event Statuses: %s\n", formatCountMap(overview.EventMetrics.ByStatus))
	fmt.Printf("Review Statuses: %s\n", formatCountMap(overview.ReviewMetrics.ByStatus))
	fmt.Printf("Queue: queued=%d active=%d completed=%d failed=%d\n", overview.Queue.Queued, overview.Queue.Active, overview.Queue.Completed, overview.Queue.Failed)
	if len(overview.RecentRuns) > 0 {
		fmt.Printf("Recent Runs:\n")
		for _, run := range overview.RecentRuns {
			fmt.Printf("- %s #%d risk=%s trust=%s action=%s/%s provider=%s at %s\n",
				run.RepoFullName, run.PRNumber, run.OverallRisk, emptyDash(run.TrustLevel), emptyDash(run.ActionTaken), emptyDash(run.ActionStatus), run.Provider, run.CreatedAt)
		}
	}
	return nil
}

func runDoctor(cfg config.Config, jsonOutput bool) error {
	checks := status.RunChecks(cfg)
	if jsonOutput {
		return printJSON(checks)
	}

	for _, check := range checks {
		state := "FAIL"
		if check.OK {
			state = "OK"
		}
		fmt.Printf("%s %s: %s\n", state, check.Name, check.Message)
	}
	return nil
}

func runLogs(cfg config.Config, store *storage.FileStorage, jsonOutput bool) error {
	eventLogs, err := store.ListEventLogs()
	if err != nil {
		return err
	}
	reviewRuns, err := store.ListReviewRuns()
	if err != nil {
		return err
	}

	recentEvents := tailEvents(eventLogs, 10)
	recentRuns := tailRuns(reviewRuns, 10)

	if jsonOutput {
		return printJSON(map[string]any{
			"recentEvents": recentEvents,
			"recentRuns":   recentRuns,
		})
	}

	fmt.Println("Recent Event Logs:")
	if len(recentEvents) == 0 {
		fmt.Println("- none")
	} else {
		for _, logEntry := range recentEvents {
			fmt.Printf("- %s %s repo=%s status=%s reason=%s error=%s\n",
				logEntry.ReceivedAt, logEntry.EventName, logEntry.RepoFullName, logEntry.ProcessStatus, emptyDash(logEntry.Reason), emptyDash(logEntry.ErrorMessage))
		}
	}

	fmt.Println("Recent Review Runs:")
	if len(recentRuns) == 0 {
		fmt.Println("- none")
	} else {
		for _, run := range recentRuns {
			fmt.Printf("- %s %s #%d risk=%s trust=%s action=%s/%s trigger=%s timings=%s\n",
				run.CreatedAt,
				run.RepoFullName,
				run.PRNumber,
				run.OverallRisk,
				emptyDash(run.TrustLevel),
				emptyDash(run.ActionTaken),
				emptyDash(run.ActionStatus),
				run.TriggerEvent,
				formatStageDurations(run.StageDurationsMS),
			)
		}
	}
	return nil
}

func runReviewPR(cfg config.Config, store *storage.FileStorage, args []string, jsonOutput bool) error {
	repoFullName, prNumber, err := parseRepoAndPR(args, "review")
	if err != nil {
		return err
	}

	service := &orchestrator.Service{
		Storage:             store,
		GitHub:              github.New(cfg.GitHub.APIBaseURL, cfg.GitHub.Token, cfg.GitHub.ReviewCommentMarker),
		Agent:               review.NewAgent(cfg.LLM.APIBaseURL, cfg.LLM.APIKey, cfg.LLM.Model),
		ConflictResolver:    conflict.NewGitResolver(cfg.GitHub.Token, cfg.Git.TempDir, cfg.Git.UserName, cfg.Git.UserEmail, review.NewAgent(cfg.LLM.APIBaseURL, cfg.LLM.APIKey, cfg.LLM.Model)),
		ReviewCommentMarker: cfg.GitHub.ReviewCommentMarker,
	}

	progress := func(stage string, details string) {
		fmt.Fprintf(os.Stderr, "[%s] %s: %s\n", time.Now().Format(time.RFC3339), stage, details)
	}

	result, err := service.ReviewPullWithProgress(repoFullName, prNumber, "manual_cli", progress)
	if err != nil {
		return err
	}

	if jsonOutput {
		return printJSON(result)
	}

	if result.Ignored {
		fmt.Printf("Ignored: %s\n", result.Reason)
		return nil
	}

	fmt.Printf("Reviewed %s #%d\n", result.RepoFullName, result.PRNumber)
	fmt.Printf("Head SHA: %s\n", result.HeadSHA)
	fmt.Printf("Risk: %s\n", result.OverallRisk)
	fmt.Printf("Trust: %s\n", emptyDash(result.TrustLevel))
	fmt.Printf("Action: %s (%s)\n", emptyDash(result.ActionTaken), emptyDash(result.ActionStatus))
	if result.ActionDetails != "" {
		fmt.Printf("Action Details: %s\n", result.ActionDetails)
	}
	fmt.Printf("Stage Timings: %s\n", formatStageDurations(result.StageDurationsMS))
	return nil
}

func runIntervenePR(cfg config.Config, store *storage.FileStorage, args []string, jsonOutput bool) error {
	repoFullName, prNumber, err := parseRepoAndPR(args, "check")
	if err != nil {
		return err
	}

	note, err := resolveInterventionNote(args)
	if err != nil {
		return err
	}
	if strings.TrimSpace(note) == "" {
		return fmt.Errorf("intervention note is required")
	}

	service := &orchestrator.Service{
		Storage:             store,
		GitHub:              github.New(cfg.GitHub.APIBaseURL, cfg.GitHub.Token, cfg.GitHub.ReviewCommentMarker),
		Agent:               review.NewAgent(cfg.LLM.APIBaseURL, cfg.LLM.APIKey, cfg.LLM.Model),
		ConflictResolver:    conflict.NewGitResolver(cfg.GitHub.Token, cfg.Git.TempDir, cfg.Git.UserName, cfg.Git.UserEmail, review.NewAgent(cfg.LLM.APIBaseURL, cfg.LLM.APIKey, cfg.LLM.Model)),
		ReviewCommentMarker: cfg.GitHub.ReviewCommentMarker,
	}

	progress := func(stage string, details string) {
		fmt.Fprintf(os.Stderr, "[%s] %s: %s\n", time.Now().Format(time.RFC3339), stage, details)
	}

	result, err := service.IntervenePull(repoFullName, prNumber, note, "manual_intervention_cli", progress)
	if err != nil {
		return err
	}

	if jsonOutput {
		return printJSON(result)
	}

	fmt.Printf("Checked %s #%d\n", result.RepoFullName, result.PRNumber)
	fmt.Printf("Head SHA: %s\n", result.HeadSHA)
	fmt.Printf("Risk: %s\n", result.OverallRisk)
	fmt.Printf("Trust: %s\n", emptyDash(result.TrustLevel))
	fmt.Printf("Action: %s (%s)\n", emptyDash(result.ActionTaken), emptyDash(result.ActionStatus))
	if result.ActionDetails != "" {
		fmt.Printf("Action Details: %s\n", result.ActionDetails)
	}
	fmt.Printf("Stage Timings: %s\n", formatStageDurations(result.StageDurationsMS))
	return nil
}

func runRegisterWebhook(cfg config.Config, args []string, jsonOutput bool) error {
	repoFullName, err := parseRepoOnly(args, "add")
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.GitHub.WebhookURL) == "" {
		return fmt.Errorf("missing GITHUB_WEBHOOK_URL in environment")
	}
	if strings.TrimSpace(cfg.GitHub.WebhookSecret) == "" {
		return fmt.Errorf("missing GITHUB_WEBHOOK_SECRET in environment")
	}

	client := github.New(cfg.GitHub.APIBaseURL, cfg.GitHub.Token, cfg.GitHub.ReviewCommentMarker)
	hooks, err := client.ListRepositoryWebhooks(repoFullName)
	if err != nil {
		return err
	}

	targetURL := strings.TrimSpace(cfg.GitHub.WebhookURL)
	events := []string{"pull_request"}

	for _, hook := range hooks {
		if strings.TrimSpace(hook.Config.URL) == targetURL {
			updatedHook, updateErr := client.UpdateRepositoryWebhook(repoFullName, hook.ID, targetURL, cfg.GitHub.WebhookSecret, events)
			if updateErr != nil {
				return updateErr
			}
			if jsonOutput {
				return printJSON(map[string]any{
					"ok":         true,
					"action":     "updated",
					"repo":       repoFullName,
					"webhookUrl": targetURL,
					"hookId":     updatedHook.ID,
					"events":     updatedHook.Events,
				})
			}
			fmt.Printf("Updated webhook for %s\n", repoFullName)
			fmt.Printf("Hook ID: %d\n", updatedHook.ID)
			fmt.Printf("URL: %s\n", targetURL)
			fmt.Printf("Events: %s\n", strings.Join(updatedHook.Events, ", "))
			return nil
		}
	}

	createdHook, err := client.CreateRepositoryWebhook(repoFullName, targetURL, cfg.GitHub.WebhookSecret, events)
	if err != nil {
		return err
	}
	if jsonOutput {
		return printJSON(map[string]any{
			"ok":         true,
			"action":     "created",
			"repo":       repoFullName,
			"webhookUrl": targetURL,
			"hookId":     createdHook.ID,
			"events":     createdHook.Events,
		})
	}
	fmt.Printf("Created webhook for %s\n", repoFullName)
	fmt.Printf("Hook ID: %d\n", createdHook.ID)
	fmt.Printf("URL: %s\n", targetURL)
	fmt.Printf("Events: %s\n", strings.Join(createdHook.Events, ", "))
	return nil
}

func printUsage() {
	fmt.Println("Usage:")
	fmt.Println("  pr-agent-go serve")
	fmt.Println("  pr-agent-go status [--json]")
	fmt.Println("  pr-agent-go stats [--json]")
	fmt.Println("  pr-agent-go doctor [--json]")
	fmt.Println("  pr-agent-go logs [--json]")
	fmt.Println("  pr-agent-go review owner/repo pr_number [--json]")
	fmt.Println("  pr-agent-go review https://github.com/owner/repo/pull/123 [--json]")
	fmt.Println("  pr-agent-go check owner/repo pr_number [--note \"...\" | stdin] [--json]")
	fmt.Println("  pr-agent-go check https://github.com/owner/repo/pull/123 [--note \"...\" | stdin] [--json]")
	fmt.Println("  pr-agent-go add owner/repo [--json]")
	fmt.Println("  pr-agent-go add https://github.com/owner/repo [--json]")
}

func summarizeCheck(checks []status.Check, name string) string {
	for _, check := range checks {
		if check.Name == name {
			if check.OK {
				return "OK - " + check.Message
			}
			return "FAIL - " + check.Message
		}
	}
	return "unknown"
}

func formatCountMap(values map[string]int) string {
	parts := make([]string, 0, len(values))
	for key, value := range values {
		parts = append(parts, fmt.Sprintf("%s=%d", key, value))
	}
	return strings.Join(parts, " ")
}

func tailEvents(items []storage.EventLog, n int) []storage.EventLog {
	if len(items) <= n {
		return items
	}
	return items[len(items)-n:]
}

func tailRuns(items []storage.ReviewRun, n int) []storage.ReviewRun {
	if len(items) <= n {
		return items
	}
	return items[len(items)-n:]
}

func emptyDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func formatStageDurations(values map[string]int64) string {
	if len(values) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	var total int64
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%dms", key, values[key]))
		total += values[key]
	}
	parts = append(parts, fmt.Sprintf("sum=%dms", total))
	return strings.Join(parts, " ")
}

func parseRepoAndPR(args []string, commandName string) (string, int, error) {
	positional := make([]string, 0, len(args))
	for _, arg := range args {
		if !strings.HasPrefix(arg, "--") {
			positional = append(positional, arg)
		}
	}
	if len(positional) >= 2 {
		if repoFullName, prNumber, ok := parseGitHubPRURL(positional[1]); ok {
			return repoFullName, prNumber, nil
		}
	}
	if len(positional) < 3 {
		return "", 0, fmt.Errorf("usage: pr-agent-go %s owner/repo pr_number | https://github.com/owner/repo/pull/123", commandName)
	}

	repoFullName := positional[1]
	prNumber, err := strconv.Atoi(positional[2])
	if err != nil || prNumber <= 0 {
		return "", 0, fmt.Errorf("invalid pr number: %s", positional[2])
	}
	return repoFullName, prNumber, nil
}

func parseRepoOnly(args []string, commandName string) (string, error) {
	positional := make([]string, 0, len(args))
	for _, arg := range args {
		if !strings.HasPrefix(arg, "--") {
			positional = append(positional, arg)
		}
	}
	if len(positional) < 2 {
		return "", fmt.Errorf("usage: pr-agent-go %s owner/repo | https://github.com/owner/repo", commandName)
	}
	if repoFullName, ok := parseGitHubRepoURL(positional[1]); ok {
		return repoFullName, nil
	}
	if strings.Count(positional[1], "/") == 1 {
		return positional[1], nil
	}
	return "", fmt.Errorf("invalid repository: %s", positional[1])
}

func parseGitHubPRURL(raw string) (string, int, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return "", 0, false
	}
	if !strings.EqualFold(parsed.Hostname(), "github.com") {
		return "", 0, false
	}

	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" {
		return "", 0, false
	}

	prNumber, err := strconv.Atoi(parts[3])
	if err != nil || prNumber <= 0 {
		return "", 0, false
	}
	return parts[0] + "/" + parts[1], prNumber, true
}

func parseGitHubRepoURL(raw string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return "", false
	}
	if !strings.EqualFold(parsed.Hostname(), "github.com") {
		return "", false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 2 {
		return "", false
	}
	return parts[0] + "/" + parts[1], true
}

func resolveInterventionNote(args []string) (string, error) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--note" {
			if i+1 >= len(args) {
				return "", fmt.Errorf("missing value for --note")
			}
			return strings.TrimSpace(args[i+1]), nil
		}
	}

	stat, err := os.Stdin.Stat()
	if err == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
		data, readErr := io.ReadAll(os.Stdin)
		if readErr != nil {
			return "", readErr
		}
		return strings.TrimSpace(string(data)), nil
	}

	fmt.Fprint(os.Stderr, "Input intervention note: ")
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func verifyGitHubSignature(body []byte, signatureHeader, secret string) bool {
	if secret == "" {
		return true
	}
	if !strings.HasPrefix(signatureHeader, "sha256=") {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := mac.Sum(nil)

	receivedHex := strings.TrimPrefix(signatureHeader, "sha256=")
	received, err := hex.DecodeString(receivedHex)
	if err != nil {
		return false
	}
	if len(expected) != len(received) {
		return false
	}

	return subtle.ConstantTimeCompare(expected, received) == 1
}

func writeJSON(w http.ResponseWriter, statusCode int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(payload)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func decodeGitHubPayload(body []byte, contentType string) ([]byte, error) {
	normalizedType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch normalizedType {
	case "", "application/json":
		return body, nil
	case "application/x-www-form-urlencoded":
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, err
		}
		payload := values.Get("payload")
		if payload == "" {
			return nil, errors.New("missing payload field")
		}
		return []byte(payload), nil
	default:
		return body, nil
	}
}
