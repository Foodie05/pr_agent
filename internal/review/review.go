package review

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Context struct {
	RepoFullName string        `json:"repo"`
	PRNumber     int           `json:"pr_number"`
	Title        string        `json:"title"`
	Body         string        `json:"body"`
	Author       string        `json:"author"`
	BaseRef      string        `json:"base_branch"`
	HeadRef      string        `json:"head_branch"`
	HeadSHA      string        `json:"head_sha"`
	Draft        bool          `json:"draft"`
	ChangedFiles []ChangedFile `json:"changed_files"`
	CI           CIStatus      `json:"ci"`
}

type ChangedFile struct {
	Filename  string `json:"path"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch"`
}

type CIStatus struct {
	State      string `json:"status"`
	TotalCount int    `json:"total_count"`
}

type Finding struct {
	Severity   string `json:"severity"`
	File       string `json:"file"`
	Title      string `json:"title"`
	Detail     string `json:"detail"`
	Suggestion string `json:"suggestion"`
}

type Result struct {
	Summary         string    `json:"summary"`
	OverallRisk     string    `json:"overall_risk"`
	Confidence      float64   `json:"confidence"`
	ConfidenceSet   bool      `json:"-"`
	OperatorGoal    string    `json:"operator_goal,omitempty"`
	Findings        []Finding `json:"findings"`
	Strengths       []string  `json:"strengths"`
	TestSuggestions []string  `json:"test_suggestions"`
	MergeReadiness  string    `json:"merge_readiness"`
	Provider        string    `json:"provider"`
}

type InterventionContext struct {
	RepoFullName   string `json:"repo"`
	PRNumber       int    `json:"pr_number"`
	Title          string `json:"title"`
	HeadSHA        string `json:"head_sha"`
	Mergeable      *bool  `json:"mergeable,omitempty"`
	MergeableState string `json:"mergeable_state,omitempty"`
	CIState        string `json:"ci_state,omitempty"`
	ReviewSummary  string `json:"review_summary,omitempty"`
	OverallRisk    string `json:"overall_risk,omitempty"`
	TrustLevel     string `json:"trust_level,omitempty"`
	UserNote       string `json:"user_note"`
}

type InterventionDecision struct {
	Action                string `json:"action"`
	Comment               string `json:"comment"`
	Summary               string `json:"summary"`
	AuthoritativeApproval bool   `json:"authoritative_approval"`
}

type ConflictContext struct {
	RepoFullName    string `json:"repo"`
	PRNumber        int    `json:"pr_number"`
	PullTitle       string `json:"pull_title"`
	FilePath        string `json:"file_path"`
	BlockIndex      int    `json:"block_index,omitempty"`
	BlockCount      int    `json:"block_count,omitempty"`
	OperatorGoal    string `json:"operator_goal,omitempty"`
	ReviewSummary   string `json:"review_summary,omitempty"`
	OverallRisk     string `json:"overall_risk,omitempty"`
	BaseContent     string `json:"base_content"`
	CurrentContent  string `json:"current_content"`
	IncomingContent string `json:"incoming_content"`
	ConflictMarkers string `json:"conflict_markers"`
}

type ConflictDecision struct {
	ResolvedContent string  `json:"resolved_content"`
	Summary         string  `json:"summary"`
	Confidence      float64 `json:"confidence"`
	ShouldApply     bool    `json:"should_apply"`
}

type ConflictBlockDecision struct {
	ResolvedBlock string  `json:"resolved_block"`
	Summary       string  `json:"summary"`
	Confidence    float64 `json:"confidence"`
	ShouldApply   bool    `json:"should_apply"`
}

type ConflictSummaryContext struct {
	RepoFullName  string                `json:"repo"`
	PRNumber      int                   `json:"pr_number"`
	PullTitle     string                `json:"pull_title"`
	OperatorGoal  string                `json:"operator_goal,omitempty"`
	ReviewSummary string                `json:"review_summary,omitempty"`
	OverallRisk   string                `json:"overall_risk,omitempty"`
	Conflicts     []ConflictFileSummary `json:"conflicts"`
}

type ConflictFileSummary struct {
	FilePath        string `json:"file_path"`
	ConflictMarkers string `json:"conflict_markers"`
}

type ConflictSummary struct {
	Summary     string   `json:"summary"`
	Suggestions []string `json:"suggestions"`
}

type Agent struct {
	APIBaseURL      string
	APIKey          string
	Model           string
	HTTPClient      *http.Client
	ReviewBatchSize int
}

