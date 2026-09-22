package state

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fixtureCall(t *testing.T, b *admission, configID, key string, stream bool) ExecutionRecord {
	t.Helper()
	goal := "fixture goal"
	record, err := b.store.CreateSystem(context.Background(), localAdministrator, "create-"+key, CreateSystemCommand{Name: "research", Goal: &goal}, b.cfg, configID)
	if err != nil {
		t.Fatal(err)
	}
	started, err := b.store.StartSystem(context.Background(), localAdministrator, "start-"+key, record.ID, "fixture-owner",
		StartSystemCommand{ExpectedRevision: 1, Stream: stream}, b.cfg, b.now())
	if err != nil {
		t.Fatal(err)
	}
	return *started.Execution
}

func admitCall(t *testing.T, b *admission, e ExecutionRecord, want bool) ExecutionRecord {
	t.Helper()
	current, admitted, err := b.admit(context.Background(), e, b.now())
	if err != nil || admitted != want {
		t.Fatalf("admit %s: admitted=%v err=%v state=%s reason=%s", e.CallID, admitted, err, current.State, current.Reason)
	}
	return current
}

func TestProviderTimeoutSetsDefaultGoalDeadline(t *testing.T) {
	for _, tc := range []struct {
		name              string
		timeout, lifetime int64
		want              time.Duration
	}{
		{"default", 0, 0, 210 * time.Second},
		{"local model", 600, 0, 1830 * time.Second},
		{"short request", 1, 0, 33 * time.Second},
		{"explicit goal deadline", 600, 17, 17 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fixtureConfiguration(t)
			provider := cfg.Providers["primary"]
			provider.TimeoutSeconds = tc.timeout
			cfg.Providers["primary"] = provider
			store, id := fixtureStore(t, cfg)
			ctx := context.Background()
			record, err := store.CreateSystem(ctx, localAdministrator, "create", CreateSystemCommand{Name: "research", Goal: fixtureGoal()}, cfg, id)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Unix(1720000000, 123000000)
			record, err = store.StartSystem(ctx, localAdministrator, "start", record.ID, "fixture-owner",
				StartSystemCommand{ExpectedRevision: 1, LifetimeSeconds: tc.lifetime}, cfg, now)
			if err != nil {
				t.Fatal(err)
			}
			if record.Execution.Deadline != now.Add(tc.want).UnixMilli() ||
				record.Execution.WaitDeadline != now.Add(30*time.Second).UnixMilli() {
				t.Fatalf("request timeout changed the wrong deadline: %+v", record.Execution)
			}
			var deadline int64
			if err := store.db.QueryRow(`SELECT deadline FROM goals WHERE system_id=? AND goal_id=?`,
				record.ID, record.Execution.GoalID).Scan(&deadline); err != nil || deadline != record.Execution.Deadline {
				t.Fatalf("goal did not retain its shared deadline: %d, %v", deadline, err)
			}
		})
	}
}

