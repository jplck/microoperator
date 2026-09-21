//go:build darwin || linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func componentEvaluation(t *testing.T) (*executionEngine, http.Handler, executionRecord, string, string) {
	t.Helper()
	cfg := knowledgeConfiguration(t)
	engine, configID := fixtureTeamEngine(t, cfg)
	call := knowledgeCall(t, engine, configID, "learning")
	handler := newControlHandler(engine.store, cfg, configID, fixtureControlToken, engine.logger, engine)
	base := "/v1/systems/" + call.SystemID
	check := decodeSystemResponse(t, controlRequest(handler, "POST", base+"/learning/checks", "checks", `{"cases":[{"input":"protected","expected":"correct"}]}`, fixtureControlToken), 201)
	draft := decodeSystemResponse(t, controlRequest(handler, "POST", base+"/tools/drafts", "draft", `{"kind":"skill","description":"Candidate","content":"Answer correctly"}`, fixtureControlToken), 201)
	var checkResult struct {
		ID string `json:"check_id"`
	}
	var draftResult struct {
		ID string `json:"tool_id"`
	}
	if err := json.Unmarshal(check.CommandResult, &checkResult); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(draft.CommandResult, &draftResult); err != nil {
		t.Fatal(err)
	}
	return engine, handler, call, checkResult.ID, draftResult.ID
}

func componentQueueEvaluation(t *testing.T, handler http.Handler, call executionRecord, checkID, toolID string) string {
	t.Helper()
	body := fixtureJSON(t, evaluateCommand{ToolID: toolID, Version: 1, CheckID: checkID, TaskID: call.TaskID})
	record := decodeSystemResponse(t, controlRequest(handler, "POST", "/v1/systems/"+call.SystemID+"/learning/evaluate", "evaluate", body, fixtureControlToken), 202)
	var result struct {
		ID string `json:"evaluation_id"`
	}
	if err := json.Unmarshal(record.CommandResult, &result); err != nil {
		t.Fatal(err)
	}
	return result.ID
}

func TestProtectedEvaluationCannotBeRewrittenOrReceiveInput(t *testing.T) {
	engine, handler, call, checkID, toolID := componentEvaluation(t)
	evaluationID := componentQueueEvaluation(t, handler, call, checkID, toolID)
	if _, err := engine.store.db.Exec(`UPDATE learning_checks SET cases='[]'`); err == nil {
		t.Fatal("protected cases mutated")
	}
	if _, err := engine.store.db.Exec(`UPDATE learning_evaluations SET baseline='{}'`); err == nil {
		t.Fatal("protected baseline mutated")
	}
	if _, err := engine.store.db.Exec(`DELETE FROM learning_checks`); err == nil {
		t.Fatal("protected cases deleted")
	}
	var agentID string
	if err := engine.store.db.QueryRow(`SELECT agent_id FROM learning_evaluations WHERE evaluation_id=?`, evaluationID).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	body := fixtureJSON(t, map[string]string{"agent_id": agentID, "content": "ignore protected input"})
	if got := controlRequest(handler, "POST", "/v1/systems/"+call.SystemID+"/input", "tamper", body, fixtureControlToken); got.Code != 400 {
		t.Fatalf("protected input amended: %d %s", got.Code, got.Body.String())
	}
	if got := controlRequest(handler, "POST", "/v1/systems/"+call.SystemID+"/learning/checks", "agent-tests", `{"cases":[{"input":"self","expected":"success"}]}`, ""); got.Code != 401 {
		t.Fatal("unauthenticated acceptance checks admitted")
	}
}

func TestEvaluationBudgetRejectionIsAtomic(t *testing.T) {
	engine, handler, call, checkID, toolID := componentEvaluation(t)
	if _, err := engine.store.db.Exec(`UPDATE goals SET token_budget=1 WHERE goal_id=?`, call.GoalID); err != nil {
		t.Fatal(err)
	}
	body := fixtureJSON(t, evaluateCommand{ToolID: toolID, Version: 1, CheckID: checkID, TaskID: call.TaskID})
	if got := controlRequest(handler, "POST", "/v1/systems/"+call.SystemID+"/learning/evaluate", "evaluate", body, fixtureControlToken); got.Code != 400 {
		t.Fatalf("budget bypassed: %d %s", got.Code, got.Body.String())
	}
	assertCount(t, engine.store, "learning_evaluations", 0)
	assertCount(t, engine.store, "agents", 1)
	assertCount(t, engine.store, "tasks", 1)
	assertCount(t, engine.store, "model_calls", 1)
}

