//go:build darwin || linux

package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

func (api *controlAPI) registerKnowledgeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/systems/{system_id}/memory", api.memory)
	mux.HandleFunc("POST /v1/systems/{system_id}/memory", api.memory)
	mux.HandleFunc("POST /v1/systems/{system_id}/memory/{memory_id}/{action}", api.memoryState)
	mux.HandleFunc("GET /v1/systems/{system_id}/schedules", api.schedules)
	mux.HandleFunc("POST /v1/systems/{system_id}/schedules", api.schedules)
	mux.HandleFunc("POST /v1/systems/{system_id}/schedules/{schedule_id}/cancel", api.cancelSchedule)
	mux.HandleFunc("GET /v1/systems/{system_id}/subscriptions", api.subscriptions)
	mux.HandleFunc("POST /v1/systems/{system_id}/subscriptions", api.subscriptions)
	mux.HandleFunc("POST /v1/systems/{system_id}/subscriptions/{subscription_id}/cancel", api.cancelSubscription)
}

func (api *controlAPI) memory(w http.ResponseWriter, r *http.Request) {
	if !api.executionAvailable(w) {
		return
	}
	id := r.PathValue("system_id")
	if r.Method == "POST" {
		var command memoryCommand
		key, err := readCommand(w, r, &command)
		if err != nil {
			api.failure(w, err)
			return
		}
		record, err := api.mutation(r.Context(), key, id, "memory.put", command, func(tx *sql.Tx, s systemRecord) (any, error) {
			t := taskRecord{SystemID: s.ID, AgentID: s.OperatorID}
			entry, err := putMemory(r.Context(), tx, t, command, true, time.Now())
			if err != nil {
				return nil, err
			}
			source := taskRecord{SystemID: s.ID, AgentID: localAdministrator}
			if err := api.engine.notifySubscriptions(r.Context(), tx, source, "memory.changed", entry.Scope, entry.Owner, "", entry.ID, time.Now()); err != nil {
				return nil, err
			}
			return map[string]any{"memory_id": entry.ID, "revision": entry.Revision, "state": entry.State, "expires_at_ms": entry.Expires}, nil
		})
		if err == nil {
			api.engine.notify()
		}
		api.systemResponse(w, 201, record, err)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		api.failure(w, invalid("query", "invalid memory query"))
		return
	}
	for key, values := range query {
		if len(values) != 1 || (key != "scope" && key != "owner_id" && key != "query" && key != "after" && key != "include_pending") {
			api.failure(w, invalid("query", "unknown or repeated memory query field"))
			return
		}
	}
	search := memoryQuery{Scope: query.Get("scope"), Owner: query.Get("owner_id"), Query: query.Get("query"), After: query.Get("after")}
	if search.Scope == "" {
		search.Scope = "system"
	}
	if value := query.Get("include_pending"); value != "" {
		search.Pending, err = strconv.ParseBool(value)
		if err != nil {
			api.failure(w, invalid("include_pending", "requires a boolean"))
			return
		}
	}
	var result any
	err = func() (err error) {
		tx, err := api.store.db.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer rollback(tx, &err)
		record, err := readSystem(r.Context(), tx, localAdministrator, id)
		if err != nil {
			return err
		}
		result, err = searchMemory(r.Context(), tx, taskRecord{SystemID: id, AgentID: record.OperatorID}, search, true, time.Now())
		return err
	}()
	if err != nil {
		api.failure(w, err)
		return
	}
	api.respond(w, 200, result)
}

