// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is the HTTP client for the VibeCraft API.
type Client struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
	UserAgent  string
}

// New creates a new VibeCraft API client.
func New(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		UserAgent: "vibecraft-cli/dev",
	}
}

// APIError represents an error response from the API.
type APIError struct {
	StatusCode int
	Message    string
	Body       string
	RetryAfter time.Duration // honored on 429 from the Retry-After header
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("API error (%d): %s", e.StatusCode, redactSecrets(e.Message))
	}
	return fmt.Sprintf("API error (%d): %s", e.StatusCode, redactSecrets(e.Body))
}

// redactSecrets scrubs anything that looks like a vc_machine_* key from
// strings that may end up in error messages or logs. Mirror of the
// daemon's vault/mask.go but with no dependency on it (kept private).
func redactSecrets(s string) string {
	const prefix = "vc_machine_"
	idx := strings.Index(s, prefix)
	if idx < 0 {
		return s
	}
	out := s[:idx] + "[redacted]"
	// Skip past the key body — accept any alnum / _ / -.
	end := idx + len(prefix)
	for end < len(s) {
		c := s[end]
		isAlnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !isAlnum && c != '_' && c != '-' {
			break
		}
		end++
	}
	if end < len(s) {
		out += redactSecrets(s[end:])
	}
	return out
}

// MachineStatus represents the daemon /status response (user-facing).
type MachineStatus struct {
	Status      string `json:"status"`
	MachineID   string `json:"machine_id"`
	Uptime      int64  `json:"uptime"`
	Timestamp   string `json:"timestamp"`
	CurrentTask string `json:"current_task"`
}

// CreateKeyRequest is the body for creating an API key on the machine.
type CreateKeyRequest struct {
	Name string `json:"name"`
}

// CreateKeyResponse is the response from creating an API key.
type CreateKeyResponse struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

// Task represents a task in the system.
type Task struct {
	ID             string `json:"id"`
	Instruction    string `json:"instruction"`
	Status         string `json:"status"`
	Result         string `json:"result"`
	Error          string `json:"error_message"`
	ConversationID string `json:"conversation_id"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

// TaskSubmitResponse is returned when a new task is submitted.
type TaskSubmitResponse struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	ConversationID string `json:"conversation_id"`
	CreatedAt      string `json:"created_at"`
}

// TaskListResponse wraps a list of tasks.
type TaskListResponse struct {
	Tasks []Task `json:"tasks"`
}

// Message is one entry in a conversation transcript.
type Message struct {
	ID        string `json:"id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
	TaskID    string `json:"task_id,omitempty"`
}

// ConversationResponse is the daemon /conversations/{id} payload.
type ConversationResponse struct {
	Conversation map[string]any `json:"conversation"`
	Messages     []Message      `json:"messages"`
	Tasks        []Task         `json:"tasks"`
}

func (c *Client) newRequest(method, path string, body interface{}) (*http.Request, error) {
	reqURL := c.BaseURL + "/api" + path

	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshaling request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, reqURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	ua := c.UserAgent
	if ua == "" {
		ua = "vibecraft-cli/dev"
	}
	req.Header.Set("User-Agent", ua)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return req, nil
}

