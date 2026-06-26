package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/agent/tools"
	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
	"github.com/fastclaw-ai/fastclaw/internal/session"
)

type flakySummarizer struct {
	failuresBeforeSuccess int
	calls                 int
}

func (f *flakySummarizer) Chat(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.Response, error) {
	f.calls++
	if f.calls <= f.failuresBeforeSuccess {
		return nil, errors.New("summary failed")
	}
	return &provider.Response{Content: "llm summary"}, nil
}

func (f *flakySummarizer) ChatStream(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.StreamReader, error) {
	return nil, nil
}

type forcedFinalRetryProvider struct {
	streamCalls int
	requests    [][]provider.Message
	tools       [][]provider.Tool
}

func (f *forcedFinalRetryProvider) Chat(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.Response, error) {
	return &provider.Response{Content: "llm summary"}, nil
}

func (f *forcedFinalRetryProvider) ChatStream(_ context.Context, messages []provider.Message, tools []provider.Tool, _ string, _ int, _ float64) (*provider.StreamReader, error) {
	f.streamCalls++
	f.requests = append(f.requests, append([]provider.Message(nil), messages...))
	f.tools = append(f.tools, append([]provider.Tool(nil), tools...))
	if f.streamCalls == 1 {
		return nil, errors.New("too many tokens")
	}
	ch := make(chan provider.StreamChunk, 1)
	ch <- provider.StreamChunk{Content: "FORCED_FINAL_OK", Done: true}
	close(ch)
	return provider.NewStreamReader(ch), nil
}

