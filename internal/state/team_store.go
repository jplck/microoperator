package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	MaxTaskTurns    = 8
	MaxGoalTasks    = 64
	MaxGoalEvents   = 256
	MaxSystemEvents = 4096
	MaxEventBytes   = 8192
	MaxEventDepth   = 16
)

const schemaV3 = `
CREATE TABLE systems_v3 (
 system_id TEXT PRIMARY KEY, owner TEXT NOT NULL, operator_id TEXT NOT NULL UNIQUE,
 launch TEXT NOT NULL,state TEXT NOT NULL CHECK(state IN ('inactive','running','stopping','stopped','paused')),
 revision INTEGER NOT NULL CHECK(revision>0),used_tokens INTEGER NOT NULL DEFAULT 0 CHECK(used_tokens>=0),
 reserved_tokens INTEGER NOT NULL DEFAULT 0 CHECK(reserved_tokens>=0),last_admission INTEGER NOT NULL DEFAULT 0,created_at TEXT NOT NULL,
 FOREIGN KEY(system_id,revision) REFERENCES system_revisions(system_id,revision) DEFERRABLE INITIALLY DEFERRED
);
INSERT INTO systems_v3 SELECT * FROM systems;
DROP TABLE systems;
ALTER TABLE systems_v3 RENAME TO systems;
CREATE INDEX systems_owner ON systems(owner,system_id);
ALTER TABLE goals ADD COLUMN control TEXT NOT NULL DEFAULT 'active' CHECK(control IN ('active','paused','stopped'));
ALTER TABLE goals ADD COLUMN deadline INTEGER NOT NULL DEFAULT 0;
CREATE TABLE model_calls_v3 (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,call_id TEXT NOT NULL UNIQUE,system_id TEXT NOT NULL,goal_id TEXT NOT NULL,
 task_id TEXT NOT NULL,activation_id TEXT NOT NULL UNIQUE,activation_owner TEXT NOT NULL,
 revision INTEGER NOT NULL,model TEXT NOT NULL,stream INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('awaiting_worker','queued','running','completed','failed','canceled','rejected','unknown')),
 reason TEXT NOT NULL DEFAULT '',response TEXT NOT NULL DEFAULT '',reservation INTEGER NOT NULL CHECK(reservation>0),
 attempts INTEGER NOT NULL DEFAULT 0,input_tokens INTEGER NOT NULL DEFAULT 0,output_tokens INTEGER NOT NULL DEFAULT 0,
 usage_known INTEGER NOT NULL DEFAULT 0,queue_wait_ms INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL,
 queued_at INTEGER NOT NULL,deadline INTEGER NOT NULL,wait_deadline INTEGER NOT NULL,dispatched_at INTEGER NOT NULL DEFAULT 0,
 agent_id TEXT NOT NULL DEFAULT '',request BLOB NOT NULL DEFAULT '',actions BLOB NOT NULL DEFAULT '[]',
 FOREIGN KEY(system_id,goal_id) REFERENCES goals(system_id,goal_id),
 FOREIGN KEY(system_id,revision) REFERENCES system_revisions(system_id,revision)
);
INSERT INTO model_calls_v3(sequence,call_id,system_id,goal_id,task_id,activation_id,activation_owner,revision,model,stream,state,
 reason,response,reservation,attempts,input_tokens,output_tokens,usage_known,queue_wait_ms,created_at,queued_at,deadline,wait_deadline,dispatched_at)
 SELECT * FROM model_calls;
UPDATE model_calls_v3 SET agent_id=(SELECT operator_id FROM systems WHERE systems.system_id=model_calls_v3.system_id);
DROP TABLE model_calls;
ALTER TABLE model_calls_v3 RENAME TO model_calls;
CREATE INDEX calls_queue ON model_calls(state,sequence);
CREATE INDEX calls_system ON model_calls(system_id,sequence);
CREATE UNIQUE INDEX scoped_call ON model_calls(system_id,call_id);
CREATE TABLE audit_v3 (
 audit_id INTEGER PRIMARY KEY,system_id TEXT NOT NULL,principal TEXT NOT NULL,action TEXT NOT NULL,
 revision INTEGER NOT NULL,created_at TEXT NOT NULL,
 FOREIGN KEY(system_id,revision) REFERENCES system_revisions(system_id,revision)
);
INSERT INTO audit_v3 SELECT * FROM audit;
DROP TABLE audit;
ALTER TABLE audit_v3 RENAME TO audit;
CREATE TRIGGER immutable_audit_update BEFORE UPDATE ON audit BEGIN SELECT RAISE(ABORT,'audit is append-only'); END;
CREATE TRIGGER immutable_audit_delete BEFORE DELETE ON audit BEGIN SELECT RAISE(ABORT,'audit is append-only'); END;
CREATE TABLE agents (
 system_id TEXT NOT NULL REFERENCES systems(system_id),agent_id TEXT NOT NULL,
 parent_id TEXT NOT NULL DEFAULT '',goal_id TEXT NOT NULL DEFAULT '',name TEXT NOT NULL,
 revision INTEGER NOT NULL,state TEXT NOT NULL CHECK(state IN ('active','paused','stopped')),
 depth INTEGER NOT NULL,token_budget INTEGER NOT NULL,max_turns INTEGER NOT NULL,
 PRIMARY KEY(system_id,agent_id)
);
CREATE TABLE agent_revisions (
 system_id TEXT NOT NULL,agent_id TEXT NOT NULL,revision INTEGER NOT NULL,
 definition BLOB NOT NULL,tools BLOB NOT NULL,
 PRIMARY KEY(system_id,agent_id,revision),
 FOREIGN KEY(system_id,agent_id) REFERENCES agents(system_id,agent_id)
);
CREATE TRIGGER immutable_agent_revision_update BEFORE UPDATE ON agent_revisions BEGIN SELECT RAISE(ABORT,'agent revisions are immutable'); END;
CREATE TRIGGER immutable_agent_revision_delete BEFORE DELETE ON agent_revisions BEGIN SELECT RAISE(ABORT,'agent revisions are immutable'); END;
CREATE TABLE agent_usage (
 system_id TEXT NOT NULL,agent_id TEXT NOT NULL,goal_id TEXT NOT NULL,
 used_tokens INTEGER NOT NULL DEFAULT 0,reserved_tokens INTEGER NOT NULL DEFAULT 0,turns INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(system_id,agent_id,goal_id),
 FOREIGN KEY(system_id,agent_id) REFERENCES agents(system_id,agent_id),
 FOREIGN KEY(system_id,goal_id) REFERENCES goals(system_id,goal_id)
);
CREATE TABLE tasks (
 system_id TEXT NOT NULL,task_id TEXT NOT NULL,goal_id TEXT NOT NULL,agent_id TEXT NOT NULL,agent_revision INTEGER NOT NULL,
 parent_task TEXT NOT NULL DEFAULT '',state TEXT NOT NULL CHECK(state IN ('queued','running','waiting','completed','failed','canceled','rejected')),
 control TEXT NOT NULL DEFAULT 'active' CHECK(control IN ('active','paused','stopped')),
 tools BLOB NOT NULL,conversation BLOB NOT NULL,turns INTEGER NOT NULL DEFAULT 1,
 call_id TEXT NOT NULL,waiting_tool TEXT NOT NULL DEFAULT '',response TEXT NOT NULL DEFAULT '',reason TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(system_id,task_id),
 FOREIGN KEY(system_id,goal_id) REFERENCES goals(system_id,goal_id),
 FOREIGN KEY(system_id,agent_id,agent_revision) REFERENCES agent_revisions(system_id,agent_id,revision)
);
CREATE INDEX tasks_agent ON tasks(system_id,agent_id,state);
CREATE TABLE events (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,event_id TEXT NOT NULL UNIQUE,system_id TEXT NOT NULL,
 type TEXT NOT NULL,version INTEGER NOT NULL DEFAULT 1,source TEXT NOT NULL,recipient TEXT NOT NULL,
 goal_id TEXT NOT NULL,task_id TEXT NOT NULL,correlation_id TEXT NOT NULL,causation_id TEXT NOT NULL,
 created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL,classification TEXT NOT NULL DEFAULT 'system-private',
 authorization_ref TEXT NOT NULL,depth INTEGER NOT NULL,payload BLOB NOT NULL,
 UNIQUE(system_id,event_id),
 FOREIGN KEY(system_id,task_id) REFERENCES tasks(system_id,task_id)
);
CREATE TRIGGER immutable_event_update BEFORE UPDATE ON events BEGIN SELECT RAISE(ABORT,'events are immutable'); END;
CREATE TRIGGER immutable_event_delete BEFORE DELETE ON events BEGIN SELECT RAISE(ABORT,'events are immutable'); END;
CREATE TABLE mailboxes (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,system_id TEXT NOT NULL,event_id TEXT NOT NULL,recipient TEXT NOT NULL,task_id TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('pending','leased','acked','dead')),
 lease_owner TEXT NOT NULL DEFAULT '',lease_until INTEGER NOT NULL DEFAULT 0,
 attempts INTEGER NOT NULL DEFAULT 0,not_before INTEGER NOT NULL DEFAULT 0,reason TEXT NOT NULL DEFAULT '',
 applied INTEGER NOT NULL DEFAULT 0,
 UNIQUE(system_id,event_id,recipient),
 FOREIGN KEY(system_id,event_id) REFERENCES events(system_id,event_id),
 FOREIGN KEY(system_id,recipient) REFERENCES agents(system_id,agent_id)
);
CREATE INDEX mailbox_ready ON mailboxes(state,not_before,sequence);
CREATE TABLE tool_drafts (
 system_id TEXT NOT NULL REFERENCES systems(system_id),tool_id TEXT NOT NULL,version INTEGER NOT NULL,
 creator TEXT NOT NULL,goal_id TEXT NOT NULL DEFAULT '',task_id TEXT NOT NULL DEFAULT '',kind TEXT NOT NULL,description TEXT NOT NULL,content TEXT NOT NULL,
 requires_tools BLOB NOT NULL,digest TEXT NOT NULL,state TEXT NOT NULL CHECK(state IN ('draft','rejected','disabled')),
 PRIMARY KEY(system_id,tool_id,version)
);
CREATE TRIGGER immutable_draft_content BEFORE UPDATE OF system_id,tool_id,version,creator,goal_id,task_id,kind,description,content,requires_tools,digest ON tool_drafts
 BEGIN SELECT RAISE(ABORT,'tool revisions are immutable'); END;
CREATE TABLE tool_revocations (
 system_id TEXT NOT NULL REFERENCES systems(system_id),name TEXT NOT NULL,version INTEGER NOT NULL,
 PRIMARY KEY(system_id,name,version)
);
CREATE TABLE tool_calls (
 system_id TEXT NOT NULL,call_id TEXT NOT NULL,tool_call_id TEXT NOT NULL,task_id TEXT NOT NULL,
 name TEXT NOT NULL,version INTEGER NOT NULL,arguments_hash TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('running','completed','failed','unknown')),
 result TEXT NOT NULL DEFAULT '',PRIMARY KEY(system_id,call_id,tool_call_id),
 FOREIGN KEY(system_id,call_id) REFERENCES model_calls(system_id,call_id),
 FOREIGN KEY(system_id,task_id) REFERENCES tasks(system_id,task_id)
);
CREATE TABLE artifacts (
 system_id TEXT NOT NULL,artifact_id TEXT NOT NULL,goal_id TEXT NOT NULL,task_id TEXT NOT NULL,
 digest TEXT NOT NULL,content BLOB NOT NULL,PRIMARY KEY(system_id,artifact_id),
 FOREIGN KEY(system_id,task_id) REFERENCES tasks(system_id,task_id)
);
PRAGMA user_version=3;
`

