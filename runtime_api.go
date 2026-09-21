//go:build darwin || linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (api *controlAPI) registerRuntimeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/tools", api.tools)
	mux.HandleFunc("GET /v1/tools/{tool_id}", api.tools)
	mux.HandleFunc("GET /v1/systems/{system_id}/tools", api.tools)
	mux.HandleFunc("GET /v1/systems/{system_id}/tools/{tool_id}", api.tools)
	mux.HandleFunc("POST /v1/systems/{system_id}/tools/drafts", api.draft)
	mux.HandleFunc("POST /v1/systems/{system_id}/tools/revoke", api.revoke)
	mux.HandleFunc("POST /v1/systems/{system_id}/tools/{tool_id}/state", api.draftState)
	mux.HandleFunc("GET /v1/systems/{system_id}/agents", api.directory)
	mux.HandleFunc("PUT /v1/systems/{system_id}/agents/{agent_id}/tools", api.assignTools)
	mux.HandleFunc("GET /v1/systems/{system_id}/tasks", api.tasks)
	mux.HandleFunc("GET /v1/systems/{system_id}/events", api.events)
	mux.HandleFunc("GET /v1/systems/{system_id}/artifacts/{artifact_id}", api.artifact)
	mux.HandleFunc("GET /v1/systems/{system_id}/artifacts", api.artifacts)
	mux.HandleFunc("GET /v1/systems/{system_id}/tool-calls", api.toolCalls)
	mux.HandleFunc("POST /v1/systems/{system_id}/input", api.input)
	mux.HandleFunc("POST /v1/systems/{system_id}/{action}", api.control)
	mux.HandleFunc("POST /v1/systems/{system_id}/agents/{agent_id}/{action}", api.control)
	mux.HandleFunc("POST /v1/systems/{system_id}/goals/{goal_id}/{action}", api.control)
}

func (api *controlAPI) tools(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("system_id")
	entries, err := api.store.catalog(r.Context(), api.cfg, localAdministrator, id)
	if err != nil {
		api.failure(w, err)
		return
	}
	if toolID := r.PathValue("tool_id"); toolID != "" {
		for _, entry := range entries {
			if entry.ID == toolID {
				api.respond(w, 200, entry)
				return
			}
		}
		api.failure(w, errSystemNotFound)
		return
	}
	api.respond(w, 200, struct {
		Tools []registryEntry `json:"tools"`
	}{entries})
}

