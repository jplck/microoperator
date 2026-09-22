package state

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jplck/microoperator/internal/protocol"
)

type ReviseAgentArgs struct {
	AgentID          string   `json:"agent_id"`
	ExpectedRevision int64    `json:"expected_revision"`
	Prompt           string   `json:"prompt"`
	Model            string   `json:"model"`
	Tools            []string `json:"tools"`
}

type RetireAgentArgs struct {
	AgentID          string `json:"agent_id"`
	ExpectedRevision int64  `json:"expected_revision"`
}

func (cfg Configuration) authorizeModel(system SystemConfig, name string) error {
	if _, ok := cfg.Models[name]; !ok || !slices.Contains(system.AllowedModels(), name) {
		return Invalid("model", "alias is not configured and allowed by this system")
	}
	return nil
}

func narrowToolPins(granted []ToolPin, names []string) ([]ToolPin, error) {
	if err := UniqueNames(names, "tools"); err != nil {
		return nil, err
	}
	pins := make([]ToolPin, 0, len(names))
	for _, name := range names {
		index := slices.IndexFunc(granted, func(pin ToolPin) bool { return pin.Name == name })
		if index < 0 {
			return nil, Invalid("tools", "agent cannot expand its creator's or caller's task grant")
		}
		pins = append(pins, granted[index])
	}
	return pins, nil
}

func (engine *workflow) manageAgent(ctx context.Context, tx *sql.Tx, task TaskRecord, name, arguments string) (string, error) {
	system, err := readSystem(ctx, tx, localAdministrator, task.SystemID)
	if err != nil {
		return "", err
	}
	if name == "runtime.model.list" {
		var args struct {
			After string `json:"after"`
		}
		if err := protocol.DecodeJSON([]byte(arguments), &args); err != nil {
			return "", err
		}
		names := system.Configuration.AllowedModels()
		sort.Strings(names)
		type modelView struct {
			Name            string `json:"name"`
			Provider        string `json:"provider"`
			Model           string `json:"model"`
			MaxOutputTokens int64  `json:"max_output_tokens"`
		}
		result := []modelView{}
		next := ""
		for _, alias := range names {
			if alias <= args.After {
				continue
			}
			if len(result) == 5 {
				next = result[len(result)-1].Name
				break
			}
			if err := engine.cfg.authorizeModel(system.Configuration, alias); err != nil {
				return "", err
			}
			model := engine.cfg.Models[alias]
			result = append(result, modelView{alias, model.Provider, model.Model, model.MaxOutputTokens})
		}
		return ToolOutcome(struct {
			Models []modelView `json:"models"`
			Next   string      `json:"next"`
		}{result, next})
	}
	if task.AgentID != system.OperatorID {
		return "", Invalid("agent", "only the system operator may revise or retire agents")
	}
	var revision ReviseAgentArgs
	var target RetireAgentArgs
	switch name {
	case "runtime.agent.revise":
		if err := protocol.DecodeJSON([]byte(arguments), &revision); err != nil {
			return "", err
		}
		target = RetireAgentArgs{revision.AgentID, revision.ExpectedRevision}
	case "runtime.agent.retire":
		if err := protocol.DecodeJSON([]byte(arguments), &target); err != nil {
			return "", err
		}
	default:
		return "", Invalid("tool", "unsupported agent operation")
	}
	agent, err := readAgent(ctx, tx, task.SystemID, target.AgentID, 0)
	if errors.Is(err, sql.ErrNoRows) {
		return "", Invalid("agent", "agent unavailable in this scope")
	}
	if err != nil {
		return "", err
	}
	if agent.ID == system.OperatorID || agent.Parent == "" || agent.GoalID != task.GoalID || agent.State == "stopped" {
		return "", Invalid("agent", "requires a live child belonging to this goal")
	}
	if target.ExpectedRevision < 1 || agent.Revision != target.ExpectedRevision {
		return "", Invalid("expected_revision", "agent revision has changed")
	}
	if name == "runtime.agent.retire" {
		rows, err := tx.QueryContext(ctx, `WITH RECURSIVE children(agent_id) AS (
		 SELECT agent_id FROM agents WHERE system_id=? AND agent_id=?
		 UNION SELECT a.agent_id FROM agents a JOIN children c ON a.parent_id=c.agent_id WHERE a.system_id=? AND a.goal_id=?
		) SELECT agent_id FROM children`, task.SystemID, agent.ID, task.SystemID, task.GoalID)
		if err != nil {
			return "", err
		}
		ids, err := readIDs(rows)
		if err != nil {
			return "", err
		}
		result := ControlResult{Scope: "agent", ID: agent.ID, Action: "retire", Calls: []string{}}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE agents SET state='stopped' WHERE system_id=? AND agent_id=?`, task.SystemID, id); err != nil {
				return "", err
			}
			calls, err := stopTasks(ctx, tx, task.SystemID, "agent", id, task.AgentID, "retired by operator")
			if err != nil {
				return "", err
			}
			result.Calls = append(result.Calls, calls...)
		}
		sort.Strings(result.Calls)
		result.Calls = slices.Compact(result.Calls)
		return ToolOutcome(result)
	}
	if strings.TrimSpace(revision.Prompt) == "" || len(revision.Prompt) > 4096 || !utf8.ValidString(revision.Prompt) {
		return "", Invalid("prompt", "requires 1-4096 UTF-8 bytes")
	}
	var protected bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM learning_evaluations WHERE system_id=? AND agent_id=?)`, task.SystemID, agent.ID).Scan(&protected); err != nil {
		return "", err
	}
	if protected {
		return "", Invalid("agent", "protected evaluation agents cannot be revised")
	}
	if err := engine.cfg.authorizeModel(system.Configuration, revision.Model); err != nil {
		return "", err
	}
	pins, err := narrowToolPins(task.Tools, revision.Tools)
	if err != nil {
		return "", err
	}
	parent, err := readAgent(ctx, tx, task.SystemID, agent.Parent, 0)
	if err != nil {
		return "", err
	}
	for _, pin := range pins {
		if !slices.Contains(parent.Tools, pin) {
			return "", Invalid("tools", "revision exceeds the creation parent's current grant")
		}
	}
	if err := authorizePins(ctx, tx, engine.cfg, task.SystemID, pins); err != nil {
		return "", err
	}
	agent.Revision++
	agent.Definition.Prompt, agent.Definition.Model, agent.Definition.Tools = revision.Prompt, revision.Model, revision.Tools
	agent.Tools = pins
	if err := insertAgentRevision(ctx, tx, agent); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agents SET revision=? WHERE system_id=? AND agent_id=?`, agent.Revision, task.SystemID, agent.ID); err != nil {
		return "", err
	}
	return ToolOutcome(struct {
		ID       string `json:"agent_id"`
		Revision int64  `json:"revision"`
	}{agent.ID, agent.Revision})
}
