package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

const (
	maxToolArgValueRunes        = 220
	toolSummarySnippetLines     = 3
	toolSummarySnippetLineRunes = 240
)

type toolArg struct {
	Key   string
	Value string
}

type toolCallInfo struct {
	Name string
	Args []toolArg
}

var toolExitCodePattern = regexp.MustCompile(`(?im)(?:^exit code:\s*|exit[_ ]status\s+|\[status\]\s+exited\s+\(code=)(-?\d+)`)

func sanitizeToolPairs(messages []provider.Message) []provider.Message {
	sanitized, _ := sanitizeToolPairsWithChange(messages)
	return sanitized
}

func sanitizeToolPairsWithChange(messages []provider.Message) ([]provider.Message, bool) {
	out := make([]provider.Message, 0, len(messages))
	changed := false

	for i := 0; i < len(messages); {
		msg := messages[i]
		if msg.Role == "tool" {
			changed = true
			i++
			continue
		}

		if msg.Role != "assistant" {
			out = append(out, msg)
			i++
			continue
		}

		expected := expectedToolCallIDs(msg)
		if len(expected) == 0 {
			out = append(out, msg)
			i++
			continue
		}

		j := i + 1
		for j < len(messages) && messages[j].Role == "tool" {
			j++
		}

		seen := make(map[string]struct{}, len(expected))
		tools := make([]provider.Message, 0, len(expected))
		for k := i + 1; k < j; k++ {
			tool := messages[k]
			if _, ok := expected[tool.ToolCallID]; !ok {
				changed = true
				continue
			}
			if _, ok := seen[tool.ToolCallID]; ok {
				changed = true
				continue
			}
			seen[tool.ToolCallID] = struct{}{}
			tools = append(tools, tool)
		}

		complete := len(seen) == len(expected)
		if complete {
			out = append(out, msg)
			out = append(out, tools...)
			i = j
			continue
		}

		changed = true
		if assistantHasText(msg) {
			msg.ToolCalls = nil
			msg.RawAssistant = nil
			out = append(out, msg)
		}
		i = j
	}

	if !changed {
		return messages, false
	}
	return out, true
}

func assistantHasText(msg provider.Message) bool {
	return msg.Content != "" || len(msg.ContentParts) > 0 || msg.Thinking != ""
}

func expectedToolCallIDs(msg provider.Message) map[string]struct{} {
	ids := make(map[string]struct{}, len(msg.ToolCalls))
	for _, tc := range msg.ToolCalls {
		if tc.ID != "" {
			ids[tc.ID] = struct{}{}
		}
	}
	for _, id := range rawAssistantToolCallIDs(msg.RawAssistant) {
		ids[id] = struct{}{}
	}
	return ids
}

