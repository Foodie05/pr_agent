package review

import "testing"

func TestNormalizeResultFiltersEmptyFindings(t *testing.T) {
	result := normalizeResult(Result{
		Summary:        "summary",
		OverallRisk:    "medium",
		Confidence:     0.5,
		ConfidenceSet:  true,
		MergeReadiness: "needs_human_review",
		Findings: []Finding{
			{Severity: "medium"},
			{Severity: "low", File: "a.go", Title: "keep me"},
		},
	})

	if len(result.Findings) != 1 {
		t.Fatalf("expected 1 finding after filtering, got %d", len(result.Findings))
	}
	if result.Findings[0].Title != "keep me" {
		t.Fatalf("expected remaining finding to be preserved, got %+v", result.Findings[0])
	}
}

func TestNormalizeResultFiltersFileOnlyFindings(t *testing.T) {
	result := normalizeResult(Result{
		Summary:        "summary",
		OverallRisk:    "low",
		MergeReadiness: "ready_for_manual_approval",
		Findings: []Finding{
			{File: "LICENSE"},
			{File: "README.md", Suggestion: "keep"},
			{File: "README.md", Title: "valid", Detail: "has content"},
		},
	})

	if len(result.Findings) != 1 {
		t.Fatalf("expected 1 finding after filtering, got %d", len(result.Findings))
	}
	if result.Findings[0].Title != "valid" {
		t.Fatalf("expected valid finding to remain, got %+v", result.Findings[0])
	}
}

func TestResultFromMapTracksMissingConfidence(t *testing.T) {
	result := resultFromMap(map[string]any{
		"summary":          "summary",
		"overall_risk":     "low",
		"findings":         []any{},
		"strengths":        []any{},
		"test_suggestions": []any{},
		"merge_readiness":  "ready_for_manual_approval",
	})

	if result.ConfidenceSet {
		t.Fatalf("expected missing confidence to remain unset")
	}
	if result.Confidence != 0 {
		t.Fatalf("expected zero confidence value when unset, got %v", result.Confidence)
	}
}

func TestHeuristicReviewSetsConfidence(t *testing.T) {
	result := heuristicReview(Context{
		ChangedFiles: []ChangedFile{{Filename: "README.md"}},
		CI:           CIStatus{State: "success"},
	})

	if !result.ConfidenceSet {
		t.Fatalf("expected heuristic review to provide confidence")
	}
	if result.Confidence <= 0 {
		t.Fatalf("expected heuristic confidence to be positive, got %v", result.Confidence)
	}
}
