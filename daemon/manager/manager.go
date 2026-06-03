// SPDX-License-Identifier: Apache-2.0

package manager

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxRetries = 3

const (
	apiURL  = "https://api.openai.com/v1/responses"
	modelID = "gpt-5.4-mini"
)

// DefaultModelID exposes the agent-loop model so other packages reuse one
// launch default.
const DefaultModelID = modelID

const (
	// DefaultComputerMaxOutputTokens is the real manager-loop generation
	// budget. This includes hidden reasoning tokens and visible output.
	DefaultComputerMaxOutputTokens = 65536
	DefaultDeepMaxOutputTokens     = MaxComputerMaxOutputTokens
	DefaultTextMaxOutputTokens     = 4096
	DefaultReasoningEffort         = "medium"
	DefaultDeepReasoningEffort     = "high"
	MinComputerMaxOutputTokens     = 1024
	MaxComputerMaxOutputTokens     = 128000
)

// MetricsHook is set by main at boot so successful manager API calls update
// daemon metrics. OpenAI currently reports cached input reads, but not cache
// creation as a separate billed bucket.
var MetricsHook func(cacheRead, cacheCreation, inputFresh, output int)

// ClientOptions configures the manager runtime without changing the request
// shapes used by small helper calls such as guardrail classification.
type ClientOptions struct {
	Model               string
	ReasoningEffort     string
	MaxOutputTokens     int
	DeepReasoningEffort string
	DeepMaxOutputTokens int
}

// Client communicates with the OpenAI Responses API for the VibeCraft manager.
type Client struct {
	mu                         sync.RWMutex
	apiKey                     string
	model                      string
	reasoningEffort            string
	deepReasoningEffort        string
	computerMaxOutputTokens    int
	deepMaxOutputTokens        int
	textDefaultMaxOutputTokens int
	httpClient                 *http.Client
	baseURL                    string
}

type runtimeProfile struct {
	reasoningEffort string
	maxOutputTokens int
	deep            bool
}

// NewClient creates an OpenAI manager API client. The optional model override
// is for self-host/enterprise config; hosted launch uses gpt-5.4-mini.
func NewClient(apiKey string, modelOverride ...string) *Client {
	if len(modelOverride) > 0 && strings.TrimSpace(modelOverride[0]) != "" {
		return NewClientWithOptions(apiKey, ClientOptions{Model: modelOverride[0]})
	}
	return NewClientWithOptions(apiKey, ClientOptions{})
}

// NewClientWithOptions creates an OpenAI manager API client with explicit
// runtime policy for the main manager loop.
func NewClientWithOptions(apiKey string, opts ClientOptions) *Client {
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = modelID
	}
	reasoningEffort := strings.TrimSpace(opts.ReasoningEffort)
	if reasoningEffort == "" {
		reasoningEffort = DefaultReasoningEffort
	}
	deepReasoningEffort := strings.TrimSpace(opts.DeepReasoningEffort)
	if deepReasoningEffort == "" {
		deepReasoningEffort = DefaultDeepReasoningEffort
	}
	computerMaxOutputTokens := opts.MaxOutputTokens
	if computerMaxOutputTokens <= 0 {
		computerMaxOutputTokens = DefaultComputerMaxOutputTokens
	}
	deepMaxOutputTokens := opts.DeepMaxOutputTokens
	if deepMaxOutputTokens <= 0 {
		deepMaxOutputTokens = DefaultDeepMaxOutputTokens
	}
	textDefaultMaxOutputTokens := DefaultTextMaxOutputTokens

	transport := &http.Transport{
		ForceAttemptHTTP2: false,
		TLSNextProto:      make(map[string]func(string, *tls.Conn) http.RoundTripper),
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          10,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}

	return &Client{
		apiKey:                     strings.TrimSpace(apiKey),
		model:                      model,
		reasoningEffort:            reasoningEffort,
		deepReasoningEffort:        deepReasoningEffort,
		computerMaxOutputTokens:    computerMaxOutputTokens,
		deepMaxOutputTokens:        deepMaxOutputTokens,
		textDefaultMaxOutputTokens: textDefaultMaxOutputTokens,
		baseURL:                    apiURL,
		httpClient: &http.Client{
			Timeout:   180 * time.Second,
			Transport: transport,
		},
	}
}