func rawAssistantToolCallIDs(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var msg struct {
		ToolCalls []struct {
			ID string `json:"id"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil
	}
	ids := make([]string, 0, len(msg.ToolCalls))
	for _, tc := range msg.ToolCalls {
		if tc.ID != "" {
			ids = append(ids, tc.ID)
		}
	}
	return ids
}

func buildToolCallLookup(messages []provider.Message) map[string]toolCallInfo {
	lookup := make(map[string]toolCallInfo)
	for _, msg := range messages {
		if msg.Role != "assistant" {
			continue
		}
		for _, tc := range msg.ToolCalls {
			addToolCallInfo(lookup, tc.ID, tc.Function.Name, tc.Function.Arguments)
		}
		for _, tc := range rawAssistantToolCallInfos(msg.RawAssistant) {
			if _, ok := lookup[tc.ID]; ok {
				continue
			}
			addToolCallInfo(lookup, tc.ID, tc.Function.Name, tc.Function.Arguments)
		}
	}
	return lookup
}

func buildToolResultInfoByIndex(messages []provider.Message) map[int]toolCallInfo {
	byIndex := make(map[int]toolCallInfo)
	for i := 0; i < len(messages); i++ {
		if messages[i].Role != "assistant" {
			continue
		}
		group := toolCallInfosByID(messages[i])
		if len(group) == 0 {
			continue
		}
		for j := i + 1; j < len(messages) && messages[j].Role == "tool"; j++ {
			if info, ok := group[messages[j].ToolCallID]; ok {
				byIndex[j] = info
			}
		}
	}
	return byIndex
}

func toolCallInfosByID(msg provider.Message) map[string]toolCallInfo {
	infos := make(map[string]toolCallInfo)
	for _, tc := range msg.ToolCalls {
		addToolCallInfo(infos, tc.ID, tc.Function.Name, tc.Function.Arguments)
	}
	for _, tc := range rawAssistantToolCallInfos(msg.RawAssistant) {
		if _, ok := infos[tc.ID]; ok {
			continue
		}
		addToolCallInfo(infos, tc.ID, tc.Function.Name, tc.Function.Arguments)
	}
	return infos
}

func addToolCallInfo(lookup map[string]toolCallInfo, id, name, rawArgs string) {
	if id == "" {
		return
	}
	lookup[id] = toolCallInfo{
		Name: name,
		Args: parseToolArgs(rawArgs),
	}
}

func rawAssistantToolCallInfos(raw json.RawMessage) []provider.ToolCall {
	if len(raw) == 0 {
		return nil
	}
	var msg struct {
		ToolCalls []provider.ToolCall `json:"tool_calls"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil
	}
	return msg.ToolCalls
}

func summarizeToolResult(msg provider.Message, lookup map[string]toolCallInfo) provider.Message {
	return summarizeToolResultWithInfo(msg, lookup[msg.ToolCallID])
}

func summarizeToolResultWithInfo(msg provider.Message, info toolCallInfo) provider.Message {
	if info.Name == "" {
		info.Name = msg.Name
	}
	msg.Content = formatToolSummary(info, msg.Content)
	msg.Metadata = nil
	return msg
}

func parseToolArgs(raw string) []toolArg {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return nil
	}

	var args []toolArg
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return args
		}
		key, ok := keyTok.(string)
		if !ok {
			return args
		}
		var value any
		if err := dec.Decode(&value); err != nil {
			return args
		}
		args = append(args, toolArg{Key: key, Value: formatToolArgValue(value)})
	}
	return args
}

func formatToolArgValue(value any) string {
	switch v := value.(type) {
	case string:
		return summarizeToolArgValue(v)
	case json.Number:
		return truncateToolArgValue(v.String(), utf8.RuneCountInString(v.String()))
	case bool:
		if v {
			return truncateToolArgValue("true", 4)
		}
		return truncateToolArgValue("false", 5)
	case nil:
		return truncateToolArgValue("null", 4)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return summarizeToolArgValue(fmt.Sprint(v))
		}
		return summarizeToolArgValue(string(b))
	}
}

func summarizeToolArgValue(s string) string {
	s = singleLineToolValue(s)
	return truncateToolArgValue(s, utf8.RuneCountInString(s))
}

func truncateToolArgValue(s string, originalRunes int) string {
	if originalRunes <= maxToolArgValueRunes {
		return s
	}
	runes := []rune(s)
	if len(runes) > maxToolArgValueRunes {
		runes = runes[:maxToolArgValueRunes]
	}
	return fmt.Sprintf("%s [truncated, chars=%d]", string(runes), originalRunes)
}

