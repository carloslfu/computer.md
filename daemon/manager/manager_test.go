// SPDX-License-Identifier: Apache-2.0

package manager

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseResponse_TextToolCallAndUsage(t *testing.T) {
	apiResp := apiResponse{
		Output: []apiOutputItem{
			{
				Type: "message",
				Content: []apiContentPart{{
					Type: "output_text",
					Text: "done",
				}},
			},
			{
				Type:      "function_call",
				CallID:    "call_123",
				Name:      "bash",
				Arguments: `{"command":"echo hi","retries":2}`,
			},
		},
	}
	apiResp.Usage.InputTokens = 100
	apiResp.Usage.OutputTokens = 25
	apiResp.Usage.InputTokensDetails.CachedTokens = 40

	resp := parseResponse(apiResp)
	if resp.TextContent != "done" {
		t.Fatalf("TextContent = %q, want done", resp.TextContent)
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("StopReason = %q, want tool_use", resp.StopReason)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_123" || tc.Name != "bash" || tc.Input["command"] != "echo hi" || tc.Input["retries"] != "2" {
		t.Fatalf("unexpected tool call: %+v", tc)
	}
	if resp.Usage.InputTokens != 100 || resp.Usage.CacheReadInputTokens != 40 || resp.Usage.OutputTokens != 25 {
		t.Fatalf("unexpected usage: %+v", resp.Usage)
	}
}

func TestParseResponse_PreservesReasoningItemsForStatelessToolLoop(t *testing.T) {
	apiResp := apiResponse{
		Output: []apiOutputItem{
			{
				Type:             "reasoning",
				ID:               "rs_123",
				Status:           "completed",
				Summary:          []apiContentPart{{Type: "summary_text", Text: "Need to inspect the file first."}},
				EncryptedContent: "encrypted-reasoning",
			},
			{
				Type:      "function_call",
				CallID:    "call_123",
				Name:      "bash",
				Arguments: `{"command":"pwd"}`,
			},
		},
	}

	resp := parseResponse(apiResp)
	rawItems, err := rawOutputItems(resp.RawContent)
	if err != nil {
		t.Fatal(err)
	}
	rawJSON, _ := json.Marshal(rawItems)
	if !strings.Contains(string(rawJSON), `"type":"reasoning"`) ||
		!strings.Contains(string(rawJSON), `"encrypted_content":"encrypted-reasoning"`) {
		t.Fatalf("RawContent did not preserve reasoning item: %s", rawJSON)
	}
}