// SetAPIKey updates the upstream manager key in memory after an operator
// completes local Connected/BYOM setup. The key is still only used as an
// Authorization header on manager requests.
func (c *Client) SetAPIKey(apiKey string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.apiKey = strings.TrimSpace(apiKey)
}

// Model returns the configured model for the main manager loop.
func (c *Client) Model() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.model
}

// Message represents a conversation message for the manager API.
type Message struct {
	Role        string       `json:"role"`
	Content     interface{}  `json:"content"`
	ToolResults []ToolResult `json:"-"`
	RawContent  interface{}  `json:"-"`
}

// ContentBlock is a single content block in a message content array.
type ContentBlock struct {
	Type      string       `json:"type"`
	Text      string       `json:"text,omitempty"`
	ID        string       `json:"id,omitempty"`
	Name      string       `json:"name,omitempty"`
	Input     interface{}  `json:"input,omitempty"`
	ToolUseID string       `json:"tool_use_id,omitempty"`
	Content   interface{}  `json:"content,omitempty"`
	IsError   bool         `json:"is_error,omitempty"`
	Source    *ImageSource `json:"source,omitempty"`
}

// ImageSource describes a base64 image for tool results.
type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// ToolCall represents a function_call item from OpenAI's response.
type ToolCall struct {
	ID    string            `json:"id"`
	Name  string            `json:"name"`
	Input map[string]string `json:"input"`
}

// InputString returns a readable representation of the tool-call input.
func (tc ToolCall) InputString() string {
	if cmd, ok := tc.Input["command"]; ok {
		return cmd
	}
	if action, ok := tc.Input["action"]; ok {
		parts := []string{action}
		if text, ok := tc.Input["text"]; ok {
			parts = append(parts, text)
		}
		return fmt.Sprintf("%s", parts)
	}
	data, _ := json.Marshal(tc.Input)
	return string(data)
}

// InputCoords extracts x,y coordinates from the input.
func (tc ToolCall) InputCoords() (int, int) {
	if coordStr, ok := tc.Input["coordinate"]; ok {
		var coords []int
		_ = json.Unmarshal([]byte(coordStr), &coords)
		if len(coords) == 2 {
			return coords[0], coords[1]
		}
	}
	x, _ := strconv.Atoi(tc.Input["x"])
	y, _ := strconv.Atoi(tc.Input["y"])
	return x, y
}

// ToolResult is the result of a tool execution sent back to the manager.
type ToolResult struct {
	ToolUseID     string `json:"tool_use_id"`
	Content       string `json:"content"`
	IsError       bool   `json:"is_error,omitempty"`
	IsBase64Image bool   `json:"-"`
}

// Response is a parsed API response.
type Response struct {
	TextContent string
	ToolCalls   []ToolCall
	Blocks      []Block
	StopReason  string
	RawContent  interface{}
	Usage       Usage
}

// Block is a single output item from the manager in natural order.
type Block struct {
	Type     string
	Text     string
	ToolCall *ToolCall
}

// Usage tracks token usage from the API.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
}

// TotalInputTokens returns the raw input-token count that counts against
// context. OpenAI reports cached input inside InputTokens; do not add cached
// buckets again or compaction will fire early.
func (u Usage) TotalInputTokens() int {
	return u.InputTokens
}

type apiRequest struct {
	Model           string        `json:"model"`
	Instructions    string        `json:"instructions,omitempty"`
	Input           []interface{} `json:"input"`
	Tools           []apiTool     `json:"tools,omitempty"`
	Include         []string      `json:"include,omitempty"`
	Reasoning       *apiReasoning `json:"reasoning,omitempty"`
	MaxOutputTokens int           `json:"max_output_tokens,omitempty"`
	Store           bool          `json:"store"`
}

type apiReasoning struct {
	Effort string `json:"effort,omitempty"`
}

type apiTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type apiResponse struct {
	ID     string          `json:"id"`
	Status string          `json:"status"`
	Output []apiOutputItem `json:"output"`
	Usage  apiUsage        `json:"usage"`
	Error  *apiError       `json:"error,omitempty"`
}

type apiOutputItem struct {
	Type             string           `json:"type"`
	ID               string           `json:"id,omitempty"`
	CallID           string           `json:"call_id,omitempty"`
	Status           string           `json:"status,omitempty"`
	Role             string           `json:"role,omitempty"`
	Name             string           `json:"name,omitempty"`
	Arguments        string           `json:"arguments,omitempty"`
	Content          []apiContentPart `json:"content,omitempty"`
	Summary          []apiContentPart `json:"summary,omitempty"`
	EncryptedContent string           `json:"encrypted_content,omitempty"`
}

type apiContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type apiUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

type apiError struct {
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// GetUsageStats returns cumulative usage statistics.
func (c *Client) GetUsageStats() map[string]interface{} {
	return map[string]interface{}{
		"model": c.model,
		"note":  "usage tracking available per-response via Response.Usage",
	}
}

// SetBaseURLForTests overrides the OpenAI API endpoint for tests.
func SetBaseURLForTests(c *Client, url string) {
	if c == nil {
		return
	}
	c.baseURL = url
}

// SendText sends a plain text request with no tools.
func (c *Client) SendText(ctx context.Context, model, systemPrompt, userPrompt string, maxTokensOverride int) (*Response, error) {
	if strings.TrimSpace(model) == "" {
		model = c.model
	}
	if maxTokensOverride <= 0 {
		maxTokensOverride = c.textDefaultMaxOutputTokens
	}
	reqBody := apiRequest{
		Model:           model,
		Instructions:    systemPrompt,
		Input:           []interface{}{inputMessage("user", []interface{}{inputText(userPrompt)})},
		MaxOutputTokens: maxTokensOverride,
		Store:           false,
	}
	return c.send(ctx, reqBody)
}

// SendComputerUse sends a manager loop request with the local computer,
// shell, editor, and credential tools exposed as OpenAI function tools.
func (c *Client) SendComputerUse(ctx context.Context, systemPrompt string, messages []Message) (*Response, error) {
	input, err := buildInputItems(messages)
	if err != nil {
		return nil, err
	}
	if len(input) == 0 {
		return nil, fmt.Errorf("no user-role messages to send")
	}
	profile := c.runtimeProfileForMessages(messages)

	reqBody := apiRequest{
		Model:           c.model,
		Instructions:    systemPrompt,
		Input:           input,
		Tools:           managerTools(),
		Include:         reasoningInclude(profile.reasoningEffort),
		Reasoning:       reasoningConfig(profile.reasoningEffort),
		MaxOutputTokens: profile.maxOutputTokens,
		Store:           false,
	}

	body, _ := json.Marshal(reqBody)
	log.Printf("manager: sending request (items=%d, body=%d bytes, model=%s, reasoning=%s, max_output_tokens=%d, deep_profile=%t)", len(input), len(body), c.model, profile.reasoningEffort, profile.maxOutputTokens, profile.deep)

	return c.send(ctx, reqBody)
}

// SendNoTools sends the same message shape as SendComputerUse but exposes no
// desktop or shell tools. It is used for direct user-attachment analysis where
// the correct answer is in the uploaded image, not on the live desktop.
func (c *Client) SendNoTools(ctx context.Context, systemPrompt string, messages []Message) (*Response, error) {
	input, err := buildInputItems(messages)
	if err != nil {
		return nil, err
	}
	if len(input) == 0 {
		return nil, fmt.Errorf("no user-role messages to send")
	}
	profile := c.runtimeProfileForMessages(messages)

	reqBody := apiRequest{
		Model:           c.model,
		Instructions:    systemPrompt,
		Input:           input,
		Reasoning:       reasoningConfig(profile.reasoningEffort),
		MaxOutputTokens: profile.maxOutputTokens,
		Store:           false,
	}

	body, _ := json.Marshal(reqBody)
	log.Printf("manager: sending no-tools request (items=%d, body=%d bytes, model=%s, reasoning=%s, max_output_tokens=%d, deep_profile=%t)", len(input), len(body), c.model, profile.reasoningEffort, profile.maxOutputTokens, profile.deep)

	return c.send(ctx, reqBody)
}

func (c *Client) runtimeProfileForMessages(messages []Message) runtimeProfile {
	profile := runtimeProfile{
		reasoningEffort: c.reasoningEffort,
		maxOutputTokens: c.computerMaxOutputTokens,
	}
	if shouldUseDeepRuntime(messages) {
		profile.reasoningEffort = c.deepReasoningEffort
		profile.maxOutputTokens = c.deepMaxOutputTokens
		profile.deep = true
	}
	return profile
}

func reasoningConfig(effort string) *apiReasoning {
	if strings.TrimSpace(effort) == "" {
		return nil
	}
	return &apiReasoning{Effort: effort}
}

func reasoningInclude(effort string) []string {
	if effort == "" || effort == "none" {
		return nil
	}
	return []string{"reasoning.encrypted_content"}
}

func shouldUseDeepRuntime(messages []Message) bool {
	text := strings.ToLower(strings.Join(recentUserTexts(messages), "\n"))
	if text == "" {
		return false
	}
	for _, trigger := range deepRuntimeTriggers {
		if strings.Contains(text, trigger) {
			return true
		}
	}
	return false
}

var deepRuntimeTriggers = []string{
	"think hard",
	"think deeply",
	"reason deeply",
	"deep planning",
	"deep plan",
	"deep research",
	"comprehensive plan",
	"architecture",
	"architectural",
	"system design",
	"design the system",
	"persistent system",
	"build a system",
	"multi-worker",
	"multi worker",
	"parallel workers",
	"fan out",
	"fan-out",
	"worker synthesis",
	"postmortem",
	"post-mortem",
	"root cause",
	"production incident",
	"security review",
	"migration plan",
	"release plan",
	"rollout plan",
	"end-to-end",
	"e2e",
	"test in prod",
	"production verification",
	"deploy and test",
	"deploy it and test",
	"best possible",
	"full implementation",
	"whole job",
}

func recentUserTexts(messages []Message) []string {
	const maxTexts = 4
	var texts []string
	for i := len(messages) - 1; i >= 0 && len(texts) < maxTexts; i-- {
		msg := messages[i]
		if msg.Role != "" && msg.Role != "user" {
			continue
		}
		if len(msg.ToolResults) > 0 || msg.RawContent != nil {
			continue
		}
		parts := contentTextParts(msg.Content)
		for j := len(parts) - 1; j >= 0 && len(texts) < maxTexts; j-- {
			if text := strings.TrimSpace(parts[j]); text != "" {
				texts = append(texts, text)
			}
		}
	}
	return texts
}

func contentTextParts(content interface{}) []string {
	switch v := content.(type) {
	case nil:
		return nil
	case string:
		return []string{v}
	case []ContentBlock:
		out := make([]string, 0, len(v))
		for _, block := range v {
			if block.Type == "text" || block.Type == "input_text" {
				out = append(out, block.Text)
			}
		}
		return out
	case []map[string]interface{}:
		return mapTextParts(v)
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				out = append(out, mapTextParts([]map[string]interface{}{m})...)
			}
		}
		return out
	default:
		return nil
	}
}

