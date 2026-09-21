package state

const schemaV4 = `
CREATE TABLE memory_heads(
 system_id TEXT NOT NULL REFERENCES systems(system_id),memory_id TEXT NOT NULL,
 scope TEXT NOT NULL CHECK(scope IN ('task','agent','system')),owner_id TEXT NOT NULL,
 revision INTEGER NOT NULL,state TEXT NOT NULL CHECK(state IN ('active','pending','deleted')),
 expires_at INTEGER NOT NULL,PRIMARY KEY(system_id,memory_id)
);
CREATE TRIGGER immutable_memory_scope BEFORE UPDATE OF system_id,memory_id,scope,owner_id ON memory_heads
 BEGIN SELECT RAISE(ABORT,'memory scope is immutable'); END;
CREATE INDEX memory_expiry ON memory_heads(expires_at,state);
CREATE TABLE memory_revisions(
 system_id TEXT NOT NULL,memory_id TEXT NOT NULL,revision INTEGER NOT NULL,content TEXT NOT NULL,evidence TEXT NOT NULL,
 confidence INTEGER NOT NULL,artifacts BLOB NOT NULL,author TEXT NOT NULL,goal_id TEXT NOT NULL,task_id TEXT NOT NULL,
 created_at INTEGER NOT NULL,PRIMARY KEY(system_id,memory_id,revision),
 FOREIGN KEY(system_id,memory_id) REFERENCES memory_heads(system_id,memory_id)
);
CREATE TRIGGER immutable_memory_revision BEFORE UPDATE ON memory_revisions
 BEGIN SELECT RAISE(ABORT,'memory revisions are immutable'); END;
CREATE TABLE schedules(
 system_id TEXT NOT NULL,schedule_id TEXT NOT NULL,task_id TEXT NOT NULL,creator TEXT NOT NULL,
 expression TEXT NOT NULL,timezone TEXT NOT NULL,content TEXT NOT NULL,next_due INTEGER NOT NULL,
 expires_at INTEGER NOT NULL,remaining INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('active','completed','canceled','expired','failed')),
 reason TEXT NOT NULL DEFAULT '',PRIMARY KEY(system_id,schedule_id),
 FOREIGN KEY(system_id,task_id) REFERENCES tasks(system_id,task_id)
);
CREATE INDEX schedule_due ON schedules(state,next_due);
CREATE TABLE schedule_occurrences(
 system_id TEXT NOT NULL,schedule_id TEXT NOT NULL,due INTEGER NOT NULL,event_id TEXT NOT NULL,
 PRIMARY KEY(system_id,schedule_id,due),
 FOREIGN KEY(system_id,schedule_id) REFERENCES schedules(system_id,schedule_id),
 FOREIGN KEY(system_id,event_id) REFERENCES events(system_id,event_id)
);
CREATE TABLE subscriptions(
 system_id TEXT NOT NULL,subscription_id TEXT NOT NULL,task_id TEXT NOT NULL,creator TEXT NOT NULL,
 type TEXT NOT NULL CHECK(type IN ('memory.changed','task.completed')),scope TEXT NOT NULL,
 expires_at INTEGER NOT NULL,remaining INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('active','completed','canceled','expired','failed')),
 reason TEXT NOT NULL DEFAULT '',PRIMARY KEY(system_id,subscription_id),
 FOREIGN KEY(system_id,task_id) REFERENCES tasks(system_id,task_id)
);
PRAGMA user_version=4;
`
