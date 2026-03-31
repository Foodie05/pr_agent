package conflict

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"pr-agent-go/internal/github"
	"pr-agent-go/internal/review"
)

const (
	ModeAutoResolve = "auto_resolve"
	ModeSuggestOnly = "suggest_only"
)

type Resolver interface {
	Resolve(pull github.Pull, reviewResult review.Result, mode string) (Outcome, error)
}

type Outcome struct {
	Mode                string
	MergeClean          bool
	AutoResolved        bool
	Pushed              bool
	ConflictFiles       []FileConflict
	ConflictSummary     review.ConflictSummary
	ResolutionSummaries []string
}

type RetryableError struct {
	Step    string
	Message string
}

func (e RetryableError) Error() string {
	if strings.TrimSpace(e.Message) == "" {
		return fmt.Sprintf("retryable conflict step failure at %s", e.Step)
	}
	return fmt.Sprintf("%s: %s", e.Step, e.Message)
}

type FileConflict struct {
	Path            string
	BaseContent     string
	CurrentContent  string
	IncomingContent string
	ConflictMarkers string
}

type GitResolver struct {
	Token     string
	TempDir   string
	UserName  string
	UserEmail string
	Agent     *review.Agent
	Runner    CommandRunner
}

type CommandRunner interface {
	Run(ctx context.Context, dir string, name string, args ...string) (string, error)
}

type execRunner struct{}

func (r execRunner) Run(ctx context.Context, dir string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		if text == "" {
			return "", err
		}
		return text, fmt.Errorf("%w: %s", err, text)
	}
	return text, nil
}

func NewGitResolver(token, tempDir, userName, userEmail string, agent *review.Agent) *GitResolver {
	return &GitResolver{
		Token:     token,
		TempDir:   tempDir,
		UserName:  userName,
		UserEmail: userEmail,
		Agent:     agent,
		Runner:    execRunner{},
	}
}