func (c *Client) do(req *http.Request, result interface{}) error {
	// G3: retry on transient failures (network, 5xx, 429). Idempotent
	// methods (GET) retry freely; non-idempotent methods (POST, etc.)
	// only retry on 429 (since the daemon explicitly told us to back
	// off — the request never executed against business state) and on
	// pre-flight network errors (the request never landed).
	const maxAttempts = 3

	idempotent := req.Method == http.MethodGet || req.Method == http.MethodHead

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Body is consumed once on first attempt; capture for replay.
		var bodyBytes []byte
		if req.Body != nil && req.GetBody == nil {
			b, err := io.ReadAll(req.Body)
			if err != nil {
				return fmt.Errorf("reading request body: %w", err)
			}
			bodyBytes = b
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}
		if attempt > 1 && req.GetBody != nil {
			b, err := req.GetBody()
			if err != nil {
				return fmt.Errorf("re-reading request body: %w", err)
			}
			req.Body = b
		} else if attempt > 1 && bodyBytes != nil {
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}

		err := c.doOnce(req, result)
		if err == nil {
			return nil
		}
		lastErr = err

		// Decide whether to retry.
		var apiErr *APIError
		canRetry := false
		var sleepFor time.Duration
		if isNetErr(err) {
			canRetry = true
			sleepFor = backoff(attempt)
		} else if errorsAs(err, &apiErr) {
			switch {
			case apiErr.StatusCode == 429:
				canRetry = true
				if apiErr.RetryAfter > 0 {
					sleepFor = apiErr.RetryAfter
				} else {
					sleepFor = backoff(attempt)
				}
			case apiErr.StatusCode >= 500 && apiErr.StatusCode <= 599 && idempotent:
				canRetry = true
				sleepFor = backoff(attempt)
			}
		}
		if !canRetry || attempt == maxAttempts {
			return err
		}
		time.Sleep(sleepFor)
	}
	return lastErr
}

// doOnce is the single-attempt HTTP call without any retry logic.
func (c *Client) doOnce(req *http.Request, result interface{}) error {
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &APIError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
		}
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil {
				apiErr.RetryAfter = time.Duration(secs) * time.Second
			}
		}
		var errResp struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(respBody, &errResp) == nil {
			if errResp.Error != "" {
				apiErr.Message = errResp.Error
			} else if errResp.Message != "" {
				apiErr.Message = errResp.Message
			}
		}
		return apiErr
	}

	if result != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}

	return nil
}

// errorsAs is a thin wrapper around errors.As to avoid importing errors
// just for one call site. Same semantics.
func errorsAs(err error, target any) bool {
	for err != nil {
		if t, ok := target.(**APIError); ok {
			if a, ok := err.(*APIError); ok {
				*t = a
				return true
			}
		}
		type wrapper interface{ Unwrap() error }
		if w, ok := err.(wrapper); ok {
			err = w.Unwrap()
			continue
		}
		break
	}
	return false
}

// isNetErr returns true for errors that look like transient transport
// failures (DNS, connection refused, EOF mid-read, etc.) and false for
// API-level errors that should not be retried by default.
func isNetErr(err error) bool {
	if err == nil {
		return false
	}
	// We don't have a strict net.Error check here because net errors are
	// often wrapped through fmt.Errorf("%w") with our own context. The
	// only failure path that surfaces as a non-net error is APIError.
	if _, ok := err.(*APIError); ok {
		return false
	}
	return true
}

// backoff returns the sleep duration before the next retry. Capped at
// ~5s to keep agents responsive.
func backoff(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 250 * time.Millisecond
	case 2:
		return 750 * time.Millisecond
	default:
		return 2 * time.Second
	}
}

