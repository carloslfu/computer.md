// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	managerclient "github.com/carloslfu/computer.md/daemon/manager"
)

// CompactionThresholdTokens is the input-token watermark that triggers
// a mid-task compaction. Track K3 — lowered from 140K → 80K so long
// tasks compact sooner and each turn's fresh-input footprint stays
// smaller. Side benefit: faster API responses on long conversations.
const CompactionThresholdTokens = 80_000

// KeepRecentPairs is how many recent assistant+user message pairs to
// preserve verbatim when compaction fires. The agent reasons over these
// directly; anything older is collapsed into the summary block. 10 pairs
// = 20 messages, which is enough recency for the visual stack (last few
// screenshots) plus a couple of reasoning turns.
const KeepRecentPairs = 10

// MaxToolResultCharsInSummary caps how much of any single tool result
// we hand to the summarizer. Long shell outputs would otherwise blow
// up the summarizer's own input budget for no benefit — the summary
// just needs to know "what kind of result, roughly".
const MaxToolResultCharsInSummary = 1500

// Compactor summarizes old turns into a single synthetic user message
// so the agent's context stays bounded over long tasks. Uses the main
// agent model for the summary call — quality matters because the
// summary becomes the agent's only memory of those turns.
type Compactor struct {
	manager *managerclient.Client
	model   string
	broker  EventBroker
	usage   UsageRecorder // nil = usage not recorded (tests)
}

// SetUsageRecorder wires the per-machine usage accumulator for
// compaction calls. Called once at startup wiring.
func (c *Compactor) SetUsageRecorder(u UsageRecorder) {
	c.usage = u
}

// NewCompactor wires the compactor against the same manager client the
// engine uses. If model is empty, the client's default is used.
func NewCompactor(manager *managerclient.Client, model string, broker EventBroker) *Compactor {
	if model == "" {
		model = managerclient.DefaultModelID
	}
	return &Compactor{manager: manager, model: model, broker: broker}
}

// ShouldCompact returns true when the last API response's total input
// (cached + uncached) crossed the watermark. Caller invokes Compact
// before the next iteration's SendComputerUse.
func ShouldCompact(usage managerclient.Usage) bool {
	return usage.TotalInputTokens() > CompactionThresholdTokens
}

// Compact replaces messages between the initial (pre-loop) prefix and
// the recent tail with a single synthetic user message containing a
// structured summary. The initial messages and the last KeepRecentPairs*2
// messages pass through untouched.
//
// taskID + conversationID flow through so the broker event + usage
// attribution can identify which chat the compaction belonged to.
//
// On any error the original messages are returned unchanged — failed
// compaction degrades to "next call may be expensive" rather than
// breaking the task.
//
// taskID is used purely for the SSE event so the dashboard can show a
// "summarizing history…" status; pass "" to disable the event.
func (c *Compactor) Compact(ctx context.Context, taskID, conversationID string, messages []managerclient.Message, initMsgCount int) ([]managerclient.Message, error) {
	tail := KeepRecentPairs * 2
	if len(messages) <= initMsgCount+tail {
		return messages, nil
	}

	middle := messages[initMsgCount : len(messages)-tail]
	if len(middle) == 0 {
		return messages, nil
	}

	if taskID != "" && c.broker != nil {
		c.broker.Emit("task:compacting", map[string]interface{}{
			"task_id":          taskID,
			"summarizing_msgs": len(middle),
			"timestamp":        time.Now().UTC().Format(time.RFC3339),
		})
	}

	transcript := renderTranscript(middle)

	systemPrompt := "You are compacting an agent's working memory mid-task. The agent operates a real Linux desktop on behalf of a non-technical customer. Output a structured summary another instance of the same agent can use to continue without re-reading the transcript."

	userPrompt := fmt.Sprintf(`Below is the transcript of an agent's actions and observations so far. Produce a summary using exactly these sections, in order:

GOAL
- One sentence: what the customer asked for, restated.

COMPLETED ACTIONS
- Numbered list of meaningful things the agent did. Group related sub-steps (e.g. "Signed in to Gmail" not "clicked email field, typed, clicked next, …"). Reference real artifacts: apps opened, URLs visited, commands run, files created or read, values stored in the vault.

CURRENT STATE
- One paragraph: what is on screen right now, what processes are running, where in the task the agent is. Use the most recent observations.

OPEN QUESTIONS OR BLOCKERS
- Anything pausing the agent: pending user input, failed login, awaiting credentials, an error not yet resolved. Empty list if none.

KEY VALUES
- file paths, URLs, vault variable names ($NAME form), identifiers the agent has touched and may need again. Compact key: value form.

Be terse — under 1500 tokens total. This is working memory, not a report. Do not add narration about being a summarizer; output the sections directly.

<transcript>
%s
</transcript>`, transcript)

	release, reserveErr := reserveUsageCall(c.usage, c.model, conversationID)
	if reserveErr != nil {
		return messages, nil
	}
	start := time.Now()
	resp, err := c.manager.SendText(ctx, c.model, systemPrompt, userPrompt, 4096)
	elapsed := time.Since(start)
	if err != nil {
		release()
		return messages, fmt.Errorf("compactor SendText: %w", err)
	}

	// Attribute the compaction call's tokens to the originating chat so
	// the Usage panel's per-conversation breakdown reflects total spend.
	// The compactor is not "free"; its turns hit the same upstream
	// OpenAI bill as the main manager loop.
	if c.usage != nil {
		if uerr := c.usage.Record(c.model, conversationID, resp.Usage); uerr != nil {
			log.Printf("compactor: usage.Record: %v", uerr)
		}
	}
	release()

	summaryText := strings.TrimSpace(resp.TextContent)
	if summaryText == "" {
		return messages, fmt.Errorf("compactor: empty summary from model")
	}

	log.Printf(
		"compactor: collapsed %d msgs into %d output tokens in %s (input=%d, model=%s)",
		len(middle), resp.Usage.OutputTokens, elapsed, resp.Usage.InputTokens, c.model,
	)

	syntheticMsg := managerclient.Message{
		Role: "user",
		Content: fmt.Sprintf(
			"<task_history_summary>\nThe %d earlier turns of this task have been compacted into the following summary. Treat this as your memory of what happened before; the recent turns below are verbatim.\n\n%s\n</task_history_summary>",
			len(middle),
			summaryText,
		),
	}

	newMessages := make([]managerclient.Message, 0, initMsgCount+1+tail)
	newMessages = append(newMessages, messages[:initMsgCount]...)
	newMessages = append(newMessages, syntheticMsg)
	newMessages = append(newMessages, messages[len(messages)-tail:]...)

	if taskID != "" && c.broker != nil {
		c.broker.Emit("task:compacted", map[string]interface{}{
			"task_id":            taskID,
			"collapsed_msgs":     len(middle),
			"summary_tokens":     resp.Usage.OutputTokens,
			"summary_duration_s": elapsed.Seconds(),
			"timestamp":          time.Now().UTC().Format(time.RFC3339),
		})
	}

	return newMessages, nil
}

