package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

type EventLog struct {
	DeliveryID    string `json:"deliveryId"`
	EventName     string `json:"eventName"`
	Action        string `json:"action"`
	RepoFullName  string `json:"repoFullName"`
	ReceivedAt    string `json:"receivedAt"`
	ProcessedAt   string `json:"processedAt,omitempty"`
	ProcessStatus string `json:"processStatus"`
	Reason        string `json:"reason,omitempty"`
	ErrorMessage  string `json:"errorMessage,omitempty"`
}

type ReviewRun struct {
	RepoFullName        string           `json:"repoFullName"`
	PRNumber            int              `json:"prNumber"`
	HeadSHA             string           `json:"headSha"`
	TriggerEvent        string           `json:"triggerEvent"`
	Status              string           `json:"status"`
	Provider            string           `json:"provider"`
	Summary             string           `json:"summary"`
	OverallRisk         string           `json:"overallRisk"`
	Confidence          float64          `json:"confidence"`
	TrustLevel          string           `json:"trustLevel,omitempty"`
	MergeReadiness      string           `json:"mergeReadiness"`
	ActionTaken         string           `json:"actionTaken,omitempty"`
	ActionStatus        string           `json:"actionStatus,omitempty"`
	ActionDetails       string           `json:"actionDetails,omitempty"`
	StageDurationsMS    map[string]int64 `json:"stageDurationsMs,omitempty"`
	RawResultJSON       any              `json:"rawResultJson"`
	RenderedCommentBody string           `json:"renderedCommentBody"`
	CommentID           int64            `json:"commentId"`
	CreatedAt           string           `json:"createdAt"`
}

type DailySummary struct {
	Date         string         `json:"date"`
	ReviewCount  int            `json:"reviewCount"`
	RiskCounts   map[string]int `json:"riskCounts"`
	Repos        []string       `json:"repos"`
	FailedEvents int            `json:"failedEvents"`
}

type ConflictRetry struct {
	RepoFullName     string `json:"repoFullName"`
	PRNumber         int    `json:"prNumber"`
	HeadSHA          string `json:"headSha"`
	TrustLevel       string `json:"trustLevel"`
	AllowAutoResolve bool   `json:"allowAutoResolve"`
	ForceResolve     bool   `json:"forceResolve"`
	OperatorGoal     string `json:"operatorGoal,omitempty"`
	Pull             any    `json:"pull"`
	ReviewResult     any    `json:"reviewResult"`
	FailedStep       string `json:"failedStep"`
	ErrorMessage     string `json:"errorMessage"`
	CreatedAt        string `json:"createdAt"`
	UpdatedAt        string `json:"updatedAt"`
}

type FileStorage struct {
	dataDir       string
	eventLogPath  string
	reviewRunPath string
	conflictPath  string
	mu            sync.Mutex
}

func (s *FileStorage) ListEventLogs() ([]EventLog, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var logs []EventLog
	if err := readJSONFile(s.eventLogPath, &logs); err != nil {
		return nil, err
	}
	result := make([]EventLog, len(logs))
	copy(result, logs)
	return result, nil
}

func (s *FileStorage) ListReviewRuns() ([]ReviewRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var runs []ReviewRun
	if err := readJSONFile(s.reviewRunPath, &runs); err != nil {
		return nil, err
	}
	result := make([]ReviewRun, len(runs))
	copy(result, runs)
	return result, nil
}

func New(dataDir string) (*FileStorage, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}

	return &FileStorage{
		dataDir:       dataDir,
		eventLogPath:  filepath.Join(dataDir, "event-logs.json"),
		reviewRunPath: filepath.Join(dataDir, "review-runs.json"),
		conflictPath:  filepath.Join(dataDir, "conflict-retries.json"),
	}, nil
}

func (s *FileStorage) SaveEventLog(log EventLog) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var logs []EventLog
	if err := readJSONFile(s.eventLogPath, &logs); err != nil {
		return err
	}
	logs = append(logs, log)
	return writeJSONFile(s.eventLogPath, logs)
}

func (s *FileStorage) UpdateEventLog(deliveryID string, updater func(EventLog) EventLog) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var logs []EventLog
	if err := readJSONFile(s.eventLogPath, &logs); err != nil {
		return err
	}
	for i := range logs {
		if logs[i].DeliveryID == deliveryID {
			logs[i] = updater(logs[i])
		}
	}
	return writeJSONFile(s.eventLogPath, logs)
}

func (s *FileStorage) HasProcessedDelivery(deliveryID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var logs []EventLog
	if err := readJSONFile(s.eventLogPath, &logs); err != nil {
		return false, err
	}
	for _, log := range logs {
		if log.DeliveryID == deliveryID && log.ProcessStatus == "completed" {
			return true, nil
		}
	}
	return false, nil
}

