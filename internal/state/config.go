package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"strings"
)

const MaxConfigBytes = 1 << 20

var (
	ConfigName = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	EnvName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

type Configuration struct {
	RuntimeDigest   string                    `json:"-"`
	Learning        *LearningConfig           `json:"learning,omitempty"`
	SchemaVersion   int                       `json:"schema_version"`
	DataDir         string                    `json:"data_dir"`
	Providers       map[string]ProviderConfig `json:"providers"`
	Models          map[string]ModelConfig    `json:"models"`
	QuotaGroups     map[string]QuotaConfig    `json:"quota_groups"`
	SandboxProfiles map[string]SandboxConfig  `json:"sandbox_profiles"`
	Tools           map[string]ToolConfig     `json:"tools"`
	Systems         map[string]SystemConfig   `json:"systems"`
}

type ProviderConfig struct {
	Adapter   string `json:"adapter"`
	BaseURL   string `json:"base_url"`
	APIKeyEnv string `json:"api_key_env"`
}

type ModelConfig struct {
	Provider             string   `json:"provider"`
	Model                string   `json:"model"`
	QuotaGroups          []string `json:"quota_groups"`
	MaxOutputTokens      int64    `json:"max_output_tokens"`
	InputHeadroomPercent int64    `json:"input_headroom_percent,omitempty"`
}

type QuotaConfig struct {
	RequestsPerMinute int64 `json:"requests_per_minute"`
	TokensPerMinute   int64 `json:"tokens_per_minute"`
	BurstRequests     int64 `json:"burst_requests"`
	MaxConcurrent     int64 `json:"max_concurrent"`
	QueueCapacity     int64 `json:"queue_capacity"`
	MaxWaitSeconds    int64 `json:"max_wait_seconds"`
}

type SandboxConfig struct {
	Read         []string        `json:"read"`
	ReadWrite    []string        `json:"read_write"`
	Network      string          `json:"network"`
	Resources    *ResourceLimits `json:"resources,omitempty"`
	ResourceRoot string          `json:"-"`
	Toolchain    string          `json:"-"`
}

type ToolConfig struct {
	Kind          string   `json:"kind"`
	Version       int64    `json:"version"`
	Description   string   `json:"description"`
	Content       string   `json:"content"`
	RequiresTools []string `json:"requires_tools"`
	BinaryDigest  string   `json:"binary_digest,omitempty"`
	ProfileDigest string   `json:"profile_digest,omitempty"`
	RuntimeDigest string   `json:"runtime_digest,omitempty"`
}

type OperatorConfig struct {
	Prompt         string   `json:"prompt"`
	Model          string   `json:"model"`
	Tools          []string `json:"tools"`
	SandboxProfile string   `json:"sandbox_profile"`
}

type SystemLimits struct {
	MaxAgents       int64 `json:"max_agents"`
	MaxActiveAgents int64 `json:"max_active_agents"`
	TokenBudget     int64 `json:"token_budget"`
}

type SystemConfig struct {
	Tools    []string       `json:"tools"`
	Operator OperatorConfig `json:"operator"`
	Limits   SystemLimits   `json:"limits"`
}

type FieldError struct{ field, problem string }

func (e *FieldError) Error() string { return e.field + ": " + e.problem }

func Invalid(field, problem string) error { return &FieldError{field, problem} }

func (cfg Configuration) Validate(lookupEnv func(string) (string, bool)) error {
	if cfg.SchemaVersion != 1 {
		return Invalid("schema_version", "must be 1")
	}
	if cfg.DataDir == "" || strings.ContainsRune(cfg.DataDir, 0) {
		return Invalid("data_dir", "must be a nonempty path")
	}
	if cfg.Learning != nil {
		if err := ValidateLearningConfig(*cfg.Learning); err != nil {
			return err
		}
	}
	for name, provider := range cfg.Providers {
		if !ConfigName.MatchString(name) {
			return Invalid("providers", "invalid name")
		}
		field := "providers." + name
		if provider.Adapter != "openai-chat-completions" {
			return Invalid(field+".adapter", "unsupported adapter configuration")
		}
		endpoint, err := url.Parse(provider.BaseURL)
		if err != nil || endpoint.Hostname() == "" || endpoint.User != nil ||
			endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Opaque != "" {
			return Invalid(field+".base_url", "must be an absolute URL without credentials, query, or fragment")
		}
		loopback := endpoint.Hostname() == "localhost"
		if ip := net.ParseIP(endpoint.Hostname()); ip != nil {
			loopback = ip.IsLoopback()
		}
		if endpoint.Scheme != "https" && !(endpoint.Scheme == "http" && loopback) {
			return Invalid(field+".base_url", "requires HTTPS, except for loopback HTTP")
		}
		if !EnvName.MatchString(provider.APIKeyEnv) {
			return Invalid(field+".api_key_env", "must name an environment variable")
		}
		// Check availability only. Credential values never enter the configuration
		// object, SQLite snapshots, inspection responses, or validation errors.
		if value, ok := lookupEnv(provider.APIKeyEnv); !ok || strings.TrimSpace(value) == "" {
			return Invalid(field+".api_key_env", "required credential is unavailable")
		}
	}
	for name, quota := range cfg.QuotaGroups {
		if !ConfigName.MatchString(name) {
			return Invalid("quota_groups", "invalid name")
		}
		field := "quota_groups." + name
		for _, bound := range []struct {
			name       string
			value, max int64
		}{
			{"requests_per_minute", quota.RequestsPerMinute, 1000000},
			{"tokens_per_minute", quota.TokensPerMinute, 1000000000},
			{"burst_requests", quota.BurstRequests, quota.RequestsPerMinute},
			{"max_concurrent", quota.MaxConcurrent, 1024},
			{"queue_capacity", quota.QueueCapacity, 100000},
			{"max_wait_seconds", quota.MaxWaitSeconds, 3600},
		} {
			if bound.value < 1 || bound.value > bound.max {
				return Invalid(field+"."+bound.name, "outside supported bounds")
			}
		}
	}
	for name, model := range cfg.Models {
		if !ConfigName.MatchString(name) {
			return Invalid("models", "invalid name")
		}
		field := "models." + name
		if model.InputHeadroomPercent < 0 || model.InputHeadroomPercent > 1000 {
			return Invalid(field+".input_headroom_percent", "must be between 0 and 1000; zero selects the 20 percent default")
		}
		if _, ok := cfg.Providers[model.Provider]; !ok {
			return Invalid(field+".provider", "unknown provider")
		}
		if strings.TrimSpace(model.Model) == "" || len(model.Model) > 256 {
			return Invalid(field+".model", "must contain 1-256 bytes")
		}
		if model.MaxOutputTokens < 1 || model.MaxOutputTokens > 1000000000 {
			return Invalid(field+".max_output_tokens", "outside supported bounds")
		}
		if len(model.QuotaGroups) == 0 {
			return Invalid(field+".quota_groups", "requires at least one quota group")
		}
		if err := UniqueNames(model.QuotaGroups, field+".quota_groups"); err != nil {
			return err
		}
		for _, group := range model.QuotaGroups {
			quota, ok := cfg.QuotaGroups[group]
			if !ok || model.MaxOutputTokens > quota.TokensPerMinute {
				return Invalid(field+".quota_groups", "unknown group or output cap exceeds its token limit")
			}
		}
	}
	for name, profile := range cfg.SandboxProfiles {
		if !ConfigName.MatchString(name) {
			return Invalid("sandbox_profiles", "invalid name")
		}
		field := "sandbox_profiles." + name
		if err := ValidateSandbox(field, profile); err != nil {
			return err
		}
	}
	for name, Tool := range cfg.Tools {
		if !ConfigName.MatchString(name) || !strings.HasPrefix(name, "shared.") {
			return Invalid("tools", "administrative tool names must start with shared.")
		}
		field := "tools." + name
		if Tool.Kind != "skill" {
			return Invalid(field+".kind", "administrative executables are not enabled; use reviewed runtime tools or inert local drafts")
		}
		if Tool.Version < 1 || Tool.Version > 2147483647 {
			return Invalid(field+".version", "outside supported bounds")
		}
		if strings.TrimSpace(Tool.Description) == "" || len(Tool.Description) > 4096 ||
			strings.TrimSpace(Tool.Content) == "" || len(Tool.Content) > 32768 {
			return Invalid(field, "requires a description (up to 4096 bytes) and content (up to 32768 bytes)")
		}
		if err := UniqueNames(Tool.RequiresTools, field+".requires_tools"); err != nil {
			return err
		}
		for _, dependency := range Tool.RequiresTools {
			if _, ok := cfg.Tool(dependency); !ok {
				return Invalid(field+".requires_tools", "unknown dependency")
			}
		}
	}
	visiting, visited := make(map[string]bool), make(map[string]bool)
	var visit func(string, int) error
	visit = func(name string, depth int) error {
		if visiting[name] || depth > 32 {
			return Invalid("tools", "dependency cycle or excessive nesting")
		}
		if visited[name] {
			return nil
		}
		visiting[name] = true
		Tool, _ := cfg.Tool(name)
		for _, dependency := range Tool.RequiresTools {
			if err := visit(dependency, depth+1); err != nil {
				return err
			}
		}
		delete(visiting, name)
		visited[name] = true
		return nil
	}
	for name := range cfg.Tools {
		if err := visit(name, 0); err != nil {
			return err
		}
	}
	for name, system := range cfg.Systems {
		if !ConfigName.MatchString(name) {
			return Invalid("systems", "invalid launch-configuration name")
		}
		if err := cfg.ValidateSystem(system); err != nil {
			return fmt.Errorf("systems.%s: %w", name, err)
		}
	}
	return nil
}

func ValidateSandbox(field string, profile SandboxConfig) error {
	if limits := profile.Resources; limits != nil {
		if limits.MemoryBytes < 64<<20 || limits.MemoryBytes > 16<<30 || limits.WorkspaceBytes < 1<<20 || limits.WorkspaceBytes > 1<<30 ||
			limits.WorkspaceBytes > limits.MemoryBytes || limits.Processes < 32 || limits.Processes > 4096 || limits.CPUPercent < 1 || limits.CPUPercent > 1000 {
			return Invalid(field+".resources", "requires memory 64 MiB-16 GiB, workspace 1 MiB-1 GiB (<= memory), 32-4096 processes/threads, and 1-1000 CPU percent")
		}
	}
	if profile.Network != "blocked" {
		return Invalid(field+".network", "only blocked networking is supported")
	}
	for _, list := range []struct {
		name     string
		paths    []string
		writable bool
	}{{"read", profile.Read, false}, {"read_write", profile.ReadWrite, true}} {
		if len(list.paths) > 256 {
			return Invalid(field+"."+list.name, "too many paths")
		}
		seen := make(map[string]bool)
		for _, value := range list.paths {
			base, _, _ := strings.Cut(value, "/")
			validBase := base == "scratch" || base == "output" || (!list.writable && base == "inputs")
			if !validBase || value != path.Clean(value) || len(value) > 4096 || strings.ContainsAny(value, "\\\x00") || seen[value] {
				return Invalid(field+"."+list.name, "paths must be unique, clean, and inside permitted workspace directories")
			}
			seen[value] = true
		}
	}
	return nil
}

func UniqueNames(names []string, field string) error {
	if len(names) > 256 {
		return Invalid(field, "too many references")
	}
	seen := make(map[string]bool)
	for _, name := range names {
		if !ConfigName.MatchString(name) || seen[name] {
			return Invalid(field, "invalid or duplicate reference")
		}
		seen[name] = true
	}
	return nil
}

func (cfg Configuration) ValidateSystem(system SystemConfig) error {
	if strings.TrimSpace(system.Operator.Prompt) == "" || len(system.Operator.Prompt) > 32768 {
		return Invalid("operator.prompt", "must contain 1-32768 bytes")
	}
	if _, ok := cfg.Models[system.Operator.Model]; !ok {
		return Invalid("operator.model", "unknown model alias")
	}
	if _, ok := cfg.SandboxProfiles[system.Operator.SandboxProfile]; !ok {
		return Invalid("operator.sandbox_profile", "unknown profile")
	}
	if system.Limits.MaxAgents < 1 || system.Limits.MaxAgents > 1024 ||
		system.Limits.MaxActiveAgents < 1 || system.Limits.MaxActiveAgents > system.Limits.MaxAgents {
		return Invalid("limits", "require 1 <= max_active_agents <= max_agents <= 1024")
	}
	if system.Limits.TokenBudget < 1 {
		return Invalid("limits.token_budget", "must be positive")
	}
	if err := UniqueNames(system.Tools, "tools"); err != nil {
		return err
	}
	granted := make(map[string]bool)
	for _, name := range system.Tools {
		if _, ok := cfg.Tool(name); !ok {
			return Invalid("tools", "unknown or unimplemented tool")
		}
		granted[name] = true
	}
	if err := UniqueNames(system.Operator.Tools, "operator.tools"); err != nil {
		return err
	}
	operator := make(map[string]bool)
	for _, name := range system.Operator.Tools {
		if !granted[name] {
			return Invalid("operator.tools", "must be a subset of system tools")
		}
		operator[name] = true
	}
	for _, grants := range []map[string]bool{granted, operator} {
		for name := range grants {
			Tool, _ := cfg.Tool(name)
			for _, dependency := range Tool.RequiresTools {
				if !grants[dependency] {
					return Invalid("tools", "skill dependencies must already be granted at each scope")
				}
			}
		}
	}
	return nil
}

func JsonDigest(value any) (string, []byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", nil, err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), data, nil
}
