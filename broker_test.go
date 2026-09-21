//go:build darwin || linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type brokerClock struct{ milliseconds atomic.Int64 }

func (c *brokerClock) now() time.Time { return time.UnixMilli(c.milliseconds.Load()) }

func fixtureBroker(t *testing.T, cfg configuration) (*modelBroker, *brokerClock, string) {
	t.Helper()
	store, id := fixtureStore(t, cfg)
	clock := &brokerClock{}
	clock.milliseconds.Store(time.Now().UnixMilli())
	b := newModelBroker(store, cfg, log.New(io.Discard, "", 0))
	b.now, b.lookup = clock.now, fixtureLookup
	t.Cleanup(b.client.CloseIdleConnections)
	return b, clock, id
}

func fixtureCall(t *testing.T, b *modelBroker, configID, key string, stream bool) executionRecord {
	t.Helper()
	goal := "fixture goal"
	record, err := b.store.createSystem(context.Background(), localAdministrator, "create-"+key, createSystemCommand{"research", &goal}, b.cfg, configID)
	if err != nil {
		t.Fatal(err)
	}
	started, err := b.store.startSystem(context.Background(), localAdministrator, "start-"+key, record.ID, "fixture-owner",
		startSystemCommand{ExpectedRevision: 1, Stream: stream}, b.cfg, b.now())
	if err != nil {
		t.Fatal(err)
	}
	return *started.Execution
}

func completion(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/json")
	data, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": text}, "finish_reason": "stop"}},
		"usage":   map[string]int{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18},
	})
	w.Write(data)
}

func providerConfigFor(t *testing.T, handler http.HandlerFunc) (configuration, *atomic.Int64) {
	t.Helper()
	count := &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		if r.Method != "POST" || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+fixtureProviderSecret {
			t.Error("provider request escaped its configured protocol or credential scope")
			w.WriteHeader(400)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	cfg := fixtureConfiguration(t)
	provider := cfg.Providers["primary"]
	provider.BaseURL = server.URL + "/v1"
	cfg.Providers["primary"] = provider
	return cfg, count
}

func queuedCall(t *testing.T, b *modelBroker, id, key string) executionRecord {
	t.Helper()
	e := fixtureCall(t, b, id, key, false)
	if err := b.queue(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	return e
}

func admitCall(t *testing.T, b *modelBroker, e executionRecord, want bool) executionRecord {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	current, admitted, err := b.admit(context.Background(), e, b.now())
	if err != nil || admitted != want {
		t.Fatalf("admit %s: admitted=%v err=%v state=%s reason=%s", e.CallID, admitted, err, current.State, current.Reason)
	}
	return current
}

func dispatchCall(t *testing.T, b *modelBroker, e executionRecord) {
	t.Helper()
	current := admitCall(t, b, e, true)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := b.perform(ctx, current)
	if !result.Known || result.Reason != "" {
		t.Fatalf("provider result: %+v", result)
	}
	if err := b.settle(ctx, current, result, b.now()); err != nil {
		t.Fatal(err)
	}
}

func TestBrokerSharedAdmissionAndFairness(t *testing.T) {
	cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) { completion(w, "tracked result") })
	q := cfg.QuotaGroups["account"]
	q.BurstRequests, q.MaxConcurrent = 2, 1
	cfg.QuotaGroups["account"] = q
	cfg.Models["alias"] = cfg.Models["default"]
	b, clock, id := fixtureBroker(t, cfg)
	a := queuedCall(t, b, id, "a")
	def := cfg.Systems["research"]
	def.Operator.Model = "alias"
	cfg.Systems["research"] = def
	c := queuedCall(t, b, id, "b")
	admitCall(t, b, c, false)
	current := admitCall(t, b, a, true)
	admitCall(t, b, c, false)
	ctx := context.Background()
	if err := b.settle(ctx, current, b.perform(ctx, current), b.now()); err != nil {
		t.Fatal(err)
	}
	dispatchCall(t, b, c)
	third := queuedCall(t, b, id, "c")
	admitCall(t, b, third, false)
	if count.Load() != 2 {
		t.Fatalf("dispatch count %d; want 2", count.Load())
	}
	clock.milliseconds.Add(1000)
	dispatchCall(t, b, third)
	if count.Load() != 3 {
		t.Fatalf("dispatch count after refill = %d", count.Load())
	}
	record, err := b.store.getSystem(ctx, localAdministrator, a.SystemID)
	if err != nil || record.UsedTokens != 18 || record.ReservedTokens != 0 || record.Execution.Response != "tracked result" {
		t.Fatalf("usage/result not reconciled: %+v %v", record, err)
	}
	replay, err := b.call(ctx, a)
	if err != nil || replay.State != "completed" || count.Load() != 3 {
		t.Fatalf("call replay duplicated dispatch: %+v %v", replay, err)
	}
}

