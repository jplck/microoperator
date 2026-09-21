package state

const schemaV5 = `
ALTER TABLE tasks ADD COLUMN learning_id TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN learning_role TEXT NOT NULL DEFAULT '';
CREATE TABLE learning_checks (
 system_id TEXT NOT NULL REFERENCES systems(system_id),check_id TEXT NOT NULL,
 cases BLOB NOT NULL,digest TEXT NOT NULL,created_at INTEGER NOT NULL,
 PRIMARY KEY(system_id,check_id)
);
CREATE TRIGGER immutable_checks_update BEFORE UPDATE ON learning_checks
 BEGIN SELECT RAISE(ABORT,'protected checks are immutable'); END;
CREATE TRIGGER immutable_checks_delete BEFORE DELETE ON learning_checks
 BEGIN SELECT RAISE(ABORT,'protected checks are immutable'); END;
CREATE TABLE learning_feedback (
 system_id TEXT NOT NULL,feedback_id TEXT NOT NULL,task_id TEXT NOT NULL,
 rating TEXT NOT NULL,content TEXT NOT NULL,created_at INTEGER NOT NULL,
 PRIMARY KEY(system_id,feedback_id),FOREIGN KEY(system_id,task_id) REFERENCES tasks(system_id,task_id)
);
CREATE TRIGGER immutable_feedback_update BEFORE UPDATE ON learning_feedback
 BEGIN SELECT RAISE(ABORT,'feedback is immutable'); END;
CREATE TABLE learning_evaluations (
 system_id TEXT NOT NULL,evaluation_id TEXT NOT NULL,tool_id TEXT NOT NULL,version INTEGER NOT NULL,
 check_id TEXT NOT NULL,origin_task TEXT NOT NULL,agent_id TEXT NOT NULL,
 baseline BLOB NOT NULL,profile_digest TEXT NOT NULL,toolchain_digest TEXT NOT NULL,runtime_digest TEXT NOT NULL,
 system_revision INTEGER NOT NULL,configuration_digest TEXT NOT NULL,notify INTEGER NOT NULL DEFAULT 0,
 state TEXT NOT NULL CHECK(state IN ('queued','evaluating','pending_approval','active','failed','rejected','disabled')),
 evidence BLOB NOT NULL DEFAULT '[]',artifact_digest TEXT NOT NULL DEFAULT '',
 definition BLOB NOT NULL DEFAULT '{}',digest TEXT NOT NULL DEFAULT '',reason TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(system_id,evaluation_id),UNIQUE(system_id,tool_id,version),
 FOREIGN KEY(system_id,tool_id,version) REFERENCES tool_drafts(system_id,tool_id,version),
 FOREIGN KEY(system_id,check_id) REFERENCES learning_checks(system_id,check_id),
 FOREIGN KEY(system_id,origin_task) REFERENCES tasks(system_id,task_id)
);
CREATE TRIGGER immutable_evaluation_identity BEFORE UPDATE OF system_id,evaluation_id,tool_id,version,check_id,origin_task,agent_id,baseline,profile_digest,toolchain_digest,runtime_digest,system_revision,configuration_digest,notify ON learning_evaluations
 BEGIN SELECT RAISE(ABORT,'evaluation identity is immutable'); END;
CREATE TRIGGER immutable_evaluation_evidence BEFORE UPDATE OF evidence,artifact_digest,definition,digest ON learning_evaluations WHEN OLD.state NOT IN ('queued','evaluating')
 BEGIN SELECT RAISE(ABORT,'finished evaluation evidence is immutable'); END;
CREATE INDEX pending_evaluations ON learning_evaluations(state) WHERE state IN ('queued','evaluating');
CREATE TABLE learning_approvals (
 system_id TEXT NOT NULL,tool_id TEXT NOT NULL,version INTEGER NOT NULL,
 evaluation_id TEXT NOT NULL,definition BLOB NOT NULL,digest TEXT NOT NULL,
 expires_at INTEGER NOT NULL,remaining_tasks INTEGER NOT NULL CHECK(remaining_tasks>=0),
 PRIMARY KEY(system_id,tool_id,version),
 FOREIGN KEY(system_id,evaluation_id) REFERENCES learning_evaluations(system_id,evaluation_id)
);
CREATE TRIGGER immutable_approval BEFORE UPDATE OF system_id,tool_id,version,evaluation_id,definition,digest,expires_at ON learning_approvals
 BEGIN SELECT RAISE(ABORT,'approvals bind immutable evidence and definitions'); END;
CREATE TABLE learning_uses (
 system_id TEXT NOT NULL,tool_id TEXT NOT NULL,version INTEGER NOT NULL,task_id TEXT NOT NULL,
 PRIMARY KEY(system_id,tool_id,version,task_id),
 FOREIGN KEY(system_id,tool_id,version) REFERENCES learning_approvals(system_id,tool_id,version),
 FOREIGN KEY(system_id,task_id) REFERENCES tasks(system_id,task_id)
);
CREATE INDEX learning_task ON tasks(system_id,learning_id,learning_role) WHERE learning_id!='';
PRAGMA user_version=5;
`
