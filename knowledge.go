//go:build darwin || linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

type memoryCommand struct {
	ID               string   `json:"memory_id,omitempty"`
	Revision         int64    `json:"expected_revision"`
	Scope            string   `json:"scope"`
	Owner            string   `json:"owner_id,omitempty"`
	Content          string   `json:"content"`
	Evidence         string   `json:"evidence"`
	Confidence       int      `json:"confidence"`
	Artifacts        []string `json:"artifacts"`
	RetentionSeconds int64    `json:"retention_seconds"`
}
type memoryQuery struct {
	Scope   string `json:"scope"`
	Owner   string `json:"owner_id,omitempty"`
	Query   string `json:"query"`
	After   string `json:"after,omitempty"`
	Pending bool   `json:"include_pending,omitempty"`
}
type memoryEntry struct {
	ID         string   `json:"memory_id"`
	Revision   int64    `json:"revision"`
	Scope      string   `json:"scope"`
	Owner      string   `json:"owner_id"`
	State      string   `json:"state"`
	Content    string   `json:"content"`
	Evidence   string   `json:"evidence"`
	Confidence int      `json:"confidence"`
	Artifacts  []string `json:"artifacts"`
	Author     string   `json:"author"`
	GoalID     string   `json:"goal_id"`
	TaskID     string   `json:"task_id"`
	Created    int64    `json:"created_at_ms"`
	Expires    int64    `json:"expires_at_ms"`
}

func memoryOwner(ctx context.Context, tx *sql.Tx, t taskRecord, scope, owner string, admin bool) (string, error) {
	if !admin && owner != "" {
		return "", invalid("owner_id", "identity is derived from the worker session")
	}
	switch scope {
	case "system":
		if owner != "" {
			return "", invalid("owner_id", "system memory has no private owner")
		}
		return "", nil
	case "agent":
		if owner == "" {
			owner = t.AgentID
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM agents WHERE system_id=? AND agent_id=?)+(SELECT count(*) FROM systems WHERE system_id=? AND operator_id=?)`, t.SystemID, owner, t.SystemID, owner).Scan(&count); err != nil {
			return "", err
		}
		if count == 0 {
			return "", errSystemNotFound
		}
	case "task":
		if owner == "" {
			owner = t.ID
		}
		if _, err := readTask(ctx, tx, t.SystemID, owner); errors.Is(err, sql.ErrNoRows) {
			return "", errSystemNotFound
		} else if err != nil {
			return "", err
		}
	default:
		return "", invalid("scope", "requires task, agent or system")
	}
	return owner, nil
}

func putMemory(ctx context.Context, tx *sql.Tx, t taskRecord, command memoryCommand, admin bool, now time.Time) (memoryEntry, error) {
	entry := memoryEntry{}
	owner, err := memoryOwner(ctx, tx, t, command.Scope, command.Owner, admin)
	if err != nil {
		return entry, err
	}
	if strings.TrimSpace(command.Content) == "" || len(command.Content) > 4096 || len(command.Evidence) > 1024 || command.Confidence < 0 || command.Confidence > 100 ||
		command.RetentionSeconds < 1 || command.RetentionSeconds > 365*86400 || len(command.Artifacts) > 8 {
		return entry, invalid("memory", "invalid content, provenance, confidence, retention or artifact bound")
	}
	encoded, err := json.Marshal(command)
	if err != nil {
		return entry, err
	}
	if len(encoded) > 6144 {
		return entry, invalid("memory", "encoded entry exceeds 6 KiB")
	}
	if err := uniqueNames(command.Artifacts, "artifacts"); err != nil {
		return entry, err
	}
	for _, id := range command.Artifacts {
		var artifactTask string
		err := tx.QueryRowContext(ctx, `SELECT task_id FROM artifacts WHERE system_id=? AND artifact_id=?`, t.SystemID, id).Scan(&artifactTask)
		if errors.Is(err, sql.ErrNoRows) || (!admin && artifactTask != t.ID) {
			return entry, errSystemNotFound
		}
		if err != nil {
			return entry, err
		}
	}
	id, revision := command.ID, int64(1)
	state, author := "active", t.AgentID
	if admin {
		author = localAdministrator
	} else if command.Scope == "system" {
		state = "pending"
	}
	if id == "" {
		if command.Revision != 0 {
			return entry, errRevisionConflict
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM memory_heads WHERE system_id=?`, t.SystemID).Scan(&count); err != nil {
			return entry, err
		}
		if count >= 512 {
			return entry, invalid("memory", "system memory-ID capacity exhausted")
		}
		id, err = newID("memory_")
		if err != nil {
			return entry, err
		}
	} else {
		// Shared changes by an agent are new proposals, never an overwrite that
		// could hide or replace a previously approved fact before human review.
		if !admin && command.Scope == "system" {
			return entry, invalid("memory", "submit a new shared-memory proposal for review")
		}
		var prior int64
		err := tx.QueryRowContext(ctx, `SELECT revision FROM memory_heads WHERE system_id=? AND memory_id=? AND scope=? AND owner_id=? AND state!='deleted' AND expires_at>?`, t.SystemID, id, command.Scope, owner, now.UnixMilli()).Scan(&prior)
		if errors.Is(err, sql.ErrNoRows) {
			return entry, errSystemNotFound
		}
		if err != nil {
			return entry, err
		}
		if prior != command.Revision {
			return entry, errRevisionConflict
		}
		revision = prior + 1
		if revision > 32 {
			return entry, invalid("memory", "revision capacity exhausted")
		}
	}
	expires := now.Add(time.Duration(command.RetentionSeconds) * time.Second).UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_heads(system_id,memory_id,scope,owner_id,revision,state,expires_at) VALUES(?,?,?,?,?,?,?)
	 ON CONFLICT(system_id,memory_id) DO UPDATE SET revision=excluded.revision,state=excluded.state,expires_at=excluded.expires_at`,
		t.SystemID, id, command.Scope, owner, revision, state, expires); err != nil {
		return entry, err
	}
	artifacts, err := json.Marshal(append([]string{}, command.Artifacts...))
	if err != nil {
		return entry, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_revisions(system_id,memory_id,revision,content,evidence,confidence,artifacts,author,goal_id,task_id,created_at)
	 VALUES(?,?,?,?,?,?,?,?,?,?,?)`, t.SystemID, id, revision, command.Content, command.Evidence, command.Confidence, artifacts, author, t.GoalID, t.ID, now.UnixMilli()); err != nil {
		return entry, err
	}
	entry = memoryEntry{ID: id, Revision: revision, Scope: command.Scope, Owner: owner, State: state, Content: command.Content, Evidence: command.Evidence,
		Confidence: command.Confidence, Artifacts: append([]string{}, command.Artifacts...), Author: author, GoalID: t.GoalID, TaskID: t.ID, Created: now.UnixMilli(), Expires: expires}
	return entry, nil
}

