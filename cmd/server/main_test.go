package main

import "testing"

func TestParseRepoAndPRFromGitHubURL(t *testing.T) {
	repo, number, err := parseRepoAndPR([]string{"review-pr", "https://github.com/Foodie05/psyche_project/pull/1"}, "review-pr")
	if err != nil {
		t.Fatalf("parseRepoAndPR returned error: %v", err)
	}
	if repo != "Foodie05/psyche_project" {
		t.Fatalf("expected repo Foodie05/psyche_project, got %s", repo)
	}
	if number != 1 {
		t.Fatalf("expected PR number 1, got %d", number)
	}
}