func TestStoreMigrationFromPopulatedVersionOne(t *testing.T) {
	cfg := fixtureConfiguration(t)
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(cfg.DataDir, "state.db")
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	db, err := sql.Open("sqlite", filename)
	if err != nil {
		t.Fatal(err)
	}
	legacy := &Store{db: db}
	if _, err := db.Exec(schemaV1); err != nil {
		t.Fatal(err)
	}
	id, err := legacy.RememberConfiguration(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	goal := "preserve this goal"
	record, err := legacy.CreateSystem(context.Background(), localAdministrator, "legacy", CreateSystemCommand{Name: "research", Goal: &goal}, cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE systems SET used_tokens=23 WHERE system_id=?`, record.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(context.Background(), filename)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.db.Close()
	saved, err := migrated.GetSystem(context.Background(), localAdministrator, record.ID)
	if err != nil || saved.UsedTokens != 23 || saved.Goal.Prompt != goal || saved.State != "inactive" {
		t.Fatalf("migration lost state: %+v %v", saved, err)
	}
	var keys, version int
	if err := migrated.db.QueryRow("PRAGMA foreign_keys").Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if err := migrated.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if keys != 1 || version != 5 {
		t.Fatalf("migration enforcement/version = %d/%d", keys, version)
	}
	if _, err := migrated.db.Exec(`DELETE FROM audit`); err == nil {
		t.Fatal("migration removed audit protection")
	}
	if _, err := migrated.db.Exec(`UPDATE goals SET system_id='foreign'`); err == nil {
		t.Fatal("migration disabled foreign keys")
	}
	replay, err := migrated.CreateSystem(context.Background(), localAdministrator, "legacy", CreateSystemCommand{Name: "research", Goal: &goal}, cfg, id)
	if err != nil || replay.ID != record.ID {
		t.Fatalf("legacy receipt failed: %+v %v", replay, err)
	}
}

func TestStoreMigrationPreservesPopulatedVersionTwo(t *testing.T) {
	cfg := fixtureConfiguration(t)
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(cfg.DataDir, "state.db")
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	db, err := sql.Open("sqlite", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schemaV1); err != nil {
		t.Fatal(err)
	}
	legacy := &Store{db: db}
	id, err := legacy.RememberConfiguration(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	goal := "legacy dispatched goal"
	record, err := legacy.CreateSystem(context.Background(), localAdministrator, "legacy-v2", CreateSystemCommand{Name: "research", Goal: &goal}, cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schemaV2); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, err := db.Exec(`UPDATE systems SET state='running',used_tokens=23,reserved_tokens=123 WHERE system_id=?;
	 UPDATE goals SET state='running',used_tokens=23,reserved_tokens=123 WHERE system_id=?`, record.ID, record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_calls(call_id,system_id,goal_id,task_id,activation_id,activation_owner,revision,model,stream,state,reservation,attempts,created_at,queued_at,deadline,wait_deadline)
	 VALUES('legacy-call',?,?,'legacy-task','legacy-activation','legacy-owner',1,'default',0,'running',123,1,?,?,?,?)`,
		record.ID, record.Goal.ID, now, now, now+60000, now+60000); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO call_groups(call_id,system_id,group_name) VALUES('legacy-call',?,'account')`, record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_attempts(call_id,system_id,dispatched_at,reservation,state) VALUES('legacy-call',?,?,123,'running')`, record.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO quota_state(group_name,credit,updated_at,cooldown_until) VALUES('account',0,?,?)`, now, now+30000); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(context.Background(), filename)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.db.Close()
	if err := migrated.RecoverExecutions(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, err := migrated.GetSystem(context.Background(), localAdministrator, record.ID)
	if err != nil || saved.State != "stopped" || saved.UsedTokens != 23 || saved.ReservedTokens != 123 || saved.Execution.State != "unknown" || saved.Execution.AgentID != record.OperatorID {
		t.Fatalf("v2 accounting/history lost: %+v %v", saved, err)
	}
	for table, count := range map[string]int{"model_calls": 1, "model_attempts": 1, "call_groups": 1, "command_receipts": 1, "tasks": 0} {
		assertCount(t, migrated, table, count)
	}
	var cooldown int64
	if err := migrated.db.QueryRow(`SELECT cooldown_until FROM quota_state WHERE group_name='account'`).Scan(&cooldown); err != nil || cooldown != now+30000 {
		t.Fatalf("cooldown lost: %d %v", cooldown, err)
	}
	replay, err := migrated.CreateSystem(context.Background(), localAdministrator, "legacy-v2", CreateSystemCommand{Name: "research", Goal: &goal}, cfg, id)
	if err != nil || replay.ID != record.ID {
		t.Fatalf("legacy receipt lost: %+v %v", replay, err)
	}
	if _, err := migrated.db.Exec(`DELETE FROM audit`); err == nil {
		t.Fatal("audit became mutable")
	}
}
