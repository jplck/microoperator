package state

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type ControlResult struct {
	Scope  string   `json:"scope"`
	ID     string   `json:"id"`
	Action string   `json:"action"`
	Calls  []string `json:"affected_calls"`
}

func stopTasks(ctx context.Context, tx *sql.Tx, systemID, scope, id, source, reason string) (calls []string, err error) {
	if scope == "agent" {
		var goalID string
		err := tx.QueryRowContext(ctx, `SELECT goal_id FROM goals g JOIN systems s USING(system_id)
		 WHERE g.system_id=? AND s.operator_id=? AND g.continuous=1 AND g.state IN ('queued','running','waiting')`, systemID, id).Scan(&goalID)
		if err == nil {
			scope, id = "goal", goalID
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE selected(task_id) AS (
	 SELECT task_id FROM tasks WHERE system_id=? AND (?='system' OR (?='goal' AND goal_id=?) OR (?='agent' AND agent_id=?))
	 UNION SELECT t.task_id FROM tasks t JOIN selected s ON t.parent_task=s.task_id WHERE t.system_id=?
	) SELECT t.task_id,t.call_id FROM tasks t JOIN selected s USING(task_id) WHERE t.system_id=? AND t.state IN ('queued','running','waiting')`,
		systemID, scope, scope, id, scope, id, systemID, systemID)
	if err != nil {
		return nil, err
	}
	var taskIDs []string
	for rows.Next() {
		var task, call string
		if err := rows.Scan(&task, &call); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		taskIDs = append(taskIDs, task)
		calls = append(calls, call)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for _, taskID := range taskIDs {
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET control='stopped' WHERE system_id=? AND task_id=?`, systemID, taskID); err != nil {
			return nil, err
		}
	}
	for _, taskID := range taskIDs {
		t, err := readTask(ctx, tx, systemID, taskID)
		if err != nil {
			return nil, err
		}
		if t.Parent == "" {
			if _, err := tx.ExecContext(ctx, `UPDATE systems SET state='stopping' WHERE system_id=?`, systemID); err != nil {
				return nil, err
			}
		}
		var leased int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mailboxes WHERE system_id=? AND task_id=? AND state='leased'`, systemID, taskID).Scan(&leased); err != nil {
			return nil, err
		}
		if leased == 0 {
			if err := terminateTask(ctx, tx, t, "canceled", "", reason, source, "", time.Now()); err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE model_calls SET state='canceled',reason='stopped before dispatch' WHERE system_id=? AND task_id=? AND state IN ('awaiting_worker','queued')`, systemID, taskID); err != nil {
				return nil, err
			}
		}
	}
	if scope == "system" || scope == "goal" {
		result, err := tx.ExecContext(ctx, `UPDATE goals SET control='stopped',state='canceled' WHERE system_id=? AND continuous=1
		 AND (?='system' OR goal_id=?) AND state IN ('queued','running','waiting','canceled')`, systemID, scope, id)
		if err != nil {
			return nil, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if count > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE agents SET state='stopped' WHERE system_id=? AND
			 (agent_id=(SELECT operator_id FROM systems WHERE system_id=?) OR goal_id IN
			 (SELECT goal_id FROM goals WHERE system_id=? AND continuous=1 AND control='stopped' AND (?='system' OR goal_id=?)))`,
				systemID, systemID, systemID, scope, id); err != nil {
				return nil, err
			}
			for _, table := range []string{"schedules", "subscriptions"} {
				if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET state='canceled',reason='continuous goal stopped' WHERE system_id=? AND state='active'
				 AND task_id IN (SELECT task_id FROM tasks WHERE system_id=? AND goal_id IN
				 (SELECT goal_id FROM goals WHERE system_id=? AND continuous=1 AND control='stopped' AND (?='system' OR goal_id=?)))`,
					systemID, systemID, systemID, scope, id); err != nil {
					return nil, err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE systems SET state=CASE WHEN EXISTS
			 (SELECT 1 FROM mailboxes WHERE system_id=? AND state='leased') THEN 'stopping' ELSE 'stopped' END WHERE system_id=?`,
				systemID, systemID); err != nil {
				return nil, err
			}
		}
	}
	return calls, nil
}
