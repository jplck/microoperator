package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jplck/microoperator/internal/protocol"
)

type LearningEvaluation struct {
	ID, ToolID, CheckID, OriginTask, AgentID, State, ProfileDigest, ToolchainDigest, RuntimeDigest string
	ConfigurationDigest                                                                            string
	Version, SystemRevision                                                                        int64
	Notify                                                                                         bool
	Baseline                                                                                       EvaluationBaseline
	Cases                                                                                          []ProtectedCase
	CheckDigest                                                                                    string
	Draft                                                                                          DraftCommand
}

func readEvaluation(ctx context.Context, tx *sql.Tx, systemID, id string) (e LearningEvaluation, err error) {
	var baseline, cases []byte
	err = tx.QueryRowContext(ctx, `SELECT e.evaluation_id,e.tool_id,e.version,e.check_id,e.origin_task,e.agent_id,e.state,e.profile_digest,e.toolchain_digest,e.runtime_digest,e.baseline,c.cases,c.digest,e.system_revision,e.configuration_digest,e.notify
	 FROM learning_evaluations e JOIN learning_checks c ON c.system_id=e.system_id AND c.check_id=e.check_id WHERE e.system_id=? AND e.evaluation_id=?`, systemID, id).
		Scan(&e.ID, &e.ToolID, &e.Version, &e.CheckID, &e.OriginTask, &e.AgentID, &e.State, &e.ProfileDigest, &e.ToolchainDigest, &e.RuntimeDigest, &baseline, &cases, &e.CheckDigest, &e.SystemRevision, &e.ConfigurationDigest, &e.Notify)
	if err != nil {
		return e, err
	}
	if err := protocol.DecodeJSON(baseline, &e.Baseline); err != nil {
		return e, err
	}
	e.Cases, err = ProtectedCases(cases)
	if err != nil {
		return e, err
	}
	e.Draft, err = readLearningDraft(ctx, tx, systemID, e.ToolID, e.Version)
	return e, err
}

func learningEligible(ctx context.Context, tx *sql.Tx, cfg Configuration, systemID string, e LearningEvaluation, now time.Time) error {
	record, err := readSystem(ctx, tx, localAdministrator, systemID)
	if err != nil {
		return err
	}
	record, err = cfg.Inspect(record)
	if err != nil {
		return err
	}
	if record.Revision != e.SystemRevision || record.Grants.DefinitionsDigest != e.ConfigurationDigest || record.BlockedReason != "" {
		return Invalid("evaluation", "evaluated configuration changed")
	}
	t, err := readTask(ctx, tx, systemID, e.OriginTask)
	if err != nil {
		return err
	}
	if TaskTerminal(t.State) || t.Control == "stopped" {
		return Invalid("evaluation", "originating work stopped")
	}
	a, err := readAgent(ctx, tx, systemID, t.AgentID, t.Revision)
	if err != nil {
		return err
	}
	if a.State == "stopped" {
		return Invalid("evaluation", "originating agent stopped")
	}
	var state, control string
	var deadline int64
	if err := tx.QueryRowContext(ctx, `SELECT s.state,g.control,g.deadline FROM systems s JOIN goals g USING(system_id) WHERE s.system_id=? AND g.goal_id=?`, systemID, t.GoalID).Scan(&state, &control, &deadline); err != nil {
		return err
	}
	if (state != "running" && state != "paused") || control == "stopped" || now.UnixMilli() >= deadline {
		return Invalid("evaluation", "owning goal stopped or expired")
	}
	if err := authorizePins(ctx, tx, cfg, systemID, t.Tools); err != nil {
		return err
	}
	digest, _, err := JsonDigest(cfg.SandboxProfiles[a.Definition.SandboxProfile])
	if err != nil {
		return err
	}
	if digest != e.ProfileDigest || cfg.RuntimeDigest != e.RuntimeDigest {
		return Invalid("evaluation", "runtime or capability profile changed")
	}
	return nil
}