func NewAgent(apiBaseURL, apiKey, model string) *Agent {
	return &Agent{
		APIBaseURL:      strings.TrimRight(apiBaseURL, "/"),
		APIKey:          apiKey,
		Model:           model,
		HTTPClient:      &http.Client{},
		ReviewBatchSize: 6,
	}
}

func (a *Agent) CheckConnection() error {
	if a.APIKey == "" {
		return fmt.Errorf("openai api key is empty")
	}

	req, err := http.NewRequest(http.MethodGet, a.APIBaseURL+"/models/"+a.Model, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.APIKey)

	resp, err := a.doRequestWithRetry("check_connection", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	modelCheckBody := strings.TrimSpace(string(body))
	if !supportsModelsEndpoint(resp.StatusCode, modelCheckBody) {
		return fmt.Errorf("model check failed: %d %s", resp.StatusCode, modelCheckBody)
	}

	compatibilityErr := a.checkChatCompletionsCompatibility()
	if compatibilityErr == nil {
		return nil
	}
	return compatibilityErr
}

func (a *Agent) checkChatCompletionsCompatibility() error {
	requestBody := map[string]any{
		"model":      a.Model,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
	}

	data, err := json.Marshal(requestBody)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, a.APIBaseURL+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.doRequestWithRetry("check_chat_completions_compatibility", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	responseBody := strings.TrimSpace(string(body))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	if strings.Contains(responseBody, "insufficient_balance") {
		return fmt.Errorf("model endpoint compatible, but account balance is insufficient")
	}

	return fmt.Errorf("chat completions compatibility check failed: %d %s", resp.StatusCode, responseBody)
}

func (a *Agent) postChatCompletion(operation string, body []byte) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, a.APIBaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.doRequestWithRetry(operation, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("llm request failed: %d %s", resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

func (a *Agent) doRequestWithRetry(operation string, req *http.Request) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		cloned := req.Clone(req.Context())
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			cloned.Body = body
		}

		resp, err := a.HTTPClient.Do(cloned)
		if err == nil {
			if shouldRetryLLMStatus(resp.StatusCode) && attempt < 2 {
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				log.Printf("llm retry op=%s attempt=%d status=%d body=%s", operation, attempt+1, resp.StatusCode, truncateForLog(string(body), 400))
				time.Sleep(time.Duration(attempt+1) * time.Second)
				continue
			}
			return resp, nil
		}

		lastErr = err
		if !IsRetryableModelError(err) || attempt == 2 {
			return nil, err
		}
		log.Printf("llm retry op=%s attempt=%d err=%v", operation, attempt+1, err)
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	return nil, lastErr
}

func shouldRetryLLMStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return statusCode >= 500 && statusCode <= 599
	}
}

func IsRetryableModelError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout() || netErr.Temporary()
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "deadline exceeded") ||
		strings.Contains(message, "timeout") ||
		strings.Contains(message, "temporarily unavailable") ||
		strings.Contains(message, "connection reset by peer") ||
		strings.Contains(message, "unexpected eof") ||
		strings.Contains(message, "eof")
}

func truncateForLog(value string, max int) string {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) <= max {
		return trimmed
	}
	return trimmed[:max]
}

func supportsModelsEndpoint(statusCode int, body string) bool {
	if statusCode == http.StatusNotFound {
		return true
	}
	lowerBody := strings.ToLower(body)
	return strings.Contains(lowerBody, "no static resource") ||
		strings.Contains(lowerBody, "not_found") ||
		strings.Contains(lowerBody, "not found")
}

func (a *Agent) Review(context Context) (Result, error) {
	if a.APIKey == "" {
		result := heuristicReview(context)
		result.Provider = "heuristic"
		return result, nil
	}

	batchSize := a.ReviewBatchSize
	if batchSize <= 0 {
		batchSize = 6
	}
	if len(context.ChangedFiles) > batchSize {
		return a.reviewInBatches(context, batchSize)
	}
	return a.reviewSingle(context)
}

