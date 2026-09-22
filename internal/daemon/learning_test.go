//go:build darwin || linux

package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jplck/microoperator/internal/state"
)

func componentEvaluation(t *testing.T) (*executionEngine, http.Handler, state.ExecutionRecord, string, string) {
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

func componentQueueEvaluation(t *testing.T, handler http.Handler, call state.ExecutionRecord, checkID, toolID string) string {
	t.Helper()
	body := fixtureJSON(t, state.EvaluateCommand{ToolID: toolID, Version: 1, CheckID: checkID, TaskID: call.TaskID})
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
	if _, err := testDB(t, engine.store).Exec(`UPDATE learning_checks SET cases='[]'`); err == nil {
		t.Fatal("protected cases mutated")
	}
	if _, err := testDB(t, engine.store).Exec(`UPDATE learning_evaluations SET baseline='{}'`); err == nil {
		t.Fatal("protected baseline mutated")
	}
	if _, err := testDB(t, engine.store).Exec(`DELETE FROM learning_checks`); err == nil {
		t.Fatal("protected cases deleted")
	}
	var agentID string
	if err := testDB(t, engine.store).QueryRow(`SELECT agent_id FROM learning_evaluations WHERE evaluation_id=?`, evaluationID).Scan(&agentID); err != nil {
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
	if _, err := testDB(t, engine.store).Exec(`UPDATE goals SET token_budget=1 WHERE goal_id=?`, call.GoalID); err != nil {
		t.Fatal(err)
	}
	body := fixtureJSON(t, state.EvaluateCommand{ToolID: toolID, Version: 1, CheckID: checkID, TaskID: call.TaskID})
	if got := controlRequest(handler, "POST", "/v1/systems/"+call.SystemID+"/learning/evaluate", "evaluate", body, fixtureControlToken); got.Code != 400 {
		t.Fatalf("budget bypassed: %d %s", got.Code, got.Body.String())
	}
	assertCount(t, engine.store, "learning_evaluations", 0)
	assertCount(t, engine.store, "agents", 1)
	assertCount(t, engine.store, "tasks", 1)
	assertCount(t, engine.store, "model_calls", 1)
}

func TestInterruptedBuildIsNotReplayed(t *testing.T) {
	engine, handler, call, checkID, toolID := componentEvaluation(t)
	evaluationID := componentQueueEvaluation(t, handler, call, checkID, toolID)
	if _, err := testDB(t, engine.store).Exec(`UPDATE tasks SET state='running',learning_role='build0' WHERE learning_id=?`, evaluationID); err != nil {
		t.Fatal(err)
	}

	if err := engine.store.RecoverExecutions(context.Background()); err != nil {
		t.Fatal(err)
	}
	var queued int
	if err := testDB(t, engine.store).QueryRow(`SELECT count(*) FROM tasks WHERE learning_id=? AND state!='failed'`, evaluationID).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Fatal("interrupted build was queued for replay")
	}
	var pending int
	if err := testDB(t, engine.store).QueryRow(`SELECT count(*) FROM mailboxes m JOIN tasks t USING(system_id,task_id) WHERE t.learning_id=? AND m.state IN ('pending','leased')`, evaluationID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatal("ambiguous build delivery survived recovery")
	}
}
