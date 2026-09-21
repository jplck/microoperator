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

func stopTasks(ctx context.Context, tx *sql.Tx, systemID, scope, id string) (calls []string, err error) {
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
			if err := terminateTask(ctx, tx, t, "canceled", "", "stopped by administrator", localAdministrator, "", time.Now()); err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE model_calls SET state='canceled',reason='stopped before dispatch' WHERE system_id=? AND task_id=? AND state IN ('awaiting_worker','queued')`, systemID, taskID); err != nil {
				return nil, err
			}
		}
	}
	return calls, nil
}
