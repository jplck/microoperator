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

// ponytail: cap reviewed activations at 64; configure this only when measured workloads need more.
const MaxOperators = 64

const schemaV2 = `
CREATE TABLE systems_v2 (
 system_id TEXT PRIMARY KEY, owner TEXT NOT NULL, operator_id TEXT NOT NULL UNIQUE,
 launch TEXT NOT NULL, state TEXT NOT NULL CHECK(state IN ('inactive','running','stopping','stopped')),
 revision INTEGER NOT NULL CHECK(revision > 0),
 used_tokens INTEGER NOT NULL DEFAULT 0 CHECK(used_tokens >= 0),
 reserved_tokens INTEGER NOT NULL DEFAULT 0 CHECK(reserved_tokens >= 0),
 last_admission INTEGER NOT NULL DEFAULT 0,
 created_at TEXT NOT NULL,
 FOREIGN KEY(system_id,revision) REFERENCES system_revisions(system_id,revision) DEFERRABLE INITIALLY DEFERRED
);
INSERT INTO systems_v2(system_id,owner,operator_id,launch,state,revision,used_tokens,created_at)
 SELECT system_id,owner,operator_id,launch,state,revision,used_tokens,created_at FROM systems;
DROP TABLE systems;
ALTER TABLE systems_v2 RENAME TO systems;
CREATE INDEX systems_owner ON systems(owner,system_id);
CREATE TABLE goals_v2 (
 goal_id TEXT PRIMARY KEY, system_id TEXT NOT NULL REFERENCES systems(system_id),
 owner TEXT NOT NULL, prompt TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('pending','queued','running','waiting','completed','failed','canceled','rejected')),
 token_budget INTEGER NOT NULL CHECK(token_budget > 0),
 used_tokens INTEGER NOT NULL DEFAULT 0 CHECK(used_tokens >= 0),
 reserved_tokens INTEGER NOT NULL DEFAULT 0 CHECK(reserved_tokens >= 0),
 is_initial INTEGER NOT NULL DEFAULT 1 CHECK(is_initial IN (0,1))
);
INSERT INTO goals_v2(goal_id,system_id,owner,prompt,state,token_budget)
 SELECT goal_id,system_id,owner,prompt,state,token_budget FROM goals;
DROP TABLE goals;
ALTER TABLE goals_v2 RENAME TO goals;
CREATE UNIQUE INDEX initial_goal ON goals(system_id) WHERE is_initial = 1;
CREATE UNIQUE INDEX scoped_goal ON goals(system_id,goal_id);
CREATE TABLE audit_v2 (
 audit_id INTEGER PRIMARY KEY, system_id TEXT NOT NULL, principal TEXT NOT NULL,
 action TEXT NOT NULL CHECK(action IN ('system.create','system.revise','system.start','system.stop',
 'execution.recover','model.dispatch','model.completed','model.failed','model.unknown','model.queued',
 'task.completed','task.failed','task.canceled','task.rejected')), revision INTEGER NOT NULL, created_at TEXT NOT NULL,
 FOREIGN KEY(system_id,revision) REFERENCES system_revisions(system_id,revision)
);
INSERT INTO audit_v2 SELECT * FROM audit;
DROP TABLE audit;
ALTER TABLE audit_v2 RENAME TO audit;
CREATE TRIGGER immutable_audit_update BEFORE UPDATE ON audit BEGIN SELECT RAISE(ABORT,'audit is append-only'); END;
CREATE TRIGGER immutable_audit_delete BEFORE DELETE ON audit BEGIN SELECT RAISE(ABORT,'audit is append-only'); END;
CREATE TABLE model_calls (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,
 call_id TEXT NOT NULL UNIQUE, system_id TEXT NOT NULL, goal_id TEXT NOT NULL UNIQUE,
 task_id TEXT NOT NULL UNIQUE, activation_id TEXT NOT NULL UNIQUE, activation_owner TEXT NOT NULL,
 revision INTEGER NOT NULL, model TEXT NOT NULL, stream INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('awaiting_worker','queued','running','completed','failed','canceled','rejected','unknown')),
 reason TEXT NOT NULL DEFAULT '', response TEXT NOT NULL DEFAULT '',
 reservation INTEGER NOT NULL CHECK(reservation > 0), attempts INTEGER NOT NULL DEFAULT 0,
 input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
 usage_known INTEGER NOT NULL DEFAULT 0,
 queue_wait_ms INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL, queued_at INTEGER NOT NULL, deadline INTEGER NOT NULL,
 wait_deadline INTEGER NOT NULL, dispatched_at INTEGER NOT NULL DEFAULT 0,
 FOREIGN KEY(system_id,goal_id) REFERENCES goals(system_id,goal_id),
 FOREIGN KEY(system_id,revision) REFERENCES system_revisions(system_id,revision)
);
CREATE INDEX calls_queue ON model_calls(state,sequence);
CREATE INDEX calls_system ON model_calls(system_id,sequence);
CREATE UNIQUE INDEX scoped_call ON model_calls(system_id,call_id);
CREATE TABLE call_groups (
 call_id TEXT NOT NULL, system_id TEXT NOT NULL, group_name TEXT NOT NULL,
 PRIMARY KEY(call_id,group_name),
 FOREIGN KEY(system_id,call_id) REFERENCES model_calls(system_id,call_id)
);
CREATE TABLE model_attempts (
 attempt_id INTEGER PRIMARY KEY AUTOINCREMENT, call_id TEXT NOT NULL,
 system_id TEXT NOT NULL REFERENCES systems(system_id),
 dispatched_at INTEGER NOT NULL, reservation INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('running','known','unknown')),
 throttled INTEGER NOT NULL DEFAULT 0 CHECK(throttled IN (0,1)),
 input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
 FOREIGN KEY(system_id,call_id) REFERENCES model_calls(system_id,call_id)
);
CREATE INDEX attempts_window ON model_attempts(dispatched_at);
CREATE TABLE quota_state (
 group_name TEXT PRIMARY KEY, credit REAL NOT NULL, updated_at INTEGER NOT NULL,
 cooldown_until INTEGER NOT NULL DEFAULT 0
);
PRAGMA user_version = 2;
`

