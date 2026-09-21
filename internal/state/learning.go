package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type ProtectedCase struct {
	Input    string `json:"input"`
	Expected string `json:"expected"`
}

type LearningEvidence struct {
	Input           string `json:"input"`
	Expected        string `json:"expected"`
	Baseline        string `json:"baseline"`
	Candidate       string `json:"candidate"`
	BaselinePassed  bool   `json:"baseline_passed"`
	CandidatePassed bool   `json:"candidate_passed"`
}

func (cfg Configuration) WithLocalTools(tools map[string]ToolConfig) Configuration {
	if len(tools) == 0 {
		return cfg
	}
	copyTools := make(map[string]ToolConfig, len(cfg.Tools)+len(tools))
	for name, Tool := range cfg.Tools {
		copyTools[name] = Tool
	}
	for name, Tool := range tools {
		copyTools[name] = Tool
	}
	cfg.Tools = copyTools
	return cfg
}

func loadLocalTools(ctx context.Context, tx *sql.Tx, systemID string, pins []ToolPin) (map[string]ToolConfig, error) {
	var tools map[string]ToolConfig
	for _, pin := range pins {
		if !strings.HasPrefix(pin.Name, "local.") {
			continue
		}
		var definition []byte
		var digest string
		err := tx.QueryRowContext(ctx, `SELECT definition,digest FROM learning_approvals WHERE system_id=? AND tool_id=? AND version=?`, systemID, pin.Name, pin.Version).Scan(&definition, &digest)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, Invalid("tool", "local revision is not approved in this system")
		}
		if err != nil {
			return nil, err
		}
		if digest != pin.Digest {
			return nil, Invalid("tool", "local definition digest does not match its exact pin")
		}
		var Tool ToolConfig
		if err := DecodeJSON(definition, &Tool); err != nil {
			return nil, err
		}
		actual, _, err := JsonDigest(Tool)
		if err != nil {
			return nil, err
		}
		if actual != digest || Tool.Version != pin.Version {
			return nil, errors.New("stored approved definition is inconsistent")
		}
		if tools == nil {
			tools = make(map[string]ToolConfig)
		}
		tools[pin.Name] = Tool
	}
	return tools, nil
}

func authorizeLocalPin(ctx context.Context, tx *sql.Tx, systemID string, pin ToolPin, now time.Time) error {
	var state, draftState string
	var expires int64
	err := tx.QueryRowContext(ctx, `SELECT e.state,d.state,a.expires_at FROM learning_approvals a
	 JOIN learning_evaluations e ON e.system_id=a.system_id AND e.evaluation_id=a.evaluation_id
	 JOIN tool_drafts d ON d.system_id=a.system_id AND d.tool_id=a.tool_id AND d.version=a.version
	 WHERE a.system_id=? AND a.tool_id=? AND a.version=? AND a.digest=?`, systemID, pin.Name, pin.Version, pin.Digest).Scan(&state, &draftState, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Invalid("tool", "no exact local approval")
	}
	if err != nil {
		return err
	}
	if state != "active" || draftState != "draft" || now.UnixMilli() >= expires {
		return Invalid("tool", "local approval expired, was rejected, or was disabled")
	}
	return nil
}

// An approval admits a bounded number of distinct tasks. The first model
// dispatch consumes a use atomically; retries and continuations cannot reset it.
func admitLearningUses(ctx context.Context, tx *sql.Tx, e ExecutionRecord, now time.Time) error {
	t, err := readTask(ctx, tx, e.SystemID, e.TaskID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var pending []ToolPin
	for _, pin := range t.Tools {
		if !strings.HasPrefix(pin.Name, "local.") {
			continue
		}
		if err := authorizeLocalPin(ctx, tx, t.SystemID, pin, now); err != nil {
			return err
		}
		var used int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM learning_uses WHERE system_id=? AND tool_id=? AND version=? AND task_id=?`, t.SystemID, pin.Name, pin.Version, t.ID).Scan(&used); err != nil {
			return err
		}
		if used != 0 {
			continue
		}
		var remaining int64
		if err := tx.QueryRowContext(ctx, `SELECT remaining_tasks FROM learning_approvals WHERE system_id=? AND tool_id=? AND version=?`, t.SystemID, pin.Name, pin.Version).Scan(&remaining); err != nil {
			return err
		}
		if remaining == 0 {
			return Invalid("approval", "task-use limit exhausted")
		}
		pending = append(pending, pin)
	}
	for _, pin := range pending {
		result, err := tx.ExecContext(ctx, `UPDATE learning_approvals SET remaining_tasks=remaining_tasks-1 WHERE system_id=? AND tool_id=? AND version=? AND remaining_tasks>0`, t.SystemID, pin.Name, pin.Version)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return errors.New("approval reservation changed inside transaction")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO learning_uses(system_id,tool_id,version,task_id) VALUES(?,?,?,?)`, t.SystemID, pin.Name, pin.Version, t.ID); err != nil {
			return err
		}
	}
	return nil
}