func (a *Agent) reviewSingle(context Context) (Result, error) {

	systemPrompt := `You are a pull request review assistant.
You are not the final approver.
Base your output only on the provided pull request context.
Return valid JSON with keys:
summary, overall_risk, confidence, findings, strengths, test_suggestions, merge_readiness.
overall_risk must be one of: low, medium, high.
merge_readiness must be one of: needs_human_review, ready_for_manual_approval.
confidence must be a number between 0 and 1.
Do not omit any required field.`

	requestBody := map[string]any{
		"model":       a.Model,
		"temperature": 0.2,
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "pr_review_result",
				"strict": true,
				"schema": reviewResultSchema(),
			},
		},
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": mustJSON(context)},
		},
	}

	data, err := json.Marshal(requestBody)
	if err != nil {
		return Result{}, err
	}
	respBody, err := a.postChatCompletion("review", data)
	if err != nil {
		return Result{}, err
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return Result{}, err
	}
	if len(parsed.Choices) == 0 {
		return Result{}, fmt.Errorf("llm response missing choices")
	}

	raw, err := messageContentText(parsed.Choices[0].Message.Content)
	if err != nil {
		return Result{}, err
	}

	jsonPayload, err := extractJSONObject(raw)
	if err != nil {
		return Result{}, err
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(jsonPayload), &payload); err != nil {
		return Result{}, err
	}
	result := resultFromMap(payload)
	result = normalizeResult(result)
	result.Provider = a.Model
	return result, nil
}

func (a *Agent) reviewInBatches(context Context, batchSize int) (Result, error) {
	chunks := chunkChangedFiles(context.ChangedFiles, batchSize)
	results := make([]Result, 0, len(chunks))
	for index, chunk := range chunks {
		log.Printf("review batch %d/%d repo=%s pr=%d files=%d", index+1, len(chunks), context.RepoFullName, context.PRNumber, len(chunk))
		batchContext := context
		batchContext.ChangedFiles = chunk
		result, err := a.reviewSingle(batchContext)
		if err != nil {
			return Result{}, err
		}
		results = append(results, result)
	}

	log.Printf("review aggregate repo=%s pr=%d batches=%d", context.RepoFullName, context.PRNumber, len(results))
	aggregated, err := a.aggregateBatchReviews(context, results)
	if err != nil {
		return heuristicAggregateResults(context, results), nil
	}
	return aggregated, nil
}

func (a *Agent) aggregateBatchReviews(context Context, batchResults []Result) (Result, error) {
	if len(batchResults) == 1 {
		return batchResults[0], nil
	}

	if a.APIKey == "" {
		return heuristicAggregateResults(context, batchResults), nil
	}

	type batchSummary struct {
		Index           int       `json:"index"`
		Summary         string    `json:"summary"`
		OverallRisk     string    `json:"overall_risk"`
		Confidence      float64   `json:"confidence"`
		MergeReadiness  string    `json:"merge_readiness"`
		Findings        []Finding `json:"findings"`
		Strengths       []string  `json:"strengths"`
		TestSuggestions []string  `json:"test_suggestions"`
	}

	summaries := make([]batchSummary, 0, len(batchResults))
	for i, item := range batchResults {
		summaries = append(summaries, batchSummary{
			Index:           i + 1,
			Summary:         item.Summary,
			OverallRisk:     item.OverallRisk,
			Confidence:      item.Confidence,
			MergeReadiness:  item.MergeReadiness,
			Findings:        item.Findings,
			Strengths:       item.Strengths,
			TestSuggestions: item.TestSuggestions,
		})
	}

	systemPrompt := `You are aggregating multiple partial pull request review batches into one final review.
Return valid JSON with keys:
summary, overall_risk, confidence, findings, strengths, test_suggestions, merge_readiness.
overall_risk must be one of: low, medium, high.
merge_readiness must be one of: needs_human_review, ready_for_manual_approval.
confidence must be a number between 0 and 1.
Prefer preserving concrete findings. Avoid duplicate findings across batches.`

	requestBody := map[string]any{
		"model":       a.Model,
		"temperature": 0.1,
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "pr_review_result_aggregate",
				"strict": true,
				"schema": reviewResultSchema(),
			},
		},
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": mustJSON(map[string]any{
				"repo":          context.RepoFullName,
				"pr_number":     context.PRNumber,
				"title":         context.Title,
				"head_sha":      context.HeadSHA,
				"batch_reviews": summaries,
				"file_count":    len(context.ChangedFiles),
				"ci":            context.CI,
			})},
		},
	}

	data, err := json.Marshal(requestBody)
	if err != nil {
		return Result{}, err
	}
	respBody, err := a.postChatCompletion("review_aggregate", data)
	if err != nil {
		return Result{}, err
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return Result{}, err
	}
	if len(parsed.Choices) == 0 {
		return Result{}, fmt.Errorf("llm response missing choices")
	}

	raw, err := messageContentText(parsed.Choices[0].Message.Content)
	if err != nil {
		return Result{}, err
	}
	jsonPayload, err := extractJSONObject(raw)
	if err != nil {
		return Result{}, err
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(jsonPayload), &payload); err != nil {
		return Result{}, err
	}
	result := normalizeResult(resultFromMap(payload))
	result.Provider = a.Model
	return result, nil
}

