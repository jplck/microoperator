package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

func (store *Store) mutation(ctx context.Context, key string, systemID string, operation string, body any,
	mutate func(*sql.Tx, SystemRecord) (any, error)) (SystemRecord, error) {
	hash, _, err := JsonDigest(struct {
		Operation, SystemID string
		Body                any
	}{operation, systemID, body})
	if err != nil {
		return SystemRecord{}, err
	}
	return store.command(ctx, localAdministrator, key, hash, func(tx *sql.Tx) (SystemRecord, error) {
		record, err := readSystem(ctx, tx, localAdministrator, systemID)
		if err != nil {
			return record, err
		}
		result, err := mutate(tx, record)
		if err != nil {
			return record, err
		}
		data, err := json.Marshal(result)
		if err != nil {
			return record, err
		}
		if err := auditExecution(ctx, tx, systemID, localAdministrator, operation, record.Revision, time.Now()); err != nil {
			return record, err
		}
		record, err = readSystem(ctx, tx, localAdministrator, systemID)
		record.CommandResult = data
		return record, err
	})
}
func (store *Store) ProposeTool(ctx context.Context, cfg Configuration, key string, systemID string, command DraftCommand) (SystemRecord, error) {
	return store.mutation(ctx, key, systemID, "tool.propose", command, func(tx *sql.Tx, s SystemRecord) (any, error) {
		id, err := submitDraft(ctx, tx, cfg, s.ID, localAdministrator, "", "", command)
		return map[string]string{"tool_id": id, "state": "draft"}, err
	})
}

type ChangeDraftStateCommand struct {
	Version int64  `json:"version"`
	State   string `json:"state"`
}