func (s *FileStorage) HasReviewRun(repoFullName string, prNumber int, headSHA string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var runs []ReviewRun
	if err := readJSONFile(s.reviewRunPath, &runs); err != nil {
		return false, err
	}
	for _, run := range runs {
		if run.RepoFullName == repoFullName && run.PRNumber == prNumber && run.HeadSHA == headSHA {
			return true, nil
		}
	}
	return false, nil
}

func (s *FileStorage) SaveReviewRun(run ReviewRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var runs []ReviewRun
	if err := readJSONFile(s.reviewRunPath, &runs); err != nil {
		return err
	}
	runs = append(runs, run)
	return writeJSONFile(s.reviewRunPath, runs)
}

func (s *FileStorage) FindLatestReviewRun(repoFullName string, prNumber int) (ReviewRun, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var runs []ReviewRun
	if err := readJSONFile(s.reviewRunPath, &runs); err != nil {
		return ReviewRun{}, false, err
	}

	var latest ReviewRun
	found := false
	for _, run := range runs {
		if run.RepoFullName != repoFullName || run.PRNumber != prNumber {
			continue
		}
		if !found || run.CreatedAt >= latest.CreatedAt {
			latest = run
			found = true
		}
	}
	return latest, found, nil
}

func (s *FileStorage) SaveConflictRetry(entry ConflictRetry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var entries []ConflictRetry
	if err := readJSONFile(s.conflictPath, &entries); err != nil {
		return err
	}

	updated := false
	for i := range entries {
		if entries[i].RepoFullName == entry.RepoFullName && entries[i].PRNumber == entry.PRNumber {
			entries[i] = entry
			updated = true
			break
		}
	}
	if !updated {
		entries = append(entries, entry)
	}
	return writeJSONFile(s.conflictPath, entries)
}

func (s *FileStorage) FindConflictRetry(repoFullName string, prNumber int) (ConflictRetry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var entries []ConflictRetry
	if err := readJSONFile(s.conflictPath, &entries); err != nil {
		return ConflictRetry{}, false, err
	}

	for _, entry := range entries {
		if entry.RepoFullName == repoFullName && entry.PRNumber == prNumber {
			return entry, true, nil
		}
	}
	return ConflictRetry{}, false, nil
}

func (s *FileStorage) DeleteConflictRetry(repoFullName string, prNumber int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var entries []ConflictRetry
	if err := readJSONFile(s.conflictPath, &entries); err != nil {
		return err
	}

	filtered := entries[:0]
	for _, entry := range entries {
		if entry.RepoFullName == repoFullName && entry.PRNumber == prNumber {
			continue
		}
		filtered = append(filtered, entry)
	}
	return writeJSONFile(s.conflictPath, filtered)
}

func (s *FileStorage) DailySummary(now time.Time) (DailySummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	dayPrefix := now.UTC().Format("2006-01-02")

	var runs []ReviewRun
	if err := readJSONFile(s.reviewRunPath, &runs); err != nil {
		return DailySummary{}, err
	}

	riskCounts := map[string]int{
		"low":     0,
		"medium":  0,
		"high":    0,
		"unknown": 0,
	}
	reposSet := map[string]struct{}{}
	reviewCount := 0
	for _, run := range runs {
		if len(run.CreatedAt) >= 10 && run.CreatedAt[:10] == dayPrefix {
			reviewCount++
			if _, ok := riskCounts[run.OverallRisk]; ok {
				riskCounts[run.OverallRisk]++
			} else {
				riskCounts["unknown"]++
			}
			reposSet[run.RepoFullName] = struct{}{}
		}
	}

	var logs []EventLog
	if err := readJSONFile(s.eventLogPath, &logs); err != nil {
		return DailySummary{}, err
	}

	failedEvents := 0
	for _, log := range logs {
		if len(log.ReceivedAt) >= 10 && log.ReceivedAt[:10] == dayPrefix && log.ProcessStatus == "failed" {
			failedEvents++
		}
	}

	repos := make([]string, 0, len(reposSet))
	for repo := range reposSet {
		repos = append(repos, repo)
	}
	slices.Sort(repos)

	return DailySummary{
		Date:         dayPrefix,
		ReviewCount:  reviewCount,
		RiskCounts:   riskCounts,
		Repos:        repos,
		FailedEvents: failedEvents,
	}, nil
}

func readJSONFile[T any](path string, target *T) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := writeJSONFile(path, []any{}); err != nil {
			return err
		}
		return json.Unmarshal([]byte("[]"), target)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		data = []byte("[]")
	}
	return json.Unmarshal(data, target)
}

func writeJSONFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}