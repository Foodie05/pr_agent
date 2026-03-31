package render

import (
	"strings"
	"testing"
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
