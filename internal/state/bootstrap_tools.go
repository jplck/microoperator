package state

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/jplck/microoperator/internal/protocol"
)

func (engine *workflow) bootstrapTool(ctx context.Context, tx *sql.Tx, task TaskRecord, name, arguments string) (string, error) {
	switch name {
	case "runtime.capabilities":
		var args struct{}
		if err := protocol.DecodeJSON([]byte(arguments), &args); err != nil {
			return "", err
		}
		system, err := readSystem(ctx, tx, localAdministrator, task.SystemID)
		if err != nil {
			return "", err
		}
		agent, err := readAgent(ctx, tx, task.SystemID, task.AgentID, task.Revision)
		if err != nil {
			return "", err
		}
		var budget, used, reserved, deadline int64
		if err := tx.QueryRowContext(ctx, `SELECT token_budget,used_tokens,reserved_tokens,deadline FROM goals WHERE system_id=? AND goal_id=?`,
			task.SystemID, task.GoalID).Scan(&budget, &used, &reserved, &deadline); err != nil {
			return "", err
		}
		remaining := min(budget-used-reserved, system.Configuration.Limits.TokenBudget-system.UsedTokens-system.ReservedTokens)
		ancestors, err := budgetAgents(ctx, tx, ExecutionRecord{SystemID: task.SystemID, TaskID: task.ID, AgentID: task.AgentID})
		if err != nil {
			return "", err
		}
		for _, id := range ancestors {
			var available int64
			if err := tx.QueryRowContext(ctx, `SELECT a.token_budget-u.used_tokens-u.reserved_tokens FROM agents a JOIN agent_usage u
			 ON a.system_id=u.system_id AND a.agent_id=u.agent_id WHERE a.system_id=? AND a.agent_id=? AND u.goal_id=?`,
				task.SystemID, id, task.GoalID).Scan(&available); err != nil {
				return "", err
			}
			remaining = min(remaining, available)
		}
		rows, err := tx.QueryContext(ctx, `SELECT check_id FROM learning_checks WHERE system_id=? ORDER BY check_id LIMIT 64`, task.SystemID)
		if err != nil {
			return "", err
		}
		checks, err := readIDs(rows)
		if err != nil {
			return "", err
		}
		buildReason := ""
		if err := GeneratedProfile(engine.cfg.SandboxProfiles[agent.Definition.SandboxProfile]); err != nil {
			buildReason = err.Error()
		} else if engine.cfg.Learning == nil {
			buildReason = "a pinned local Go toolchain is not configured"
		}
		return ToolOutcome(struct {
			Tools                 []ToolPin    `json:"granted_tools"`
			Model                 string       `json:"model"`
			Sandbox               string       `json:"sandbox_profile"`
			Limits                SystemLimits `json:"system_limits"`
			RemainingTokens       int64        `json:"remaining_tokens"`
			RemainingTaskTurns    int          `json:"remaining_task_turns"`
			Deadline              int64        `json:"goal_deadline_ms"`
			ProtectedChecks       []string     `json:"protected_check_ids"`
			GeneratedBuildReady   bool         `json:"generated_build_configured"`
			GeneratedBuildBlocker string       `json:"generated_build_blocker,omitempty"`
			HumanApproval         bool         `json:"exact_artifact_approval_required"`
			GeneratedContract     string       `json:"generated_tool_contract"`
		}{task.Tools, agent.Definition.Model, agent.Definition.SandboxProfile, system.Configuration.Limits,
			remaining, max(0, MaxTaskTurns-task.Turns), deadline, checks, buildReason == "", buildReason, true,
			"package main; Process(string) (string, error); stdlib only; source <=8192 bytes; input <=4096 bytes; output <=8192 bytes; 10 seconds; no network/broker access; explicit state input/output"})
	case "runtime.artifact.put":
		var args struct {
			Content string `json:"content"`
		}
		if err := protocol.DecodeJSON([]byte(arguments), &args); err != nil {
			return "", err
		}
		if strings.TrimSpace(args.Content) == "" || len(args.Content) > 3072 || !utf8.ValidString(args.Content) || strings.ContainsRune(args.Content, 0) {
			return "", Invalid("content", "requires 1-3072 bytes of inert UTF-8 text without NUL")
		}
		// Bound the serialized read result as well as its raw stored content.
		if _, err := ToolOutcome(map[string]string{"content": args.Content}); err != nil {
			return "", err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM artifacts WHERE system_id=? AND goal_id=?`, task.SystemID, task.GoalID).Scan(&count); err != nil {
			return "", err
		}
		if count >= 128 {
			return "", Invalid("artifacts", "goal artifact limit reached")
		}
		id, err := NewID("artifact_")
		if err != nil {
			return "", err
		}
		content := []byte(args.Content)
		if _, err := tx.ExecContext(ctx, `INSERT INTO artifacts(system_id,artifact_id,goal_id,task_id,digest,content) VALUES(?,?,?,?,?,?)`,
			task.SystemID, id, task.GoalID, task.ID, ArtifactDigest(content), content); err != nil {
			return "", err
		}
		return ToolOutcome(map[string]string{"artifact_id": id})
	case "runtime.artifact.get":
		var args struct {
			ID string `json:"artifact_id"`
		}
		if err := protocol.DecodeJSON([]byte(arguments), &args); err != nil {
			return "", err
		}
		var content []byte
		err := tx.QueryRowContext(ctx, `SELECT content FROM artifacts WHERE system_id=? AND goal_id=? AND artifact_id=?`,
			task.SystemID, task.GoalID, args.ID).Scan(&content)
		if errors.Is(err, sql.ErrNoRows) {
			return "", Invalid("artifact", "not found in this goal")
		}
		if err != nil {
			return "", err
		}
		if len(content) > 3072 || !utf8.Valid(content) {
			return "", Invalid("artifact", "exceeds the bounded text-reader contract")
		}
		return ToolOutcome(map[string]string{"content": string(content)})
	default:
		return "", Invalid("tool", "unsupported bootstrap operation")
	}
}