func (store *Store) ChangeDraftState(ctx context.Context, key string, systemID string, command ChangeDraftStateCommand, id string) (SystemRecord, error) {
	if command.Version < 1 || (command.State != "rejected" && command.State != "disabled") {
		return SystemRecord{}, Invalid("state", "revisions may only be rejected or disabled here; approval requires exact protected evaluation evidence")
	}
	return store.mutation(ctx, key, systemID, "tool.draft-state", struct {
		ID      string
		Command any
	}{id, command}, func(tx *sql.Tx, s SystemRecord) (any, error) {
		result, err := tx.ExecContext(ctx, `UPDATE tool_drafts SET state=? WHERE system_id=? AND tool_id=? AND version=?`, command.State, s.ID, id, command.Version)
		if err != nil {
			return nil, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n != 1 {
			return nil, ErrSystemNotFound
		}
		if _, err := tx.ExecContext(ctx, `UPDATE learning_evaluations SET state=?
		 WHERE system_id=? AND tool_id=? AND version=? AND state NOT IN ('queued','evaluating')`,
			command.State, s.ID, id, command.Version); err != nil {
			return nil, err
		}
		return map[string]string{"tool_id": id, "state": command.State}, nil
	})
}

type RevokeToolCommand struct {
	Name    string `json:"name"`
	Version int64  `json:"version"`
}

func (store *Store) RevokeTool(ctx context.Context, cfg Configuration, key string, systemID string, command RevokeToolCommand) (SystemRecord, error) {
	return store.mutation(ctx, key, systemID, "tool.revoke", command, func(tx *sql.Tx, s SystemRecord) (any, error) {
		if strings.HasPrefix(command.Name, "local.") {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM learning_approvals WHERE system_id=? AND tool_id=? AND version=?`, s.ID, command.Name, command.Version).Scan(&count); err != nil {
				return nil, err
			}
			if count != 1 {
				return nil, Invalid("tool", "no approved local version in this system")
			}
		} else if Tool, ok := cfg.Tool(command.Name); !ok || command.Version != Tool.Version {
			return nil, Invalid("tool", "unknown or stale shared version")
		}
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO tool_revocations(system_id,name,version) VALUES(?,?,?)`, s.ID, command.Name, command.Version)
		return command, err
	})
}

type AssignToolsCommand struct {
	ExpectedRevision int64     `json:"expected_revision"`
	Tools            []ToolPin `json:"tools"`
}

func (store *Store) AssignTools(ctx context.Context, cfg Configuration, key string, systemID string, command AssignToolsCommand, agentID string) (SystemRecord, error) {
	return store.mutation(ctx, key, systemID, "agent.assign-tools", struct {
		AgentID string
		Command any
	}{agentID, command}, func(tx *sql.Tx, s SystemRecord) (any, error) {
		if agentID == s.OperatorID {
			return nil, Invalid("agent", "revise the stopped system configuration to assign operator tools")
		}
		a, err := readAgent(ctx, tx, s.ID, agentID, 0)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSystemNotFound
		}
		if err != nil {
			return nil, err
		}
		if a.State == "stopped" {
			return nil, ErrExecutionConflict
		}
		if command.ExpectedRevision != a.Revision {
			return nil, ErrRevisionConflict
		}
		names := []string{}
		for _, pin := range command.Tools {
			found := false
			for _, grant := range s.Grants.SystemTools {
				if grant == pin {
					found = true
					break
				}
			}
			if !found {
				return nil, Invalid("tools", "assignment is not an exact system-granted pin")
			}
			names = append(names, pin.Name)
		}
		if err := UniqueNames(names, "tools"); err != nil {
			return nil, err
		}
		if err := authorizePins(ctx, tx, cfg, s.ID, command.Tools); err != nil {
			return nil, err
		}
		a.Revision++
		a.Tools = append([]ToolPin{}, command.Tools...)
		a.Definition.Tools = names
		if err := insertAgentRevision(ctx, tx, a); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agents SET revision=? WHERE system_id=? AND agent_id=?`, a.Revision, s.ID, a.ID); err != nil {
			return nil, err
		}
		return a, nil
	})
}

type AddInputCommand struct {
	AgentID string `json:"agent_id,omitempty"`
	Content string `json:"content"`
}

func (store *Store) AddInput(ctx context.Context, key string, systemID string, command AddInputCommand) (SystemRecord, error) {
	if strings.TrimSpace(command.Content) == "" || len(command.Content) > 4096 {
		return SystemRecord{}, Invalid("content", "requires 1-4096 bytes")
	}
	return store.mutation(ctx, key, systemID, "user.input", command, func(tx *sql.Tx, s SystemRecord) (any, error) {
		t, err := activeInputTask(ctx, tx, s, command.AgentID)
		if err != nil {
			return nil, err
		}
		id, err := emitEvent(ctx, tx, t, "user.input", localAdministrator, "", struct {
			Content string `json:"content"`
		}{command.Content}, time.Now())
		return map[string]string{"event_id": id, "state": "pending"}, err
	})
}
func (store *Store) Control(ctx context.Context, cfg Configuration, key string, systemID string, scope string, action string, id string) (SystemRecord, error) {
	if (action != "pause" && action != "resume" && action != "stop") || (scope != "system" && scope != "goal" && scope != "agent") {
		return SystemRecord{}, ErrSystemNotFound
	}
	return store.mutation(ctx, key, systemID, scope+"."+action, struct{ ID string }{id}, func(tx *sql.Tx, s SystemRecord) (any, error) {
		result := ControlResult{Scope: scope, ID: id, Action: action, Calls: []string{}}
		control := "paused"
		if action == "resume" {
			control = "active"
		}
		if action == "stop" {
			control = "stopped"
		}
		switch scope {
		case "system":
			if s.State == "stopped" || s.State == "stopping" || s.State == "inactive" {
				return nil, ErrExecutionConflict
			}
			state := "paused"
			if action == "resume" {
				state = "running"
			}
			if action == "stop" {
				state = "stopping"
			}
			if _, err := tx.ExecContext(ctx, `UPDATE systems SET state=? WHERE system_id=?`, state, s.ID); err != nil {
				return nil, err
			}
		case "goal":
			var state, prior string
			err := tx.QueryRowContext(ctx, `SELECT state,control FROM goals WHERE system_id=? AND goal_id=?`, s.ID, id).Scan(&state, &prior)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrSystemNotFound
			}
			if err != nil {
				return nil, err
			}
			if TaskTerminal(state) || prior == "stopped" {
				return nil, ErrExecutionConflict
			}
			if _, err := tx.ExecContext(ctx, `UPDATE goals SET control=? WHERE system_id=? AND goal_id=?`, control, s.ID, id); err != nil {
				return nil, err
			}
		case "agent":
			a, err := readAgent(ctx, tx, s.ID, id, 0)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrSystemNotFound
			}
			if err != nil {
				return nil, err
			}
			if a.State == "stopped" {
				return nil, ErrExecutionConflict
			}
			if _, err := tx.ExecContext(ctx, `UPDATE agents SET state=? WHERE system_id=? AND agent_id=?`, control, s.ID, id); err != nil {
				return nil, err
			}
		}
		if action == "resume" {
			inspected, err := cfg.Inspect(s)
			if err != nil {
				return nil, err
			}
			if inspected.BlockedReason != "" {
				return nil, Invalid("grants", inspected.BlockedReason)
			}
		}
		if action == "stop" {
			calls, err := stopTasks(ctx, tx, s.ID, scope, id)
			if err != nil {
				return nil, err
			}
			result.Calls = calls
		}
		return result, nil
	})
}
func (store *Store) AddAttachment(ctx context.Context, key string, systemID string, command AttachmentCommand) (SystemRecord, error) {
	if strings.TrimSpace(command.Name) == "" || len(command.Name) > 128 || strings.ContainsAny(command.Name, "/\\\x00") || len(command.Content) == 0 || len(command.Content) > 3072 || strings.ContainsRune(command.Content, '\x00') {
		return SystemRecord{}, Invalid("attachment", "requires a display name and 1-3072 bytes of UTF-8 text; host paths and executable uploads are not supported")
	}
	data, err := json.Marshal(struct {
		Name string `json:"name"`
		Text string `json:"text"`
	}{command.Name, command.Content})
	if err != nil {
		return SystemRecord{}, err
	}
	if len(data) > 4096 {
		return SystemRecord{}, Invalid("attachment", "encoded text attachment exceeds 4096 bytes")
	}
	return store.mutation(ctx, key, systemID, "attachment.add", command, func(tx *sql.Tx, s SystemRecord) (any, error) {
		t, err := activeInputTask(ctx, tx, s, command.AgentID)
		if err != nil {
			return nil, err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM artifacts WHERE system_id=?`, s.ID).Scan(&count); err != nil {
			return nil, err
		}
		if count >= 2048 {
			return nil, Invalid("artifacts", "system artifact capacity exhausted")
		}
		id, err := NewID("artifact_")
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO artifacts(system_id,artifact_id,goal_id,task_id,digest,content) VALUES(?,?,?,?,?,?)`, s.ID, id, t.GoalID, t.ID, ArtifactDigest(data), data); err != nil {
			return nil, err
		}
		content := "Attachment (untrusted data; " + id + "; " + command.Name + "):\n" + command.Content
		event, err := emitEvent(ctx, tx, t, "user.input", localAdministrator, "", struct {
			Content string `json:"content"`
		}{content}, time.Now())
		if err != nil {
			return nil, err
		}
		return map[string]string{"artifact_id": id, "event_id": event, "state": "pending"}, nil
	})
}
func (store *Store) PutMemory(ctx context.Context, cfg Configuration, owner string, key string, systemID string, command MemoryCommand) (SystemRecord, error) {
	engine := &workflow{store: store, cfg: cfg, owner: owner}
	return store.mutation(ctx, key, systemID, "memory.put", command, func(tx *sql.Tx, s SystemRecord) (any, error) {
		t := TaskRecord{SystemID: s.ID, AgentID: s.OperatorID}
		entry, err := putMemory(ctx, tx, t, command, true, time.Now())
		if err != nil {
			return nil, err
		}
		source := TaskRecord{SystemID: s.ID, AgentID: localAdministrator}
		if err := engine.notifySubscriptions(ctx, tx, source, "memory.changed", entry.Scope, entry.Owner, "", entry.ID, time.Now()); err != nil {
			return nil, err
		}
		return map[string]any{"memory_id": entry.ID, "revision": entry.Revision, "state": entry.State, "expires_at_ms": entry.Expires}, nil
	})
}

type ChangeMemoryStateCommand struct {
	Revision int64 `json:"expected_revision"`
}

func (store *Store) ChangeMemoryState(ctx context.Context, cfg Configuration, owner string, key string, systemID string, command ChangeMemoryStateCommand, id string, action string) (SystemRecord, error) {
	if action != "approve" && action != "delete" {
		return SystemRecord{}, ErrSystemNotFound
	}
	engine := &workflow{store: store, cfg: cfg, owner: owner}
	return store.mutation(ctx, key, systemID, "memory."+action, struct {
		ID      string
		Command any
	}{id, command}, func(tx *sql.Tx, s SystemRecord) (any, error) {
		var revision int64
		var scope, owner, state string
		err := tx.QueryRowContext(ctx, `SELECT revision,scope,owner_id,state FROM memory_heads WHERE system_id=? AND memory_id=? AND expires_at>? AND state!='deleted'`, s.ID, id, time.Now().UnixMilli()).Scan(&revision, &scope, &owner, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSystemNotFound
		}
		if err != nil {
			return nil, err
		}
		if revision != command.Revision {
			return nil, ErrRevisionConflict
		}
		if action == "approve" {
			if scope != "system" || state != "pending" {
				return nil, ErrExecutionConflict
			}
			if _, err := tx.ExecContext(ctx, `UPDATE memory_heads SET state='active' WHERE system_id=? AND memory_id=?`, s.ID, id); err != nil {
				return nil, err
			}
			if err := engine.notifySubscriptions(ctx, tx, TaskRecord{SystemID: s.ID, AgentID: localAdministrator}, "memory.changed", scope, owner, "", id, time.Now()); err != nil {
				return nil, err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE memory_heads SET state='deleted' WHERE system_id=? AND memory_id=?`, s.ID, id); err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM memory_revisions WHERE system_id=? AND memory_id=?`, s.ID, id); err != nil {
				return nil, err
			}
		}
		return map[string]any{"memory_id": id, "revision": revision, "action": action}, nil
	})
}
func activeInputTask(ctx context.Context, tx *sql.Tx, s SystemRecord, agentID string) (TaskRecord, error) {
	if s.State != "running" && s.State != "paused" {
		return TaskRecord{}, ErrExecutionConflict
	}
	if agentID == "" {
		agentID = s.OperatorID
	}
	var id string
	err := tx.QueryRowContext(ctx, `SELECT t.task_id FROM tasks t JOIN agents a ON a.system_id=t.system_id AND a.agent_id=t.agent_id JOIN goals g ON g.system_id=t.system_id AND g.goal_id=t.goal_id
	 WHERE t.system_id=? AND t.agent_id=? AND t.state IN ('queued','running','waiting') AND t.control!='stopped' AND a.state!='stopped' AND g.control!='stopped'`, s.ID, agentID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskRecord{}, ErrSystemNotFound
	}
	if err != nil {
		return TaskRecord{}, err
	}
	task, err := readTask(ctx, tx, s.ID, id)
	if err == nil && task.LearningID != "" {
		return TaskRecord{}, Invalid("evaluation", "protected inputs cannot be amended")
	}
	return task, err
}
func (store *Store) CreateSchedule(ctx context.Context, cfg Configuration, key string, systemID string, command ScheduleCommand) (SystemRecord, error) {
	return store.mutation(ctx, key, systemID, "schedule.create", command, func(tx *sql.Tx, s SystemRecord) (any, error) {
		t, err := activeInputTask(ctx, tx, s, command.AgentID)
		if err != nil {
			return nil, err
		}
		if err := authorizePins(ctx, tx, cfg, s.ID, t.Tools); err != nil {
			return nil, err
		}
		id, err := createSchedule(ctx, tx, t, command, localAdministrator, time.Now())
		return map[string]string{"schedule_id": id, "state": "active"}, err
	})
}
func (store *Store) CreateSubscription(ctx context.Context, cfg Configuration, key string, systemID string, command SubscriptionCommand) (SystemRecord, error) {
	return store.mutation(ctx, key, systemID, "subscription.create", command, func(tx *sql.Tx, s SystemRecord) (any, error) {
		t, err := activeInputTask(ctx, tx, s, command.AgentID)
		if err != nil {
			return nil, err
		}
		if err := authorizePins(ctx, tx, cfg, s.ID, t.Tools); err != nil {
			return nil, err
		}
		id, err := createSubscription(ctx, tx, t, command, localAdministrator, time.Now())
		return map[string]string{"subscription_id": id, "state": "active"}, err
	})
}

func (store *Store) CancelTrigger(ctx context.Context, key string, systemID string, id string, schedule bool) (SystemRecord, error) {
	table, column := "subscriptions", "subscription_id"
	if schedule {
		table, column = "schedules", "schedule_id"
	}
	return store.mutation(ctx, key, systemID, table+".cancel", struct{ ID string }{id}, func(tx *sql.Tx, s SystemRecord) (any, error) {
		result, err := tx.ExecContext(ctx, `UPDATE `+table+` SET state='canceled',reason='canceled by administrator' WHERE system_id=? AND `+column+`=?`, s.ID, id)
		if err != nil {
			return nil, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if count != 1 {
			return nil, ErrSystemNotFound
		}
		return map[string]string{column: id, "state": "canceled"}, nil
	})
}

type CreateChecksCommand struct {
	Cases []ProtectedCase `json:"cases"`
}

func (store *Store) CreateChecks(ctx context.Context, key string, systemID string, command CreateChecksCommand) (SystemRecord, error) {
	data, err := json.Marshal(command.Cases)
	if err != nil {
		return SystemRecord{}, err
	}
	if _, err := ProtectedCases(data); err != nil {
		return SystemRecord{}, err
	}
	return store.mutation(ctx, key, systemID, "learning.checks", command, func(tx *sql.Tx, s SystemRecord) (any, error) {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM learning_checks WHERE system_id=?`, s.ID).Scan(&count); err != nil {
			return nil, err
		}
		if count >= 64 {
			return nil, Invalid("checks", "protected check capacity exhausted")
		}
		id, err := NewID("check_")
		if err != nil {
			return nil, err
		}
		digest := ArtifactDigest(data)
		_, err = tx.ExecContext(ctx, `INSERT INTO learning_checks(system_id,check_id,cases,digest,created_at) VALUES(?,?,?,?,?)`, s.ID, id, data, digest, time.Now().UnixMilli())
		return map[string]string{"check_id": id, "digest": digest}, err
	})
}

