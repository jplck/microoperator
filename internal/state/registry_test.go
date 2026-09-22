package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func teamConfiguration(t *testing.T) Configuration {
	cfg := fixtureConfiguration(t)
	def := *cfg.Bootstrap
	def.Tools = []string{"runtime.agent.propose", "runtime.agent.list", "runtime.task.delegate", "runtime.task.progress", "runtime.tool.propose", "runtime.text.analyze"}
	def.Operator.Tools = append([]string{}, def.Tools...)
	cfg.Bootstrap = &def
	q := cfg.QuotaGroups["account"]
	q.BurstRequests, q.RequestsPerMinute, q.TokensPerMinute = 60, 600, 1000000
	cfg.QuotaGroups["account"] = q
	return cfg
}

func claimAction(t *testing.T, engine *fixtureWorkflow, name string, args any) ExecutionRecord {
	t.Helper()
	_, e, found, err := engine.claim(context.Background(), time.Now())
	if err != nil || !found {
		t.Fatalf("claim: %v %v", found, err)
	}
	if err := engine.broker.queue(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	admitted := admitCall(t, engine.broker, e, true)
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	action := ModelToolCall{ID: "action", Type: "function"}
	action.Function.Name, action.Function.Arguments = WireToolName(name), string(data)
	if err := engine.broker.settle(context.Background(), admitted, ProviderResult{Known: true, Input: 11, Output: 7, Actions: []ModelToolCall{action}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	record, err := engine.store.GetSystem(context.Background(), localAdministrator, e.SystemID)
	if err != nil {
		t.Fatal(err)
	}
	current := *record.Execution
	current.EventID = e.EventID
	return current
}

func invokeAction(t *testing.T, engine *fixtureWorkflow, e ExecutionRecord) string {
	t.Helper()
	result, err := engine.invokeTool(context.Background(), e, e.Actions[0])
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func finishAction(t *testing.T, engine *fixtureWorkflow, e ExecutionRecord) {
	t.Helper()
	if err := engine.finishDelivery(context.Background(), e, nil, false); err != nil {
		t.Fatal(err)
	}
}

func TestFailedDeliveryPreservesProviderReason(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(fmt.Sprint("known-", known), func(t *testing.T) {
			cfg := fixtureConfiguration(t)
			engine, id := fixtureTeamEngine(t, cfg)
			fixtureCall(t, engine.broker, id, "failure", false)
			ctx := context.Background()
			_, e, found, err := engine.claim(ctx, time.Now())
			if err != nil || !found {
				t.Fatalf("claim: %v %v", found, err)
			}
			if err := engine.broker.queue(ctx, e); err != nil {
				t.Fatal(err)
			}
			admitted := admitCall(t, engine.broker, e, true)
			reason := "provider request deadline exceeded after admission; outcome unknown and reservation retained"
			if known {
				reason = "provider rejected request (HTTP 401)"
			}
			if err := engine.broker.settle(ctx, admitted, ProviderResult{Known: known, Reason: reason}, time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := engine.finishDelivery(ctx, e, errors.New("fixture-private-worker-diagnostic"), false); err != nil {
				t.Fatal(err)
			}
			view, err := engine.store.Activity(ctx, localAdministrator, e.SystemID)
			if err != nil {
				t.Fatal(err)
			}
			var event *ActivityEvent
			for i := range view.Events {
				if view.Events[i].ID == e.EventID {
					event = &view.Events[i]
				}
			}
			if event == nil || event.State != "dead" || event.Reason != reason ||
				len(view.Agents) != 1 || view.Agents[0].Reason != reason {
				t.Fatalf("provider failure hidden or raw worker error leaked: %+v", view)
			}
			if known && view.System.ReservedTokens != 0 || !known && view.System.ReservedTokens != e.Reservation {
				t.Fatalf("delivery changed provider accounting: %+v", view.System)
			}
		})
	}
}

func TestMailboxDeduplicationRetryAndDeadLetter(t *testing.T) {
	cfg := fixtureConfiguration(t)
	engine, id := fixtureTeamEngine(t, cfg)
	fixtureCall(t, engine.broker, id, "one", false)
	_, e, found, err := engine.claim(context.Background(), time.Now())
	if err != nil || !found {
		t.Fatalf("claim: %v %v", found, err)
	}
	if _, err := engine.store.db.Exec(`INSERT INTO mailboxes(system_id,event_id,recipient,task_id,state) VALUES(?,?,?,?,'pending')`, e.SystemID, e.EventID, e.AgentID, e.TaskID); err == nil {
		t.Fatal("duplicate event/recipient accepted")
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if err := engine.finishDelivery(context.Background(), e, errors.New("fixture worker setup failure"), false); err != nil {
			t.Fatal(err)
		}
		if attempt < 3 {
			var due int64
			if err := engine.store.db.QueryRow(`SELECT not_before FROM mailboxes WHERE event_id=?`, e.EventID).Scan(&due); err != nil {
				t.Fatal(err)
			}
			_, e, found, err = engine.claim(context.Background(), time.UnixMilli(due))
			if err != nil || !found {
				t.Fatalf("retry claim: %v %v", found, err)
			}
		}
	}
	var state string
	var attempts int
	if err := engine.store.db.QueryRow(`SELECT state,attempts FROM mailboxes WHERE event_id=?`, e.EventID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "dead" || attempts != 3 {
		t.Fatalf("unbounded retries: %s %d", state, attempts)
	}
	assertCount(t, engine.store, "model_attempts", 0)
	tx, err := engine.store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := auditExecution(context.Background(), tx, e.SystemID, "fixture", "fixture", 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	var count int
	if err := engine.store.db.QueryRow(`SELECT count(*) FROM audit WHERE action='fixture'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("transactional event/audit rollback failed")
	}
}

func TestTerminalDeliveryLimitFailsOnlyItsContinuation(t *testing.T) {
	cfg := teamConfiguration(t)
	engine, id := fixtureTeamEngine(t, cfg)
	root := fixtureCall(t, engine.broker, id, "bounded-reply", false)
	propose := claimAction(t, engine, "runtime.agent.propose", ProposeAgentArgs{Name: "child", Prompt: "work", Tools: []string{}, TokenBudget: 10000})
	var child struct {
		ID string `json:"agent_id"`
	}
	if err := json.Unmarshal([]byte(invokeAction(t, engine, propose)), &child); err != nil {
		t.Fatal(err)
	}
	finishAction(t, engine, propose)
	delegate := claimAction(t, engine, "runtime.task.delegate", DelegateArgs{AgentID: child.ID, Prompt: "bounded"})
	var result struct {
		ID string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(invokeAction(t, engine, delegate)), &result); err != nil {
		t.Fatal(err)
	}
	finishAction(t, engine, delegate)
	tx, err := engine.store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	parent, err := readTask(context.Background(), tx, root.SystemID, root.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	task, err := readTask(context.Background(), tx, root.SystemID, result.ID)
	if err != nil {
		t.Fatal(err)
	}
	cause := ""
	for i := 0; i <= MaxEventDepth; i++ {
		cause, err = emitEvent(context.Background(), tx, parent, "task.progress", "runtime", cause, struct{}{}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := terminateTask(context.Background(), tx, task, "failed", "", "fixture failure", "runtime", cause, time.Now()); err != nil {
		t.Fatalf("delivery limit poisoned transaction: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	parent, err = engine.taskSnapshot(context.Background(), root.SystemID, root.TaskID)
	if err != nil || parent.State != "rejected" || !strings.Contains(parent.Reason, "child result delivery rejected") {
		t.Fatalf("lost continuation: %+v %v", parent, err)
	}
	if _, _, found, err := engine.claim(context.Background(), time.Now()); err != nil || found {
		t.Fatalf("terminal work reopened: %v %v", found, err)
	}
}

func TestMailboxRejectsEventAmplification(t *testing.T) {
	cfg := fixtureConfiguration(t)
	engine, id := fixtureTeamEngine(t, cfg)
	e := fixtureCall(t, engine.broker, id, "events", false)
	tx, err := engine.store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	task, err := readTask(context.Background(), tx, e.SystemID, e.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	cause := ""
	for i := 0; i <= MaxEventDepth; i++ {
		cause, err = emitEvent(context.Background(), tx, task, "task.progress", "runtime", cause, map[string]string{"message": "fixture"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := emitEvent(context.Background(), tx, task, "task.progress", "runtime", cause, struct{}{}, time.Now()); err == nil {
		t.Fatal("causation depth was unbounded")
	}
	if _, err := emitEvent(context.Background(), tx, task, "user.input", localAdministrator, "", strings.Repeat("x", MaxEventBytes+1), time.Now()); err == nil {
		t.Fatal("event payload was unbounded")
	}
	_, err = tx.Exec(`UPDATE events SET source='spoofed'`)
	if err == nil {
		t.Fatal("trusted envelope could be overwritten")
	}
}
