package state

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type PreparedTool struct {
	Dispatch  bool
	Result    string
	Generated bool
	Arguments TextArguments
	Profile   SandboxConfig
	Binary    []byte
	execution ExecutionRecord
	action    ModelToolCall
	task      TaskRecord
}

func (store *Store) PrepareTool(ctx context.Context, cfg Configuration, owner string, session ExecutionRecord, action ModelToolCall) (plan PreparedTool, err error) {
	engine := &workflow{store: store, cfg: cfg, owner: owner}
	var result string
	tx, err := engine.store.db.BeginTx(ctx, nil)
	if err != nil {
		return plan, err
	}
	defer rollback(tx, &err)
	e, err := authorizeCall(ctx, tx, session)
	if err != nil {
		return plan, err
	}
	e.EventID = session.EventID
	if e.State != "completed" || len(e.Actions) != 1 || e.Actions[0] != action {
		return plan, Invalid("tool", "request is not this call's persisted model decision")
	}
	t, err := readTask(ctx, tx, e.SystemID, e.TaskID)
	if err != nil {
		return plan, err
	}
	if t.CallID != e.CallID || TaskTerminal(t.State) || t.Control == "stopped" {
		return plan, ErrExecutionConflict
	}
	if time.Now().UnixMilli() >= e.Deadline {
		return plan, Invalid("session", "activation deadline expired")
	}
	var leased int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mailboxes WHERE system_id=? AND task_id=? AND event_id=? AND state='leased' AND lease_owner=?`,
		t.SystemID, t.ID, session.EventID, engine.owner).Scan(&leased); err != nil {
		return plan, err
	}
	if leased != 1 {
		return plan, Invalid("session", "tool invocation has no live mailbox lease")
	}
	var systemState, goalControl string
	if err := tx.QueryRowContext(ctx, `SELECT s.state,g.control FROM systems s JOIN goals g USING(system_id) WHERE s.system_id=? AND g.goal_id=?`, t.SystemID, t.GoalID).Scan(&systemState, &goalControl); err != nil {
		return plan, err
	}
	if systemState == "stopping" || systemState == "stopped" || goalControl == "stopped" {
		return plan, ErrExecutionConflict
	}
	if err := authorizePins(ctx, tx, engine.cfg, e.SystemID, t.Tools); err != nil {
		return plan, err
	}
	locals, err := loadLocalTools(ctx, tx, e.SystemID, t.Tools)
	if err != nil {
		return plan, err
	}
	cfg = engine.cfg.WithLocalTools(locals)
	pin, err := ResolveTool(cfg, t.Tools, action.Function.Name)
	if err != nil {
		return plan, err
	}
	definition, _ := cfg.Tool(pin.Name)
	schema, err := ExecutableSchema(pin.Name, definition)
	if err != nil {
		return plan, err
	}
	if err := ValidateArguments(schema.Function, action.Function.Arguments); err != nil {
		return plan, err
	}
	hash, _, err := JsonDigest(action)
	if err != nil {
		return plan, err
	}
	var savedHash, state string
	err = tx.QueryRowContext(ctx, `SELECT arguments_hash,state,result FROM tool_calls WHERE system_id=? AND call_id=? AND tool_call_id=?`, e.SystemID, e.CallID, action.ID).Scan(&savedHash, &state, &result)
	if err == nil {
		if hash != savedHash {
			return plan, ErrCommandConflict
		}
		if state != "completed" {
			return plan, Invalid("tool", "prior tool outcome is not safely replayable")
		}
		return PreparedTool{Result: result}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return plan, err
	}
	if len(action.Function.Arguments) > MaxEventBytes {
		return plan, Invalid("arguments", "tool arguments exceed limit")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tool_calls(system_id,call_id,tool_call_id,task_id,name,version,arguments_hash,state)
	 VALUES(?,?,?,?,?,?,?,'running')`, e.SystemID, e.CallID, action.ID, t.ID, pin.Name, pin.Version, hash); err != nil {
		return plan, err
	}
	generated := strings.HasPrefix(pin.Name, "local.")
	if pin.Name != "runtime.text.analyze" && !generated {
		result, err = engine.builtin(ctx, tx, e, t, pin.Name, action.Function.Arguments)
		if err != nil {
			return plan, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tool_calls SET state='completed',result=? WHERE system_id=? AND call_id=? AND tool_call_id=?`, result, e.SystemID, e.CallID, action.ID); err != nil {
			return plan, err
		}
		if err := auditExecution(ctx, tx, e.SystemID, t.AgentID, pin.Name, e.Revision, time.Now()); err != nil {
			return plan, err
		}
		return PreparedTool{Result: result}, tx.Commit()
	}
	var args TextArguments
	if err := DecodeJSON([]byte(action.Function.Arguments), &args); err != nil {
		return plan, err
	}
	if len(args.Text) > 4096 {
		return plan, Invalid("text", "exceeds 4096 bytes")
	}
	a, err := readAgent(ctx, tx, t.SystemID, t.AgentID, t.Revision)
	if err != nil {
		return plan, err
	}
	profile := engine.cfg.SandboxProfiles[a.Definition.SandboxProfile]
	var binary []byte
	if generated {
		if definition.RuntimeDigest != engine.cfg.RuntimeDigest {
			return plan, Invalid("runtime", "approved confinement implementation changed")
		}
		digest, _, err := JsonDigest(profile)
		if err != nil {
			return plan, err
		}
		if definition.ProfileDigest != digest {
			return plan, Invalid("profile", "approved capabilities do not match this task")
		}
		var used int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM learning_uses WHERE system_id=? AND tool_id=? AND version=? AND task_id=?`, t.SystemID, pin.Name, pin.Version, t.ID).Scan(&used); err != nil {
			return plan, err
		}
		if used != 1 {
			return plan, Invalid("approval", "task has no reserved approval use")
		}
		binary, err = ReadGeneratedArtifact(engine.cfg.DataDir, t.SystemID, definition.BinaryDigest)
		if err != nil {
			return plan, err
		}
	}
	writable := false
	for _, path := range profile.ReadWrite {
		if path == "output" {
			writable = true
		}
	}
	if args.Save && !writable {
		return plan, Invalid("artifact", "task profile does not permit writing output")
	}
	if err := auditExecution(ctx, tx, e.SystemID, t.AgentID, "tool.dispatch", e.Revision, time.Now()); err != nil {
		return plan, err
	}
	if err := tx.Commit(); err != nil {
		return plan, err
	}
	return PreparedTool{Dispatch: true, Generated: generated, Arguments: args, Profile: profile, Binary: binary, execution: e, action: action, task: t}, nil
}
func (store *Store) CompleteTool(ctx context.Context, plan PreparedTool, output TextResult, artifact []byte, generatedOutput string, runErr error) (result string, err error) {
	if !plan.Dispatch || plan.execution.CallID == "" || plan.action.ID == "" {
		return "", Invalid("tool", "completion requires a prepared dispatch")
	}
	e, action, generated, t := plan.execution, plan.action, plan.Generated, plan.task
	finish, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer rollback(finish, &err)
	if runErr != nil {
		if _, err := finish.ExecContext(ctx, `UPDATE tool_calls SET state='unknown',result='subprocess outcome unknown; not retried'
		 WHERE system_id=? AND call_id=? AND tool_call_id=?`, e.SystemID, e.CallID, action.ID); err != nil {
			return "", err
		}
		return "", errors.Join(runErr, finish.Commit())
	}
	if artifact != nil {
		id, err := NewID("artifact_")
		if err != nil {
			return "", err
		}
		if _, err := finish.ExecContext(ctx, `INSERT INTO artifacts(system_id,artifact_id,goal_id,task_id,digest,content) VALUES(?,?,?,?,?,?)`,
			e.SystemID, id, e.GoalID, e.TaskID, ArtifactDigest(artifact), artifact); err != nil {
			return "", err
		}
		output.Artifact = id
	}
	if generated {
		result, err = ToolOutcome(map[string]string{"text": generatedOutput})
	} else {
		result, err = ToolOutcome(output)
	}
	if err != nil {
		return "", err
	}
	if _, err := finish.ExecContext(ctx, `UPDATE tool_calls SET state='completed',result=? WHERE system_id=? AND call_id=? AND tool_call_id=?`,
		result, e.SystemID, e.CallID, action.ID); err != nil {
		return "", err
	}
	if err := auditExecution(ctx, finish, e.SystemID, t.AgentID, "tool.completed", e.Revision, time.Now()); err != nil {
		return "", err
	}
	return result, finish.Commit()
}