type AddFeedbackCommand struct {
	TaskID  string `json:"task_id"`
	Rating  string `json:"rating"`
	Content string `json:"content"`
}

func (store *Store) AddFeedback(ctx context.Context, key string, systemID string, command AddFeedbackCommand) (SystemRecord, error) {
	if (command.Rating != "success" && command.Rating != "failure") || strings.TrimSpace(command.Content) == "" || len(command.Content) > 2048 {
		return SystemRecord{}, Invalid("feedback", "requires success/failure and 1-2048 bytes of evidence")
	}
	return store.mutation(ctx, key, systemID, "learning.feedback", command, func(tx *sql.Tx, s SystemRecord) (any, error) {
		if _, err := readTask(ctx, tx, s.ID, command.TaskID); err != nil {
			return nil, err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM learning_feedback WHERE system_id=?`, s.ID).Scan(&count); err != nil {
			return nil, err
		}
		if count >= 512 {
			return nil, Invalid("feedback", "feedback capacity exhausted")
		}
		id, err := NewID("feedback_")
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO learning_feedback(system_id,feedback_id,task_id,rating,content,created_at) VALUES(?,?,?,?,?,?)`, s.ID, id, command.TaskID, command.Rating, command.Content, time.Now().UnixMilli())
		return map[string]string{"feedback_id": id}, err
	})
}
func (store *Store) EvaluateLearning(ctx context.Context, cfg Configuration, owner string, key string, systemID string, command EvaluateCommand) (SystemRecord, error) {
	engine := &workflow{store: store, cfg: cfg, owner: owner}
	return store.mutation(ctx, key, systemID, "learning.evaluate", command, func(tx *sql.Tx, s SystemRecord) (any, error) {
		return queueLearning(ctx, tx, engine, s, command, localAdministrator, time.Now())
	})
}