func mapTextParts(blocks []map[string]interface{}) []string {
	out := make([]string, 0, len(blocks))
	for _, block := range blocks {
		t, _ := block["type"].(string)
		if t != "text" && t != "input_text" {
			continue
		}
		if text, _ := block["text"].(string); text != "" {
			out = append(out, text)
		}
	}
	return out
}

func (c *Client) send(ctx context.Context, reqBody apiRequest) (*Response, error) {
	c.mu.RLock()
	apiKey := strings.TrimSpace(c.apiKey)
	c.mu.RUnlock()
	if apiKey == "" {
		return nil, fmt.Errorf("OpenAI manager key is not configured")
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	var respBody []byte
	for attempt := 0; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("creating request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)

		reqStart := time.Now()
		resp, err := c.httpClient.Do(req)
		doElapsed := time.Since(reqStart)
		if err != nil {
			log.Printf("manager: Do() error after %s: %v", doElapsed, err)
			if attempt < maxRetries {
				if waitErr := waitBeforeRetry(ctx, attempt, ""); waitErr != nil {
					return nil, waitErr
				}
				continue
			}
			return nil, fmt.Errorf("API request failed: %w", err)
		}

		respBody, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading response: %w", err)
		}

		if resp.StatusCode == http.StatusOK {
			break
		}
		if (resp.StatusCode == 429 || resp.StatusCode >= 500) && attempt < maxRetries {
			log.Printf("manager: HTTP %d (attempt %d/%d), retrying", resp.StatusCode, attempt+1, maxRetries+1)
			if waitErr := waitBeforeRetry(ctx, attempt, resp.Header.Get("Retry-After")); waitErr != nil {
				return nil, waitErr
			}
			continue
		}

		var apiErr struct {
			Error apiError `json:"error"`
		}
		_ = json.Unmarshal(respBody, &apiErr)
		msg := strings.TrimSpace(apiErr.Error.Message)
		if msg == "" {
			msg = "request rejected by upstream API"
		}
		return nil, fmt.Errorf("API error (%d): %s - %s", resp.StatusCode, apiErr.Error.Type, msg)
	}

	var apiResp apiResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}
	if apiResp.Error != nil {
		return nil, fmt.Errorf("API error: %s - %s", apiResp.Error.Type, apiResp.Error.Message)
	}
	var rawResp struct {
		Output []json.RawMessage `json:"output"`
	}
	_ = json.Unmarshal(respBody, &rawResp)
	return parseResponseWithRaw(apiResp, rawResp.Output), nil
}