// renderTranscript flattens a slice of agent-loop messages into a
// readable text format for the summarizer. Base64 screenshot payloads
// are NEVER passed through — they would inflate the summarizer's input
// by orders of magnitude with no quality benefit. Long shell outputs
// are truncated.
func renderTranscript(msgs []managerclient.Message) string {
	var b strings.Builder
	for i, msg := range msgs {
		fmt.Fprintf(&b, "[Turn %d, role=%s]\n", i+1, msg.Role)
		renderOneMessage(&b, msg)
		b.WriteString("\n")
	}
	return b.String()
}

func renderOneMessage(b *strings.Builder, msg managerclient.Message) {
	// Tool results message (user role, carries tool_result blocks).
	if len(msg.ToolResults) > 0 {
		for _, tr := range msg.ToolResults {
			if tr.IsBase64Image {
				fmt.Fprintf(b, "  tool_result(screenshot): [image — contents not shown in summary]\n")
				continue
			}
			content := tr.Content
			if len(content) > MaxToolResultCharsInSummary {
				content = content[:MaxToolResultCharsInSummary] + "…[truncated]"
			}
			if tr.IsError {
				fmt.Fprintf(b, "  tool_result(error): %s\n", content)
			} else {
				fmt.Fprintf(b, "  tool_result: %s\n", content)
			}
		}
		return
	}

	// Assistant turn — RawContent is the parsed manager response content,
	// JSON-serializable directly.
	if msg.RawContent != nil {
		renderRawContent(b, msg.RawContent)
		return
	}

	// User turn with a typed Content (string or block array).
	switch v := msg.Content.(type) {
	case string:
		fmt.Fprintf(b, "  %s\n", v)
	case []map[string]interface{}:
		for _, block := range v {
			renderInterfaceBlock(b, block)
		}
	case []interface{}:
		for _, block := range v {
			if m, ok := block.(map[string]interface{}); ok {
				renderInterfaceBlock(b, m)
			}
		}
	default:
		// Best-effort JSON dump for anything we didn't anticipate.
		data, _ := json.Marshal(msg.Content)
		fmt.Fprintf(b, "  %s\n", string(data))
	}
}

// renderRawContent handles assistant turns. RawContent comes back from
// the API as a concrete manager-client struct, but for our purposes it's
// enough to round-trip through JSON and pick out the fields by name.
// Avoids exporting the struct.
func renderRawContent(b *strings.Builder, raw interface{}) {
	data, err := json.Marshal(raw)
	if err != nil {
		fmt.Fprintf(b, "  [unrenderable assistant turn: %v]\n", err)
		return
	}
	var blocks []map[string]interface{}
	if err := json.Unmarshal(data, &blocks); err != nil {
		fmt.Fprintf(b, "  [unparsable assistant turn: %v]\n", err)
		return
	}
	for _, block := range blocks {
		renderInterfaceBlock(b, block)
	}
}

