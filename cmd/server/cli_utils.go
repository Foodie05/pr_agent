package main

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"pr-agent-go/internal/status"
	"pr-agent-go/internal/storage"
)

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
	fmt.Println("  pr-agent-go recheck owner/repo pr_number [--json]")
	fmt.Println("  pr-agent-go recheck https://github.com/owner/repo/pull/123 [--json]")
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