func GeneratedArtifactPath(dataDir, systemID, digest string) (string, error) {
	if !SystemIDPattern.MatchString(systemID) || len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		return "", Invalid("artifact", "invalid generated artifact identity")
	}
	return filepath.Join(dataDir, "generated", systemID, digest), nil
}

func SaveGeneratedArtifact(dataDir, systemID string, binary []byte) (string, error) {
	if len(binary) == 0 || len(binary) > MaxGeneratedBinary {
		return "", Invalid("artifact", "binary exceeds bound")
	}
	digest := ArtifactDigest(binary)
	path, err := GeneratedArtifactPath(dataDir, systemID, digest)
	if err != nil {
		return "", err
	}
	for _, directory := range []string{filepath.Join(dataDir, "generated"), filepath.Dir(path)} {
		if err := os.Mkdir(directory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		info, err := os.Lstat(directory)
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return "", errors.New("generated artifact directory is not private")
		}
	}
	file, err := OpenOwnedFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, true)
	if errors.Is(err, os.ErrExist) {
		data, readErr := ReadGeneratedArtifact(dataDir, systemID, digest)
		if readErr != nil {
			return "", readErr
		}
		if len(data) != len(binary) {
			return "", errors.New("existing artifact is inconsistent")
		}
		return digest, nil
	}
	if err != nil {
		return "", err
	}
	_, err = file.Write(binary)
	err = errors.Join(err, file.Sync(), file.Close())
	if err != nil {
		return "", errors.Join(err, os.Remove(path))
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	err = errors.Join(directory.Sync(), directory.Close())
	return digest, err
}

func ReadGeneratedArtifact(dataDir, systemID, digest string) ([]byte, error) {
	path, err := GeneratedArtifactPath(dataDir, systemID, digest)
	if err != nil {
		return nil, err
	}
	file, err := OpenOwnedFile(path, os.O_RDONLY, true)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxGeneratedBinary+1))
	err = errors.Join(err, file.Close())
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > MaxGeneratedBinary || ArtifactDigest(data) != digest {
		return nil, Invalid("artifact", "approved binary changed or exceeds size bound")
	}
	return data, nil
}

func ProtectedCases(data []byte) ([]ProtectedCase, error) {
	var cases []ProtectedCase
	if err := DecodeJSON(data, &cases); err != nil {
		return nil, err
	}
	if len(cases) < 1 || len(cases) > 2 {
		return nil, Invalid("checks", "requires one or two protected cases")
	}
	for _, check := range cases {
		if strings.TrimSpace(check.Input) == "" || len(check.Input) > 1024 || len(check.Expected) > 4096 {
			return nil, Invalid("checks", "input or expected output exceeds limit")
		}
	}
	return cases, nil
}

func CandidateDefinition(draft DraftCommand, version int64, binary, profile string) ToolConfig {
	return ToolConfig{Kind: draft.Kind, Version: version, Description: draft.Description, Content: draft.Content, RequiresTools: draft.Requires,
		BinaryDigest: binary, ProfileDigest: profile}
}

func readLearningDraft(ctx context.Context, tx *sql.Tx, systemID, id string, version int64) (DraftCommand, error) {
	var draft DraftCommand
	var requires []byte
	var state string
	err := tx.QueryRowContext(ctx, `SELECT kind,description,content,requires_tools,state FROM tool_drafts WHERE system_id=? AND tool_id=? AND version=?`, systemID, id, version).
		Scan(&draft.Kind, &draft.Description, &draft.Content, &requires, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return draft, ErrSystemNotFound
	}
	if err != nil {
		return draft, err
	}
	if state != "draft" {
		return draft, Invalid("draft", "candidate is rejected or disabled")
	}
	err = json.Unmarshal(requires, &draft.Requires)
	return draft, err
}