func (a *Agent) ResolveIntervention(context InterventionContext) (InterventionDecision, error) {
	if a.APIKey == "" {
		return heuristicIntervention(context), nil
	}

	systemPrompt := `You are a pull request operations assistant.
Read the current PR state, the latest automated review summary, and the user's note.
Return valid JSON with keys: action, comment, summary, authoritative_approval.
Allowed action values are: merge, update_branch, comment_only, re_review.
Choose merge only when the user clearly approves merging.
If the user explicitly says to accept, directly merge, or force merge the PR, treat that note as an authoritative approval signal.
For those explicit approval cases, choose merge unless the note clearly asks for a different action, and set authoritative_approval to true.
Set authoritative_approval to false for all other cases.
Choose update_branch only when the user asks to refresh/rebase/sync the branch or when the PR is trusted and merely behind.
Choose comment_only when human action is still needed or the note is informational.
Choose re_review when the user asks to rerun or refresh the automated review.
The comment must be concise and suitable for posting on GitHub.`

	requestBody := map[string]any{
		"model":       a.Model,
		"temperature": 0.1,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": mustJSON(context)},
		},
	}

	data, err := json.Marshal(requestBody)
	if err != nil {
		return InterventionDecision{}, err
	}
	respBody, err := a.postChatCompletion("intervention", data)
	if err != nil {
		return InterventionDecision{}, err
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return InterventionDecision{}, err
	}
	if len(parsed.Choices) == 0 {
		return InterventionDecision{}, fmt.Errorf("llm response missing choices")
	}

	raw, err := messageContentText(parsed.Choices[0].Message.Content)
	if err != nil {
		return InterventionDecision{}, err
	}
	jsonPayload, err := extractJSONObject(raw)
	if err != nil {
		return InterventionDecision{}, err
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(jsonPayload), &payload); err != nil {
		return InterventionDecision{}, err
	}
	return normalizeInterventionDecision(InterventionDecision{
		Action:                stringValue(payload["action"]),
		Comment:               stringValue(payload["comment"]),
		Summary:               stringValue(payload["summary"]),
		AuthoritativeApproval: boolValue(payload["authoritative_approval"]),
	}), nil
}

func (a *Agent) ResolveConflict(context ConflictContext) (ConflictDecision, error) {
	if a.APIKey == "" {
		return ConflictDecision{}, fmt.Errorf("conflict auto-resolution requires model api key")
	}

	systemPrompt := `You are resolving a git merge conflict for a pull request.
Return valid JSON with keys: resolved_content, summary, confidence, should_apply.
Use only the provided file versions and conflict markers.
The operator_goal tells you what outcome the user wants from this intervention. Keep that goal in mind while resolving the conflict.
should_apply must be true only if you are confident the merged file is correct and complete.
resolved_content must contain the entire resolved file with no conflict markers.`

	requestBody := map[string]any{
		"model":       a.Model,
		"temperature": 0.1,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": mustJSON(context)},
		},
	}

	data, err := json.Marshal(requestBody)
	if err != nil {
		return ConflictDecision{}, err
	}
	respBody, err := a.postChatCompletion("resolve_conflict", data)
	if err != nil {
		return ConflictDecision{}, err
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return ConflictDecision{}, err
	}
	if len(parsed.Choices) == 0 {
		return ConflictDecision{}, fmt.Errorf("llm response missing choices")
	}

	raw, err := messageContentText(parsed.Choices[0].Message.Content)
	if err != nil {
		return ConflictDecision{}, err
	}
	jsonPayload, err := extractJSONObject(raw)
	if err != nil {
		return ConflictDecision{}, err
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(jsonPayload), &payload); err != nil {
		return ConflictDecision{}, err
	}

	return normalizeConflictDecision(ConflictDecision{
		ResolvedContent: stringValue(payload["resolved_content"]),
		Summary:         stringValue(payload["summary"]),
		Confidence:      floatValue(payload["confidence"]),
		ShouldApply:     boolValue(payload["should_apply"]),
	}), nil
}

