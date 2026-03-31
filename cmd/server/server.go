package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
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

func runServer(cfg config.Config, store *storage.FileStorage) error {
	service := newService(cfg, store)
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

func newService(cfg config.Config, store *storage.FileStorage) *orchestrator.Service {
	githubClient := github.New(cfg.GitHub.APIBaseURL, cfg.GitHub.Token, cfg.GitHub.ReviewCommentMarker)
	githubClient.HTTPClient = &http.Client{Timeout: 20 * time.Second}

	agent := review.NewAgent(cfg.LLM.APIBaseURL, cfg.LLM.APIKey, cfg.LLM.Model)
	agent.ReviewBatchSize = cfg.LLM.ReviewBatchSize
	agent.HTTPClient = &http.Client{Timeout: time.Duration(cfg.LLM.RequestTimeoutSecs) * time.Second}

	conflictAgent := review.NewAgent(cfg.LLM.APIBaseURL, cfg.LLM.APIKey, cfg.LLM.Model)
	conflictAgent.HTTPClient = &http.Client{Timeout: time.Duration(cfg.LLM.RequestTimeoutSecs) * time.Second}

	conflictResolver := conflict.NewGitResolver(cfg.GitHub.Token, cfg.Git.TempDir, cfg.Git.UserName, cfg.Git.UserEmail, conflictAgent)
	conflictResolver.StepTimeout = time.Duration(cfg.Git.StepTimeoutSecs) * time.Second
	conflictResolver.ConflictBatchSize = cfg.Git.ConflictBatchSize

	return &orchestrator.Service{
		Storage:             store,
		GitHub:              githubClient,
		Agent:               agent,
		ConflictResolver:    conflictResolver,
		ReviewCommentMarker: cfg.GitHub.ReviewCommentMarker,
	}
}