func TestBrokerAllQuotaGroupsAndTokenWindow(t *testing.T) {
	cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) { completion(w, "small") })
	model := cfg.Models["default"]
	model.QuotaGroups = []string{"account", "organization"}
	cfg.Models["default"] = model
	q := cfg.QuotaGroups["account"]
	q.BurstRequests = 10
	q.MaxConcurrent = 10
	q.MaxWaitSeconds = 120
	cfg.QuotaGroups["account"], cfg.QuotaGroups["organization"] = q, q
	grants, err := cfg.grantsFor(cfg.Systems["research"])
	if err != nil {
		t.Fatal(err)
	}
	_, reservation, err := modelRequest(cfg, systemRecord{Configuration: cfg.Systems["research"], Grants: grants}, "fixture goal", false)
	if err != nil {
		t.Fatal(err)
	}
	q.TokensPerMinute = reservation
	cfg.QuotaGroups["organization"] = q
	b, clock, id := fixtureBroker(t, cfg)
	a := queuedCall(t, b, id, "a")
	second := queuedCall(t, b, id, "b")
	dispatchCall(t, b, a)
	admitCall(t, b, second, false)
	clock.milliseconds.Add(59999)
	waiting := admitCall(t, b, second, false)
	if waiting.State != "queued" {
		t.Fatalf("request expired before the token window boundary: %+v", waiting)
	}
	if count.Load() != 1 {
		t.Fatalf("token window dispatched %d requests", count.Load())
	}
	clock.milliseconds.Add(1)
	dispatchCall(t, b, second)
	if count.Load() != 2 {
		t.Fatalf("token window reset count = %d", count.Load())
	}
}

func TestBrokerQueueDeadlineIsExact(t *testing.T) {
	cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) { completion(w, "one") })
	q := cfg.QuotaGroups["account"]
	q.RequestsPerMinute, q.MaxWaitSeconds = 1, 2
	cfg.QuotaGroups["account"] = q
	b, clock, id := fixtureBroker(t, cfg)
	first := queuedCall(t, b, id, "first")
	second := queuedCall(t, b, id, "second")
	dispatchCall(t, b, first)
	clock.milliseconds.Add(1999)
	if result := admitCall(t, b, second, false); result.State != "queued" {
		t.Fatalf("expired early: %+v", result)
	}
	clock.milliseconds.Add(1)
	result := admitCall(t, b, second, false)
	if result.State != "rejected" || result.Reason != "queue deadline exceeded" || count.Load() != 1 {
		t.Fatalf("queue expiry: %+v; dispatched %d", result, count.Load())
	}
}