func (r *GitResolver) Resolve(pull github.Pull, reviewResult review.Result, mode string) (Outcome, error) {
	if r.Runner == nil {
		r.Runner = execRunner{}
	}
	if pull.Head.Repo.CloneURL == "" || pull.Base.Repo.CloneURL == "" {
		return Outcome{}, fmt.Errorf("missing clone url in pull payload")
	}

	if err := os.MkdirAll(r.TempDir, 0o755); err != nil {
		return Outcome{}, err
	}

	workspace, err := os.MkdirTemp(r.TempDir, "conflict-*")
	if err != nil {
		return Outcome{}, err
	}
	defer os.RemoveAll(workspace)

	repoDir := filepath.Join(workspace, "repo")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if _, err := r.runGitStepWithRetry(ctx, "", "clone", "git", "clone", "--branch", pull.Head.Ref, "--single-branch", withToken(pull.Head.Repo.CloneURL, r.Token), repoDir); err != nil {
		return Outcome{}, err
	}
	if _, err := r.Runner.Run(ctx, repoDir, "git", "config", "user.name", fallback(r.UserName, "pr-agent-go")); err != nil {
		return Outcome{}, err
	}
	if _, err := r.Runner.Run(ctx, repoDir, "git", "config", "user.email", fallback(r.UserEmail, "pr-agent-go@local")); err != nil {
		return Outcome{}, err
	}

	if _, err := r.Runner.Run(ctx, repoDir, "git", "remote", "add", "upstream", withToken(pull.Base.Repo.CloneURL, r.Token)); err != nil && !strings.Contains(err.Error(), "already exists") {
		return Outcome{}, err
	}
	if _, err := r.runGitStepWithRetry(ctx, repoDir, "fetch", "git", "fetch", "upstream", pull.Base.Ref); err != nil {
		return Outcome{}, err
	}

	_, mergeErr := r.Runner.Run(ctx, repoDir, "git", "merge", "--no-ff", "--no-commit", "upstream/"+pull.Base.Ref)
	if mergeErr == nil {
		if _, err := r.Runner.Run(ctx, repoDir, "git", "commit", "-m", fmt.Sprintf("Merge %s into %s for PR #%d", pull.Base.Ref, pull.Head.Ref, pull.Number)); err != nil {
			return Outcome{}, err
		}
		if _, err := r.Runner.Run(ctx, repoDir, "git", "push", "origin", "HEAD:"+pull.Head.Ref); err != nil {
			return Outcome{}, err
		}
		return Outcome{
			Mode:         mode,
			MergeClean:   true,
			AutoResolved: true,
			Pushed:       true,
		}, nil
	}

	conflicts, err := r.collectConflicts(ctx, repoDir)
	if err != nil {
		return Outcome{}, err
	}
	if len(conflicts) == 0 {
		return Outcome{}, mergeErr
	}

	outcome := Outcome{
		Mode:          mode,
		ConflictFiles: conflicts,
	}

	if mode != ModeAutoResolve {
		summary, err := r.summarizeConflicts(pull, reviewResult, conflicts)
		if err != nil {
			return Outcome{}, err
		}
		outcome.ConflictSummary = summary
		return outcome, nil
	}

	if len(conflicts) > 5 {
		summary, err := r.summarizeConflicts(pull, reviewResult, conflicts)
		if err != nil {
			return Outcome{}, err
		}
		outcome.ConflictSummary = summary
		return outcome, nil
	}

	resolutionSummaries := make([]string, 0, len(conflicts))
	for _, file := range conflicts {
		if !isResolvableFile(file) {
			summary, err := r.summarizeConflicts(pull, reviewResult, conflicts)
			if err != nil {
				return Outcome{}, err
			}
			outcome.ConflictSummary = summary
			return outcome, nil
		}

		decision, err := r.Agent.ResolveConflict(review.ConflictContext{
			RepoFullName:    pull.Base.Repo.FullName,
			PRNumber:        pull.Number,
			PullTitle:       pull.Title,
			FilePath:        file.Path,
			ReviewSummary:   reviewResult.Summary,
			OverallRisk:     reviewResult.OverallRisk,
			BaseContent:     file.BaseContent,
			CurrentContent:  file.CurrentContent,
			IncomingContent: file.IncomingContent,
			ConflictMarkers: file.ConflictMarkers,
		})
		if err != nil {
			summary, sumErr := r.summarizeConflicts(pull, reviewResult, conflicts)
			if sumErr != nil {
				return Outcome{}, err
			}
			outcome.ConflictSummary = summary
			return outcome, nil
		}
		if !decision.ShouldApply || decision.Confidence < 0.8 || strings.Contains(decision.ResolvedContent, "<<<<<<<") {
			summary, err := r.summarizeConflicts(pull, reviewResult, conflicts)
			if err != nil {
				return Outcome{}, err
			}
			outcome.ConflictSummary = summary
			return outcome, nil
		}

		fullPath := filepath.Join(repoDir, file.Path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			return Outcome{}, err
		}
		if err := os.WriteFile(fullPath, []byte(decision.ResolvedContent), 0o644); err != nil {
			return Outcome{}, err
		}
		if _, err := r.Runner.Run(ctx, repoDir, "git", "add", file.Path); err != nil {
			return Outcome{}, err
		}
		resolutionSummaries = append(resolutionSummaries, fmt.Sprintf("%s: %s", file.Path, decision.Summary))
	}

	unmerged, err := r.Runner.Run(ctx, repoDir, "git", "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return Outcome{}, err
	}
	if strings.TrimSpace(unmerged) != "" {
		summary, err := r.summarizeConflicts(pull, reviewResult, conflicts)
		if err != nil {
			return Outcome{}, err
		}
		outcome.ConflictSummary = summary
		outcome.ResolutionSummaries = resolutionSummaries
		return outcome, nil
	}

	if _, err := r.Runner.Run(ctx, repoDir, "git", "commit", "-m", fmt.Sprintf("Resolve merge conflicts for PR #%d", pull.Number)); err != nil {
		return Outcome{}, err
	}
	if _, err := r.Runner.Run(ctx, repoDir, "git", "push", "origin", "HEAD:"+pull.Head.Ref); err != nil {
		return Outcome{}, err
	}

	outcome.AutoResolved = true
	outcome.Pushed = true
	outcome.ResolutionSummaries = resolutionSummaries
	return outcome, nil
}

