package review

import "testing"

func TestNormalizeResultFiltersEmptyFindings(t *testing.T) {
	result := normalizeResult(Result{
		Summary:        "summary",
		OverallRisk:    "medium",
		Confidence:     0.5,
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
