package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestActivitySnapshotShowsScopedCurrentWorkAndRecentEvents(t *testing.T) {
	cfg := teamConfiguration(t)
	engine, configID := fixtureTeamEngine(t, cfg)
	ctx := context.Background()
	root := fixtureCall(t, engine.broker, configID, "activity", false)
	propose := claimAction(t, engine, "runtime.agent.propose", ProposeAgentArgs{
		Name: "researcher", Prompt: "Analyze the fixture.", Tools: []string{}, TokenBudget: 10000,
	})
	var child struct {
		ID string `json:"agent_id"`
	}
	if err := json.Unmarshal([]byte(invokeAction(t, engine, propose)), &child); err != nil {
		t.Fatal(err)
	}
	finishAction(t, engine, propose)
	delegate := claimAction(t, engine, "runtime.task.delegate", DelegateArgs{AgentID: child.ID, Prompt: "Inspect the recorded facts."})
	invokeAction(t, engine, delegate)
	finishAction(t, engine, delegate)
	var latest string
	for i := 0; i < 25; i++ {
		record, err := engine.store.AddInput(ctx, fmt.Sprintf("input-%d", i), root.SystemID,
			AddInputCommand{Content: fmt.Sprintf("Fixture context %d", i)})
		if err != nil {
			t.Fatal(err)
		}
		var receipt struct {
			ID string `json:"event_id"`
		}
		if err := json.Unmarshal(record.CommandResult, &receipt); err != nil {
			t.Fatal(err)
		}
		latest = receipt.ID
	}
	view, err := engine.store.Activity(ctx, localAdministrator, root.SystemID)
	if err != nil {
		t.Fatal(err)
	}
	if view.System.ID != root.SystemID || len(view.Agents) != 2 || len(view.Calls) != 3 || len(view.Events) != 20 || view.Truncated {
		t.Fatalf("unexpected activity snapshot: %+v", view)
	}
	if view.Agents[0].ID != root.AgentID || view.Agents[0].TaskState != "waiting" ||
		view.Agents[1].Parent != root.AgentID || view.Agents[1].Assignment != "Inspect the recorded facts." {
		t.Fatalf("operator/child work missing: %+v", view.Agents)
	}
	if view.Calls[0].AgentID != child.ID || view.Calls[0].State != "awaiting_worker" ||
		view.Calls[1].Tool != "runtime.task.delegate" || view.Calls[1].ToolState != "completed" || view.Events[0].ID != latest {
		t.Fatalf("activity is not newest-first: %+v %+v", view.Calls, view.Events)
	}
	if _, err := engine.store.Activity(ctx, "foreign-owner", root.SystemID); !errors.Is(err, ErrSystemNotFound) {
		t.Fatalf("foreign principal saw activity: %v", err)
	}
	other, err := engine.store.CreateSystem(ctx, localAdministrator, "other", CreateSystemCommand{Name: "Other", Goal: fixtureGoal()}, cfg, configID)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := engine.store.Activity(ctx, localAdministrator, other.ID)
	if err != nil || len(empty.Agents) != 0 || len(empty.Calls) != 0 || len(empty.Events) != 0 {
		t.Fatalf("inactive/foreign system leaked activity: %+v %v", empty, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := engine.store.Activity(canceled, localAdministrator, root.SystemID); !errors.Is(err, context.Canceled) {
		t.Fatalf("snapshot ignored cancellation: %v", err)
	}
	for i := 0; i < 32; i++ {
		id, err := NewID("agent_")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.store.db.Exec(`INSERT INTO agents(system_id,agent_id,parent_id,goal_id,name,revision,state,depth,token_budget,max_turns)
		 SELECT system_id,?,parent_id,goal_id,?,revision,state,depth,token_budget,max_turns FROM agents WHERE system_id=? AND agent_id=?`,
			id, fmt.Sprintf("fixture-%d", i), root.SystemID, child.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.store.db.Exec(`INSERT INTO agent_revisions(system_id,agent_id,revision,definition,tools)
		 SELECT system_id,?,revision,definition,tools FROM agent_revisions WHERE system_id=? AND agent_id=?`, id, root.SystemID, child.ID); err != nil {
			t.Fatal(err)
		}
	}
	lastCall := ""
	for i := 0; i < 25; i++ {
		id, err := NewID("call_")
		if err != nil {
			t.Fatal(err)
		}
		activation, err := NewID("activation_")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.store.db.Exec(`INSERT INTO model_calls(call_id,system_id,goal_id,task_id,activation_id,activation_owner,revision,model,stream,state,reservation,created_at,queued_at,deadline,wait_deadline,agent_id)
		 SELECT ?,system_id,goal_id,task_id,?,activation_owner,revision,model,stream,state,reservation,created_at,queued_at,deadline,wait_deadline,agent_id
		 FROM model_calls WHERE system_id=? AND call_id=?`, id, activation, root.SystemID, delegate.CallID); err != nil {
			t.Fatal(err)
		}
		lastCall = id
	}
	bounded, err := engine.store.Activity(ctx, localAdministrator, root.SystemID)
	if err != nil || len(bounded.Agents) != 32 || !bounded.Truncated || bounded.Agents[0].ID != root.AgentID ||
		len(bounded.Calls) != 20 || bounded.Calls[0].ID != lastCall {
		t.Fatalf("snapshot limits/order incorrect: agents=%d calls=%d truncated=%v err=%v", len(bounded.Agents), len(bounded.Calls), bounded.Truncated, err)
	}
	assertCount(t, engine.store, "model_attempts", 2)
}

func TestArtifactViewEncodesPlainTextWithoutChangingStoredContent(t *testing.T) {
	cfg := teamConfiguration(t)
	cfg.Bootstrap = nil
	engine, configID := fixtureTeamEngine(t, cfg)
	root := fixtureCall(t, engine.broker, configID, "artifact-view", false)
	put := claimAction(t, engine, "runtime.artifact.put", map[string]string{"content": "A plain text plan, not JSON."})
	var receipt struct {
		ID string `json:"artifact_id"`
	}
	if err := json.Unmarshal([]byte(invokeAction(t, engine, put)), &receipt); err != nil {
		t.Fatal(err)
	}
	view, err := engine.store.Artifact(context.Background(), root.SystemID, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	var text string
	if err := json.Unmarshal(view.Content, &text); err != nil || text != "A plain text plan, not JSON." {
		t.Fatalf("artifact not safely represented as JSON text: %s %v", view.Content, err)
	}
	if view.Digest != ArtifactDigest([]byte(text)) {
		t.Fatal("view rewrote artifact identity")
	}
}