func (r *GitResolver) summarizeConflicts(pull github.Pull, reviewResult review.Result, conflicts []FileConflict) (review.ConflictSummary, error) {
	items := make([]review.ConflictFileSummary, 0, len(conflicts))
	for _, file := range conflicts {
		items = append(items, review.ConflictFileSummary{
			FilePath:        file.Path,
			ConflictMarkers: truncate(file.ConflictMarkers, 2000),
		})
	}
	return r.Agent.SummarizeConflicts(review.ConflictSummaryContext{
		RepoFullName:  pull.Base.Repo.FullName,
		PRNumber:      pull.Number,
		PullTitle:     pull.Title,
		ReviewSummary: reviewResult.Summary,
		OverallRisk:   reviewResult.OverallRisk,
		Conflicts:     items,
	})
}

func (r *GitResolver) collectConflicts(ctx context.Context, repoDir string) ([]FileConflict, error) {
	output, err := r.Runner.Run(ctx, repoDir, "git", "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	paths := strings.Fields(strings.TrimSpace(output))
	conflicts := make([]FileConflict, 0, len(paths))
	for _, path := range paths {
		markers, err := os.ReadFile(filepath.Join(repoDir, path))
		if err != nil {
			return nil, err
		}
		baseContent, _ := r.showStage(ctx, repoDir, "1", path)
		currentContent, _ := r.showStage(ctx, repoDir, "2", path)
		incomingContent, _ := r.showStage(ctx, repoDir, "3", path)
		conflicts = append(conflicts, FileConflict{
			Path:            path,
			BaseContent:     baseContent,
			CurrentContent:  currentContent,
			IncomingContent: incomingContent,
			ConflictMarkers: string(markers),
		})
	}
	return conflicts, nil
}

func (r *GitResolver) showStage(ctx context.Context, repoDir, stage, path string) (string, error) {
	output, err := r.Runner.Run(ctx, repoDir, "git", "show", fmt.Sprintf(":%s:%s", stage, path))
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", nil
		}
		if strings.Contains(err.Error(), "path") || strings.Contains(err.Error(), "exists on disk") {
			return "", nil
		}
		return "", err
	}
	return output, nil
}

func withToken(cloneURL, token string) string {
	if token == "" || cloneURL == "" {
		return cloneURL
	}
	return strings.Replace(cloneURL, "https://", "https://x-access-token:"+token+"@", 1)
}

func isResolvableFile(file FileConflict) bool {
	if strings.Contains(file.Path, ".lock") || strings.Contains(file.Path, ".png") || strings.Contains(file.Path, ".jpg") || strings.Contains(file.Path, ".jpeg") || strings.Contains(file.Path, ".gif") || strings.Contains(file.Path, ".pdf") {
		return false
	}
	if len(file.ConflictMarkers) > 40000 || len(file.BaseContent) > 40000 || len(file.CurrentContent) > 40000 || len(file.IncomingContent) > 40000 {
		return false
	}
	return true
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}

func fallback(value, defaultValue string) string {
	if strings.TrimSpace(value) == "" {
		return defaultValue
	}
	return value
}

func (r *GitResolver) runGitStepWithRetry(ctx context.Context, dir string, step string, name string, args ...string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		output, err := r.Runner.Run(ctx, dir, name, args...)
		if err == nil {
			return output, nil
		}
		lastErr = err
		if !isRetryableGitError(err) || attempt == 2 {
			return "", RetryableError{Step: step, Message: err.Error()}
		}
		time.Sleep(time.Duration(attempt+1) * 1200 * time.Millisecond)
	}
	return "", RetryableError{Step: step, Message: lastErr.Error()}
}

func isRetryableGitError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "timed out") ||
		strings.Contains(message, "timeout") ||
		strings.Contains(message, "failed to connect") ||
		strings.Contains(message, "connection reset") ||
		strings.Contains(message, "connection refused") ||
		strings.Contains(message, "temporary failure") ||
		strings.Contains(message, "network is unreachable") ||
		strings.Contains(message, "could not resolve host")
}