func (engine *workflow) claimLearning(ctx context.Context, tx *sql.Tx, record SystemRecord, t TaskRecord, c mailboxCandidate, now time.Time) (ExecutionRecord, error) {
	e, err := readEvaluation(ctx, tx, t.SystemID, t.LearningID)
	if err != nil {
		return ExecutionRecord{}, err
	}
	if e.State != "queued" || t.LearningRole != "build0" {
		return ExecutionRecord{}, Invalid("evaluation", "build is not safely replayable")
	}
	if err := learningEligible(ctx, tx, engine.cfg, t.SystemID, e, now); err != nil {
		return ExecutionRecord{}, err
	}
	var deadline int64
	if err := tx.QueryRowContext(ctx, `SELECT deadline FROM goals WHERE system_id=? AND goal_id=?`, t.SystemID, t.GoalID).Scan(&deadline); err != nil {
		return ExecutionRecord{}, err
	}
	deadline = min(deadline, now.Add(120*time.Second).UnixMilli())
	if _, err := tx.ExecContext(ctx, `UPDATE learning_evaluations SET state='evaluating' WHERE system_id=? AND evaluation_id=?`, t.SystemID, e.ID); err != nil {
		return ExecutionRecord{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state='running' WHERE system_id=? AND task_id=?`, t.SystemID, t.ID); err != nil {
		return ExecutionRecord{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mailboxes SET state='leased',lease_owner=?,lease_until=?,attempts=attempts+1 WHERE system_id=? AND event_id=?`, engine.owner, deadline, t.SystemID, c.eventID); err != nil {
		return ExecutionRecord{}, err
	}
	return ExecutionRecord{LearningID: e.ID, CallID: e.ID, SystemID: t.SystemID, GoalID: t.GoalID, TaskID: t.ID, AgentID: t.AgentID, EventID: c.eventID, Deadline: deadline, Revision: record.Revision, ActivationID: e.ID}, nil
}

func finishEvaluation(ctx context.Context, tx *sql.Tx, systemID string, e LearningEvaluation, evidence []LearningEvidence, artifact string, failure error) error {
	if failure == nil {
		if len(evidence) != len(e.Cases) {
			failure = errors.New("protected cases did not all complete")
		}
		for _, check := range evidence {
			if !check.CandidatePassed {
				failure = errors.New("candidate failed a protected acceptance check")
			}
		}
	}
	state, reason := "pending_approval", ""
	if failure != nil {
		state, reason = "failed", failure.Error()
	}
	if len(reason) > 8192 {
		reason = reason[:8192]
	}
	def := CandidateDefinition(e.Draft, e.Version, artifact, "")
	if e.Draft.Kind == "executable" {
		def.ProfileDigest = e.ProfileDigest
		def.RuntimeDigest = e.RuntimeDigest
	}
	definition, err := json.Marshal(def)
	if err != nil {
		return err
	}
	data, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	digest, _, err := JsonDigest(struct {
		ToolID                                        string
		Version                                       int64
		CheckDigest                                   string
		Baseline                                      EvaluationBaseline
		Definition                                    ToolConfig
		Evidence                                      []LearningEvidence
		ToolchainDigest, RuntimeDigest, BuildSettings string
		SystemRevision                                int64
		ConfigurationDigest                           string
	}{e.ToolID, e.Version, e.CheckDigest, e.Baseline, def, evidence, e.ToolchainDigest, e.RuntimeDigest, "abi=1;CGO_ENABLED=0;GOTOOLCHAIN=local;GOPROXY=off;GOSUMDB=off;GOENV=off;GO111MODULE=off;GOOS=linux;GOARCH=amd64;trimpath;buildvcs=false", e.SystemRevision, e.ConfigurationDigest})
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE learning_evaluations SET state=?,reason=?,evidence=?,artifact_digest=?,definition=?,digest=? WHERE system_id=? AND evaluation_id=? AND state IN ('queued','evaluating')`,
		state, reason, data, artifact, definition, digest, systemID, e.ID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return Invalid("evaluation", "evaluation already finished or was disabled")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agents SET state='stopped' WHERE system_id=? AND agent_id=?`, systemID, e.AgentID); err != nil {
		return err
	}
	if e.Notify {
		origin, err := readTask(ctx, tx, systemID, e.OriginTask)
		if err != nil {
			return err
		}
		if !TaskTerminal(origin.State) && origin.Control != "stopped" {
			content := "Protected evaluation " + e.ID + " finished: " + state + ". Evidence digest: " + digest + ". This is evidence, not approval or an assignment."
			if _, err := emitEvent(ctx, tx, origin, "task.notice", "runtime", "", struct {
				Content string `json:"content"`
			}{content}, time.Now()); err != nil {
				if !DeniedTask(err) {
					return err
				}
				return terminateTask(ctx, tx, origin, "rejected", "", "evaluation notification rejected: "+err.Error(), "runtime", "", time.Now())
			}
		}
	}
	return nil
}

func (engine *workflow) pumpLearning(ctx context.Context, tx *sql.Tx, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT system_id,evaluation_id FROM learning_evaluations WHERE state IN ('queued','evaluating')
	 AND NOT EXISTS(SELECT 1 FROM tasks t WHERE t.system_id=learning_evaluations.system_id AND t.learning_id=learning_evaluations.evaluation_id AND t.state IN ('queued','running','waiting')) LIMIT 64`)
	if err != nil {
		return err
	}
	var ids [][2]string
	for rows.Next() {
		var id [2]string
		if err := rows.Scan(&id[0], &id[1]); err != nil {
			return errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, id := range ids {
		e, err := readEvaluation(ctx, tx, id[0], id[1])
		if err != nil && !DeniedTask(err) {
			return err
		}
		failure := err
		if failure == nil {
			failure = learningEligible(ctx, tx, engine.cfg, id[0], e, now)
		}
		if e.Draft.Kind == "executable" && failure == nil {
			failure = errors.New("build outcome unknown after interruption; submit a new revision")
		}
		var evidence []LearningEvidence
		if e.Draft.Kind == "skill" {
			for i, check := range e.Cases {
				value := LearningEvidence{Input: check.Input, Expected: check.Expected}
				for _, role := range []string{"baseline", "candidate"} {
					var state, response, reason string
					err := tx.QueryRowContext(ctx, `SELECT state,response,reason FROM tasks WHERE system_id=? AND learning_id=? AND learning_role=?`, id[0], e.ID, fmt.Sprintf("%s%d", role, i)).Scan(&state, &response, &reason)
					if err != nil {
						return err
					}
					if state != "completed" {
						failure = errors.New("protected model evaluation did not complete: " + reason)
					}
					passed := state == "completed" && response == check.Expected
					if len(response) > 4096 {
						response = response[:4096]
					}
					if role == "baseline" {
						value.Baseline = response
						value.BaselinePassed = passed
					} else {
						value.Candidate = response
						value.CandidatePassed = passed
					}
				}
				evidence = append(evidence, value)
			}
		}
		if err := finishEvaluation(ctx, tx, id[0], e, evidence, "", failure); err != nil {
			return err
		}
	}
	return nil
}