func (api *controlAPI) mutation(ctx context.Context, key, systemID, operation string, body any,
	mutate func(*sql.Tx, systemRecord) (any, error)) (systemRecord, error) {
	hash, _, err := jsonDigest(struct {
		Operation, SystemID string
		Body                any
	}{operation, systemID, body})
	if err != nil {
		return systemRecord{}, err
	}
	return api.store.command(ctx, localAdministrator, key, hash, func(tx *sql.Tx) (systemRecord, error) {
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

func (api *controlAPI) draft(w http.ResponseWriter, r *http.Request) {
	var command draftCommand
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	record, err := api.mutation(r.Context(), key, r.PathValue("system_id"), "tool.propose", command, func(tx *sql.Tx, s systemRecord) (any, error) {
		id, err := submitDraft(r.Context(), tx, api.cfg, s.ID, localAdministrator, "", "", command)
		return map[string]string{"tool_id": id, "state": "draft"}, err
	})
	api.systemResponse(w, 201, record, err)
}

func (api *controlAPI) draftState(w http.ResponseWriter, r *http.Request) {
	var command struct {
		Version int64  `json:"version"`
		State   string `json:"state"`
	}
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	if command.Version < 1 || (command.State != "rejected" && command.State != "disabled") {
		api.failure(w, invalid("state", "drafts may only be rejected or disabled; evaluation/promotion is not enabled"))
		return
	}
	id := r.PathValue("tool_id")
	record, err := api.mutation(r.Context(), key, r.PathValue("system_id"), "tool.draft-state", struct {
		ID      string
		Command any
	}{id, command}, func(tx *sql.Tx, s systemRecord) (any, error) {
		result, err := tx.ExecContext(r.Context(), `UPDATE tool_drafts SET state=? WHERE system_id=? AND tool_id=? AND version=?`, command.State, s.ID, id, command.Version)
		if err != nil {
			return nil, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n != 1 {
			return nil, errSystemNotFound
		}
		return map[string]string{"tool_id": id, "state": command.State}, nil
	})
	api.systemResponse(w, 200, record, err)
}

func (api *controlAPI) revoke(w http.ResponseWriter, r *http.Request) {
	var command struct {
		Name    string `json:"name"`
		Version int64  `json:"version"`
	}
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	tool, ok := api.cfg.tool(command.Name)
	if !ok || command.Version != tool.Version {
		api.failure(w, invalid("tool", "unknown or stale shared version"))
		return
	}
	if api.engine != nil {
		api.engine.mu.Lock()
		defer api.engine.mu.Unlock()
	}
	record, err := api.mutation(r.Context(), key, r.PathValue("system_id"), "tool.revoke", command, func(tx *sql.Tx, s systemRecord) (any, error) {
		_, err := tx.ExecContext(r.Context(), `INSERT OR IGNORE INTO tool_revocations(system_id,name,version) VALUES(?,?,?)`, s.ID, command.Name, command.Version)
		return command, err
	})
	if err == nil && api.engine != nil {
		for _, active := range api.engine.active {
			if active.systemID != record.ID {
				continue
			}
			task, taskErr := api.engine.taskSnapshot(r.Context(), record.ID, active.taskID)
			if taskErr != nil {
				api.logger.Printf("revocation task lookup: %v", taskErr)
				active.cancel()
				continue
			}
			for _, pin := range task.Tools {
				if pin.Name == command.Name && pin.Version == command.Version {
					active.cancel()
				}
			}
		}
		api.engine.notify()
	}
	api.systemResponse(w, 200, record, err)
}

func runtimeCursor(r *http.Request) (string, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) > 1 || (len(query) == 1 && len(query["after"]) != 1) {
		return "", invalid("after", "invalid cursor")
	}
	return query.Get("after"), nil
}

func (api *controlAPI) directory(w http.ResponseWriter, r *http.Request) {
	after, err := runtimeCursor(r)
	if err != nil {
		api.failure(w, err)
		return
	}
	tx, err := api.store.db.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		api.failure(w, err)
		return
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			api.logger.Printf("directory rollback: %v", rollbackErr)
		}
	}()
	id := r.PathValue("system_id")
	if _, err := readSystem(r.Context(), tx, localAdministrator, id); err != nil {
		api.failure(w, err)
		return
	}
	rows, err := tx.QueryContext(r.Context(), `SELECT agent_id FROM agents WHERE system_id=? AND agent_id>? ORDER BY agent_id LIMIT 21`, id, after)
	if err != nil {
		api.failure(w, err)
		return
	}
	ids, err := readIDs(rows)
	if err != nil {
		api.failure(w, err)
		return
	}
	next := ""
	if len(ids) > 20 {
		ids = ids[:20]
		next = ids[19]
	}
	agents := make([]agentRecord, 0, len(ids))
	for _, agentID := range ids {
		a, err := readAgent(r.Context(), tx, id, agentID, 0)
		if err != nil {
			api.failure(w, err)
			return
		}
		agents = append(agents, a)
	}
	api.respond(w, 200, struct {
		Agents []agentRecord `json:"agents"`
		Next   string        `json:"next,omitempty"`
	}{agents, next})
}

func readIDs(rows *sql.Rows) (ids []string, err error) {
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
	}
	return ids, errors.Join(rows.Err(), rows.Close())
}

func (api *controlAPI) tasks(w http.ResponseWriter, r *http.Request) {
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
	rows, err := api.store.db.QueryContext(r.Context(), `SELECT task_id FROM tasks WHERE system_id=? AND task_id>? ORDER BY task_id LIMIT 21`, id, after)
	if err != nil {
		api.failure(w, err)
		return
	}
	ids, err := readIDs(rows)
	if err != nil {
		api.failure(w, err)
		return
	}
	next := ""
	if len(ids) > 20 {
		ids = ids[:20]
		next = ids[19]
	}
	tx, err := api.store.db.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		api.failure(w, err)
		return
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			api.logger.Printf("tasks rollback: %v", err)
		}
	}()
	tasks := make([]taskRecord, 0, len(ids))
	for _, taskID := range ids {
		t, err := readTask(r.Context(), tx, id, taskID)
		if err != nil {
			api.failure(w, err)
			return
		}
		tasks = append(tasks, t)
	}
	api.respond(w, 200, struct {
		Tasks []taskRecord `json:"tasks"`
		Next  string       `json:"next,omitempty"`
	}{tasks, next})
}

type eventView struct {
	Sequence       int64           `json:"sequence"`
	ID             string          `json:"event_id"`
	SystemID       string          `json:"system_id"`
	Type           string          `json:"type"`
	Version        int             `json:"version"`
	Source         string          `json:"source"`
	Recipient      string          `json:"recipient"`
	GoalID         string          `json:"goal_id"`
	TaskID         string          `json:"task_id"`
	Correlation    string          `json:"correlation_id"`
	Causation      string          `json:"causation_id"`
	CreatedAt      int64           `json:"created_at_ms"`
	ExpiresAt      int64           `json:"expires_at_ms"`
	Classification string          `json:"classification"`
	Authorization  string          `json:"authorization_ref"`
	Depth          int             `json:"depth"`
	Payload        json.RawMessage `json:"payload"`
	State          string          `json:"delivery_state"`
	Attempts       int             `json:"attempts"`
	LeaseUntil     int64           `json:"lease_until_ms"`
	Reason         string          `json:"reason,omitempty"`
}

