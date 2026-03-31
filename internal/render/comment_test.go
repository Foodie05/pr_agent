package render

import (
	"strings"
	"testing"

	"pr-agent-go/internal/review"
)

func TestFinalStatusCommentShowsAcceptedStatus(t *testing.T) {
	body := FinalStatusComment(ReviewOutcome{
		ActionTaken:  "merge",
		ActionStatus: "merged",
	})

	if !strings.Contains(body, "Accepted. Thank you for your contribution!") {
		t.Fatalf("expected accepted status message, got %q", body)
	}
}

func TestFinalStatusCommentAcceptsCompletedMergeFallback(t *testing.T) {
	body := FinalStatusComment(ReviewOutcome{
		ActionTaken:  "merge",
		ActionStatus: "completed",
	})

	if !strings.Contains(body, "Accepted. Thank you for your contribution!") {
		t.Fatalf("expected accepted status message for completed merge fallback, got %q", body)
	}
}

func TestReviewCommentUsesUnavailableWhenConfidenceMissing(t *testing.T) {
	body := ReviewComment(review.Result{
		Summary:         "summary",
		OverallRisk:     "low",
		MergeReadiness:  "ready_for_manual_approval",
		Findings:        []review.Finding{},
		Strengths:       []string{},
		TestSuggestions: []string{},
	}, "<!-- marker -->", ReviewOutcome{})

	if !strings.Contains(body, "confidence=unavailable") {
		t.Fatalf("expected unavailable confidence label, got %q", body)
	}
}