func TestIsContextLimitError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "context_length_exceeded", err: errors.New("context_length_exceeded"), want: true},
		{name: "maximum context length", err: errors.New("maximum context length is 128000 tokens"), want: true},
		{name: "prompt too long", err: errors.New("prompt too long"), want: true},
		{name: "prompt is too long", err: errors.New("prompt is too long"), want: true},
		{name: "too many tokens", err: errors.New("too many tokens"), want: true},
		{name: "too many tokens in request", err: errors.New("too many tokens in request"), want: true},
		{name: "input length exceeds context window", err: errors.New("input length exceeds context window"), want: true},
		{name: "request too large", err: errors.New("request too large"), want: true},
		{name: "rate limit exceeded", err: errors.New("rate limit exceeded"), want: false},
		{name: "rate limit tokens per minute", err: errors.New("rate limit: too many tokens per minute"), want: false},
		{name: "quota exceeded", err: errors.New("quota exceeded"), want: false},
		{name: "throttle", err: errors.New("request throttle exceeded"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isContextLimitError(tt.err); got != tt.want {
				t.Fatalf("isContextLimitError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestForcedFinalDeliveryUsesEmergencyRetryAndKeepsCapNudgeRequestOnly(t *testing.T) {
	home := t.TempDir()
	sessions := session.NewManager(t.TempDir())
	msg := bus.InboundMessage{
		Text:      "new request",
		Channel:   "web",
		AccountID: "acct-1",
		ChatID:    "chat-1",
		UserID:    "owner-1",
	}
	sess := sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	for i := 0; i < 12; i++ {
		sess.Append(provider.Message{Role: "user", Content: strings.Repeat("old user ", 20), Origin: provider.OriginUser})
		sess.Append(provider.Message{Role: "assistant", Content: strings.Repeat("old assistant ", 20), Origin: provider.OriginUser})
	}

	prov := &forcedFinalRetryProvider{}
	mem := NewMemory(home)
	a := &Agent{
		name:              "agent-test",
		provider:          prov,
		registry:          tools.NewRegistry(home, home),
		sessions:          sessions,
		memory:            mem,
		ctxBuilder:        NewContextBuilder(home, mem, ""),
		hooks:             NewHookRegistry(),
		model:             "fake-model",
		maxTokens:         20,
		maxToolIterations: 0,
		contextWindow:     120,
		homePath:          home,
		homeDir:           home,
		ownerUserID:       "owner-1",
	}

	reply := a.HandleMessage(context.Background(), msg)

	if !strings.Contains(reply, "FORCED_FINAL_OK") {
		t.Fatalf("reply = %q, want forced final retry content", reply)
	}
	if prov.streamCalls != 2 {
		t.Fatalf("streamCalls = %d, want 2", prov.streamCalls)
	}
	if len(prov.tools) != 2 || len(prov.tools[0]) != 0 || len(prov.tools[1]) != 0 {
		t.Fatalf("forced final tools = %+v, want no tools on both attempts", prov.tools)
	}
	retryText := messagesText(prov.requests[1])
	if !strings.Contains(retryText, "[Reactive Context Summary]") {
		t.Fatalf("retry request missing reactive summary:\n%s", retryText)
	}
	if !strings.Contains(retryText, "You've used all 0 tool-call iterations") {
		t.Fatalf("retry request missing cap nudge:\n%s", retryText)
	}
	if strings.Contains(messagesText(sess.GetMessages()), "You've used all 0 tool-call iterations") {
		t.Fatalf("session messages included request-only cap nudge:\n%s", messagesText(sess.GetMessages()))
	}
}

func TestSummaryRetriesThenSucceeds(t *testing.T) {
	f := &flakySummarizer{failuresBeforeSuccess: 2}
	out, err := compressOlderMessages(longConversation(), CompactOptions{Provider: f, Model: "fake-model"})
	if err != nil {
		t.Fatalf("compressOlderMessages: %v", err)
	}
	if f.calls != 3 {
		t.Fatalf("summary calls = %d, want 3", f.calls)
	}
	if len(out) == 0 || !strings.Contains(out[0].Content, "llm summary") {
		t.Fatalf("first output message missing LLM summary: %+v", out)
	}
}

func TestSummaryFallsBackAfterThreeFailures(t *testing.T) {
	f := &flakySummarizer{failuresBeforeSuccess: 3}
	out, err := compressOlderMessages(longConversation(), CompactOptions{Provider: f, Model: "fake-model"})
	if err != nil {
		t.Fatalf("compressOlderMessages: %v", err)
	}
	if f.calls != 3 {
		t.Fatalf("summary calls = %d, want 3", f.calls)
	}
	if len(out) == 0 || !strings.Contains(out[0].Content, "deterministic fallback") {
		t.Fatalf("first output message missing deterministic fallback: %+v", out)
	}
	if strings.Contains(out[0].Content, "RUNTIME_GOAL_CONTEXT_SHOULD_NOT_APPEAR") {
		t.Fatalf("fallback included runtime goal context: %s", out[0].Content)
	}
}

func TestEmergencyRetryRetriesWithinSameIteration(t *testing.T) {
	mgr := session.NewManager(t.TempDir())
	sess := mgr.Get("web", "", "chat", "")
	sess.Append(provider.Message{Role: "user", Content: strings.Repeat("old user ", 40), Origin: provider.OriginUser})
	sess.Append(provider.Message{Role: "assistant", Content: strings.Repeat("old assistant ", 40), Origin: provider.OriginUser})
	sess.Append(provider.Message{Role: "user", Content: "KEEP_RECENT_USER_TURN", Origin: provider.OriginUser})

	a := &Agent{
		homePath:      t.TempDir(),
		model:         "fake-model",
		contextWindow: 120,
		maxTokens:     20,
	}
	overhead := []provider.Message{{Role: "system", Content: strings.Repeat("overhead ", 5)}}
	if got := a.compactionOptions(CompactModeEmergency, overhead, nil, sess.SessionKey()).MinTailTurns; got != MinimumTailTurns {
		t.Fatalf("emergency compaction MinTailTurns = %d, want %d", got, MinimumTailTurns)
	}
	messages := compactionRequestMessages(sess.GetMessages(), overhead)

	attempts := 0
	resp, rebuilt, retried, err := a.callLLMWithEmergencyRetry(
		context.Background(),
		sess,
		overhead,
		nil,
		messages,
		nil,
		false,
		nil,
		nil,
		func(request []provider.Message, tools []provider.Tool) (*provider.Response, error) {
			attempts++
			if attempts == 1 {
				return nil, errors.New("too many tokens")
			}
			text := messagesText(request)
			if !strings.Contains(text, "[Reactive Context Summary]") {
				t.Fatalf("retry request missing reactive summary:\n%s", text)
			}
			if !strings.Contains(text, "KEEP_RECENT_USER_TURN") {
				t.Fatalf("retry request missing recent user turn:\n%s", text)
			}
			return &provider.Response{Content: "ok"}, nil
		},
	)
	if err != nil {
		t.Fatalf("callLLMWithEmergencyRetry: %v", err)
	}
	if resp == nil || resp.Content != "ok" {
		t.Fatalf("response = %+v, want ok", resp)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if !retried {
		t.Fatal("retried = false, want true")
	}
	if !strings.Contains(messagesText(rebuilt), "[Reactive Context Summary]") {
		t.Fatalf("rebuilt canonical messages missing reactive summary:\n%s", messagesText(rebuilt))
	}
}

func TestEmergencyRetryPreparesRetryRequest(t *testing.T) {
	mgr := session.NewManager(t.TempDir())
	sess := mgr.Get("web", "", "chat", "")
	sess.Append(provider.Message{Role: "user", Content: strings.Repeat("old SECRET_TOKEN user ", 40), Origin: provider.OriginUser})
	sess.Append(provider.Message{Role: "assistant", Content: strings.Repeat("old SECRET_TOKEN assistant ", 40), Origin: provider.OriginUser})
	sess.Append(provider.Message{Role: "user", Content: "KEEP_RECENT_SECRET_TOKEN_USER_TURN", Origin: provider.OriginUser})

	a := &Agent{
		homePath:      t.TempDir(),
		model:         "fake-model",
		contextWindow: 120,
		maxTokens:     20,
	}
	overhead := []provider.Message{{Role: "system", Content: strings.Repeat("overhead ", 5)}}
	messages := compactionRequestMessages(sess.GetMessages(), overhead)

	attempts := 0
	var sentTexts []string
	resp, rebuilt, retried, err := a.callLLMWithEmergencyRetry(
		context.Background(),
		sess,
		overhead,
		nil,
		messages,
		nil,
		false,
		nil,
		redactSecretToken,
		func(request []provider.Message, tools []provider.Tool) (*provider.Response, error) {
			attempts++
			text := messagesText(request)
			sentTexts = append(sentTexts, text)
			if strings.Contains(text, "SECRET_TOKEN") {
				t.Fatalf("attempt %d sent raw secret:\n%s", attempts, text)
			}
			if !strings.Contains(text, "[REDACTED]") {
				t.Fatalf("attempt %d missing redaction:\n%s", attempts, text)
			}
			if attempts == 1 {
				return nil, errors.New("too many tokens")
			}
			if !strings.Contains(text, "[Reactive Context Summary]") {
				t.Fatalf("retry request missing reactive summary:\n%s", text)
			}
			return &provider.Response{Content: "ok"}, nil
		},
	)
	if err != nil {
		t.Fatalf("callLLMWithEmergencyRetry: %v", err)
	}
	if resp == nil || resp.Content != "ok" {
		t.Fatalf("response = %+v, want ok", resp)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if !retried {
		t.Fatal("retried = false, want true")
	}
	if len(sentTexts) != 2 {
		t.Fatalf("sentTexts = %d, want 2", len(sentTexts))
	}
	if !strings.Contains(messagesText(rebuilt), "SECRET_TOKEN") {
		t.Fatalf("rebuilt canonical messages missing raw secret:\n%s", messagesText(rebuilt))
	}
}

func TestEmergencyRetryPreservesRequestOnlySuffix(t *testing.T) {
	mgr := session.NewManager(t.TempDir())
	sess := mgr.Get("web", "", "chat", "")
	sess.Append(provider.Message{Role: "user", Content: strings.Repeat("old user ", 40), Origin: provider.OriginUser})
	sess.Append(provider.Message{Role: "assistant", Content: strings.Repeat("old assistant ", 40), Origin: provider.OriginUser})
	sess.Append(provider.Message{Role: "user", Content: "KEEP_RECENT_USER_TURN", Origin: provider.OriginUser})

	a := &Agent{
		homePath:      t.TempDir(),
		model:         "fake-model",
		contextWindow: 120,
		maxTokens:     20,
	}
	overhead := []provider.Message{{Role: "system", Content: strings.Repeat("overhead ", 5)}}
	buildRequest := func(sessionMessages []provider.Message) []provider.Message {
		return compactionRequestMessages(timestampUserMessages(sessionMessages), overhead)
	}
	base := buildRequest(sess.GetMessages())
	messages := append(append([]provider.Message(nil), base...), provider.Message{
		Role:    "system",
		Content: "TRANSIENT_NO_TOOLS_NUDGE",
	})

	attempts := 0
	resp, rebuilt, retried, err := a.callLLMWithEmergencyRetry(
		context.Background(),
		sess,
		overhead,
		nil,
		messages,
		nil,
		false,
		buildRequest,
		nil,
		func(request []provider.Message, tools []provider.Tool) (*provider.Response, error) {
			attempts++
			if attempts == 1 {
				return nil, errors.New("too many tokens")
			}
			text := messagesText(request)
			if !strings.Contains(text, "[Reactive Context Summary]") {
				t.Fatalf("retry request missing reactive summary:\n%s", text)
			}
			if !strings.Contains(text, "KEEP_RECENT_USER_TURN") {
				t.Fatalf("retry request missing recent user turn:\n%s", text)
			}
			if !strings.Contains(text, "TRANSIENT_NO_TOOLS_NUDGE") {
				t.Fatalf("retry request missing request-only suffix:\n%s", text)
			}
			return &provider.Response{Content: "ok"}, nil
		},
	)
	if err != nil {
		t.Fatalf("callLLMWithEmergencyRetry: %v", err)
	}
	if resp == nil || resp.Content != "ok" {
		t.Fatalf("response = %+v, want ok", resp)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if !retried {
		t.Fatal("retried = false, want true")
	}
	if strings.Contains(messagesText(rebuilt), "TRANSIENT_NO_TOOLS_NUDGE") {
		t.Fatalf("rebuilt canonical messages included request-only suffix:\n%s", messagesText(rebuilt))
	}
}

func longConversation() []provider.Message {
	var msgs []provider.Message
	for i := 0; i < 12; i++ {
		msgs = append(msgs,
			provider.Message{Role: "user", Content: strings.Repeat("real user content ", 8), Origin: provider.OriginUser},
			provider.Message{Role: "user", Content: "RUNTIME_GOAL_CONTEXT_SHOULD_NOT_APPEAR", Origin: provider.OriginGoalContext},
			provider.Message{Role: "assistant", Content: strings.Repeat("assistant reply ", 8), Origin: provider.OriginUser},
		)
	}
	return msgs
}

func redactSecretToken(messages []provider.Message) []provider.Message {
	out := make([]provider.Message, len(messages))
	copy(out, messages)
	for i := range out {
		out[i].Content = strings.ReplaceAll(out[i].Content, "SECRET_TOKEN", "[REDACTED]")
	}
	return out
}

func timestampUserMessages(messages []provider.Message) []provider.Message {
	out := make([]provider.Message, len(messages))
	copy(out, messages)
	for i := range out {
		if out[i].Role == "user" {
			out[i].Content = "TIMESTAMPED: " + out[i].Content
		}
	}
	return out
}

func messagesText(messages []provider.Message) string {
	var b strings.Builder
	for _, msg := range messages {
		b.WriteString(msg.TextContent())
		b.WriteByte('\n')
	}
	return b.String()
}
