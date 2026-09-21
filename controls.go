//go:build darwin || linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

type controlResult struct {
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

func (api *controlAPI) control(w http.ResponseWriter, r *http.Request) {
	if !api.executionAvailable(w) {
		return
	}
	action := r.PathValue("action")
	if action != "pause" && action != "resume" && action != "stop" {
		api.failure(w, errSystemNotFound)
		return
	}
	scope, id := "system", r.PathValue("system_id")
	if r.PathValue("agent_id") != "" {
		scope, id = "agent", r.PathValue("agent_id")
	}
	if r.PathValue("goal_id") != "" {
		scope, id = "goal", r.PathValue("goal_id")
	}
	var body struct{}
	key, err := readCommand(w, r, &body)
	if err != nil {
		api.failure(w, err)
		return
	}
	engine := api.engine
	engine.mu.Lock()
	defer engine.mu.Unlock()
	record, err := api.mutation(r.Context(), key, r.PathValue("system_id"), scope+"."+action, struct{ ID string }{id}, func(tx *sql.Tx, s systemRecord) (any, error) {
		result := controlResult{Scope: scope, ID: id, Action: action, Calls: []string{}}
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
				return nil, errExecutionConflict
			}
			state := "paused"
			if action == "resume" {
				state = "running"
			}
			if action == "stop" {
				state = "stopping"
			}
			if _, err := tx.ExecContext(r.Context(), `UPDATE systems SET state=? WHERE system_id=?`, state, s.ID); err != nil {
				return nil, err
			}
		case "goal":
			var state, prior string
			err := tx.QueryRowContext(r.Context(), `SELECT state,control FROM goals WHERE system_id=? AND goal_id=?`, s.ID, id).Scan(&state, &prior)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, errSystemNotFound
			}
			if err != nil {
				return nil, err
			}
			if taskTerminal(state) || prior == "stopped" {
				return nil, errExecutionConflict
			}
			if _, err := tx.ExecContext(r.Context(), `UPDATE goals SET control=? WHERE system_id=? AND goal_id=?`, control, s.ID, id); err != nil {
				return nil, err
			}
		case "agent":
			a, err := readAgent(r.Context(), tx, s.ID, id, 0)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, errSystemNotFound
			}
			if err != nil {
				return nil, err
			}
			if a.State == "stopped" {
				return nil, errExecutionConflict
			}
			if _, err := tx.ExecContext(r.Context(), `UPDATE agents SET state=? WHERE system_id=? AND agent_id=?`, control, s.ID, id); err != nil {
				return nil, err
			}
		}
		if action == "resume" {
			inspected, err := api.cfg.inspect(s)
			if err != nil {
				return nil, err
			}
			if inspected.BlockedReason != "" {
				return nil, invalid("grants", inspected.BlockedReason)
			}
		}
		if action == "stop" {
			calls, err := stopTasks(r.Context(), tx, s.ID, scope, id)
			if err != nil {
				return nil, err
			}
			result.Calls = calls
		}
		return result, nil
	})
	if action == "stop" {
		var result controlResult
		if err == nil {
			if decodeErr := json.Unmarshal(record.CommandResult, &result); decodeErr != nil {
				api.failure(w, decodeErr)
				return
			}
			for _, active := range engine.active {
				for _, call := range result.Calls {
					if active.callID == call {
						active.cancel()
					}
				}
			}
		} else if !errors.Is(err, errSystemNotFound) && !errors.Is(err, errCommandConflict) {
			for _, active := range engine.active {
				if active.systemID == r.PathValue("system_id") && (scope == "system" || (scope == "goal" && active.goalID == id) || (scope == "agent" && active.agentID == id)) {
					active.cancel()
				}
			}
		}
	}
	engine.notify()
	api.systemResponse(w, 202, record, err)
}
