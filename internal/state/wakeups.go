package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func HasTaskTool(t TaskRecord, name string) bool {
	for _, pin := range t.Tools {
		if pin.Name == name {
			return true
		}
	}
	return false
}

func (engine *workflow) wakeEligible(ctx context.Context, tx *sql.Tx, t TaskRecord, now time.Time) (bool, string, error) {
	var systemState, agentState, goalControl string
	var deadline int64
	if err := tx.QueryRowContext(ctx, `SELECT s.state,a.state,g.control,g.deadline FROM systems s JOIN agents a USING(system_id)
	 JOIN goals g USING(system_id) WHERE s.system_id=? AND a.agent_id=? AND g.goal_id=?`, t.SystemID, t.AgentID, t.GoalID).Scan(&systemState, &agentState, &goalControl, &deadline); err != nil {
		return false, "", err
	}
	if TaskTerminal(t.State) || t.Control == "stopped" || goalControl == "stopped" || agentState == "stopped" || systemState == "stopped" || systemState == "stopping" {
		return false, "target stopped or terminal", nil
	}
	if deadline <= now.UnixMilli() {
		return false, "goal expired", nil
	}
	if err := authorizePins(ctx, tx, engine.cfg, t.SystemID, t.Tools); err != nil {
		if DeniedTask(err) {
			return false, err.Error(), nil
		}
		return false, "", err
	}
	if systemState != "running" || agentState != "active" || goalControl != "active" || t.Control != "active" {
		return false, "", nil
	}
	return true, "", nil
}

