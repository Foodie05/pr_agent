package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"pr-agent-go/internal/config"
	"pr-agent-go/internal/storage"
)

func runReviewPR(cfg config.Config, store *storage.FileStorage, args []string, jsonOutput bool) error {
	repoFullName, prNumber, err := parseRepoAndPR(args, "review")
	if err != nil {
		return err
	}

	service := newService(cfg, store)
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

	service := newService(cfg, store)
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

func runRecheckConflict(cfg config.Config, store *storage.FileStorage, args []string, jsonOutput bool) error {
	repoFullName, prNumber, err := parseRepoAndPR(args, "recheck")
	if err != nil {
		return err
	}

	service := newService(cfg, store)
	progress := func(stage string, details string) {
		fmt.Fprintf(os.Stderr, "[%s] %s: %s\n", time.Now().Format(time.RFC3339), stage, details)
	}

	result, err := service.RecheckConflict(repoFullName, prNumber, "manual_recheck_cli", progress)
	if err != nil {
		return err
	}

	if jsonOutput {
		return printJSON(result)
	}

	fmt.Printf("Rechecked %s #%d\n", result.RepoFullName, result.PRNumber)
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

	client := newService(cfg, nil).GitHub
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