type TaskRecord struct {
	SystemID     string        `json:"system_id"`
	ID           string        `json:"task_id"`
	GoalID       string        `json:"goal_id"`
	AgentID      string        `json:"agent_id"`
	Revision     int64         `json:"agent_revision"`
	Parent       string        `json:"parent_task,omitempty"`
	State        string        `json:"state"`
	Control      string        `json:"control"`
	Tools        []ToolPin     `json:"tools"`
	Conversation []ChatMessage `json:"-"`
	Turns        int           `json:"turns"`
	Deadline     int64         `json:"deadline_ms"`
	CallID       string        `json:"call_id"`
	WaitingTool  string        `json:"waiting_tool,omitempty"`
	Response     string        `json:"response,omitempty"`
	Reason       string        `json:"reason,omitempty"`
	LearningID   string        `json:"evaluation_id,omitempty"`
	LearningRole string        `json:"evaluation_role,omitempty"`
}

type AgentRecord struct {
	SystemID    string         `json:"system_id"`
	ID          string         `json:"agent_id"`
	Parent      string         `json:"parent_id,omitempty"`
	GoalID      string         `json:"goal_id,omitempty"`
	Name        string         `json:"name"`
	Revision    int64          `json:"revision"`
	State       string         `json:"state"`
	Depth       int            `json:"depth"`
	TokenBudget int64          `json:"token_budget"`
	MaxTurns    int            `json:"max_turns"`
	Definition  OperatorConfig `json:"definition"`
	Tools       []ToolPin      `json:"tools"`
	Available   bool           `json:"available"`
}