// GetStatus returns the machine status (user-facing, authenticated via API key).
func (c *Client) GetStatus() (*MachineStatus, error) {
	req, err := c.newRequest("GET", "/status", nil)
	if err != nil {
		return nil, err
	}
	var status MachineStatus
	if err := c.do(req, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// SubmitTaskRequest extends SubmitTask with optional fields.
type SubmitTaskRequest struct {
	Instruction    string `json:"instruction"`
	ConversationID string `json:"conversation_id,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// SubmitTask sends a new task to the machine.
func (c *Client) SubmitTask(message string) (*TaskSubmitResponse, error) {
	return c.SubmitTaskWith(SubmitTaskRequest{Instruction: message})
}

// SubmitTaskWith is the full form supporting conversation continuation and idempotency.
func (c *Client) SubmitTaskWith(req SubmitTaskRequest) (*TaskSubmitResponse, error) {
	httpReq, err := c.newRequest("POST", "/task", req)
	if err != nil {
		return nil, err
	}
	var resp TaskSubmitResponse
	if err := c.do(httpReq, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetTask returns the status of a specific task.
func (c *Client) GetTask(id string) (*Task, error) {
	req, err := c.newRequest("GET", "/task/"+url.PathEscape(id)+"/status", nil)
	if err != nil {
		return nil, err
	}
	var task Task
	if err := c.do(req, &task); err != nil {
		return nil, err
	}
	return &task, nil
}

// ListTasksOpts is the filter set for ListTasks.
type ListTasksOpts struct {
	Status string
	Since  string
	Limit  int
}

// ListTasks returns recent tasks, with optional filters applied client-side.
func (c *Client) ListTasks() (*TaskListResponse, error) {
	return c.ListTasksWith(ListTasksOpts{})
}

// ListTasksWith fetches recent tasks and applies the optional filters.
// The daemon exposes the recent-tasks list at /api/tasks (plural), not
// /api/task. /api/task is POST-only (task create); /api/task/<id>/...
// is the by-id family. Filtering is client-side because the daemon's
// list endpoint caps at 50.
func (c *Client) ListTasksWith(opts ListTasksOpts) (*TaskListResponse, error) {
	req, err := c.newRequest("GET", "/tasks", nil)
	if err != nil {
		return nil, err
	}
	var resp TaskListResponse
	if err := c.do(req, &resp); err != nil {
		return nil, err
	}

	// Apply filters client-side.
	if opts.Status == "" && opts.Since == "" && opts.Limit == 0 {
		return &resp, nil
	}

	var sinceT time.Time
	if opts.Since != "" {
		t, parseErr := time.Parse(time.RFC3339, opts.Since)
		if parseErr != nil {
			return nil, fmt.Errorf("parsing --since timestamp (need RFC3339): %w", parseErr)
		}
		sinceT = t
	}

	out := make([]Task, 0, len(resp.Tasks))
	for _, t := range resp.Tasks {
		if opts.Status != "" && t.Status != opts.Status {
			continue
		}
		if !sinceT.IsZero() {
			tt, parseErr := time.Parse(time.RFC3339, t.CreatedAt)
			if parseErr == nil && tt.Before(sinceT) {
				continue
			}
		}
		out = append(out, t)
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}
	return &TaskListResponse{Tasks: out}, nil
}

// CancelTask cancels a running task.
func (c *Client) CancelTask(id string) error {
	req, err := c.newRequest("POST", "/task/"+url.PathEscape(id)+"/cancel", nil)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

// RespondToTask posts a user input message to a task that is waiting_for_input.
func (c *Client) RespondToTask(id, input string) error {
	req, err := c.newRequest("POST", "/task/"+url.PathEscape(id)+"/respond", map[string]string{
		"input": input,
	})
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

// GetConversation returns the full conversation (messages + tasks).
func (c *Client) GetConversation(id string) (*ConversationResponse, error) {
	req, err := c.newRequest("GET", "/conversations/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	var resp ConversationResponse
	if err := c.do(req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetTaskMessages returns the messages for the conversation that owns the task.
func (c *Client) GetTaskMessages(taskID string) (*ConversationResponse, error) {
	task, err := c.GetTask(taskID)
	if err != nil {
		return nil, err
	}
	if task.ConversationID == "" {
		return nil, fmt.Errorf("task %s has no conversation_id", taskID)
	}
	return c.GetConversation(task.ConversationID)
}

// SubmitAndWatchTask submits a task and polls for its completion.
func (c *Client) SubmitAndWatchTask(ctx context.Context, message string, onUpdate func(*Task)) (*Task, error) {
	resp, err := c.SubmitTask(message)
	if err != nil {
		return nil, err
	}

	return c.PollTask(ctx, resp.ID, 2*time.Second, onUpdate)
}

// GetScreenshot downloads a screenshot from the machine and returns PNG bytes.
func (c *Client) GetScreenshot() ([]byte, error) {
	req, err := c.newRequest("GET", "/screenshot", nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{
		Transport: c.HTTPClient.Transport,
		Timeout:   60 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &APIError{
			StatusCode: resp.StatusCode,
			Body:       string(body),
		}
		var errResp struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &errResp) == nil {
			if errResp.Error != "" {
				apiErr.Message = errResp.Error
			} else if errResp.Message != "" {
				apiErr.Message = errResp.Message
			}
		}
		return nil, apiErr
	}

	// The daemon may return JSON with base64-encoded image data.
	var imgResp struct {
		Image    string `json:"image"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(body, &imgResp); err != nil {
		// Not JSON — return raw bytes.
		return body, nil
	}

	if imgResp.Image != "" && imgResp.Encoding == "base64" {
		decoded, err := base64.StdEncoding.DecodeString(imgResp.Image)
		if err != nil {
			return nil, fmt.Errorf("decoding screenshot: %w", err)
		}
		return decoded, nil
	}

	return body, nil
}

// CreateKey creates a new API key on the machine using a JWT for auth.
func (c *Client) CreateKey(jwt string, name string) (*CreateKeyResponse, error) {
	reqURL := c.BaseURL + "/api/keys"

	body, err := json.Marshal(CreateKeyRequest{Name: name})
	if err != nil {
		return nil, fmt.Errorf("marshaling request body: %w", err)
	}

	req, err := http.NewRequest("POST", reqURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	ua := c.UserAgent
	if ua == "" {
		ua = "vibecraft-cli/dev"
	}
	req.Header.Set("User-Agent", ua)

	var resp CreateKeyResponse
	if err := c.do(req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// PollTask polls a task until it reaches a terminal state or context is cancelled.
func (c *Client) PollTask(ctx context.Context, id string, interval time.Duration, onUpdate func(*Task)) (*Task, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		task, err := c.GetTask(id)
		if err != nil {
			return nil, err
		}

		if onUpdate != nil {
			onUpdate(task)
		}

		switch task.Status {
		case "completed", "failed", "cancelled", "waiting_for_input":
			return task, nil
		}

		select {
		case <-ctx.Done():
			return task, ctx.Err()
		case <-ticker.C:
		}
	}
}

// StreamEvent is one event from the daemon's SSE /stream endpoint.
type StreamEvent struct {
	Type string          `json:"-"` // SSE "event:" field; empty for "data only" events
	Data json.RawMessage `json:"-"` // raw JSON from the data: lines
	ID   string          `json:"-"` // SSE "id:" if present
	Raw  string          `json:"-"` // the entire data: payload as a string (for non-JSON consumers)
}

// StreamOpts controls subscription filtering on the daemon.
type StreamOpts struct {
	// TaskID, when non-empty, filters to events about this task only.
	TaskID string
	// ConversationID, when non-empty, filters to events about this conversation.
	ConversationID string
}

// Stream opens an SSE connection to /api/stream and invokes onEvent for
// each event received. Returns when the context is cancelled, the
// connection closes cleanly, or an error occurs.
//
// onEvent is called from the reader goroutine — callers should not block
// inside it for long.
func (c *Client) Stream(ctx context.Context, opts StreamOpts, onEvent func(StreamEvent) error) error {
	u, err := url.Parse(c.BaseURL + "/api/stream")
	if err != nil {
		return fmt.Errorf("parsing URL: %w", err)
	}
	q := u.Query()
	if opts.TaskID != "" {
		q.Set("task", opts.TaskID)
	}
	if opts.ConversationID != "" {
		q.Set("conversation", opts.ConversationID)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	ua := c.UserAgent
	if ua == "" {
		ua = "vibecraft-cli/dev"
	}
	req.Header.Set("User-Agent", ua)

	// Streams have no overall timeout; the per-request timeout applies to
	// the initial response only, so swap to a no-timeout client for the body.
	streamClient := &http.Client{
		Transport: c.HTTPClient.Transport,
		Timeout:   0,
	}
	resp, err := streamClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		apiErr := &APIError{
			StatusCode: resp.StatusCode,
			Body:       string(body),
		}
		var errResp struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &errResp) == nil {
			if errResp.Error != "" {
				apiErr.Message = errResp.Error
			} else if errResp.Message != "" {
				apiErr.Message = errResp.Message
			}
		}
		return apiErr
	}

	reader := bufio.NewReader(resp.Body)
	return parseSSE(reader, onEvent)
}

// parseSSE reads SSE-formatted events and dispatches them through onEvent.
// Each event is delimited by a blank line and consists of optional
// "event:", "data:", "id:" and "retry:" lines.
func parseSSE(reader *bufio.Reader, onEvent func(StreamEvent) error) error {
	var ev StreamEvent
	var dataBuf bytes.Buffer

	flush := func() error {
		if dataBuf.Len() == 0 && ev.Type == "" {
			return nil
		}
		ev.Raw = dataBuf.String()
		if len(ev.Raw) > 0 {
			ev.Data = json.RawMessage(strings.TrimSpace(ev.Raw))
		}
		err := onEvent(ev)
		ev = StreamEvent{}
		dataBuf.Reset()
		return err
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				_ = flush()
				return nil
			}
			return fmt.Errorf("reading SSE: %w", err)
		}
		// Strip trailing \r\n or \n.
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			// SSE comment / heartbeat; ignore.
			continue
		}
		colon := strings.IndexByte(line, ':')
		var field, value string
		if colon < 0 {
			field = line
		} else {
			field = line[:colon]
			value = line[colon+1:]
			value = strings.TrimPrefix(value, " ")
		}
		switch field {
		case "event":
			ev.Type = value
		case "data":
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(value)
		case "id":
			ev.ID = value
		case "retry":
			// Ignored — we have our own reconnect policy.
		}
	}
}

