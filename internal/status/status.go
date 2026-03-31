package status

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"strings"
	"time"

	"pr-agent-go/internal/config"
	"pr-agent-go/internal/github"
	"pr-agent-go/internal/processor"
	"pr-agent-go/internal/review"
	"pr-agent-go/internal/storage"
)

type Check struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

type ConfigSummary struct {
	Port           int    `json:"port"`
	DataDir        string `json:"dataDir"`
	WorkerCount    int    `json:"workerCount"`
	QueueSize      int    `json:"queueSize"`
	GitHubBaseURL  string `json:"githubBaseUrl"`
	Model          string `json:"model"`
	WebhookSecured bool   `json:"webhookSecured"`
}

type ReviewMetrics struct {
	Total       int            `json:"total"`
	ByRisk      map[string]int `json:"byRisk"`
	ByStatus    map[string]int `json:"byStatus"`
	LatestRepo  string         `json:"latestRepo,omitempty"`
	LatestPR    int            `json:"latestPr,omitempty"`
	LatestRunAt string         `json:"latestRunAt,omitempty"`
}

type EventMetrics struct {
	Total    int            `json:"total"`
	ByStatus map[string]int `json:"byStatus"`
}

type Overview struct {
	Now           string               `json:"now"`
	Service       string               `json:"service"`
	Config        ConfigSummary        `json:"config"`
	Checks        []Check              `json:"checks"`
	Queue         processor.Snapshot   `json:"queue"`
	Daily         storage.DailySummary `json:"daily"`
	ReviewMetrics ReviewMetrics        `json:"reviewMetrics"`
	EventMetrics  EventMetrics         `json:"eventMetrics"`
	RecentRuns    []storage.ReviewRun  `json:"recentRuns"`
}

type CheckCache struct {
	cfg         config.Config
	mu          sync.RWMutex
	checks      []Check
	refreshedAt time.Time
}

func NewCheckCache(cfg config.Config) *CheckCache {
	return &CheckCache{
		cfg: cfg,
		checks: []Check{
			checkServiceConfig(cfg),
			{Name: "github", OK: false, Message: "status check pending"},
			{Name: "model", OK: false, Message: "status check pending"},
		},
	}
}

func (c *CheckCache) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}

	c.refresh()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.refresh()
			}
		}
	}()
}

func (c *CheckCache) Snapshot() []Check {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]Check, len(c.checks))
	copy(out, c.checks)
	return out
}

func RunChecks(cfg config.Config) []Check {
	results := make([]Check, 3)
	results[0] = checkServiceConfig(cfg)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		results[1] = checkGitHub(cfg)
	}()

	go func() {
		defer wg.Done()
		results[2] = checkModel(cfg)
	}()

	wg.Wait()
	return results
}

func BuildOverview(cfg config.Config, store *storage.FileStorage, queue processor.Snapshot, checks []Check) (Overview, error) {
	daily, err := store.DailySummary(time.Now())
	if err != nil {
		return Overview{}, err
	}

	runs, err := store.ListReviewRuns()
	if err != nil {
		return Overview{}, err
	}

	logs, err := store.ListEventLogs()
	if err != nil {
		return Overview{}, err
	}

	reviewMetrics := ReviewMetrics{
		Total:    len(runs),
		ByRisk:   map[string]int{"low": 0, "medium": 0, "high": 0, "unknown": 0},
		ByStatus: map[string]int{},
	}
	for _, run := range runs {
		if _, ok := reviewMetrics.ByRisk[run.OverallRisk]; ok {
			reviewMetrics.ByRisk[run.OverallRisk]++
		} else {
			reviewMetrics.ByRisk["unknown"]++
		}
		reviewMetrics.ByStatus[run.Status]++
		if run.CreatedAt >= reviewMetrics.LatestRunAt {
			reviewMetrics.LatestRunAt = run.CreatedAt
			reviewMetrics.LatestRepo = run.RepoFullName
			reviewMetrics.LatestPR = run.PRNumber
		}
	}

	eventMetrics := EventMetrics{
		Total:    len(logs),
		ByStatus: map[string]int{},
	}
	for _, log := range logs {
		eventMetrics.ByStatus[log.ProcessStatus]++
	}

	recentRuns := make([]storage.ReviewRun, 0, 5)
	for i := len(runs) - 1; i >= 0 && len(recentRuns) < 5; i-- {
		recentRuns = append(recentRuns, runs[i])
	}

	return Overview{
		Now:     time.Now().UTC().Format(time.RFC3339),
		Service: "pr-agent-go",
		Config: ConfigSummary{
			Port:           cfg.Port,
			DataDir:        cfg.DataDir,
			WorkerCount:    cfg.Server.WorkerCount,
			QueueSize:      cfg.Server.QueueSize,
			GitHubBaseURL:  cfg.GitHub.APIBaseURL,
			Model:          cfg.LLM.Model,
			WebhookSecured: cfg.GitHub.WebhookSecret != "",
		},
		Checks:        checks,
		Queue:         queue,
		Daily:         daily,
		ReviewMetrics: reviewMetrics,
		EventMetrics:  eventMetrics,
		RecentRuns:    recentRuns,
	}, nil
}

func FetchRemoteOverview(port int) (Overview, error) {
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/internal/status", port))
	if err != nil {
		return Overview{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return Overview{}, fmt.Errorf("status endpoint %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var overview Overview
	if err := json.NewDecoder(resp.Body).Decode(&overview); err != nil {
		return Overview{}, err
	}
	return overview, nil
}

func (c *CheckCache) refresh() {
	checks := RunChecks(c.cfg)
	c.mu.Lock()
	c.checks = checks
	c.refreshedAt = time.Now()
	c.mu.Unlock()
}

func checkServiceConfig(cfg config.Config) Check {
	if cfg.Port <= 0 || cfg.Server.WorkerCount <= 0 || cfg.Server.QueueSize <= 0 {
		return Check{Name: "service_config", OK: false, Message: "invalid port or worker/queue configuration"}
	}
	return Check{Name: "service_config", OK: true, Message: fmt.Sprintf("port=%d workers=%d queue=%d", cfg.Port, cfg.Server.WorkerCount, cfg.Server.QueueSize)}
}

func checkGitHub(cfg config.Config) Check {
	client := github.New(cfg.GitHub.APIBaseURL, cfg.GitHub.Token, cfg.GitHub.ReviewCommentMarker)
	client.HTTPClient = &http.Client{Timeout: 2 * time.Second}
	if err := client.CheckConnection(); err != nil {
		return Check{Name: "github", OK: false, Message: err.Error()}
	}
	return Check{Name: "github", OK: true, Message: "GitHub API reachable and token accepted"}
}

func checkModel(cfg config.Config) Check {
	agent := review.NewAgent(cfg.LLM.APIBaseURL, cfg.LLM.APIKey, cfg.LLM.Model)
	agent.HTTPClient = &http.Client{Timeout: 2 * time.Second}
	if err := agent.CheckConnection(); err != nil {
		return Check{Name: "model", OK: false, Message: err.Error()}
	}
	return Check{Name: "model", OK: true, Message: "model endpoint reachable"}
}
