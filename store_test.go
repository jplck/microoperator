//go:build darwin || linux

package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func fixtureStore(t *testing.T, cfg configuration) (*stateStore, string) {
	t.Helper()
	if err := cfg.validate(fixtureLookup); err != nil {
		t.Fatal(err)
	}
	lock, err := lockStateDirectory(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	})
	store, err := openStore(context.Background(), filepath.Join(cfg.DataDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.db.Close(); err != nil {
			t.Error(err)
		}
	})
	configID, err := store.rememberConfiguration(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return store, configID
}

func assertCount(t *testing.T, store *stateStore, table string, want int) {
	t.Helper()
	var got int
	// Table names here are fixed test literals, never request-supplied identifiers.
	if err := store.db.QueryRow("SELECT count(*) FROM " + table).Scan(&got); err != nil || got != want {
		t.Fatalf("%s rows = %d, %v; want %d", table, got, err, want)
	}
}

func TestStoreIsolationRevisionsAndRecovery(t *testing.T) {
	ctx := context.Background()
	cfg := fixtureConfiguration(t)
	store, configID := fixtureStore(t, cfg)
	goal := "Use only fixture evidence."
	command := createSystemCommand{"research", &goal}
	first, err := store.createSystem(ctx, localAdministrator, "first", command, cfg, configID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.createSystem(ctx, localAdministrator, "second", command, cfg, configID)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID || first.OperatorID == second.OperatorID ||
		first.Goal.ID == second.Goal.ID || first.Goal.SystemID != first.ID ||
		first.State != "inactive" || second.State != "inactive" {
		t.Fatal("created systems are not independent, inactive runtime identities")
	}
	replay, err := store.createSystem(ctx, localAdministrator, "first", command, cfg, configID)
	if err != nil || !reflect.DeepEqual(replay, first) {
		t.Fatalf("create replay changed result: %+v, %v", replay, err)
	}
	otherGoal := "Different request."
	_, err = store.createSystem(ctx, localAdministrator, "first", createSystemCommand{"research", &otherGoal}, cfg, configID)
	if !errors.Is(err, errCommandConflict) {
		t.Fatalf("key reuse: %v", err)
	}
	if _, err := store.getSystem(ctx, "different-principal", first.ID); !errors.Is(err, errSystemNotFound) {
		t.Fatalf("foreign principal read: %v", err)
	}
	foreign, _, err := store.listSystems(ctx, "different-principal", "")
	if err != nil || len(foreign) != 0 {
		t.Fatalf("foreign listing: %+v, %v", foreign, err)
	}

	// Simulate already-consumed usage without a model call. Revision changes and
	// process restarts must never manufacture a fresh lifetime allowance.
	if _, err := store.db.Exec("UPDATE systems SET used_tokens = 10 WHERE system_id = ?", first.ID); err != nil {
		t.Fatal(err)
	}
	next := first.Configuration
	next.Operator.Prompt = "A revised, explicit prompt."
	next.Limits.TokenBudget = 60000
	update := reviseSystemCommand{first.Revision, &next}
	revised, err := store.reviseSystem(ctx, localAdministrator, "revise", first.ID, update, cfg, configID)
	if err != nil {
		t.Fatal(err)
	}
	if revised.Revision != 2 || revised.UsedTokens != 10 || revised.OperatorID != first.OperatorID {
		t.Fatalf("revision/usage changed incorrectly: %+v", revised)
	}
	replay, err = store.reviseSystem(ctx, localAdministrator, "revise", first.ID, update, cfg, configID)
	if err != nil || !reflect.DeepEqual(replay, revised) {
		t.Fatalf("revision replay: %+v, %v", replay, err)
	}
	if _, err := store.reviseSystem(ctx, localAdministrator, "stale", first.ID, update, cfg, configID); !errors.Is(err, errRevisionConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	belowUsage := next
	belowUsage.Limits.TokenBudget = 9
	if _, err := store.reviseSystem(ctx, localAdministrator, "too-low", first.ID, reviseSystemCommand{2, &belowUsage}, cfg, configID); err == nil {
		t.Fatal("budget below consumed usage accepted")
	}
	for _, table := range []string{"system_revisions", "config_snapshots", "audit"} {
		if _, err := store.db.Exec("DELETE FROM " + table); err == nil {
			t.Fatalf("immutable %s could be deleted", table)
		}
	}
	assertCount(t, store, "systems", 2)
	assertCount(t, store, "goals", 2)
	assertCount(t, store, "system_revisions", 3)
	assertCount(t, store, "audit", 3)
	assertCount(t, store, "command_receipts", 3)
	var snapshot string
	if err := store.db.QueryRow("SELECT configuration FROM config_snapshots WHERE config_id = ?", configID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(snapshot, fixtureProviderSecret) || strings.Contains(snapshot, fixtureControlToken) {
		t.Fatal("credential value persisted")
	}
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStore(ctx, filepath.Join(cfg.DataDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()
	got, err := reopened.getSystem(ctx, localAdministrator, first.ID)
	if err != nil || !reflect.DeepEqual(got, revised) {
		t.Fatalf("restart changed first system: %+v, %v", got, err)
	}
	got, err = reopened.getSystem(ctx, localAdministrator, second.ID)
	if err != nil || !reflect.DeepEqual(got, second) {
		t.Fatalf("restart changed second system: %+v, %v", got, err)
	}
	got, err = reopened.createSystem(ctx, localAdministrator, "first", command, cfg, configID)
	if err != nil || got.ID != first.ID {
		t.Fatalf("restart lost receipt: %+v, %v", got, err)
	}
	assertCount(t, reopened, "systems", 2)
}

func TestStoreAtomicityCancellationAndConcurrency(t *testing.T) {
	ctx := context.Background()
	cfg := fixtureConfiguration(t)
	store, configID := fixtureStore(t, cfg)
	if _, err := store.db.Exec(`CREATE TRIGGER reject_fixture_audit BEFORE INSERT ON audit
		BEGIN SELECT RAISE(ABORT, 'fixture audit unavailable'); END;`); err != nil {
		t.Fatal(err)
	}
	command := createSystemCommand{Launch: "research"}
	if _, err := store.createSystem(ctx, localAdministrator, "retry", command, cfg, configID); err == nil {
		t.Fatal("audit failure did not block creation")
	}
	for _, table := range []string{"systems", "system_revisions", "audit", "command_receipts"} {
		assertCount(t, store, table, 0)
	}
	if _, err := store.db.Exec("DROP TRIGGER reject_fixture_audit"); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.createSystem(canceled, localAdministrator, "canceled", command, cfg, configID); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled command: %v", err)
	}
	assertCount(t, store, "systems", 0)
	record, err := store.createSystem(ctx, localAdministrator, "retry", command, cfg, configID)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			replay, err := store.createSystem(ctx, localAdministrator, "retry", command, cfg, configID)
			if err == nil && replay.ID != record.ID {
				err = errors.New("duplicate system created")
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertCount(t, store, "systems", 1)
	assertCount(t, store, "audit", 1)
	results = make(chan error, 2)
	for _, key := range []string{"one", "two"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			next := record.Configuration
			next.Operator.Prompt = key
			_, err := store.reviseSystem(ctx, localAdministrator, key, record.ID, reviseSystemCommand{1, &next}, cfg, configID)
			results <- err
		}(key)
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, errRevisionConflict):
			conflict++
		default:
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("concurrent revisions: success=%d conflict=%d", success, conflict)
	}
	assertCount(t, store, "system_revisions", 2)
	assertCount(t, store, "audit", 2)
}

func TestStoreRejectsAnUnrecognizedDatabase(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "state.db")
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	foreign, err := sql.Open("sqlite", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.Exec("CREATE TABLE unrelated (value TEXT); INSERT INTO unrelated VALUES ('fixture')"); err != nil {
		foreign.Close()
		t.Fatal(err)
	}
	if err := foreign.Close(); err != nil {
		t.Fatal(err)
	}
	if store, err := openStore(context.Background(), filename); err == nil {
		store.db.Close()
		t.Fatal("an unrelated database was adopted as daemon state")
	}
	foreign, err = sql.Open("sqlite", filename)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	var value, journalMode string
	if err := foreign.QueryRow("SELECT value FROM unrelated").Scan(&value); err != nil || value != "fixture" {
		t.Fatalf("unrelated data changed: %q, %v", value, err)
	}
	if err := foreign.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil || journalMode != "delete" {
		t.Fatalf("unrelated journal settings changed: %q, %v", journalMode, err)
	}
}

func TestPinnedDefinitionsAndFutureSchema(t *testing.T) {
	ctx := context.Background()
	cfg := fixtureConfiguration(t)
	store, configID := fixtureStore(t, cfg)
	record, err := store.createSystem(ctx, localAdministrator, "create", createSystemCommand{Launch: "research"}, cfg, configID)
	if err != nil {
		t.Fatal(err)
	}
	originalDigest := record.Grants.DefinitionsDigest
	template := cfg.Systems["research"]
	template.Operator.Prompt = "Changed launch template."
	cfg.Systems["research"] = template
	inspected, err := cfg.inspect(record)
	if err != nil || inspected.BlockedReason != "" || inspected.Configuration.Operator.Prompt == template.Operator.Prompt {
		t.Fatalf("template rewrote an instance: %+v, %v", inspected, err)
	}
	tool := cfg.Tools["shared.notes"]
	tool.Version++
	tool.Content = "Changed skill."
	cfg.Tools["shared.notes"] = tool
	inspected, err = cfg.inspect(record)
	if err != nil || inspected.BlockedReason == "" || inspected.Grants.DefinitionsDigest != originalDigest ||
		inspected.Grants.OperatorTools[0].Version != 1 {
		t.Fatalf("changed definitions were silently substituted: %+v, %v", inspected, err)
	}
	newConfigID, err := store.rememberConfiguration(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	revised, err := store.reviseSystem(ctx, localAdministrator, "new-definitions", record.ID,
		reviseSystemCommand{1, &record.Configuration}, cfg, newConfigID)
	if err != nil {
		t.Fatal(err)
	}
	inspected, err = cfg.inspect(revised)
	if err != nil || inspected.BlockedReason != "" || inspected.Grants.OperatorTools[0].Version != 2 {
		t.Fatalf("explicit revision did not select current definitions: %+v, %v", inspected, err)
	}
	delete(cfg.Models, "default")
	inspected, err = cfg.inspect(record)
	if err != nil || !strings.Contains(inspected.BlockedReason, "operator.model") {
		t.Fatalf("removed model was not reported: %+v, %v", inspected, err)
	}
	if _, err := store.db.Exec("PRAGMA user_version = 3"); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := openStore(ctx, filepath.Join(cfg.DataDir, "state.db")); err == nil {
		reopened.db.Close()
		t.Fatal("future database schema was accepted")
	}
}