func TestBuildInputItems_PreservesEmptyReasoningSummary(t *testing.T) {
	apiResp := apiResponse{
		Output: []apiOutputItem{
			{
				Type:             "reasoning",
				ID:               "rs_123",
				Status:           "completed",
				Summary:          []apiContentPart{},
				EncryptedContent: "encrypted-reasoning",
			},
			{
				Type:      "function_call",
				ID:        "fc_123",
				CallID:    "call_123",
				Name:      "bash",
				Arguments: `{"command":"pwd"}`,
			},
		},
	}
	resp := parseResponse(apiResp)

	items, err := buildInputItems([]Message{
		{Role: "user", Content: "where am I?"},
		{Role: "assistant", RawContent: resp.RawContent},
		{Role: "user", ToolResults: []ToolResult{{
			ToolUseID: "call_123",
			Content:   "/home/vibecraft",
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(items) != 4 {
		t.Fatalf("items len = %d, want 4: %#v", len(items), items)
	}
	reasoning, ok := items[1].(map[string]interface{})
	if !ok || reasoning["type"] != "reasoning" {
		t.Fatalf("second item should be replayed reasoning, got %#v", items[1])
	}
	summary, ok := reasoning["summary"]
	if !ok {
		t.Fatalf("replayed reasoning item omitted required empty summary: %#v", reasoning)
	}
	if parts, ok := summary.([]interface{}); !ok || len(parts) != 0 {
		t.Fatalf("summary = %#v, want empty array", summary)
	}
}

func TestParseResponseWithRaw_PreservesUnknownOutputFields(t *testing.T) {
	apiResp := apiResponse{
		Output: []apiOutputItem{
			{
				Type:             "reasoning",
				ID:               "rs_123",
				Status:           "completed",
				EncryptedContent: "encrypted-reasoning",
			},
			{
				Type:      "function_call",
				ID:        "fc_123",
				CallID:    "call_123",
				Name:      "bash",
				Arguments: `{"command":"pwd"}`,
			},
		},
	}
	rawOutput := []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"rs_123","status":"completed","encrypted_content":"encrypted-reasoning","phase":"analysis"}`),
		json.RawMessage(`{"type":"function_call","id":"fc_123","call_id":"call_123","name":"bash","arguments":"{\"command\":\"pwd\"}","status":"completed","phase":"tool_call"}`),
	}

	resp := parseResponseWithRaw(apiResp, rawOutput)
	rawItems, err := rawOutputItems(resp.RawContent)
	if err != nil {
		t.Fatal(err)
	}
	reasoning, ok := rawItems[0].(map[string]interface{})
	if !ok {
		t.Fatalf("reasoning raw item = %#v", rawItems[0])
	}
	if reasoning["phase"] != "analysis" {
		t.Fatalf("reasoning phase was not preserved: %#v", reasoning)
	}
	if _, ok := reasoning["summary"]; !ok {
		t.Fatalf("reasoning summary was not normalized: %#v", reasoning)
	}
	call, ok := rawItems[1].(map[string]interface{})
	if !ok {
		t.Fatalf("call raw item = %#v", rawItems[1])
	}
	if call["phase"] != "tool_call" {
		t.Fatalf("function call phase was not preserved: %#v", call)
	}
}

func TestParseResponse_PreservesTextPartBoundaries(t *testing.T) {
	apiResp := apiResponse{
		Output: []apiOutputItem{{
			Type: "message",
			Content: []apiContentPart{
				{Type: "output_text", Text: "Reading the visible rates from the pricing page."},
				{Type: "output_text", Text: "- Source: https://openai.com/api/pricing/\n- API usage is billed per token.\n- Example rate: input, cached input, and output."},
			},
		}},
	}

	resp := parseResponse(apiResp)
	want := "Reading the visible rates from the pricing page.\n- Source: https://openai.com/api/pricing/\n- API usage is billed per token.\n- Example rate: input, cached input, and output."
	if resp.TextContent != want {
		t.Fatalf("TextContent = %q, want %q", resp.TextContent, want)
	}
}

func TestBuildInputItems_ScreenshotToolResult(t *testing.T) {
	items, err := buildInputItems([]Message{{
		ToolResults: []ToolResult{{
			ToolUseID:     "call_img",
			Content:       "BASE64PNG",
			IsBase64Image: true,
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items len = %d, want 2", len(items))
	}
	out, ok := items[0].(map[string]interface{})
	if !ok || out["type"] != "function_call_output" || out["call_id"] != "call_img" {
		t.Fatalf("first item should be function_call_output, got %#v", items[0])
	}
	msg, ok := items[1].(map[string]interface{})
	if !ok || msg["type"] != "message" || msg["role"] != "user" {
		t.Fatalf("second item should be user message, got %#v", items[1])
	}
	content, ok := msg["content"].([]interface{})
	if !ok || len(content) != 2 {
		t.Fatalf("second message content = %#v", msg["content"])
	}
	image, ok := content[1].(map[string]interface{})
	if !ok || image["type"] != "input_image" {
		t.Fatalf("second content item should be input_image, got %#v", content[1])
	}
	if !strings.Contains(image["image_url"].(string), "BASE64PNG") {
		t.Fatalf("image_url did not contain screenshot data: %#v", image)
	}
}

func TestBuildInputItems_AttachmentImageSourceMapString(t *testing.T) {
	items, err := buildInputItems([]Message{{
		Role: "user",
		Content: []map[string]interface{}{
			{"type": "text", "text": "Describe this image."},
			{"type": "image", "source": map[string]string{"media_type": "image/png", "data": "BASE64PNG"}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items len = %d, want 1", len(items))
	}
	msg, ok := items[0].(map[string]interface{})
	if !ok || msg["type"] != "message" || msg["role"] != "user" {
		t.Fatalf("item should be user message, got %#v", items[0])
	}
	content, ok := msg["content"].([]interface{})
	if !ok || len(content) != 2 {
		t.Fatalf("message content = %#v", msg["content"])
	}
	image, ok := content[1].(map[string]interface{})
	if !ok || image["type"] != "input_image" {
		t.Fatalf("second content item should be input_image, got %#v", content[1])
	}
	if !strings.Contains(image["image_url"].(string), "BASE64PNG") {
		t.Fatalf("image_url did not contain attachment data: %#v", image)
	}
}

func TestBuildInputItems_RawAssistantFunctionCallBeforeToolOutput(t *testing.T) {
	raw := []apiOutputItem{
		{
			Type: "message",
			Role: "assistant",
			Content: []apiContentPart{{
				Type: "output_text",
				Text: "I'll check that.",
			}},
		},
		{
			Type:      "function_call",
			ID:        "fc_123",
			CallID:    "call_123",
			Name:      "bash",
			Arguments: `{"command":"pwd"}`,
		},
	}

	items, err := buildInputItems([]Message{
		{Role: "user", Content: "where am I?"},
		{Role: "assistant", RawContent: raw},
		{Role: "user", ToolResults: []ToolResult{{
			ToolUseID: "call_123",
			Content:   "/home/vibecraft",
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("items len = %d, want 4: %#v", len(items), items)
	}
	call, ok := items[2].(map[string]interface{})
	if !ok || call["type"] != "function_call" || call["call_id"] != "call_123" {
		t.Fatalf("third item should preserve raw function call, got %#v", items[2])
	}
	out, ok := items[3].(map[string]interface{})
	if !ok || out["type"] != "function_call_output" || out["call_id"] != "call_123" {
		t.Fatalf("fourth item should be matching tool output, got %#v", items[3])
	}
}

func TestSendText_UsesAuthorizationHeaderOnly(t *testing.T) {
	const apiKey = "sk-proj-secret-for-test-abcdefghijklmnopqrstuvwxyz"
	var gotAuth, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_1",
			"status":"completed",
			"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],
			"usage":{"input_tokens":3,"output_tokens":1}
		}`))
	}))
	defer server.Close()

	c := NewClient(apiKey)
	SetBaseURLForTests(c, server.URL)
	resp, err := c.SendText(context.Background(), "", "system", "hello", 32)
	if err != nil {
		t.Fatal(err)
	}
	if resp.TextContent != "ok" {
		t.Fatalf("TextContent = %q, want ok", resp.TextContent)
	}
	if gotAuth != "Bearer "+apiKey {
		t.Fatalf("Authorization = %q, want bearer key", gotAuth)
	}
	if strings.Contains(gotBody, apiKey) {
		t.Fatalf("request body leaked API key: %s", gotBody)
	}
	body := decodeRequestBody(t, gotBody)
	if _, ok := body["reasoning"]; ok {
		t.Fatalf("SendText should not attach manager-loop reasoning: %s", gotBody)
	}
}