func (a *Agent) ResolveConflictBlock(context ConflictContext) (ConflictBlockDecision, error) {
	if a.APIKey == "" {
		return ConflictBlockDecision{}, fmt.Errorf("conflict auto-resolution requires model api key")
	}

	systemPrompt := `You are resolving a single git merge conflict block inside a larger file.
Return valid JSON with keys: resolved_block, summary, confidence, should_apply.
Use only the provided conflict block and short surrounding context.
The operator_goal tells you what outcome the user wants from this intervention. Keep that goal in mind while resolving this block.
resolved_block must contain only the replacement text for this conflict block and must not include conflict markers.
should_apply must be true only if you are confident the replacement is correct and complete for this block.`

	requestBody := map[string]any{
		"model":       a.Model,
		"temperature": 0.1,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": mustJSON(context)},
		},
	}

	data, err := json.Marshal(requestBody)
	if err != nil {
		return ConflictBlockDecision{}, err
	}
	respBody, err := a.postChatCompletion("resolve_conflict_block", data)
	if err != nil {
		return ConflictBlockDecision{}, err
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return ConflictBlockDecision{}, err
	}
	if len(parsed.Choices) == 0 {
		return ConflictBlockDecision{}, fmt.Errorf("llm response missing choices")
	}

	raw, err := messageContentText(parsed.Choices[0].Message.Content)
	if err != nil {
		return ConflictBlockDecision{}, err
	}
	jsonPayload, err := extractJSONObject(raw)
	if err != nil {
		return ConflictBlockDecision{}, err
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(jsonPayload), &payload); err != nil {
		return ConflictBlockDecision{}, err
	}

	return normalizeConflictBlockDecision(ConflictBlockDecision{
		ResolvedBlock: stringValue(payload["resolved_block"]),
		Summary:       stringValue(payload["summary"]),
		Confidence:    floatValue(payload["confidence"]),
		ShouldApply:   boolValue(payload["should_apply"]),
	}), nil
}

func (a *Agent) SummarizeConflicts(context ConflictSummaryContext) (ConflictSummary, error) {
	if a.APIKey == "" {
		return heuristicConflictSummary(context), nil
	}

	systemPrompt := `You are helping a developer review git merge conflicts on a pull request.
Return valid JSON with keys: summary, suggestions.
The operator_goal tells you what outcome the user wants from this intervention and should be reflected in your suggestions.
summary should explain the likely cause of the conflict.
suggestions should be a short list of concrete next steps for a human reviewer.`

	requestBody := map[string]any{
		"model":       a.Model,
		"temperature": 0.1,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": mustJSON(context)},
		},
	}

	data, err := json.Marshal(requestBody)
	if err != nil {
		return ConflictSummary{}, err
	}
	respBody, err := a.postChatCompletion("summarize_conflicts", data)
	if err != nil {
		return ConflictSummary{}, err
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return ConflictSummary{}, err
	}
	if len(parsed.Choices) == 0 {
		return ConflictSummary{}, fmt.Errorf("llm response missing choices")
	}

	raw, err := messageContentText(parsed.Choices[0].Message.Content)
	if err != nil {
		return ConflictSummary{}, err
	}
	jsonPayload, err := extractJSONObject(raw)
	if err != nil {
		return ConflictSummary{}, err
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(jsonPayload), &payload); err != nil {
		return ConflictSummary{}, err
	}

	return normalizeConflictSummary(ConflictSummary{
		Summary:     stringValue(payload["summary"]),
		Suggestions: stringSliceValue(payload["suggestions"]),
	}), nil
}

func heuristicReview(context Context) Result {
	topFiles := []string{}
	for i, file := range context.ChangedFiles {
		if i >= 3 {
			break
		}
		topFiles = append(topFiles, file.Filename)
	}

	risk := inferRisk(context.ChangedFiles)
	findings := []Finding{}

	if context.CI.State != "" && context.CI.State != "success" {
		findings = append(findings, Finding{
			Severity:   "medium",
			File:       "CI",
			Title:      "当前 CI 结果未通过",
			Detail:     fmt.Sprintf("检测到提交 %s 的状态为 %s。", shortSHA(context.HeadSHA), context.CI.State),
			Suggestion: "先处理失败或等待 CI 完成，再继续人工审核。",
		})
	}

	if !hasTestFiles(context.ChangedFiles) && len(context.ChangedFiles) > 0 {
		severity := "low"
		if risk != "low" {
			severity = "medium"
		}
		findings = append(findings, Finding{
			Severity:   severity,
			File:       context.ChangedFiles[0].Filename,
			Title:      "未发现明显测试变更",
			Detail:     "本次变更中没有观察到直接相关的测试文件修改。",
			Suggestion: "人工审核时重点确认是否需要补充单元测试或集成测试。",
		})
	}

	summaryTarget := "少量文件"
	if len(topFiles) > 0 {
		summaryTarget = strings.Join(topFiles, "、")
	}

	return normalizeResult(Result{
		Summary:       fmt.Sprintf("本 PR 主要涉及 %s 的修改，当前由本地启发式规则生成摘要，仍建议人工复核关键逻辑。", summaryTarget),
		OverallRisk:   risk,
		Confidence:    0.42,
		ConfidenceSet: true,
		Findings:      findings,
		Strengths: []string{
			fmt.Sprintf("本次共改动 %d 个文件", len(context.ChangedFiles)),
			fmt.Sprintf("CI 状态为 %s", fallback(context.CI.State, "unknown")),
		},
		TestSuggestions: []string{
			"为核心路径补充至少一个成功场景测试",
			"如果修改了错误处理逻辑，补充异常分支测试",
		},
		MergeReadiness: "needs_human_review",
	})
}

