package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

func TestSanitizeToolPairsDropsOrphanToolResult(t *testing.T) {
	messages := []provider.Message{
		{Role: "user", Content: "hello"},
		{Role: "tool", ToolCallID: "orphan", Name: "terminal", Content: "orphan result"},
		{Role: "assistant", Content: "done"},
	}

	got := sanitizeToolPairs(messages)
	if len(got) != 2 {
		t.Fatalf("sanitizeToolPairs returned %d messages, want 2", len(got))
	}
	if got[0].Role != "user" || got[0].Content != "hello" {
		t.Fatalf("first message = %+v, want original user", got[0])
	}
	if got[1].Role != "assistant" || got[1].Content != "done" {
		t.Fatalf("second message = %+v, want original assistant", got[1])
	}
}

func TestSanitizeToolPairsDropsIncompleteAssistantToolCallGroup(t *testing.T) {
	messages := []provider.Message{
		{Role: "user", Content: "run both"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{
			{ID: "call_a", Type: "function", Function: provider.FunctionCall{Name: "terminal"}},
			{ID: "call_b", Type: "function", Function: provider.FunctionCall{Name: "read_file"}},
		}},
		{Role: "tool", ToolCallID: "call_a", Name: "terminal", Content: "ok"},
		{Role: "user", Content: "next"},
	}

	got := sanitizeToolPairs(messages)
	if len(got) != 2 {
		t.Fatalf("sanitizeToolPairs returned %d messages, want 2", len(got))
	}
	if got[0].Role != "user" || got[0].Content != "run both" {
		t.Fatalf("first message = %+v, want first user", got[0])
	}
	if got[1].Role != "user" || got[1].Content != "next" {
		t.Fatalf("second message = %+v, want following user", got[1])
	}
}

func TestSanitizeToolPairsKeepsCompleteParsedToolCallGroupUnchanged(t *testing.T) {
	messages := []provider.Message{
		{Role: "user", Content: "run both"},
		{Role: "assistant", Content: "using tools", ToolCalls: []provider.ToolCall{
			{ID: "call_a", Type: "function", Function: provider.FunctionCall{Name: "terminal"}},
			{ID: "call_b", Type: "function", Function: provider.FunctionCall{Name: "read_file"}},
		}},
		{Role: "tool", ToolCallID: "call_a", Name: "terminal", Content: "ok"},
		{Role: "tool", ToolCallID: "call_b", Name: "read_file", Content: "file"},
		{Role: "assistant", Content: "done"},
	}

	got := sanitizeToolPairs(messages)
	if len(got) != len(messages) {
		t.Fatalf("sanitizeToolPairs returned %d messages, want %d", len(got), len(messages))
	}
	for i := range messages {
		if got[i].Role != messages[i].Role || got[i].Content != messages[i].Content || got[i].ToolCallID != messages[i].ToolCallID {
			t.Fatalf("message %d = %+v, want role/content/tool id from %+v", i, got[i], messages[i])
		}
		if len(got[i].ToolCalls) != len(messages[i].ToolCalls) {
			t.Fatalf("message %d has %d tool calls, want %d", i, len(got[i].ToolCalls), len(messages[i].ToolCalls))
		}
		for j := range messages[i].ToolCalls {
			if got[i].ToolCalls[j].ID != messages[i].ToolCalls[j].ID {
				t.Fatalf("message %d tool call %d id = %q, want %q", i, j, got[i].ToolCalls[j].ID, messages[i].ToolCalls[j].ID)
			}
		}
	}
}

func TestSanitizeToolPairsKeepsTextFromIncompleteAssistantToolCallGroup(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"tool_calls": []map[string]any{
			{"id": "call_a", "type": "function", "function": map[string]any{"name": "terminal", "arguments": "{}"}},
			{"id": "call_b", "type": "function", "function": map[string]any{"name": "read_file", "arguments": "{}"}},
		},
	})
	if err != nil {
		t.Fatalf("marshal raw assistant: %v", err)
	}
	messages := []provider.Message{
		{Role: "user", Content: "run both"},
		{
			Role:         "assistant",
			Content:      "working",
			Thinking:     "thought",
			RawAssistant: raw,
			ToolCalls: []provider.ToolCall{
				{ID: "call_a", Type: "function", Function: provider.FunctionCall{Name: "terminal"}},
				{ID: "call_b", Type: "function", Function: provider.FunctionCall{Name: "read_file"}},
			},
		},
		{Role: "tool", ToolCallID: "call_a", Name: "terminal", Content: "ok"},
		{Role: "user", Content: "next"},
	}

	got := sanitizeToolPairs(messages)
	if len(got) != 3 {
		t.Fatalf("sanitizeToolPairs returned %d messages, want 3", len(got))
	}
	if got[1].Role != "assistant" || got[1].Content != "working" || got[1].Thinking != "thought" {
		t.Fatalf("preserved assistant = %+v, want text/thinking assistant", got[1])
	}
	if len(got[1].ToolCalls) != 0 {
		t.Fatalf("preserved assistant kept %d tool calls, want 0", len(got[1].ToolCalls))
	}
	if len(got[1].RawAssistant) != 0 {
		t.Fatalf("preserved assistant kept RawAssistant %s, want cleared", string(got[1].RawAssistant))
	}
	if got[2].Role != "user" || got[2].Content != "next" {
		t.Fatalf("last message = %+v, want following user", got[2])
	}
}

