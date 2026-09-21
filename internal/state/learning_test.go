package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func evaluatedFixture(t *testing.T) (*fixtureWorkflow, string, ExecutionRecord, string, string, string) {
	t.Helper()
	ctx := context.Background()
	engine, configID := fixtureTeamEngine(t, knowledgeConfiguration(t))
	call := knowledgeCall(t, engine, configID, "learning")
	checks := CreateChecksCommand{Cases: []ProtectedCase{{Input: "protected", Expected: "correct"}}}
	record, err := engine.store.CreateChecks(ctx, "checks", call.SystemID, checks)
	if err != nil {
		t.Fatal(err)
	}
	var check struct {
		ID string `json:"check_id"`
	}
	if err := json.Unmarshal(record.CommandResult, &check); err != nil {
		t.Fatal(err)
	}
	record, err = engine.store.ProposeTool(ctx, engine.cfg, "draft", call.SystemID, DraftCommand{Kind: "skill", Description: "Candidate", Content: "Answer correctly"})
	if err != nil {
		t.Fatal(err)
	}
	var draft struct {
		ID string `json:"tool_id"`
	}
	if err := json.Unmarshal(record.CommandResult, &draft); err != nil {
		t.Fatal(err)
	}
	record, err = engine.store.EvaluateLearning(ctx, engine.cfg, engine.owner, "evaluate", call.SystemID, EvaluateCommand{ToolID: draft.ID, Version: 1, CheckID: check.ID, TaskID: call.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	var evaluation struct {
		ID string `json:"evaluation_id"`
	}
	if err := json.Unmarshal(record.CommandResult, &evaluation); err != nil {
		t.Fatal(err)
	}
	knowledgeTransaction(t, engine, call, func(tx *sql.Tx, _ TaskRecord) {
		e, err := readEvaluation(ctx, tx, call.SystemID, evaluation.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := finishEvaluation(ctx, tx, call.SystemID, e, []LearningEvidence{{Input: "protected", Expected: "correct", Baseline: "incorrect", Candidate: "correct", CandidatePassed: true}}, "", nil); err != nil {
			t.Fatal(err)
		}
	})
	var digest string
	if err := engine.store.db.QueryRow(`SELECT digest FROM learning_evaluations WHERE evaluation_id=?`, evaluation.ID).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	return engine, configID, call, draft.ID, evaluation.ID, digest
}

func TestLearningApprovalExpiryAndRevocation(t *testing.T) {
	engine, _, call, toolID, evaluationID, digest := evaluatedFixture(t)
	ctx := context.Background()
	if _, err := engine.store.db.Exec(`UPDATE learning_evaluations SET evidence='[]'`); err == nil {
		t.Fatal("finished evidence mutated")
	}
	approved, err := engine.store.ApproveLearning(ctx, engine.cfg, "approve", call.SystemID, ApproveLearningCommand{Digest: digest, ExpiresSeconds: 1, TaskUses: 2}, evaluationID)
	if err != nil {
		t.Fatal(err)
	}
	var pin ToolPin
	if err := json.Unmarshal(approved.CommandResult, &pin); err != nil {
		t.Fatal(err)
	}
	knowledgeTransaction(t, engine, call, func(tx *sql.Tx, _ TaskRecord) {
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
	if _, err := engine.store.ChangeDraftState(ctx, "disable", call.SystemID, ChangeDraftStateCommand{Version: 1, State: "disabled"}, toolID); err != nil {
		t.Fatal(err)
	}
	knowledgeTransaction(t, engine, call, func(tx *sql.Tx, _ TaskRecord) {
		if err := authorizePins(ctx, tx, engine.cfg, call.SystemID, []ToolPin{pin}); err == nil {
			t.Fatal("disabled local definition resolved")
		}
	})
}

func TestLearningApprovalRejectsChangedConfiguration(t *testing.T) {
	engine, configID, call, _, evaluationID, digest := evaluatedFixture(t)
	ctx := context.Background()
	if _, err := engine.store.Control(ctx, engine.cfg, "stop", call.SystemID, "system", "stop", call.SystemID); err != nil {
		t.Fatal(err)
	}
	def := engine.cfg.Systems["research"]
	def.Operator.Prompt = "Changed baseline"
	if _, err := engine.store.ReviseSystem(ctx, localAdministrator, "revise", call.SystemID, ReviseSystemCommand{ExpectedRevision: 1, Configuration: &def}, engine.cfg, configID); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.store.ApproveLearning(ctx, engine.cfg, "stale", call.SystemID, ApproveLearningCommand{Digest: digest, ExpiresSeconds: 3600, TaskUses: 1}, evaluationID); err == nil {
		t.Fatal("stale baseline approved")
	}
	assertCount(t, engine.store, "learning_approvals", 0)
}