func (engine *workflow) pumpWakeups(ctx context.Context, tx *sql.Tx, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_revisions WHERE EXISTS(SELECT 1 FROM memory_heads h WHERE h.system_id=memory_revisions.system_id AND h.memory_id=memory_revisions.memory_id AND (h.expires_at<=? OR h.state='deleted'))`, now.UnixMilli()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_heads SET state='deleted' WHERE expires_at<=? AND state!='deleted'`, now.UnixMilli()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE subscriptions SET state='expired',reason='subscription lifetime expired' WHERE state='active' AND expires_at<=?`, now.UnixMilli()); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT t.system_id,t.task_id FROM tasks t JOIN goals g ON g.system_id=t.system_id AND g.goal_id=t.goal_id
	 WHERE t.state='waiting' AND g.deadline<=? AND NOT EXISTS(SELECT 1 FROM mailboxes m WHERE m.system_id=t.system_id AND m.task_id=t.task_id AND m.state='leased') LIMIT 512`, now.UnixMilli())
	if err != nil {
		return err
	}
	var expired [][2]string
	for rows.Next() {
		var id [2]string
		if err := rows.Scan(&id[0], &id[1]); err != nil {
			return errors.Join(err, rows.Close())
		}
		expired = append(expired, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, id := range expired {
		t, err := readTask(ctx, tx, id[0], id[1])
		if err != nil {
			return err
		}
		if err := terminateTask(ctx, tx, t, "failed", "", "goal lifetime expired while waiting", "runtime", "", now); err != nil {
			return err
		}
	}
	rows, err = tx.QueryContext(ctx, `SELECT q.system_id,q.schedule_id,q.task_id,q.creator,q.expression,q.timezone,q.content,q.next_due,q.expires_at,q.remaining
	 FROM schedules q JOIN tasks t ON t.system_id=q.system_id AND t.task_id=q.task_id
	 JOIN systems s ON s.system_id=t.system_id JOIN agents a ON a.system_id=t.system_id AND a.agent_id=t.agent_id
	 JOIN goals g ON g.system_id=t.system_id AND g.goal_id=t.goal_id
	 WHERE q.state='active' AND q.next_due<=? AND
	 (q.expires_at<=? OR t.state IN ('completed','failed','canceled','rejected') OR t.control='stopped' OR a.state='stopped' OR g.control='stopped' OR s.state IN ('stopped','stopping')
	 OR (s.state='running' AND a.state='active' AND t.control='active' AND g.control='active')) ORDER BY q.next_due,q.schedule_id LIMIT 128`, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return err
	}
	type dueSchedule struct {
		system, id, task, creator, expression, zone, content string
		due, expires                                         int64
		remaining                                            int
	}
	var due []dueSchedule
	for rows.Next() {
		var s dueSchedule
		if err := rows.Scan(&s.system, &s.id, &s.task, &s.creator, &s.expression, &s.zone, &s.content, &s.due, &s.expires, &s.remaining); err != nil {
			return errors.Join(err, rows.Close())
		}
		due = append(due, s)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, s := range due {
		t, err := readTask(ctx, tx, s.system, s.task)
		if err != nil {
			return err
		}
		ready, reason, err := engine.wakeEligible(ctx, tx, t, now)
		if err != nil {
			return err
		}
		if s.creator != localAdministrator && !HasTaskTool(t, "runtime.schedule.create") {
			ready, reason = false, "scheduling grant unavailable"
		}
		state := "active"
		if s.expires <= now.UnixMilli() {
			ready, reason, state = false, "schedule expired", "expired"
		}
		if !ready {
			if reason == "" {
				continue
			}
			if state == "active" {
				state = "canceled"
			}
			if _, err := tx.ExecContext(ctx, `UPDATE schedules SET state=?,reason=? WHERE system_id=? AND schedule_id=?`, state, reason, s.system, s.id); err != nil {
				return err
			}
			continue
		}
		var recorded int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM schedule_occurrences WHERE system_id=? AND schedule_id=? AND due=?`, s.system, s.id, s.due).Scan(&recorded); err != nil {
			return err
		}
		if recorded != 0 {
			return errors.New("schedule cursor did not advance with its occurrence")
		}
		event, emitErr := emitEvent(ctx, tx, t, "schedule.fired", s.creator, "", struct {
			Content string `json:"content"`
		}{fmt.Sprintf("Scheduled context (untrusted data; %s): %s", s.id, s.content)}, now)
		if emitErr != nil {
			if !DeniedTask(emitErr) {
				return emitErr
			}
			if _, err := tx.ExecContext(ctx, `UPDATE schedules SET state='failed',reason=? WHERE system_id=? AND schedule_id=?`, emitErr.Error(), s.system, s.id); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schedule_occurrences(system_id,schedule_id,due,event_id) VALUES(?,?,?,?)`, s.system, s.id, s.due, event); err != nil {
			return err
		}
		next := s.due
		state = "completed"
		if s.expression != "" && s.remaining > 1 {
			// Coalesce missed occurrences into this single event. The next cursor
			// is after now, never a replay of each missed wall-clock instant.
			future, err := NextCron(s.expression, s.zone, now)
			if err != nil {
				return err
			}
			next = future.UnixMilli()
			if next < s.expires {
				state = "active"
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE schedules SET next_due=?,remaining=remaining-1,state=? WHERE system_id=? AND schedule_id=?`, next, state, s.system, s.id); err != nil {
			return err
		}
	}
	return nil
}

func (engine *workflow) notifySubscriptions(ctx context.Context, tx *sql.Tx, source TaskRecord, kind, scope, owner, cause, reference string, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT subscription_id,task_id,creator,scope,expires_at FROM subscriptions WHERE system_id=? AND type=? AND state='active' ORDER BY subscription_id`, source.SystemID, kind)
	if err != nil {
		return err
	}
	type subscription struct {
		id, task, creator, scope string
		expires                  int64
	}
	var subscriptions []subscription
	for rows.Next() {
		var s subscription
		if err := rows.Scan(&s.id, &s.task, &s.creator, &s.scope, &s.expires); err != nil {
			return errors.Join(err, rows.Close())
		}
		subscriptions = append(subscriptions, s)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, s := range subscriptions {
		if s.task == source.ID {
			continue
		} // Suppress a task's own memory feedback loop.
		t, err := readTask(ctx, tx, source.SystemID, s.task)
		if err != nil {
			return err
		}
		if kind == "task.completed" && t.GoalID != source.GoalID {
			continue
		}
		if kind == "memory.changed" && (s.scope != scope || (scope == "task" && owner != t.ID) || (scope == "agent" && owner != t.AgentID)) {
			continue
		}
		ready, reason, err := engine.wakeEligible(ctx, tx, t, now)
		if err != nil {
			return err
		}
		if s.expires <= now.UnixMilli() {
			ready, reason = false, "subscription expired"
		}
		if s.creator != localAdministrator && !HasTaskTool(t, "runtime.events.subscribe") {
			ready, reason = false, "subscription grant unavailable"
		}
		if kind == "memory.changed" && !HasTaskTool(t, "runtime.memory.search") {
			ready, reason = false, "memory retrieval grant unavailable"
		}
		if reason != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE subscriptions SET state='canceled',reason=? WHERE system_id=? AND subscription_id=?`, reason, t.SystemID, s.id); err != nil {
				return err
			}
			continue
		}
		// Paused tasks may durably receive data, but claim will not activate them.
		_ = ready
		eventKind := "memory.changed"
		if kind == "task.completed" {
			eventKind = "task.notice"
		}
		_, emitErr := emitEvent(ctx, tx, t, eventKind, source.AgentID, cause, struct {
			Content string `json:"content"`
		}{fmt.Sprintf("Notification (untrusted data): %s %s", kind, reference)}, now)
		if emitErr != nil {
			if !DeniedTask(emitErr) {
				return emitErr
			}
			if _, err := tx.ExecContext(ctx, `UPDATE subscriptions SET state='failed',reason=? WHERE system_id=? AND subscription_id=?`, emitErr.Error(), t.SystemID, s.id); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE subscriptions SET remaining=remaining-1,state=CASE WHEN remaining=1 THEN 'completed' ELSE 'active' END WHERE system_id=? AND subscription_id=?`, t.SystemID, s.id); err != nil {
			return err
		}
	}
	return nil
}
