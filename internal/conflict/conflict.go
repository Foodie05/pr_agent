package conflict

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"pr-agent-go/internal/github"
	"pr-agent-go/internal/review"
)

const (
	ModeAutoResolve  = "auto_resolve"
	ModeSuggestOnly  = "suggest_only"
	ModeForceResolve = "force_resolve"
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

type conflictBlock struct {
	StartMarker int
	EndMarker   int
	BlockText   string
	Before      string
	After       string
}

type GitResolver struct {
	Token             string
	TempDir           string
	UserName          string
	UserEmail         string
	Agent             *review.Agent
	Runner            CommandRunner
	StepTimeout       time.Duration
	ConflictBatchSize int
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
		Token:             token,
		TempDir:           tempDir,
		UserName:          userName,
		UserEmail:         userEmail,
		Agent:             agent,
		Runner:            execRunner{},
		StepTimeout:       75 * time.Second,
		ConflictBatchSize: 3,
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
	if _, err := r.runGitStepWithRetry("", "clone", "git", "clone", "--branch", pull.Head.Ref, "--single-branch", withToken(pull.Head.Repo.CloneURL, r.Token), repoDir); err != nil {
		return Outcome{}, err
	}
	if _, err := r.runStep(repoDir, "git", "config", "user.name", fallback(r.UserName, "pr-agent-go")); err != nil {
		return Outcome{}, err
	}
	if _, err := r.runStep(repoDir, "git", "config", "user.email", fallback(r.UserEmail, "pr-agent-go@local")); err != nil {
		return Outcome{}, err
	}

	if _, err := r.runStep(repoDir, "git", "remote", "add", "upstream", withToken(pull.Base.Repo.CloneURL, r.Token)); err != nil && !strings.Contains(err.Error(), "already exists") {
		return Outcome{}, err
	}
	if _, err := r.runGitStepWithRetry(repoDir, "fetch", "git", "fetch", "upstream", pull.Base.Ref); err != nil {
		return Outcome{}, err
	}

	log.Printf("conflict merge setup repo=%s pr=%d", pull.Base.Repo.FullName, pull.Number)
	_, mergeErr := r.runStep(repoDir, "git", "merge", "--no-ff", "--no-commit", "upstream/"+pull.Base.Ref)
	if mergeErr == nil {
		if _, err := r.runStep(repoDir, "git", "commit", "-m", fmt.Sprintf("Merge %s into %s for PR #%d", pull.Base.Ref, pull.Head.Ref, pull.Number)); err != nil {
			return Outcome{}, err
		}
		if _, err := r.runStep(repoDir, "git", "push", "origin", "HEAD:"+pull.Head.Ref); err != nil {
			return Outcome{}, err
		}
		return Outcome{
			Mode:         mode,
			MergeClean:   true,
			AutoResolved: true,
			Pushed:       true,
		}, nil
	}

	conflicts, err := r.collectConflicts(r.newStepContext(), repoDir)
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

	if mode == ModeSuggestOnly {
		summary, err := r.summarizeConflicts(pull, reviewResult, conflicts)
		if err != nil {
			return Outcome{}, err
		}
		outcome.ConflictSummary = summary
		return outcome, nil
	}

	if mode != ModeForceResolve && len(conflicts) > 10 {
		summary, err := r.summarizeConflicts(pull, reviewResult, conflicts)
		if err != nil {
			return Outcome{}, err
		}
		outcome.ConflictSummary = summary
		return outcome, nil
	}

	batchSize := r.ConflictBatchSize
	if batchSize <= 0 {
		batchSize = 3
	}
	resolutionSummaries := make([]string, 0, len(conflicts))
	for start := 0; start < len(conflicts); start += batchSize {
		end := start + batchSize
		if end > len(conflicts) {
			end = len(conflicts)
		}
		log.Printf("conflict auto-resolve batch %d-%d/%d repo=%s pr=%d", start+1, end, len(conflicts), pull.Base.Repo.FullName, pull.Number)
		for _, file := range conflicts[start:end] {
			log.Printf("conflict file start repo=%s pr=%d path=%s mode=%s", pull.Base.Repo.FullName, pull.Number, file.Path, mode)
			if !isResolvableFile(file) {
				log.Printf("conflict file blocked repo=%s pr=%d path=%s reason=unresolvable_file", pull.Base.Repo.FullName, pull.Number, file.Path)
				if mode == ModeForceResolve {
					return Outcome{}, fmt.Errorf("force conflict resolution cannot safely handle file %s", file.Path)
				}
				summary, err := r.summarizeConflicts(pull, reviewResult, conflicts)
				if err != nil {
					return Outcome{}, err
				}
				outcome.ConflictSummary = summary
				return outcome, nil
			}

			resolvedContent, decisionSummary, decisionConfidence, err := r.resolveConflictFile(pull, reviewResult, file, mode)
			if err != nil {
				if retryable, ok := err.(RetryableError); ok {
					return Outcome{}, retryable
				}
				log.Printf("conflict file failed repo=%s pr=%d path=%s reason=resolve_file err=%v", pull.Base.Repo.FullName, pull.Number, file.Path, err)
				if mode == ModeForceResolve {
					return Outcome{}, err
				}
				summary, sumErr := r.summarizeConflicts(pull, reviewResult, conflicts)
				if sumErr != nil {
					return Outcome{}, err
				}
				outcome.ConflictSummary = summary
				return outcome, nil
			}

			fullPath := filepath.Join(repoDir, file.Path)
			if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
				return Outcome{}, err
			}
			if err := os.WriteFile(fullPath, []byte(resolvedContent), 0o644); err != nil {
				return Outcome{}, err
			}
			if _, err := r.runStep(repoDir, "git", "add", file.Path); err != nil {
				return Outcome{}, err
			}
			log.Printf("conflict file applied repo=%s pr=%d path=%s confidence=%.2f", pull.Base.Repo.FullName, pull.Number, file.Path, decisionConfidence)
			resolutionSummaries = append(resolutionSummaries, fmt.Sprintf("%s: %s", file.Path, decisionSummary))
		}
	}

	unmerged, err := r.runStep(repoDir, "git", "diff", "--name-only", "--diff-filter=U")
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

	if _, err := r.runStep(repoDir, "git", "commit", "-m", fmt.Sprintf("Resolve merge conflicts for PR #%d", pull.Number)); err != nil {
		return Outcome{}, err
	}
	if _, err := r.runStep(repoDir, "git", "push", "origin", "HEAD:"+pull.Head.Ref); err != nil {
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
	summary, err := r.Agent.SummarizeConflicts(review.ConflictSummaryContext{
		RepoFullName:  pull.Base.Repo.FullName,
		PRNumber:      pull.Number,
		PullTitle:     pull.Title,
		OperatorGoal:  reviewResult.OperatorGoal,
		ReviewSummary: reviewResult.Summary,
		OverallRisk:   reviewResult.OverallRisk,
		Conflicts:     items,
	})
	if err != nil && review.IsRetryableModelError(err) {
		return review.ConflictSummary{}, RetryableError{
			Step:    "summarize_conflicts",
			Message: err.Error(),
		}
	}
	return summary, err
}

func (r *GitResolver) resolveConflictFile(pull github.Pull, reviewResult review.Result, file FileConflict, mode string) (string, string, float64, error) {
	if shouldResolveByBlocks(file) {
		return r.resolveConflictFileByBlocks(pull, reviewResult, file, mode)
	}

	decision, err := r.Agent.ResolveConflict(review.ConflictContext{
		RepoFullName:    pull.Base.Repo.FullName,
		PRNumber:        pull.Number,
		PullTitle:       pull.Title,
		FilePath:        file.Path,
		OperatorGoal:    reviewResult.OperatorGoal,
		ReviewSummary:   reviewResult.Summary,
		OverallRisk:     reviewResult.OverallRisk,
		BaseContent:     file.BaseContent,
		CurrentContent:  file.CurrentContent,
		IncomingContent: file.IncomingContent,
		ConflictMarkers: file.ConflictMarkers,
	})
	if err != nil {
		if review.IsRetryableModelError(err) {
			return "", "", 0, RetryableError{Step: "resolve_conflict:" + file.Path, Message: err.Error()}
		}
		return "", "", 0, err
	}
	if strings.TrimSpace(decision.ResolvedContent) == "" {
		return "", "", 0, fmt.Errorf("empty resolution for %s", file.Path)
	}
	if strings.Contains(decision.ResolvedContent, "<<<<<<<") {
		return "", "", 0, fmt.Errorf("resolution still contains conflict markers for %s", file.Path)
	}
	if mode != ModeForceResolve && (!decision.ShouldApply || decision.Confidence < 0.8) {
		return "", "", 0, fmt.Errorf("low confidence resolution for %s", file.Path)
	}
	return decision.ResolvedContent, decision.Summary, decision.Confidence, nil
}

func (r *GitResolver) resolveConflictFileByBlocks(pull github.Pull, reviewResult review.Result, file FileConflict, mode string) (string, string, float64, error) {
	blocks := extractConflictBlocks(file.ConflictMarkers)
	if len(blocks) == 0 {
		return "", "", 0, fmt.Errorf("no conflict blocks found for %s", file.Path)
	}

	resolved := file.ConflictMarkers
	summaries := make([]string, 0, len(blocks))
	minConfidence := 1.0
	offset := 0
	for index, block := range blocks {
		log.Printf("conflict block start repo=%s pr=%d path=%s block=%d/%d", pull.Base.Repo.FullName, pull.Number, file.Path, index+1, len(blocks))
		decision, err := r.Agent.ResolveConflictBlock(review.ConflictContext{
			RepoFullName:    pull.Base.Repo.FullName,
			PRNumber:        pull.Number,
			PullTitle:       pull.Title,
			FilePath:        file.Path,
			BlockIndex:      index + 1,
			BlockCount:      len(blocks),
			OperatorGoal:    reviewResult.OperatorGoal,
			ReviewSummary:   reviewResult.Summary,
			OverallRisk:     reviewResult.OverallRisk,
			BaseContent:     block.Before,
			CurrentContent:  block.Before,
			IncomingContent: block.After,
			ConflictMarkers: block.BlockText,
		})
		if err != nil {
			if review.IsRetryableModelError(err) {
				return "", "", 0, RetryableError{Step: fmt.Sprintf("resolve_conflict_block:%s:%d", file.Path, index+1), Message: err.Error()}
			}
			return "", "", 0, err
		}
		if strings.TrimSpace(decision.ResolvedBlock) == "" {
			return "", "", 0, fmt.Errorf("empty block resolution for %s block %d", file.Path, index+1)
		}
		if strings.Contains(decision.ResolvedBlock, "<<<<<<<") {
			return "", "", 0, fmt.Errorf("block resolution still contains conflict markers for %s block %d", file.Path, index+1)
		}
		if mode != ModeForceResolve && (!decision.ShouldApply || decision.Confidence < 0.8) {
			return "", "", 0, fmt.Errorf("low confidence block resolution for %s block %d", file.Path, index+1)
		}

		start := block.StartMarker + offset
		end := block.EndMarker + offset
		resolved = resolved[:start] + decision.ResolvedBlock + resolved[end:]
		offset += len(decision.ResolvedBlock) - (block.EndMarker - block.StartMarker)
		summaries = append(summaries, fmt.Sprintf("block %d: %s", index+1, decision.Summary))
		if decision.Confidence < minConfidence {
			minConfidence = decision.Confidence
		}
		log.Printf("conflict block applied repo=%s pr=%d path=%s block=%d/%d confidence=%.2f", pull.Base.Repo.FullName, pull.Number, file.Path, index+1, len(blocks), decision.Confidence)
	}

	if strings.Contains(resolved, "<<<<<<<") {
		return "", "", 0, fmt.Errorf("resolved file still contains conflict markers for %s", file.Path)
	}
	return resolved, strings.Join(summaries, " | "), minConfidence, nil
}

func shouldResolveByBlocks(file FileConflict) bool {
	return len(file.ConflictMarkers) > 16000 || countConflictMarkers(file.ConflictMarkers) > 1
}

func countConflictMarkers(value string) int {
	return strings.Count(value, "<<<<<<<")
}

func extractConflictBlocks(content string) []conflictBlock {
	lines := strings.SplitAfter(content, "\n")
	blocks := []conflictBlock{}
	pos := 0
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		lineStart := pos
		pos += len(line)
		if !strings.HasPrefix(line, "<<<<<<<") {
			continue
		}
		startLine := i
		endLine := i + 1
		foundSeparator := false
		for ; endLine < len(lines); endLine++ {
			if strings.HasPrefix(lines[endLine], "=======") {
				foundSeparator = true
			}
			if strings.HasPrefix(lines[endLine], ">>>>>>>") {
				endLine++
				break
			}
		}
		if !foundSeparator || endLine > len(lines) {
			break
		}
		startOffset := lineStart
		endOffset := startOffset
		for _, entry := range lines[startLine:endLine] {
			endOffset += len(entry)
		}
		beforeStart := startLine - 12
		if beforeStart < 0 {
			beforeStart = 0
		}
		afterEnd := endLine + 12
		if afterEnd > len(lines) {
			afterEnd = len(lines)
		}
		blocks = append(blocks, conflictBlock{
			StartMarker: startOffset,
			EndMarker:   endOffset,
			BlockText:   strings.Join(lines[startLine:endLine], ""),
			Before:      strings.Join(lines[beforeStart:startLine], ""),
			After:       strings.Join(lines[endLine:afterEnd], ""),
		})
		i = endLine - 1
		pos = endOffset
	}
	return blocks
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

func (r *GitResolver) runStep(dir string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.effectiveStepTimeout())
	defer cancel()
	return r.Runner.Run(ctx, dir, name, args...)
}

func (r *GitResolver) newStepContext() context.Context {
	ctx, _ := context.WithTimeout(context.Background(), r.effectiveStepTimeout())
	return ctx
}

func (r *GitResolver) effectiveStepTimeout() time.Duration {
	timeout := r.StepTimeout
	if timeout <= 0 {
		timeout = 75 * time.Second
	}
	return timeout
}

func (r *GitResolver) runGitStepWithRetry(dir string, step string, name string, args ...string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		output, err := r.runStep(dir, name, args...)
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
