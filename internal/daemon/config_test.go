//go:build linux

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/jplck/microoperator/internal/protocol"
	"github.com/jplck/microoperator/internal/state"
)

const (
	fixtureProviderEnv    = "MICROOPERATOR_TEST_PROVIDER_KEY"
	fixtureProviderSecret = "fixture-provider-secret-not-real"
	fixtureControlToken   = "fixture-control-token-not-real-0123456789"
)

func fixtureConfiguration(t *testing.T) state.Configuration {
	t.Helper()
	return state.Configuration{
		SchemaVersion: 1,
		DataDir:       filepath.Join(t.TempDir(), "state"),
		Providers: map[string]state.ProviderConfig{
			"primary": {Adapter: "openai-chat-completions", BaseURL: "https://provider.example.invalid/v1", APIKeyEnv: fixtureProviderEnv},
		},
		Models: map[string]state.ModelConfig{
			"default": {Provider: "primary", Model: "fixture-model", QuotaGroups: []string{"account"}, MaxOutputTokens: 1024},
		},
		QuotaGroups: map[string]state.QuotaConfig{
			"account": {RequestsPerMinute: 60, TokensPerMinute: 60000, BurstRequests: 1, MaxConcurrent: 2, QueueCapacity: 100, MaxWaitSeconds: 30},
		},
		SandboxProfiles: map[string]state.SandboxConfig{
			"worker": {Read: []string{"inputs"}, ReadWrite: []string{"scratch", "output"}, Network: "blocked"},
		},
		Tools: map[string]state.ToolConfig{
			"shared.notes": {Kind: "skill", Version: 1, Description: "Fixture guidance", Content: "Use sources, not guesses.", RequiresTools: []string{}},
		},
		Bootstrap: &state.SystemConfig{
			Tools:    []string{"shared.notes"},
			Operator: state.OperatorConfig{Prompt: "Research $HOME literally.", Model: "default", Tools: []string{"shared.notes"}, SandboxProfile: "worker"},
			Limits:   state.SystemLimits{MaxAgents: 4, MaxActiveAgents: 2, TokenBudget: 50000},
		},
	}
}

func fixtureGoal() *string {
	goal := "fixture goal"
	return &goal
}

func fixtureLookup(name string) (string, bool) {
	return fixtureProviderSecret, name == fixtureProviderEnv
}