func readTask(ctx context.Context, tx *sql.Tx, systemID, id string) (t TaskRecord, err error) {
	var tools, conversation []byte
	err = tx.QueryRowContext(ctx, `SELECT system_id,task_id,goal_id,agent_id,agent_revision,parent_task,state,control,tools,conversation,turns,call_id,waiting_tool,response,reason,learning_id,learning_role,deadline
	 FROM tasks WHERE system_id=? AND task_id=?`, systemID, id).Scan(&t.SystemID, &t.ID, &t.GoalID, &t.AgentID, &t.Revision, &t.Parent, &t.State, &t.Control, &tools, &conversation, &t.Turns, &t.CallID, &t.WaitingTool, &t.Response, &t.Reason, &t.LearningID, &t.LearningRole, &t.Deadline)
	if err != nil {
		return t, err
	}
	if err = json.Unmarshal(tools, &t.Tools); err != nil {
		return t, err
	}
	err = json.Unmarshal(conversation, &t.Conversation)
	return
}

func readAgent(ctx context.Context, tx *sql.Tx, systemID, id string, revision int64) (a AgentRecord, err error) {
	var definition, tools []byte
	err = tx.QueryRowContext(ctx, `SELECT a.system_id,a.agent_id,a.parent_id,a.goal_id,a.name,r.revision,a.state,a.depth,a.token_budget,a.max_turns,r.definition,r.tools
	 FROM agents a JOIN agent_revisions r ON r.system_id=a.system_id AND r.agent_id=a.agent_id AND r.revision=CASE WHEN ?=0 THEN a.revision ELSE ? END
	 WHERE a.system_id=? AND a.agent_id=?`, revision, revision, systemID, id).Scan(&a.SystemID, &a.ID, &a.Parent, &a.GoalID, &a.Name, &a.Revision, &a.State, &a.Depth, &a.TokenBudget, &a.MaxTurns, &definition, &tools)
	if err != nil {
		return a, err
	}
	if err = json.Unmarshal(definition, &a.Definition); err != nil {
		return a, err
	}
	err = json.Unmarshal(tools, &a.Tools)
	if err != nil {
		return a, err
	}
	err = tx.QueryRowContext(ctx, `SELECT ?='active' AND NOT EXISTS(SELECT 1 FROM tasks WHERE system_id=? AND agent_id=? AND state IN ('queued','running','waiting'))`,
		a.State, systemID, id).Scan(&a.Available)
	return
}

