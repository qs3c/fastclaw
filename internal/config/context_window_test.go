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

func TestResolveContextWindowUsesLongestProviderPrefix(t *testing.T) {
	providers := map[string]ProviderConfig{
		"openrouter": {
			Models: []ModelEntry{
				{ID: "qwen/qwen3-coder", ContextWindow: 131072},
			},
		},
		"openrouter/qwen": {
			Models: []ModelEntry{
				{ID: "qwen3-coder", ContextWindow: 262144},
			},
		},
	}

	got := ResolveContextWindow(providers, "openrouter/qwen/qwen3-coder", 8192)
	if got != 262144 {
		t.Fatalf("context window = %d, want 262144", got)
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

func TestResolveContextWindowUsesSortedProviderOrderForAmbiguousUnprefixedModel(t *testing.T) {
	providers := map[string]ProviderConfig{
		"zeta": {
			Models: []ModelEntry{
				{ID: "shared", ContextWindow: 262144},
			},
		},
		"alpha": {
			Models: []ModelEntry{
				{ID: "shared", ContextWindow: 128000},
			},
		},
	}

	got := ResolveContextWindow(providers, "shared", 8192)
	if got != 128000 {
		t.Fatalf("context window = %d, want 128000", got)
	}
}

func TestResolveContextWindowIgnoresNonPositiveContextWindow(t *testing.T) {
	providers := map[string]ProviderConfig{
		"alpha": {
			Models: []ModelEntry{
				{ID: "shared", ContextWindow: 0},
			},
		},
		"beta": {
			Models: []ModelEntry{
				{ID: "shared", ContextWindow: 200000},
			},
		},
	}

	got := ResolveContextWindow(providers, "shared", 8192)
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

func TestResolvedAgentRefreshModelContextWindowNilSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RefreshModelContextWindow panicked: %v", r)
		}
	}()

	var rc *ResolvedAgent
	rc.RefreshModelContextWindow()
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

func TestMergedAgentConfigRefreshesContextWindow(t *testing.T) {
	origLoader := AgentFileConfigLoader
	AgentFileConfigLoader = func(string, string) (AgentFileConfig, bool) {
		return AgentFileConfig{}, false
	}
	t.Cleanup(func() {
		AgentFileConfigLoader = origLoader
	})

	cfg := Config{
		Agents: AgentsConfig{
			Defaults: AgentDefaults{
				Model:     "openai/gpt-4.1",
				MaxTokens: 12000,
			},
		},
		Providers: map[string]ProviderConfig{
			"openai": {
				Models: []ModelEntry{
					{ID: "gpt-4.1", ContextWindow: 1048576},
				},
			},
		},
	}

	resolved := cfg.MergedAgentConfig(AgentEntry{ID: "agent-1"})
	if resolved.ContextWindow != 1048576 {
		t.Fatalf("context window = %d, want 1048576", resolved.ContextWindow)
	}
}
