//go:build darwin || linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
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

func budgetAgents(ctx context.Context, tx *sql.Tx, e executionRecord) (ids []string, err error) {
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

func checkTaskAdmission(ctx context.Context, tx *sql.Tx, cfg configuration, e executionRecord) (string, error) {
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
	if t.Control == "stopped" || a.State == "stopped" || goalControl == "stopped" || taskTerminal(t.State) {
		return "task or agent stopped", nil
	}
	// A claimed activation may finish while paused; pause prevents the next claim.
	if err := authorizePins(ctx, tx, cfg, e.SystemID, t.Tools); err != nil {
		var field *fieldError
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

func chargeAgents(ctx context.Context, tx *sql.Tx, e executionRecord, reserved, used int64) error {
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

func prepareNextCall(ctx context.Context, tx *sql.Tx, cfg configuration, t taskRecord, owner string, now time.Time) error {
	record, err := readSystem(ctx, tx, localAdministrator, t.SystemID)
	if err != nil {
		return err
	}
	record, err = cfg.inspect(record)
	if err != nil {
		return err
	}
	if record.BlockedReason != "" {
		return invalid("grants", record.BlockedReason)
	}
	a, err := readAgent(ctx, tx, t.SystemID, t.AgentID, t.Revision)
	if err != nil {
		return err
	}
	if a.State == "stopped" || t.Control == "stopped" {
		return errExecutionConflict
	}
	if err := authorizePins(ctx, tx, cfg, t.SystemID, t.Tools); err != nil {
		return err
	}
	var turns int
	if err := tx.QueryRowContext(ctx, `SELECT turns FROM agent_usage WHERE system_id=? AND agent_id=? AND goal_id=?`, t.SystemID, t.AgentID, t.GoalID).Scan(&turns); err != nil {
		return err
	}
	if turns >= a.MaxTurns || t.Turns >= maxTaskTurns {
		return invalid("turns", "agent/task turn limit exhausted")
	}
	record.Configuration.Operator = a.Definition
	record.Grants.Model, record.Grants.SandboxProfile, record.Grants.OperatorTools = a.Definition.Model, a.Definition.SandboxProfile, t.Tools
	body, reservation, err := conversationRequest(cfg, record, t.Conversation, false)
	if err != nil {
		return err
	}
	var budget, used, reserved, deadline int64
	if err := tx.QueryRowContext(ctx, `SELECT token_budget,used_tokens,reserved_tokens,deadline FROM goals WHERE system_id=? AND goal_id=?`, t.SystemID, t.GoalID).
		Scan(&budget, &used, &reserved, &deadline); err != nil {
		return err
	}
	if now.UnixMilli() >= deadline {
		return invalid("goal", "goal lifetime exhausted")
	}
	if reservation > budget-used-reserved || reservation > record.RemainingTokens {
		return invalid("budget", "goal or system budget exhausted")
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
			return invalid("quota", "continuation cannot fit model queue/token capacity")
		}
	}
	callID, err := newID("call_")
	if err != nil {
		return err
	}
	activationID, err := newID("activation_")
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
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET call_id=?,turns=turns+1,state='queued',conversation=?,waiting_tool='',reason=''
	 WHERE system_id=? AND task_id=?`, callID, conversation, t.SystemID, t.ID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE agent_usage SET turns=turns+1 WHERE system_id=? AND agent_id=? AND goal_id=?`, t.SystemID, a.ID, t.GoalID)
	return err
}

type proposeAgentArgs struct {
	Name        string   `json:"name"`
	Prompt      string   `json:"prompt"`
	Tools       []string `json:"tools"`
	TokenBudget int64    `json:"token_budget"`
}
type delegateArgs struct {
	AgentID string `json:"agent_id"`
	Prompt  string `json:"prompt"`
}

func (engine *executionEngine) builtin(ctx context.Context, tx *sql.Tx, e executionRecord, t taskRecord, name, arguments string) (string, error) {
	switch name {
	case "runtime.agent.list":
		var args struct {
			After string `json:"after"`
		}
		if err := decodeJSON([]byte(arguments), &args); err != nil {
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
			result = append(result, map[string]any{"agent_id": a.ID, "name": a.Name, "state": a.State, "available": a.Available, "model": a.Definition.Model, "sandbox_profile": a.Definition.SandboxProfile, "tools": names, "tool_count": len(a.Tools)})
		}
		return toolOutcome(map[string]any{"agents": result, "next": next})
	case "runtime.agent.propose":
		var args proposeAgentArgs
		if err := decodeJSON([]byte(arguments), &args); err != nil {
			return "", err
		}
		if !configName.MatchString(args.Name) || strings.TrimSpace(args.Prompt) == "" || len(args.Prompt) > 4096 || args.TokenBudget < 1 {
			return "", invalid("agent", "invalid name, prompt, or token cap")
		}
		if err := uniqueNames(args.Tools, "tools"); err != nil {
			return "", err
		}
		parent, err := readAgent(ctx, tx, t.SystemID, t.AgentID, t.Revision)
		if err != nil {
			return "", err
		}
		if args.TokenBudget > parent.TokenBudget || parent.Depth >= 8 {
			return "", invalid("agent", "child exceeds parent budget or depth")
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
			return "", invalid("agent", "system agent capacity exhausted")
		}
		pins := make([]toolPin, 0, len(args.Tools))
		for _, name := range args.Tools {
			found := false
			for _, pin := range t.Tools {
				if pin.Name == name {
					pins = append(pins, pin)
					found = true
					break
				}
			}
			if !found {
				return "", invalid("tools", "child cannot expand its creator's task grant")
			}
		}
		if err := authorizePins(ctx, tx, engine.cfg, t.SystemID, pins); err != nil {
			return "", err
		}
		id, err := newID("agent_")
		if err != nil {
			return "", err
		}
		def := parent.Definition
		def.Prompt, def.Tools = args.Prompt, args.Tools
		child := agentRecord{SystemID: t.SystemID, ID: id, Parent: t.AgentID, GoalID: t.GoalID, Name: args.Name, Revision: 1, State: "active", Depth: parent.Depth + 1, TokenBudget: args.TokenBudget, MaxTurns: parent.MaxTurns, Definition: def, Tools: pins}
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
		return toolOutcome(map[string]string{"agent_id": id})
	case "runtime.task.delegate":
		var args delegateArgs
		if err := decodeJSON([]byte(arguments), &args); err != nil {
			return "", err
		}
		if args.AgentID == t.AgentID || strings.TrimSpace(args.Prompt) == "" || len(args.Prompt) > 4096 {
			return "", invalid("delegate", "invalid recipient or prompt")
		}
		recipient, err := readAgent(ctx, tx, t.SystemID, args.AgentID, 0)
		if errors.Is(err, sql.ErrNoRows) {
			return "", invalid("delegate", "recipient unavailable in this scope")
		}
		if err != nil {
			return "", err
		}
		if recipient.GoalID != t.GoalID || recipient.State != "active" {
			return "", invalid("delegate", "recipient belongs to another goal or is unavailable")
		}
		var occupied, count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE system_id=? AND agent_id=? AND state IN ('queued','running','waiting')`, t.SystemID, args.AgentID).Scan(&occupied); err != nil {
			return "", err
		}
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE system_id=? AND goal_id=?`, t.SystemID, t.GoalID).Scan(&count); err != nil {
			return "", err
		}
		if occupied != 0 || count >= maxGoalTasks {
			return "", invalid("delegate", "recipient busy or goal task budget exhausted")
		}
		caller, err := readAgent(ctx, tx, t.SystemID, t.AgentID, t.Revision)
		if err != nil {
			return "", err
		}
		if caller.Definition.Model != recipient.Definition.Model || caller.Definition.SandboxProfile != recipient.Definition.SandboxProfile {
			return "", invalid("delegate", "recipient cannot lend a different model or host profile")
		}
		pins := []toolPin{}
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
		id, err := newID("task_")
		if err != nil {
			return "", err
		}
		tools, err := json.Marshal(pins)
		if err != nil {
			return "", err
		}
		conversation, err := json.Marshal([]chatMessage{{Role: "user", Content: args.Prompt}})
		if err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tasks(system_id,task_id,goal_id,agent_id,agent_revision,parent_task,state,tools,conversation,turns,call_id)
		 VALUES(?,?,?,?,?,?,'queued',?,?,0,'')`, t.SystemID, id, t.GoalID, args.AgentID, recipient.Revision, t.ID, tools, conversation); err != nil {
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
		t.Conversation = append(t.Conversation, chatMessage{Role: "assistant", Content: e.Response, ToolCalls: e.Actions})
		if err := updateConversation(ctx, tx, t); err != nil {
			return "", err
		}
		return toolOutcome(map[string]string{"task_id": id, "state": "queued"})
	case "runtime.task.progress":
		var args struct {
			Message string `json:"message"`
		}
		if err := decodeJSON([]byte(arguments), &args); err != nil {
			return "", err
		}
		if t.Parent == "" || strings.TrimSpace(args.Message) == "" || len(args.Message) > 2048 {
			return "", invalid("progress", "requires a parent and bounded message")
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
		var args draftCommand
		if err := decodeJSON([]byte(arguments), &args); err != nil {
			return "", err
		}
		id, err := submitDraft(ctx, tx, engine.cfg, t.SystemID, t.AgentID, t.GoalID, t.ID, args)
		if err != nil {
			return "", err
		}
		return toolOutcome(map[string]string{"tool_id": id, "state": "draft"})
	}
	return "", invalid("tool", "no reviewed handler")
}

// invokeTool authenticates the pipe session and immutable provider decision, then
// intersects task pins with live revocations. Built-in changes and the receipt
// share a transaction. Subprocess dispatch is marked first and never blindly retried.
func (engine *executionEngine) invokeTool(ctx context.Context, session executionRecord, action modelToolCall) (result string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := engine.store.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer rollback(tx, &err)
	e, err := authorizeCall(ctx, tx, session)
	if err != nil {
		return "", err
	}
	e.EventID = session.EventID
	if e.State != "completed" || len(e.Actions) != 1 || e.Actions[0] != action {
		return "", invalid("tool", "request is not this call's persisted model decision")
	}
	t, err := readTask(ctx, tx, e.SystemID, e.TaskID)
	if err != nil {
		return "", err
	}
	if t.CallID != e.CallID || taskTerminal(t.State) || t.Control == "stopped" {
		return "", errExecutionConflict
	}
	if time.Now().UnixMilli() >= e.Deadline {
		return "", invalid("session", "activation deadline expired")
	}
	var leased int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mailboxes WHERE system_id=? AND task_id=? AND event_id=? AND state='leased' AND lease_owner=?`,
		t.SystemID, t.ID, session.EventID, engine.owner).Scan(&leased); err != nil {
		return "", err
	}
	if leased != 1 {
		return "", invalid("session", "tool invocation has no live mailbox lease")
	}
	var systemState, goalControl string
	if err := tx.QueryRowContext(ctx, `SELECT s.state,g.control FROM systems s JOIN goals g USING(system_id) WHERE s.system_id=? AND g.goal_id=?`, t.SystemID, t.GoalID).Scan(&systemState, &goalControl); err != nil {
		return "", err
	}
	if systemState == "stopping" || systemState == "stopped" || goalControl == "stopped" {
		return "", errExecutionConflict
	}
	if err := authorizePins(ctx, tx, engine.cfg, e.SystemID, t.Tools); err != nil {
		return "", err
	}
	pin, err := resolveTool(engine.cfg, t.Tools, action.Function.Name)
	if err != nil {
		return "", err
	}
	definition, _ := engine.cfg.tool(pin.Name)
	schema, err := executableSchema(pin.Name, definition)
	if err != nil {
		return "", err
	}
	if err := validateArguments(schema.Function, action.Function.Arguments); err != nil {
		return "", err
	}
	hash, _, err := jsonDigest(action)
	if err != nil {
		return "", err
	}
	var savedHash, state string
	err = tx.QueryRowContext(ctx, `SELECT arguments_hash,state,result FROM tool_calls WHERE system_id=? AND call_id=? AND tool_call_id=?`, e.SystemID, e.CallID, action.ID).Scan(&savedHash, &state, &result)
	if err == nil {
		if hash != savedHash {
			return "", errCommandConflict
		}
		if state != "completed" {
			return "", invalid("tool", "prior tool outcome is not safely replayable")
		}
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if len(action.Function.Arguments) > maxEventBytes {
		return "", invalid("arguments", "tool arguments exceed limit")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tool_calls(system_id,call_id,tool_call_id,task_id,name,version,arguments_hash,state)
	 VALUES(?,?,?,?,?,?,?,'running')`, e.SystemID, e.CallID, action.ID, t.ID, pin.Name, pin.Version, hash); err != nil {
		return "", err
	}
	if pin.Name != "runtime.text.analyze" {
		result, err = engine.builtin(ctx, tx, e, t, pin.Name, action.Function.Arguments)
		if err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tool_calls SET state='completed',result=? WHERE system_id=? AND call_id=? AND tool_call_id=?`, result, e.SystemID, e.CallID, action.ID); err != nil {
			return "", err
		}
		if err := auditExecution(ctx, tx, e.SystemID, t.AgentID, pin.Name, e.Revision, time.Now()); err != nil {
			return "", err
		}
		return result, tx.Commit()
	}
	var args textArguments
	if err := decodeJSON([]byte(action.Function.Arguments), &args); err != nil {
		return "", err
	}
	if len(args.Text) > 4096 {
		return "", invalid("text", "exceeds 4096 bytes")
	}
	a, err := readAgent(ctx, tx, t.SystemID, t.AgentID, t.Revision)
	if err != nil {
		return "", err
	}
	profile := engine.cfg.SandboxProfiles[a.Definition.SandboxProfile]
	writable := false
	for _, path := range profile.ReadWrite {
		if path == "output" {
			writable = true
		}
	}
	if args.Save && !writable {
		return "", invalid("artifact", "task profile does not permit writing output")
	}
	if err := auditExecution(ctx, tx, e.SystemID, t.AgentID, "tool.dispatch", e.Revision, time.Now()); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	output, artifact, runErr := engine.runTextTool(ctx, e, args, profile)
	finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	finish, err := engine.store.db.BeginTx(finishCtx, nil)
	if err != nil {
		return "", err
	}
	defer rollback(finish, &err)
	if runErr != nil {
		if _, err := finish.ExecContext(finishCtx, `UPDATE tool_calls SET state='unknown',result='subprocess outcome unknown; not retried'
		 WHERE system_id=? AND call_id=? AND tool_call_id=?`, e.SystemID, e.CallID, action.ID); err != nil {
			return "", err
		}
		return "", errors.Join(runErr, finish.Commit())
	}
	if artifact != nil {
		id, err := newID("artifact_")
		if err != nil {
			return "", err
		}
		if _, err := finish.ExecContext(finishCtx, `INSERT INTO artifacts(system_id,artifact_id,goal_id,task_id,digest,content) VALUES(?,?,?,?,?,?)`,
			e.SystemID, id, e.GoalID, e.TaskID, artifactDigest(artifact), artifact); err != nil {
			return "", err
		}
		output.Artifact = id
	}
	result, err = toolOutcome(output)
	if err != nil {
		return "", err
	}
	if _, err := finish.ExecContext(finishCtx, `UPDATE tool_calls SET state='completed',result=? WHERE system_id=? AND call_id=? AND tool_call_id=?`,
		result, e.SystemID, e.CallID, action.ID); err != nil {
		return "", err
	}
	if err := auditExecution(finishCtx, finish, e.SystemID, t.AgentID, "tool.completed", e.Revision, time.Now()); err != nil {
		return "", err
	}
	return result, finish.Commit()
}

func updateConversation(ctx context.Context, tx *sql.Tx, t taskRecord) error {
	data, err := json.Marshal(t.Conversation)
	if err != nil {
		return err
	}
	if len(data) > maxFrame {
		return invalid("conversation", "continuation exceeds 64 KiB")
	}
	_, err = tx.ExecContext(ctx, `UPDATE tasks SET conversation=? WHERE system_id=? AND task_id=?`, data, t.SystemID, t.ID)
	return err
}

func terminateTask(ctx context.Context, tx *sql.Tx, t taskRecord, state, response, reason, source, cause string, now time.Time) error {
	if taskTerminal(t.State) {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state=?,response=?,reason=? WHERE system_id=? AND task_id=?`, state, response, reason, t.SystemID, t.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE model_calls SET state=?,reason=? WHERE system_id=? AND task_id=? AND state IN ('awaiting_worker','queued')`,
		state, reason, t.SystemID, t.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mailboxes SET state='dead',reason=? WHERE system_id=? AND task_id=? AND state='pending'`, "task is terminal: "+state, t.SystemID, t.ID); err != nil {
		return err
	}
	if t.Parent != "" {
		parent, err := readTask(ctx, tx, t.SystemID, t.Parent)
		if err != nil {
			return err
		}
		if !taskTerminal(parent.State) && parent.Control != "stopped" {
			payload := map[string]string{"task_id": t.ID, "state": state}
			if _, err := emitEvent(ctx, tx, parent, "task.result", source, cause, payload, now); err != nil {
				if !deniedTask(err) {
					return err
				}
				// Completion must remain durable even if no further wakeup fits.
				// Fail the waiting continuation explicitly, never the shared broker.
				return terminateTask(ctx, tx, parent, "rejected", "", "child result delivery rejected: "+err.Error(), "runtime", cause, now)
			}
		}
	} else {
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

func appendToolResult(t *taskRecord, e executionRecord, result string) {
	t.Conversation = append(t.Conversation, chatMessage{Role: "assistant", Content: e.Response, ToolCalls: e.Actions},
		chatMessage{Role: "tool", Content: result, ToolCallID: e.Actions[0].ID})
}

func deniedTask(err error) bool {
	var field *fieldError
	return errors.As(err, &field) || errors.Is(err, errExecutionConflict)
}

func taskFailureReason(err error) string {
	if deniedTask(err) {
		return err.Error()
	}
	return "activation failed; inspect daemon diagnostics"
}