func heuristicIntervention(context InterventionContext) InterventionDecision {
	note := strings.ToLower(strings.TrimSpace(context.UserNote))

	decision := InterventionDecision{
		Action:                "comment_only",
		Summary:               "保留为人工介入处理。",
		Comment:               "已收到人工介入意见，当前保留给人工继续处理。",
		AuthoritativeApproval: false,
	}

	switch {
	case strings.Contains(note, "rerun"), strings.Contains(note, "re-run"), strings.Contains(note, "重跑"), strings.Contains(note, "重新审核"), strings.Contains(note, "再审一次"):
		decision.Action = "re_review"
		decision.Summary = "将重新执行自动审核。"
		decision.Comment = "已收到人工意见，准备重新执行自动审核。"
	case strings.Contains(note, "update branch"), strings.Contains(note, "sync branch"), strings.Contains(note, "rebase"), strings.Contains(note, "refresh branch"), strings.Contains(note, "更新分支"), strings.Contains(note, "同步分支"), strings.Contains(note, "解决冲突"):
		decision.Action = "update_branch"
		decision.Summary = "将尝试更新 PR 分支。"
		decision.Comment = "已收到人工意见，系统将尝试更新 PR 分支并刷新冲突状态。"
	case strings.Contains(note, "merge"), strings.Contains(note, "approve and merge"), strings.Contains(note, "accept directly"), strings.Contains(note, "force merge"), strings.Contains(note, "合并"), strings.Contains(note, "可以合并"), strings.Contains(note, "接受"), strings.Contains(note, "直接发版"):
		decision.Action = "merge"
		decision.Summary = "将尝试按人工意见直接合并。"
		decision.Comment = "已收到人工意见，系统将尝试直接合并该 PR。"
		decision.AuthoritativeApproval = true
	}

	return normalizeInterventionDecision(decision)
}

func heuristicConflictSummary(context ConflictSummaryContext) ConflictSummary {
	suggestions := []string{
		"先确认冲突文件中的业务预期，以 base 分支当前行为作为参照。",
		"逐个核对冲突块，优先保留最新的公共修复，再补回本 PR 的业务改动。",
		"解决后重新运行相关测试与 CI。",
	}
	return normalizeConflictSummary(ConflictSummary{
		Summary:     fmt.Sprintf("检测到 %d 个冲突文件，建议人工逐个确认合并结果。", len(context.Conflicts)),
		Suggestions: suggestions,
	})
}

func heuristicAggregateResults(context Context, results []Result) Result {
	if len(results) == 0 {
		return heuristicReview(context)
	}

	aggregated := Result{
		Summary:         fmt.Sprintf("本 PR 改动较大，已分 %d 批完成自动审核并汇总。", len(results)),
		OverallRisk:     "low",
		Confidence:      1,
		ConfidenceSet:   true,
		Findings:        []Finding{},
		Strengths:       []string{},
		TestSuggestions: []string{},
		MergeReadiness:  "ready_for_manual_approval",
		Provider:        results[0].Provider,
	}

	seenStrengths := map[string]struct{}{}
	seenTests := map[string]struct{}{}
	seenFindings := map[string]struct{}{}
	for _, item := range results {
		if riskRank(item.OverallRisk) > riskRank(aggregated.OverallRisk) {
			aggregated.OverallRisk = item.OverallRisk
		}
		if item.ConfidenceSet && item.Confidence < aggregated.Confidence {
			aggregated.Confidence = item.Confidence
		}
		if item.MergeReadiness == "needs_human_review" {
			aggregated.MergeReadiness = "needs_human_review"
		}
		for _, finding := range item.Findings {
			key := strings.TrimSpace(finding.File + "|" + finding.Title + "|" + finding.Detail)
			if key == "||" {
				continue
			}
			if _, exists := seenFindings[key]; exists {
				continue
			}
			seenFindings[key] = struct{}{}
			aggregated.Findings = append(aggregated.Findings, finding)
		}
		for _, strength := range item.Strengths {
			key := strings.TrimSpace(strength)
			if key == "" {
				continue
			}
			if _, exists := seenStrengths[key]; exists {
				continue
			}
			seenStrengths[key] = struct{}{}
			aggregated.Strengths = append(aggregated.Strengths, strength)
		}
		for _, suggestion := range item.TestSuggestions {
			key := strings.TrimSpace(suggestion)
			if key == "" {
				continue
			}
			if _, exists := seenTests[key]; exists {
				continue
			}
			seenTests[key] = struct{}{}
			aggregated.TestSuggestions = append(aggregated.TestSuggestions, suggestion)
		}
	}

	if aggregated.Confidence == 1 && len(results) > 0 && !results[0].ConfidenceSet {
		aggregated.ConfidenceSet = false
	}
	return normalizeResult(aggregated)
}