func TestBrokerDenialsAndAtomicReservations(t *testing.T) {
	for _, mode := range []string{"scope", "activation", "stopped", "revoked", "budget", "audit failure"} {
		t.Run(mode, func(t *testing.T) {
			cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) { completion(w, "must not happen") })
			b, _, id := fixtureBroker(t, cfg)
			e := fixtureCall(t, b, id, "deny", false)
			switch mode {
			case "scope":
				e.SystemID = "sys_00000000000000000000000000000000"
			case "activation":
				e.ActivationID = "activation_00000000000000000000000000000000"
			case "revoked":
				delete(cfg.Models, "default")
			case "stopped":
				if _, err := b.store.stopSystem(context.Background(), localAdministrator, "stop", e.SystemID, b.now()); err != nil {
					t.Fatal(err)
				}

			case "budget":
				if _, err := b.store.db.Exec(`UPDATE systems SET used_tokens=49000 WHERE system_id=?`, e.SystemID); err != nil {
					t.Fatal(err)
				}
			case "audit failure":
				if _, err := b.store.db.Exec(`CREATE TRIGGER fail_dispatch BEFORE INSERT ON audit WHEN NEW.action='model.dispatch'
				 BEGIN SELECT RAISE(ABORT,'fixture audit failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result, err := b.call(ctx, e)
			if err == nil && result.State != "rejected" && result.State != "canceled" {
				t.Fatalf("denied operation succeeded: %+v", result)
			}
			if count.Load() != 0 {
				t.Fatal("denied operation reached provider")
			}
			assertCount(t, b.store, "model_attempts", 0)
			var reserved int64
			if err := b.store.db.QueryRow(`SELECT COALESCE(SUM(reserved_tokens),0) FROM systems`).Scan(&reserved); err != nil || reserved != 0 {
				t.Fatalf("partial reservation: %d %v", reserved, err)
			}
			if mode == "audit failure" && b.available() {
				t.Fatal("accounting failure did not close admission")
			}
		})
	}
}

func TestBrokerClockRollbackCannotRefundTokenRate(t *testing.T) {
	cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) { completion(w, "bounded") })
	grants, err := cfg.grantsFor(cfg.Systems["research"])
	if err != nil {
		t.Fatal(err)
	}
	_, reservation, err := modelRequest(cfg, systemRecord{Configuration: cfg.Systems["research"], Grants: grants}, "fixture goal", false)
	if err != nil {
		t.Fatal(err)
	}
	q := cfg.QuotaGroups["account"]
	q.BurstRequests, q.TokensPerMinute = 3, 2*reservation
	cfg.QuotaGroups["account"] = q
	b, clock, id := fixtureBroker(t, cfg)
	first := queuedCall(t, b, id, "first")
	dispatchCall(t, b, first)
	clock.milliseconds.Add(-int64(time.Hour / time.Millisecond))
	second := queuedCall(t, b, id, "second")
	dispatchCall(t, b, second)
	third := queuedCall(t, b, id, "third")
	admitCall(t, b, third, false)
	if count.Load() != 2 {
		t.Fatalf("backward clock bypassed token window: %d", count.Load())
	}
}

func TestInitialGoalBudgetAndCommandReplay(t *testing.T) {
	cfg := fixtureConfiguration(t)
	b, _, id := fixtureBroker(t, cfg)
	goal := "fixture goal"
	record, err := b.store.createSystem(context.Background(), localAdministrator, "create", createSystemCommand{"research", &goal}, cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	def := record.Configuration
	def.Limits.TokenBudget = 60000
	revised, err := b.store.reviseSystem(context.Background(), localAdministrator, "revise", record.ID, reviseSystemCommand{ExpectedRevision: 1, Configuration: &def}, cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	started, err := b.store.startSystem(context.Background(), localAdministrator, "start", record.ID, "owner", startSystemCommand{ExpectedRevision: 2}, cfg, b.now())
	if err != nil || started.Execution.GoalBudget != record.Goal.TokenBudget || revised.Configuration.Limits.TokenBudget != 60000 {
		t.Fatalf("pending goal cap expanded or prevented a valid start: %+v %v", started.Execution, err)
	}
	var canceled atomic.Bool
	engine := &executionEngine{store: b.store, active: map[string]activation{record.ID: {callID: started.Execution.CallID, cancel: func() { canceled.Store(true) }}}}
	if _, err := engine.stop(context.Background(), localAdministrator, "stop", record.ID); err != nil {
		t.Fatal(err)
	}
	if !canceled.Load() {
		t.Fatal("stop did not cancel its activation")
	}
	// The new activation has a different call identity. Replaying the old stop
	// receipt must not apply an out-of-transaction cancellation to this run.
	canceled.Store(false)
	engine.active[record.ID] = activation{callID: "different-call", cancel: func() { canceled.Store(true) }}
	if _, err := engine.stop(context.Background(), localAdministrator, "stop", record.ID); err != nil {
		t.Fatal(err)
	}
	if canceled.Load() {
		t.Fatal("replayed stop canceled another activation")
	}
	if _, _, err := b.store.listCalls(context.Background(), "foreign-owner", record.ID, 0); !errors.Is(err, errSystemNotFound) {
		t.Fatalf("call history leaked across owners: %v", err)
	}
	if err := b.store.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.stop(context.Background(), localAdministrator, "emergency", record.ID); err == nil || !canceled.Load() {
		t.Fatal("storage failure prevented emergency cancellation")
	}
}

func TestBrokerQueueBoundsAndCancellation(t *testing.T) {
	cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) { completion(w, "unused") })
	q := cfg.QuotaGroups["account"]
	q.QueueCapacity = 1
	cfg.QuotaGroups["account"] = q
	b, _, id := fixtureBroker(t, cfg)
	e := fixtureCall(t, b, id, "one", false)
	goal := "fixture goal"
	record, err := b.store.createSystem(context.Background(), localAdministrator, "two", createSystemCommand{"research", &goal}, cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.store.startSystem(context.Background(), localAdministrator, "start-two", record.ID, "fixture-owner", startSystemCommand{ExpectedRevision: 1}, cfg, b.now())
	if err == nil || !strings.Contains(err.Error(), "queue") {
		t.Fatalf("queue bound: %v", err)
	}
	if err := b.queue(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	result, err := b.cancelQueued(e)
	if err != nil || result.State != "canceled" || count.Load() != 0 {
		t.Fatalf("queued cancellation: %+v %v", result, err)
	}
	assertCount(t, b.store, "model_attempts", 0)
}

func TestBrokerThrottleRecoveryAndUnknownUsage(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint("unknown=", unknown), func(t *testing.T) {
			cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) {
				if unknown {
					io.WriteString(w, `{"choices":[{"index":0,"message":{"content":"ambiguous"},"finish_reason":"stop"}]}`)
					return
				}
				w.Header().Set("Retry-After", "10")
				w.WriteHeader(429)
			})
			q := cfg.QuotaGroups["account"]
			q.BurstRequests = 2
			cfg.QuotaGroups["account"] = q
			b, clock, id := fixtureBroker(t, cfg)
			e := queuedCall(t, b, id, "first")
			current := admitCall(t, b, e, true)
			result := b.perform(context.Background(), current)
			if err := b.settle(context.Background(), current, result, b.now()); err != nil {
				t.Fatal(err)
			}
			before, err := b.store.getSystem(context.Background(), localAdministrator, e.SystemID)
			if err != nil {
				t.Fatal(err)
			}
			if unknown && (before.ReservedTokens != e.Reservation || before.Execution.State != "unknown" || before.Execution.UsageKnown) {
				t.Fatalf("unknown usage refunded: %+v", before)
			}
			if !unknown && (before.ReservedTokens != 0 || before.Execution.State != "queued") {
				t.Fatalf("throttle reservation: %+v", before)
			}
			if err := b.store.db.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := openStore(context.Background(), filepath.Join(cfg.DataDir, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { reopened.db.Close() })
			if err := reopened.recoverExecutions(context.Background()); err != nil {
				t.Fatal(err)
			}
			b.store = reopened
			after, err := reopened.getSystem(context.Background(), localAdministrator, e.SystemID)
			wantState := "running"
			if unknown {
				wantState = "stopped"
			}
			if err != nil || after.ReservedTokens != before.ReservedTokens || after.State != wantState {
				t.Fatalf("restart replenished state: %+v %v", after, err)
			}
			next := queuedCall(t, b, id, "next")
			if !unknown {
				admitCall(t, b, next, false)
				clock.milliseconds.Add(10000)
			}
			admitCall(t, b, next, true)
			if count.Load() != 1 {
				t.Fatal("recovery replayed a provider call")
			}
		})
	}
}

func TestRetryCannotOverfillSharedQueue(t *testing.T) {
	cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Retry-After", "10"); w.WriteHeader(429) })
	q := cfg.QuotaGroups["account"]
	q.QueueCapacity, q.BurstRequests = 1, 2
	cfg.QuotaGroups["account"] = q
	b, clock, id := fixtureBroker(t, cfg)
	first := queuedCall(t, b, id, "first")
	current := admitCall(t, b, first, true)
	queuedCall(t, b, id, "second")
	result := b.perform(context.Background(), current)
	clock.milliseconds.Add(2000)
	if err := b.settle(context.Background(), current, result, b.now()); err != nil {
		t.Fatal(err)
	}
	record, err := b.store.getSystem(context.Background(), localAdministrator, first.SystemID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Execution.State != "failed" || !strings.Contains(record.Execution.Reason, "queue capacity") || record.ReservedTokens != 0 {
		t.Fatalf("retry overfilled queue: %+v", record.Execution)
	}
	quotas, err := b.quotas(context.Background())
	if err != nil || quotas[0].Queued != 1 || quotas[0].CooldownUntil != b.now().Add(10*time.Second).UnixMilli() || count.Load() != 1 {
		t.Fatalf("retry queue/cooldown accounting: %+v %v", quotas, err)
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
	legacy := &stateStore{db: db}
	if _, err := db.Exec(schemaV1); err != nil {
		t.Fatal(err)
	}
	id, err := legacy.rememberConfiguration(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	goal := "preserve this goal"
	record, err := legacy.createSystem(context.Background(), localAdministrator, "legacy", createSystemCommand{"research", &goal}, cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE systems SET used_tokens=23 WHERE system_id=?`, record.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := openStore(context.Background(), filename)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.db.Close()
	saved, err := migrated.getSystem(context.Background(), localAdministrator, record.ID)
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
	replay, err := migrated.createSystem(context.Background(), localAdministrator, "legacy", createSystemCommand{"research", &goal}, cfg, id)
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
	legacy := &stateStore{db: db}
	id, err := legacy.rememberConfiguration(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	goal := "legacy dispatched goal"
	record, err := legacy.createSystem(context.Background(), localAdministrator, "legacy-v2", createSystemCommand{"research", &goal}, cfg, id)
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
	migrated, err := openStore(context.Background(), filename)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.db.Close()
	if err := migrated.recoverExecutions(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, err := migrated.getSystem(context.Background(), localAdministrator, record.ID)
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
	replay, err := migrated.createSystem(context.Background(), localAdministrator, "legacy-v2", createSystemCommand{"research", &goal}, cfg, id)
	if err != nil || replay.ID != record.ID {
		t.Fatalf("legacy receipt lost: %+v %v", replay, err)
	}
	if _, err := migrated.db.Exec(`DELETE FROM audit`); err == nil {
		t.Fatal("audit became mutable")
	}
}

func TestBrokerRejectsOversizedAndImpossibleRequests(t *testing.T) {
	cfg := fixtureConfiguration(t)
	b, _, id := fixtureBroker(t, cfg)
	goal := strings.Repeat("\x01", 32768)
	record, err := b.store.createSystem(context.Background(), localAdministrator, "large", createSystemCommand{"research", &goal}, cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.store.startSystem(context.Background(), localAdministrator, "large-start", record.ID, "owner", startSystemCommand{ExpectedRevision: 1}, cfg, b.now())
	var field *fieldError
	if !errors.As(err, &field) {
		t.Fatalf("oversized model request accepted: %v", err)
	}
	assertCount(t, b.store, "model_calls", 0)
	assertCount(t, b.store, "model_attempts", 0)
}