func (api *controlAPI) memoryState(w http.ResponseWriter, r *http.Request) {
	if !api.executionAvailable(w) {
		return
	}
	action := r.PathValue("action")
	if action != "approve" && action != "delete" {
		api.failure(w, errSystemNotFound)
		return
	}
	var command struct {
		Revision int64 `json:"expected_revision"`
	}
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	id := r.PathValue("memory_id")
	record, err := api.mutation(r.Context(), key, r.PathValue("system_id"), "memory."+action, struct {
		ID      string
		Command any
	}{id, command}, func(tx *sql.Tx, s systemRecord) (any, error) {
		var revision int64
		var scope, owner, state string
		err := tx.QueryRowContext(r.Context(), `SELECT revision,scope,owner_id,state FROM memory_heads WHERE system_id=? AND memory_id=? AND expires_at>? AND state!='deleted'`, s.ID, id, time.Now().UnixMilli()).Scan(&revision, &scope, &owner, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errSystemNotFound
		}
		if err != nil {
			return nil, err
		}
		if revision != command.Revision {
			return nil, errRevisionConflict
		}
		if action == "approve" {
			if scope != "system" || state != "pending" {
				return nil, errExecutionConflict
			}
			if _, err := tx.ExecContext(r.Context(), `UPDATE memory_heads SET state='active' WHERE system_id=? AND memory_id=?`, s.ID, id); err != nil {
				return nil, err
			}
			if err := api.engine.notifySubscriptions(r.Context(), tx, taskRecord{SystemID: s.ID, AgentID: localAdministrator}, "memory.changed", scope, owner, "", id, time.Now()); err != nil {
				return nil, err
			}
		} else {
			if _, err := tx.ExecContext(r.Context(), `UPDATE memory_heads SET state='deleted' WHERE system_id=? AND memory_id=?`, s.ID, id); err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(r.Context(), `DELETE FROM memory_revisions WHERE system_id=? AND memory_id=?`, s.ID, id); err != nil {
				return nil, err
			}
		}
		return map[string]any{"memory_id": id, "revision": revision, "action": action}, nil
	})
	if err == nil {
		api.engine.notify()
	}
	api.systemResponse(w, 200, record, err)
}

func activeInputTask(ctx context.Context, tx *sql.Tx, s systemRecord, agentID string) (taskRecord, error) {
	if s.State != "running" && s.State != "paused" {
		return taskRecord{}, errExecutionConflict
	}
	if agentID == "" {
		agentID = s.OperatorID
	}
	var id string
	err := tx.QueryRowContext(ctx, `SELECT t.task_id FROM tasks t JOIN agents a ON a.system_id=t.system_id AND a.agent_id=t.agent_id JOIN goals g ON g.system_id=t.system_id AND g.goal_id=t.goal_id
	 WHERE t.system_id=? AND t.agent_id=? AND t.state IN ('queued','running','waiting') AND t.control!='stopped' AND a.state!='stopped' AND g.control!='stopped'`, s.ID, agentID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return taskRecord{}, errSystemNotFound
	}
	if err != nil {
		return taskRecord{}, err
	}
	task, err := readTask(ctx, tx, s.ID, id)
	if err == nil && task.LearningID != "" {
		return taskRecord{}, invalid("evaluation", "protected inputs cannot be amended")
	}
	return task, err
}

func (api *controlAPI) schedules(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		api.triggerList(w, r, true)
		return
	}
	if !api.executionAvailable(w) {
		return
	}
	var command scheduleCommand
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	record, err := api.mutation(r.Context(), key, r.PathValue("system_id"), "schedule.create", command, func(tx *sql.Tx, s systemRecord) (any, error) {
		t, err := activeInputTask(r.Context(), tx, s, command.AgentID)
		if err != nil {
			return nil, err
		}
		if err := authorizePins(r.Context(), tx, api.cfg, s.ID, t.Tools); err != nil {
			return nil, err
		}
		id, err := createSchedule(r.Context(), tx, t, command, localAdministrator, time.Now())
		return map[string]string{"schedule_id": id, "state": "active"}, err
	})
	if err == nil {
		api.engine.notify()
	}
	api.systemResponse(w, 201, record, err)
}