func (api *controlAPI) events(w http.ResponseWriter, r *http.Request) {
	cursor, err := runtimeCursor(r)
	if err != nil {
		api.failure(w, err)
		return
	}
	after := int64(0)
	if cursor != "" {
		after, err = strconv.ParseInt(cursor, 10, 64)
	}
	if err != nil || after < 0 {
		api.failure(w, invalid("after", "requires a nonnegative event sequence"))
		return
	}
	id := r.PathValue("system_id")
	if _, err := api.store.getSystem(r.Context(), localAdministrator, id); err != nil {
		api.failure(w, err)
		return
	}
	rows, err := api.store.db.QueryContext(r.Context(), `SELECT e.sequence,e.event_id,e.system_id,e.type,e.version,e.source,e.recipient,e.goal_id,e.task_id,
	 e.correlation_id,e.causation_id,e.created_at,e.expires_at,e.classification,e.authorization_ref,e.depth,e.payload,m.state,m.attempts,m.lease_until,m.reason
	 FROM events e JOIN mailboxes m ON e.system_id=m.system_id AND e.event_id=m.event_id WHERE e.system_id=? AND e.sequence>? ORDER BY e.sequence LIMIT 21`, id, after)
	if err != nil {
		api.failure(w, err)
		return
	}
	events := []eventView{}
	for rows.Next() {
		var e eventView
		if err := rows.Scan(&e.Sequence, &e.ID, &e.SystemID, &e.Type, &e.Version, &e.Source, &e.Recipient, &e.GoalID, &e.TaskID, &e.Correlation, &e.Causation,
			&e.CreatedAt, &e.ExpiresAt, &e.Classification, &e.Authorization, &e.Depth, &e.Payload, &e.State, &e.Attempts, &e.LeaseUntil, &e.Reason); err != nil {
			rows.Close()
			api.failure(w, err)
			return
		}
		events = append(events, e)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		api.failure(w, err)
		return
	}
	next := int64(0)
	if len(events) > 20 {
		events = events[:20]
		next = events[19].Sequence
	}
	api.respond(w, 200, struct {
		Events []eventView `json:"events"`
		Next   int64       `json:"next,omitempty"`
	}{events, next})
}

func (api *controlAPI) assignTools(w http.ResponseWriter, r *http.Request) {
	var command struct {
		ExpectedRevision int64     `json:"expected_revision"`
		Tools            []toolPin `json:"tools"`
	}
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	agentID := r.PathValue("agent_id")
	record, err := api.mutation(r.Context(), key, r.PathValue("system_id"), "agent.assign-tools", struct {
		AgentID string
		Command any
	}{agentID, command}, func(tx *sql.Tx, s systemRecord) (any, error) {
		if agentID == s.OperatorID {
			return nil, invalid("agent", "revise the stopped system configuration to assign operator tools")
		}
		a, err := readAgent(r.Context(), tx, s.ID, agentID, 0)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errSystemNotFound
		}
		if err != nil {
			return nil, err
		}
		if a.State == "stopped" {
			return nil, errExecutionConflict
		}
		if command.ExpectedRevision != a.Revision {
			return nil, errRevisionConflict
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
				return nil, invalid("tools", "assignment is not an exact system-granted pin")
			}
			names = append(names, pin.Name)
		}
		if err := uniqueNames(names, "tools"); err != nil {
			return nil, err
		}
		if err := authorizePins(r.Context(), tx, api.cfg, s.ID, command.Tools); err != nil {
			return nil, err
		}
		a.Revision++
		a.Tools = append([]toolPin{}, command.Tools...)
		a.Definition.Tools = names
		if err := insertAgentRevision(r.Context(), tx, a); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(r.Context(), `UPDATE agents SET revision=? WHERE system_id=? AND agent_id=?`, a.Revision, s.ID, a.ID); err != nil {
			return nil, err
		}
		return a, nil
	})
	api.systemResponse(w, 200, record, err)
}

