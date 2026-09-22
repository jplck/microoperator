package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type EvaluateCommand struct {
	ToolID   string   `json:"tool_id"`
	Version  int64    `json:"version"`
	CheckID  string   `json:"check_id"`
	TaskID   string   `json:"task_id"`
	Baseline *ToolPin `json:"baseline,omitempty"`
}

type EvaluationBaseline struct {
	Prompt string   `json:"prompt"`
	Tool   *ToolPin `json:"tool,omitempty"`
}

func queueLearning(ctx context.Context, tx *sql.Tx, engine *workflow, s SystemRecord, command EvaluateCommand, source string, now time.Time) (any, error) {
	if s.State != "running" || s.Configuration.Limits.TokenBudget <= s.UsedTokens+s.ReservedTokens {
		return nil, ErrExecutionConflict
	}
	origin, err := readTask(ctx, tx, s.ID, command.TaskID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSystemNotFound
	}
	if err != nil {
		return nil, err
	}
	if TaskTerminal(origin.State) || origin.Control != "active" || origin.LearningID != "" {
		return nil, ErrExecutionConflict
	}
	parent, err := readAgent(ctx, tx, s.ID, origin.AgentID, origin.Revision)
	if err != nil {
		return nil, err
	}
	if parent.State != "active" || parent.Depth >= 8 {
		return nil, ErrExecutionConflict
	}
	var deadline, budget, used, reserved int64
	var control string
	if err := tx.QueryRowContext(ctx, `SELECT deadline,token_budget,used_tokens,reserved_tokens,control FROM goals WHERE system_id=? AND goal_id=?`, s.ID, origin.GoalID).Scan(&deadline, &budget, &used, &reserved, &control); err != nil {
		return nil, err
	}
	if control != "active" || (deadline > 0 && deadline <= now.UnixMilli()) || origin.Deadline <= now.UnixMilli() || budget <= used+reserved {
		return nil, Invalid("goal", "evaluation exceeds remaining goal lifetime or budget")
	}
	draft, err := readLearningDraft(ctx, tx, s.ID, command.ToolID, command.Version)
	if err != nil {
		return nil, err
	}
	var data []byte
	if err := tx.QueryRowContext(ctx, `SELECT cases FROM learning_checks WHERE system_id=? AND check_id=?`, s.ID, command.CheckID).Scan(&data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSystemNotFound
		}
		return nil, err
	}
	cases, err := ProtectedCases(data)
	if err != nil {
		return nil, err
	}
	if err := authorizePins(ctx, tx, engine.cfg, s.ID, origin.Tools); err != nil {
		return nil, err
	}
	profile := engine.cfg.SandboxProfiles[parent.Definition.SandboxProfile]
	profileDigest, _, err := JsonDigest(profile)
	if err != nil {
		return nil, err
	}
	Toolchain := ""
	if draft.Kind == "executable" {
		if len(draft.Requires) != 0 {
			return nil, Invalid("capabilities", "generated tools cannot call brokers or other tools")
		}
		if err := GeneratedProfile(profile); err != nil {
			return nil, err
		}
		if engine.cfg.Learning == nil {
			return nil, Invalid("learning", "a pinned local Go toolchain is required")
		}
		if err := ValidateGeneratedSource(draft.Content); err != nil {
			return nil, err
		}
		Toolchain = engine.cfg.Learning.ToolchainDigest
	}
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM learning_evaluations WHERE system_id=? AND tool_id=? AND version=?`, s.ID, command.ToolID, command.Version).Scan(&existing); err != nil {
		return nil, err
	}
	if existing != 0 {
		return nil, Invalid("evaluation", "this revision was already evaluated; submit a new immutable version")
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM learning_evaluations e JOIN tasks t ON t.system_id=e.system_id AND t.task_id=e.origin_task WHERE e.system_id=? AND t.goal_id=?`, s.ID, origin.GoalID).Scan(&existing); err != nil {
		return nil, err
	}
	if existing >= 4 {
		return nil, Invalid("evaluation", "four evaluations per goal allowed")
	}
	var agents, tasks int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agents WHERE system_id=? AND state!='stopped'`, s.ID).Scan(&agents); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE system_id=? AND goal_id=?`, s.ID, origin.GoalID).Scan(&tasks); err != nil {
		return nil, err
	}
	if agents >= s.Configuration.Limits.MaxAgents || tasks+int64(2*len(cases)) > 64 {
		return nil, Invalid("evaluation", "ordinary agent/task capacity exhausted")
	}
	locals, err := loadLocalTools(ctx, tx, s.ID, origin.Tools)
	if err != nil {
		return nil, err
	}
	cfg := engine.cfg.WithLocalTools(locals)
	baselinePrompt, candidatePrompt := parent.Definition.Prompt, parent.Definition.Prompt
	baselineFound := command.Baseline == nil
	for _, pin := range origin.Tools {
		Tool, _ := cfg.Tool(pin.Name)
		replaced := command.Baseline != nil && pin == *command.Baseline
		if replaced {
			baselineFound = true
			if Tool.Kind != draft.Kind {
				return nil, Invalid("baseline", "baseline kind differs from candidate")
			}
			if draft.Kind == "executable" && (Tool.BinaryDigest == "" || Tool.ProfileDigest != profileDigest) {
				return nil, Invalid("baseline", "requires an approved generated tool with the same capabilities")
			}
		}
		if Tool.Kind == "skill" {
			baselinePrompt += "\n\n" + Tool.Content
			if !replaced {
				candidatePrompt += "\n\n" + Tool.Content
			}
		}
	}
	if !baselineFound {
		return nil, Invalid("baseline", "baseline must be an exact originating task pin")
	}
	candidatePrompt += "\n\n" + draft.Content
	if len(baselinePrompt) > 32768 || (draft.Kind == "skill" && len(candidatePrompt) > 32768) {
		return nil, Invalid("evaluation", "combined prompts exceed context bound")
	}
	for _, required := range draft.Requires {
		found := false
		for _, pin := range origin.Tools {
			if pin.Name == required {
				found = true
			}
		}
		if !found {
			return nil, Invalid("requires_tools", "evaluation cannot expand originating task capabilities")
		}
	}
	id, err := NewID("evaluation_")
	if err != nil {
		return nil, err
	}
	agentID, err := NewID("agent_")
	if err != nil {
		return nil, err
	}
	def := parent.Definition
	def.Tools = []string{}
	def.Prompt = baselinePrompt
	agent := AgentRecord{SystemID: s.ID, ID: agentID, Parent: parent.ID, GoalID: origin.GoalID, Name: "protected-evaluation", Revision: 1, State: "active", Depth: parent.Depth + 1, TokenBudget: min(parent.TokenBudget, budget-used-reserved), MaxTurns: 8, Definition: def, Tools: []ToolPin{}}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agents(system_id,agent_id,parent_id,goal_id,name,revision,state,depth,token_budget,max_turns) VALUES(?,?,?,?,?,1,'active',?,?,8)`, s.ID, agent.ID, parent.ID, origin.GoalID, agent.Name, agent.Depth, agent.TokenBudget); err != nil {
		return nil, err
	}
	if err := insertAgentRevision(ctx, tx, agent); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_usage(system_id,agent_id,goal_id) VALUES(?,?,?)`, s.ID, agent.ID, origin.GoalID); err != nil {
		return nil, err
	}
	if draft.Kind == "skill" {
		agent.Revision = 2
		agent.Definition.Prompt = candidatePrompt
		if err := insertAgentRevision(ctx, tx, agent); err != nil {
			return nil, err
		}
	}
	baseline, err := json.Marshal(EvaluationBaseline{Prompt: baselinePrompt, Tool: command.Baseline})
	if err != nil {
		return nil, err
	}
	state := "queued"
	if draft.Kind == "skill" {
		state = "evaluating"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO learning_evaluations(system_id,evaluation_id,tool_id,version,check_id,origin_task,agent_id,baseline,profile_digest,toolchain_digest,runtime_digest,system_revision,configuration_digest,notify,state)
	 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, s.ID, id, command.ToolID, command.Version, command.CheckID, origin.ID, agent.ID, baseline, profileDigest, Toolchain, engine.cfg.RuntimeDigest, s.Revision, s.Grants.DefinitionsDigest, source != localAdministrator, state); err != nil {
		return nil, err
	}
	for index, check := range cases {
		roles := []string{"baseline", "candidate"}
		if draft.Kind == "executable" {
			roles = []string{"build"}
		}
		for _, role := range roles {
			taskID, err := NewID("task_")
			if err != nil {
				return nil, err
			}
			revision := int64(1)
			if role == "candidate" {
				revision = 2
			}
			conversation, err := json.Marshal([]ChatMessage{{Role: "user", Content: check.Input}})
			if err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO tasks(system_id,task_id,goal_id,agent_id,agent_revision,parent_task,state,tools,conversation,turns,call_id,learning_id,learning_role,deadline)
			 VALUES(?,?,?,?,?,?,'queued','[]',?,0,'',?,?,?)`, s.ID, taskID, origin.GoalID, agent.ID, revision, origin.ID, conversation, id, role+string(rune('0'+index)), origin.Deadline); err != nil {
				return nil, err
			}
			task, err := readTask(ctx, tx, s.ID, taskID)
			if err != nil {
				return nil, err
			}
			kind := "task.ready"
			if role == "build" {
				kind = "tool.evaluate"
			} else if err := prepareNextCall(ctx, tx, engine.cfg, task, engine.owner, now); err != nil {
				return nil, err
			}
			if _, err := emitEvent(ctx, tx, task, kind, source, "", struct{}{}, now); err != nil {
				return nil, err
			}
		}
		if draft.Kind == "executable" {
			break
		}
	}
	return map[string]string{"evaluation_id": id, "state": state}, nil
}