func waitBeforeRetry(ctx context.Context, attempt int, retryAfter string) error {
	backoff := time.Duration(math.Pow(2, float64(attempt))) * time.Second
	if retryAfter != "" {
		if secs, parseErr := strconv.Atoi(retryAfter); parseErr == nil {
			backoff = time.Duration(secs) * time.Second
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(backoff):
		return nil
	}
}

func parseResponse(apiResp apiResponse) *Response {
	return parseResponseWithRaw(apiResp, nil)
}

func parseResponseWithRaw(apiResp apiResponse, rawOutput []json.RawMessage) *Response {
	u := Usage{
		InputTokens:          apiResp.Usage.InputTokens,
		OutputTokens:         apiResp.Usage.OutputTokens,
		CacheReadInputTokens: apiResp.Usage.InputTokensDetails.CachedTokens,
	}
	result := &Response{
		StopReason: "end_turn",
		Usage:      u,
		RawContent: rawOutputContent(apiResp.Output, rawOutput),
	}
	if MetricsHook != nil {
		MetricsHook(u.CacheReadInputTokens, u.CacheCreationInputTokens, u.InputTokens, u.OutputTokens)
	}

	for _, item := range apiResp.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" || part.Type == "text" {
					result.TextContent = appendOutputText(result.TextContent, part.Text)
					result.Blocks = append(result.Blocks, Block{Type: "text", Text: part.Text})
				}
			}
		case "function_call":
			input := map[string]string{}
			if strings.TrimSpace(item.Arguments) != "" {
				input = parseToolArguments(item.Arguments)
			}
			id := item.CallID
			if id == "" {
				id = item.ID
			}
			tc := ToolCall{ID: id, Name: item.Name, Input: input}
			result.ToolCalls = append(result.ToolCalls, tc)
			tcCopy := tc
			result.Blocks = append(result.Blocks, Block{Type: "tool_use", ToolCall: &tcCopy})
		}
	}
	if len(result.ToolCalls) > 0 {
		result.StopReason = "tool_use"
	}
	log.Printf("manager: usage input=%d output=%d cached_input=%d total_input=%d", u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.TotalInputTokens())
	return result
}

func rawOutputContent(output []apiOutputItem, rawOutput []json.RawMessage) []interface{} {
	if len(rawOutput) > 0 {
		items := make([]interface{}, 0, len(rawOutput))
		for _, raw := range rawOutput {
			var item interface{}
			if err := json.Unmarshal(raw, &item); err != nil {
				continue
			}
			items = append(items, item)
		}
		normalizeReasoningInputItems(items)
		return items
	}
	items, err := rawOutputItems(output)
	if err != nil {
		return nil
	}
	return items
}

func appendOutputText(current, next string) string {
	if current == "" || next == "" {
		return current + next
	}
	if strings.HasSuffix(current, "\n") || strings.HasPrefix(next, "\n") {
		return current + next
	}
	return current + "\n" + next
}

func parseToolArguments(raw string) map[string]string {
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return map[string]string{"_raw": raw}
	}
	out := make(map[string]string, len(parsed))
	for k, v := range parsed {
		switch val := v.(type) {
		case string:
			out[k] = val
		default:
			data, _ := json.Marshal(val)
			out[k] = string(data)
		}
	}
	return out
}

