package orchestrator

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pr-agent-go/internal/github"
	"pr-agent-go/internal/review"
	"pr-agent-go/internal/storage"
)

func TestHandleWebhookEndToEnd(t *testing.T) {
	t.Helper()

	var createdCommentBody string
	commentCreates := 0
	httpClient := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api/pulls/7":
			return jsonResponse(t, map[string]any{
				"number": 7,
				"title":  "fix: handle empty profile response",
				"body":   "This PR adds a guard for null profile payloads.",
				"draft":  false,
				"user": map[string]any{
					"login": "alice",
				},
				"base": map[string]any{
					"ref": "main",
					"repo": map[string]any{
						"full_name": "acme/api",
					},
				},
				"head": map[string]any{
					"ref": "fix/profile-empty",
					"sha": "abc1234567890",
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api/pulls/7/files":
			return jsonResponse(t, []map[string]any{
				{
					"filename":  "internal/profile/service.go",
					"status":    "modified",
					"additions": 12,
					"deletions": 2,
					"patch":     "@@ -10,7 +10,17 @@ func LoadProfile() {}",
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api/commits/abc1234567890/status":
			return jsonResponse(t, map[string]any{
				"state":    "success",
				"statuses": []any{},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api/issues/7/comments":
			return jsonResponse(t, []any{})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/api/issues/7/comments":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read comment body: %v", err)
			}
			var payload struct {
				Body string `json:"body"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("decode comment payload: %v", err)
			}
			commentCreates++
			if strings.Contains(payload.Body, "<!-- marker -->") {
				createdCommentBody = payload.Body
			}
			return jsonResponse(t, map[string]any{
				"id":   98 + commentCreates,
				"body": payload.Body,
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
			return nil, nil
		}
	})

	dataDir := t.TempDir()
	store, err := storage.New(dataDir)
	if err != nil {
		t.Fatalf("init storage: %v", err)
	}

	service := &Service{
		Storage:             store,
		GitHub:              githubClientWithHTTPClient("https://api.github.test", "test-token", "<!-- marker -->", httpClient),
		Agent:               review.NewAgent("https://unused.example.com", "", "unused-model"),
		ReviewCommentMarker: "<!-- marker -->",
	}

	result, err := service.HandleWebhook(WebhookEnvelope{
		EventName:  "pull_request",
		DeliveryID: "delivery-1",
		Payload: PullRequestEvent{
			Action: "opened",
			Repository: struct {
				FullName string `json:"full_name"`
			}{
				FullName: "acme/api",
			},
			PullRequest: struct {
				Number int `json:"number"`
				Head   struct {
					SHA string `json:"sha"`
				} `json:"head"`
			}{
				Number: 7,
				Head: struct {
					SHA string `json:"sha"`
				}{
					SHA: "abc1234567890",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("handle webhook: %v", err)
	}

	if !result.OK {
		t.Fatalf("expected successful result, got %+v", result)
	}
	if result.OverallRisk != "low" {
		t.Fatalf("expected low risk, got %s", result.OverallRisk)
	}
	if !strings.Contains(createdCommentBody, "<!-- marker -->") {
		t.Fatalf("expected comment marker in body, got %q", createdCommentBody)
	}
	if !strings.Contains(createdCommentBody, "PR Agent Review") {
		t.Fatalf("expected rendered review comment, got %q", createdCommentBody)
	}
	if commentCreates != 2 {
		t.Fatalf("expected 2 created comments, got %d", commentCreates)
	}

	reviewRunsPath := filepath.Join(dataDir, "review-runs.json")
	rawRuns, err := os.ReadFile(reviewRunsPath)
	if err != nil {
		t.Fatalf("read review runs: %v", err)
	}
	var runs []storage.ReviewRun
	if err := json.Unmarshal(rawRuns, &runs); err != nil {
		t.Fatalf("decode review runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 review run, got %d", len(runs))
	}
	if runs[0].CommentID != 99 {
		t.Fatalf("expected comment id 99, got %d", runs[0].CommentID)
	}
	if runs[0].TrustLevel == "" {
		t.Fatalf("expected trust level to be recorded")
	}
	if len(runs[0].StageDurationsMS) == 0 {
		t.Fatalf("expected stage durations to be recorded")
	}
}

func TestDecideTrustLevelIgnoresFindingsWhenRiskAndConfidencePass(t *testing.T) {
	pull := github.Pull{
		Draft: false,
		Mergeable: func() *bool {
			value := true
			return &value
		}(),
	}
	status := github.CommitStatus{
		State:    "success",
		Statuses: []struct{}{},
	}
	result := review.Result{
		OverallRisk:    "low",
		Confidence:     0.95,
		ConfidenceSet:  true,
		MergeReadiness: "ready_for_manual_approval",
		Findings: []review.Finding{
			{File: "README.md", Title: "doc note", Detail: "non-blocking note"},
		},
	}

	if got := decideTrustLevel(pull, status, result); got != trustTrusted {
		t.Fatalf("expected trusted, got %s", got)
	}
}

func TestInterveneMergePublishesFinalComment(t *testing.T) {
	t.Helper()

	commentBodies := []string{}
	httpClient := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api/pulls/8":
			return jsonResponse(t, map[string]any{
				"node_id": "PR_node_8",
				"number":  8,
				"title":   "docs: update readme",
				"body":    "refresh docs",
				"draft":   false,
				"user": map[string]any{
					"login": "alice",
				},
				"base": map[string]any{
					"ref": "main",
					"repo": map[string]any{
						"full_name": "acme/api",
					},
				},
				"head": map[string]any{
					"ref": "docs/readme",
					"sha": "deadbeef1234567",
				},
				"mergeable":       true,
				"mergeable_state": "clean",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api/commits/deadbeef1234567/status":
			return jsonResponse(t, map[string]any{
				"state":    "success",
				"statuses": []any{},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/api/issues/8/comments":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read comment body: %v", err)
			}
			var payload struct {
				Body string `json:"body"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("decode comment payload: %v", err)
			}
			commentBodies = append(commentBodies, payload.Body)
			return jsonResponse(t, map[string]any{
				"id":   100 + len(commentBodies),
				"body": payload.Body,
			})
		case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/api/pulls/8/merge":
			return jsonResponse(t, map[string]any{"merged": true})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
			return nil, nil
		}
	})

	dataDir := t.TempDir()
	store, err := storage.New(dataDir)
	if err != nil {
		t.Fatalf("init storage: %v", err)
	}
	if err := store.SaveReviewRun(storage.ReviewRun{
		RepoFullName:        "acme/api",
		PRNumber:            8,
		HeadSHA:             "deadbeef1234567",
		TriggerEvent:        "manual_cli",
		Status:              "completed",
		Provider:            "heuristic",
		Summary:             "summary",
		OverallRisk:         "low",
		Confidence:          0.9,
		TrustLevel:          trustTrusted,
		MergeReadiness:      "ready_for_manual_approval",
		RenderedCommentBody: "<!-- marker -->\n## PR Agent Review",
		CommentID:           123,
		CreatedAt:           "2026-03-31T12:00:00Z",
	}); err != nil {
		t.Fatalf("seed review run: %v", err)
	}

	service := &Service{
		Storage:             store,
		GitHub:              githubClientWithHTTPClient("https://api.github.test", "test-token", "<!-- marker -->", httpClient),
		Agent:               review.NewAgent("https://unused.example.com", "", "unused-model"),
		ReviewCommentMarker: "<!-- marker -->",
	}

	result, err := service.IntervenePull("acme/api", 8, "merge this PR", "manual_intervention_cli", nil)
	if err != nil {
		t.Fatalf("intervene pull: %v", err)
	}
	if result.ActionStatus != "merged" {
		t.Fatalf("expected merged status, got %s", result.ActionStatus)
	}
	if len(commentBodies) != 1 {
		t.Fatalf("expected exactly one final comment, got %d", len(commentBodies))
	}
	if !strings.Contains(commentBodies[0], "Accepted. Thank you for your contribution!") {
		t.Fatalf("expected final accepted comment, got %q", commentBodies[0])
	}
}

func TestMergeWithRepositoryRulesRetriesAfterApprove(t *testing.T) {
	t.Helper()

	previousDelay := mergeRetryDelayBase
	mergeRetryDelayBase = time.Millisecond
	defer func() { mergeRetryDelayBase = previousDelay }()

	mergeAttempts := 0
	approveCalls := 0
	httpClient := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/api/pulls/9/merge":
			mergeAttempts++
			if mergeAttempts < 3 {
				return &http.Response{
					StatusCode: http.StatusMethodNotAllowed,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{"message":"Repository rule violations found\n\nAt least 1 approving review is required by reviewers with write access.\n\n","status":"405"}`)),
				}, nil
			}
			return jsonResponse(t, map[string]any{"merged": true})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/api/pulls/9/reviews":
			approveCalls++
			return jsonResponse(t, map[string]any{"id": 501})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
			return nil, nil
		}
	})

	service := &Service{
		GitHub: githubClientWithHTTPClient("https://api.github.test", "test-token", "<!-- marker -->", httpClient),
	}

	if err := service.mergeWithRepositoryRules("acme/api", 9, "docs: refresh"); err != nil {
		t.Fatalf("mergeWithRepositoryRules returned error: %v", err)
	}
	if approveCalls != 1 {
		t.Fatalf("expected 1 approve call, got %d", approveCalls)
	}
	if mergeAttempts != 3 {
		t.Fatalf("expected 3 merge attempts, got %d", mergeAttempts)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func githubClientWithHTTPClient(baseURL, token, marker string, transport http.RoundTripper) *github.Client {
	client := github.New(baseURL, token, marker)
	client.HTTPClient = &http.Client{Transport: transport}
	return client
}

func jsonResponse(t *testing.T, value any) (*http.Response, error) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode json response: %v", err)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: io.NopCloser(bytes.NewReader(data)),
	}, nil
}