func (api *controlAPI) subscriptions(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		api.triggerList(w, r, false)
		return
	}
	if !api.executionAvailable(w) {
		return
	}
	var command subscriptionCommand
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	record, err := api.mutation(r.Context(), key, r.PathValue("system_id"), "subscription.create", command, func(tx *sql.Tx, s systemRecord) (any, error) {
		t, err := activeInputTask(r.Context(), tx, s, command.AgentID)
		if err != nil {
			return nil, err
		}
		if err := authorizePins(r.Context(), tx, api.cfg, s.ID, t.Tools); err != nil {
			return nil, err
		}
		id, err := createSubscription(r.Context(), tx, t, command, localAdministrator, time.Now())
		return map[string]string{"subscription_id": id, "state": "active"}, err
	})
	if err == nil {
		api.engine.notify()
	}
	api.systemResponse(w, 201, record, err)
}

func (api *controlAPI) cancelSchedule(w http.ResponseWriter, r *http.Request) {
	api.cancelTrigger(w, r, true)
}
func (api *controlAPI) cancelSubscription(w http.ResponseWriter, r *http.Request) {
	api.cancelTrigger(w, r, false)
}

func (api *controlAPI) cancelTrigger(w http.ResponseWriter, r *http.Request, schedule bool) {
	var command struct{}
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	table, column, id := "subscriptions", "subscription_id", r.PathValue("subscription_id")
	if schedule {
		table, column, id = "schedules", "schedule_id", r.PathValue("schedule_id")
	}
	record, err := api.mutation(r.Context(), key, r.PathValue("system_id"), table+".cancel", struct{ ID string }{id}, func(tx *sql.Tx, s systemRecord) (any, error) {
		result, err := tx.ExecContext(r.Context(), `UPDATE `+table+` SET state='canceled',reason='canceled by administrator' WHERE system_id=? AND `+column+`=?`, s.ID, id)
		if err != nil {
			return nil, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if count != 1 {
			return nil, errSystemNotFound
		}
		return map[string]string{column: id, "state": "canceled"}, nil
	})
	api.systemResponse(w, 200, record, err)
}

func (api *controlAPI) triggerList(w http.ResponseWriter, r *http.Request, schedule bool) {
	after, err := runtimeCursor(r)
	if err != nil {
		api.failure(w, err)
		return
	}
	id := r.PathValue("system_id")
	if _, err := api.store.getSystem(r.Context(), localAdministrator, id); err != nil {
		api.failure(w, err)
		return
	}
	table, column := "subscriptions", "subscription_id"
	fields := `type,scope,'',0`
	if schedule {
		table, column, fields = "schedules", "schedule_id", `expression,timezone,content,next_due`
	}
	rows, err := api.store.db.QueryContext(r.Context(), `SELECT `+column+`,task_id,creator,`+fields+`,expires_at,remaining,state,reason FROM `+table+` WHERE system_id=? AND `+column+`>? ORDER BY `+column+` LIMIT 21`, id, after)
	if err != nil {
		api.failure(w, err)
		return
	}
	type entry struct {
		ID        string `json:"id"`
		TaskID    string `json:"task_id"`
		Creator   string `json:"creator"`
		Cron      string `json:"cron,omitempty"`
		Timezone  string `json:"timezone,omitempty"`
		Type      string `json:"type,omitempty"`
		Scope     string `json:"scope,omitempty"`
		Content   string `json:"content,omitempty"`
		Due       int64  `json:"next_due_ms,omitempty"`
		Expires   int64  `json:"expires_at_ms"`
		Remaining int    `json:"remaining"`
		State     string `json:"state"`
		Reason    string `json:"reason,omitempty"`
	}
	entries := []entry{}
	for rows.Next() {
		var e entry
		var expression, zone string
		if err := rows.Scan(&e.ID, &e.TaskID, &e.Creator, &expression, &zone, &e.Content, &e.Due, &e.Expires, &e.Remaining, &e.State, &e.Reason); err != nil {
			api.failure(w, errors.Join(err, rows.Close()))
			return
		}
		if schedule {
			e.Cron, e.Timezone = expression, zone
		} else {
			e.Type, e.Scope = expression, zone
		}
		entries = append(entries, e)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		api.failure(w, err)
		return
	}
	next := ""
	if len(entries) > 20 {
		entries = entries[:20]
		next = entries[19].ID
	}
	api.respond(w, 200, map[string]any{table: entries, "next": next})
}