func buildInputItems(messages []Message) ([]interface{}, error) {
	var input []interface{}
	for _, msg := range messages {
		if len(msg.ToolResults) > 0 {
			for _, tr := range msg.ToolResults {
				if tr.IsBase64Image {
					input = append(input, map[string]interface{}{
						"type":    "function_call_output",
						"call_id": tr.ToolUseID,
						"output":  toolOutputString("Screenshot captured. The image is attached as the next user input.", tr.IsError),
					})
					input = append(input, inputMessage("user", []interface{}{
						inputText("Screenshot result for the previous computer action."),
						inputImage("image/png", tr.Content),
					}))
					continue
				}
				input = append(input, map[string]interface{}{
					"type":    "function_call_output",
					"call_id": tr.ToolUseID,
					"output":  toolOutputString(tr.Content, tr.IsError),
				})
			}
			continue
		}
		if msg.RawContent != nil {
			rawItems, err := rawOutputItems(msg.RawContent)
			if err != nil {
				return nil, err
			}
			input = append(input, rawItems...)
			continue
		}
		content, err := contentParts(msg.Content)
		if err != nil {
			return nil, err
		}
		role := msg.Role
		if role == "" {
			role = "user"
		}
		input = append(input, inputMessage(role, content))
	}

	for len(input) > 0 {
		last, ok := input[len(input)-1].(map[string]interface{})
		if !ok {
			break
		}
		role, _ := last["role"].(string)
		if role != "assistant" {
			break
		}
		log.Printf("manager: WARNING dropping trailing assistant message (prefill guard)")
		input = input[:len(input)-1]
	}
	return input, nil
}

func rawOutputItems(raw interface{}) ([]interface{}, error) {
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshaling raw manager content: %w", err)
	}
	var items []interface{}
	if err := json.Unmarshal(b, &items); err != nil {
		return nil, fmt.Errorf("parsing raw manager content: %w", err)
	}
	normalizeReasoningInputItems(items)
	return items, nil
}

func normalizeReasoningInputItems(items []interface{}) {
	for _, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok || m["type"] != "reasoning" {
			continue
		}
		if summary, ok := m["summary"]; !ok || summary == nil {
			m["summary"] = []interface{}{}
		}
	}
}

func contentParts(content interface{}) ([]interface{}, error) {
	switch v := content.(type) {
	case nil:
		return []interface{}{inputText("")}, nil
	case string:
		return []interface{}{inputText(v)}, nil
	case []ContentBlock:
		parts := make([]interface{}, 0, len(v))
		for _, block := range v {
			converted := contentBlockPart(block.Type, block.Text, block.Source)
			if converted != nil {
				parts = append(parts, converted)
			}
		}
		if len(parts) == 0 {
			parts = append(parts, inputText(""))
		}
		return parts, nil
	case []map[string]interface{}:
		return mapContentParts(v), nil
	case []interface{}:
		maps := make([]map[string]interface{}, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				maps = append(maps, m)
			}
		}
		return mapContentParts(maps), nil
	default:
		data, _ := json.Marshal(v)
		return []interface{}{inputText(string(data))}, nil
	}
}

func mapContentParts(blocks []map[string]interface{}) []interface{} {
	parts := make([]interface{}, 0, len(blocks))
	for _, block := range blocks {
		t, _ := block["type"].(string)
		switch t {
		case "text", "input_text":
			text, _ := block["text"].(string)
			parts = append(parts, inputText(text))
		case "image", "input_image":
			source := sourceFromMap(block["source"])
			if source != nil {
				parts = append(parts, inputImage(source.MediaType, source.Data))
			}
		}
	}
	if len(parts) == 0 {
		parts = append(parts, inputText(""))
	}
	return parts
}

func contentBlockPart(t, text string, source *ImageSource) interface{} {
	switch t {
	case "text", "input_text":
		return inputText(text)
	case "image", "input_image":
		if source == nil {
			return nil
		}
		return inputImage(source.MediaType, source.Data)
	default:
		return nil
	}
}

func sourceFromMap(v interface{}) *ImageSource {
	var media string
	var data string
	switch m := v.(type) {
	case map[string]interface{}:
		media, _ = m["media_type"].(string)
		data, _ = m["data"].(string)
	case map[string]string:
		media = m["media_type"]
		data = m["data"]
	case ImageSource:
		media = m.MediaType
		data = m.Data
	case *ImageSource:
		if m == nil {
			return nil
		}
		media = m.MediaType
		data = m.Data
	default:
		return nil
	}
	if media == "" {
		media = "image/png"
	}
	if data == "" {
		return nil
	}
	return &ImageSource{Type: "base64", MediaType: media, Data: data}
}

func inputMessage(role string, content []interface{}) map[string]interface{} {
	if role == "assistant" {
		return map[string]interface{}{
			"type":    "message",
			"role":    role,
			"content": outputContent(content),
		}
	}
	return map[string]interface{}{
		"type":    "message",
		"role":    role,
		"content": content,
	}
}

