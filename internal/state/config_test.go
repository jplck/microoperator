package state

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixtureConfiguration(t *testing.T) Configuration {
	t.Helper()
	return Configuration{
		SchemaVersion: 1,
		DataDir:       filepath.Join(t.TempDir(), "state"),
		Providers: map[string]ProviderConfig{
			"primary": {Adapter: "openai-chat-completions", BaseURL: "https://provider.example.invalid/v1", APIKeyEnv: fixtureProviderEnv},
		},
		Models: map[string]ModelConfig{
			"default": {"primary", "fixture-model", []string{"account"}, 1024, 0},
		},
		QuotaGroups: map[string]QuotaConfig{
			"account": {60, 60000, 1, 2, 100, 30},
		},
		SandboxProfiles: map[string]SandboxConfig{
			"worker": {Read: []string{"inputs"}, ReadWrite: []string{"scratch", "output"}, Network: "blocked"},
		},
		Tools: map[string]ToolConfig{
			"shared.notes": {Kind: "skill", Version: 1, Description: "Fixture guidance", Content: "Use sources, not guesses.", RequiresTools: []string{}},
		},
		Bootstrap: &SystemConfig{
			Tools:    []string{"shared.notes"},
			Operator: OperatorConfig{"Research $HOME literally.", "default", []string{"shared.notes"}, "worker"},
			Limits:   SystemLimits{4, 2, 50000},
		},
	}
}

func TestProviderTimeoutConfiguration(t *testing.T) {
	for _, seconds := range []int64{-1, 0, 1, 600, 3600, 3601} {
		cfg := fixtureConfiguration(t)
		provider := cfg.Providers["primary"]
		provider.TimeoutSeconds = seconds
		cfg.Providers["primary"] = provider
		err := cfg.Validate(fixtureLookup)
		if seconds < 0 || seconds > 3600 {
			if err == nil || !strings.Contains(err.Error(), "providers.primary.timeout_seconds") {
				t.Fatalf("timeout %d: expected validation error, got %v", seconds, err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		want := time.Duration(seconds) * time.Second
		if seconds == 0 {
			want = 60 * time.Second
		}
		if got := provider.RequestTimeout(); got != want {
			t.Fatalf("timeout %d: got %s, want %s", seconds, got, want)
		}
	}
}

func fixtureGoal() *string {
	goal := "fixture goal"
	return &goal
}

func fixtureLookup(name string) (string, bool) {
	return fixtureProviderSecret, name == fixtureProviderEnv
}

func TestProviderConfigurationModes(t *testing.T) {
	for _, tc := range []struct {
		name, adapter, endpoint, key, field string
	}{
		{"existing API key", "openai-chat-completions", "https://provider.example.invalid/v1", fixtureProviderEnv, ""},
		{"API key required", "openai-chat-completions", "https://provider.example.invalid/v1", "", "api_key_env"},
		{"Ollama IPv4", "ollama", "http://127.0.0.1:11434/v1", "", ""},
		{"Ollama IPv6", "ollama", "http://[::1]:11434/v1/", "", ""},
		{"Ollama localhost", "ollama", "http://localhost:11434/v1", "", ""},
		{"Ollama remote", "ollama", "https://example.invalid/v1", "", "base_url"},
		{"Ollama native API", "ollama", "http://127.0.0.1:11434/api", "", "base_url"},
		{"Ollama key rejected", "ollama", "http://127.0.0.1:11434/v1", fixtureProviderEnv, "api_key_env"},
		{"Azure OpenAI", "azure-openai", "https://resource.openai.azure.com/openai/v1/", "", ""},
		{"Azure Foundry", "azure-openai", "https://resource.services.ai.azure.com/openai/v1", "", ""},
		{"Azure plaintext", "azure-openai", "http://127.0.0.1/openai/v1", "", "base_url"},
		{"Azure missing v1", "azure-openai", "https://resource.openai.azure.com", "", "base_url"},
		{"Azure legacy API", "azure-openai", "https://resource.openai.azure.com/openai/deployments/model?api-version=2024-10-21", "", "base_url"},
		{"Azure key rejected", "azure-openai", "https://resource.openai.azure.com/openai/v1", fixtureProviderEnv, "api_key_env"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fixtureConfiguration(t)
			cfg.Providers["primary"] = ProviderConfig{Adapter: tc.adapter, BaseURL: tc.endpoint, APIKeyEnv: tc.key}
			err := cfg.Validate(func(name string) (string, bool) {
				if tc.adapter != "openai-chat-completions" {
					t.Fatal("keyless provider looked up an API key")
				}
				return fixtureLookup(name)
			})
			if tc.field == "" && err != nil || tc.field != "" && (err == nil || !strings.Contains(err.Error(), tc.field)) {
				t.Fatalf("validation error = %v; want field %q", err, tc.field)
			}
		})
	}
}
