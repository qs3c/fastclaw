package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

type countingSummarizer struct{ calls int }

func (f *countingSummarizer) Chat(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.Response, error) {
	f.calls++
	return &provider.Response{Content: "summary"}, nil
}

func (f *countingSummarizer) ChatStream(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.StreamReader, error) {
	return nil, nil
}

func TestProactiveCompactionUsesPercentOfContextWindow(t *testing.T) {
	var msgs []provider.Message
	for i := 0; i < 20; i++ {
		msgs = append(msgs,
			provider.Message{Role: "user", Content: strings.Repeat("u", 70), Origin: provider.OriginUser},
			provider.Message{Role: "assistant", Content: strings.Repeat("a", 70), Origin: provider.OriginUser},
		)
	}

	opts := CompactOptions{
		Mode:            CompactModeProactive,
		ContextWindow:   1200,
		MaxOutputTokens: 400,
		TriggerPercent:  75,
		TargetPercent:   55,
	}
	normalized := normalizeCompactOptions(opts)
	requestTokens := EstimateRequestTokens(msgs, nil)
	if requestTokens <= compactTriggerLimit(normalized) {
		t.Fatalf("fixture request tokens = %d, want above compact trigger limit %d", requestTokens, compactTriggerLimit(normalized))
	}
	if requestTokens >= percentOf(normalized.ContextWindow, normalized.TriggerPercent) {
		t.Fatalf("fixture request tokens = %d, want below raw context-window trigger %d", requestTokens, percentOf(normalized.ContextWindow, normalized.TriggerPercent))
	}

	f := &countingSummarizer{}
	res, err := CompactMessagesWithOptions(msgs, CompactOptions{
		Mode:            CompactModeProactive,
		Provider:        f,
		Model:           "fake-model",
		ContextWindow:   1200,
		MaxOutputTokens: 400,
		TriggerPercent:  75,
		TargetPercent:   55,
	})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !res.Pruned {
		t.Fatal("expected proactive compaction to prune")
	}
	if f.calls != 1 {
		t.Fatalf("summary calls = %d, want 1", f.calls)
	}
}

func TestCompactInputBudgetUsesMinimumWhenOutputConsumesWindow(t *testing.T) {
	opts := normalizeCompactOptions(CompactOptions{ContextWindow: 1000, MaxOutputTokens: 1200})
	if got := compactInputBudget(opts); got != 1 {
		t.Fatalf("compactInputBudget = %d, want 1", got)
	}
}

func TestEstimateTokensIncludesTextContentParts(t *testing.T) {
	msgs := []provider.Message{{
		Role: "user",
		ContentParts: []provider.ContentPart{{
			Type: "text",
			Text: strings.Repeat("x", 1200),
		}},
	}}
	if got := EstimateTokens(msgs); got != 300 {
		t.Fatalf("EstimateTokens = %d, want 300", got)
	}
}

func TestProactiveCompactionIncludesRequestOverheadAndToolDefs(t *testing.T) {
	var msgs []provider.Message
	for i := 0; i < 12; i++ {
		msgs = append(msgs,
			provider.Message{Role: "user", Content: strings.Repeat("u", 100), Origin: provider.OriginUser},
			provider.Message{Role: "assistant", Content: strings.Repeat("a", 100), Origin: provider.OriginUser},
		)
	}

	f := &countingSummarizer{}
	res, err := CompactMessagesWithOptions(msgs, CompactOptions{
		Mode:            CompactModeProactive,
		Provider:        f,
		Model:           "fake-model",
		ContextWindow:   4600,
		MaxOutputTokens: 600,
		TriggerPercent:  75,
		TargetPercent:   55,
		OverheadMessages: []provider.Message{{
			Role:    "system",
			Content: strings.Repeat("s", 7000),
		}},
		ToolDefs: []provider.Tool{{
			Type: "function",
			Function: provider.ToolFunction{
				Name:        "large_tool",
				Description: strings.Repeat("d", 1000),
				Parameters: map[string]any{
					"type":        "object",
					"description": strings.Repeat("p", 3000),
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if f.calls != 1 {
		t.Fatalf("summary calls = %d, want 1", f.calls)
	}
	if !res.Pruned {
		t.Fatal("expected overhead/tool defs to trigger compaction")
	}
	if len(res.Messages) >= len(msgs) {
		t.Fatalf("message count = %d, want less than %d", len(res.Messages), len(msgs))
	}
}