type StartSystemCommand struct {
	ExpectedRevision int64   `json:"expected_revision"`
	Goal             *string `json:"goal,omitempty"`
	TokenBudget      int64   `json:"token_budget,omitempty"`
	LifetimeSeconds  int64   `json:"lifetime_seconds,omitempty"`
	Stream           bool    `json:"stream,omitempty"`
	Continuous       bool    `json:"continuous,omitempty"`
}

type ExecutionRecord struct {
	LearningID      string          `json:"-"`
	CallID          string          `json:"call_id"`
	SystemID        string          `json:"system_id"`
	GoalID          string          `json:"goal_id"`
	TaskID          string          `json:"task_id"`
	ActivationID    string          `json:"activation_id"`
	ActivationOwner string          `json:"-"`
	Revision        int64           `json:"revision"`
	Model           string          `json:"model"`
	Stream          bool            `json:"stream"`
	State           string          `json:"state"`
	Reason          string          `json:"reason,omitempty"`
	Response        string          `json:"response,omitempty"`
	Reservation     int64           `json:"estimated_attempt_tokens"`
	Attempts        int64           `json:"attempts"`
	InputTokens     int64           `json:"input_tokens"`
	OutputTokens    int64           `json:"output_tokens"`
	UsageKnown      bool            `json:"usage_known"`
	CreatedAt       int64           `json:"created_at_ms"`
	QueuedAt        int64           `json:"queued_at_ms"`
	Deadline        int64           `json:"deadline_ms"`
	WaitDeadline    int64           `json:"wait_deadline_ms"`
	DispatchedAt    int64           `json:"dispatched_at_ms"`
	Prompt          string          `json:"prompt"`
	GoalBudget      int64           `json:"goal_token_budget"`
	GoalUsed        int64           `json:"goal_used_tokens"`
	GoalReserved    int64           `json:"goal_reserved_tokens"`
	TaskState       string          `json:"task_state"`
	Continuous      bool            `json:"continuous,omitempty"`
	QueueWaitMs     int64           `json:"queue_wait_ms"`
	Retries         int64           `json:"retries"`
	AgentID         string          `json:"agent_id"`
	Request         []byte          `json:"-"`
	Actions         []ModelToolCall `json:"tool_calls,omitempty"`
	EventID         string          `json:"-"`
}

