package state

import (
	"context"
	"database/sql"
	"errors"
)

type ActivityAgent struct {
	ID         string `json:"agent_id"`
	Parent     string `json:"parent_id,omitempty"`
	Name       string `json:"name"`
	State      string `json:"state"`
	Depth      int    `json:"depth"`
	Model      string `json:"model"`
	TaskID     string `json:"task_id,omitempty"`
	TaskState  string `json:"task_state,omitempty"`
	Control    string `json:"control,omitempty"`
	Assignment string `json:"assignment,omitempty"`
	Turns      int    `json:"turns"`
	Reason     string `json:"reason,omitempty"`
	Response   string `json:"response,omitempty"`
}

type ActivityCall struct {
	ID         string `json:"call_id"`
	AgentID    string `json:"agent_id"`
	TaskID     string `json:"task_id"`
	Model      string `json:"model"`
	State      string `json:"state"`
	Tool       string `json:"tool,omitempty"`
	ToolState  string `json:"tool_state,omitempty"`
	Input      int64  `json:"input_tokens"`
	Output     int64  `json:"output_tokens"`
	UsageKnown bool   `json:"usage_known"`
	CreatedAt  int64  `json:"created_at_ms"`
}

type ActivityEvent struct {
	ID        string `json:"event_id"`
	Type      string `json:"type"`
	Source    string `json:"source"`
	Recipient string `json:"recipient"`
	TaskID    string `json:"task_id"`
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
	CreatedAt int64  `json:"created_at_ms"`
}

type ActivitySnapshot struct {
	System    SystemRecord    `json:"system"`
	Agents    []ActivityAgent `json:"agents"`
	Calls     []ActivityCall  `json:"calls"`
	Events    []ActivityEvent `json:"events"`
	Truncated bool            `json:"agents_truncated"`
}

func (store *Store) Activity(ctx context.Context, principal, systemID string) (result ActivitySnapshot, err error) {
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer rollback(tx, &err)
	result.System, err = readSystem(ctx, tx, principal, systemID)
	if err != nil {
		return result, err
	}
	goalID := ""
	if result.System.Execution != nil {
		goalID = result.System.Execution.GoalID
	} else if result.System.Goal != nil {
		goalID = result.System.Goal.ID
	}
	rows, err := tx.QueryContext(ctx, `SELECT task_id FROM tasks WHERE system_id=? AND goal_id=?
	 ORDER BY state IN ('completed','failed','canceled','rejected'),rowid DESC LIMIT 64`, systemID, goalID)
	if err != nil {
		return result, err
	}
	ids, err := readIDs(rows)
	if err != nil {
		return result, err
	}
	tasks, current := map[string]TaskRecord{}, map[string]TaskRecord{}
	for _, id := range ids {
		task, err := readTask(ctx, tx, systemID, id)
		if err != nil {
			return result, err
		}
		tasks[id] = task
		if _, found := current[task.AgentID]; !found {
			current[task.AgentID] = task
		}
	}
	rows, err = tx.QueryContext(ctx, `SELECT agent_id FROM agents WHERE system_id=? AND (agent_id=? OR goal_id=?)
	 ORDER BY agent_id!=?,state='stopped',depth,agent_id LIMIT 33`, systemID, result.System.OperatorID, goalID, result.System.OperatorID)
	if err != nil {
		return result, err
	}
	ids, err = readIDs(rows)
	if err != nil {
		return result, err
	}
	if len(ids) > 32 {
		ids, result.Truncated = ids[:32], true
	}
	result.Agents = []ActivityAgent{}
	for _, id := range ids {
		agent, err := readAgent(ctx, tx, systemID, id, 0)
		if err != nil {
			return result, err
		}
		task := current[id]
		entry := ActivityAgent{ID: id, Parent: agent.Parent, Name: agent.Name, State: agent.State, Depth: agent.Depth,
			Model: agent.Definition.Model, TaskID: task.ID, TaskState: task.State, Control: task.Control,
			Turns: task.Turns, Reason: task.Reason, Response: task.Response}
		for _, message := range task.Conversation {
			if message.Role == "user" {
				entry.Assignment = message.Content
				break
			}
		}
		result.Agents = append(result.Agents, entry)
	}
	rows, err = tx.QueryContext(ctx, `SELECT call_id FROM model_calls WHERE system_id=? AND goal_id=? ORDER BY sequence DESC LIMIT 20`, systemID, goalID)
	if err != nil {
		return result, err
	}
	ids, err = readIDs(rows)
	if err != nil {
		return result, err
	}
	result.Calls = []ActivityCall{}
	for _, id := range ids {
		call, err := readExecution(ctx, tx, systemID, id)
		if err != nil {
			return result, err
		}
		entry := ActivityCall{ID: id, AgentID: call.AgentID, TaskID: call.TaskID, Model: call.Model, State: call.State,
			Input: call.InputTokens, Output: call.OutputTokens, UsageKnown: call.UsageKnown, CreatedAt: call.CreatedAt}
		if len(call.Actions) == 1 {
			entry.Tool = call.Actions[0].Function.Name
			for _, pin := range tasks[call.TaskID].Tools {
				if WireToolName(pin.Name) == entry.Tool {
					entry.Tool = pin.Name
					break
				}
			}
			err := tx.QueryRowContext(ctx, `SELECT name,state FROM tool_calls WHERE system_id=? AND call_id=? AND tool_call_id=?`,
				systemID, id, call.Actions[0].ID).Scan(&entry.Tool, &entry.ToolState)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return result, err
			}
		}
		result.Calls = append(result.Calls, entry)
	}
	rows, err = tx.QueryContext(ctx, `SELECT e.event_id,e.type,e.source,e.recipient,e.task_id,m.state,m.reason,e.created_at
	 FROM events e JOIN mailboxes m ON m.system_id=e.system_id AND m.event_id=e.event_id
	 WHERE e.system_id=? AND e.goal_id=? ORDER BY e.sequence DESC,m.sequence DESC LIMIT 20`, systemID, goalID)
	if err != nil {
		return result, err
	}
	result.Events = []ActivityEvent{}
	for rows.Next() {
		var entry ActivityEvent
		if err := rows.Scan(&entry.ID, &entry.Type, &entry.Source, &entry.Recipient, &entry.TaskID, &entry.State, &entry.Reason, &entry.CreatedAt); err != nil {
			return result, errors.Join(err, rows.Close())
		}
		result.Events = append(result.Events, entry)
	}
	return result, errors.Join(rows.Err(), rows.Close())
}
