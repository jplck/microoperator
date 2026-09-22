package state

import (
	"path/filepath"
	"testing"
)

func fixtureConfiguration(t *testing.T) Configuration {
	t.Helper()
	return Configuration{
		SchemaVersion: 1,
		DataDir:       filepath.Join(t.TempDir(), "state"),
		Providers: map[string]ProviderConfig{
			"primary": {"openai-chat-completions", "https://provider.example.invalid/v1", fixtureProviderEnv},
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
		Systems: map[string]SystemConfig{
			"research": {
				Tools:    []string{"shared.notes"},
				Operator: OperatorConfig{"Research $HOME literally.", "default", []string{"shared.notes"}, "worker"},
				Limits:   SystemLimits{4, 2, 50000},
			},
		},
	}
}

func fixtureLookup(name string) (string, bool) {
	return fixtureProviderSecret, name == fixtureProviderEnv
}