func insertAgentRevision(ctx context.Context, tx *sql.Tx, a AgentRecord) error {
	def, err := json.Marshal(a.Definition)
	if err != nil {
		return err
	}
	tools, err := json.Marshal(a.Tools)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO agent_revisions(system_id,agent_id,revision,definition,tools) VALUES(?,?,?,?,?)`, a.SystemID, a.ID, a.Revision, def, tools)
	return err
}

func emitEvent(ctx context.Context, tx *sql.Tx, t TaskRecord, kind, source, causation string, payload any, now time.Time) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	if len(data) > MaxEventBytes {
		return "", Invalid("event", "payload exceeds 8 KiB")
	}
	var continuous bool
	var deadline int64
	if err := tx.QueryRowContext(ctx, `SELECT continuous,deadline FROM goals WHERE system_id=? AND goal_id=?`, t.SystemID, t.GoalID).Scan(&continuous, &deadline); err != nil {
		return "", err
	}
	depth := 0
	if causation != "" {
		if err := tx.QueryRowContext(ctx, `SELECT depth+1 FROM events WHERE system_id=? AND event_id=?`, t.SystemID, causation).Scan(&depth); err != nil {
			return "", err
		}
	}
	var total, goalCount, pending int
	if err := tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(SUM(CASE WHEN goal_id=? AND (?=0 OR task_id=?) THEN 1 ELSE 0 END),0)
	 FROM events WHERE system_id=? AND (?=0 OR created_at>?)`, t.GoalID, continuous, t.ID, t.SystemID, continuous, now.Add(-24*time.Hour).UnixMilli()).Scan(&total, &goalCount); err != nil {
		return "", err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mailboxes WHERE system_id=? AND recipient=? AND state IN ('pending','leased')`, t.SystemID, t.AgentID).Scan(&pending); err != nil {
		return "", err
	}
	var reserved, goalReserved, recipientReserved, sourceRate int
	if err := tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(SUM(CASE WHEN goal_id=? THEN 1 ELSE 0 END),0),
	 COALESCE(SUM(CASE WHEN parent_task=? THEN 1 ELSE 0 END),0) FROM tasks WHERE system_id=? AND state IN ('queued','running','waiting')`,
		t.GoalID, t.ID, t.SystemID).Scan(&reserved, &goalReserved, &recipientReserved); err != nil {
		return "", err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE system_id=? AND source=? AND created_at>?`, t.SystemID, source, now.Add(-time.Minute).UnixMilli()).Scan(&sourceRate); err != nil {
		return "", err
	}
	if total+reserved >= MaxSystemEvents || goalCount+goalReserved >= MaxGoalEvents || pending+recipientReserved >= 64 || depth > MaxEventDepth ||
		(strings.HasPrefix(source, "agent_") && sourceRate >= 32) {
		return "", Invalid("events", "event budget, queue, or causation limit exhausted")
	}
	id, err := NewID("event_")
	if err != nil {
		return "", err
	}
	if !continuous && deadline <= now.UnixMilli() && kind != "task.result" {
		return "", Invalid("event", "goal lifetime has expired")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(event_id,system_id,type,source,recipient,goal_id,task_id,correlation_id,causation_id,created_at,expires_at,authorization_ref,depth,payload)
	 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, t.SystemID, kind, source, t.AgentID, t.GoalID, t.ID, t.ID, causation, now.UnixMilli(), deadline, fmt.Sprintf("%s:%d", t.AgentID, t.Revision), depth, data); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mailboxes(system_id,event_id,recipient,task_id,state) VALUES(?,?,?,?,'pending')`, t.SystemID, id, t.AgentID, t.ID); err != nil {
		return "", err
	}
	return id, nil
}

