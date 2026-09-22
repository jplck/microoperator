package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func continuousCommand(t *testing.T, lifetime int64) StartSystemCommand {
	t.Helper()
	var command StartSystemCommand
	if err := json.Unmarshal([]byte(fmt.Sprintf(`{"expected_revision":1,"continuous":true,"lifetime_seconds":%d}`, lifetime)), &command); err != nil {
		t.Fatal(err)
	}
	return command
}

func continuousFixture(t *testing.T, lifetime int64) (*fixtureWorkflow, ExecutionRecord) {
	t.Helper()
	cfg := knowledgeConfiguration(t)
	cfg.Bootstrap.Tools = append(cfg.Bootstrap.Tools, "runtime.task.delegate", "runtime.agent.list")
	cfg.Bootstrap.Operator.Tools = append([]string{}, cfg.Bootstrap.Tools...)
	engine, id := fixtureTeamEngine(t, cfg)
	ctx := context.Background()
	s, err := engine.store.CreateSystem(ctx, localAdministrator, "continuous-create", CreateSystemCommand{Name: "continuous", Goal: fixtureGoal()}, cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.store.StartSystem(ctx, localAdministrator, "continuous-start", s.ID, engine.owner, continuousCommand(t, lifetime), cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	e := ExecutionRecord{SystemID: s.ID, AgentID: s.OperatorID, GoalID: s.Goal.ID}
	if err := engine.store.db.QueryRow(`SELECT task_id FROM tasks WHERE system_id=? AND goal_id=? AND parent_task=''`, s.ID, e.GoalID).Scan(&e.TaskID); err != nil {
		t.Fatal(err)
	}
	return engine, e
}

func continuousInt(t *testing.T, engine *fixtureWorkflow, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := engine.store.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func continuousClaim(t *testing.T, engine *fixtureWorkflow, now time.Time) ExecutionRecord {
	t.Helper()
	_, e, found, err := engine.claim(context.Background(), now)
	if err != nil || !found {
		t.Fatalf("continuous claim: found=%v err=%v", found, err)
	}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var mode struct{ Continuous bool }
	if err := json.Unmarshal(data, &mode); err != nil || !mode.Continuous {
		t.Fatalf("execution lost continuous mode: %s %v", data, err)
	}
	return e
}

func continuousFinish(t *testing.T, engine *fixtureWorkflow, e ExecutionRecord, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := engine.broker.queue(ctx, e); err != nil {
		t.Fatal(err)
	}
	admitted, ok, err := engine.broker.admit(ctx, e, now)
	if err != nil || !ok {
		t.Fatalf("completion admission: %+v %v", admitted, err)
	}
	if err := engine.broker.settle(ctx, admitted, ProviderResult{Known: true, Input: 11, Output: 7, Text: "continuous answer"}, now); err != nil {
		t.Fatal(err)
	}
	if err := engine.finishDelivery(ctx, e, nil, false); err != nil {
		t.Fatal(err)
	}
	task, err := engine.store.Task(ctx, e.SystemID, e.TaskID)
	if err != nil || task.State != "completed" {
		t.Fatalf("plain completion did not finish only its task: %+v %v", task, err)
	}
}

func continuousIdle(t *testing.T, engine *fixtureWorkflow, root ExecutionRecord, now time.Time) {
	t.Helper()
	calls := continuousInt(t, engine, `SELECT count(*) FROM model_calls`)
	used := continuousInt(t, engine, `SELECT used_tokens FROM systems WHERE system_id=?`, root.SystemID)
	turns := continuousInt(t, engine, `SELECT sum(turns) FROM agent_usage WHERE system_id=?`, root.SystemID)
	if _, _, found, err := engine.claim(context.Background(), now); err != nil || found {
		t.Fatalf("idle goal dispatched: %v %v", found, err)
	}
	if continuousInt(t, engine, `SELECT count(*) FROM model_calls`) != calls ||
		continuousInt(t, engine, `SELECT used_tokens FROM systems WHERE system_id=?`, root.SystemID) != used ||
		continuousInt(t, engine, `SELECT sum(turns) FROM agent_usage WHERE system_id=?`, root.SystemID) != turns {
		t.Fatal("idle claim created work or reset accounting")
	}
	var system, goal string
	if err := engine.store.db.QueryRow(`SELECT s.state,g.state FROM systems s JOIN goals g USING(system_id) WHERE g.goal_id=?`, root.GoalID).Scan(&system, &goal); err != nil || (system != "running" && system != "paused") || goal != "waiting" {
		t.Fatalf("idle lifecycle: system=%s goal=%s err=%v", system, goal, err)
	}
}

func continuousRestart(t *testing.T, engine *fixtureWorkflow) {
	t.Helper()
	if err := engine.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(context.Background(), filepath.Join(engine.cfg.DataDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	engine.store, engine.broker.store = store, store
	if err := store.RecoverExecutions(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestContinuousSequentialHumanTasksKeepGoalTeamAndUsage(t *testing.T) {
	engine, root := continuousFixture(t, 0)
	propose := claimAction(t, engine, "runtime.agent.propose", ProposeAgentArgs{Name: "helper", Prompt: "Help.", Tools: []string{}, TokenBudget: 10000})
	var child struct {
		ID string `json:"agent_id"`
	}
	if err := json.Unmarshal([]byte(invokeAction(t, engine, propose)), &child); err != nil {
		t.Fatal(err)
	}
	finishAction(t, engine, propose)
	delegate := claimAction(t, engine, "runtime.task.delegate", DelegateArgs{AgentID: child.ID, Prompt: "One bounded assignment."})
	invokeAction(t, engine, delegate)
	finishAction(t, engine, delegate)
	childCall := continuousClaim(t, engine, time.Now())
	if childCall.AgentID != child.ID || childCall.Deadline != delegate.Deadline {
		t.Fatalf("delegation lost agent/shared deadline: %+v", childCall)
	}
	continuousFinish(t, engine, childCall, time.Now())
	continuousFinish(t, engine, continuousClaim(t, engine, time.Now()), time.Now())
	const followups = 70
	start := time.Now()
	for i := 0; i < followups; i++ {
		now := start.Add(time.Duration(i+1) * time.Minute)
		continuousIdle(t, engine, root, now)
		calls := continuousInt(t, engine, `SELECT count(*) FROM model_calls`)
		if _, err := engine.store.AddInput(context.Background(), fmt.Sprintf("human-%d", i), root.SystemID, AddInputCommand{Content: fmt.Sprintf("Human follow-up %d", i)}); err != nil {
			t.Fatal(err)
		}
		if continuousInt(t, engine, `SELECT count(*) FROM model_calls`) != calls ||
			continuousInt(t, engine, `SELECT reserved_tokens FROM systems WHERE system_id=?`, root.SystemID) != 0 {
			t.Fatal("human input reserved a call before claim")
		}
		e := continuousClaim(t, engine, now)
		if e.GoalID != root.GoalID || e.AgentID != root.AgentID || e.TaskID == root.TaskID || !strings.Contains(string(e.Request), *fixtureGoal()) {
			t.Fatalf("human input replaced team/goal or lost original goal: %+v", e)
		}
		continuousFinish(t, engine, e, now)
	}
	continuousIdle(t, engine, root, time.Now().Add(24*time.Hour))
	assertCount(t, engine.store, "goals", 1)
	assertCount(t, engine.store, "tasks", followups+2)
	assertCount(t, engine.store, "model_attempts", followups+4)
	if continuousInt(t, engine, `SELECT count(*) FROM agents WHERE state='active'`) != 2 ||
		continuousInt(t, engine, `SELECT turns FROM agent_usage WHERE agent_id=?`, root.AgentID) != followups+3 ||
		continuousInt(t, engine, `SELECT used_tokens FROM systems WHERE system_id=?`, root.SystemID) != (followups+4)*18 {
		t.Fatal("team or cumulative accounting was reset after completion")
	}
}

func TestContinuousInitialDeadlineMatchesPreparedCall(t *testing.T) {
	engine, root := continuousFixture(t, 17)
	task, err := engine.store.Task(context.Background(), root.SystemID, root.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	deadline := continuousInt(t, engine, `SELECT deadline FROM tasks WHERE task_id=?`, root.TaskID)
	now := time.Now()
	if task.CallID == "" {
		if deadline != 0 {
			t.Fatalf("unprepared task has deadline %d", deadline)
		}
		now = now.Add(time.Hour)
		deadline = now.Add(17 * time.Second).UnixMilli()
	} else if deadline <= now.UnixMilli() {
		t.Fatalf("prepared initial task lacks a live deadline: %d", deadline)
	}
	e := continuousClaim(t, engine, now)
	if e.Deadline != deadline ||
		continuousInt(t, engine, `SELECT deadline FROM tasks WHERE task_id=?`, root.TaskID) != e.Deadline {
		t.Fatalf("claim changed the prepared task lifetime: deadline=%d want=%d", e.Deadline, deadline)
	}
	continuousFinish(t, engine, e, now)
}

func TestContinuousInputRacingFinalCompletionTransfersOnce(t *testing.T) {
	engine, root := continuousFixture(t, 0)
	e := continuousClaim(t, engine, time.Now())
	command := AddInputCommand{Content: "arrived during the final activation"}
	receipt, err := engine.store.AddInput(context.Background(), "racing-input", root.SystemID, command)
	if err != nil {
		t.Fatal(err)
	}
	continuousFinish(t, engine, e, time.Now())
	next := continuousClaim(t, engine, time.Now())
	if next.TaskID == e.TaskID || next.GoalID != e.GoalID || strings.Count(string(next.Request), command.Content) != 1 {
		t.Fatalf("pending input was dropped, duplicated, or reused terminal task: %+v", next)
	}
	continuousFinish(t, engine, next, time.Now())
	replay, err := engine.store.AddInput(context.Background(), "racing-input", root.SystemID, command)
	if err != nil || string(replay.CommandResult) != string(receipt.CommandResult) {
		t.Fatalf("input receipt changed: %s %v", replay.CommandResult, err)
	}
	if continuousInt(t, engine, `SELECT count(*) FROM events WHERE type='user.input'`) != 1 ||
		continuousInt(t, engine, `SELECT count(*) FROM mailboxes m JOIN events e USING(event_id) WHERE e.type='user.input' AND m.applied=1 AND m.state='acked'`) != 1 {
		t.Fatal("transferred input was not acknowledged exactly once")
	}
	continuousIdle(t, engine, root, time.Now())
}

func TestContinuousPauseRestartReplayThenResume(t *testing.T) {
	engine, root := continuousFixture(t, 17)
	ctx := context.Background()
	continuousFinish(t, engine, continuousClaim(t, engine, time.Now()), time.Now())
	if _, err := engine.store.Control(ctx, engine.cfg, "pause", root.SystemID, "system", "pause", ""); err != nil {
		t.Fatal(err)
	}
	command := AddInputCommand{Content: "durable paused input"}
	receipt, err := engine.store.AddInput(ctx, "paused-input", root.SystemID, command)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.store.AddAttachment(ctx, "paused-attachment", root.SystemID, AttachmentCommand{Name: "context.txt", Content: "durable attachment"}); err != nil {
		t.Fatal(err)
	}
	continuousRestart(t, engine)
	replay, err := engine.store.AddInput(ctx, "paused-input", root.SystemID, command)
	if err != nil || string(replay.CommandResult) != string(receipt.CommandResult) {
		t.Fatalf("restart lost receipt: %s %v", replay.CommandResult, err)
	}
	now := time.Now().Add(time.Hour)
	if _, _, found, err := engine.claim(ctx, now); err != nil || found {
		t.Fatalf("paused work dispatched: %v %v", found, err)
	}
	assertCount(t, engine.store, "model_calls", 1)
	if continuousInt(t, engine, `SELECT count(*) FROM tasks WHERE state='queued' AND deadline=0`) != 1 {
		t.Fatal("paused input started its task deadline")
	}
	if _, err := engine.store.Control(ctx, engine.cfg, "resume", root.SystemID, "system", "resume", ""); err != nil {
		t.Fatal(err)
	}
	e := continuousClaim(t, engine, now)
	if e.GoalID != root.GoalID || e.Deadline != now.Add(17*time.Second).UnixMilli() ||
		strings.Count(string(e.Request), command.Content) != 1 || !strings.Contains(string(e.Request), "durable attachment") {
		t.Fatalf("resumed input/history/deadline incorrect: %+v", e)
	}
	continuousFinish(t, engine, e, now)
	continuousRestart(t, engine)
	continuousIdle(t, engine, root, now.Add(time.Hour))
	if continuousInt(t, engine, `SELECT used_tokens FROM systems WHERE system_id=?`, root.SystemID) != 36 {
		t.Fatal("restart reset usage")
	}
}

func TestContinuousIdleStopRequiresFreshGoalWithoutBudgetReset(t *testing.T) {
	engine, root := continuousFixture(t, 0)
	ctx := context.Background()
	knowledgeTransaction(t, engine, root, func(tx *sql.Tx, task TaskRecord) {
		if _, err := createSchedule(ctx, tx, task, ScheduleCommand{Cron: "* * * * *", Timezone: "UTC", Content: "old timer", Budget: 2}, task.AgentID, time.Now()); err != nil {
			t.Fatal(err)
		}
		if _, err := createSubscription(ctx, tx, task, SubscriptionCommand{Type: "memory.changed", Scope: "agent", Budget: 2}, task.AgentID, time.Now()); err != nil {
			t.Fatal(err)
		}
	})
	continuousFinish(t, engine, continuousClaim(t, engine, time.Now()), time.Now())
	if continuousInt(t, engine, `SELECT count(*) FROM subscriptions WHERE state='active'`) != 1 {
		t.Fatal("successful completion canceled its subscription")
	}
	stopped, err := engine.store.StopSystem(ctx, localAdministrator, "stop-idle", root.SystemID, time.Now())
	if err != nil || stopped.State != "stopped" {
		t.Fatalf("idle stop: %+v %v", stopped, err)
	}
	if continuousInt(t, engine, `SELECT count(*) FROM goals WHERE goal_id=? AND state='canceled'`, root.GoalID) != 1 ||
		continuousInt(t, engine, `SELECT count(*) FROM agents WHERE state!='stopped'`) != 0 ||
		continuousInt(t, engine, `SELECT count(*) FROM schedules WHERE state='active'`) != 0 ||
		continuousInt(t, engine, `SELECT count(*) FROM subscriptions WHERE state='active'`) != 0 {
		t.Fatal("idle stop left a goal, team, or wakeup active")
	}
	if _, err := engine.store.AddInput(ctx, "stopped-input", root.SystemID, AddInputCommand{Content: "must not run"}); err == nil {
		t.Fatal("stopped input accepted")
	}
	command := continuousCommand(t, 0)
	if _, err := engine.store.StartSystem(ctx, localAdministrator, "missing-new-goal", root.SystemID, engine.owner, command, engine.cfg, time.Now()); err == nil {
		t.Fatal("restart silently replayed completed goal")
	}
	goal := "explicit replacement goal"
	command.Goal = &goal
	if _, err := engine.store.StartSystem(ctx, localAdministrator, "fresh-start", root.SystemID, engine.owner, command, engine.cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	if continuousInt(t, engine, `SELECT used_tokens FROM systems WHERE system_id=?`, root.SystemID) != 18 {
		t.Fatal("new start reset system allowance")
	}
	e := continuousClaim(t, engine, time.Now())
	if e.GoalID == root.GoalID || strings.Contains(string(e.Request), "old timer") || !strings.Contains(string(e.Request), goal) {
		t.Fatalf("new start replayed old work: %+v", e)
	}
	continuousFinish(t, engine, e, time.Now())
	continuousIdle(t, engine, e, time.Now().Add(time.Hour))
}

func TestContinuousTimerOutlivesTaskDeadlineButHasFiniteOccurrences(t *testing.T) {
	engine, root := continuousFixture(t, 1)
	ctx := context.Background()
	now := time.Now()
	knowledgeTransaction(t, engine, root, func(tx *sql.Tx, task TaskRecord) {
		if _, err := createSchedule(ctx, tx, task, ScheduleCommand{Cron: "* * * * *", Timezone: "UTC", Content: "finite timer", Budget: 2}, task.AgentID, now); err != nil {
			t.Fatal(err)
		}
	})
	wait := claimAction(t, engine, "runtime.task.wait", map[string]string{"reason": "idle until timer"})
	invokeAction(t, engine, wait)
	finishAction(t, engine, wait)
	if continuousInt(t, engine, `SELECT count(*) FROM tasks WHERE task_id=? AND state='completed'`, root.TaskID) != 1 ||
		continuousInt(t, engine, `SELECT expires_at FROM schedules`) != 0 {
		t.Fatal("continuous wait or schedule retained goal lifetime")
	}
	for i := 1; i <= 2; i++ {
		future := now.Add(time.Duration(i*5) * time.Minute)
		e := continuousClaim(t, engine, future)
		if e.TaskID == root.TaskID || e.GoalID != root.GoalID || e.Deadline != future.Add(time.Second).UnixMilli() ||
			!strings.Contains(string(e.Request), "finite timer") {
			t.Fatalf("timer did not wake fresh bounded task: task=%s goal=%s deadline=%d want=%d", e.TaskID, e.GoalID, e.Deadline, future.Add(time.Second).UnixMilli())
		}
		continuousFinish(t, engine, e, future)
		assertCount(t, engine.store, "schedule_occurrences", i)
	}
	if continuousInt(t, engine, `SELECT remaining FROM schedules`) != 0 {
		t.Fatal("timer trigger budget was replenished")
	}
	continuousIdle(t, engine, root, now.Add(time.Hour))
	assertCount(t, engine.store, "model_attempts", 3)
}

func TestContinuousIdleTriggerRegistrationAndCancellationCreateNoWork(t *testing.T) {
	engine, root := continuousFixture(t, 0)
	ctx := context.Background()
	continuousFinish(t, engine, continuousClaim(t, engine, time.Now()), time.Now())
	for _, schedule := range []bool{true, false} {
		var record SystemRecord
		var err error
		column := "subscription_id"
		if schedule {
			column = "schedule_id"
			record, err = engine.store.CreateSchedule(ctx, engine.cfg, "idle-schedule", root.SystemID,
				ScheduleCommand{At: time.Now().Add(time.Hour).Format(time.RFC3339Nano), Content: "future context", Budget: 1})
		} else {
			record, err = engine.store.CreateSubscription(ctx, engine.cfg, "idle-subscription", root.SystemID,
				SubscriptionCommand{Type: "memory.changed", Scope: "agent", Budget: 1})
		}
		if err != nil || !record.Idle {
			t.Fatalf("idle %s registration created work: idle=%v err=%v", column, record.Idle, err)
		}
		var receipt map[string]string
		if err := json.Unmarshal(record.CommandResult, &receipt); err != nil || receipt[column] == "" {
			t.Fatalf("missing trigger receipt: %s %v", record.CommandResult, err)
		}
		assertCount(t, engine.store, "tasks", 1)
		assertCount(t, engine.store, "model_calls", 1)
		record, err = engine.store.CancelTrigger(ctx, "cancel-"+column, root.SystemID, receipt[column], schedule)
		if err != nil || !record.Idle {
			t.Fatalf("idle %s cancellation created work: idle=%v err=%v", column, record.Idle, err)
		}
	}
	continuousIdle(t, engine, root, time.Now().Add(2*time.Hour))
	assertCount(t, engine.store, "tasks", 1)
	assertCount(t, engine.store, "model_calls", 1)
}

func TestContinuousRejectedTimerRollsBackWakeWithoutBlockingOtherSystems(t *testing.T) {
	engine, root := continuousFixture(t, 0)
	ctx := context.Background()
	continuousFinish(t, engine, continuousClaim(t, engine, time.Now()), time.Now())
	now := time.Now()
	if _, err := engine.store.CreateSchedule(ctx, engine.cfg, "saturated-timer", root.SystemID,
		ScheduleCommand{At: now.Add(time.Second).Format(time.RFC3339Nano), Content: "limited wake", Budget: 1}); err != nil {
		t.Fatal(err)
	}
	// Fill the rolling system window without introducing pending deliveries.
	if _, err := engine.store.db.Exec(`WITH RECURSIVE numbers(n) AS (
	 SELECT 1 UNION ALL SELECT n+1 FROM numbers WHERE n<?)
	 INSERT INTO events(event_id,system_id,type,source,recipient,goal_id,task_id,correlation_id,causation_id,created_at,expires_at,authorization_ref,depth,payload)
	 SELECT 'saturated-'||n,e.system_id,e.type,e.source,e.recipient,e.goal_id,e.task_id,e.correlation_id,'',?,0,e.authorization_ref,0,e.payload
	 FROM numbers CROSS JOIN (SELECT * FROM events WHERE system_id=? ORDER BY sequence LIMIT 1) e`,
		MaxSystemEvents-1, now.UnixMilli(), root.SystemID); err != nil {
		t.Fatal(err)
	}
	id, err := engine.store.RememberConfiguration(ctx, engine.cfg)
	if err != nil {
		t.Fatal(err)
	}
	healthy := fixtureCall(t, engine.broker, id, "unaffected-system", false)
	future := now.Add(2 * time.Second)
	_, e, found, err := engine.claim(ctx, future)
	if err != nil || !found || e.SystemID != healthy.SystemID {
		t.Fatalf("rejected wake poisoned another system: found=%v system=%s err=%v", found, e.SystemID, err)
	}
	continuousFinish(t, engine, e, future)
	if continuousInt(t, engine, `SELECT count(*) FROM tasks WHERE system_id=?`, root.SystemID) != 1 ||
		continuousInt(t, engine, `SELECT count(*) FROM schedules WHERE state='failed' AND reason LIKE '%event budget%'`) != 1 {
		t.Fatal("rejected event left an orphan task or failed to settle its schedule")
	}
	assertCount(t, engine.store, "schedule_occurrences", 0)
	continuousIdle(t, engine, root, future)
	record, err := engine.store.GetSystem(ctx, localAdministrator, root.SystemID)
	if err != nil || !record.Idle {
		t.Fatalf("rejected timer made system busy: idle=%v err=%v", record.Idle, err)
	}
	future = now.Add(25 * time.Hour)
	if _, err := engine.store.CreateSchedule(ctx, engine.cfg, "new-window", root.SystemID,
		ScheduleCommand{At: future.Format(time.RFC3339Nano), Content: "after window", Budget: 1}); err != nil {
		t.Fatal(err)
	}
	continuousFinish(t, engine, continuousClaim(t, engine, future), future)
	if continuousInt(t, engine, `SELECT count(*) FROM events WHERE system_id=?`, root.SystemID) != MaxSystemEvents+1 {
		t.Fatal("historical events incorrectly remain a lifetime cap")
	}
}

func TestContinuousCompletedTaskWakeupsRecheckRevokedAndNarrowedGrants(t *testing.T) {
	for _, mode := range []string{"revoked", "narrowed"} {
		t.Run(mode, func(t *testing.T) {
			engine, root := continuousFixture(t, 0)
			ctx := context.Background()
			propose := claimAction(t, engine, "runtime.agent.propose", ProposeAgentArgs{Name: "watcher", Prompt: "Watch for context.",
				Tools: []string{"runtime.schedule.create", "runtime.events.subscribe", "runtime.memory.search"}, TokenBudget: 10000})
			var child struct {
				ID string `json:"agent_id"`
			}
			if err := json.Unmarshal([]byte(invokeAction(t, engine, propose)), &child); err != nil {
				t.Fatal(err)
			}
			finishAction(t, engine, propose)
			delegate := claimAction(t, engine, "runtime.task.delegate", DelegateArgs{AgentID: child.ID, Prompt: "Prepare to watch."})
			invokeAction(t, engine, delegate)
			finishAction(t, engine, delegate)
			e := continuousClaim(t, engine, time.Now())
			continuousFinish(t, engine, e, time.Now())
			continuousFinish(t, engine, continuousClaim(t, engine, time.Now()), time.Now())
			now := time.Now()
			if _, err := engine.store.CreateSchedule(ctx, engine.cfg, "watch", root.SystemID,
				ScheduleCommand{AgentID: child.ID, At: now.Add(time.Second).Format(time.RFC3339Nano), Content: "watch context", Budget: 1}); err != nil {
				t.Fatal(err)
			}
			if _, err := engine.store.CreateSubscription(ctx, engine.cfg, "subscribe", root.SystemID,
				SubscriptionCommand{AgentID: child.ID, Type: "memory.changed", Scope: "agent", Budget: 1}); err != nil {
				t.Fatal(err)
			}
			var err error
			if mode == "revoked" {
				_, err = engine.store.RevokeTool(ctx, engine.cfg, "revoke", root.SystemID, RevokeToolCommand{Name: "runtime.schedule.create", Version: 1})
			} else {
				_, err = engine.store.AssignTools(ctx, engine.cfg, "narrow", root.SystemID, AssignToolsCommand{ExpectedRevision: 1, Tools: []ToolPin{}}, child.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			future := now.Add(2 * time.Second)
			knowledgeTransaction(t, engine, e, func(tx *sql.Tx, task TaskRecord) {
				task.ID, task.AgentID = "", localAdministrator
				if err := engine.notifySubscriptions(ctx, tx, task, "memory.changed", "agent", child.ID, "", "changed-memory", future); err != nil {
					t.Fatal(err)
				}
			})
			continuousIdle(t, engine, root, future)
			if continuousInt(t, engine, `SELECT count(*) FROM schedules WHERE state='canceled'`) != 1 ||
				continuousInt(t, engine, `SELECT count(*) FROM subscriptions WHERE state='canceled'`) != 1 {
				t.Fatal("obsolete grants retained a live wakeup")
			}
			assertCount(t, engine.store, "tasks", 2)
			assertCount(t, engine.store, "model_calls", 4)
			assertCount(t, engine.store, "schedule_occurrences", 0)
		})
	}
}

func TestContinuousStillRejectsTaskTurnsDeadlineAndBudget(t *testing.T) {
	t.Run("task turns", func(t *testing.T) {
		engine, root := continuousFixture(t, 0)
		for i := 0; i < MaxTaskTurns; i++ {
			e := claimAction(t, engine, "runtime.agent.list", struct{}{})
			invokeAction(t, engine, e)
			finishAction(t, engine, e)
		}
		task, err := engine.store.Task(context.Background(), root.SystemID, root.TaskID)
		if err != nil || task.State != "rejected" || task.Turns != MaxTaskTurns || !strings.Contains(task.Reason, "turn") {
			t.Fatalf("continuous task bypassed turn cap: %+v %v", task, err)
		}
		assertCount(t, engine.store, "model_attempts", MaxTaskTurns)
		if _, _, found, err := engine.claim(context.Background(), time.Now()); err != nil || found {
			t.Fatalf("rejected task automatically retried: %v %v", found, err)
		}
	})
	for _, limit := range []string{"deadline", "budget"} {
		t.Run(limit, func(t *testing.T) {
			engine, root := continuousFixture(t, 1)
			now := time.Now()
			e := continuousClaim(t, engine, now)
			if limit == "deadline" {
				now = time.UnixMilli(e.Deadline)
			} else if _, err := engine.store.db.Exec(`UPDATE systems SET used_tokens=? WHERE system_id=?`, engine.cfg.Bootstrap.Limits.TokenBudget, root.SystemID); err != nil {
				t.Fatal(err)
			}
			if err := engine.broker.queue(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			current, admitted, err := engine.broker.admit(context.Background(), e, now)
			if err != nil || admitted || current.State != "rejected" || !strings.Contains(current.Reason, limit) {
				t.Fatalf("%s bypassed: %+v admitted=%v err=%v", limit, current, admitted, err)
			}
			finishAction(t, engine, e)
			assertCount(t, engine.store, "model_attempts", 0)
		})
	}
	for _, known := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed known=%v", known), func(t *testing.T) {
			engine, root := continuousFixture(t, 0)
			knowledgeTransaction(t, engine, root, func(tx *sql.Tx, task TaskRecord) {
				if _, err := createSchedule(context.Background(), tx, task, ScheduleCommand{Cron: "* * * * *", Timezone: "UTC", Content: "must not retry failure", Budget: 1}, task.AgentID, time.Now()); err != nil {
					t.Fatal(err)
				}
			})
			e := continuousClaim(t, engine, time.Now())
			ctx := context.Background()
			if err := engine.broker.queue(ctx, e); err != nil {
				t.Fatal(err)
			}
			admitted := admitCall(t, engine.broker, e, true)
			if err := engine.broker.settle(ctx, admitted, ProviderResult{Known: known, Reason: "fixture provider failure"}, time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := engine.finishDelivery(ctx, e, errors.New("fixture provider failure"), false); err != nil {
				t.Fatal(err)
			}
			continuousRestart(t, engine)
			if _, _, found, err := engine.claim(ctx, time.Now().Add(time.Hour)); err != nil || found {
				t.Fatalf("failed/unknown task automatically retried: %v %v", found, err)
			}
			want := int64(0)
			if !known {
				want = admitted.Reservation
			}
			if continuousInt(t, engine, `SELECT reserved_tokens FROM systems WHERE system_id=?`, root.SystemID) != want {
				t.Fatal("failed/unknown reservation accounting changed on recovery")
			}
			assertCount(t, engine.store, "model_attempts", 1)
		})
	}
}

func TestContinuousSubscriptionsWakeIdleTasksWithoutSelfFeedback(t *testing.T) {
	ctx := context.Background()
	t.Run("memory changes", func(t *testing.T) {
		engine, root := continuousFixture(t, 0)
		continuousFinish(t, engine, continuousClaim(t, engine, time.Now()), time.Now())
		if _, err := engine.store.CreateSubscription(ctx, engine.cfg, "subscribe", root.SystemID,
			SubscriptionCommand{Type: "memory.changed", Scope: "system", Budget: 2}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if _, err := engine.store.PutMemory(ctx, engine.cfg, engine.owner, fmt.Sprintf("memory-%d", i), root.SystemID,
				MemoryCommand{Scope: "system", Content: "New observation", Evidence: "Fixture", Confidence: 90, RetentionSeconds: 3600}); err != nil {
				t.Fatal(err)
			}
			e := continuousClaim(t, engine, time.Now())
			if e.TaskID == root.TaskID || e.GoalID != root.GoalID || !strings.Contains(string(e.Request), "memory.changed") {
				t.Fatalf("subscription did not wake fresh work: %+v", e)
			}
			continuousFinish(t, engine, e, time.Now())
			continuousIdle(t, engine, root, time.Now())
		}
		if continuousInt(t, engine, `SELECT remaining FROM subscriptions`) != 0 {
			t.Fatal("continuous memory wake reset trigger allowance")
		}
		assertCount(t, engine.store, "model_calls", 3)
	})
	t.Run("own completion", func(t *testing.T) {
		engine, root := continuousFixture(t, 0)
		continuousFinish(t, engine, continuousClaim(t, engine, time.Now()), time.Now())
		if _, err := engine.store.CreateSubscription(ctx, engine.cfg, "subscribe", root.SystemID,
			SubscriptionCommand{Type: "task.completed", Budget: 2}); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.store.AddInput(ctx, "input", root.SystemID, AddInputCommand{Content: "One more piece of work"}); err != nil {
			t.Fatal(err)
		}
		continuousFinish(t, engine, continuousClaim(t, engine, time.Now()), time.Now())
		continuousIdle(t, engine, root, time.Now())
		if continuousInt(t, engine, `SELECT remaining FROM subscriptions`) != 2 {
			t.Fatal("agent woke itself through its prior task's subscription")
		}
	})
	t.Run("another agent waits", func(t *testing.T) {
		engine, root := continuousFixture(t, 0)
		propose := claimAction(t, engine, "runtime.agent.propose", ProposeAgentArgs{
			Name: "observer", Prompt: "Watch progress.", Tools: []string{}, TokenBudget: 10000})
		var child struct {
			ID string `json:"agent_id"`
		}
		if err := json.Unmarshal([]byte(invokeAction(t, engine, propose)), &child); err != nil {
			t.Fatal(err)
		}
		finishAction(t, engine, propose)
		delegate := claimAction(t, engine, "runtime.task.delegate", DelegateArgs{AgentID: child.ID, Prompt: "Observe."})
		invokeAction(t, engine, delegate)
		finishAction(t, engine, delegate)
		continuousFinish(t, engine, continuousClaim(t, engine, time.Now()), time.Now())
		continuousFinish(t, engine, continuousClaim(t, engine, time.Now()), time.Now())
		if _, err := engine.store.CreateSubscription(ctx, engine.cfg, "subscribe", root.SystemID,
			SubscriptionCommand{AgentID: child.ID, Type: "task.completed", Budget: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.store.AddInput(ctx, "input", root.SystemID, AddInputCommand{Content: "Wait for input"}); err != nil {
			t.Fatal(err)
		}
		wait := claimAction(t, engine, "runtime.task.wait", map[string]string{"reason": "Need more context"})
		invokeAction(t, engine, wait)
		finishAction(t, engine, wait)
		e := continuousClaim(t, engine, time.Now())
		if e.AgentID != child.ID || !strings.Contains(string(e.Request), "task.completed") {
			t.Fatalf("explicit wait completion did not notify the observer: %+v", e)
		}
		continuousFinish(t, engine, e, time.Now())
		continuousIdle(t, engine, root, time.Now())
		assertCount(t, engine.store, "model_calls", 6)
	})
}

func TestContinuousMigrationSixDefaultsPreserveBoundedTasks(t *testing.T) {
	cfg := fixtureConfiguration(t)
	engine, id := fixtureTeamEngine(t, cfg)
	e := fixtureCall(t, engine.broker, id, "legacy-bounded", false)
	// Reconstruct a populated v5 database using the preceding schema's columns.
	if _, err := engine.store.db.Exec(`ALTER TABLE tasks DROP COLUMN deadline;
	 ALTER TABLE goals DROP COLUMN continuous;
	 ALTER TABLE goals DROP COLUMN task_lifetime_seconds;
	 PRAGMA user_version=5;`); err != nil {
		t.Fatal(err)
	}
	continuousRestart(t, engine)
	if continuousInt(t, engine, `PRAGMA user_version`) != 6 ||
		continuousInt(t, engine, `SELECT continuous FROM goals WHERE goal_id=?`, e.GoalID) != 0 ||
		continuousInt(t, engine, `SELECT task_lifetime_seconds FROM goals WHERE goal_id=?`, e.GoalID) != 0 ||
		continuousInt(t, engine, `SELECT deadline FROM tasks WHERE task_id=?`, e.TaskID) != e.Deadline {
		t.Fatal("v6 migration changed bounded defaults or failed deadline backfill")
	}
	_, claimed, found, err := engine.claim(context.Background(), time.Now())
	if err != nil || !found {
		t.Fatalf("legacy task lost: %v %v", found, err)
	}
	continuousFinish(t, engine, claimed, time.Now())
	s, err := engine.store.GetSystem(context.Background(), localAdministrator, e.SystemID)
	if err != nil || s.State != "inactive" {
		t.Fatalf("default bounded task now persists: %+v %v", s, err)
	}
}