func searchMemory(ctx context.Context, tx *sql.Tx, t taskRecord, query memoryQuery, admin bool, now time.Time) (any, error) {
	owner, err := memoryOwner(ctx, tx, t, query.Scope, query.Owner, admin)
	if err != nil {
		return nil, err
	}
	if len(query.Query) > 256 || len(query.After) > 64 || (!admin && query.Pending) {
		return nil, invalid("memory", "query exceeds scope or size limits")
	}
	// Authorization is in the SQL predicate, before text matching or pagination.
	rows, err := tx.QueryContext(ctx, `SELECT h.memory_id,h.revision,h.scope,h.owner_id,h.state,r.content,r.evidence,r.confidence,r.artifacts,r.author,r.goal_id,r.task_id,r.created_at,h.expires_at
	 FROM memory_heads h JOIN memory_revisions r ON r.system_id=h.system_id AND r.memory_id=h.memory_id AND r.revision=h.revision
	 WHERE h.system_id=? AND h.scope=? AND h.owner_id=? AND (h.state='active' OR (? AND h.state='pending')) AND h.expires_at>?
	 AND h.memory_id>? AND instr(lower(r.content),lower(?))>0 ORDER BY h.memory_id LIMIT 11`,
		t.SystemID, query.Scope, owner, admin && query.Pending, now.UnixMilli(), query.After, query.Query)
	if err != nil {
		return nil, err
	}
	entries := []memoryEntry{}
	next := ""
	size := 64
	for rows.Next() {
		var e memoryEntry
		var refs []byte
		if err := rows.Scan(&e.ID, &e.Revision, &e.Scope, &e.Owner, &e.State, &e.Content, &e.Evidence, &e.Confidence, &refs, &e.Author, &e.GoalID, &e.TaskID, &e.Created, &e.Expires); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		if err := json.Unmarshal(refs, &e.Artifacts); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		data, err := json.Marshal(e)
		if err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		if len(entries) == 10 || size+len(data) > maxEventBytes-128 {
			if len(entries) == 0 {
				return nil, errors.Join(invalid("memory", "stored entry exceeds retrieval limit"), rows.Close())
			}
			next = entries[len(entries)-1].ID
			break
		}
		size += len(data) + 1
		entries = append(entries, e)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return struct {
		Untrusted bool          `json:"untrusted_data"`
		Entries   []memoryEntry `json:"entries"`
		Next      string        `json:"next,omitempty"`
	}{true, entries, next}, nil
}

type scheduleCommand struct {
	AgentID  string `json:"agent_id,omitempty"`
	At       string `json:"at,omitempty"`
	Cron     string `json:"cron,omitempty"`
	Timezone string `json:"timezone,omitempty"`
	Content  string `json:"content"`
	Budget   int    `json:"trigger_budget"`
}
type subscriptionCommand struct {
	AgentID string `json:"agent_id,omitempty"`
	Type    string `json:"type"`
	Scope   string `json:"scope"`
	Budget  int    `json:"trigger_budget"`
}

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

func nextCron(expression, zone string, after time.Time) (time.Time, error) {
	if len(expression) > 256 || len(strings.Fields(expression)) != 5 || zone == "Local" || len(zone) > 128 {
		return time.Time{}, invalid("cron", "requires five fields and an explicit IANA zone (default UTC)")
	}
	if zone == "" {
		zone = "UTC"
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return time.Time{}, invalid("timezone", "unknown IANA zone")
	}
	schedule, err := cronParser.Parse("CRON_TZ=" + zone + " " + expression)
	if err != nil {
		return time.Time{}, invalid("cron", "invalid expression")
	}
	next := schedule.Next(after)
	if next.IsZero() {
		return next, invalid("cron", "no occurrence within the parser's five-year horizon")
	}
	return next, nil
}

func createSchedule(ctx context.Context, tx *sql.Tx, t taskRecord, command scheduleCommand, creator string, now time.Time) (string, error) {
	if (command.At == "") == (command.Cron == "") || strings.TrimSpace(command.Content) == "" || len(command.Content) > 2048 || command.Budget < 1 || command.Budget > 8 {
		return "", invalid("schedule", "requires exactly one at/cron, bounded content and 1-8 triggers")
	}
	if command.Timezone == "" {
		command.Timezone = "UTC"
	}
	var next time.Time
	var err error
	if command.Cron != "" {
		next, err = nextCron(command.Cron, command.Timezone, now)
	} else {
		next, err = time.Parse(time.RFC3339Nano, command.At)
		if err != nil {
			return "", invalid("at", "requires an RFC3339 timestamp")
		}
		if command.Budget != 1 {
			return "", invalid("trigger_budget", "one-shot schedules require one trigger")
		}
	}
	if err != nil {
		return "", err
	}
	var deadline int64
	if err := tx.QueryRowContext(ctx, `SELECT deadline FROM goals WHERE system_id=? AND goal_id=?`, t.SystemID, t.GoalID).Scan(&deadline); err != nil {
		return "", err
	}
	if !next.After(now) || next.UnixMilli() >= deadline {
		return "", invalid("schedule", "occurrence must be future and within the owning goal's lifetime")
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM schedules WHERE system_id=?`, t.SystemID).Scan(&count); err != nil {
		return "", err
	}
	if count >= 128 {
		return "", invalid("schedule", "system schedule capacity exhausted")
	}
	id, err := newID("schedule_")
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO schedules(system_id,schedule_id,task_id,creator,expression,timezone,content,next_due,expires_at,remaining,state)
	 VALUES(?,?,?,?,?,?,?,?,?,?,'active')`, t.SystemID, id, t.ID, creator, command.Cron, command.Timezone, command.Content, next.UnixMilli(), deadline, command.Budget)
	return id, err
}

func createSubscription(ctx context.Context, tx *sql.Tx, t taskRecord, command subscriptionCommand, creator string, now time.Time) (string, error) {
	if (command.Type != "memory.changed" && command.Type != "task.completed") || command.Budget < 1 || command.Budget > 8 {
		return "", invalid("subscription", "unsupported type or trigger budget")
	}
	if command.Type == "memory.changed" {
		if _, err := memoryOwner(ctx, tx, t, command.Scope, "", false); err != nil {
			return "", err
		}
		if !hasTaskTool(t, "runtime.memory.search") {
			return "", invalid("subscription", "memory retrieval grant required")
		}
	} else if command.Scope != "" {
		return "", invalid("scope", "task completion subscriptions are same-goal only")
	}
	var count int
	var deadline int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM subscriptions WHERE system_id=?`, t.SystemID).Scan(&count); err != nil {
		return "", err
	}
	if count >= 128 {
		return "", invalid("subscription", "system subscription capacity exhausted")
	}
	if err := tx.QueryRowContext(ctx, `SELECT deadline FROM goals WHERE system_id=? AND goal_id=?`, t.SystemID, t.GoalID).Scan(&deadline); err != nil {
		return "", err
	}
	if deadline <= now.UnixMilli() {
		return "", invalid("subscription", "goal lifetime expired")
	}
	id, err := newID("subscription_")
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO subscriptions(system_id,subscription_id,task_id,creator,type,scope,expires_at,remaining,state) VALUES(?,?,?,?,?,?,?,?,'active')`,
		t.SystemID, id, t.ID, creator, command.Type, command.Scope, deadline, command.Budget)
	return id, err
}

