package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

type ListAgentsResult struct {
	Agents []AgentRecord `json:"agents"`
	Next   string        `json:"next,omitempty"`
}

func (store *Store) ListAgents(ctx context.Context, systemID string, after string) (result ListAgentsResult, err error) {
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer rollback(tx, &err)
	id := systemID
	if _, err := readSystem(ctx, tx, localAdministrator, id); err != nil {
		return result, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT agent_id FROM agents WHERE system_id=? AND agent_id>? ORDER BY agent_id LIMIT 21`, id, after)
	if err != nil {
		return result, err
	}
	ids, err := readIDs(rows)
	if err != nil {
		return result, err
	}
	next := ""
	if len(ids) > 20 {
		ids = ids[:20]
		next = ids[19]
	}
	agents := make([]AgentRecord, 0, len(ids))
	for _, agentID := range ids {
		a, err := readAgent(ctx, tx, id, agentID, 0)
		if err != nil {
			return result, err
		}
		agents = append(agents, a)
	}
	return ListAgentsResult{agents, next}, nil
}

type ListTasksResult struct {
	Tasks []TaskRecord `json:"tasks"`
	Next  string       `json:"next,omitempty"`
}

func (store *Store) ListTasks(ctx context.Context, systemID string, after string) (result ListTasksResult, err error) {
	id := systemID
	if _, err := store.GetSystem(ctx, localAdministrator, id); err != nil {
		return result, err
	}
	rows, err := store.db.QueryContext(ctx, `SELECT task_id FROM tasks WHERE system_id=? AND task_id>? ORDER BY task_id LIMIT 21`, id, after)
	if err != nil {
		return result, err
	}
	ids, err := readIDs(rows)
	if err != nil {
		return result, err
	}
	next := ""
	if len(ids) > 20 {
		ids = ids[:20]
		next = ids[19]
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer rollback(tx, &err)
	tasks := make([]TaskRecord, 0, len(ids))
	for _, taskID := range ids {
		t, err := readTask(ctx, tx, id, taskID)
		if err != nil {
			return result, err
		}
		tasks = append(tasks, t)
	}
	return ListTasksResult{tasks, next}, nil
}

type ListEventsResult struct {
	Events []EventView `json:"events"`
	Next   int64       `json:"next,omitempty"`
}

func (store *Store) ListEvents(ctx context.Context, systemID string, after int64) (result ListEventsResult, err error) {
	id := systemID
	if _, err := store.GetSystem(ctx, localAdministrator, id); err != nil {
		return result, err
	}
	rows, err := store.db.QueryContext(ctx, `SELECT e.sequence,e.event_id,e.system_id,e.type,e.version,e.source,e.recipient,e.goal_id,e.task_id,
	 e.correlation_id,e.causation_id,e.created_at,e.expires_at,e.classification,e.authorization_ref,e.depth,e.payload,m.state,m.attempts,m.lease_until,m.reason
	 FROM events e JOIN mailboxes m ON e.system_id=m.system_id AND e.event_id=m.event_id WHERE e.system_id=? AND e.sequence>? ORDER BY e.sequence LIMIT 21`, id, after)
	if err != nil {
		return result, err
	}
	events := []EventView{}
	for rows.Next() {
		var e EventView
		if err := rows.Scan(&e.Sequence, &e.ID, &e.SystemID, &e.Type, &e.Version, &e.Source, &e.Recipient, &e.GoalID, &e.TaskID, &e.Correlation, &e.Causation, &e.CreatedAt, &e.ExpiresAt, &e.Classification, &e.Authorization, &e.Depth, &e.Payload, &e.State, &e.Attempts, &e.LeaseUntil, &e.Reason); err != nil {
			rows.Close()
			return result, err
		}
		events = append(events, e)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return result, err
	}
	next := int64(0)
	if len(events) > 20 {
		events = events[:20]
		next = events[19].Sequence
	}
	return ListEventsResult{events, next}, nil
}

type ListArtifactsResult struct {
	Artifacts []ListArtifactsArtifactEntry `json:"artifacts"`
	Next      string                       `json:"next,omitempty"`
}
type ListArtifactsArtifactEntry struct {
	ID     string `json:"artifact_id"`
	GoalID string `json:"goal_id"`
	TaskID string `json:"task_id"`
	Digest string `json:"digest"`
	Bytes  int    `json:"bytes"`
}

func (store *Store) ListArtifacts(ctx context.Context, systemID string, after string) (result ListArtifactsResult, err error) {
	id := systemID
	if _, err := store.GetSystem(ctx, localAdministrator, id); err != nil {
		return result, err
	}
	rows, err := store.db.QueryContext(ctx, `SELECT artifact_id,goal_id,task_id,digest,length(content) FROM artifacts WHERE system_id=? AND artifact_id>? ORDER BY artifact_id LIMIT 21`, id, after)
	if err != nil {
		return result, err
	}
	entries := []ListArtifactsArtifactEntry{}
	for rows.Next() {
		var entry ListArtifactsArtifactEntry
		if err := rows.Scan(&entry.ID, &entry.GoalID, &entry.TaskID, &entry.Digest, &entry.Bytes); err != nil {
			return result, errors.Join(err, rows.Close())
		}
		entries = append(entries, entry)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return result, err
	}
	next := ""
	if len(entries) > 20 {
		entries = entries[:20]
		next = entries[19].ID
	}
	return ListArtifactsResult{entries, next}, nil
}

type ListToolCallsResult struct {
	Calls []ListToolCallsReceipt `json:"calls"`
	Next  string                 `json:"next,omitempty"`
}
type ListToolCallsReceipt struct {
	CallID  string `json:"call_id"`
	ID      string `json:"tool_call_id"`
	TaskID  string `json:"task_id"`
	Name    string `json:"name"`
	Version int64  `json:"version"`
	State   string `json:"state"`
	Result  string `json:"result"`
}

func (store *Store) ListToolCalls(ctx context.Context, systemID string, after string) (result ListToolCallsResult, err error) {
	id := systemID
	if _, err := store.GetSystem(ctx, localAdministrator, id); err != nil {
		return result, err
	}
	rows, err := store.db.QueryContext(ctx, `SELECT call_id,tool_call_id,task_id,name,version,state,result FROM tool_calls WHERE system_id=? AND call_id>? ORDER BY call_id LIMIT 21`, id, after)
	if err != nil {
		return result, err
	}
	entries := []ListToolCallsReceipt{}
	for rows.Next() {
		var entry ListToolCallsReceipt
		if err := rows.Scan(&entry.CallID, &entry.ID, &entry.TaskID, &entry.Name, &entry.Version, &entry.State, &entry.Result); err != nil {
			return result, errors.Join(err, rows.Close())
		}
		entries = append(entries, entry)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return result, err
	}
	next := ""
	if len(entries) > 20 {
		entries = entries[:20]
		next = entries[19].CallID
	}
	return ListToolCallsResult{entries, next}, nil
}

type ArtifactResult struct {
	ID      string          `json:"artifact_id"`
	TaskID  string          `json:"task_id"`
	Digest  string          `json:"digest"`
	Content json.RawMessage `json:"content"`
}

func (store *Store) Artifact(ctx context.Context, systemID string, artifactID string) (result ArtifactResult, err error) {
	id := systemID
	if _, err := store.GetSystem(ctx, localAdministrator, id); err != nil {
		return result, err
	}
	var content []byte
	var digest, taskID string
	err = store.db.QueryRowContext(ctx, `SELECT content,digest,task_id FROM artifacts WHERE system_id=? AND artifact_id=?`, id, artifactID).Scan(&content, &digest, &taskID)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrSystemNotFound
	}
	if err != nil {
		return result, err
	}
	return ArtifactResult{artifactID, taskID, digest, content}, nil
}

type ListTriggersResult map[string]any
type ListTriggersEntry struct {
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

func (store *Store) ListTriggers(ctx context.Context, systemID string, after string, schedule bool) (result ListTriggersResult, err error) {
	id := systemID
	if _, err := store.GetSystem(ctx, localAdministrator, id); err != nil {
		return result, err
	}
	table, column := "subscriptions", "subscription_id"
	fields := `type,scope,'',0`
	if schedule {
		table, column, fields = "schedules", "schedule_id", `expression,timezone,content,next_due`
	}
	rows, err := store.db.QueryContext(ctx, `SELECT `+column+`,task_id,creator,`+fields+`,expires_at,remaining,state,reason FROM `+table+` WHERE system_id=? AND `+column+`>? ORDER BY `+column+` LIMIT 21`, id, after)
	if err != nil {
		return result, err
	}
	entries := []ListTriggersEntry{}
	for rows.Next() {
		var e ListTriggersEntry
		var expression, zone string
		if err := rows.Scan(&e.ID, &e.TaskID, &e.Creator, &expression, &zone, &e.Content, &e.Due, &e.Expires, &e.Remaining, &e.State, &e.Reason); err != nil {
			return result, errors.Join(err, rows.Close())
		}
		if schedule {
			e.Cron, e.Timezone = expression, zone
		} else {
			e.Type, e.Scope = expression, zone
		}
		entries = append(entries, e)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return result, err
	}
	next := ""
	if len(entries) > 20 {
		entries = entries[:20]
		next = entries[19].ID
	}
	return ListTriggersResult{table: entries, "next": next}, nil
}

type LearningViewResult struct {
	Items []json.RawMessage `json:"items"`
	Next  string            `json:"next,omitempty"`
}

func (store *Store) LearningView(ctx context.Context, systemID string, after string, section string) (result LearningViewResult, err error) {
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer rollback(tx, &err)
	if _, err := readSystem(ctx, tx, localAdministrator, systemID); err != nil {
		return result, err
	}
	query := ""
	switch section {
	case "":
		query = `SELECT evaluation_id,json_object('evaluation_id',evaluation_id,'tool_id',tool_id,'version',version,'check_id',check_id,'origin_task',origin_task,'state',state,'reason',reason,'digest',digest,'artifact_digest',artifact_digest,'profile_digest',profile_digest,'toolchain_digest',toolchain_digest,'baseline',json(baseline),'evidence',json(evidence)) FROM learning_evaluations WHERE system_id=? AND evaluation_id>? ORDER BY evaluation_id LIMIT 21`
	case "checks":
		query = `SELECT check_id,json_object('check_id',check_id,'digest',digest,'cases',json(cases),'created_at',created_at) FROM learning_checks WHERE system_id=? AND check_id>? ORDER BY check_id LIMIT 21`
	case "feedback":
		query = `SELECT feedback_id,json_object('feedback_id',feedback_id,'task_id',task_id,'rating',rating,'content',content,'created_at',created_at) FROM learning_feedback WHERE system_id=? AND feedback_id>? ORDER BY feedback_id LIMIT 21`
	default:
		return result, ErrSystemNotFound
	}
	rows, err := tx.QueryContext(ctx, query, systemID, after)
	if err != nil {
		return result, err
	}
	items := []json.RawMessage{}
	ids := []string{}
	for rows.Next() {
		var id string
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			rows.Close()
			return result, err
		}
		ids = append(ids, id)
		items = append(items, json.RawMessage(data))
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return result, err
	}
	next := ""
	if len(items) > 20 {
		items = items[:20]
		next = ids[19]
	}
	return LearningViewResult{items, next}, nil
}
