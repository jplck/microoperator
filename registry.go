//go:build darwin || linux

package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

type functionSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}
type modelFunction struct {
	Type     string         `json:"type"`
	Function functionSchema `json:"function"`
}
type modelToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func builtinTool(name string) (toolConfig, bool) {
	description := ""
	switch name {
	case "runtime.text.analyze":
		description = "Analyze text in a confined reviewed subprocess; optionally save its JSON report as a scoped artifact."
	case "runtime.agent.list":
		description = "List collaborators in this system and goal. Names and descriptions never grant authority."
	case "runtime.agent.propose":
		description = "Propose a child with a prompt, narrower tools and a token cap. Returns an agent_id; does not delegate work."
	case "runtime.task.delegate":
		description = "Delegate to a same-goal collaborator with intersected task grants. The parent waits without occupying a worker slot."
	case "runtime.task.progress":
		description = "Record bounded progress for the parent via the durable mailbox."
	case "runtime.tool.propose":
		description = "Store a system-private, inert skill or source draft. This never installs or executes it."
	default:
		return toolConfig{}, false
	}
	return toolConfig{Kind: "executable", Version: 1, Description: description, Content: "reviewed:" + name, RequiresTools: []string{}}, true
}

func (cfg configuration) tool(name string) (toolConfig, bool) {
	if tool, ok := builtinTool(name); ok {
		if name == "runtime.text.analyze" {
			tool.Content += ":sha256:" + cfg.runtimeDigest
		}
		return tool, true
	}
	tool, ok := cfg.Tools[name]
	return tool, ok
}

func wireToolName(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "fn_" + hex.EncodeToString(sum[:16])
}

func executableSchema(name string, tool toolConfig) (modelFunction, error) {
	parameters := ""
	switch name {
	case "runtime.text.analyze":
		parameters = `{"type":"object","properties":{"text":{"type":"string","maxLength":4096},"save_artifact":{"type":"boolean"}},"required":["text"],"additionalProperties":false}`
	case "runtime.agent.list":
		parameters = `{"type":"object","properties":{"after":{"type":"string","maxLength":64}},"additionalProperties":false}`
	case "runtime.agent.propose":
		parameters = `{"type":"object","properties":{"name":{"type":"string"},"prompt":{"type":"string"},"tools":{"type":"array","items":{"type":"string"}},"token_budget":{"type":"integer","minimum":1}},"required":["name","prompt","tools","token_budget"],"additionalProperties":false}`
	case "runtime.task.delegate":
		parameters = `{"type":"object","properties":{"agent_id":{"type":"string"},"prompt":{"type":"string"}},"required":["agent_id","prompt"],"additionalProperties":false}`
	case "runtime.task.progress":
		parameters = `{"type":"object","properties":{"message":{"type":"string","maxLength":2048}},"required":["message"],"additionalProperties":false}`
	case "runtime.tool.propose":
		parameters = `{"type":"object","properties":{"kind":{"type":"string","enum":["skill","executable"]},"description":{"type":"string"},"content":{"type":"string"},"requires_tools":{"type":"array","items":{"type":"string"}}},"required":["kind","description","content"],"additionalProperties":false}`
	default:
		return modelFunction{}, invalid("tool", "no reviewed executable implementation")
	}
	return modelFunction{Type: "function", Function: functionSchema{Name: wireToolName(name), Description: name + ": " + tool.Description, Parameters: json.RawMessage(parameters)}}, nil
}

