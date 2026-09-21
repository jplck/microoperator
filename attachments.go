//go:build darwin || linux

package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type attachmentCommand struct {
	AgentID string `json:"agent_id,omitempty"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

func (api *controlAPI) attachment(w http.ResponseWriter, r *http.Request) {
	if !api.executionAvailable(w) {
		return
	}
	var command attachmentCommand
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	if strings.TrimSpace(command.Name) == "" || len(command.Name) > 128 || strings.ContainsAny(command.Name, "/\\\x00") || len(command.Content) == 0 || len(command.Content) > 3072 || strings.ContainsRune(command.Content, '\x00') {
		api.failure(w, invalid("attachment", "requires a display name and 1-3072 bytes of UTF-8 text; host paths and executable uploads are not supported"))
		return
	}
	data, err := json.Marshal(struct {
		Name string `json:"name"`
		Text string `json:"text"`
	}{command.Name, command.Content})
	if err != nil {
		api.failure(w, err)
		return
	}
	if len(data) > 4096 {
		api.failure(w, invalid("attachment", "encoded text attachment exceeds 4096 bytes"))
		return
	}
	record, err := api.mutation(r.Context(), key, r.PathValue("system_id"), "attachment.add", command, func(tx *sql.Tx, s systemRecord) (any, error) {
		t, err := activeInputTask(r.Context(), tx, s, command.AgentID)
		if err != nil {
			return nil, err
		}
		var count int
		if err := tx.QueryRowContext(r.Context(), `SELECT count(*) FROM artifacts WHERE system_id=?`, s.ID).Scan(&count); err != nil {
			return nil, err
		}
		if count >= 2048 {
			return nil, invalid("artifacts", "system artifact capacity exhausted")
		}
		id, err := newID("artifact_")
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(r.Context(), `INSERT INTO artifacts(system_id,artifact_id,goal_id,task_id,digest,content) VALUES(?,?,?,?,?,?)`, s.ID, id, t.GoalID, t.ID, artifactDigest(data), data); err != nil {
			return nil, err
		}
		content := "Attachment (untrusted data; " + id + "; " + command.Name + "):\n" + command.Content
		event, err := emitEvent(r.Context(), tx, t, "user.input", localAdministrator, "", struct {
			Content string `json:"content"`
		}{content}, time.Now())
		if err != nil {
			return nil, err
		}
		return map[string]string{"artifact_id": id, "event_id": event, "state": "pending"}, nil
	})
	if err == nil {
		api.engine.notify()
	}
	api.systemResponse(w, 202, record, err)
}