func (engine *executionEngine) knowledgeTool(ctx context.Context, tx *sql.Tx, e executionRecord, t taskRecord, name, arguments string) (string, error) {
	now := time.Now()
	switch name {
	case "runtime.memory.put":
		var command memoryCommand
		if err := decodeJSON([]byte(arguments), &command); err != nil {
			return "", err
		}
		entry, err := putMemory(ctx, tx, t, command, false, now)
		if err != nil {
			return "", err
		}
		if entry.State == "active" {
			if err := engine.notifySubscriptions(ctx, tx, t, "memory.changed", entry.Scope, entry.Owner, e.EventID, entry.ID, now); err != nil {
				return "", err
			}
		}
		return toolOutcome(map[string]any{"memory_id": entry.ID, "revision": entry.Revision, "state": entry.State})
	case "runtime.memory.search":
		var query memoryQuery
		if err := decodeJSON([]byte(arguments), &query); err != nil {
			return "", err
		}
		result, err := searchMemory(ctx, tx, t, query, false, now)
		if err != nil {
			return "", err
		}
		return toolOutcome(result)
	case "runtime.schedule.create":
		var command scheduleCommand
		if err := decodeJSON([]byte(arguments), &command); err != nil {
			return "", err
		}
		if command.AgentID != "" {
			return "", invalid("agent_id", "schedules can only wake this task")
		}
		id, err := createSchedule(ctx, tx, t, command, t.AgentID, now)
		if err != nil {
			return "", err
		}
		return toolOutcome(map[string]string{"schedule_id": id, "state": "active"})
	case "runtime.events.subscribe":
		var command subscriptionCommand
		if err := decodeJSON([]byte(arguments), &command); err != nil {
			return "", err
		}
		if command.AgentID != "" {
			return "", invalid("agent_id", "subscriptions can only wake this task")
		}
		id, err := createSubscription(ctx, tx, t, command, t.AgentID, now)
		if err != nil {
			return "", err
		}
		return toolOutcome(map[string]string{"subscription_id": id, "state": "active"})
	case "runtime.schedule.cancel":
		var command struct {
			ID string `json:"schedule_id"`
		}
		if err := decodeJSON([]byte(arguments), &command); err != nil {
			return "", err
		}
		result, err := tx.ExecContext(ctx, `UPDATE schedules SET state='canceled',reason='canceled by owning task' WHERE system_id=? AND task_id=? AND schedule_id=?`, t.SystemID, t.ID, command.ID)
		if err != nil {
			return "", err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return "", err
		}
		if count != 1 {
			return "", errSystemNotFound
		}
		return `{"state":"canceled"}`, nil
	case "runtime.events.unsubscribe":
		var command struct {
			ID string `json:"subscription_id"`
		}
		if err := decodeJSON([]byte(arguments), &command); err != nil {
			return "", err
		}
		result, err := tx.ExecContext(ctx, `UPDATE subscriptions SET state='canceled',reason='canceled by owning task' WHERE system_id=? AND task_id=? AND subscription_id=?`, t.SystemID, t.ID, command.ID)
		if err != nil {
			return "", err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return "", err
		}
		if count != 1 {
			return "", errSystemNotFound
		}
		return `{"state":"canceled"}`, nil
	case "runtime.task.wait":
		var args struct {
			Reason string `json:"reason"`
		}
		if err := decodeJSON([]byte(arguments), &args); err != nil {
			return "", err
		}
		if strings.TrimSpace(args.Reason) == "" || len(args.Reason) > 1024 {
			return "", invalid("reason", "requires a bounded waiting reason")
		}
		result := `{"state":"waiting"}`
		appendToolResult(&t, e, result)
		if err := updateConversation(ctx, tx, t); err != nil {
			return "", err
		}
		_, err := tx.ExecContext(ctx, `UPDATE tasks SET state='waiting',waiting_tool='',reason=? WHERE system_id=? AND task_id=?`, args.Reason, t.SystemID, t.ID)
		return result, err
	}
	return "", fmt.Errorf("unsupported knowledge operation %s", name)
}