// FileEntry mirrors the daemon's FileInfo response for files-ls.
type FileEntry struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Size    int64  `json:"size,omitempty"`
	Type    string `json:"type"`
	ModTime string `json:"mtime"`
}

// FilesLsResponse is the shape of GET /api/files-ls.
type FilesLsResponse struct {
	Path    string      `json:"path"`
	Entries []FileEntry `json:"entries"`
}

// ListFiles returns the directory listing for a path inside the machine's
// /home/vibecraft jail. Empty path defaults to /home/vibecraft.
func (c *Client) ListFiles(path string) (*FilesLsResponse, error) {
	endpoint := "/files-ls"
	if path != "" {
		q := url.Values{}
		q.Set("path", path)
		endpoint += "?" + q.Encode()
	}
	req, err := c.newRequest("GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	var resp FilesLsResponse
	if err := c.do(req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// PullFile streams the file at the given path. Caller must close the
// returned io.ReadCloser. Size is set from Content-Length when present.
func (c *Client) PullFile(path string) (io.ReadCloser, int64, string, error) {
	endpoint := "/files/" + strings.TrimPrefix(path, "/")
	reqURL := c.BaseURL + "/api" + endpoint
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, 0, "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	ua := c.UserAgent
	if ua == "" {
		ua = "vibecraft-cli/dev"
	}
	req.Header.Set("User-Agent", ua)

	streamClient := &http.Client{
		Transport: c.HTTPClient.Transport,
		Timeout:   5 * time.Minute,
	}
	resp, err := streamClient.Do(req)
	if err != nil {
		return nil, 0, "", fmt.Errorf("sending request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		apiErr := &APIError{
			StatusCode: resp.StatusCode,
			Body:       string(body),
		}
		var errResp struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &errResp)
		if errResp.Error != "" {
			apiErr.Message = errResp.Error
		}
		return nil, 0, "", apiErr
	}
	return resp.Body, resp.ContentLength, resp.Header.Get("Content-Type"), nil
}

// VaultSecret describes a stored secret's metadata (no value).
type VaultSecret struct {
	Name      string `json:"name"`
	Label     string `json:"label,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// ListVault returns the metadata for every stored secret.
func (c *Client) ListVault() ([]VaultSecret, error) {
	req, err := c.newRequest("GET", "/vault", nil)
	if err != nil {
		return nil, err
	}
	var resp []VaultSecret
	if err := c.do(req, &resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// SetVault stores a secret (creates or updates).
func (c *Client) SetVault(name, value, label string) error {
	body := map[string]string{"name": name, "value": value}
	if label != "" {
		body["label"] = label
	}
	req, err := c.newRequest("POST", "/vault", body)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

// DeleteVault removes a secret by name.
func (c *Client) DeleteVault(name string) error {
	req, err := c.newRequest("DELETE", "/vault/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

// MemoryItem is one memory key/value record.
type MemoryItem struct {
	ID        string  `json:"id"`
	Category  string  `json:"category"`
	Key       string  `json:"key"`
	Value     string  `json:"value"`
	Metadata  *string `json:"metadata,omitempty"`
	CreatedAt string  `json:"created_at,omitempty"`
	UpdatedAt string  `json:"updated_at,omitempty"`
}

// ListMemoryOpts is the filter set for ListMemory.
type ListMemoryOpts struct {
	Category string
	Query    string
}

// ListMemory returns memory items, optionally filtered.
func (c *Client) ListMemory(opts ListMemoryOpts) ([]MemoryItem, error) {
	path := "/memory"
	q := url.Values{}
	if opts.Category != "" {
		q.Set("category", opts.Category)
	}
	if opts.Query != "" {
		q.Set("q", opts.Query)
	}
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := c.newRequest("GET", path, nil)
	if err != nil {
		return nil, err
	}
	var resp []MemoryItem
	if err := c.do(req, &resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// SetMemory writes a memory key/value pair.
func (c *Client) SetMemory(category, key, value string) (*MemoryItem, error) {
	body := map[string]string{"key": key, "value": value}
	if category != "" {
		body["category"] = category
	}
	req, err := c.newRequest("POST", "/memory", body)
	if err != nil {
		return nil, err
	}
	var resp MemoryItem
	if err := c.do(req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// DeleteMemory removes a memory item by its daemon-assigned id.
func (c *Client) DeleteMemory(id string) error {
	req, err := c.newRequest("DELETE", "/memory/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

// GetVersion calls the daemon's /api/version probe. Older daemons return
// 404; the client treats that as "schema version 1 implicit" so we don't
// hard-fail against pre-version-handshake daemons.
type DaemonVersion struct {
	Version        string `json:"version"`
	SchemaVersions []int  `json:"schema_versions"`
}

func (c *Client) GetVersion() (*DaemonVersion, error) {
	req, err := c.newRequest("GET", "/version", nil)
	if err != nil {
		return nil, err
	}
	var resp DaemonVersion
	if err := c.do(req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// UploadFile pushes a single file to the daemon's /api/upload endpoint
// for a given conversation. Returns the daemon's response payload.
func (c *Client) UploadFile(conversationID, filename string, content io.Reader) (map[string]any, error) {
	var buf bytes.Buffer
	mw := newMultipartWriter(&buf)
	if err := mw.writeField("conversation_id", conversationID); err != nil {
		return nil, err
	}
	if err := mw.writeFile("file", filename, content); err != nil {
		return nil, err
	}
	if err := mw.close(); err != nil {
		return nil, err
	}

	reqURL := c.BaseURL + "/api/upload"
	req, err := http.NewRequest("POST", reqURL, &buf)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", mw.contentType())
	req.Header.Set("Content-Length", strconv.Itoa(buf.Len()))
	ua := c.UserAgent
	if ua == "" {
		ua = "vibecraft-cli/dev"
	}
	req.Header.Set("User-Agent", ua)

	var resp map[string]any
	if err := c.do(req, &resp); err != nil {
		return nil, err
	}
	return resp, nil
}