func initializeRootTask(ctx context.Context, tx *sql.Tx, record SystemRecord, e ExecutionRecord, now time.Time) error {
	a := AgentRecord{SystemID: record.ID, ID: record.OperatorID, Name: "operator", Revision: record.Revision, State: "active", TokenBudget: record.Configuration.Limits.TokenBudget, MaxTurns: MaxTaskTurns, Definition: record.Configuration.Operator, Tools: record.Grants.OperatorTools}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agents(system_id,agent_id,name,revision,state,depth,token_budget,max_turns) VALUES(?,?,?,?,?,0,?,?)
	 ON CONFLICT(system_id,agent_id) DO UPDATE SET revision=excluded.revision,state='active',token_budget=excluded.token_budget`,
		a.SystemID, a.ID, a.Name, a.Revision, a.State, a.TokenBudget, a.MaxTurns); err != nil {
		return err
	}
	if err := insertAgentRevision(ctx, tx, a); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_usage(system_id,agent_id,goal_id,turns) VALUES(?,?,?,1)`, a.SystemID, a.ID, e.GoalID); err != nil {
		return err
	}
	pins, err := json.Marshal(a.Tools)
	if err != nil {
		return err
	}
	conversation, err := json.Marshal([]ChatMessage{{Role: "user", Content: e.Prompt}})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tasks(system_id,task_id,goal_id,agent_id,agent_revision,state,tools,conversation,call_id,deadline)
	 VALUES(?,?,?,?,?,'queued',?,?,?,?)`, record.ID, e.TaskID, e.GoalID, a.ID, a.Revision, pins, conversation, e.CallID, e.Deadline); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE goals SET deadline=CASE WHEN continuous=1 THEN 0 ELSE ? END WHERE system_id=? AND goal_id=?`, e.Deadline, record.ID, e.GoalID); err != nil {
		return err
	}
	t, err := readTask(ctx, tx, record.ID, e.TaskID)
	if err != nil {
		return err
	}
	_, err = emitEvent(ctx, tx, t, "task.ready", localAdministrator, "", struct{}{}, now)
	return err
}

func TaskTerminal(state string) bool {
	return state == "completed" || state == "failed" || state == "canceled" || state == "rejected"
}

func pinnedTaskRecord(ctx context.Context, tx *sql.Tx, record SystemRecord, e ExecutionRecord) (SystemRecord, TaskRecord, error) {
	t, err := readTask(ctx, tx, e.SystemID, e.TaskID)
	if errors.Is(err, sql.ErrNoRows) {
		return record, t, nil
	} // Pre-migration call history has no resumable task.
	if err != nil {
		return record, t, err
	}
	a, err := readAgent(ctx, tx, t.SystemID, t.AgentID, t.Revision)
	if err != nil {
		return record, t, err
	}
	record.Configuration.Operator = a.Definition
	record.Grants.OperatorTools = t.Tools
	record.Grants.Model = a.Definition.Model
	record.Grants.SandboxProfile = a.Definition.SandboxProfile
	return record, t, nil
}
