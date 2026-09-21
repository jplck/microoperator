//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const (
	fixtureProviderEnv    = "MICROOPERATOR_TEST_PROVIDER_KEY"
	fixtureProviderSecret = "fixture-provider-secret-not-real"
	fixtureControlToken   = "fixture-control-token-not-real-0123456789"
)

func fixtureConfiguration(t *testing.T) configuration {
	t.Helper()
	return configuration{
		SchemaVersion: 1,
		DataDir:       filepath.Join(t.TempDir(), "state"),
		Providers: map[string]providerConfig{
			"primary": {Adapter: "openai-chat-completions", BaseURL: "https://provider.example.invalid/v1", APIKeyEnv: fixtureProviderEnv},
		},
		Models: map[string]modelConfig{
			"default": {Provider: "primary", Model: "fixture-model", QuotaGroups: []string{"account"}, MaxOutputTokens: 1024},
		},
		QuotaGroups: map[string]quotaConfig{
			"account": {RequestsPerMinute: 60, TokensPerMinute: 60000, BurstRequests: 1, MaxConcurrent: 2, QueueCapacity: 100, MaxWaitSeconds: 30},
		},
		SandboxProfiles: map[string]sandboxConfig{
			"worker": {Read: []string{"inputs"}, ReadWrite: []string{"scratch", "output"}, Network: "blocked"},
		},
		Tools: map[string]toolConfig{
			"shared.notes": {Kind: "skill", Version: 1, Description: "Fixture guidance", Content: "Use sources, not guesses.", RequiresTools: []string{}},
		},
		Systems: map[string]systemConfig{
			"research": {
				Tools:    []string{"shared.notes"},
				Operator: operatorConfig{Prompt: "Research $HOME literally.", Model: "default", Tools: []string{"shared.notes"}, SandboxProfile: "worker"},
				Limits:   systemLimits{MaxAgents: 4, MaxActiveAgents: 2, TokenBudget: 50000},
			},
		},
	}
}

func fixtureLookup(name string) (string, bool) {
	return fixtureProviderSecret, name == fixtureProviderEnv
}

