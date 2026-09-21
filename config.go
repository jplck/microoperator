//go:build darwin || linux

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

const maxConfigBytes = 1 << 20

var (
	configName = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	envName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

type configuration struct {
	SchemaVersion   int                       `json:"schema_version"`
	DataDir         string                    `json:"data_dir"`
	Providers       map[string]providerConfig `json:"providers"`
	Models          map[string]modelConfig    `json:"models"`
	QuotaGroups     map[string]quotaConfig    `json:"quota_groups"`
	SandboxProfiles map[string]sandboxConfig  `json:"sandbox_profiles"`
	Tools           map[string]toolConfig     `json:"tools"`
	Systems         map[string]systemConfig   `json:"systems"`
}

type providerConfig struct {
	Adapter   string `json:"adapter"`
	BaseURL   string `json:"base_url"`
	APIKeyEnv string `json:"api_key_env"`
}

type modelConfig struct {
	Provider             string   `json:"provider"`
	Model                string   `json:"model"`
	QuotaGroups          []string `json:"quota_groups"`
	MaxOutputTokens      int64    `json:"max_output_tokens"`
	InputHeadroomPercent int64    `json:"input_headroom_percent,omitempty"`
}

type quotaConfig struct {
	RequestsPerMinute int64 `json:"requests_per_minute"`
	TokensPerMinute   int64 `json:"tokens_per_minute"`
	BurstRequests     int64 `json:"burst_requests"`
	MaxConcurrent     int64 `json:"max_concurrent"`
	QueueCapacity     int64 `json:"queue_capacity"`
	MaxWaitSeconds    int64 `json:"max_wait_seconds"`
}

type sandboxConfig struct {
	Read      []string `json:"read"`
	ReadWrite []string `json:"read_write"`
	Network   string   `json:"network"`
}

type toolConfig struct {
	Kind          string   `json:"kind"`
	Version       int64    `json:"version"`
	Description   string   `json:"description"`
	Content       string   `json:"content"`
	RequiresTools []string `json:"requires_tools"`
}

type operatorConfig struct {
	Prompt         string   `json:"prompt"`
	Model          string   `json:"model"`
	Tools          []string `json:"tools"`
	SandboxProfile string   `json:"sandbox_profile"`
}

type systemLimits struct {
	MaxAgents       int64 `json:"max_agents"`
	MaxActiveAgents int64 `json:"max_active_agents"`
	TokenBudget     int64 `json:"token_budget"`
}

type systemConfig struct {
	Tools    []string       `json:"tools"`
	Operator operatorConfig `json:"operator"`
	Limits   systemLimits   `json:"limits"`
}

type fieldError struct{ field, problem string }

func (e *fieldError) Error() string { return e.field + ": " + e.problem }

func invalid(field, problem string) error { return &fieldError{field, problem} }

// decodeJSON bounds nesting and rejects ambiguous duplicate/case-variant keys
// before decoding into explicit types. Parser errors intentionally do not echo
// submitted values, which could contain an accidentally pasted credential.
func decodeJSON(data []byte, target any) error {
	if !utf8.Valid(data) {
		return errors.New("JSON must be valid UTF-8")
	}
	scanner := json.NewDecoder(bytes.NewReader(data))
	scanner.UseNumber()
	if err := scanJSONValue(scanner, 0); err != nil {
		return errors.New("JSON contains invalid, duplicate, or incorrectly cased fields")
	}
	if _, err := scanner.Token(); !errors.Is(err, io.EOF) {
		return errors.New("JSON must contain exactly one value")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("JSON contains an unknown field or an invalid field type")
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("JSON nesting limit exceeded")
	}
	value, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := value.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok || seen[key] || key != strings.ToLower(key) {
				return errors.New("invalid object key")
			}
			seen[key] = true
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected delimiter")
	}
	_, err = decoder.Token()
	return err
}