type ApproveLearningCommand struct {
	Digest         string `json:"digest"`
	ExpiresSeconds int64  `json:"expires_seconds"`
	TaskUses       int64  `json:"task_uses"`
}

func (store *Store) ApproveLearning(ctx context.Context, cfg Configuration, key string, systemID string, command ApproveLearningCommand, id string) (SystemRecord, error) {
	if len(command.Digest) != 64 || command.ExpiresSeconds < 1 || command.ExpiresSeconds > 86400 || command.TaskUses < 1 || command.TaskUses > 64 {
		return SystemRecord{}, Invalid("approval", "requires an exact evidence digest, 1-86400 second lifetime and 1-64 task uses")
	}
	return store.mutation(ctx, key, systemID, "learning.approve", struct {
		ID      string
		Command any
	}{id, command}, func(tx *sql.Tx, s SystemRecord) (any, error) {
		var Tool, state, digest, configurationDigest string
		var version, systemRevision int64
		var definition []byte
		err := tx.QueryRowContext(ctx, `SELECT tool_id,version,state,digest,definition,system_revision,configuration_digest FROM learning_evaluations WHERE system_id=? AND evaluation_id=?`, s.ID, id).Scan(&Tool, &version, &state, &digest, &definition, &systemRevision, &configurationDigest)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSystemNotFound
		}
		if err != nil {
			return nil, err
		}
		if state != "pending_approval" || digest != command.Digest {
			return nil, Invalid("approval", "stale, replayed or mismatched evaluation approval")
		}
		s, err = cfg.Inspect(s)
		if err != nil {
			return nil, err
		}
		if s.Revision != systemRevision || s.Grants.DefinitionsDigest != configurationDigest || s.BlockedReason != "" {
			return nil, Invalid("approval", "evaluated configuration or baseline revision changed")
		}
		if _, err := readLearningDraft(ctx, tx, s.ID, Tool, version); err != nil {
			return nil, err
		}
		var def ToolConfig
		if err := DecodeJSON(definition, &def); err != nil {
			return nil, err
		}
		if def.Kind == "executable" {
			if _, err := ReadGeneratedArtifact(cfg.DataDir, s.ID, def.BinaryDigest); err != nil {
				return nil, err
			}
		}
		pinDigest, _, err := JsonDigest(def)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO learning_approvals(system_id,tool_id,version,evaluation_id,definition,digest,expires_at,remaining_tasks) VALUES(?,?,?,?,?,?,?,?)`,
			s.ID, Tool, version, id, definition, pinDigest, time.Now().Add(time.Duration(command.ExpiresSeconds)*time.Second).UnixMilli(), command.TaskUses)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE learning_evaluations SET state='active' WHERE system_id=? AND evaluation_id=?`, s.ID, id); err != nil {
			return nil, err
		}
		return ToolPin{Name: Tool, Version: version, Digest: pinDigest}, nil
	})
}

type AttachmentCommand struct {
	AgentID string `json:"agent_id,omitempty"`
	Name    string `json:"name"`
	Content string `json:"content"`
}
