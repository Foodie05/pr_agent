package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	maxRequestAttempts = 3
	retryDelay         = 300 * time.Millisecond
)

type Client struct {
	BaseURL             string
	Token               string
	ReviewCommentMarker string
	HTTPClient          *http.Client
}

type Pull struct {
	NodeID         string `json:"node_id"`
	Number         int    `json:"number"`
	Title          string `json:"title"`
	Body           string `json:"body"`
	Draft          bool   `json:"draft"`
	Mergeable      *bool  `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
	User           struct {
		Login string `json:"login"`
	} `json:"user"`
	Base struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
			CloneURL string `json:"clone_url"`
		} `json:"repo"`
	} `json:"base"`
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
			CloneURL string `json:"clone_url"`
		} `json:"repo"`
	} `json:"head"`
}

type PullFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch"`
}

type CommitStatus struct {
	State    string     `json:"state"`
	Statuses []struct{} `json:"statuses"`
}

type IssueComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

type RepositoryWebhook struct {
	ID     int64    `json:"id"`
	Active bool     `json:"active"`
	Events []string `json:"events"`
	Config struct {
		URL         string `json:"url"`
		ContentType string `json:"content_type"`
		InsecureSSL string `json:"insecure_ssl"`
	} `json:"config"`
}

func New(baseURL, token, marker string) *Client {
	return &Client{
		BaseURL:             strings.TrimRight(baseURL, "/"),
		Token:               token,
		ReviewCommentMarker: marker,
		HTTPClient:          &http.Client{},
	}
}

func (c *Client) CheckConnection() error {
	if c.Token == "" {
		return fmt.Errorf("github token is empty")
	}

	var payload map[string]any
	if err := c.requestJSON(http.MethodGet, "/rate_limit", nil, &payload); err != nil {
		return err
	}
	return nil
}

func (c *Client) GetPull(repoFullName string, prNumber int) (Pull, error) {
	var pull Pull
	err := c.requestJSON(http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repoFullName, prNumber), nil, &pull)
	return pull, err
}

func (c *Client) ListPullFiles(repoFullName string, prNumber int) ([]PullFile, error) {
	files := []PullFile{}
	page := 1
	for {
		var batch []PullFile
		path := fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=100&page=%d", repoFullName, prNumber, page)
		if err := c.requestJSON(http.MethodGet, path, nil, &batch); err != nil {
			return nil, err
		}
		files = append(files, batch...)
		if len(batch) < 100 {
			break
		}
		page++
	}
	return files, nil
}

func (c *Client) GetCommitStatus(repoFullName, ref string) (CommitStatus, error) {
	var status CommitStatus
	err := c.requestJSON(http.MethodGet, fmt.Sprintf("/repos/%s/commits/%s/status", repoFullName, ref), nil, &status)
	return status, err
}