func authorizePins(ctx context.Context, tx *sql.Tx, cfg configuration, systemID string, pins []toolPin) error {
	allowed := make(map[string]bool)
	for _, pin := range pins {
		tool, ok := cfg.tool(pin.Name)
		if !ok {
			return invalid("tools", "pinned definition is unavailable")
		}
		digest, _, err := jsonDigest(tool)
		if err != nil {
			return err
		}
		if pin.Version != tool.Version || pin.Digest != digest {
			return invalid("tools", "stale pinned definition")
		}
		var revoked int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tool_revocations WHERE system_id=? AND name=? AND version=?`, systemID, pin.Name, pin.Version).Scan(&revoked); err != nil {
			return err
		}
		if revoked != 0 {
			return invalid("tools", "pinned version was revoked")
		}
		allowed[pin.Name] = true
	}
	for _, pin := range pins {
		tool, _ := cfg.tool(pin.Name)
		for _, required := range tool.RequiresTools {
			if !allowed[required] {
				return invalid("tools", "missing pinned dependency")
			}
		}
	}
	return nil
}

type registryEntry struct {
	ID               string          `json:"tool_id"`
	SystemID         string          `json:"system_id,omitempty"`
	Version          int64           `json:"version"`
	Kind             string          `json:"kind"`
	Description      string          `json:"description"`
	Content          string          `json:"content,omitempty"`
	Requires         []string        `json:"requires_tools"`
	Digest           string          `json:"digest"`
	State            string          `json:"state"`
	Origin           string          `json:"origin"`
	Schema           *modelFunction  `json:"schema,omitempty"`
	Effect           string          `json:"effect,omitempty"`
	TimeoutSeconds   int             `json:"timeout_seconds,omitempty"`
	Execution        string          `json:"execution,omitempty"`
	OutputSchema     json.RawMessage `json:"output_schema,omitempty"`
	InputBytes       int             `json:"max_input_bytes,omitempty"`
	OutputBytes      int             `json:"max_output_bytes,omitempty"`
	ArtifactBytes    int             `json:"max_artifact_bytes,omitempty"`
	ExecutableDigest string          `json:"executable_digest,omitempty"`
	Arguments        []string        `json:"arguments,omitempty"`
	Capabilities     []string        `json:"required_capabilities,omitempty"`
	GoalID           string          `json:"goal_id,omitempty"`
	TaskID           string          `json:"task_id,omitempty"`
}

func (store *stateStore) catalog(ctx context.Context, cfg configuration, principal, systemID string) (entries []registryEntry, err error) {
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer rollback(tx, &err)
	if systemID != "" {
		if _, err := readSystem(ctx, tx, principal, systemID); err != nil {
			return nil, err
		}
	}
	names := []string{"runtime.text.analyze", "runtime.agent.list", "runtime.agent.propose", "runtime.task.delegate", "runtime.task.progress", "runtime.tool.propose"}
	for name := range cfg.Tools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		tool, _ := cfg.tool(name)
		digest, _, err := jsonDigest(tool)
		if err != nil {
			return nil, err
		}
		entry := registryEntry{ID: name, Version: tool.Version, Kind: tool.Kind, Description: tool.Description, Content: tool.Content, Requires: tool.RequiresTools, Digest: digest, State: "active", Origin: "administrative-json"}
		if _, ok := builtinTool(name); ok {
			entry.Origin, entry.Effect, entry.TimeoutSeconds = "reviewed-runtime", "local-transaction", 10
			entry.Execution, entry.InputBytes, entry.OutputBytes = "trusted-builtin", maxEventBytes, maxEventBytes
			entry.OutputSchema = json.RawMessage(`{"type":"object"}`)
			schema, err := executableSchema(name, tool)
			if err != nil {
				return nil, err
			}
			entry.Schema = &schema
			if name == "runtime.text.analyze" {
				entry.Effect = "private-workspace-output"
				entry.Execution, entry.ArtifactBytes = "sandboxed-subprocess", 4096
				entry.ExecutableDigest = cfg.runtimeDigest
				entry.Arguments = []string{"tool", "text-analyze"}
				entry.Capabilities = []string{"private output write when save_artifact=true"}
				entry.OutputSchema = json.RawMessage(`{"type":"object","properties":{"runes":{"type":"integer","minimum":0},"words":{"type":"integer","minimum":0},"artifact":{"type":"string"}},"required":["runes","words"],"additionalProperties":false}`)
			}
		}
		if systemID != "" {
			var revoked int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tool_revocations WHERE system_id=? AND name=? AND version=?`, systemID, name, tool.Version).Scan(&revoked); err != nil {
				return nil, err
			}
			if revoked > 0 {
				entry.State = "revoked"
			}
		}
		entries = append(entries, entry)
	}
	if systemID == "" {
		return entries, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT tool_id,version,kind,description,content,requires_tools,digest,state,creator,goal_id,task_id FROM tool_drafts WHERE system_id=? ORDER BY tool_id,version`, systemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		e := registryEntry{SystemID: systemID}
		var requires []byte
		if err := rows.Scan(&e.ID, &e.Version, &e.Kind, &e.Description, &e.Content, &requires, &e.Digest, &e.State, &e.Origin, &e.GoalID, &e.TaskID); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(requires, &e.Requires); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

type draftCommand struct {
	Kind        string   `json:"kind"`
	Description string   `json:"description"`
	Content     string   `json:"content"`
	Requires    []string `json:"requires_tools"`
}

func submitDraft(ctx context.Context, tx *sql.Tx, cfg configuration, systemID, creator, goalID, taskID string, command draftCommand) (string, error) {
	if (command.Kind != "skill" && command.Kind != "executable") || strings.TrimSpace(command.Description) == "" ||
		len(command.Description) > 2048 || strings.TrimSpace(command.Content) == "" || len(command.Content) > maxEventBytes {
		return "", invalid("draft", "requires a skill or inert executable source, description and bounded content")
	}
	if err := uniqueNames(command.Requires, "requires_tools"); err != nil {
		return "", err
	}
	for _, name := range command.Requires {
		if _, ok := cfg.tool(name); !ok {
			return "", invalid("requires_tools", "unknown dependency")
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tool_drafts WHERE system_id=?`, systemID).Scan(&count); err != nil {
		return "", err
	}
	if count >= 64 {
		return "", invalid("draft", "system draft capacity exhausted")
	}
	id, err := newID("local.")
	if err != nil {
		return "", err
	}
	digest, _, err := jsonDigest(command)
	if err != nil {
		return "", err
	}
	requires, err := json.Marshal(command.Requires)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO tool_drafts(system_id,tool_id,version,creator,goal_id,task_id,kind,description,content,requires_tools,digest,state)
	 VALUES(?,?,1,?,?,?,?,?,?,?,?,'draft')`, systemID, id, creator, goalID, taskID, command.Kind, command.Description, command.Content, requires, digest)
	return id, err
}

type textArguments struct {
	Text string `json:"text"`
	Save bool   `json:"save_artifact"`
}
type textResult struct {
	Runes    int    `json:"runes"`
	Words    int    `json:"words"`
	Artifact string `json:"artifact,omitempty"`
}

// reviewedTextTool is fixed Go code. Agent-authored source never reaches this
// entry point; it has no shell, network, arbitrary path, or code-loading option.
func reviewedTextTool(in io.Reader, out io.Writer) error {
	if err := writeMessage(out, message{Type: "ready"}); err != nil {
		return err
	}
	request, err := readMessage(bufio.NewReaderSize(in, maxFrame+1))
	if err != nil {
		return err
	}
	var args textArguments
	if request.Type != "tool.input" || decodeJSON([]byte(request.Data), &args) != nil || len(args.Text) > 4096 {
		return errors.New("invalid text analysis arguments")
	}
	result := textResult{Runes: utf8.RuneCountInString(args.Text), Words: len(strings.Fields(args.Text))}
	if args.Save {
		data, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if err := os.WriteFile("output/report.json", data, 0600); err != nil {
			return err
		}
		result.Artifact = "output/report.json"
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return writeMessage(out, message{Type: "tool.result", ID: request.ID, Data: string(data)})
}

func (engine *executionEngine) runTextTool(ctx context.Context, e executionRecord, args textArguments, profile sandboxConfig) (result textResult, artifact []byte, err error) {
	digest, err := executableDigest(engine.executable)
	if err != nil {
		return result, nil, err
	}
	if digest != engine.cfg.runtimeDigest {
		return result, nil, invalid("executable", "reviewed binary changed; restart and explicitly revise its assignments")
	}
	root, err := os.MkdirTemp(engine.cfg.DataDir, "tool-"+e.SystemID+"-")
	if err != nil {
		return result, nil, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(root)) }()
	paths := append([]string{"inputs", "scratch", "output"}, profile.Read...)
	paths = append(paths, profile.ReadWrite...)
	for _, name := range paths {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			return result, nil, err
		}
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return result, nil, err
	}
	data, err := json.Marshal(args)
	if err != nil {
		return result, nil, err
	}
	toolCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err = supervise(toolCtx, engine.executable, root, engine.executable, []string{"tool", "text-analyze"}, func(in io.Writer, out io.Reader) error {
		reader := bufio.NewReaderSize(out, maxFrame+1)
		ready, err := readMessage(reader)
		if err != nil {
			return err
		}
		if ready != (message{Type: "ready"}) {
			return errors.New("invalid tool readiness")
		}
		if err := writeMessage(in, message{Type: "tool.input", ID: e.CallID, Data: string(data)}); err != nil {
			return err
		}
		reply, err := readMessage(reader)
		if err != nil {
			return err
		}
		if reply.Type != "tool.result" || reply.ID != e.CallID {
			return errors.New("tool response correlation mismatch")
		}
		if err := decodeJSON([]byte(reply.Data), &result); err != nil {
			return err
		}
		if result.Runes != utf8.RuneCountInString(args.Text) || result.Words != len(strings.Fields(args.Text)) {
			return errors.New("invalid tool result")
		}
		return nil
	}, profile)
	if err != nil {
		return result, nil, err
	}
	if args.Save {
		if result.Artifact != "output/report.json" {
			return result, nil, errors.New("tool artifact escaped its manifest")
		}
		file, err := openOwnedFile(filepath.Join(root, result.Artifact), os.O_RDONLY, true)
		if err != nil {
			return result, nil, err
		}
		artifact, err = io.ReadAll(io.LimitReader(file, 4097))
		err = errors.Join(err, file.Close())
		if err != nil || len(artifact) > 4096 {
			return result, nil, errors.Join(err, errors.New("invalid artifact size"))
		}
		var report textResult
		if err := decodeJSON(artifact, &report); err != nil || report.Runes != result.Runes || report.Words != result.Words || report.Artifact != "" {
			return result, nil, errors.New("invalid report artifact")
		}
	} else if result.Artifact != "" {
		return result, nil, errors.New("unexpected tool artifact")
	}
	return result, artifact, nil
}

func executableDigest(filename string) (string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, err = io.Copy(hash, file)
	if err = errors.Join(err, file.Close()); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func artifactDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func resolveTool(cfg configuration, pins []toolPin, wireName string) (toolPin, error) {
	for _, pin := range pins {
		if wireToolName(pin.Name) == wireName {
			tool, ok := cfg.tool(pin.Name)
			if !ok || tool.Kind != "executable" {
				break
			}
			return pin, nil
		}
	}
	return toolPin{}, invalid("tool", "function is not in this task's pinned grant")
}

// Only the small schema vocabulary used by reviewed built-ins is accepted.
// Definitions cannot supply a validator, shell command, or executable code.
func validateArguments(schema functionSchema, arguments string) error {
	var spec struct {
		Required   []string `json:"required"`
		Properties map[string]struct {
			Type      string   `json:"type"`
			MaxLength int      `json:"maxLength"`
			Minimum   *int64   `json:"minimum"`
			Enum      []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema.Parameters, &spec); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := decodeJSON([]byte(arguments), &fields); err != nil {
		return invalid("arguments", err.Error())
	}
	if fields == nil {
		return invalid("arguments", "requires an object")
	}
	for _, name := range spec.Required {
		if _, ok := fields[name]; !ok {
			return invalid("arguments", "missing required field "+name)
		}
	}
	for name, data := range fields {
		property, ok := spec.Properties[name]
		if !ok {
			return invalid("arguments", "unknown field")
		}
		if string(data) == "null" {
			return invalid("arguments", "null is not supported")
		}
		switch property.Type {
		case "string":
			var value string
			if json.Unmarshal(data, &value) != nil {
				return invalid("arguments", "expected a string")
			}
			if property.MaxLength > 0 && len(value) > property.MaxLength {
				return invalid("arguments", "string exceeds byte limit")
			}
			if len(property.Enum) > 0 {
				found := false
				for _, item := range property.Enum {
					if value == item {
						found = true
					}
				}
				if !found {
					return invalid("arguments", "unsupported value")
				}
			}
		case "integer":
			var value int64
			if json.Unmarshal(data, &value) != nil || (property.Minimum != nil && value < *property.Minimum) {
				return invalid("arguments", "invalid integer")
			}
		case "boolean":
			var value bool
			if json.Unmarshal(data, &value) != nil {
				return invalid("arguments", "expected a boolean")
			}
		case "array":
			var values []string
			if json.Unmarshal(data, &values) != nil || len(values) > 256 {
				return invalid("arguments", "expected a bounded string array")
			}
		default:
			return errors.New("unsupported reviewed argument schema")
		}
	}
	return nil
}

func toolOutcome(data any) (string, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	if len(b) > maxEventBytes {
		return "", invalid("tool result", fmt.Sprintf("exceeds %d bytes", maxEventBytes))
	}
	return string(b), nil
}
