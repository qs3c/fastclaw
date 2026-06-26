package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

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

func messagesText(messages []provider.Message) string {
	var b strings.Builder
	for _, msg := range messages {
		b.WriteString(msg.TextContent())
		b.WriteByte('\n')
	}
	return b.String()
}