func writeFixtureConfiguration(t *testing.T, filename string, cfg state.Configuration) {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSampleConfigurationLoadsWithoutProviderCredentials(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "microoperator.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	filename := filepath.Join(root, "microoperator.json")
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfiguration(filename, func(string) (string, bool) {
		t.Fatal("sample configuration requires provider credentials")
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != filepath.Join(root, "state") || len(cfg.Providers) != 1 ||
		cfg.Providers["local"].Adapter != "ollama" || cfg.Providers["local"].BaseURL != "http://127.0.0.1:11434/v1" ||
		cfg.Models["default"].Model != "qwen3.8:27b" {
		t.Fatal("sample does not use the documented local defaults")
	}
	system, err := cfg.BootstrapSystem()
	if err != nil || system.Operator.Model != "default" || len(system.Operator.Tools) < 10 ||
		system.Limits.MaxAgents != 8 || system.Limits.MaxActiveAgents != 2 || cfg.Learning != nil {
		t.Fatal("sample does not enable bounded general-purpose bootstrap")
	}
}

func TestReadmeConfiguration(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, example, found := strings.Cut(string(data), "```json\n")
	if !found {
		t.Fatal("README has no JSON configuration example")
	}
	example, _, found = strings.Cut(example, "```")
	if !found {
		t.Fatal("README JSON example is unterminated")
	}
	var cfg state.Configuration
	if err := protocol.DecodeJSON([]byte(example), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(func(name string) (string, bool) {
		return fixtureProviderSecret, name == "MICROOPERATOR_LLM_KEY"
	}); err != nil {
		t.Fatalf("README configuration is not accepted: %v", err)
	}
}

func TestReadmeOllamaAndAzureConfiguration(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(data), "### Ollama and Azure\n")
	if !found {
		t.Fatal("README has no provider setup")
	}
	_, example, found := strings.Cut(section, "```json\n")
	if !found {
		t.Fatal("README has no provider configuration example")
	}
	example, _, found = strings.Cut(example, "```")
	if !found {
		t.Fatal("README provider example is unterminated")
	}
	var providers state.Configuration
	if err := protocol.DecodeJSON([]byte(example), &providers); err != nil {
		t.Fatal(err)
	}
	cfg := fixtureConfiguration(t)
	cfg.Providers, cfg.Models, cfg.QuotaGroups = providers.Providers, providers.Models, providers.QuotaGroups
	for _, model := range []string{"default", "azure"} {
		system := *cfg.Bootstrap
		system.Operator.Model = model
		cfg.Bootstrap = &system
		if err := cfg.Validate(func(string) (string, bool) {
			t.Fatal("README keyless provider example required an API key")
			return "", false
		}); err != nil {
			t.Fatalf("README provider example for %s: %v", model, err)
		}
	}
}

func TestConfigurationValidation(t *testing.T) {
	cases := []struct {
		name   string
		change func(*state.Configuration)
		field  string
	}{
		{"version", func(c *state.Configuration) { c.SchemaVersion = 2 }, "schema_version"},
		{"data directory", func(c *state.Configuration) { c.DataDir = "" }, "data_dir"},
		{"adapter", func(c *state.Configuration) {
			p := c.Providers["primary"]
			p.Adapter = "unknown"
			c.Providers["primary"] = p
		}, "adapter"},
		{"credential in URL", func(c *state.Configuration) {
			p := c.Providers["primary"]
			p.BaseURL = "https://user:" + fixtureProviderSecret + "@example.invalid"
			c.Providers["primary"] = p
		}, "base_url"},
		{"query in URL", func(c *state.Configuration) {
			p := c.Providers["primary"]
			p.BaseURL += "?key=" + fixtureProviderSecret
			c.Providers["primary"] = p
		}, "base_url"},
		{"remote plaintext", func(c *state.Configuration) {
			p := c.Providers["primary"]
			p.BaseURL = "http://example.invalid"
			c.Providers["primary"] = p
		}, "base_url"},
		{"credential unavailable", func(c *state.Configuration) {
			p := c.Providers["primary"]
			p.APIKeyEnv = "UNAVAILABLE_FIXTURE_KEY"
			c.Providers["primary"] = p
		}, "api_key_env"},
		{"zero quota", func(c *state.Configuration) {
			q := c.QuotaGroups["account"]
			q.RequestsPerMinute = 0
			c.QuotaGroups["account"] = q
		}, "quota_groups"},
		{"burst exceeds rate", func(c *state.Configuration) {
			q := c.QuotaGroups["account"]
			q.BurstRequests = 61
			c.QuotaGroups["account"] = q
		}, "burst_requests"},
		{"excessive queue", func(c *state.Configuration) {
			q := c.QuotaGroups["account"]
			q.QueueCapacity = 100001
			c.QuotaGroups["account"] = q
		}, "queue_capacity"},
		{"provider reference", func(c *state.Configuration) {
			m := c.Models["default"]
			m.Provider = "missing"
			c.Models["default"] = m
		}, "provider"},
		{"quota reference", func(c *state.Configuration) {
			m := c.Models["default"]
			m.QuotaGroups = []string{"missing"}
			c.Models["default"] = m
		}, "quota_groups"},
		{"duplicate quota", func(c *state.Configuration) {
			m := c.Models["default"]
			m.QuotaGroups = []string{"account", "account"}
			c.Models["default"] = m
		}, "quota_groups"},
		{"output cannot fit", func(c *state.Configuration) {
			m := c.Models["default"]
			m.MaxOutputTokens = 60001
			c.Models["default"] = m
		}, "quota_groups"},
		{"unsupported networking", func(c *state.Configuration) {
			p := c.SandboxProfiles["worker"]
			p.Network = "allow-all"
			c.SandboxProfiles["worker"] = p
		}, "network"},
		{"absolute path", func(c *state.Configuration) {
			p := c.SandboxProfiles["worker"]
			p.Read = []string{"/tmp"}
			c.SandboxProfiles["worker"] = p
		}, "read"},
		{"path traversal", func(c *state.Configuration) {
			p := c.SandboxProfiles["worker"]
			p.Read = []string{"inputs/../scratch"}
			c.SandboxProfiles["worker"] = p
		}, "read"},
		{"writable inputs", func(c *state.Configuration) {
			p := c.SandboxProfiles["worker"]
			p.ReadWrite = []string{"inputs"}
			c.SandboxProfiles["worker"] = p
		}, "read_write"},
		{"unimplemented executable", func(c *state.Configuration) {
			tool := c.Tools["shared.notes"]
			tool.Kind = "executable"
			c.Tools["shared.notes"] = tool
		}, "kind"},
		{"unimplemented built-in", func(c *state.Configuration) {
			s := *c.Bootstrap
			s.Tools = []string{"runtime.agent.propose"}
			c.Bootstrap = &s
		}, "tools"},
		{"dependency cycle", func(c *state.Configuration) {
			tool := c.Tools["shared.notes"]
			tool.RequiresTools = []string{"shared.notes"}
			c.Tools["shared.notes"] = tool
		}, "tools"},
		{"operator escalation", func(c *state.Configuration) {
			s := *c.Bootstrap
			s.Tools = nil
			c.Bootstrap = &s
		}, "operator.tools"},
		{"ungranted dependency", func(c *state.Configuration) {
			c.Tools["shared.other"] = state.ToolConfig{Kind: "skill", Version: 1, Description: "Dependency", Content: "Fixture"}
			tool := c.Tools["shared.notes"]
			tool.RequiresTools = []string{"shared.other"}
			c.Tools["shared.notes"] = tool
		}, "tools"},
		{"unknown model", func(c *state.Configuration) {
			s := *c.Bootstrap
			s.Operator.Model = "missing"
			c.Bootstrap = &s
		}, "operator.model"},
		{"unknown profile", func(c *state.Configuration) {
			s := *c.Bootstrap
			s.Operator.SandboxProfile = "missing"
			c.Bootstrap = &s
		}, "sandbox_profile"},
		{"blank prompt", func(c *state.Configuration) {
			s := *c.Bootstrap
			s.Operator.Prompt = " "
			c.Bootstrap = &s
		}, "prompt"},
		{"excess activations", func(c *state.Configuration) {
			s := *c.Bootstrap
			s.Limits.MaxActiveAgents = 5
			c.Bootstrap = &s
		}, "limits"},
		{"empty budget", func(c *state.Configuration) {
			s := *c.Bootstrap
			s.Limits.TokenBudget = 0
			c.Bootstrap = &s
		}, "token_budget"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fixtureConfiguration(t)
			tc.change(&cfg)
			err := cfg.Validate(fixtureLookup)
			if err == nil || !strings.Contains(err.Error(), tc.field) || strings.Contains(err.Error(), fixtureProviderSecret) {
				t.Fatalf("validation error = %v; want safe field error for %s", err, tc.field)
			}
		})
	}
	cfg := fixtureConfiguration(t)
	if err := cfg.Validate(fixtureLookup); err != nil {
		t.Fatal(err)
	}
	p := cfg.Providers["primary"]
	p.BaseURL = "http://127.0.0.1:12345/v1"
	cfg.Providers["primary"] = p
	if err := cfg.Validate(fixtureLookup); err != nil {
		t.Fatalf("loopback fixture provider rejected: %v", err)
	}
}

func TestConfigurationJSONAndOwnership(t *testing.T) {
	cfg := fixtureConfiguration(t)
	cfg.DataDir = "state"
	filename := filepath.Join(t.TempDir(), "microoperator.json")
	writeFixtureConfiguration(t, filename, cfg)
	loaded, err := loadConfiguration(filename, fixtureLookup)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DataDir != filepath.Join(filepath.Dir(filename), "state") ||
		loaded.Bootstrap.Operator.Prompt != cfg.Bootstrap.Operator.Prompt {
		t.Fatal("paths were not relative to configuration, or prompt was expanded")
	}
	for _, data := range []string{
		`{"schema_version":1,"schema_version":2}`,
		`{"Schema_version":1}`,
		`{"schema_version":"1"}`,
		`{"schema_version":1,"api_key":"` + fixtureProviderSecret + `"}`,
		`{"schema_version":1} {}`,
		`null`,
		strings.Repeat(" ", state.MaxConfigBytes+1),
	} {
		if err := os.WriteFile(filename, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfiguration(filename, fixtureLookup); err == nil || strings.Contains(err.Error(), fixtureProviderSecret) {
			t.Fatalf("unsafe or missing validation error: %v", err)
		}
	}
	writeFixtureConfiguration(t, filename, cfg)
	link := filepath.Join(t.TempDir(), "link.json")
	if err := os.Symlink(filename, link); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfiguration(link, fixtureLookup); err == nil {
		t.Fatal("symlinked configuration accepted")
	}
	if err := os.Chmod(filename, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfiguration(filename, fixtureLookup); err == nil {
		t.Fatal("writable-by-others configuration accepted")
	}
	fifo := filepath.Join(t.TempDir(), "config.fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfiguration(fifo, fixtureLookup); err == nil {
		t.Fatal("FIFO configuration accepted")
	}
}

func TestInvalidConfigurationNeverBecomesReady(t *testing.T) {
	cfg := fixtureConfiguration(t)
	filename := filepath.Join(t.TempDir(), "config.json")
	writeFixtureConfiguration(t, filename, cfg)
	t.Setenv(protocol.ControlTokenEnv, fixtureControlToken)
	t.Setenv(fixtureProviderEnv, "")
	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), filename, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "api_key_env") || stdout.Len() != 0 {
		t.Fatalf("missing credential allowed readiness: err=%v stdout=%q", err, stdout.String())
	}
	if _, err := os.Stat(cfg.DataDir); !os.IsNotExist(err) {
		t.Fatalf("invalid configuration created state: %v", err)
	}
}
