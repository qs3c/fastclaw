package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
	"github.com/fastclaw-ai/fastclaw/internal/session"
)

type slashCompactContextKey struct{}

func TestSlashRequiresAdmin(t *testing.T) {
	tests := []struct {
		name     string
		cmd      string
		peerKind string
		want     bool
	}{
		{name: "new in dm", cmd: "/new", peerKind: "dm", want: false},
		{name: "reset in dm", cmd: "/reset", peerKind: "dm", want: false},
		{name: "new with legacy empty peer kind", cmd: "/new", want: false},
		{name: "new in group", cmd: "/new", peerKind: "group", want: true},
		{name: "reset in group", cmd: "/reset", peerKind: "group", want: true},
		{name: "model in dm", cmd: "/model", peerKind: "dm", want: true},
		{name: "read command", cmd: "/status", peerKind: "group", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := bus.InboundMessage{PeerKind: tt.peerKind}
			if got := slashRequiresAdmin(tt.cmd, msg); got != tt.want {
				t.Fatalf("slashRequiresAdmin(%q, peer=%q) = %v, want %v", tt.cmd, tt.peerKind, got, tt.want)
			}
		})
	}
}

func TestSlashCompactUsesManualFocus(t *testing.T) {
	sessions := session.NewManager(filepath.Join(t.TempDir(), "sessions"))
	msg := bus.InboundMessage{
		Text:      "/compact preserve filesystem decisions",
		Channel:   "web",
		ChatID:    "chat-1",
		UserID:    "owner-1",
		AccountID: "account-1",
	}
	sess := sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	for i := 0; i < 12; i++ {
		sess.Append(provider.Message{Role: "user", Content: "user turn with filesystem decision details", Origin: provider.OriginUser})
		sess.Append(provider.Message{Role: "assistant", Content: "assistant turn preserving context", Origin: provider.OriginUser})
	}
	before := len(sess.GetMessages())

	summarizer := &fakeSummarizer{}
	a := &Agent{
		provider:      summarizer,
		sessions:      sessions,
		model:         "fake-model",
		maxTokens:     4096,
		contextWindow: 32000,
		homePath:      t.TempDir(),
		ownerUserID:   "owner-1",
	}

	sentinel := "sentinel"
	ctx := context.WithValue(context.Background(), slashCompactContextKey{}, sentinel)
	result := a.handleSlashCommand(ctx, msg)

	if !result.handled {
		t.Fatal("/compact was not handled")
	}
	if !strings.Contains(result.reply, "Compacted checkpoint") {
		t.Fatalf("reply = %q, want compacted checkpoint", result.reply)
	}
	if !strings.Contains(summarizer.gotSummaryRequest, "Manual compaction focus:\npreserve filesystem decisions") {
		t.Fatalf("summary request missing manual focus:\n%s", summarizer.gotSummaryRequest)
	}
	if got := summarizer.gotCtx.Value(slashCompactContextKey{}); got != sentinel {
		t.Fatalf("summarizer context sentinel = %v, want %q", got, sentinel)
	}
	if after := len(sess.GetMessages()); after >= before {
		t.Fatalf("session message count = %d, want less than %d", after, before)
	}
}

func TestSlashModelRefreshesContextWindow(t *testing.T) {
	a := &Agent{
		model:         "openai/small-model",
		maxTokens:     4096,
		contextWindow: 32000,
		providerConfigs: map[string]config.ProviderConfig{
			"openai": {
				Models: []config.ModelEntry{
					{ID: "large-model", ContextWindow: 200000},
				},
			},
		},
	}

	result := a.slashModel(bus.InboundMessage{}, "openai/large-model")

	if !result.handled {
		t.Fatal("/model was not handled")
	}
	if a.model != "openai/large-model" {
		t.Fatalf("model = %q, want openai/large-model", a.model)
	}
	if a.contextWindow != 200000 {
		t.Fatalf("contextWindow = %d, want 200000", a.contextWindow)
	}
}