func formatToolSummary(info toolCallInfo, content string) string {
	toolName := info.Name
	if toolName == "" {
		toolName = "unknown"
	}

	var lines []string
	lines = append(lines, "[Tool Result Summary]")
	lines = append(lines, "tool: "+toolName)

	switch toolKind(toolName) {
	case "command":
		if command, ok := firstToolArgValue(info.Args, "command", "cmd", "script"); ok {
			lines = append(lines, "command: "+command)
		}
		if code, ok := extractToolExitCode(content); ok {
			lines = append(lines, "exit_code: "+code)
		}
		lines = appendToolOutputStats(lines, content, true)
	case "read":
		if path, ok := firstToolArgValue(info.Args, "path", "file", "filename"); ok {
			lines = append(lines, "path: "+path)
		}
		lines = appendToolOutputStats(lines, content, false)
	case "search":
		if query, ok := firstToolArgValue(info.Args, "query", "pattern", "q"); ok {
			lines = append(lines, "query: "+query)
		}
		if path, ok := firstToolArgValue(info.Args, "path", "dir", "directory"); ok {
			lines = append(lines, "path: "+path)
		}
		lines = appendToolOutputStats(lines, content, true)
	case "web":
		if query, ok := firstToolArgValue(info.Args, "query", "url"); ok {
			if looksLikeURL(query) {
				lines = append(lines, "url: "+query)
			} else {
				lines = append(lines, "query: "+query)
			}
		}
		lines = appendToolOutputStats(lines, content, false)
	default:
		for i, arg := range info.Args {
			if i >= 2 {
				break
			}
			lines = append(lines, "arg."+arg.Key+": "+arg.Value)
		}
		lines = appendToolOutputStats(lines, content, true)
	}
	lines = appendToolOutputSnippets(lines, content)

	return strings.Join(lines, "\n")
}

func toolKind(name string) string {
	name = strings.ToLower(strings.ReplaceAll(name, "-", "_"))
	switch {
	case strings.Contains(name, "terminal") || strings.Contains(name, "shell") || strings.Contains(name, "command") || name == "exec" || name == "host_exec":
		return "command"
	case name == "read" || strings.Contains(name, "read_file"):
		return "read"
	case strings.Contains(name, "web_search") || strings.Contains(name, "web_fetch"):
		return "web"
	case strings.Contains(name, "search") || strings.Contains(name, "grep"):
		return "search"
	default:
		return "generic"
	}
}

func firstToolArgValue(args []toolArg, keys ...string) (string, bool) {
	for _, key := range keys {
		for _, arg := range args {
			if strings.EqualFold(arg.Key, key) && arg.Value != "" {
				return arg.Value, true
			}
		}
	}
	return "", false
}

func extractToolExitCode(content string) (string, bool) {
	matches := toolExitCodePattern.FindStringSubmatch(content)
	if len(matches) < 2 {
		return "", false
	}
	return matches[1], true
}

func appendToolOutputStats(lines []string, content string, includeLines bool) []string {
	if includeLines {
		lines = append(lines, fmt.Sprintf("output_lines: %d", toolOutputLineCount(content)))
	}
	return append(lines, fmt.Sprintf("output_chars: %d", utf8.RuneCountInString(content)))
}

func appendToolOutputSnippets(lines []string, content string) []string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	content = strings.TrimSuffix(content, "\n")
	if content == "" {
		return lines
	}
	outputLines := strings.Split(content, "\n")
	headCount := minInt(toolSummarySnippetLines, len(outputLines))
	lines = append(lines, "output_head:")
	for _, line := range outputLines[:headCount] {
		lines = append(lines, "  "+summarizeToolOutputLine(line))
	}

	tailCount := minInt(toolSummarySnippetLines, len(outputLines))
	tailStart := len(outputLines) - tailCount
	if tailStart < headCount {
		tailStart = headCount
	}
	if tailStart < len(outputLines) {
		lines = append(lines, "output_tail:")
		for _, line := range outputLines[tailStart:] {
			lines = append(lines, "  "+summarizeToolOutputLine(line))
		}
	}
	return lines
}

func summarizeToolOutputLine(line string) string {
	line = strings.TrimRight(line, " \t")
	if line == "" {
		return "<blank>"
	}
	runes := []rune(line)
	if len(runes) <= toolSummarySnippetLineRunes {
		return line
	}
	return fmt.Sprintf("%s [truncated, chars=%d]", string(runes[:toolSummarySnippetLineRunes]), len(runes))
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func toolOutputLineCount(content string) int {
	if content == "" {
		return 0
	}
	lines := strings.Count(content, "\n")
	if !strings.HasSuffix(content, "\n") {
		lines++
	}
	return lines
}

func looksLikeURL(s string) bool {
	lower := strings.ToLower(s)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

func singleLineToolValue(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	s = strings.ReplaceAll(s, "\n", "\\n")
	return s
}