func readExecution(ctx context.Context, tx *sql.Tx, systemID, callID string) (e ExecutionRecord, err error) {
	var actions []byte
	err = tx.QueryRowContext(ctx, `SELECT c.call_id,c.system_id,c.goal_id,c.task_id,c.activation_id,
	 c.activation_owner,c.revision,c.model,c.stream,c.state,c.reason,c.response,c.reservation,
	 c.attempts,c.input_tokens,c.output_tokens,c.usage_known,c.created_at,c.queued_at,c.deadline,
	 c.wait_deadline,c.dispatched_at,g.prompt,g.token_budget,g.used_tokens,g.reserved_tokens,
	 COALESCE((SELECT state FROM tasks t WHERE t.system_id=c.system_id AND t.task_id=c.task_id),g.state),c.queue_wait_ms,c.agent_id,c.request,c.actions,g.continuous
	 FROM model_calls c JOIN goals g ON g.system_id=c.system_id AND g.goal_id=c.goal_id
	 WHERE c.system_id=? AND (?='' OR c.call_id=?) ORDER BY c.sequence DESC LIMIT 1`,
		systemID, callID, callID).Scan(&e.CallID, &e.SystemID, &e.GoalID, &e.TaskID, &e.ActivationID,
		&e.ActivationOwner, &e.Revision, &e.Model, &e.Stream, &e.State, &e.Reason, &e.Response, &e.Reservation,
		&e.Attempts, &e.InputTokens, &e.OutputTokens, &e.UsageKnown, &e.CreatedAt, &e.QueuedAt, &e.Deadline,
		&e.WaitDeadline, &e.DispatchedAt, &e.Prompt, &e.GoalBudget, &e.GoalUsed, &e.GoalReserved, &e.TaskState, &e.QueueWaitMs, &e.AgentID, &e.Request, &actions, &e.Continuous)
	if err == nil {
		err = json.Unmarshal(actions, &e.Actions)
	}
	e.Retries = max(e.Attempts-1, 0)
	return
}