func (api *controlAPI) artifacts(w http.ResponseWriter, r *http.Request) {
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
	rows, err := api.store.db.QueryContext(r.Context(), `SELECT artifact_id,goal_id,task_id,digest,length(content) FROM artifacts WHERE system_id=? AND artifact_id>? ORDER BY artifact_id LIMIT 21`, id, after)
	if err != nil {
		api.failure(w, err)
		return
	}
	type artifactEntry struct {
		ID     string `json:"artifact_id"`
		GoalID string `json:"goal_id"`
		TaskID string `json:"task_id"`
		Digest string `json:"digest"`
		Bytes  int    `json:"bytes"`
	}
	entries := []artifactEntry{}
	for rows.Next() {
		var entry artifactEntry
		if err := rows.Scan(&entry.ID, &entry.GoalID, &entry.TaskID, &entry.Digest, &entry.Bytes); err != nil {
			api.failure(w, errors.Join(err, rows.Close()))
			return
		}
		entries = append(entries, entry)
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
	api.respond(w, 200, struct {
		Artifacts []artifactEntry `json:"artifacts"`
		Next      string          `json:"next,omitempty"`
	}{entries, next})
}

func (api *controlAPI) toolCalls(w http.ResponseWriter, r *http.Request) {
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
	// One action per model call makes call_id a stable cursor and receipt key.
	rows, err := api.store.db.QueryContext(r.Context(), `SELECT call_id,tool_call_id,task_id,name,version,state,result FROM tool_calls WHERE system_id=? AND call_id>? ORDER BY call_id LIMIT 21`, id, after)
	if err != nil {
		api.failure(w, err)
		return
	}
	type receipt struct {
		CallID  string `json:"call_id"`
		ID      string `json:"tool_call_id"`
		TaskID  string `json:"task_id"`
		Name    string `json:"name"`
		Version int64  `json:"version"`
		State   string `json:"state"`
		Result  string `json:"result"`
	}
	entries := []receipt{}
	for rows.Next() {
		var entry receipt
		if err := rows.Scan(&entry.CallID, &entry.ID, &entry.TaskID, &entry.Name, &entry.Version, &entry.State, &entry.Result); err != nil {
			api.failure(w, errors.Join(err, rows.Close()))
			return
		}
		entries = append(entries, entry)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		api.failure(w, err)
		return
	}
	next := ""
	if len(entries) > 20 {
		entries = entries[:20]
		next = entries[19].CallID
	}
	api.respond(w, 200, struct {
		Calls []receipt `json:"calls"`
		Next  string    `json:"next,omitempty"`
	}{entries, next})
}

func (api *controlAPI) artifact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("system_id")
	if _, err := api.store.getSystem(r.Context(), localAdministrator, id); err != nil {
		api.failure(w, err)
		return
	}
	var content []byte
	var digest, taskID string
	err := api.store.db.QueryRowContext(r.Context(), `SELECT content,digest,task_id FROM artifacts WHERE system_id=? AND artifact_id=?`, id, r.PathValue("artifact_id")).Scan(&content, &digest, &taskID)
	if errors.Is(err, sql.ErrNoRows) {
		err = errSystemNotFound
	}
	if err != nil {
		api.failure(w, err)
		return
	}
	api.respond(w, 200, struct {
		ID      string          `json:"artifact_id"`
		TaskID  string          `json:"task_id"`
		Digest  string          `json:"digest"`
		Content json.RawMessage `json:"content"`
	}{r.PathValue("artifact_id"), taskID, digest, content})
}

func (api *controlAPI) input(w http.ResponseWriter, r *http.Request) {
	if !api.executionAvailable(w) {
		return
	}
	var command struct {
		AgentID string `json:"agent_id,omitempty"`
		Content string `json:"content"`
	}
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	if strings.TrimSpace(command.Content) == "" || len(command.Content) > 4096 {
		api.failure(w, invalid("content", "requires 1-4096 bytes"))
		return
	}
	record, err := api.mutation(r.Context(), key, r.PathValue("system_id"), "user.input", command, func(tx *sql.Tx, s systemRecord) (any, error) {
		if s.State != "running" && s.State != "paused" {
			return nil, errExecutionConflict
		}
		agentID := command.AgentID
		if agentID == "" {
			agentID = s.OperatorID
		}
		var taskID string
		err := tx.QueryRowContext(r.Context(), `SELECT task_id FROM tasks WHERE system_id=? AND agent_id=? AND state IN ('queued','running','waiting')`, s.ID, agentID).Scan(&taskID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errSystemNotFound
		}
		if err != nil {
			return nil, err
		}
		t, err := readTask(r.Context(), tx, s.ID, taskID)
		if err != nil {
			return nil, err
		}
		var control string
		if err := tx.QueryRowContext(r.Context(), `SELECT control FROM goals WHERE system_id=? AND goal_id=?`, s.ID, t.GoalID).Scan(&control); err != nil {
			return nil, err
		}
		if control == "stopped" || t.Control == "stopped" {
			return nil, errExecutionConflict
		}
		id, err := emitEvent(r.Context(), tx, t, "user.input", localAdministrator, "", struct {
			Content string `json:"content"`
		}{command.Content}, time.Now())
		return map[string]string{"event_id": id, "state": "pending"}, err
	})
	if err == nil {
		api.engine.notify()
	}
	api.systemResponse(w, 202, record, err)
}