func TestSendComputerUse_UsesExplicitManagerRuntimeDefaults(t *testing.T) {
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_1",
			"status":"completed",
			"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],
			"usage":{"input_tokens":3,"output_tokens":1}
		}`))
	}))
	defer server.Close()

	c := NewClient("sk-test")
	SetBaseURLForTests(c, server.URL)
	_, err := c.SendComputerUse(context.Background(), "system", []Message{{Role: "user", Content: "hello"}})
	if err != nil {
		t.Fatal(err)
	}

	body := decodeRequestBody(t, gotBody)
	if got := int(body["max_output_tokens"].(float64)); got != DefaultComputerMaxOutputTokens {
		t.Fatalf("max_output_tokens = %d, want %d", got, DefaultComputerMaxOutputTokens)
	}
	reasoning, ok := body["reasoning"].(map[string]interface{})
	if !ok || reasoning["effort"] != DefaultReasoningEffort {
		t.Fatalf("reasoning = %#v, want effort %q", body["reasoning"], DefaultReasoningEffort)
	}
	include, ok := body["include"].([]interface{})
	if !ok || len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %#v, want reasoning.encrypted_content", body["include"])
	}
	if _, ok := body["tools"]; !ok {
		t.Fatalf("SendComputerUse omitted tools: %s", gotBody)
	}
}

func TestSendComputerUse_EscalatesDeepPlanningRequests(t *testing.T) {
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_1",
			"status":"completed",
			"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],
			"usage":{"input_tokens":3,"output_tokens":1}
		}`))
	}))
	defer server.Close()

	c := NewClient("sk-test")
	SetBaseURLForTests(c, server.URL)
	_, err := c.SendComputerUse(context.Background(), "system", []Message{{
		Role:    "user",
		Content: "Think hard, design the system, deploy it, and test in prod.",
	}})
	if err != nil {
		t.Fatal(err)
	}

	body := decodeRequestBody(t, gotBody)
	if got := int(body["max_output_tokens"].(float64)); got != DefaultDeepMaxOutputTokens {
		t.Fatalf("max_output_tokens = %d, want %d", got, DefaultDeepMaxOutputTokens)
	}
	reasoning, ok := body["reasoning"].(map[string]interface{})
	if !ok || reasoning["effort"] != DefaultDeepReasoningEffort {
		t.Fatalf("reasoning = %#v, want effort %q", body["reasoning"], DefaultDeepReasoningEffort)
	}
}