func writeFixtureConfiguration(t *testing.T, filename string, cfg configuration) {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestReadmeConfiguration(t *testing.T) {
	data, err := os.ReadFile("README.md")
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
	var cfg configuration
	if err := decodeJSON([]byte(example), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(func(name string) (string, bool) {
		return fixtureProviderSecret, name == "MICROOPERATOR_LLM_KEY"
	}); err != nil {
		t.Fatalf("README configuration is not accepted: %v", err)
	}
}

func TestConfigurationValidation(t *testing.T) {
	cases := []struct {
		name   string
		change func(*configuration)
		field  string
	}{
		{"version", func(c *configuration) { c.SchemaVersion = 2 }, "schema_version"},
		{"data directory", func(c *configuration) { c.DataDir = "" }, "data_dir"},
		{"adapter", func(c *configuration) {
			p := c.Providers["primary"]
			p.Adapter = "unknown"
			c.Providers["primary"] = p
		}, "adapter"},
		{"credential in URL", func(c *configuration) {
			p := c.Providers["primary"]
			p.BaseURL = "https://user:" + fixtureProviderSecret + "@example.invalid"
			c.Providers["primary"] = p
		}, "base_url"},
		{"query in URL", func(c *configuration) {
			p := c.Providers["primary"]
			p.BaseURL += "?key=" + fixtureProviderSecret
			c.Providers["primary"] = p
		}, "base_url"},
		{"remote plaintext", func(c *configuration) {
			p := c.Providers["primary"]
			p.BaseURL = "http://example.invalid"
			c.Providers["primary"] = p
		}, "base_url"},
		{"credential unavailable", func(c *configuration) {
			p := c.Providers["primary"]
			p.APIKeyEnv = "UNAVAILABLE_FIXTURE_KEY"
			c.Providers["primary"] = p
		}, "api_key_env"},
		{"zero quota", func(c *configuration) {
			q := c.QuotaGroups["account"]
			q.RequestsPerMinute = 0
			c.QuotaGroups["account"] = q
		}, "quota_groups"},
		{"burst exceeds rate", func(c *configuration) {
			q := c.QuotaGroups["account"]
			q.BurstRequests = 61
			c.QuotaGroups["account"] = q
		}, "burst_requests"},
		{"excessive queue", func(c *configuration) {
			q := c.QuotaGroups["account"]
			q.QueueCapacity = 100001
			c.QuotaGroups["account"] = q
		}, "queue_capacity"},
		{"provider reference", func(c *configuration) {
			m := c.Models["default"]
			m.Provider = "missing"
			c.Models["default"] = m
		}, "provider"},
		{"quota reference", func(c *configuration) {
			m := c.Models["default"]
			m.QuotaGroups = []string{"missing"}
			c.Models["default"] = m
		}, "quota_groups"},
		{"duplicate quota", func(c *configuration) {
			m := c.Models["default"]
			m.QuotaGroups = []string{"account", "account"}
			c.Models["default"] = m
		}, "quota_groups"},
		{"output cannot fit", func(c *configuration) {
			m := c.Models["default"]
			m.MaxOutputTokens = 60001
			c.Models["default"] = m
		}, "quota_groups"},
		{"unsupported networking", func(c *configuration) {
			p := c.SandboxProfiles["worker"]
			p.Network = "allow-all"
			c.SandboxProfiles["worker"] = p
		}, "network"},
		{"absolute path", func(c *configuration) {
			p := c.SandboxProfiles["worker"]
			p.Read = []string{"/tmp"}
			c.SandboxProfiles["worker"] = p
		}, "read"},
		{"path traversal", func(c *configuration) {
			p := c.SandboxProfiles["worker"]
			p.Read = []string{"inputs/../scratch"}
			c.SandboxProfiles["worker"] = p
		}, "read"},
		{"writable inputs", func(c *configuration) {
			p := c.SandboxProfiles["worker"]
			p.ReadWrite = []string{"inputs"}
			c.SandboxProfiles["worker"] = p
		}, "read_write"},
		{"unimplemented executable", func(c *configuration) {
			tool := c.Tools["shared.notes"]
			tool.Kind = "executable"
			c.Tools["shared.notes"] = tool
		}, "kind"},
		{"unimplemented built-in", func(c *configuration) {
			s := c.Systems["research"]
			s.Tools = []string{"runtime.agent.propose"}
			c.Systems["research"] = s
		}, "tools"},
		{"dependency cycle", func(c *configuration) {
			tool := c.Tools["shared.notes"]
			tool.RequiresTools = []string{"shared.notes"}
			c.Tools["shared.notes"] = tool
		}, "tools"},
		{"operator escalation", func(c *configuration) {
			s := c.Systems["research"]
			s.Tools = nil
			c.Systems["research"] = s
		}, "operator.tools"},
		{"ungranted dependency", func(c *configuration) {
			c.Tools["shared.other"] = toolConfig{Kind: "skill", Version: 1, Description: "Dependency", Content: "Fixture"}
			tool := c.Tools["shared.notes"]
			tool.RequiresTools = []string{"shared.other"}
			c.Tools["shared.notes"] = tool
		}, "tools"},
		{"unknown model", func(c *configuration) {
			s := c.Systems["research"]
			s.Operator.Model = "missing"
			c.Systems["research"] = s
		}, "operator.model"},
		{"unknown profile", func(c *configuration) {
			s := c.Systems["research"]
			s.Operator.SandboxProfile = "missing"
			c.Systems["research"] = s
		}, "sandbox_profile"},
		{"empty prompt", func(c *configuration) {
			s := c.Systems["research"]
			s.Operator.Prompt = ""
			c.Systems["research"] = s
		}, "prompt"},
		{"excess activations", func(c *configuration) {
			s := c.Systems["research"]
			s.Limits.MaxActiveAgents = 5
			c.Systems["research"] = s
		}, "limits"},
		{"empty budget", func(c *configuration) {
			s := c.Systems["research"]
			s.Limits.TokenBudget = 0
			c.Systems["research"] = s
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
		loaded.Systems["research"].Operator.Prompt != cfg.Systems["research"].Operator.Prompt {
		t.Fatal("paths were not relative to configuration, or prompt was expanded")
	}
	for _, data := range []string{
		`{"schema_version":1,"schema_version":2}`,
		`{"Schema_version":1}`,
		`{"schema_version":"1"}`,
		`{"schema_version":1,"api_key":"` + fixtureProviderSecret + `"}`,
		`{"schema_version":1} {}`,
		`null`,
		strings.Repeat(" ", maxConfigBytes+1),
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
	t.Setenv(controlTokenEnv, fixtureControlToken)
	t.Setenv(fixtureProviderEnv, "")
	var stdout, stderr bytes.Buffer
	err := runDaemon(context.Background(), filename, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "api_key_env") || stdout.Len() != 0 {
		t.Fatalf("missing credential allowed readiness: err=%v stdout=%q", err, stdout.String())
	}
	if _, err := os.Stat(cfg.DataDir); !os.IsNotExist(err) {
		t.Fatalf("invalid configuration created state: %v", err)
	}
}