func chunkChangedFiles(files []ChangedFile, batchSize int) [][]ChangedFile {
	if batchSize <= 0 || len(files) == 0 {
		return [][]ChangedFile{files}
	}
	chunks := make([][]ChangedFile, 0, (len(files)+batchSize-1)/batchSize)
	for start := 0; start < len(files); start += batchSize {
		end := start + batchSize
		if end > len(files) {
			end = len(files)
		}
		chunk := make([]ChangedFile, end-start)
		copy(chunk, files[start:end])
		chunks = append(chunks, chunk)
	}
	return chunks
}

func riskRank(value string) int {
	switch value {
	case "high":
		return 3
	case "medium":
		return 2
	default:
		return 1
	}
}

func normalizeResult(result Result) Result {
	if result.Summary == "" {
		result.Summary = "未能从输出中提取稳定摘要，建议人工查看本次变更。"
	}

	switch result.OverallRisk {
	case "low", "medium", "high":
	default:
		result.OverallRisk = "medium"
	}

	if result.ConfidenceSet {
		if result.Confidence < 0 {
			result.Confidence = 0
		}
		if result.Confidence > 1 {
			result.Confidence = 1
		}
	}

	switch result.MergeReadiness {
	case "needs_human_review", "ready_for_manual_approval":
	default:
		result.MergeReadiness = "needs_human_review"
	}

	if result.Findings == nil {
		result.Findings = []Finding{}
	} else {
		filteredFindings := make([]Finding, 0, len(result.Findings))
		for _, finding := range result.Findings {
			file := strings.TrimSpace(finding.File)
			title := strings.TrimSpace(finding.Title)
			detail := strings.TrimSpace(finding.Detail)
			suggestion := strings.TrimSpace(finding.Suggestion)
			if file == "" && title == "" && detail == "" && suggestion == "" {
				continue
			}
			if title == "" && detail == "" {
				continue
			}
			filteredFindings = append(filteredFindings, finding)
		}
		result.Findings = filteredFindings
	}
	if result.Strengths == nil {
		result.Strengths = []string{}
	}
	if result.TestSuggestions == nil {
		result.TestSuggestions = []string{}
	}

	return result
}

func normalizeInterventionDecision(decision InterventionDecision) InterventionDecision {
	switch decision.Action {
	case "merge", "update_branch", "comment_only", "re_review":
	default:
		decision.Action = "comment_only"
	}

	if decision.Summary == "" {
		decision.Summary = "已记录人工介入意见。"
	}
	if decision.Comment == "" {
		decision.Comment = decision.Summary
	}
	if decision.Action != "merge" {
		decision.AuthoritativeApproval = false
	}

	return decision
}

func normalizeConflictDecision(decision ConflictDecision) ConflictDecision {
	if decision.Confidence < 0 {
		decision.Confidence = 0
	}
	if decision.Confidence > 1 {
		decision.Confidence = 1
	}
	if decision.Summary == "" {
		decision.Summary = "未提供冲突解决说明。"
	}
	if decision.ResolvedContent == "" {
		decision.ShouldApply = false
	}
	return decision
}

func normalizeConflictBlockDecision(decision ConflictBlockDecision) ConflictBlockDecision {
	if decision.Confidence < 0 {
		decision.Confidence = 0
	}
	if decision.Confidence > 1 {
		decision.Confidence = 1
	}
	decision.ResolvedBlock = strings.TrimSpace(decision.ResolvedBlock)
	decision.Summary = strings.TrimSpace(decision.Summary)
	return decision
}

func normalizeConflictSummary(summary ConflictSummary) ConflictSummary {
	if summary.Summary == "" {
		summary.Summary = "检测到冲突，建议人工复核。"
	}
	if summary.Suggestions == nil {
		summary.Suggestions = []string{}
	}
	return summary
}

