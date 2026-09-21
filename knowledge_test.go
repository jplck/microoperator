//go:build darwin || linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func knowledgeConfiguration(t *testing.T) configuration {
	cfg := fixtureConfiguration(t)
	def := cfg.Systems["research"]
	def.Tools = []string{"runtime.memory.put", "runtime.memory.search", "runtime.schedule.create", "runtime.schedule.cancel", "runtime.events.subscribe", "runtime.events.unsubscribe", "runtime.task.wait", "runtime.agent.propose"}
	def.Operator.Tools = append([]string{}, def.Tools...)
	cfg.Systems["research"] = def
	q := cfg.QuotaGroups["account"]
	q.BurstRequests = 20
	q.TokensPerMinute = 1000000
	cfg.QuotaGroups["account"] = q
	return cfg
}

func knowledgeCall(t *testing.T, engine *executionEngine, id, key string) executionRecord {
	t.Helper()
	prompt := "knowledge goal"
	s, err := engine.store.createSystem(context.Background(), localAdministrator, "create-"+key, createSystemCommand{"research", &prompt}, engine.cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	s, err = engine.store.startSystem(context.Background(), localAdministrator, "start-"+key, s.ID, engine.owner, startSystemCommand{ExpectedRevision: 1, LifetimeSeconds: 3600}, engine.cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return *s.Execution
}

func knowledgeTransaction(t *testing.T, engine *executionEngine, e executionRecord, fn func(*sql.Tx, taskRecord)) {
	t.Helper()
	tx, err := engine.store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	task, err := readTask(context.Background(), tx, e.SystemID, e.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	fn(tx, task)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func memoryResult(t *testing.T, value any) []memoryEntry {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Untrusted bool          `json:"untrusted_data"`
		Entries   []memoryEntry `json:"entries"`
		Next      string        `json:"next"`
	}
	if err := json.Unmarshal(data, &result); err != nil || !result.Untrusted {
		t.Fatalf("memory was treated as instructions: %s %v", data, err)
	}
	return result.Entries
}

func TestMemoryScopeRevisionApprovalAndRetention(t *testing.T) {
	cfg := knowledgeConfiguration(t)
	engine, id := fixtureTeamEngine(t, cfg)
	first := knowledgeCall(t, engine, id, "first")
	second := knowledgeCall(t, engine, id, "second")
	now := time.Now()
	var note, shared memoryEntry
	knowledgeTransaction(t, engine, first, func(tx *sql.Tx, task taskRecord) {
		var err error
		note, err = putMemory(context.Background(), tx, task, memoryCommand{Scope: "agent", Content: "private first note", Evidence: "fixture", Confidence: 90, RetentionSeconds: 60}, false, now)
		if err != nil {
			t.Fatal(err)
		}
		shared, err = putMemory(context.Background(), tx, task, memoryCommand{Scope: "system", Content: "shared proposal", Evidence: "unreviewed", Confidence: 100, RetentionSeconds: 60}, false, now)
		if err != nil || shared.State != "pending" {
			t.Fatalf("shared note bypassed approval: %+v %v", shared, err)
		}
		value, err := searchMemory(context.Background(), tx, task, memoryQuery{Scope: "system", Query: "proposal"}, false, now)
		if err != nil || len(memoryResult(t, value)) != 0 {
			t.Fatal("unapproved shared fact was exposed")
		}
		if _, err := searchMemory(context.Background(), tx, task, memoryQuery{Scope: "agent", Owner: second.AgentID, Query: ""}, false, now); err == nil {
			t.Fatal("caller selected a foreign memory owner")
		}
		note, err = putMemory(context.Background(), tx, task, memoryCommand{ID: note.ID, Revision: 1, Scope: "agent", Content: "revised first note", Evidence: "fixture", Confidence: 80, RetentionSeconds: 60}, false, now)
		if err != nil || note.Revision != 2 {
			t.Fatalf("revision: %+v %v", note, err)
		}
		if _, err := putMemory(context.Background(), tx, task, memoryCommand{ID: note.ID, Revision: 1, Scope: "agent", Content: "stale", RetentionSeconds: 60}, false, now); err != errRevisionConflict {
			t.Fatalf("stale memory update: %v", err)
		}
		if _, err := tx.Exec(`UPDATE memory_revisions SET content='mutated'`); err == nil {
			t.Fatal("memory history mutated")
		}
	})
	knowledgeTransaction(t, engine, second, func(tx *sql.Tx, task taskRecord) {
		value, err := searchMemory(context.Background(), tx, task, memoryQuery{Scope: "agent", Query: "first"}, false, now)
		if err != nil || len(memoryResult(t, value)) != 0 {
			t.Fatal("cross-system result or count leaked")
		}
	})
	handler := newControlHandler(engine.store, cfg, id, fixtureControlToken, engine.logger, engine)
	response := controlRequest(handler, "POST", "/v1/systems/"+first.SystemID+"/memory/"+shared.ID+"/approve", "approve", `{"expected_revision":1}`, fixtureControlToken)
	if response.Code != 200 {
		t.Fatalf("approve: %s", response.Body.String())
	}
	if err := engine.store.recoverExecutions(context.Background()); err != nil {
		t.Fatal(err)
	}
	knowledgeTransaction(t, engine, first, func(tx *sql.Tx, task taskRecord) {
		value, err := searchMemory(context.Background(), tx, task, memoryQuery{Scope: "agent", Query: "first"}, false, now)
		if err != nil {
			t.Fatal(err)
		}
		entries := memoryResult(t, value)
		if len(entries) != 1 || entries[0].Revision != 2 || entries[0].Content != "revised first note" {
			t.Fatalf("memory recovery: %+v", entries)
		}
		value, err = searchMemory(context.Background(), tx, task, memoryQuery{Scope: "system", Query: "proposal"}, false, now)
		if err != nil || len(memoryResult(t, value)) != 1 {
			t.Fatal("approved fact unavailable")
		}
		if err := engine.pumpWakeups(context.Background(), tx, now.Add(61*time.Second)); err != nil {
			t.Fatal(err)
		}
		value, err = searchMemory(context.Background(), tx, task, memoryQuery{Scope: "agent", Query: ""}, false, now.Add(61*time.Second))
		if err != nil || len(memoryResult(t, value)) != 0 {
			t.Fatal("expired memory remained visible")
		}
	})
	assertCount(t, engine.store, "memory_revisions", 0)
	assertCount(t, engine.store, "model_attempts", 0)
}

func TestCronCalendarAndDST(t *testing.T) {
	zone, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	spring := time.Date(2026, 3, 8, 0, 0, 0, 0, zone)
	next, err := nextCron("30 2 * * *", "America/New_York", spring)
	if err != nil || !next.Equal(time.Date(2026, 3, 9, 2, 30, 0, 0, zone)) {
		t.Fatalf("nonexistent spring time: %s %v", next, err)
	}
	fall := time.Date(2026, 11, 1, 0, 0, 0, 0, zone)
	first, err := nextCron("30 1 * * *", "America/New_York", fall)
	if err != nil {
		t.Fatal(err)
	}
	second, err := nextCron("30 1 * * *", "America/New_York", first)
	if err != nil || second.Sub(first) != time.Hour || first.Hour() != 1 || second.Hour() != 1 {
		t.Fatalf("repeated fall occurrence: %s %s %v", first, second, err)
	}
	for _, expression := range []string{"@every 1s", "* * * * * *", "0 0 31 2 *", "CRON_TZ=UTC * * * * *"} {
		if _, err := nextCron(expression, "UTC", time.Now()); err == nil {
			t.Fatalf("unsupported cron accepted: %s", expression)
		}
	}
}

func TestScheduleCoalescesAndReplaysOnlyItsReceipt(t *testing.T) {
	cfg := knowledgeConfiguration(t)
	engine, id := fixtureTeamEngine(t, cfg)
	e := knowledgeCall(t, engine, id, "timer")
	now := time.Now()
	var schedule string
	knowledgeTransaction(t, engine, e, func(tx *sql.Tx, task taskRecord) {
		var err error
		schedule, err = createSchedule(context.Background(), tx, task, scheduleCommand{Cron: "* * * * *", Timezone: "UTC", Content: "once after downtime", Budget: 3}, task.AgentID, now)
		if err != nil {
			t.Fatal(err)
		}
	})
	wait := claimAction(t, engine, "runtime.task.wait", map[string]string{"reason": "waiting for timer"})
	invokeAction(t, engine, wait)
	finishAction(t, engine, wait)
	if err := engine.store.recoverExecutions(context.Background()); err != nil {
		t.Fatal(err)
	}
	future := now.Add(5 * time.Minute)
	_, wake, found, err := engine.claim(context.Background(), future)
	if err != nil || !found {
		t.Fatalf("timer wake: %v %v", found, err)
	}
	if strings.Count(string(wake.Request), "once after downtime") != 1 {
		t.Fatalf("missed occurrences amplified: %s", wake.Request)
	}
	assertCount(t, engine.store, "schedule_occurrences", 1)
	assertCount(t, engine.store, "model_calls", 2)
	var remaining int
	var next int64
	if err := engine.store.db.QueryRow(`SELECT remaining,next_due FROM schedules WHERE schedule_id=?`, schedule).Scan(&remaining, &next); err != nil || remaining != 2 || next <= future.UnixMilli() {
		t.Fatalf("cursor/budget: %d %d %v", remaining, next, err)
	}
	if err := engine.store.recoverExecutions(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, again, found, err := engine.claim(context.Background(), future)
	if err != nil || !found || again.CallID != wake.CallID || strings.Count(string(again.Request), "once after downtime") != 1 {
		t.Fatalf("timer replayed work: %+v %v", again, err)
	}
	assertCount(t, engine.store, "schedule_occurrences", 1)
	assertCount(t, engine.store, "model_attempts", 1)
}

func TestSchedulesRespectPauseStopRevocationAndExpiry(t *testing.T) {
	for _, mode := range []string{"pause", "stop", "revoke", "expire"} {
		t.Run(mode, func(t *testing.T) {
			cfg := knowledgeConfiguration(t)
			engine, id := fixtureTeamEngine(t, cfg)
			e := knowledgeCall(t, engine, id, mode)
			now := time.Now()
			knowledgeTransaction(t, engine, e, func(tx *sql.Tx, task taskRecord) {
				if _, err := createSchedule(context.Background(), tx, task, scheduleCommand{At: now.Add(time.Second).Format(time.RFC3339Nano), Content: "bounded", Budget: 1}, task.AgentID, now); err != nil {
					t.Fatal(err)
				}
			})
			future := now.Add(2 * time.Second)
			switch mode {
			case "pause":
				if _, err := engine.store.db.Exec(`UPDATE systems SET state='paused' WHERE system_id=?`, e.SystemID); err != nil {
					t.Fatal(err)
				}
			case "stop":
				if _, err := engine.store.stopSystem(context.Background(), localAdministrator, "stop", e.SystemID, now); err != nil {
					t.Fatal(err)
				}
			case "revoke":
				if _, err := engine.store.db.Exec(`INSERT INTO tool_revocations(system_id,name,version) VALUES(?,'runtime.schedule.create',1)`, e.SystemID); err != nil {
					t.Fatal(err)
				}
			case "expire":
				future = now.Add(2 * time.Hour)
			}
			if _, _, found, err := engine.claim(context.Background(), future); err != nil || found {
				t.Fatalf("ineligible timer dispatched: %v %v", found, err)
			}
			assertCount(t, engine.store, "schedule_occurrences", 0)
			assertCount(t, engine.store, "model_attempts", 0)
		})
	}
}

func TestMemorySubscriptionsAuthorizeBeforeNotification(t *testing.T) {
	cfg := knowledgeConfiguration(t)
	engine, id := fixtureTeamEngine(t, cfg)
	e := knowledgeCall(t, engine, id, "subscribe")
	now := time.Now()
	knowledgeTransaction(t, engine, e, func(tx *sql.Tx, task taskRecord) {
		if _, err := createSubscription(context.Background(), tx, task, subscriptionCommand{Type: "memory.changed", Scope: "agent", Budget: 2}, task.AgentID, now); err != nil {
			t.Fatal(err)
		}
		if err := engine.notifySubscriptions(context.Background(), tx, task, "memory.changed", "agent", task.AgentID, "", "self", now); err != nil {
			t.Fatal(err)
		}
		foreign := task
		foreign.ID = ""
		foreign.AgentID = localAdministrator
		if err := engine.notifySubscriptions(context.Background(), tx, foreign, "memory.changed", "agent", "other", "", "private", now); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := tx.QueryRow(`SELECT count(*) FROM events WHERE type='memory.changed'`).Scan(&count); err != nil || count != 0 {
			t.Fatal("self/private memory caused a wakeup")
		}
		if err := engine.notifySubscriptions(context.Background(), tx, foreign, "memory.changed", "agent", task.AgentID, "", "approved-change", now); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(`SELECT count(*) FROM events WHERE type='memory.changed'`).Scan(&count); err != nil || count != 1 {
			t.Fatalf("authorized notification missing: %d %v", count, err)
		}
		if _, err := tx.Exec(`INSERT INTO tool_revocations(system_id,name,version) VALUES(?,'runtime.memory.search',1)`, task.SystemID); err != nil {
			t.Fatal(err)
		}
		if err := engine.notifySubscriptions(context.Background(), tx, foreign, "memory.changed", "agent", task.AgentID, "", "revoked-change", now); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(`SELECT count(*) FROM events WHERE type='memory.changed'`).Scan(&count); err != nil || count != 1 {
			t.Fatal("revoked memory grant received notification")
		}
	})
}

func TestGoalLifetimeIsExplicitAndBounded(t *testing.T) {
	cfg := knowledgeConfiguration(t)
	engine, id := fixtureTeamEngine(t, cfg)
	e := knowledgeCall(t, engine, id, "lifetime")
	if remaining := time.Until(time.UnixMilli(e.Deadline)); remaining < 59*time.Minute || remaining > time.Hour {
		t.Fatalf("explicit goal lifetime ignored: %s", remaining)
	}
	for _, seconds := range []int64{-1, 86401} {
		s, err := engine.store.createSystem(context.Background(), localAdministrator, fmt.Sprintf("bad-%d", seconds), createSystemCommand{Launch: "research"}, cfg, id)
		if err != nil {
			t.Fatal(err)
		}
		goal := "invalid lifetime"
		if _, err := engine.store.startSystem(context.Background(), localAdministrator, fmt.Sprintf("bad-start-%d", seconds), s.ID, engine.owner, startSystemCommand{ExpectedRevision: 1, Goal: &goal, LifetimeSeconds: seconds}, cfg, time.Now()); err == nil {
			t.Fatal("unbounded lifetime accepted")
		}
	}
}