func loadConfiguration(filename string, lookupEnv func(string) (string, bool)) (configuration, error) {
	var cfg configuration
	absolute, err := filepath.Abs(filename)
	if err != nil {
		return cfg, fmt.Errorf("configuration path: %w", err)
	}
	file, err := openOwnedFile(absolute, os.O_RDONLY, false)
	if err != nil {
		return cfg, fmt.Errorf("open configuration: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return cfg, fmt.Errorf("read configuration: %w", err)
	}
	if len(data) > maxConfigBytes {
		return cfg, invalid("configuration", "exceeds 1 MiB")
	}
	if err := decodeJSON(data, &cfg); err != nil {
		return cfg, fmt.Errorf("configuration: %w", err)
	}
	if err := cfg.validate(lookupEnv); err != nil {
		return cfg, err
	}
	if !filepath.IsAbs(cfg.DataDir) {
		cfg.DataDir = filepath.Join(filepath.Dir(absolute), cfg.DataDir)
	}
	cfg.DataDir = filepath.Clean(cfg.DataDir)
	return cfg, nil
}

func (cfg configuration) validate(lookupEnv func(string) (string, bool)) error {
	if cfg.SchemaVersion != 1 {
		return invalid("schema_version", "must be 1")
	}
	if cfg.DataDir == "" || strings.ContainsRune(cfg.DataDir, 0) {
		return invalid("data_dir", "must be a nonempty path")
	}
	for name, provider := range cfg.Providers {
		if !configName.MatchString(name) {
			return invalid("providers", "invalid name")
		}
		field := "providers." + name
		if provider.Adapter != "openai-chat-completions" {
			return invalid(field+".adapter", "unsupported adapter configuration")
		}
		endpoint, err := url.Parse(provider.BaseURL)
		if err != nil || endpoint.Hostname() == "" || endpoint.User != nil ||
			endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Opaque != "" {
			return invalid(field+".base_url", "must be an absolute URL without credentials, query, or fragment")
		}
		loopback := endpoint.Hostname() == "localhost"
		if ip := net.ParseIP(endpoint.Hostname()); ip != nil {
			loopback = ip.IsLoopback()
		}
		if endpoint.Scheme != "https" && !(endpoint.Scheme == "http" && loopback) {
			return invalid(field+".base_url", "requires HTTPS, except for loopback HTTP")
		}
		if !envName.MatchString(provider.APIKeyEnv) {
			return invalid(field+".api_key_env", "must name an environment variable")
		}
		// Check availability only. Credential values never enter the configuration
		// object, SQLite snapshots, inspection responses, or validation errors.
		if value, ok := lookupEnv(provider.APIKeyEnv); !ok || strings.TrimSpace(value) == "" {
			return invalid(field+".api_key_env", "required credential is unavailable")
		}
	}
	for name, quota := range cfg.QuotaGroups {
		if !configName.MatchString(name) {
			return invalid("quota_groups", "invalid name")
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
				return invalid(field+"."+bound.name, "outside supported bounds")
			}
		}
	}
	for name, model := range cfg.Models {
		if !configName.MatchString(name) {
			return invalid("models", "invalid name")
		}
		field := "models." + name
		if model.InputHeadroomPercent < 0 || model.InputHeadroomPercent > 1000 {
			return invalid(field+".input_headroom_percent", "must be between 0 and 1000; zero selects the 20 percent default")
		}
		if _, ok := cfg.Providers[model.Provider]; !ok {
			return invalid(field+".provider", "unknown provider")
		}
		if strings.TrimSpace(model.Model) == "" || len(model.Model) > 256 {
			return invalid(field+".model", "must contain 1-256 bytes")
		}
		if model.MaxOutputTokens < 1 || model.MaxOutputTokens > 1000000000 {
			return invalid(field+".max_output_tokens", "outside supported bounds")
		}
		if len(model.QuotaGroups) == 0 {
			return invalid(field+".quota_groups", "requires at least one quota group")
		}
		if err := uniqueNames(model.QuotaGroups, field+".quota_groups"); err != nil {
			return err
		}
		for _, group := range model.QuotaGroups {
			quota, ok := cfg.QuotaGroups[group]
			if !ok || model.MaxOutputTokens > quota.TokensPerMinute {
				return invalid(field+".quota_groups", "unknown group or output cap exceeds its token limit")
			}
		}
	}
	for name, profile := range cfg.SandboxProfiles {
		if !configName.MatchString(name) {
			return invalid("sandbox_profiles", "invalid name")
		}
		field := "sandbox_profiles." + name
		if err := validateSandbox(field, profile); err != nil {
			return err
		}
	}
	for name, tool := range cfg.Tools {
		if !configName.MatchString(name) || !strings.HasPrefix(name, "shared.") {
			return invalid("tools", "administrative tool names must start with shared.")
		}
		field := "tools." + name
		if tool.Kind != "skill" {
			return invalid(field+".kind", "executable tools are not implemented at this milestone")
		}
		if tool.Version < 1 || tool.Version > 2147483647 {
			return invalid(field+".version", "outside supported bounds")
		}
		if strings.TrimSpace(tool.Description) == "" || len(tool.Description) > 4096 ||
			strings.TrimSpace(tool.Content) == "" || len(tool.Content) > 32768 {
			return invalid(field, "requires a description (up to 4096 bytes) and content (up to 32768 bytes)")
		}
		if err := uniqueNames(tool.RequiresTools, field+".requires_tools"); err != nil {
			return err
		}
		for _, dependency := range tool.RequiresTools {
			if _, ok := cfg.Tools[dependency]; !ok {
				return invalid(field+".requires_tools", "unknown dependency")
			}
		}
	}
	visiting, visited := make(map[string]bool), make(map[string]bool)
	var visit func(string, int) error
	visit = func(name string, depth int) error {
		if visiting[name] || depth > 32 {
			return invalid("tools", "dependency cycle or excessive nesting")
		}
		if visited[name] {
			return nil
		}
		visiting[name] = true
		for _, dependency := range cfg.Tools[name].RequiresTools {
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
		if !configName.MatchString(name) {
			return invalid("systems", "invalid launch-configuration name")
		}
		if err := cfg.validateSystem(system); err != nil {
			return fmt.Errorf("systems.%s: %w", name, err)
		}
	}
	return nil
}

func validateSandbox(field string, profile sandboxConfig) error {
	if profile.Network != "blocked" {
		return invalid(field+".network", "only blocked networking is supported")
	}
	for _, list := range []struct {
		name     string
		paths    []string
		writable bool
	}{{"read", profile.Read, false}, {"read_write", profile.ReadWrite, true}} {
		if len(list.paths) > 256 {
			return invalid(field+"."+list.name, "too many paths")
		}
		seen := make(map[string]bool)
		for _, value := range list.paths {
			base, _, _ := strings.Cut(value, "/")
			validBase := base == "scratch" || base == "output" || (!list.writable && base == "inputs")
			if !validBase || value != path.Clean(value) || len(value) > 4096 || strings.ContainsAny(value, "\\\x00") || seen[value] {
				return invalid(field+"."+list.name, "paths must be unique, clean, and inside permitted workspace directories")
			}
			seen[value] = true
		}
	}
	return nil
}

func uniqueNames(names []string, field string) error {
	if len(names) > 256 {
		return invalid(field, "too many references")
	}
	seen := make(map[string]bool)
	for _, name := range names {
		if !configName.MatchString(name) || seen[name] {
			return invalid(field, "invalid or duplicate reference")
		}
		seen[name] = true
	}
	return nil
}

func (cfg configuration) validateSystem(system systemConfig) error {
	if strings.TrimSpace(system.Operator.Prompt) == "" || len(system.Operator.Prompt) > 32768 {
		return invalid("operator.prompt", "must contain 1-32768 bytes")
	}
	if _, ok := cfg.Models[system.Operator.Model]; !ok {
		return invalid("operator.model", "unknown model alias")
	}
	if _, ok := cfg.SandboxProfiles[system.Operator.SandboxProfile]; !ok {
		return invalid("operator.sandbox_profile", "unknown profile")
	}
	if system.Limits.MaxAgents < 1 || system.Limits.MaxAgents > 1024 ||
		system.Limits.MaxActiveAgents < 1 || system.Limits.MaxActiveAgents > system.Limits.MaxAgents {
		return invalid("limits", "require 1 <= max_active_agents <= max_agents <= 1024")
	}
	if system.Limits.TokenBudget < 1 {
		return invalid("limits.token_budget", "must be positive")
	}
	if err := uniqueNames(system.Tools, "tools"); err != nil {
		return err
	}
	granted := make(map[string]bool)
	for _, name := range system.Tools {
		if _, ok := cfg.Tools[name]; !ok {
			return invalid("tools", "unknown or unimplemented tool")
		}
		granted[name] = true
	}
	if err := uniqueNames(system.Operator.Tools, "operator.tools"); err != nil {
		return err
	}
	operator := make(map[string]bool)
	for _, name := range system.Operator.Tools {
		if !granted[name] {
			return invalid("operator.tools", "must be a subset of system tools")
		}
		operator[name] = true
	}
	for _, grants := range []map[string]bool{granted, operator} {
		for name := range grants {
			for _, dependency := range cfg.Tools[name].RequiresTools {
				if !grants[dependency] {
					return invalid("tools", "skill dependencies must already be granted at each scope")
				}
			}
		}
	}
	return nil
}

func jsonDigest(value any) (string, []byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", nil, err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), data, nil
}
