package orchestrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"pr-agent-go/internal/conflict"
	"pr-agent-go/internal/github"
	"pr-agent-go/internal/render"
	"pr-agent-go/internal/review"
	"pr-agent-go/internal/storage"
)

const (
	trustTrusted               = "trusted"
	trustTrustedNeedsSync      = "trusted_needs_sync"
	trustTrustedConflict       = "trusted_conflict"
	trustNeedsUserIntervention = "needs_user_intervention"
)

var mergeRetryDelayBase = 750 * time.Millisecond

type Service struct {
	Storage             *storage.FileStorage
	GitHub              *github.Client
	Agent               *review.Agent
	ConflictResolver    conflict.Resolver
	ReviewCommentMarker string
}

type WebhookEnvelope struct {
	EventName  string
	DeliveryID string
	Payload    PullRequestEvent
}

type PullRequestEvent struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	PullRequest struct {
		Number int `json:"number"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
}

type Result struct {
	OK               bool             `json:"ok,omitempty"`
	Ignored          bool             `json:"ignored,omitempty"`
	Reason           string           `json:"reason,omitempty"`
	RepoFullName     string           `json:"repoFullName,omitempty"`
	PRNumber         int              `json:"prNumber,omitempty"`
	HeadSHA          string           `json:"headSha,omitempty"`
	OverallRisk      string           `json:"overallRisk,omitempty"`
	TrustLevel       string           `json:"trustLevel,omitempty"`
	ActionTaken      string           `json:"actionTaken,omitempty"`
	ActionStatus     string           `json:"actionStatus,omitempty"`
	ActionDetails    string           `json:"actionDetails,omitempty"`
	StageDurationsMS map[string]int64 `json:"stageDurationsMs,omitempty"`
	TotalDurationMS  int64            `json:"totalDurationMs,omitempty"`
}

type ProgressFunc func(stage string, details string)

type stageRecorder struct {
	progress  ProgressFunc
	started   time.Time
	durations map[string]int64
}

type autoAction struct {
	TrustLevel    string
	ActionTaken   string
	ActionStatus  string
	ActionDetails string
}

func newStageRecorder(progress ProgressFunc) *stageRecorder {
	return &stageRecorder{
		progress:  progress,
		started:   time.Now(),
		durations: map[string]int64{},
	}
}

func (r *stageRecorder) Start(stage, details string) func(error) {
	emitProgress(r.progress, stage, details)
	started := time.Now()
	return func(err error) {
		duration := time.Since(started).Milliseconds()
		r.durations[stage] = duration
		if err != nil {
			emitProgress(r.progress, stage+"_done", fmt.Sprintf("failed in %dms: %v", duration, err))
			return
		}
		emitProgress(r.progress, stage+"_done", fmt.Sprintf("completed in %dms", duration))
	}
}

func (r *stageRecorder) Durations() map[string]int64 {
	cloned := make(map[string]int64, len(r.durations))
	for k, v := range r.durations {
		cloned[k] = v
	}
	return cloned
}

func (r *stageRecorder) TotalMS() int64 {
	return time.Since(r.started).Milliseconds()
}

func (r *stageRecorder) Summary() string {
	if len(r.durations) == 0 {
		return "no stages recorded"
	}
	keys := make([]string, 0, len(r.durations))
	for key := range r.durations {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%dms", key, r.durations[key]))
	}
	parts = append(parts, fmt.Sprintf("total=%dms", r.TotalMS()))
	return strings.Join(parts, " ")
}

func (s *Service) HandleWebhook(envelope WebhookEnvelope) (result Result, err error) {
	action := envelope.Payload.Action

	_ = s.Storage.SaveEventLog(storage.EventLog{
		DeliveryID:    envelope.DeliveryID,
		EventName:     envelope.EventName,
		Action:        action,
		RepoFullName:  envelope.Payload.Repository.FullName,
		ReceivedAt:    time.Now().UTC().Format(time.RFC3339),
		ProcessStatus: "received",
	})

	if !isSupportedEvent(envelope.EventName, action) {
		_ = s.Storage.UpdateEventLog(envelope.DeliveryID, func(log storage.EventLog) storage.EventLog {
			log.ProcessStatus = "ignored"
			log.ProcessedAt = time.Now().UTC().Format(time.RFC3339)
			log.Reason = "unsupported_event"
			return log
		})
		return Result{Ignored: true, Reason: "unsupported_event"}, nil
	}

	processed, err := s.Storage.HasProcessedDelivery(envelope.DeliveryID)
	if err != nil {
		return Result{}, err
	}
	if processed {
		return Result{Ignored: true, Reason: "duplicate_delivery"}, nil
	}

	repoFullName := envelope.Payload.Repository.FullName
	prNumber := envelope.Payload.PullRequest.Number
	headSHA := envelope.Payload.PullRequest.Head.SHA

	defer func() {
		_ = s.Storage.UpdateEventLog(envelope.DeliveryID, func(log storage.EventLog) storage.EventLog {
			log.ProcessedAt = time.Now().UTC().Format(time.RFC3339)
			if err != nil {
				log.ProcessStatus = "failed"
				log.ErrorMessage = err.Error()
				if result.ActionStatus != "" {
					log.Reason = result.ActionStatus
				}
				return log
			}
			if result.Ignored {
				log.ProcessStatus = "ignored"
				log.Reason = firstNonEmpty(result.Reason, "ignored")
				return log
			}
			log.ProcessStatus = "completed"
			log.Reason = firstNonEmpty(result.ActionStatus, "completed")
			return log
		})
	}()

	_ = s.Storage.UpdateEventLog(envelope.DeliveryID, func(log storage.EventLog) storage.EventLog {
		log.ProcessStatus = "collecting_context"
		return log
	})

	return s.reviewPull(repoFullName, prNumber, headSHA, action, false, nil)
}

func (s *Service) ReviewPull(repoFullName string, prNumber int, triggerEvent string) (Result, error) {
	return s.ReviewPullWithProgress(repoFullName, prNumber, triggerEvent, nil)
}

func (s *Service) ReviewPullWithProgress(repoFullName string, prNumber int, triggerEvent string, progress ProgressFunc) (Result, error) {
	recorder := newStageRecorder(progress)
	done := recorder.Start("resolve_pr", fmt.Sprintf("loading PR %s #%d", repoFullName, prNumber))
	pull, err := s.GitHub.GetPull(repoFullName, prNumber)
	done(err)
	if err != nil {
		return Result{StageDurationsMS: recorder.Durations(), TotalDurationMS: recorder.TotalMS()}, err
	}
	return s.reviewPullWithRecorder(repoFullName, prNumber, pull.Head.SHA, triggerEvent, true, progress, recorder)
}

func (s *Service) IntervenePull(repoFullName string, prNumber int, userNote string, triggerEvent string, progress ProgressFunc) (Result, error) {
	recorder := newStageRecorder(progress)

	done := recorder.Start("resolve_pr", fmt.Sprintf("loading PR %s #%d for intervention", repoFullName, prNumber))
	pull, err := s.GitHub.GetPull(repoFullName, prNumber)
	done(err)
	if err != nil {
		return Result{StageDurationsMS: recorder.Durations(), TotalDurationMS: recorder.TotalMS()}, err
	}

	done = recorder.Start("ci", fmt.Sprintf("loading commit status for %s", pull.Head.SHA))
	status, err := s.GitHub.GetCommitStatus(repoFullName, pull.Head.SHA)
	done(err)
	if err != nil {
		return Result{StageDurationsMS: recorder.Durations(), TotalDurationMS: recorder.TotalMS()}, err
	}

	done = recorder.Start("load_latest_run", "loading latest review record")
	latestRun, found, err := s.Storage.FindLatestReviewRun(repoFullName, prNumber)
	done(err)
	if err != nil {
		return Result{StageDurationsMS: recorder.Durations(), TotalDurationMS: recorder.TotalMS()}, err
	}

	trustLevel := trustNeedsUserIntervention
	reviewSummary := ""
	overallRisk := "unknown"
	if found {
		trustLevel = firstNonEmpty(latestRun.TrustLevel, trustNeedsUserIntervention)
		reviewSummary = latestRun.Summary
		overallRisk = firstNonEmpty(latestRun.OverallRisk, "unknown")
	}

	done = recorder.Start("intervention", "interpreting user note with agent")
	decision, err := s.Agent.ResolveIntervention(review.InterventionContext{
		RepoFullName:   repoFullName,
		PRNumber:       prNumber,
		Title:          pull.Title,
		HeadSHA:        pull.Head.SHA,
		Mergeable:      pull.Mergeable,
		MergeableState: pull.MergeableState,
		CIState:        status.State,
		ReviewSummary:  reviewSummary,
		OverallRisk:    overallRisk,
		TrustLevel:     trustLevel,
		UserNote:       userNote,
	})
	done(err)
	if err != nil {
		return Result{StageDurationsMS: recorder.Durations(), TotalDurationMS: recorder.TotalMS()}, err
	}

	action := autoAction{
		TrustLevel:    trustLevel,
		ActionTaken:   decision.Action,
		ActionStatus:  "completed",
		ActionDetails: decision.Summary,
	}
	finalManagedCommentID := int64(0)
	finalManagedCommentBody := ""

	done = recorder.Start("action", fmt.Sprintf("executing intervention action=%s", decision.Action))
	commentBody := fmt.Sprintf("## PR Agent Intervention\n\n%s\n\n人工意见：%s", decision.Comment, strings.TrimSpace(userNote))
	switch decision.Action {
	case "merge":
		if pull.Draft {
			if err = s.GitHub.MarkPullReadyForReview(pull.NodeID); err != nil {
				break
			}
			pull, err = s.GitHub.GetPull(repoFullName, prNumber)
			if err != nil {
				break
			}
		}
		if pull.Mergeable != nil && *pull.Mergeable {
			err = s.mergeWithRepositoryRules(repoFullName, prNumber, pull.Title)
			if err == nil {
				action.ActionStatus = "merged"
				action.ActionDetails = "merged after explicit user approval"
			} else if isApprovalRequiredError(err) {
				action.ActionStatus = "awaiting_required_review"
				action.ActionDetails = err.Error()
				err = nil
			}
		} else if pull.MergeableState == "behind" {
			err = s.GitHub.UpdateBranch(repoFullName, prNumber)
			if err == nil {
				action.ActionStatus = "branch_update_requested"
				action.ActionDetails = "branch updated before merge; rerun after GitHub refreshes mergeability"
			}
		} else {
			outcome, resolveErr := s.resolveConflictsForPull(repoFullName, prNumber, pull, review.Result{
				Summary:     reviewSummary,
				OverallRisk: overallRisk,
			}, trustLevel == trustTrustedConflict)
			if resolveErr != nil {
				err = resolveErr
				break
			}
			action, err = s.completeConflictOutcome(repoFullName, prNumber, pull, outcome, trustLevel)
		}
	case "update_branch":
		err = s.GitHub.UpdateBranch(repoFullName, prNumber)
	case "re_review":
		done(nil)
		return s.reviewPull(repoFullName, prNumber, pull.Head.SHA, triggerEvent, true, progress)
	default:
		_, err = s.GitHub.CreateIssueComment(repoFullName, prNumber, commentBody)
	}
	done(err)
	if err != nil {
		return Result{
			RepoFullName:     repoFullName,
			PRNumber:         prNumber,
			HeadSHA:          pull.Head.SHA,
			OverallRisk:      overallRisk,
			TrustLevel:       trustLevel,
			ActionTaken:      action.ActionTaken,
			ActionStatus:     "failed",
			ActionDetails:    err.Error(),
			StageDurationsMS: recorder.Durations(),
			TotalDurationMS:  recorder.TotalMS(),
		}, err
	}

	if found {
		storedReviewResult := reviewResultFromRun(latestRun)
		finalManagedCommentBody = render.ReviewComment(storedReviewResult, s.ReviewCommentMarker, render.ReviewOutcome{})
		finalManagedCommentID = latestRun.CommentID
	}

	if finalCommentBody := render.FinalStatusComment(render.ReviewOutcome{
		ActionTaken:  action.ActionTaken,
		ActionStatus: action.ActionStatus,
	}); finalCommentBody != "" {
		done = recorder.Start("final_comment", fmt.Sprintf("publishing final status comment to %s #%d", repoFullName, prNumber))
		_, commentErr := s.GitHub.CreateIssueComment(repoFullName, prNumber, finalCommentBody)
		done(commentErr)
		if commentErr != nil {
			return Result{
				RepoFullName:     repoFullName,
				PRNumber:         prNumber,
				HeadSHA:          pull.Head.SHA,
				OverallRisk:      overallRisk,
				TrustLevel:       trustLevel,
				ActionTaken:      action.ActionTaken,
				ActionStatus:     "failed",
				ActionDetails:    commentErr.Error(),
				StageDurationsMS: recorder.Durations(),
				TotalDurationMS:  recorder.TotalMS(),
			}, commentErr
		}
	}

	done = recorder.Start("store", "saving intervention record")
	err = s.Storage.SaveReviewRun(storage.ReviewRun{
		RepoFullName:     repoFullName,
		PRNumber:         prNumber,
		HeadSHA:          pull.Head.SHA,
		TriggerEvent:     triggerEvent,
		Status:           "intervention_completed",
		Provider:         "manual_intervention",
		Summary:          decision.Summary,
		OverallRisk:      overallRisk,
		Confidence:       1,
		TrustLevel:       trustLevel,
		MergeReadiness:   "needs_human_review",
		ActionTaken:      action.ActionTaken,
		ActionStatus:     action.ActionStatus,
		ActionDetails:    action.ActionDetails,
		StageDurationsMS: recorder.Durations(),
		RawResultJSON: map[string]any{
			"userNote":  userNote,
			"decision":  decision,
			"reviewRun": latestRun,
		},
		RenderedCommentBody: firstNonEmpty(finalManagedCommentBody, commentBody),
		CommentID:           finalManagedCommentID,
		CreatedAt:           time.Now().UTC().Format(time.RFC3339),
	})
	done(err)
	if err != nil {
		return Result{StageDurationsMS: recorder.Durations(), TotalDurationMS: recorder.TotalMS()}, err
	}

	emitProgress(progress, "summary", recorder.Summary())

	return Result{
		OK:               true,
		RepoFullName:     repoFullName,
		PRNumber:         prNumber,
		HeadSHA:          pull.Head.SHA,
		OverallRisk:      overallRisk,
		TrustLevel:       trustLevel,
		ActionTaken:      action.ActionTaken,
		ActionStatus:     action.ActionStatus,
		ActionDetails:    action.ActionDetails,
		StageDurationsMS: recorder.Durations(),
		TotalDurationMS:  recorder.TotalMS(),
	}, nil
}

func (s *Service) RecheckConflict(repoFullName string, prNumber int, triggerEvent string, progress ProgressFunc) (Result, error) {
	recorder := newStageRecorder(progress)

	done := recorder.Start("load_retry", fmt.Sprintf("loading cached conflict retry for %s #%d", repoFullName, prNumber))
	entry, found, err := s.Storage.FindConflictRetry(repoFullName, prNumber)
	done(err)
	if err != nil {
		return Result{StageDurationsMS: recorder.Durations(), TotalDurationMS: recorder.TotalMS()}, err
	}
	if !found {
		return Result{StageDurationsMS: recorder.Durations(), TotalDurationMS: recorder.TotalMS()}, fmt.Errorf("no cached conflict retry found for %s #%d", repoFullName, prNumber)
	}

	pull, reviewResult, err := decodeConflictRetry(entry)
	if err != nil {
		return Result{StageDurationsMS: recorder.Durations(), TotalDurationMS: recorder.TotalMS()}, err
	}

	done = recorder.Start("recheck_conflict", fmt.Sprintf("retrying conflict workflow from cached %s step", entry.FailedStep))
	outcome, err := s.resolveConflictsForPull(repoFullName, prNumber, pull, reviewResult, entry.AllowAutoResolve)
	done(err)
	if err != nil {
		return Result{
			RepoFullName:     repoFullName,
			PRNumber:         prNumber,
			HeadSHA:          pull.Head.SHA,
			OverallRisk:      reviewResult.OverallRisk,
			TrustLevel:       entry.TrustLevel,
			ActionTaken:      "recheck_conflict",
			ActionStatus:     "failed",
			ActionDetails:    err.Error(),
			StageDurationsMS: recorder.Durations(),
			TotalDurationMS:  recorder.TotalMS(),
		}, err
	}

	action, err := s.completeConflictOutcome(repoFullName, prNumber, pull, outcome, entry.TrustLevel)
	if err != nil {
		return Result{
			RepoFullName:     repoFullName,
			PRNumber:         prNumber,
			HeadSHA:          pull.Head.SHA,
			OverallRisk:      reviewResult.OverallRisk,
			TrustLevel:       entry.TrustLevel,
			ActionTaken:      action.ActionTaken,
			ActionStatus:     "failed",
			ActionDetails:    err.Error(),
			StageDurationsMS: recorder.Durations(),
			TotalDurationMS:  recorder.TotalMS(),
		}, err
	}

	_ = s.Storage.DeleteConflictRetry(repoFullName, prNumber)
	if finalCommentBody := render.FinalStatusComment(render.ReviewOutcome{
		ActionTaken:  action.ActionTaken,
		ActionStatus: action.ActionStatus,
	}); finalCommentBody != "" {
		done = recorder.Start("final_comment", fmt.Sprintf("publishing final status comment to %s #%d", repoFullName, prNumber))
		_, commentErr := s.GitHub.CreateIssueComment(repoFullName, prNumber, finalCommentBody)
		done(commentErr)
		if commentErr != nil {
			return Result{StageDurationsMS: recorder.Durations(), TotalDurationMS: recorder.TotalMS()}, commentErr
		}
	}

	emitProgress(progress, "summary", recorder.Summary())
	return Result{
		OK:               true,
		RepoFullName:     repoFullName,
		PRNumber:         prNumber,
		HeadSHA:          pull.Head.SHA,
		OverallRisk:      reviewResult.OverallRisk,
		TrustLevel:       entry.TrustLevel,
		ActionTaken:      action.ActionTaken,
		ActionStatus:     action.ActionStatus,
		ActionDetails:    action.ActionDetails,
		StageDurationsMS: recorder.Durations(),
		TotalDurationMS:  recorder.TotalMS(),
	}, nil
}

func (s *Service) reviewPull(repoFullName string, prNumber int, headSHA, triggerEvent string, force bool, progress ProgressFunc) (Result, error) {
	return s.reviewPullWithRecorder(repoFullName, prNumber, headSHA, triggerEvent, force, progress, nil)
}

func (s *Service) reviewPullWithRecorder(repoFullName string, prNumber int, headSHA, triggerEvent string, force bool, progress ProgressFunc, recorder *stageRecorder) (Result, error) {
	if recorder == nil {
		recorder = newStageRecorder(progress)
	}

	if !force {
		done := recorder.Start("dedupe", fmt.Sprintf("checking duplicate run for %s #%d @ %s", repoFullName, prNumber, headSHA))
		hasRun, err := s.Storage.HasReviewRun(repoFullName, prNumber, headSHA)
		done(err)
		if err != nil {
			return Result{StageDurationsMS: recorder.Durations(), TotalDurationMS: recorder.TotalMS()}, err
		}
		if hasRun {
			return Result{
				Ignored:          true,
				Reason:           "duplicate_review_run",
				RepoFullName:     repoFullName,
				PRNumber:         prNumber,
				HeadSHA:          headSHA,
				StageDurationsMS: recorder.Durations(),
				TotalDurationMS:  recorder.TotalMS(),
			}, nil
		}
	}

	done := recorder.Start("pull", fmt.Sprintf("fetching pull request details for %s #%d", repoFullName, prNumber))
	pull, err := s.GitHub.GetPull(repoFullName, prNumber)
	done(err)
	if err != nil {
		return Result{StageDurationsMS: recorder.Durations(), TotalDurationMS: recorder.TotalMS()}, err
	}

	done = recorder.Start("files", fmt.Sprintf("loading changed files for %s #%d", repoFullName, prNumber))
	files, err := s.GitHub.ListPullFiles(repoFullName, prNumber)
	done(err)
	if err != nil {
		return Result{StageDurationsMS: recorder.Durations(), TotalDurationMS: recorder.TotalMS()}, err
	}

	done = recorder.Start("ci", fmt.Sprintf("loading commit status for %s", headSHA))
	status, err := s.GitHub.GetCommitStatus(repoFullName, headSHA)
	done(err)
	if err != nil {
		return Result{StageDurationsMS: recorder.Durations(), TotalDurationMS: recorder.TotalMS()}, err
	}

	context := buildReviewContext(pull, files, status)

	done = recorder.Start("review", fmt.Sprintf("running model review on %d changed files", len(files)))
	reviewResult, err := s.Agent.Review(context)
	done(err)
	if err != nil {
		return Result{StageDurationsMS: recorder.Durations(), TotalDurationMS: recorder.TotalMS()}, err
	}

	commentBody := render.ReviewComment(reviewResult, s.ReviewCommentMarker, render.ReviewOutcome{})

	done = recorder.Start("comment", fmt.Sprintf("publishing review comment to %s #%d", repoFullName, prNumber))
	comment, err := s.GitHub.UpsertManagedReviewComment(repoFullName, prNumber, commentBody)
	done(err)
	if err != nil {
		return Result{StageDurationsMS: recorder.Durations(), TotalDurationMS: recorder.TotalMS()}, err
	}

	done = recorder.Start("action", "evaluating automatic next action")
	action, err := s.handlePostReviewAction(repoFullName, prNumber, pull, status, reviewResult)
	done(err)
	if err != nil {
		return Result{
			RepoFullName:     repoFullName,
			PRNumber:         prNumber,
			HeadSHA:          headSHA,
			OverallRisk:      reviewResult.OverallRisk,
			TrustLevel:       action.TrustLevel,
			ActionTaken:      action.ActionTaken,
			ActionStatus:     "failed",
			ActionDetails:    err.Error(),
			StageDurationsMS: recorder.Durations(),
			TotalDurationMS:  recorder.TotalMS(),
		}, err
	}

	if finalCommentBody := render.FinalStatusComment(render.ReviewOutcome{
		ActionTaken:  action.ActionTaken,
		ActionStatus: action.ActionStatus,
	}); finalCommentBody != "" {
		done = recorder.Start("final_comment", fmt.Sprintf("publishing final status comment to %s #%d", repoFullName, prNumber))
		_, err = s.GitHub.CreateIssueComment(repoFullName, prNumber, finalCommentBody)
		done(err)
		if err != nil {
			return Result{
				RepoFullName:     repoFullName,
				PRNumber:         prNumber,
				HeadSHA:          headSHA,
				OverallRisk:      reviewResult.OverallRisk,
				TrustLevel:       action.TrustLevel,
				ActionTaken:      action.ActionTaken,
				ActionStatus:     "failed",
				ActionDetails:    err.Error(),
				StageDurationsMS: recorder.Durations(),
				TotalDurationMS:  recorder.TotalMS(),
			}, err
		}
	}

	done = recorder.Start("store", "saving review run record")
	err = s.Storage.SaveReviewRun(storage.ReviewRun{
		RepoFullName:        repoFullName,
		PRNumber:            prNumber,
		HeadSHA:             headSHA,
		TriggerEvent:        triggerEvent,
		Status:              "completed",
		Provider:            reviewResult.Provider,
		Summary:             reviewResult.Summary,
		OverallRisk:         reviewResult.OverallRisk,
		Confidence:          reviewResult.Confidence,
		TrustLevel:          action.TrustLevel,
		MergeReadiness:      reviewResult.MergeReadiness,
		ActionTaken:         action.ActionTaken,
		ActionStatus:        action.ActionStatus,
		ActionDetails:       action.ActionDetails,
		StageDurationsMS:    recorder.Durations(),
		RawResultJSON:       reviewResult,
		RenderedCommentBody: commentBody,
		CommentID:           comment.ID,
		CreatedAt:           time.Now().UTC().Format(time.RFC3339),
	})
	done(err)
	if err != nil {
		return Result{StageDurationsMS: recorder.Durations(), TotalDurationMS: recorder.TotalMS()}, err
	}

	emitProgress(progress, "summary", recorder.Summary())

	return Result{
		OK:               true,
		RepoFullName:     repoFullName,
		PRNumber:         prNumber,
		HeadSHA:          headSHA,
		OverallRisk:      reviewResult.OverallRisk,
		TrustLevel:       action.TrustLevel,
		ActionTaken:      action.ActionTaken,
		ActionStatus:     action.ActionStatus,
		ActionDetails:    action.ActionDetails,
		StageDurationsMS: recorder.Durations(),
		TotalDurationMS:  recorder.TotalMS(),
	}, nil
}

func (s *Service) handlePostReviewAction(repoFullName string, prNumber int, pull github.Pull, status github.CommitStatus, reviewResult review.Result) (autoAction, error) {
	trustLevel := decideTrustLevel(pull, status, reviewResult)
	action := autoAction{
		TrustLevel:    trustLevel,
		ActionTaken:   "request_user_intervention",
		ActionStatus:  "pending_user_input",
		ActionDetails: "automated review requires explicit user intervention via CLI",
	}

	switch trustLevel {
	case trustTrusted:
		if pull.Mergeable != nil && *pull.Mergeable {
			if err := s.mergeWithRepositoryRules(repoFullName, prNumber, pull.Title); err != nil {
				if isMergePermissionError(err) {
					body := "## PR Agent Action Required\n\n该 PR 已通过自动审核并满足自动合并条件，但当前服务使用的 GitHub Token 没有执行 merge 的权限。请更新 Token 权限后重试，或由人工手动合并。"
					_, _ = s.GitHub.CreateIssueComment(repoFullName, prNumber, body)
					return autoAction{
						TrustLevel:    trustLevel,
						ActionTaken:   "merge",
						ActionStatus:  "merge_permission_denied",
						ActionDetails: err.Error(),
					}, nil
				}
				if isApprovalRequiredError(err) {
					body := "## PR Agent Action Required\n\n该 PR 已通过自动审核，但当前仓库规则要求至少 1 个具备写权限的 Approving Review。Agent 已尝试自动补充审批，但仍未满足仓库规则，请由具备写权限的 reviewer 手动 approve 后再合并。"
					_, _ = s.GitHub.CreateIssueComment(repoFullName, prNumber, body)
					return autoAction{
						TrustLevel:    trustLevel,
						ActionTaken:   "merge",
						ActionStatus:  "awaiting_required_review",
						ActionDetails: err.Error(),
					}, nil
				}
				return autoAction{
					TrustLevel:    trustLevel,
					ActionTaken:   "merge",
					ActionStatus:  "failed",
					ActionDetails: err.Error(),
				}, err
			}
			return autoAction{
				TrustLevel:    trustLevel,
				ActionTaken:   "merge",
				ActionStatus:  "merged",
				ActionDetails: "trusted PR merged automatically",
			}, nil
		}
		if pull.MergeableState == "behind" {
			trustLevel = trustTrustedNeedsSync
		} else {
			trustLevel = trustTrustedConflict
		}
	case trustTrustedNeedsSync:
		if err := s.GitHub.UpdateBranch(repoFullName, prNumber); err != nil {
			return autoAction{
				TrustLevel:    trustLevel,
				ActionTaken:   "update_branch",
				ActionStatus:  "failed",
				ActionDetails: err.Error(),
			}, err
		}
		return autoAction{
			TrustLevel:    trustLevel,
			ActionTaken:   "update_branch",
			ActionStatus:  "branch_update_requested",
			ActionDetails: "trusted PR was behind base branch; requested GitHub update-branch",
		}, nil
	case trustTrustedConflict:
		outcome, err := s.resolveConflictsForPull(repoFullName, prNumber, pull, reviewResult, true)
		if err != nil {
			return autoAction{
				TrustLevel:    trustLevel,
				ActionTaken:   "resolve_conflicts",
				ActionStatus:  "failed",
				ActionDetails: err.Error(),
			}, err
		}
		return s.completeConflictOutcome(repoFullName, prNumber, pull, outcome, trustLevel)
	default:
		body := "## PR Agent Action Required\n\n该 PR 当前不满足自动合并条件。请使用 CLI 的 `check owner/repo pr_number` 输入处理意见，由 Agent 继续执行后续动作。"
		if _, err := s.GitHub.CreateIssueComment(repoFullName, prNumber, body); err != nil {
			return autoAction{
				TrustLevel:    trustNeedsUserIntervention,
				ActionTaken:   "request_user_intervention",
				ActionStatus:  "failed",
				ActionDetails: err.Error(),
			}, err
		}
		return action, nil
	}

	if pull.MergeableState == "behind" {
		if err := s.GitHub.UpdateBranch(repoFullName, prNumber); err != nil {
			return autoAction{
				TrustLevel:    trustTrustedNeedsSync,
				ActionTaken:   "update_branch",
				ActionStatus:  "failed",
				ActionDetails: err.Error(),
			}, err
		}
		return autoAction{
			TrustLevel:    trustTrustedNeedsSync,
			ActionTaken:   "update_branch",
			ActionStatus:  "branch_update_requested",
			ActionDetails: "trusted PR needed branch sync before merge",
		}, nil
	}

	outcome, err := s.resolveConflictsForPull(repoFullName, prNumber, pull, reviewResult, true)
	if err != nil {
		return autoAction{
			TrustLevel:    trustTrustedConflict,
			ActionTaken:   "resolve_conflicts",
			ActionStatus:  "failed",
			ActionDetails: err.Error(),
		}, err
	}
	return s.completeConflictOutcome(repoFullName, prNumber, pull, outcome, trustTrustedConflict)
}

func decideTrustLevel(pull github.Pull, status github.CommitStatus, reviewResult review.Result) string {
	if pull.Draft {
		return trustNeedsUserIntervention
	}
	// GitHub may report a non-success aggregate state even when no status checks exist.
	// We only treat CI as blocking when there are actual reported checks.
	if len(status.Statuses) > 0 && status.State != "" && status.State != "success" {
		return trustNeedsUserIntervention
	}
	if reviewResult.OverallRisk != "low" {
		return trustNeedsUserIntervention
	}
	if reviewResult.MergeReadiness != "ready_for_manual_approval" {
		return trustNeedsUserIntervention
	}
	// Trust decisions are driven by risk and confidence only.
	if !reviewResult.ConfidenceSet || reviewResult.Confidence < 0.35 {
		return trustNeedsUserIntervention
	}

	if pull.Mergeable != nil && *pull.Mergeable {
		return trustTrusted
	}
	if pull.MergeableState == "behind" {
		return trustTrustedNeedsSync
	}
	return trustTrustedConflict
}

func isSupportedEvent(eventName, action string) bool {
	if eventName != "pull_request" {
		return false
	}
	switch action {
	case "opened", "synchronize", "ready_for_review", "reopened":
		return true
	default:
		return false
	}
}

func emitProgress(progress ProgressFunc, stage string, details string) {
	if progress != nil {
		progress(stage, details)
	}
}

func isMergePermissionError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "resource not accessible by personal access token") ||
		strings.Contains(message, "resource not accessible by integration") ||
		(strings.Contains(message, "403") && strings.Contains(message, "merge"))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func reviewResultFromRun(run storage.ReviewRun) review.Result {
	result := review.Result{
		Summary:         run.Summary,
		OverallRisk:     run.OverallRisk,
		Confidence:      run.Confidence,
		MergeReadiness:  run.MergeReadiness,
		Provider:        run.Provider,
		Findings:        []review.Finding{},
		Strengths:       []string{},
		TestSuggestions: []string{},
	}

	payload, ok := run.RawResultJSON.(map[string]any)
	if !ok {
		return result
	}

	if findings, ok := payload["findings"].([]any); ok {
		result.Findings = make([]review.Finding, 0, len(findings))
		for _, item := range findings {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			result.Findings = append(result.Findings, review.Finding{
				Severity:   stringFromAny(entry["severity"]),
				File:       stringFromAny(entry["file"]),
				Title:      stringFromAny(entry["title"]),
				Detail:     stringFromAny(entry["detail"]),
				Suggestion: stringFromAny(entry["suggestion"]),
			})
		}
	}
	if strengths, ok := payload["strengths"].([]any); ok {
		for _, item := range strengths {
			if value := stringFromAny(item); value != "" {
				result.Strengths = append(result.Strengths, value)
			}
		}
	}
	if suggestions, ok := payload["test_suggestions"].([]any); ok {
		for _, item := range suggestions {
			if value := stringFromAny(item); value != "" {
				result.TestSuggestions = append(result.TestSuggestions, value)
			}
		}
	}
	return result
}

func stringFromAny(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case float64:
		return fmt.Sprintf("%v", typed)
	default:
		return ""
	}
}

func buildReviewContext(pull github.Pull, files []github.PullFile, status github.CommitStatus) review.Context {
	changedFiles := make([]review.ChangedFile, 0, len(files))
	for _, file := range files {
		changedFiles = append(changedFiles, review.ChangedFile{
			Filename:  file.Filename,
			Status:    file.Status,
			Additions: file.Additions,
			Deletions: file.Deletions,
			Patch:     file.Patch,
		})
	}

	return review.Context{
		RepoFullName: pull.Base.Repo.FullName,
		PRNumber:     pull.Number,
		Title:        pull.Title,
		Body:         pull.Body,
		Author:       pull.User.Login,
		BaseRef:      pull.Base.Ref,
		HeadRef:      pull.Head.Ref,
		HeadSHA:      pull.Head.SHA,
		Draft:        pull.Draft,
		ChangedFiles: changedFiles,
		CI: review.CIStatus{
			State:      status.State,
			TotalCount: len(status.Statuses),
		},
	}
}

func (s *Service) resolveConflictsForPull(repoFullName string, prNumber int, pull github.Pull, reviewResult review.Result, allowAutoResolve bool) (conflict.Outcome, error) {
	if s.ConflictResolver == nil {
		body := "## PR Agent Action Required\n\n检测到合并冲突，但当前服务未配置冲突处理器。请手动处理冲突后重试。"
		if _, err := s.GitHub.CreateIssueComment(repoFullName, prNumber, body); err != nil {
			return conflict.Outcome{}, err
		}
		return conflict.Outcome{
			Mode: conflict.ModeSuggestOnly,
			ConflictSummary: review.ConflictSummary{
				Summary: "服务未配置冲突处理器。",
				Suggestions: []string{
					"先在本地或 GitHub 上手动解决冲突。",
					"解决后重新运行 review 或等待 webhook 重试。",
				},
			},
		}, nil
	}

	mode := conflict.ModeSuggestOnly
	if allowAutoResolve {
		mode = conflict.ModeAutoResolve
	}
	outcome, err := s.ConflictResolver.Resolve(pull, reviewResult, mode)
	if err != nil {
		var retryable conflict.RetryableError
		if errors.As(err, &retryable) {
			_ = s.cacheConflictRetry(repoFullName, prNumber, pull, reviewResult, allowAutoResolve, retryable)
			body := fmt.Sprintf("## PR Agent Action Required\n\n冲突处理在 `%s` 步骤因网络或超时失败，已缓存当前状态。请稍后使用 `recheck owner/repo pr_number` 从冲突处理步骤继续。\n\n错误信息：`%s`", retryable.Step, retryable.Message)
			_, _ = s.GitHub.CreateIssueComment(repoFullName, prNumber, body)
		}
		return conflict.Outcome{}, err
	}
	return outcome, nil
}

func (s *Service) completeConflictOutcome(repoFullName string, prNumber int, pull github.Pull, outcome conflict.Outcome, trustLevel string) (autoAction, error) {
	if outcome.MergeClean || outcome.AutoResolved {
		refreshedPull, mergeErr := s.waitForMergeablePull(repoFullName, prNumber)
		if mergeErr == nil && refreshedPull.Mergeable != nil && *refreshedPull.Mergeable {
			if err := s.mergeWithRepositoryRules(repoFullName, prNumber, pull.Title); err == nil {
				return autoAction{
					TrustLevel:    trustLevel,
					ActionTaken:   "merge",
					ActionStatus:  "merged",
					ActionDetails: "conflicts resolved and PR merged automatically",
				}, nil
			} else if isMergePermissionError(err) {
				body := "## PR Agent Action Required\n\n冲突已处理并回推到 PR 分支，但当前 GitHub Token 没有执行 merge 的权限。请更新 Token 权限后重试，或由人工手动合并。"
				_, _ = s.GitHub.CreateIssueComment(repoFullName, prNumber, body)
				return autoAction{
					TrustLevel:    trustLevel,
					ActionTaken:   "merge",
					ActionStatus:  "merge_permission_denied",
					ActionDetails: err.Error(),
				}, nil
			} else if isApprovalRequiredError(err) {
				body := "## PR Agent Action Required\n\n冲突已处理并回推到 PR 分支，但当前仓库规则要求至少 1 个具备写权限的 Approving Review。Agent 已尝试自动补充审批，但仍未满足仓库规则，请由具备写权限的 reviewer 手动 approve 后再合并。"
				_, _ = s.GitHub.CreateIssueComment(repoFullName, prNumber, body)
				return autoAction{
					TrustLevel:    trustLevel,
					ActionTaken:   "merge",
					ActionStatus:  "awaiting_required_review",
					ActionDetails: err.Error(),
				}, nil
			}
		}

		action := autoAction{
			TrustLevel:    trustLevel,
			ActionTaken:   "resolve_conflicts",
			ActionStatus:  "branch_updated",
			ActionDetails: "conflicts resolved in temporary workspace and pushed to PR branch",
		}
		if len(outcome.ResolutionSummaries) > 0 {
			action.ActionDetails = strings.Join(outcome.ResolutionSummaries, " | ")
		}
		return action, nil
	}

	suggestionLines := make([]string, 0, len(outcome.ConflictSummary.Suggestions))
	for _, suggestion := range outcome.ConflictSummary.Suggestions {
		suggestionLines = append(suggestionLines, "- "+suggestion)
	}
	body := fmt.Sprintf("## PR Agent Conflict Review\n\n%s\n\n%s", outcome.ConflictSummary.Summary, strings.Join(suggestionLines, "\n"))
	if _, err := s.GitHub.CreateIssueComment(repoFullName, prNumber, body); err != nil {
		return autoAction{
			TrustLevel:    trustLevel,
			ActionTaken:   "request_conflict_resolution",
			ActionStatus:  "failed",
			ActionDetails: err.Error(),
		}, err
	}
	return autoAction{
		TrustLevel:    trustLevel,
		ActionTaken:   "request_conflict_resolution",
		ActionStatus:  "pending_user_input",
		ActionDetails: body,
	}, nil
}

func (s *Service) waitForMergeablePull(repoFullName string, prNumber int) (github.Pull, error) {
	var lastPull github.Pull
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		pull, err := s.GitHub.GetPull(repoFullName, prNumber)
		if err != nil {
			lastErr = err
			time.Sleep(2 * time.Second)
			continue
		}
		lastPull = pull
		if pull.Mergeable != nil && *pull.Mergeable {
			return pull, nil
		}
		time.Sleep(2 * time.Second)
	}
	if lastErr != nil {
		return github.Pull{}, lastErr
	}
	return lastPull, fmt.Errorf("pull request did not become mergeable after conflict resolution")
}

func (s *Service) mergeWithRepositoryRules(repoFullName string, prNumber int, pullTitle string) error {
	commitTitle := fmt.Sprintf("Merge PR #%d: %s", prNumber, pullTitle)
	if err := s.GitHub.MergePull(repoFullName, prNumber, commitTitle); err != nil {
		if isApprovalRequiredError(err) {
			if approveErr := s.GitHub.ApprovePullReview(repoFullName, prNumber, "Automated approval for a trusted PR that passed PR Agent review."); approveErr != nil {
				return fmt.Errorf("%w; approve attempt failed: %v", err, approveErr)
			}
			var retryErr error
			for attempt := 0; attempt < 3; attempt++ {
				time.Sleep(time.Duration(attempt+1) * mergeRetryDelayBase)
				retryErr = s.GitHub.MergePull(repoFullName, prNumber, commitTitle)
				if retryErr == nil {
					return nil
				}
				if !isApprovalRequiredError(retryErr) {
					return retryErr
				}
			}
			return retryErr
		}
		return err
	}
	return nil
}

func isApprovalRequiredError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "approving review is required") ||
		strings.Contains(message, "at least 1 approving review is required") ||
		strings.Contains(message, "repository rule violations found")
}
func (s *Service) cacheConflictRetry(repoFullName string, prNumber int, pull github.Pull, reviewResult review.Result, allowAutoResolve bool, retryable conflict.RetryableError) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return s.Storage.SaveConflictRetry(storage.ConflictRetry{
		RepoFullName:     repoFullName,
		PRNumber:         prNumber,
		HeadSHA:          pull.Head.SHA,
		TrustLevel:       decideTrustLevel(pull, github.CommitStatus{}, reviewResult),
		AllowAutoResolve: allowAutoResolve,
		Pull:             pull,
		ReviewResult:     reviewResult,
		FailedStep:       retryable.Step,
		ErrorMessage:     retryable.Message,
		CreatedAt:        now,
		UpdatedAt:        now,
	})
}

func decodeConflictRetry(entry storage.ConflictRetry) (github.Pull, review.Result, error) {
	var pull github.Pull
	var result review.Result
	pullBytes, err := json.Marshal(entry.Pull)
	if err != nil {
		return github.Pull{}, review.Result{}, err
	}
	if err := json.Unmarshal(pullBytes, &pull); err != nil {
		return github.Pull{}, review.Result{}, err
	}
	resultBytes, err := json.Marshal(entry.ReviewResult)
	if err != nil {
		return github.Pull{}, review.Result{}, err
	}
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		return github.Pull{}, review.Result{}, err
	}
	return pull, result, nil
}