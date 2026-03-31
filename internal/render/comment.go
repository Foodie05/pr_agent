package render

import (
	"fmt"
	"strings"

	"pr-agent-go/internal/review"
)

type ReviewOutcome struct {
	ActionTaken  string
	ActionStatus string
}

func ReviewComment(result review.Result, marker string, outcome ReviewOutcome) string {
	findings := "未发现需要明确指出的问题，但仍建议结合业务上下文进行人工复核。"
	if len(result.Findings) > 0 {
		parts := make([]string, 0, len(result.Findings))
		for i, finding := range result.Findings {
			part := fmt.Sprintf("%d. `%s` - **%s**\n%s", i+1, finding.File, finding.Title, finding.Detail)
			if finding.Suggestion != "" {
				part += "\n建议：" + finding.Suggestion
			}
			parts = append(parts, part)
		}
		findings = strings.Join(parts, "\n\n")
	}

	strengths := "- 无"
	if len(result.Strengths) > 0 {
		lines := make([]string, 0, len(result.Strengths))
		for _, item := range result.Strengths {
			lines = append(lines, "- "+item)
		}
		strengths = strings.Join(lines, "\n")
	}

	testSuggestions := "- 无"
	if len(result.TestSuggestions) > 0 {
		lines := make([]string, 0, len(result.TestSuggestions))
		for _, item := range result.TestSuggestions {
			lines = append(lines, "- "+item)
		}
		testSuggestions = strings.Join(lines, "\n")
	}

	return fmt.Sprintf(`%s
## PR Agent Review

### Summary
%s

### Risk
%s | confidence=%.2f

### Findings
%s

### Strengths
%s

### Test Suggestions
%s

### Note
这是自动化审核摘要，不代替最终人工审批。`, marker, result.Summary, strings.ToUpper(result.OverallRisk), result.Confidence, findings, strengths, testSuggestions)
}

func FinalStatusComment(outcome ReviewOutcome) string {
	statusMessage := reviewStatusMessage(outcome)
	if statusMessage == "" {
		return ""
	}
	return fmt.Sprintf("## PR Agent Final Update\n\n%s", statusMessage)
}

func reviewStatusMessage(outcome ReviewOutcome) string {
	switch outcome.ActionStatus {
	case "merged":
		return "Accepted. Thank you for your contribution!"
	case "merge_permission_denied":
		return "Accepted for merge, but the current GitHub token does not have permission to complete the merge automatically."
	case "branch_update_requested":
		return "Accepted in principle. The PR branch is being updated before merge."
	default:
		return ""
	}
}
