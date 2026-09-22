package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

const schemaV6 = `
ALTER TABLE goals ADD COLUMN continuous INTEGER NOT NULL DEFAULT 0 CHECK(continuous IN (0,1));
ALTER TABLE goals ADD COLUMN task_lifetime_seconds INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN deadline INTEGER NOT NULL DEFAULT 0;
UPDATE tasks SET deadline=(SELECT deadline FROM goals g WHERE g.system_id=tasks.system_id AND g.goal_id=tasks.goal_id);
PRAGMA user_version=6;
`

func goalIsContinuous(ctx context.Context, tx *sql.Tx, t TaskRecord) (bool, error) {
	var continuous bool
	err := tx.QueryRowContext(ctx, `SELECT continuous FROM goals WHERE system_id=? AND goal_id=?`, t.SystemID, t.GoalID).Scan(&continuous)
	return continuous, err
}

func (cfg Configuration) taskDeadline(model string, lifetime int64, now time.Time) int64 {
	if lifetime > 0 {
		return now.Add(time.Duration(lifetime) * time.Second).UnixMilli()
	}
	wait := int64(3600)
	for _, group := range cfg.Models[model].QuotaGroups {
		wait = min(wait, cfg.QuotaGroups[group].MaxWaitSeconds)
	}
	return now.Add(time.Duration(wait)*time.Second + 3*cfg.Providers[cfg.Models[model].Provider].RequestTimeout()).UnixMilli()
}

// A wake creates work, not another goal or allowance. Dispatch remains scheduler-owned.
func wakeContinuousTask(ctx context.Context, tx *sql.Tx, previous TaskRecord) (TaskRecord, error) {
	if !TaskTerminal(previous.State) {
		return previous, nil
	}
	var continuous bool
	var prompt, control, systemState string
	if err := tx.QueryRowContext(ctx, `SELECT g.continuous,g.prompt,g.control,s.state FROM goals g JOIN systems s USING(system_id)
	 WHERE g.system_id=? AND g.goal_id=?`, previous.SystemID, previous.GoalID).Scan(&continuous, &prompt, &control, &systemState); err != nil {
		return TaskRecord{}, err
	}
	if !continuous || control == "stopped" || (systemState != "running" && systemState != "paused") ||
		previous.Control == "stopped" || previous.LearningID != "" {
		return TaskRecord{}, ErrExecutionConflict
	}
	agent, err := readAgent(ctx, tx, previous.SystemID, previous.AgentID, 0)
	if err != nil {
		return TaskRecord{}, err
	}
	if agent.State == "stopped" {
		return TaskRecord{}, ErrExecutionConflict
	}
	var activeID string
	err = tx.QueryRowContext(ctx, `SELECT task_id FROM tasks WHERE system_id=? AND agent_id=? AND goal_id=? AND state IN ('queued','running','waiting')`,
		previous.SystemID, previous.AgentID, previous.GoalID).Scan(&activeID)
	if err == nil {
		return readTask(ctx, tx, previous.SystemID, activeID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return TaskRecord{}, err
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE system_id=? AND goal_id=? AND state IN ('queued','running','waiting')`,
		previous.SystemID, previous.GoalID).Scan(&active); err != nil {
		return TaskRecord{}, err
	}
	if active >= MaxGoalTasks {
		return TaskRecord{}, Invalid("tasks", "continuous goal active-task capacity exhausted")
	}
	pins := []ToolPin{}
	for _, pin := range agent.Tools {
		if slices.Contains(previous.Tools, pin) {
			pins = append(pins, pin)
		}
	}
	tools, err := json.Marshal(pins)
	if err != nil {
		return TaskRecord{}, err
	}
	history := append([]ChatMessage{}, previous.Conversation...)
	if previous.Response != "" {
		history = append(history, ChatMessage{Role: "assistant", Content: previous.Response})
	}
	recent := []ChatMessage{}
	size := 0
	for i := len(history) - 1; i >= 0; i-- {
		message := history[i]
		if message.Role != "user" && message.Role != "assistant" || len(message.ToolCalls) != 0 || message.Content == prompt {
			continue
		}
		if len(recent) == 8 || size+len(message.Content) > 8192 {
			break
		}
		recent = append(recent, message)
		size += len(message.Content)
	}
	slices.Reverse(recent)
	conversation := []ChatMessage{{Role: "user", Content: prompt}, {Role: "user",
		Content: "Continuing the same goal. Only bounded recent conversation follows; use scoped memory/artifacts for older context. Previous task outcome: " + previous.State + ". " + previous.Reason + " Do not replay actions with unknown outcomes."}}
	conversation = append(conversation, recent...)
	body, err := json.Marshal(conversation)
	if err != nil {
		return TaskRecord{}, err
	}
	id, err := NewID("task_")
	if err != nil {
		return TaskRecord{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tasks(system_id,task_id,goal_id,agent_id,agent_revision,parent_task,state,tools,conversation,turns,call_id)
	 VALUES(?,?,?,?,?,?,'queued',?,?,0,'')`, previous.SystemID, id, previous.GoalID, agent.ID, agent.Revision, previous.Parent, tools, body); err != nil {
		return TaskRecord{}, err
	}
	return readTask(ctx, tx, previous.SystemID, id)
}

func continuePendingInput(ctx context.Context, tx *sql.Tx, task TaskRecord) error {
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mailboxes m JOIN events e ON e.system_id=m.system_id AND e.event_id=m.event_id
	 WHERE m.system_id=? AND m.task_id=? AND m.state='pending' AND e.type IN ('user.input','schedule.fired','memory.changed','task.notice')`,
		task.SystemID, task.ID).Scan(&pending); err != nil {
		return err
	}

	if pending == 0 {
		return nil
	}
	next, err := wakeContinuousTask(ctx, tx, task)
	if err != nil {
		return err
	}
	// Preserve the accepted event/receipt and its original provenance, moving only delivery.
	_, err = tx.ExecContext(ctx, `UPDATE mailboxes SET task_id=? WHERE system_id=? AND task_id=? AND state='pending' AND
	 event_id IN (SELECT event_id FROM events WHERE system_id=? AND type IN ('user.input','schedule.fired','memory.changed','task.notice'))`,
		next.ID, task.SystemID, task.ID, task.SystemID)
	return err
}

func emitTaskWakeup(ctx context.Context, tx *sql.Tx, task TaskRecord, kind, source, cause, content string, now time.Time) (next TaskRecord, event string, err error) {
	if _, err = tx.ExecContext(ctx, "SAVEPOINT task_wakeup"); err != nil {
		return next, "", err
	}
	next, err = wakeContinuousTask(ctx, tx, task)
	if err == nil {
		event, err = emitEvent(ctx, tx, next, kind, source, cause, struct {
			Content string `json:"content"`
		}{content}, now)
	}
	if err != nil {
		if _, rollbackErr := tx.ExecContext(ctx, "ROLLBACK TO task_wakeup"); rollbackErr != nil {
			return next, "", rollbackErr
		}
	}
	if _, releaseErr := tx.ExecContext(ctx, "RELEASE task_wakeup"); releaseErr != nil {
		return next, "", releaseErr
	}
	return next, event, err
}