func (c *Client) ListIssueComments(repoFullName string, issueNumber int) ([]IssueComment, error) {
	var comments []IssueComment
	err := c.requestJSON(http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", repoFullName, issueNumber), nil, &comments)
	return comments, err
}

func (c *Client) ListRepositoryWebhooks(repoFullName string) ([]RepositoryWebhook, error) {
	var hooks []RepositoryWebhook
	err := c.requestJSON(http.MethodGet, fmt.Sprintf("/repos/%s/hooks?per_page=100", repoFullName), nil, &hooks)
	return hooks, err
}

func (c *Client) CreateRepositoryWebhook(repoFullName, webhookURL, secret string, events []string) (RepositoryWebhook, error) {
	payload := map[string]any{
		"name":   "web",
		"active": true,
		"events": events,
		"config": map[string]string{
			"url":          webhookURL,
			"content_type": "json",
			"insecure_ssl": "0",
			"secret":       secret,
		},
	}
	var hook RepositoryWebhook
	err := c.requestJSON(http.MethodPost, fmt.Sprintf("/repos/%s/hooks", repoFullName), payload, &hook)
	return hook, err
}

func (c *Client) UpdateRepositoryWebhook(repoFullName string, hookID int64, webhookURL, secret string, events []string) (RepositoryWebhook, error) {
	payload := map[string]any{
		"active": true,
		"events": events,
		"config": map[string]string{
			"url":          webhookURL,
			"content_type": "json",
			"insecure_ssl": "0",
			"secret":       secret,
		},
	}
	var hook RepositoryWebhook
	err := c.requestJSON(http.MethodPatch, fmt.Sprintf("/repos/%s/hooks/%d", repoFullName, hookID), payload, &hook)
	return hook, err
}

func (c *Client) UpsertManagedReviewComment(repoFullName string, issueNumber int, body string) (IssueComment, error) {
	comments, err := c.ListIssueComments(repoFullName, issueNumber)
	if err != nil {
		return IssueComment{}, err
	}

	for _, comment := range comments {
		if strings.Contains(comment.Body, c.ReviewCommentMarker) {
			return c.updateIssueComment(repoFullName, comment.ID, body)
		}
	}

	return c.createIssueComment(repoFullName, issueNumber, body)
}

func (c *Client) CreateIssueComment(repoFullName string, issueNumber int, body string) (IssueComment, error) {
	return c.createIssueComment(repoFullName, issueNumber, body)
}

func (c *Client) MergePull(repoFullName string, prNumber int, commitTitle string) error {
	payload := map[string]string{
		"merge_method": "squash",
	}
	if commitTitle != "" {
		payload["commit_title"] = commitTitle
	}
	return c.requestJSON(http.MethodPut, fmt.Sprintf("/repos/%s/pulls/%d/merge", repoFullName, prNumber), payload, nil)
}

func (c *Client) ApprovePullReview(repoFullName string, prNumber int, body string) error {
	payload := map[string]string{
		"event": "APPROVE",
		"body":  body,
	}
	return c.requestJSON(http.MethodPost, fmt.Sprintf("/repos/%s/pulls/%d/reviews", repoFullName, prNumber), payload, nil)
}

func (c *Client) MarkPullReadyForReview(nodeID string) error {
	if strings.TrimSpace(nodeID) == "" {
		return fmt.Errorf("missing pull request node id")
	}

	payload := map[string]any{
		"query": `mutation($pullRequestId: ID!) {
  markPullRequestReadyForReview(input: { pullRequestId: $pullRequestId }) {
    pullRequest {
      id
      isDraft
    }
  }
}`,
		"variables": map[string]any{
			"pullRequestId": nodeID,
		},
	}

	var response struct {
		Data struct {
			MarkPullRequestReadyForReview struct {
				PullRequest struct {
					ID      string `json:"id"`
					IsDraft bool   `json:"isDraft"`
				} `json:"pullRequest"`
			} `json:"markPullRequestReadyForReview"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := c.requestGraphQL(payload, &response); err != nil {
		return err
	}
	if len(response.Errors) > 0 {
		return fmt.Errorf("github graphql: %s", response.Errors[0].Message)
	}
	if response.Data.MarkPullRequestReadyForReview.PullRequest.IsDraft {
		return fmt.Errorf("pull request is still draft after ready-for-review mutation")
	}
	return nil
}

func (c *Client) UpdateBranch(repoFullName string, prNumber int) error {
	payload := map[string]string{}
	return c.requestJSON(http.MethodPut, fmt.Sprintf("/repos/%s/pulls/%d/update-branch", repoFullName, prNumber), payload, nil)
}

func (c *Client) createIssueComment(repoFullName string, issueNumber int, body string) (IssueComment, error) {
	payload := map[string]string{"body": body}
	var comment IssueComment
	err := c.requestJSON(http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repoFullName, issueNumber), payload, &comment)
	return comment, err
}

func (c *Client) updateIssueComment(repoFullName string, commentID int64, body string) (IssueComment, error) {
	payload := map[string]string{"body": body}
	var comment IssueComment
	err := c.requestJSON(http.MethodPatch, fmt.Sprintf("/repos/%s/issues/comments/%d", repoFullName, commentID), payload, &comment)
	return comment, err
}

func (c *Client) requestJSON(method, path string, payload any, target any) error {
	if c.Token == "" {
		return fmt.Errorf("missing GITHUB_TOKEN")
	}

	var rawBody []byte
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		rawBody = data
	}

	var lastErr error
	for attempt := 1; attempt <= maxRequestAttempts; attempt++ {
		var body io.Reader
		if rawBody != nil {
			body = bytes.NewReader(rawBody)
		}

		req, err := http.NewRequest(method, c.BaseURL+path, body)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("User-Agent", "pr-agent-go")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			lastErr = err
			if shouldRetry(err) && attempt < maxRequestAttempts {
				time.Sleep(retryDelay)
				continue
			}
			return err
		}

		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			if shouldRetry(readErr) && attempt < maxRequestAttempts {
				time.Sleep(retryDelay)
				continue
			}
			return readErr
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("github api %d: %s", resp.StatusCode, string(respBody))
		}
		if target == nil || len(respBody) == 0 {
			return nil
		}
		return json.Unmarshal(respBody, target)
	}

	return lastErr
}

func (c *Client) requestGraphQL(payload any, target any) error {
	if c.Token == "" {
		return fmt.Errorf("missing GITHUB_TOKEN")
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, c.BaseURL+"/graphql", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("User-Agent", "pr-agent-go")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("github graphql %d: %s", resp.StatusCode, string(body))
	}
	if target == nil || len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, target)
}

func shouldRetry(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(message, "eof") ||
		strings.Contains(message, "connection reset by peer") ||
		strings.Contains(message, "unexpected eof")
}