func outputContent(content []interface{}) []interface{} {
	out := make([]interface{}, 0, len(content))
	for _, part := range content {
		m, ok := part.(map[string]interface{})
		if !ok {
			continue
		}
		if m["type"] == "input_text" {
			out = append(out, map[string]interface{}{"type": "output_text", "text": m["text"]})
		}
	}
	return out
}

func inputText(text string) map[string]interface{} {
	return map[string]interface{}{"type": "input_text", "text": text}
}

func inputImage(mediaType, data string) map[string]interface{} {
	if mediaType == "" {
		mediaType = "image/png"
	}
	return map[string]interface{}{
		"type":      "input_image",
		"image_url": "data:" + mediaType + ";base64," + data,
	}
}

func toolOutputString(content string, isError bool) string {
	if isError {
		return "ERROR: " + content
	}
	return content
}

func managerTools() []apiTool {
	loose := false
	return []apiTool{
		functionTool("str_replace_based_edit_tool", "Structured file editor for local customer-machine files. Prefer this over shell text surgery for non-trivial file edits.", textEditorSchema, &loose),
		functionTool("bash", "Run a shell command on the customer's VibeCraft computer when the task needs local files, processes, installs, or verification.", bashSchema, &loose),
		functionTool("computer", "Operate the real customer desktop with mouse, keyboard, scrolling, and screenshots. Use this for the live desktop, not for user-attached images; attached images are already provided inline in the conversation input.", computerSchema, &loose),
		functionTool("request_credentials", "You are the operator of this computer. Request secrets through the secure customer credential card. This is the default path, not a fallback, when a login, token, API key, or two-factor code is needed. DO NOT walk the customer through entering the value themselves. DO NOT offer to 'help' enter it. Anticipate the full set in one call, for example email and password together for a login. You receive vault variable names, never secret values.", requestCredentialsSchema, &loose),
	}
}

func functionTool(name, description string, parameters json.RawMessage, strict *bool) apiTool {
	return apiTool{
		Type:        "function",
		Name:        name,
		Description: description,
		Parameters:  parameters,
		Strict:      strict,
	}
}

var bashSchema = json.RawMessage(`{
  "type": "object",
  "required": ["command"],
  "properties": {
    "command": {
      "type": "string",
      "description": "The bash command to run on the customer's machine."
    }
  },
  "additionalProperties": false
}`)

var computerSchema = json.RawMessage(`{
  "type": "object",
  "required": ["action"],
  "properties": {
    "action": {
      "type": "string",
      "enum": ["screenshot", "mouse_move", "left_click", "right_click", "double_click", "middle_click", "left_click_drag", "type", "key", "scroll", "cursor_position"]
    },
    "coordinate": {
      "type": "array",
      "items": { "type": "integer" },
      "minItems": 2,
      "maxItems": 2
    },
    "x": { "type": "integer" },
    "y": { "type": "integer" },
    "text": { "type": "string" },
    "direction": {
      "type": "string",
      "enum": ["up", "down", "left", "right"]
    }
  },
  "additionalProperties": false
}`)

var textEditorSchema = json.RawMessage(`{
  "type": "object",
  "required": ["command", "path"],
  "properties": {
    "command": {
      "type": "string",
      "enum": ["view", "create", "str_replace", "insert"]
    },
    "path": { "type": "string" },
    "file_text": { "type": "string" },
    "old_str": { "type": "string" },
    "new_str": { "type": "string" },
    "insert_line": { "type": "integer" }
  },
  "additionalProperties": false
}`)

var requestCredentialsSchema = json.RawMessage(`{
  "type": "object",
  "required": ["title", "fields"],
  "properties": {
    "title": {
      "type": "string",
      "description": "Short plain-English title for the card, e.g. 'HubSpot login' or 'OpenAI API key'."
    },
    "reason": {
      "type": "string",
      "description": "One sentence explaining why these credentials are needed right now. Shown below the title."
    },
    "fields": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "required": ["name", "label"],
        "properties": {
          "name": {
            "type": "string",
            "pattern": "^[A-Z][A-Z0-9_]*$"
          },
          "label": { "type": "string" },
          "type": {
            "type": "string",
            "enum": ["text", "password", "url", "token"]
          },
          "hint": { "type": "string" }
        },
        "additionalProperties": false
      }
    }
  },
  "additionalProperties": false
}`)