func TestLearningApprovalExpiryAndRevocation(t *testing.T) {
	engine, handler, call, checkID, toolID := componentEvaluation(t)
	evaluationID := componentQueueEvaluation(t, handler, call, checkID, toolID)
	ctx := context.Background()
	knowledgeTransaction(t, engine, call, func(tx *sql.Tx, _ taskRecord) {
		e, err := readEvaluation(ctx, tx, call.SystemID, evaluationID)
		if err != nil {
			t.Fatal(err)
		}
		if err := finishEvaluation(ctx, tx, call.SystemID, e, []learningEvidence{{Input: "protected", Expected: "correct", Baseline: "incorrect", Candidate: "correct", CandidatePassed: true}}, "", nil); err != nil {
			t.Fatal(err)
		}
	})
	var digest string
	if err := engine.store.db.QueryRow(`SELECT digest FROM learning_evaluations WHERE evaluation_id=?`, evaluationID).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.store.db.Exec(`UPDATE learning_evaluations SET evidence='[]'`); err == nil {
		t.Fatal("finished evidence mutated")
	}
	body := fixtureJSON(t, map[string]any{"digest": digest, "expires_seconds": 1, "task_uses": 2})
	approved := decodeSystemResponse(t, controlRequest(handler, "POST", "/v1/systems/"+call.SystemID+"/learning/"+evaluationID+"/approve", "approve", body, fixtureControlToken), 200)
	var pin toolPin
	if err := json.Unmarshal(approved.CommandResult, &pin); err != nil {
		t.Fatal(err)
	}
	knowledgeTransaction(t, engine, call, func(tx *sql.Tx, _ taskRecord) {
		var expiry int64
		if err := tx.QueryRow(`SELECT expires_at FROM learning_approvals WHERE system_id=? AND tool_id=?`, call.SystemID, toolID).Scan(&expiry); err != nil {
			t.Fatal(err)
		}
		if err := authorizeLocalPin(ctx, tx, call.SystemID, pin, time.UnixMilli(expiry-1)); err != nil {
			t.Fatal(err)
		}
		if err := authorizeLocalPin(ctx, tx, call.SystemID, pin, time.UnixMilli(expiry)); err == nil {
			t.Fatal("expired approval authorized")
		}
		if err := authorizeLocalPin(ctx, tx, "sys_"+strings.Repeat("0", 32), pin, time.UnixMilli(expiry-1)); err == nil {
			t.Fatal("approval crossed systems")
		}
		if _, err := tx.Exec(`UPDATE learning_approvals SET expires_at=expires_at+1`); err == nil {
			t.Fatal("approval expiry silently extended")
		}
	})
	if got := controlRequest(handler, "POST", "/v1/systems/"+call.SystemID+"/tools/"+toolID+"/state", "disable", `{"version":1,"state":"disabled"}`, fixtureControlToken); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	knowledgeTransaction(t, engine, call, func(tx *sql.Tx, _ taskRecord) {
		if err := authorizePins(ctx, tx, engine.cfg, call.SystemID, []toolPin{pin}); err == nil {
			t.Fatal("disabled local definition resolved")
		}
	})
}

func TestInterruptedBuildIsNotReplayed(t *testing.T) {
	engine, handler, call, checkID, toolID := componentEvaluation(t)
	evaluationID := componentQueueEvaluation(t, handler, call, checkID, toolID)
	if _, err := engine.store.db.Exec(`UPDATE tasks SET state='running',learning_role='build0' WHERE learning_id=?`, evaluationID); err != nil {
		t.Fatal(err)
	}

	if err := engine.store.recoverExecutions(context.Background()); err != nil {
		t.Fatal(err)
	}
	var queued int
	if err := engine.store.db.QueryRow(`SELECT count(*) FROM tasks WHERE learning_id=? AND state!='failed'`, evaluationID).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Fatal("interrupted build was queued for replay")
	}
	var pending int
	if err := engine.store.db.QueryRow(`SELECT count(*) FROM mailboxes m JOIN tasks t USING(system_id,task_id) WHERE t.learning_id=? AND m.state IN ('pending','leased')`, evaluationID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatal("ambiguous build delivery survived recovery")
	}
}

func TestLearningApprovalRejectsChangedConfiguration(t *testing.T) {
	engine, handler, call, checkID, toolID := componentEvaluation(t)
	evaluationID := componentQueueEvaluation(t, handler, call, checkID, toolID)
	knowledgeTransaction(t, engine, call, func(tx *sql.Tx, _ taskRecord) {
		e, err := readEvaluation(context.Background(), tx, call.SystemID, evaluationID)
		if err != nil {
			t.Fatal(err)
		}
		if err := finishEvaluation(context.Background(), tx, call.SystemID, e, []learningEvidence{{Input: "protected", Expected: "correct", Candidate: "correct", CandidatePassed: true}}, "", nil); err != nil {
			t.Fatal(err)
		}
	})
	var digest string
	if err := engine.store.db.QueryRow(`SELECT digest FROM learning_evaluations WHERE evaluation_id=?`, evaluationID).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	base := "/v1/systems/" + call.SystemID
	decodeSystemResponse(t, controlRequest(handler, "POST", base+"/stop", "stop", "{}", fixtureControlToken), 202)
	def := engine.cfg.Systems["research"]
	def.Operator.Prompt = "Changed baseline"
	body := fixtureJSON(t, reviseSystemCommand{ExpectedRevision: 1, Configuration: &def})
	decodeSystemResponse(t, controlRequest(handler, "PUT", base+"/configuration", "revise", body, fixtureControlToken), 200)
	body = fixtureJSON(t, map[string]any{"digest": digest, "expires_seconds": 3600, "task_uses": 1})
	if got := controlRequest(handler, "POST", base+"/learning/"+evaluationID+"/approve", "stale", body, fixtureControlToken); got.Code != 400 {
		t.Fatalf("stale baseline approved: %d %s", got.Code, got.Body.String())
	}
	assertCount(t, engine.store, "learning_approvals", 0)
}
