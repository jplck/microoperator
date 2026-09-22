package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jplck/microoperator/internal/protocol"
)

// delegationLedger includes both creation ancestry and delegation ancestry.
// A peer's broader allowance therefore cannot finance a narrower caller's task.
const delegationLedger = `WITH RECURSIVE lineage(agent_id,parent_id) AS (
 SELECT agent_id,parent_id FROM agents WHERE system_id=? AND agent_id=?
 UNION SELECT a.agent_id,a.parent_id FROM agents a JOIN lineage l ON a.agent_id=l.parent_id WHERE a.system_id=?
), parents(task_id,parent_task,agent_id) AS (
 SELECT task_id,parent_task,agent_id FROM tasks WHERE system_id=? AND task_id=?
 UNION SELECT t.task_id,t.parent_task,t.agent_id FROM tasks t JOIN parents p ON t.task_id=p.parent_task WHERE t.system_id=?
) SELECT agent_id FROM lineage UNION SELECT agent_id FROM parents`

func budgetAgents(ctx context.Context, tx *sql.Tx, e ExecutionRecord) (ids []string, err error) {
	rows, err := tx.QueryContext(ctx, delegationLedger, e.SystemID, e.AgentID, e.SystemID, e.SystemID, e.TaskID, e.SystemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func checkTaskAdmission(ctx context.Context, tx *sql.Tx, cfg Configuration, e ExecutionRecord) (string, error) {
	t, err := readTask(ctx, tx, e.SystemID, e.TaskID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	a, err := readAgent(ctx, tx, t.SystemID, t.AgentID, t.Revision)
	if err != nil {
		return "", err
	}
	var goalControl string
	if err := tx.QueryRowContext(ctx, `SELECT control FROM goals WHERE system_id=? AND goal_id=?`, e.SystemID, e.GoalID).Scan(&goalControl); err != nil {
		return "", err
	}
	if t.Control == "stopped" || a.State == "stopped" || goalControl == "stopped" || TaskTerminal(t.State) {
		return "task or agent stopped", nil
	}
	record, err := readSystem(ctx, tx, localAdministrator, e.SystemID)
	if err != nil {
		return "", err
	}
	if e.Model != a.Definition.Model {
		return "model does not match pinned agent revision", nil
	}
	if err := cfg.authorizeModel(record.Configuration, e.Model); err != nil {
		return err.Error(), nil
	}
	if t.LearningID != "" {
		evaluation, err := readEvaluation(ctx, tx, t.SystemID, t.LearningID)
		if err == nil {
			err = learningEligible(ctx, tx, cfg, t.SystemID, evaluation, time.Now())
		}
		if err != nil {
			if DeniedTask(err) {
				return err.Error(), nil
			}
			return "", err
		}
	}
	// A claimed activation may finish while paused; pause prevents the next claim.
	if err := authorizePins(ctx, tx, cfg, e.SystemID, t.Tools); err != nil {
		var field *FieldError
		if errors.As(err, &field) {
			return err.Error(), nil
		}
		return "", err
	}
	ids, err := budgetAgents(ctx, tx, e)
	if err != nil {
		return "", err
	}
	for _, id := range ids {
		var budget, used, reserved int64
		if err := tx.QueryRowContext(ctx, `SELECT a.token_budget,u.used_tokens,u.reserved_tokens FROM agents a JOIN agent_usage u
		 ON a.system_id=u.system_id AND a.agent_id=u.agent_id WHERE a.system_id=? AND a.agent_id=? AND u.goal_id=?`, e.SystemID, id, e.GoalID).
			Scan(&budget, &used, &reserved); err != nil {
			return "", err
		}
		if e.Reservation > budget-used-reserved {
			return "agent or ancestor token budget exhausted", nil
		}
	}
	return "", nil
}

func chargeAgents(ctx context.Context, tx *sql.Tx, e ExecutionRecord, reserved, used int64) error {
	ids, err := budgetAgents(ctx, tx, e)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE agent_usage SET reserved_tokens=reserved_tokens+?,used_tokens=used_tokens+?
		 WHERE system_id=? AND agent_id=? AND goal_id=?`, reserved, used, e.SystemID, id, e.GoalID); err != nil {
			return err
		}
	}
	return nil
}

func prepareNextCall(ctx context.Context, tx *sql.Tx, cfg Configuration, t TaskRecord, owner string, now time.Time) error {
	record, err := readSystem(ctx, tx, localAdministrator, t.SystemID)
	if err != nil {
		return err
	}
	record, err = cfg.Inspect(record)
	if err != nil {
		return err
	}
	if record.BlockedReason != "" {
		return Invalid("grants", record.BlockedReason)
	}
	a, err := readAgent(ctx, tx, t.SystemID, t.AgentID, t.Revision)
	if err != nil {
		return err
	}
	if a.State == "stopped" || t.Control == "stopped" {
		return ErrExecutionConflict
	}
	if err := cfg.authorizeModel(record.Configuration, a.Definition.Model); err != nil {
		return err
	}
	if err := authorizePins(ctx, tx, cfg, t.SystemID, t.Tools); err != nil {
		return err
	}
	var turns int
	if err := tx.QueryRowContext(ctx, `SELECT turns FROM agent_usage WHERE system_id=? AND agent_id=? AND goal_id=?`, t.SystemID, t.AgentID, t.GoalID).Scan(&turns); err != nil {
		return err
	}
	continuous, err := goalIsContinuous(ctx, tx, t)
	if err != nil {
		return err
	}
	if (!continuous && turns >= a.MaxTurns) || t.Turns >= MaxTaskTurns {
		return Invalid("turns", "agent/task turn limit exhausted")
	}
	record.Configuration.Operator = a.Definition
	record.Grants.Model, record.Grants.SandboxProfile, record.Grants.OperatorTools = a.Definition.Model, a.Definition.SandboxProfile, t.Tools
	body, reservation, err := ConversationRequest(cfg, record, t.Conversation, false)
	if err != nil {
		return err
	}
	var budget, used, reserved, deadline, lifetime int64
	if err := tx.QueryRowContext(ctx, `SELECT token_budget,used_tokens,reserved_tokens,deadline,task_lifetime_seconds FROM goals WHERE system_id=? AND goal_id=?`, t.SystemID, t.GoalID).
		Scan(&budget, &used, &reserved, &deadline, &lifetime); err != nil {
		return err
	}
	if continuous {
		deadline = t.Deadline
		if deadline == 0 {
			deadline = cfg.taskDeadline(a.Definition.Model, lifetime, now)
		}
	}
	if now.UnixMilli() >= deadline {
		return Invalid("deadline", "task or goal lifetime exhausted")
	}
	if reservation > budget-used-reserved || reservation > record.RemainingTokens {
		return Invalid("budget", "goal or system budget exhausted")
	}
	wait := int64(3600)
	for _, group := range cfg.Models[a.Definition.Model].QuotaGroups {
		q := cfg.QuotaGroups[group]
		wait = min(wait, q.MaxWaitSeconds)
		var queued int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM call_groups g JOIN model_calls c USING(call_id)
		 WHERE g.group_name=? AND c.state IN ('awaiting_worker','queued')`, group).Scan(&queued); err != nil {
			return err
		}
		if queued >= q.QueueCapacity || reservation > q.TokensPerMinute {
			return Invalid("quota", "continuation cannot fit model queue/token capacity")
		}
	}
	callID, err := NewID("call_")
	if err != nil {
		return err
	}
	activationID, err := NewID("activation_")
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO model_calls(call_id,system_id,goal_id,task_id,activation_id,activation_owner,revision,model,stream,state,
	 reservation,created_at,queued_at,deadline,wait_deadline,agent_id,request) VALUES(?,?,?,?,?,?,?,?,0,'awaiting_worker',?,?,?,?,?,?,?)`,
		callID, t.SystemID, t.GoalID, t.ID, activationID, owner, record.Revision, a.Definition.Model, reservation, now.UnixMilli(), now.UnixMilli(), deadline,
		min(deadline, now.Add(time.Duration(wait)*time.Second).UnixMilli()), a.ID, body); err != nil {
		return err
	}
	for _, group := range cfg.Models[a.Definition.Model].QuotaGroups {
		if _, err := tx.ExecContext(ctx, `INSERT INTO call_groups(call_id,system_id,group_name) VALUES(?,?,?)`, callID, t.SystemID, group); err != nil {
			return err
		}
	}
	conversation, err := json.Marshal(t.Conversation)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET call_id=?,turns=turns+1,state='queued',conversation=?,waiting_tool='',reason='',deadline=?
	 WHERE system_id=? AND task_id=?`, callID, conversation, deadline, t.SystemID, t.ID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE agent_usage SET turns=turns+1 WHERE system_id=? AND agent_id=? AND goal_id=?`, t.SystemID, a.ID, t.GoalID)
	return err
}

type ProposeAgentArgs struct {
	Name        string   `json:"name"`
	Prompt      string   `json:"prompt"`
	Model       string   `json:"model,omitempty"`
	Tools       []string `json:"tools"`
	TokenBudget int64    `json:"token_budget"`
}

type DelegateArgs struct {
	AgentID string `json:"agent_id"`
	Prompt  string `json:"prompt"`
}

func (engine *workflow) builtin(ctx context.Context, tx *sql.Tx, e ExecutionRecord, t TaskRecord, name, arguments string) (string, error) {
	switch name {
	case "runtime.model.list", "runtime.agent.revise", "runtime.agent.retire":
		return engine.manageAgent(ctx, tx, t, name, arguments)
	case "runtime.capabilities", "runtime.artifact.put", "runtime.artifact.get":
		return engine.bootstrapTool(ctx, tx, t, name, arguments)
	case "runtime.learning.evaluate":
		var args struct {
			ToolID   string `json:"tool_id"`
			Version  int64  `json:"version"`
			CheckID  string `json:"check_id"`
			Baseline string `json:"baseline_name"`
		}
		if err := protocol.DecodeJSON([]byte(arguments), &args); err != nil {
			return "", err
		}
		command := EvaluateCommand{ToolID: args.ToolID, Version: args.Version, CheckID: args.CheckID, TaskID: t.ID}
		if args.Baseline != "" {
			for _, pin := range t.Tools {
				if pin.Name == args.Baseline {
					copy := pin
					command.Baseline = &copy
				}
			}
			if command.Baseline == nil {
				return "", Invalid("baseline", "not an exact task grant")
			}
		}
		record, err := readSystem(ctx, tx, localAdministrator, t.SystemID)
		if err != nil {
			return "", err
		}
		value, err := queueLearning(ctx, tx, engine, record, command, t.AgentID, time.Now())
		if err != nil {
			return "", err
		}
		result, err := ToolOutcome(value)
		if err != nil {
			return "", err
		}
		AppendToolResult(&t, e, result)
		if err := updateConversation(ctx, tx, t); err != nil {
			return "", err
		}
		_, err = tx.ExecContext(ctx, `UPDATE tasks SET state='waiting',waiting_tool='',reason='waiting for protected evaluation' WHERE system_id=? AND task_id=?`, t.SystemID, t.ID)
		return result, err
	case "runtime.memory.put", "runtime.memory.search", "runtime.schedule.create", "runtime.schedule.cancel", "runtime.events.subscribe", "runtime.events.unsubscribe", "runtime.task.wait":
		return engine.knowledgeTool(ctx, tx, e, t, name, arguments)
	case "runtime.agent.list":
		var args struct {
			After string `json:"after"`
		}
		if err := protocol.DecodeJSON([]byte(arguments), &args); err != nil {
			return "", err
		}
		rows, err := tx.QueryContext(ctx, `SELECT agent_id FROM agents WHERE system_id=? AND (goal_id=? OR agent_id=(SELECT operator_id FROM systems WHERE system_id=?)) AND agent_id>? ORDER BY agent_id LIMIT 6`, t.SystemID, t.GoalID, t.SystemID, args.After)
		if err != nil {
			return "", err
		}
		ids, err := readIDs(rows)
		if err != nil {
			return "", err
		}
		next := ""
		if len(ids) > 5 {
			ids = ids[:5]
			next = ids[4]
		}
		result := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			a, err := readAgent(ctx, tx, t.SystemID, id, 0)
			if err != nil {
				return "", err
			}
			names := []string{}
			for _, pin := range a.Tools {
				if len(names) < 16 {
					names = append(names, pin.Name)
				}
			}
			result = append(result, map[string]any{"agent_id": a.ID, "name": a.Name, "revision": a.Revision, "state": a.State, "available": a.Available, "model": a.Definition.Model, "sandbox_profile": a.Definition.SandboxProfile, "tools": names, "tool_count": len(a.Tools)})
		}
		return ToolOutcome(map[string]any{"agents": result, "next": next})
	case "runtime.agent.propose":
		var args ProposeAgentArgs
		if err := protocol.DecodeJSON([]byte(arguments), &args); err != nil {
			return "", err
		}
		if !ConfigName.MatchString(args.Name) || strings.TrimSpace(args.Prompt) == "" || len(args.Prompt) > 4096 || args.TokenBudget < 1 {
			return "", Invalid("agent", "invalid name, prompt, or token cap")
		}
		if err := UniqueNames(args.Tools, "tools"); err != nil {
			return "", err
		}
		parent, err := readAgent(ctx, tx, t.SystemID, t.AgentID, t.Revision)
		if err != nil {
			return "", err
		}
		if args.TokenBudget > parent.TokenBudget || parent.Depth >= 8 {
			return "", Invalid("agent", "child exceeds parent budget or depth")
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agents WHERE system_id=? AND state!='stopped'`, t.SystemID).Scan(&count); err != nil {
			return "", err
		}
		record, err := readSystem(ctx, tx, localAdministrator, t.SystemID)
		if err != nil {
			return "", err
		}
		if int64(count) >= record.Configuration.Limits.MaxAgents {
			return "", Invalid("agent", "system agent capacity exhausted")
		}
		if args.Model == "" {
			args.Model = parent.Definition.Model
		}
		if err := engine.cfg.authorizeModel(record.Configuration, args.Model); err != nil {
			return "", err
		}
		pins, err := narrowToolPins(t.Tools, args.Tools)
		if err != nil {
			return "", err
		}
		if err := authorizePins(ctx, tx, engine.cfg, t.SystemID, pins); err != nil {
			return "", err
		}
		id, err := NewID("agent_")
		if err != nil {
			return "", err
		}
		def := parent.Definition
		def.Prompt, def.Model, def.Tools = args.Prompt, args.Model, args.Tools
		child := AgentRecord{SystemID: t.SystemID, ID: id, Parent: t.AgentID, GoalID: t.GoalID, Name: args.Name, Revision: 1, State: "active", Depth: parent.Depth + 1, TokenBudget: args.TokenBudget, MaxTurns: parent.MaxTurns, Definition: def, Tools: pins}
		if _, err := tx.ExecContext(ctx, `INSERT INTO agents(system_id,agent_id,parent_id,goal_id,name,revision,state,depth,token_budget,max_turns)
		 VALUES(?,?,?,?,?,1,'active',?,?,?)`, t.SystemID, id, t.AgentID, t.GoalID, args.Name, child.Depth, args.TokenBudget, child.MaxTurns); err != nil {
			return "", err
		}
		if err := insertAgentRevision(ctx, tx, child); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_usage(system_id,agent_id,goal_id) VALUES(?,?,?)`, t.SystemID, id, t.GoalID); err != nil {
			return "", err
		}
		return ToolOutcome(map[string]string{"agent_id": id})
	case "runtime.task.delegate":
		var args DelegateArgs
		if err := protocol.DecodeJSON([]byte(arguments), &args); err != nil {
			return "", err
		}
		if args.AgentID == t.AgentID || strings.TrimSpace(args.Prompt) == "" || len(args.Prompt) > 4096 {
			return "", Invalid("delegate", "invalid recipient or prompt")
		}
		recipient, err := readAgent(ctx, tx, t.SystemID, args.AgentID, 0)
		if errors.Is(err, sql.ErrNoRows) {
			return "", Invalid("delegate", "recipient unavailable in this scope")
		}
		if err != nil {
			return "", err
		}
		if recipient.GoalID != t.GoalID || recipient.State != "active" {
			return "", Invalid("delegate", "recipient belongs to another goal or is unavailable")
		}
		var occupied, count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE system_id=? AND agent_id=? AND state IN ('queued','running','waiting')`, t.SystemID, args.AgentID).Scan(&occupied); err != nil {
			return "", err
		}
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE system_id=? AND goal_id=? AND
		 ((SELECT continuous FROM goals WHERE system_id=? AND goal_id=?)=0 OR state IN ('queued','running','waiting'))`,
			t.SystemID, t.GoalID, t.SystemID, t.GoalID).Scan(&count); err != nil {
			return "", err
		}
		if occupied != 0 || count >= MaxGoalTasks {
			return "", Invalid("delegate", "recipient busy or goal task budget exhausted")
		}
		caller, err := readAgent(ctx, tx, t.SystemID, t.AgentID, t.Revision)
		if err != nil {
			return "", err
		}
		if caller.Definition.SandboxProfile != recipient.Definition.SandboxProfile {
			return "", Invalid("delegate", "recipient cannot lend a different host profile")
		}
		pins := []ToolPin{}
		for _, candidate := range recipient.Tools {
			for _, grant := range t.Tools {
				if candidate == grant {
					pins = append(pins, grant)
				}
			}
		}
		if err := authorizePins(ctx, tx, engine.cfg, t.SystemID, pins); err != nil {
			return "", err
		}
		id, err := NewID("task_")
		if err != nil {
			return "", err
		}
		tools, err := json.Marshal(pins)
		if err != nil {
			return "", err
		}
		conversation, err := json.Marshal([]ChatMessage{{Role: "user", Content: args.Prompt}})
		if err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tasks(system_id,task_id,goal_id,agent_id,agent_revision,parent_task,state,tools,conversation,turns,call_id,deadline)
		 VALUES(?,?,?,?,?,?,'queued',?,?,0,'',?)`, t.SystemID, id, t.GoalID, args.AgentID, recipient.Revision, t.ID, tools, conversation, t.Deadline); err != nil {
			return "", err
		}
		child, err := readTask(ctx, tx, t.SystemID, id)
		if err != nil {
			return "", err
		}
		if err := prepareNextCall(ctx, tx, engine.cfg, child, engine.owner, time.Now()); err != nil {
			return "", err
		}
		if _, err := emitEvent(ctx, tx, child, "task.ready", t.AgentID, e.EventID, struct{}{}, time.Now()); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state='waiting',waiting_tool=?,reason='awaiting delegated task result' WHERE system_id=? AND task_id=?`, e.Actions[0].ID, t.SystemID, t.ID); err != nil {
			return "", err
		}
		t.Conversation = append(t.Conversation, ChatMessage{Role: "assistant", Content: e.Response, ToolCalls: e.Actions})
		if err := updateConversation(ctx, tx, t); err != nil {
			return "", err
		}
		return ToolOutcome(map[string]string{"task_id": id, "state": "queued"})
	case "runtime.task.progress":
		var args struct {
			Message string `json:"message"`
		}
		if err := protocol.DecodeJSON([]byte(arguments), &args); err != nil {
			return "", err
		}
		if t.Parent == "" || strings.TrimSpace(args.Message) == "" || len(args.Message) > 2048 {
			return "", Invalid("progress", "requires a parent and bounded message")
		}
		parent, err := readTask(ctx, tx, t.SystemID, t.Parent)
		if err != nil {
			return "", err
		}
		if _, err := emitEvent(ctx, tx, parent, "task.progress", t.AgentID, e.EventID, args, time.Now()); err != nil {
			return "", err
		}
		return `{"recorded":true}`, nil
	case "runtime.tool.propose":
		var args DraftCommand
		if err := protocol.DecodeJSON([]byte(arguments), &args); err != nil {
			return "", err
		}
		id, err := submitDraft(ctx, tx, engine.cfg, t.SystemID, t.AgentID, t.GoalID, t.ID, args)
		if err != nil {
			return "", err
		}
		return ToolOutcome(map[string]string{"tool_id": id, "state": "draft"})
	}
	return "", Invalid("tool", "no reviewed handler")
}

func updateConversation(ctx context.Context, tx *sql.Tx, t TaskRecord) error {
	data, err := json.Marshal(t.Conversation)
	if err != nil {
		return err
	}
	if len(data) > protocol.MaxFrame {
		return Invalid("conversation", "continuation exceeds 64 KiB")
	}
	_, err = tx.ExecContext(ctx, `UPDATE tasks SET conversation=? WHERE system_id=? AND task_id=?`, data, t.SystemID, t.ID)
	return err
}

func terminateTask(ctx context.Context, tx *sql.Tx, t TaskRecord, state, response, reason, source, cause string, now time.Time) error {
	if TaskTerminal(t.State) {
		return nil
	}
	continuous, err := goalIsContinuous(ctx, tx, t)
	if err != nil {
		return err
	}
	if !continuous || state != "completed" || t.LearningID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE schedules SET state='canceled',reason='owning task is terminal' WHERE system_id=? AND task_id=? AND state='active'`, t.SystemID, t.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE subscriptions SET state='canceled',reason='owning task is terminal' WHERE system_id=? AND task_id=? AND state='active'`, t.SystemID, t.ID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state=?,response=?,reason=? WHERE system_id=? AND task_id=?`, state, response, reason, t.SystemID, t.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE model_calls SET state=?,reason=? WHERE system_id=? AND task_id=? AND state IN ('awaiting_worker','queued')`,
		state, reason, t.SystemID, t.ID); err != nil {
		return err
	}
	if continuous && state == "completed" && t.LearningID == "" {
		completed := t
		completed.State, completed.Response, completed.Reason = state, response, reason
		if err := continuePendingInput(ctx, tx, completed); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mailboxes SET state='dead',reason=? WHERE system_id=? AND task_id=? AND state='pending'`, "task is terminal: "+state, t.SystemID, t.ID); err != nil {
		return err
	}
	if continuous {
		if _, err := tx.ExecContext(ctx, `UPDATE goals SET state=CASE WHEN EXISTS
		 (SELECT 1 FROM tasks WHERE system_id=? AND goal_id=? AND state IN ('queued','running','waiting'))
		 THEN 'running' ELSE 'waiting' END WHERE system_id=? AND goal_id=? AND control!='stopped'`,
			t.SystemID, t.GoalID, t.SystemID, t.GoalID); err != nil {
			return err
		}
	}
	if t.LearningID != "" {
		return nil
	}
	if t.Parent != "" {
		parent, err := readTask(ctx, tx, t.SystemID, t.Parent)
		if err != nil {
			return err
		}
		if !TaskTerminal(parent.State) && parent.Control != "stopped" {
			payload := map[string]string{"task_id": t.ID, "state": state}
			if _, err := emitEvent(ctx, tx, parent, "task.result", source, cause, payload, now); err != nil {
				if !DeniedTask(err) {
					return err
				}
				// Completion must remain durable even if no further wakeup fits.
				// Fail the waiting continuation explicitly, never the shared broker.
				return terminateTask(ctx, tx, parent, "rejected", "", "child result delivery rejected: "+err.Error(), "runtime", cause, now)
			}
		}
	} else {
		if continuous && state != "canceled" {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE goals SET state=? WHERE system_id=? AND goal_id=?`, state, t.SystemID, t.GoalID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE systems SET state=CASE WHEN state IN ('stopping','stopped')
		 THEN CASE WHEN EXISTS(SELECT 1 FROM mailboxes WHERE system_id=? AND state='leased') THEN 'stopping' ELSE 'stopped' END
		 ELSE 'inactive' END WHERE system_id=?`, t.SystemID, t.SystemID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agents SET state='stopped' WHERE system_id=? AND goal_id=?`, t.SystemID, t.GoalID); err != nil {
			return err
		}
	}
	return nil
}

func AppendToolResult(t *TaskRecord, e ExecutionRecord, result string) {
	t.Conversation = append(t.Conversation, ChatMessage{Role: "assistant", Content: e.Response, ToolCalls: e.Actions},
		ChatMessage{Role: "tool", Content: result, ToolCallID: e.Actions[0].ID})
}

func DeniedTask(err error) bool {
	var field *FieldError
	return errors.As(err, &field) || errors.Is(err, ErrExecutionConflict)
}

func TaskFailureReason(err error) string {
	if DeniedTask(err) {
		return err.Error()
	}
	return "activation failed; inspect daemon diagnostics"
}