func TestSendNoTools_OmitsToolsAndKeepsImageInput(t *testing.T) {
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_1",
			"status":"completed",
			"output":[{"type":"message","content":[{"type":"output_text","text":"red rectangle"}]}],
			"usage":{"input_tokens":3,"output_tokens":1}
		}`))
	}))
	defer server.Close()

	c := NewClient("sk-test")
	SetBaseURLForTests(c, server.URL)
	resp, err := c.SendNoTools(context.Background(), "system", []Message{{
		Role: "user",
		Content: []map[string]interface{}{
			{"type": "text", "text": "Describe this image."},
			{"type": "image", "source": map[string]interface{}{"media_type": "image/png", "data": "BASE64PNG"}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.TextContent != "red rectangle" {
		t.Fatalf("TextContent = %q, want red rectangle", resp.TextContent)
	}
	if strings.Contains(gotBody, `"tools"`) {
		t.Fatalf("SendNoTools included tools: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"input_image"`) || !strings.Contains(gotBody, "BASE64PNG") {
		t.Fatalf("SendNoTools did not include image input: %s", gotBody)
	}
	body := decodeRequestBody(t, gotBody)
	if got := int(body["max_output_tokens"].(float64)); got != DefaultComputerMaxOutputTokens {
		t.Fatalf("max_output_tokens = %d, want %d", got, DefaultComputerMaxOutputTokens)
	}
	reasoning, ok := body["reasoning"].(map[string]interface{})
	if !ok || reasoning["effort"] != DefaultReasoningEffort {
		t.Fatalf("reasoning = %#v, want effort %q", body["reasoning"], DefaultReasoningEffort)
	}
}

func TestSendText_MissingAPIKeyFailsBeforeHTTP(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := NewClient("")
	SetBaseURLForTests(c, server.URL)
	_, err := c.SendText(context.Background(), "", "system", "hello", 32)
	if err == nil || !strings.Contains(err.Error(), "OpenAI manager key is not configured") {
		t.Fatalf("expected missing-key error, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("missing-key client must not call upstream, calls=%d", calls)
	}
}

func decodeRequestBody(t *testing.T, raw string) map[string]interface{} {
	t.Helper()
	var body map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode request body: %v\n%s", err, raw)
	}
	return body
}