func renderInterfaceBlock(b *strings.Builder, block map[string]interface{}) {
	blockType, _ := block["type"].(string)
	switch blockType {
	case "text":
		text, _ := block["text"].(string)
		if text != "" {
			fmt.Fprintf(b, "  text: %s\n", text)
		}
	case "tool_use":
		name, _ := block["name"].(string)
		inputJSON, _ := json.Marshal(block["input"])
		fmt.Fprintf(b, "  tool_use(%s): %s\n", name, string(inputJSON))
	case "image":
		fmt.Fprintf(b, "  image: [content not shown in summary]\n")
	case "tool_result":
		content := block["content"]
		data, _ := json.Marshal(content)
		s := string(data)
		if len(s) > MaxToolResultCharsInSummary {
			s = s[:MaxToolResultCharsInSummary] + "…[truncated]"
		}
		fmt.Fprintf(b, "  tool_result: %s\n", s)
	case "server_tool_use":
		// Hosted tool call. Render just the call signature; the result
		// lands as its own block.
		name, _ := block["name"].(string)
		inputJSON, _ := json.Marshal(block["input"])
		fmt.Fprintf(b, "  server_tool_use(%s): %s\n", name, string(inputJSON))
	case "web_search_tool_result":
		// Compact: just the count of results and the first few URLs.
		// The summary doesn't need full snippets; it needs to know
		// "the agent searched and got results", with enough fingerprint
		// (URLs) for the next agent to recognize the territory.
		summary := summarizeWebSearchResults(block["content"])
		fmt.Fprintf(b, "  web_search_result: %s\n", summary)
	case "web_fetch_tool_result":
		// Render the URL plus a short tail of the fetched body.
		summary := summarizeWebFetchResult(block["content"])
		fmt.Fprintf(b, "  web_fetch_result: %s\n", summary)
	case "code_execution_tool_result":
		// Render return_code plus a tail of stdout/stderr. The summarizer
		// doesn't need the full output — it needs to know "ran, succeeded,
		// produced ~X".
		summary := summarizeCodeExecResult(block["content"])
		fmt.Fprintf(b, "  code_execution_result: %s\n", summary)
	default:
		data, _ := json.Marshal(block)
		fmt.Fprintf(b, "  [%s]: %s\n", blockType, string(data))
	}
}

// summarizeWebSearchResults turns the raw content of a
// web_search_tool_result block into a one-line summary for the
// compactor — "N results: url1, url2, …".
func summarizeWebSearchResults(content interface{}) string {
	arr, ok := content.([]interface{})
	if !ok {
		return "[unrecognized search result shape]"
	}
	var urls []string
	for _, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if u, ok := m["url"].(string); ok && u != "" {
			urls = append(urls, u)
		}
	}
	if len(urls) == 0 {
		return fmt.Sprintf("%d results (no URLs parsed)", len(arr))
	}
	const maxURLs = 5
	if len(urls) > maxURLs {
		urls = urls[:maxURLs]
	}
	return fmt.Sprintf("%d results: %s", len(arr), strings.Join(urls, ", "))
}

// summarizeCodeExecResult turns the content of a code_execution_tool_result
// block into a compact "rc=N stdout=... stderr=..." line for the summarizer.
func summarizeCodeExecResult(content interface{}) string {
	m, ok := content.(map[string]interface{})
	if !ok {
		return "[unrecognized code_execution result shape]"
	}
	rc, _ := m["return_code"].(float64) // JSON numbers decode as float64
	stdout, _ := m["stdout"].(string)
	stderr, _ := m["stderr"].(string)
	const tail = 300
	if len(stdout) > tail {
		stdout = stdout[:tail] + "…"
	}
	if len(stderr) > tail {
		stderr = stderr[:tail] + "…"
	}
	if stderr != "" {
		return fmt.Sprintf("rc=%d stdout=%q stderr=%q", int(rc), stdout, stderr)
	}
	return fmt.Sprintf("rc=%d stdout=%q", int(rc), stdout)
}

// summarizeWebFetchResult extracts the URL and a short tail of the
// fetched body from a web_fetch_tool_result content block.
func summarizeWebFetchResult(content interface{}) string {
	m, ok := content.(map[string]interface{})
	if !ok {
		return "[unrecognized fetch result shape]"
	}
	url, _ := m["url"].(string)
	if url == "" {
		url = "[no url]"
	}
	// content.content.source.data is the fetched text payload.
	body := ""
	if inner, ok := m["content"].(map[string]interface{}); ok {
		if src, ok := inner["source"].(map[string]interface{}); ok {
			body, _ = src["data"].(string)
		}
	}
	const maxBody = 300
	if len(body) > maxBody {
		body = body[:maxBody] + "…"
	}
	if body == "" {
		return url
	}
	return fmt.Sprintf("%s — %s", url, body)
}