func TestPruneOldToolResultsSummarizesTerminalResultLocally(t *testing.T) {
	args := `{"command":"go test ./internal/agent","timeout":30}`
	content := "Exit code: 2\nOutput:\nfirst line\nsecond line\n" + strings.Repeat("x", 2201)
	messages := []provider.Message{
		{Role: "user", Content: "test"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{
			ID:       "call_terminal",
			Type:     "function",
			Function: provider.FunctionCall{Name: "terminal", Arguments: args},
		}}},
		{
			Role:       "tool",
			ToolCallID: "call_terminal",
			Name:       "terminal",
			Content:    content,
			Metadata:   map[string]any{"sandbox": true},
		},
	}
	messages = append(messages, compactionFillerMessages(PruneTurnAge)...)

	got, changed := pruneOldToolResultsWithChange(messages)
	if !changed {
		t.Fatal("pruneOldToolResultsWithChange changed = false, want true")
	}
	summary := got[2].Content
	assertContainsAll(t, summary,
		"[Tool Result Summary]",
		"tool: terminal",
		"command: go test ./internal/agent",
		"exit_code: 2",
		fmt.Sprintf("output_lines: %d", testLineCount(content)),
		fmt.Sprintf("output_chars: %d", len([]rune(content))),
	)
	for _, forbidden := range []string{"memory logs", "retrieve_compacted_tool_result", "archive_id"} {
		if strings.Contains(summary, forbidden) {
			t.Fatalf("summary contains forbidden %q:\n%s", forbidden, summary)
		}
	}
	if got[2].ToolCallID != "call_terminal" || got[2].Name != "terminal" {
		t.Fatalf("tool identity = (%q, %q), want preserved", got[2].ToolCallID, got[2].Name)
	}
	if got[2].Metadata != nil {
		t.Fatalf("metadata = %+v, want nil", got[2].Metadata)
	}
}

func TestPruneOldToolResultsSummarizesReadFileResult(t *testing.T) {
	args := `{"path":"internal/agent/compaction.go"}`
	content := "package agent\n\n" + strings.Repeat("func x() {}\n", 220)
	messages := []provider.Message{
		{Role: "user", Content: "read"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{
			ID:       "call_read",
			Type:     "function",
			Function: provider.FunctionCall{Name: "read_file", Arguments: args},
		}}},
		{
			Role:       "tool",
			ToolCallID: "call_read",
			Name:       "read_file",
			Content:    content,
			Metadata:   map[string]any{"badge": "file"},
		},
	}
	messages = append(messages, compactionFillerMessages(PruneTurnAge)...)

	got, changed := pruneOldToolResultsWithChange(messages)
	if !changed {
		t.Fatal("pruneOldToolResultsWithChange changed = false, want true")
	}
	assertContainsAll(t, got[2].Content,
		"[Tool Result Summary]",
		"tool: read_file",
		"path: internal/agent/compaction.go",
		fmt.Sprintf("output_chars: %d", len([]rune(content))),
	)
}

func compactionFillerMessages(n int) []provider.Message {
	messages := make([]provider.Message, 0, n)
	for i := 0; i < n; i++ {
		messages = append(messages, provider.Message{
			Role:    "user",
			Content: fmt.Sprintf("filler %d", i),
		})
	}
	return messages
}

func assertContainsAll(t *testing.T, haystack string, needles ...string) {
	t.Helper()
	for _, needle := range needles {
		if !strings.Contains(haystack, needle) {
			t.Fatalf("summary missing %q:\n%s", needle, haystack)
		}
	}
}

func testLineCount(s string) int {
	if s == "" {
		return 0
	}
	lines := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		lines++
	}
	return lines
}
