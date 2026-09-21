package state

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type GeneratedEvaluation struct {
	Evaluation   LearningEvaluation
	ProfileName  string
	BaselineTool ToolConfig
}

func (store *Store) PrepareEvaluation(ctx context.Context, session ExecutionRecord) (plan GeneratedEvaluation, err error) {
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return plan, err
	}
	defer rollback(tx, &err)
	plan.Evaluation, err = readEvaluation(ctx, tx, session.SystemID, session.LearningID)
	if err != nil {
		return plan, err
	}
	e := plan.Evaluation
	origin, err := readTask(ctx, tx, session.SystemID, e.OriginTask)
	if err != nil {
		return plan, err
	}
	a, err := readAgent(ctx, tx, session.SystemID, origin.AgentID, origin.Revision)
	if err != nil {
		return plan, err
	}
	plan.ProfileName = a.Definition.SandboxProfile
	if e.Baseline.Tool != nil {
		tools, err := loadLocalTools(ctx, tx, session.SystemID, []ToolPin{*e.Baseline.Tool})
		if err != nil {
			return plan, err
		}
		plan.BaselineTool = tools[e.Baseline.Tool.Name]
	}
	return plan, nil
}

// CompleteEvaluation rechecks current authority after the external build. A
// policy/evaluation failure is an ordinary durable result, not a storage failure.
func (store *Store) CompleteEvaluation(ctx context.Context, cfg Configuration, owner string, session ExecutionRecord, evidence []LearningEvidence, binary []byte, failure error) (evaluationErr, err error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return failure, err
	}
	defer rollback(tx, &err)
	e, readErr := readEvaluation(ctx, tx, session.SystemID, session.LearningID)
	if readErr != nil && !DeniedTask(readErr) {
		return failure, readErr
	}
	failure = errors.Join(failure, readErr)
	if failure == nil {
		failure = learningEligible(ctx, tx, cfg, session.SystemID, e, time.Now())
	}
	artifact := ""
	if failure == nil {
		artifact, failure = SaveGeneratedArtifact(cfg.DataDir, session.SystemID, binary)
	}
	if err := finishEvaluation(ctx, tx, session.SystemID, e, evidence, artifact, failure); err != nil {
		return failure, err
	}
	state, reason := "completed", ""
	if failure != nil {
		state, reason = "failed", failure.Error()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state=?,reason=? WHERE system_id=? AND task_id=?`, state, reason, session.SystemID, session.TaskID); err != nil {
		return failure, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mailboxes SET state='acked',applied=1 WHERE system_id=? AND event_id=? AND lease_owner=?`, session.SystemID, session.EventID, owner); err != nil {
		return failure, err
	}
	return failure, tx.Commit()
}