func auditExecution(ctx context.Context, tx *sql.Tx, systemID, principal, action string, revision int64, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO audit(system_id,principal,action,revision,created_at) VALUES(?,?,?,?,?)`,
		systemID, principal, action, revision, now.UTC().Format(time.RFC3339Nano))
	return err
}

func (store *Store) StartSystem(ctx context.Context, principal, key, systemID, owner string,
	command StartSystemCommand, cfg Configuration, now time.Time) (SystemRecord, error) {
	hash, _, err := JsonDigest(struct {
		Operation, SystemID string
		Command             StartSystemCommand
	}{"system.start", systemID, command})
	if err != nil {
		return SystemRecord{}, err
	}
	return store.command(ctx, principal, key, hash, func(tx *sql.Tx) (SystemRecord, error) {
		record, err := readSystem(ctx, tx, principal, systemID)
		if err != nil {
			return record, err
		}
		if record.Revision != command.ExpectedRevision {
			return record, ErrRevisionConflict
		}
		if record.State == "running" || record.State == "stopping" || record.State == "paused" {
			return record, ErrExecutionConflict
		}
		record, err = cfg.Inspect(record)
		if err != nil {
			return record, err
		}
		if record.BlockedReason != "" {
			return record, Invalid("grants", record.BlockedReason)
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM systems WHERE state IN ('running','stopping')`).Scan(&active); err != nil {
			return record, err
		}
		if active >= MaxOperators {
			return record, Invalid("execution", "daemon operator capacity exhausted")
		}
		prompt, goalID := "", ""
		if command.Goal != nil {
			prompt = *command.Goal
		} else if record.Goal != nil && record.Goal.State == "pending" {
			prompt, goalID = record.Goal.Prompt, record.Goal.ID
		}
		if strings.TrimSpace(prompt) == "" || len(prompt) > 32768 {
			return record, Invalid("goal", "requires a new 1-32768 byte goal, or a pending initial goal")
		}
		budget := command.TokenBudget
		if command.LifetimeSeconds < 0 || command.LifetimeSeconds > 86400 {
			return record, Invalid("lifetime_seconds", "must be 0 (default) or 1-86400; only administrator starts may extend goal lifetime")
		}
		if budget == 0 {
			budget = record.RemainingTokens
			if goalID != "" {
				budget = min(budget, record.Goal.TokenBudget)
			}
		}
		if budget < 1 || budget > record.RemainingTokens {
			return record, Invalid("token_budget", "exhausted budget or goal cap exceeds remaining system allowance")
		}
		record.Continuous = command.Continuous
		body, reservation, err := ModelRequest(cfg, record, prompt, command.Stream)
		if err != nil {
			return record, err
		}
		if reservation > budget {
			return record, Invalid("token_budget", "estimated input plus output cap cannot fit the goal budget")
		}
		if err := authorizePins(ctx, tx, cfg, systemID, record.Grants.OperatorTools); err != nil {
			return record, err
		}
		wait := int64(3600)
		model := cfg.Models[record.Grants.Model]
		for _, group := range model.QuotaGroups {
			quota := cfg.QuotaGroups[group]
			if reservation > quota.TokensPerMinute {
				return record, Invalid("quota", "request cannot fit token capacity")
			}
			if quota.MaxWaitSeconds < wait {
				wait = quota.MaxWaitSeconds
			}
			var queued int64
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM call_groups g JOIN model_calls c USING(call_id)
			 WHERE g.group_name=? AND c.state IN ('awaiting_worker','queued')`, group).Scan(&queued); err != nil {
				return record, err
			}
			if queued >= quota.QueueCapacity {
				return record, Invalid("quota", "model queue is full")
			}
		}
		if goalID == "" {
			goalID, err = NewID("goal_")
			if err != nil {
				return record, err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO goals(goal_id,system_id,owner,prompt,state,token_budget,is_initial)
			 VALUES(?,?,?,?,'queued',?,0)`, goalID, systemID, principal, prompt, budget); err != nil {
				return record, err
			}
		} else {
			if budget > record.Goal.TokenBudget {
				return record, Invalid("token_budget", "cannot expand the pending goal's cap")
			}
			if _, err = tx.ExecContext(ctx, `UPDATE goals SET state='queued',token_budget=? WHERE system_id=? AND goal_id=?`, budget, systemID, goalID); err != nil {
				return record, err
			}
		}
		callID, err := NewID("call_")
		if err != nil {
			return record, err
		}
		taskID, err := NewID("task_")
		if err != nil {
			return record, err
		}
		activationID, err := NewID("activation_")
		if err != nil {
			return record, err
		}
		if err := protocol.CheckFrame(protocol.Message{Type: "task", ID: callID, Data: prompt}); err != nil {
			return record, Invalid("goal", "encoded worker input exceeds frame limit")
		}
		deadline := cfg.taskDeadline(record.Grants.Model, command.LifetimeSeconds, now)
		if _, err := tx.ExecContext(ctx, `UPDATE goals SET continuous=?,task_lifetime_seconds=? WHERE system_id=? AND goal_id=?`,
			command.Continuous, command.LifetimeSeconds, systemID, goalID); err != nil {
			return record, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO model_calls(call_id,system_id,goal_id,task_id,activation_id,
		 activation_owner,revision,model,stream,state,reservation,created_at,queued_at,deadline,wait_deadline)
		 VALUES(?,?,?,?,?,?,?,?,?,'awaiting_worker',?,?,?,?,?)`,
			callID, systemID, goalID, taskID, activationID, owner, record.Revision, record.Grants.Model, command.Stream,
			reservation, now.UnixMilli(), now.UnixMilli(), deadline, now.Add(time.Duration(wait)*time.Second).UnixMilli()); err != nil {
			return record, err
		}
		for _, group := range model.QuotaGroups {
			if _, err := tx.ExecContext(ctx, `INSERT INTO call_groups(call_id,system_id,group_name) VALUES(?,?,?)`, callID, systemID, group); err != nil {
				return record, err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE systems SET state='running' WHERE system_id=?`, systemID); err != nil {
			return record, err
		}
		if err := auditExecution(ctx, tx, systemID, principal, "system.start", record.Revision, now); err != nil {
			return record, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE model_calls SET agent_id=?,request=? WHERE call_id=?`, record.OperatorID, body, callID); err != nil {
			return record, err
		}
		e, err := readExecution(ctx, tx, systemID, callID)
		if err != nil {
			return record, err
		}
		if err := initializeRootTask(ctx, tx, record, e, now); err != nil {
			return record, err
		}
		return readSystem(ctx, tx, principal, systemID)
	})
}

func (store *Store) StopSystem(ctx context.Context, principal, key, systemID string, now time.Time) (SystemRecord, error) {
	hash, _, err := JsonDigest([]string{"system.stop", systemID})
	if err != nil {
		return SystemRecord{}, err
	}
	return store.command(ctx, principal, key, hash, func(tx *sql.Tx) (SystemRecord, error) {
		record, err := readSystem(ctx, tx, principal, systemID)
		if err != nil {
			return record, err
		}
		state := "stopped"
		if record.State == "running" || record.State == "stopping" || record.State == "paused" {
			state = "stopping"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE systems SET state=? WHERE system_id=?`, state, systemID); err != nil {
			return record, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE goals SET state='canceled' WHERE system_id=? AND state='pending'`, systemID); err != nil {
			return record, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE goals SET control='stopped' WHERE system_id=? AND state IN ('queued','running','waiting')`, systemID); err != nil {
			return record, err
		}
		if _, err := stopTasks(ctx, tx, systemID, "system", "", localAdministrator, "stopped by administrator"); err != nil {
			return record, err
		}
		if err := auditExecution(ctx, tx, systemID, principal, "system.stop", record.Revision, now); err != nil {
			return record, err
		}
		return readSystem(ctx, tx, principal, systemID)
	})
}

func (store *Store) ListCalls(ctx context.Context, principal, systemID string, after int64) (calls []ExecutionRecord, next int64, err error) {
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	defer rollback(tx, &err)
	if _, err := readSystem(ctx, tx, principal, systemID); err != nil {
		return nil, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT sequence,call_id FROM model_calls WHERE system_id=? AND sequence>? ORDER BY sequence LIMIT 21`, systemID, after)
	if err != nil {
		return nil, 0, err
	}
	type cursor struct {
		seq int64
		id  string
	}
	var ids []cursor
	for rows.Next() {
		var c cursor
		if err := rows.Scan(&c.seq, &c.id); err != nil {
			return nil, 0, errors.Join(err, rows.Close())
		}
		ids = append(ids, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, 0, err
	}
	if len(ids) > 20 {
		ids = ids[:20]
		next = ids[19].seq
	}
	calls = make([]ExecutionRecord, 0, len(ids))
	for _, c := range ids {
		e, err := readExecution(ctx, tx, systemID, c.id)
		if err != nil {
			return nil, 0, err
		}
		calls = append(calls, e)
	}
	return calls, next, nil
}
