package config

import "testing"

func TestResolveContextWindowUsesProviderPrefixedModelID(t *testing.T) {
	providers := map[string]ProviderConfig{
		"openai": {
			Models: []ModelEntry{
				{ID: "gpt-4.1", ContextWindow: 1048576, MaxTokens: 32768},
			},
		},
	}

	got := ResolveContextWindow(providers, "openai/gpt-4.1", 8192)
	if got != 1048576 {
		t.Fatalf("context window = %d, want 1048576", got)
	}
}

func TestResolveContextWindowUsesModelNameWhenIDDiffers(t *testing.T) {
	providers := map[string]ProviderConfig{
		"anthropic": {
			Models: []ModelEntry{
				{ID: "claude-sonnet-4", Name: "Claude Sonnet 4", ContextWindow: 200000},
			},
		},
	}

	got := ResolveContextWindow(providers, "Claude Sonnet 4", 8192)
	if got != 200000 {
		t.Fatalf("context window = %d, want 200000", got)
	}
}

func TestResolveContextWindowFallsBackToDefault(t *testing.T) {
	got := ResolveContextWindow(nil, "unknown/model", 8192)
	if got != DefaultContextWindow {
		t.Fatalf("context window = %d, want %d", got, DefaultContextWindow)
	}
}

func TestResolveContextWindowFallsBackToLargerMaxTokens(t *testing.T) {
	got := ResolveContextWindow(nil, "unknown/model", 160000)
	if got != 160000 {
		t.Fatalf("context window = %d, want 160000", got)
	}
}

func TestResolvedAgentRefreshModelContextWindow(t *testing.T) {
	rc := ResolvedAgent{
		Model:     "openrouter/qwen/qwen3-coder",
		MaxTokens: 12000,
		Providers: map[string]ProviderConfig{
			"openrouter": {
				Models: []ModelEntry{
					{ID: "qwen/qwen3-coder", ContextWindow: 262144},
				},
			},
		},
	}

	rc.RefreshModelContextWindow()
	if rc.ContextWindow != 262144 {
		t.Fatalf("context window = %d, want 262144", rc.ContextWindow)
	}
}