func resultFromMap(payload map[string]any) Result {
	confidence, confidenceSet := optionalFloatValue(payload, "confidence")
	result := Result{
		Summary:         stringValue(payload["summary"]),
		OverallRisk:     stringValue(payload["overall_risk"]),
		Confidence:      confidence,
		ConfidenceSet:   confidenceSet,
		Strengths:       stringSliceValue(payload["strengths"]),
		TestSuggestions: stringSliceValue(payload["test_suggestions"]),
		MergeReadiness:  stringValue(payload["merge_readiness"]),
		Findings:        findingsValue(payload["findings"]),
	}
	return result
}

func findingsValue(value any) []Finding {
	items, ok := value.([]any)
	if !ok {
		return nil
	}

	findings := make([]Finding, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		findings = append(findings, Finding{
			Severity:   stringValue(entry["severity"]),
			File:       stringValue(entry["file"]),
			Title:      stringValue(entry["title"]),
			Detail:     stringValue(entry["detail"]),
			Suggestion: stringValue(entry["suggestion"]),
		})
	}
	return findings
}

func stringSliceValue(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}

	result := make([]string, 0, len(items))
	for _, item := range items {
		text := stringValue(item)
		if text != "" {
			result = append(result, text)
		}
	}
	return result
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case fmt.Stringer:
		return strings.TrimSpace(typed.String())
	case float64:
		return strings.TrimSpace(strconv.FormatFloat(typed, 'f', -1, 64))
	case int:
		return strconv.Itoa(typed)
	default:
		return ""
	}
}

func floatValue(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err == nil {
			return parsed
		}
	}
	return 0
}

func optionalFloatValue(payload map[string]any, key string) (float64, bool) {
	value, ok := payload[key]
	if !ok {
		return 0, false
	}
	return floatValue(value), true
}

func boolValue(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		normalized := strings.ToLower(strings.TrimSpace(typed))
		return normalized == "true" || normalized == "yes" || normalized == "1"
	case float64:
		return typed != 0
	case int:
		return typed != 0
	default:
		return false
	}
}

func inferRisk(files []ChangedFile) string {
	for _, file := range files {
		name := file.Filename
		if strings.HasPrefix(name, "infra/") || strings.HasPrefix(name, "migrations/") || strings.HasPrefix(name, "auth/") || strings.HasSuffix(name, ".sql") {
			return "high"
		}
		if len(file.Patch) > 2500 {
			return "medium"
		}
	}
	if len(files) > 10 {
		return "medium"
	}
	return "low"
}

func hasTestFiles(files []ChangedFile) bool {
	for _, file := range files {
		if strings.Contains(file.Filename, ".test.") || strings.Contains(file.Filename, ".spec.") {
			return true
		}
	}
	return false
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func fallback(value, defaultValue string) string {
	if value == "" {
		return defaultValue
	}
	return value
}

func mustJSON(value any) string {
	data, _ := json.MarshalIndent(value, "", "  ")
	return string(data)
}

func extractJSONObject(text string) (string, error) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return "", fmt.Errorf("model output did not contain json")
	}
	return text[start : end+1], nil
}

func messageContentText(content any) (string, error) {
	switch typed := content.(type) {
	case string:
		return typed, nil
	case []any:
		var parts []string
		for _, item := range typed {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if stringValue(entry["type"]) == "text" {
				text := stringValue(entry["text"])
				if text != "" {
					parts = append(parts, text)
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n"), nil
		}
	}
	return "", fmt.Errorf("llm response missing text content")
}

func reviewResultSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required": []string{
			"summary",
			"overall_risk",
			"confidence",
			"findings",
			"strengths",
			"test_suggestions",
			"merge_readiness",
		},
		"properties": map[string]any{
			"summary": map[string]any{
				"type": "string",
			},
			"overall_risk": map[string]any{
				"type": "string",
				"enum": []string{"low", "medium", "high"},
			},
			"confidence": map[string]any{
				"type":    "number",
				"minimum": 0,
				"maximum": 1,
			},
			"findings": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"severity", "file", "title", "detail", "suggestion"},
					"properties": map[string]any{
						"severity": map[string]any{"type": "string", "minLength": 1},
						"file":     map[string]any{"type": "string", "minLength": 1},
						"title":    map[string]any{"type": "string", "minLength": 1},
						"detail":   map[string]any{"type": "string", "minLength": 1},
						"suggestion": map[string]any{
							"type": "string",
						},
					},
				},
			},
			"strengths": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
			"test_suggestions": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
			"merge_readiness": map[string]any{
				"type": "string",
				"enum": []string{"needs_human_review", "ready_for_manual_approval"},
			},
		},
	}
}
