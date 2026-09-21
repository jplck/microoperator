//go:build darwin || linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

func (engine *executionEngine) notify() {
	if engine.wake != nil {
		select {
		case engine.wake <- struct{}{}:
		default:
		}
	}
}

func (engine *executionEngine) schedule() {
	defer close(engine.schedulerDone)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-engine.ctx.Done():
			return
		case <-engine.wake:
		case <-tick.C:
		}
		engine.mu.Lock()
		if engine.ctx.Err() != nil {
			engine.mu.Unlock()
			return
		}
		for len(engine.active) < maxOperators && engine.broker.available() {
			record, e, found, err := engine.claim(engine.ctx, time.Now())
			if err != nil {
				if engine.ctx.Err() == nil {
					engine.broker.mu.Lock()
					engine.broker.failLocked(err)
					engine.broker.mu.Unlock()
				}
				break
			}
			if !found {
				break
			}
			ctx, cancel := context.WithDeadline(engine.ctx, time.UnixMilli(e.Deadline))
			engine.active[e.AgentID] = activation{cancel: cancel, callID: e.CallID, systemID: e.SystemID, goalID: e.GoalID, agentID: e.AgentID, eventID: e.EventID, taskID: e.TaskID}
			engine.wg.Add(1)
			go engine.execute(ctx, record, e)
		}
		engine.mu.Unlock()
	}
}

type mailboxCandidate struct {
	systemID, eventID, taskID, kind string
	payload                         []byte
	expires                         int64
	attempts, applied               int
}

func (engine *executionEngine) claim(ctx context.Context, now time.Time) (record systemRecord, e executionRecord, found bool, err error) {
	tx, err := engine.store.db.BeginTx(ctx, nil)
	if err != nil {
		return record, e, false, err
	}
	defer rollback(tx, &err)
	if err := engine.pumpWakeups(ctx, tx, now); err != nil {
		return record, e, false, err
	}
	if err := engine.pumpLearning(ctx, tx, now); err != nil {
		return record, e, false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.system_id,m.event_id,m.task_id,v.type,v.payload,v.expires_at,m.attempts,m.applied
	 FROM mailboxes m JOIN events v ON v.system_id=m.system_id AND v.event_id=m.event_id JOIN systems s ON s.system_id=m.system_id
	 JOIN tasks t ON t.system_id=m.system_id AND t.task_id=m.task_id
	 JOIN agents a ON a.system_id=t.system_id AND a.agent_id=t.agent_id
	 JOIN goals g ON g.system_id=t.system_id AND g.goal_id=t.goal_id
	 WHERE m.state='pending' AND m.not_before<=?
	 AND NOT EXISTS(SELECT 1 FROM mailboxes live WHERE live.system_id=m.system_id AND live.recipient=m.recipient AND live.state='leased')
	 AND (t.state IN ('completed','failed','canceled','rejected') OR t.control='stopped' OR a.state='stopped' OR g.control='stopped'
	  OR s.state IN ('stopping','stopped') OR v.expires_at<=? OR m.attempts>=3
	  OR (s.state='running' AND a.state='active' AND t.control='active' AND g.control='active'
	   AND (t.state!='waiting' OR t.waiting_tool='' OR v.type IN ('task.result','task.progress'))))
	 ORDER BY s.last_admission,m.sequence LIMIT 512`, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return record, e, false, err
	}
	var candidates []mailboxCandidate
	for rows.Next() {
		var c mailboxCandidate
		if err := rows.Scan(&c.systemID, &c.eventID, &c.taskID, &c.kind, &c.payload, &c.expires, &c.attempts, &c.applied); err != nil {
			return record, e, false, errors.Join(err, rows.Close())
		}
		candidates = append(candidates, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return record, e, false, err
	}
	for _, c := range candidates {
		t, err := readTask(ctx, tx, c.systemID, c.taskID)
		if err != nil {
			return record, e, false, err
		}
		if _, occupied := engine.active[t.AgentID]; occupied {
			continue
		}
		record, err = readSystem(ctx, tx, localAdministrator, c.systemID)
		if err != nil {
			return record, e, false, err
		}
		a, err := readAgent(ctx, tx, c.systemID, t.AgentID, t.Revision)
		if err != nil {
			return record, e, false, err
		}
		var goalControl string
		if err := tx.QueryRowContext(ctx, `SELECT control FROM goals WHERE system_id=? AND goal_id=?`, t.SystemID, t.GoalID).Scan(&goalControl); err != nil {
			return record, e, false, err
		}
		if taskTerminal(t.State) || t.Control == "stopped" || a.State == "stopped" || goalControl == "stopped" || record.State == "stopping" || record.State == "stopped" || c.expires <= now.UnixMilli() || c.attempts >= 3 {
			reason := "delivery expired, stopped, terminal, or retry limit exhausted"
			if _, err := tx.ExecContext(ctx, `UPDATE mailboxes SET state='dead',reason=? WHERE system_id=? AND event_id=?`, reason, t.SystemID, c.eventID); err != nil {
				return record, e, false, err
			}
			if !taskTerminal(t.State) {
				state := "failed"
				if t.Control == "stopped" || a.State == "stopped" || goalControl == "stopped" || record.State == "stopping" || record.State == "stopped" {
					state = "canceled"
				}
				if err := terminateTask(ctx, tx, t, state, "", reason, "runtime", c.eventID, now); err != nil {
					return record, e, false, err
				}
			}
			continue
		}
		if record.State != "running" || a.State != "active" || t.Control != "active" || goalControl != "active" {
			continue
		}
		if _, occupied := engine.active[t.AgentID]; occupied {
			continue
		}
		var active int64
		for _, activation := range engine.active {
			if activation.systemID == t.SystemID {
				active++
			}
		}
		if active >= record.Configuration.Limits.MaxActiveAgents {
			continue
		}
		if c.kind == "tool.evaluate" {
			e, err = engine.claimLearning(ctx, tx, record, t, c, now)
			if err != nil {
				if !deniedTask(err) {
					return record, e, false, err
				}
				if err := terminateTask(ctx, tx, t, "failed", "", err.Error(), "runtime", c.eventID, now); err != nil {
					return record, e, false, err
				}
				continue
			}
			found = true
			break
		}
		if c.kind == "task.progress" {
			if _, err := tx.ExecContext(ctx, `UPDATE mailboxes SET state='acked',applied=1 WHERE system_id=? AND event_id=?`, t.SystemID, c.eventID); err != nil {
				return record, e, false, err
			}
			continue
		}
		if t.State == "waiting" && t.WaitingTool != "" && c.kind != "task.result" {
			continue
		}
		if t.State == "waiting" && t.WaitingTool == "" {
			wakeErr := engine.consumeInputs(ctx, tx, &t, "")
			if wakeErr == nil {
				wakeErr = prepareNextCall(ctx, tx, engine.cfg, t, engine.owner, now)
			}
			if wakeErr != nil {
				if !deniedTask(wakeErr) {
					return record, e, false, wakeErr
				}
				if err := terminateTask(ctx, tx, t, "rejected", "", wakeErr.Error(), "runtime", c.eventID, now); err != nil {
					return record, e, false, err
				}
				continue
			}
			t, err = readTask(ctx, tx, t.SystemID, t.ID)
			if err != nil {
				return record, e, false, err
			}
		}
		if c.kind == "task.result" && c.applied == 0 {
			var payload struct {
				TaskID   string `json:"task_id"`
				State    string `json:"state"`
				Response string `json:"response"`
				Reason   string `json:"reason"`
			}
			if err := decodeJSON(c.payload, &payload); err != nil {
				return record, e, false, err
			}
			child, err := readTask(ctx, tx, t.SystemID, payload.TaskID)
			if err != nil {
				return record, e, false, err
			}
			if t.WaitingTool == "" || child.Parent != t.ID || child.GoalID != t.GoalID || !taskTerminal(child.State) {
				if _, err := tx.ExecContext(ctx, `UPDATE mailboxes SET state='dead',reason='reply is outside this continuation' WHERE system_id=? AND event_id=?`, t.SystemID, c.eventID); err != nil {
					return record, e, false, err
				}
				continue
			}
			result, err := json.Marshal(map[string]string{"task_id": child.ID, "state": child.State, "response": child.Response, "reason": child.Reason})
			if err != nil {
				return record, e, false, err
			}
			t.Conversation = append(t.Conversation, chatMessage{Role: "tool", ToolCallID: t.WaitingTool, Content: string(result)})
			if inputErr := engine.consumeInputs(ctx, tx, &t, c.eventID); inputErr != nil {
				if !deniedTask(inputErr) {
					return record, e, false, inputErr
				}
				if err := terminateTask(ctx, tx, t, "rejected", "", inputErr.Error(), "runtime", c.eventID, now); err != nil {
					return record, e, false, err
				}
				continue
			}
			if err := prepareNextCall(ctx, tx, engine.cfg, t, engine.owner, now); err != nil {
				if !deniedTask(err) {
					return record, e, false, err
				}
				if err := terminateTask(ctx, tx, t, "rejected", "", err.Error(), "runtime", c.eventID, now); err != nil {
					return record, e, false, err
				}
				continue
			}
			t, err = readTask(ctx, tx, t.SystemID, t.ID)
			if err != nil {
				return record, e, false, err
			}
		}
		if c.kind != "task.ready" && c.kind != "task.continue" && c.kind != "task.result" && c.kind != "user.input" && c.kind != "schedule.fired" && c.kind != "memory.changed" && c.kind != "task.notice" {
			return record, e, false, errors.New("unsupported stored event type")
		}
		e, err = readExecution(ctx, tx, t.SystemID, t.CallID)
		if err != nil {
			return record, e, false, err
		}
		record, err = engine.cfg.inspect(record)
		if err != nil {
			return record, e, false, err
		}
		if record.BlockedReason != "" {
			if err := terminateTask(ctx, tx, t, "rejected", "", record.BlockedReason, "runtime", c.eventID, now); err != nil {
				return record, e, false, err
			}
			continue
		}
		if err := authorizePins(ctx, tx, engine.cfg, t.SystemID, t.Tools); err != nil {
			if !deniedTask(err) {
				return record, e, false, err
			}
			if err := terminateTask(ctx, tx, t, "rejected", "", err.Error(), "runtime", c.eventID, now); err != nil {
				return record, e, false, err
			}
			continue
		}
		if e.State == "awaiting_worker" && e.Attempts == 0 {
			if inputErr := engine.consumeInputs(ctx, tx, &t, ""); inputErr != nil {
				if !deniedTask(inputErr) {
					return record, e, false, inputErr
				}
				if err := terminateTask(ctx, tx, t, "rejected", "", inputErr.Error(), "runtime", c.eventID, now); err != nil {
					return record, e, false, err
				}
				continue
			}
			record, _, err = pinnedTaskRecord(ctx, tx, record, e)
			if err != nil {
				return record, e, false, err
			}
			body, reservation, requestErr := conversationRequest(engine.cfg, record, t.Conversation, e.Stream)
			if requestErr != nil {
				if err := terminateTask(ctx, tx, t, "rejected", "", requestErr.Error(), "runtime", c.eventID, now); err != nil {
					return record, e, false, err
				}
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE model_calls SET request=?,reservation=? WHERE call_id=?`, body, reservation, e.CallID); err != nil {
				return record, e, false, err
			}
		} else {
			record, _, err = pinnedTaskRecord(ctx, tx, record, e)
			if err != nil {
				return record, e, false, err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE model_calls SET activation_owner=? WHERE call_id=?`, engine.owner, e.CallID); err != nil {
			return record, e, false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state='running' WHERE system_id=? AND task_id=?`, t.SystemID, t.ID); err != nil {
			return record, e, false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE mailboxes SET state='leased',lease_owner=?,lease_until=?,attempts=attempts+1,applied=1
		 WHERE system_id=? AND event_id=?`, engine.owner, e.Deadline, t.SystemID, c.eventID); err != nil {
			return record, e, false, err
		}
		e, err = readExecution(ctx, tx, t.SystemID, e.CallID)
		if err != nil {
			return record, e, false, err
		}
		e.EventID = c.eventID
		if len(t.Conversation) > 0 {
			e.Prompt = t.Conversation[0].Content
		}
		found = true
		break
	}
	if _, err := tx.ExecContext(ctx, `UPDATE systems SET state='stopped' WHERE state='stopping'
	 AND NOT EXISTS(SELECT 1 FROM mailboxes WHERE mailboxes.system_id=systems.system_id AND state='leased')`); err != nil {
		return record, e, false, err
	}
	if err := tx.Commit(); err != nil {
		return record, e, false, err
	}
	return record, e, found, nil
}

func (engine *executionEngine) consumeInputs(ctx context.Context, tx *sql.Tx, t *taskRecord, exclude string) error {
	rows, err := tx.QueryContext(ctx, `SELECT m.event_id,e.payload FROM mailboxes m JOIN events e ON e.system_id=m.system_id AND e.event_id=m.event_id
	 WHERE m.system_id=? AND m.task_id=? AND m.state='pending' AND m.applied=0 AND e.type IN ('user.input','schedule.fired','memory.changed','task.notice') AND m.event_id!=? ORDER BY m.sequence`, t.SystemID, t.ID, exclude)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			return errors.Join(err, rows.Close())
		}
		var payload struct {
			Content string `json:"content"`
		}
		if err := decodeJSON(data, &payload); err != nil {
			return errors.Join(err, rows.Close())
		}
		t.Conversation = append(t.Conversation, chatMessage{Role: "user", Content: payload.Content})
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if err := updateConversation(ctx, tx, *t); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE mailboxes SET state='acked',applied=1 WHERE system_id=? AND event_id=?`, t.SystemID, id); err != nil {
			return err
		}
	}
	return nil
}

func queueContinuation(ctx context.Context, tx *sql.Tx, t taskRecord, cause string, now time.Time) error {
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mailboxes m JOIN events e ON e.system_id=m.system_id AND e.event_id=m.event_id
	 WHERE m.system_id=? AND m.task_id=? AND m.state='pending' AND e.type!='task.progress'`, t.SystemID, t.ID).Scan(&pending); err != nil {
		return err
	}
	if pending > 0 {
		return nil
	}
	_, err := emitEvent(ctx, tx, t, "task.continue", t.AgentID, cause, struct{}{}, now)
	return err
}

func (engine *executionEngine) finishDelivery(ctx context.Context, session executionRecord, workerErr error, canceled bool) (err error) {
	tx, err := engine.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx, &err)
	e, err := authorizeCall(ctx, tx, session)
	if err != nil {
		return err
	}
	t, err := readTask(ctx, tx, e.SystemID, e.TaskID)
	if err != nil {
		return err
	}
	if t.CallID != e.CallID || taskTerminal(t.State) {
		if _, err := tx.ExecContext(ctx, `UPDATE mailboxes SET state='dead',reason='activation ended after task termination' WHERE system_id=? AND event_id=? AND lease_owner=? AND state='leased'`, t.SystemID, session.EventID, engine.owner); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE systems SET state='stopped' WHERE state='stopping' AND NOT EXISTS(SELECT 1 FROM mailboxes WHERE mailboxes.system_id=systems.system_id AND state='leased')`); err != nil {
			return err
		}
		return tx.Commit()
	}
	now := time.Now()
	var systemState, goalControl string
	if err := tx.QueryRowContext(ctx, `SELECT s.state,g.control FROM systems s JOIN goals g USING(system_id) WHERE s.system_id=? AND g.goal_id=?`, t.SystemID, t.GoalID).Scan(&systemState, &goalControl); err != nil {
		return err
	}
	shutdown := engine.ctx != nil && engine.ctx.Err() != nil && systemState != "stopping" && systemState != "stopped" && goalControl != "stopped" && t.Control != "stopped"
	if shutdown && (e.State == "awaiting_worker" || (e.State == "queued" && e.Attempts == 0) || e.State == "completed") {
		var unknownTools int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tool_calls WHERE system_id=? AND call_id=? AND state IN ('running','unknown')`, e.SystemID, e.CallID).Scan(&unknownTools); err != nil {
			return err
		}
		if unknownTools == 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state=CASE WHEN state='waiting' THEN state ELSE 'queued' END WHERE system_id=? AND task_id=?`, t.SystemID, t.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE mailboxes SET state='pending',lease_owner='',lease_until=0 WHERE system_id=? AND event_id=?`, t.SystemID, session.EventID); err != nil {
				return err
			}
			return tx.Commit()
		}
	}
	stopped := canceled || systemState == "stopping" || systemState == "stopped" || goalControl == "stopped" || t.Control == "stopped"
	var attempts int
	if session.EventID != "" {
		if err := tx.QueryRowContext(ctx, `SELECT attempts FROM mailboxes WHERE system_id=? AND event_id=? AND lease_owner=?`, t.SystemID, session.EventID, engine.owner).Scan(&attempts); err != nil {
			return err
		}
	}
	if workerErr != nil && !stopped && e.Attempts == 0 && (e.State == "awaiting_worker" || e.State == "queued") && attempts < 3 {
		if _, err := tx.ExecContext(ctx, `UPDATE model_calls SET state='awaiting_worker',reason='worker failed before dispatch; bounded retry pending' WHERE call_id=?`, e.CallID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state='queued' WHERE system_id=? AND task_id=?`, t.SystemID, t.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE mailboxes SET state='pending',not_before=?,reason='worker failed before dispatch' WHERE system_id=? AND event_id=?`,
			now.Add(time.Duration(1<<attempts)*time.Second).UnixMilli(), t.SystemID, session.EventID); err != nil {
			return err
		}
		return tx.Commit()
	}
	if session.EventID != "" {
		state := "acked"
		reason := ""
		if workerErr != nil {
			state, reason = "dead", taskFailureReason(workerErr)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE mailboxes SET state=?,reason=? WHERE system_id=? AND event_id=?`, state, reason, t.SystemID, session.EventID); err != nil {
			return err
		}
	}
	state := "failed"
	reason := e.Reason
	if stopped {
		state, reason = "canceled", "execution canceled"
	}
	if workerErr == nil && e.State == "completed" && !stopped {
		if len(e.Actions) == 1 {
			var result, receiptState string
			if err := tx.QueryRowContext(ctx, `SELECT result,state FROM tool_calls WHERE system_id=? AND call_id=? AND tool_call_id=?`, t.SystemID, e.CallID, e.Actions[0].ID).Scan(&result, &receiptState); err != nil {
				return err
			}
			if receiptState != "completed" {
				return errors.New("tool completion lacks durable receipt")
			}
			if t.State == "waiting" {
				if err := auditExecution(ctx, tx, t.SystemID, t.AgentID, "task.waiting", e.Revision, now); err != nil {
					return err
				}
				return tx.Commit()
			}
			appendToolResult(&t, e, result)
		} else {
			var pending int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mailboxes m JOIN events v ON m.system_id=v.system_id AND m.event_id=v.event_id
			 WHERE m.system_id=? AND m.task_id=? AND m.state='pending' AND v.type IN ('user.input','schedule.fired','memory.changed','task.notice')`, t.SystemID, t.ID).Scan(&pending); err != nil {
				return err
			}
			if pending == 0 {
				var triggers int
				if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM schedules WHERE system_id=? AND task_id=? AND state='active')+
				 (SELECT count(*) FROM subscriptions WHERE system_id=? AND task_id=? AND state='active')`, t.SystemID, t.ID, t.SystemID, t.ID).Scan(&triggers); err != nil {
					return err
				}
				if triggers > 0 {
					t.Conversation = append(t.Conversation, chatMessage{Role: "assistant", Content: e.Response})
					if err := updateConversation(ctx, tx, t); err != nil {
						return err
					}
					if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state='waiting',waiting_tool='',reason='awaiting scheduled or subscribed input' WHERE system_id=? AND task_id=?`, t.SystemID, t.ID); err != nil {
						return err
					}
					if err := auditExecution(ctx, tx, t.SystemID, t.AgentID, "task.waiting", e.Revision, now); err != nil {
						return err
					}
					return tx.Commit()
				}
				state = "completed"
			} else {
				t.Conversation = append(t.Conversation, chatMessage{Role: "assistant", Content: e.Response})
			}
		}
		if state != "completed" {
			if err := prepareNextCall(ctx, tx, engine.cfg, t, engine.owner, now); err != nil {
				if !deniedTask(err) {
					return err
				}
				state, reason = "rejected", err.Error()
			} else {
				if err := queueContinuation(ctx, tx, t, session.EventID, now); err != nil {
					if !deniedTask(err) {
						return err
					}
					if err := terminateTask(ctx, tx, t, "rejected", "", err.Error(), "runtime", session.EventID, now); err != nil {
						return err
					}
					return tx.Commit()
				}
				if err := auditExecution(ctx, tx, t.SystemID, t.AgentID, "task.continue", e.Revision, now); err != nil {
					return err
				}
				return tx.Commit()
			}
		}
	}
	if e.State == "rejected" && !stopped {
		state = "rejected"
	}
	if reason == "" && workerErr != nil {
		reason = taskFailureReason(workerErr)
	}
	if e.State == "awaiting_worker" || e.State == "queued" {
		if err := terminalCall(ctx, tx, e, state, reason); err != nil {
			return err
		}
	}
	if err := terminateTask(ctx, tx, t, state, e.Response, reason, t.AgentID, session.EventID, now); err != nil {
		return err
	}
	if state == "completed" {
		if err := engine.notifySubscriptions(ctx, tx, t, "task.completed", "", "", session.EventID, t.ID, now); err != nil {
			return err
		}
	}
	if stopped && t.Parent == "" {
		if _, err := tx.ExecContext(ctx, `UPDATE systems SET state=CASE WHEN EXISTS(SELECT 1 FROM mailboxes WHERE system_id=? AND state='leased')
		 THEN 'stopping' ELSE 'stopped' END WHERE system_id=?`, t.SystemID, t.SystemID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE systems SET state='stopped' WHERE state='stopping'
	 AND NOT EXISTS(SELECT 1 FROM mailboxes WHERE mailboxes.system_id=systems.system_id AND state='leased')`); err != nil {
		return err
	}
	if err := auditExecution(ctx, tx, t.SystemID, t.AgentID, "task."+state, e.Revision, now); err != nil {
		return err
	}
	return tx.Commit()
}

// Recovery only retries work proven not to have dispatched, or reuses a durable
// completed call and its local receipts. Unknown network/subprocess outcomes end
// the task; they never become fresh attempts after a daemon restart.
func (store *stateStore) recoverExecutions(ctx context.Context) (err error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx, &err)
	if _, err := tx.ExecContext(ctx, `
	 UPDATE tasks SET state='failed',reason='daemon restarted during generated build; not retried' WHERE learning_role='build0' AND state='running';
	 UPDATE mailboxes SET state='dead',reason='generated build outcome unknown'
	  WHERE EXISTS(SELECT 1 FROM tasks t WHERE t.system_id=mailboxes.system_id AND t.task_id=mailboxes.task_id AND t.learning_role='build0' AND t.state='failed');
	 UPDATE model_attempts SET state='unknown' WHERE state='running';
	 UPDATE model_calls SET state='unknown',reason='daemon restarted after dispatch; outcome requires reconciliation' WHERE state='running';
	 UPDATE tool_calls SET state='unknown',result='daemon restarted during tool dispatch; not retried' WHERE state='running';
	 UPDATE mailboxes SET state='pending',lease_owner='',lease_until=0 WHERE state='leased';
	 UPDATE mailboxes SET state='acked' WHERE state='pending' AND applied=1
	  AND EXISTS(SELECT 1 FROM tasks t JOIN tool_calls c ON c.system_id=t.system_id AND c.call_id=t.call_id
	   WHERE t.system_id=mailboxes.system_id AND t.task_id=mailboxes.task_id AND t.state='waiting' AND c.state='completed');
	 UPDATE tasks SET state='queued' WHERE state='running';`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT t.system_id,t.task_id FROM tasks t JOIN model_calls c ON c.call_id=t.call_id
	 WHERE t.state IN ('queued','running','waiting') AND (c.state IN ('unknown','failed','canceled','rejected')
	 OR EXISTS(SELECT 1 FROM tool_calls u WHERE u.system_id=t.system_id AND u.call_id=c.call_id AND u.state='unknown'))`)
	if err != nil {
		return err
	}
	var ids [][2]string
	for rows.Next() {
		var id [2]string
		if err := rows.Scan(&id[0], &id[1]); err != nil {
			return errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, id := range ids {
		t, err := readTask(ctx, tx, id[0], id[1])
		if err != nil {
			return err
		}
		if err := terminateTask(ctx, tx, t, "failed", "", "daemon restarted with an unrecoverable outcome", "runtime", "", time.Now()); err != nil {
			return err
		}
		if t.Parent == "" {
			if _, err := tx.ExecContext(ctx, `UPDATE systems SET state='stopped' WHERE system_id=?`, t.SystemID); err != nil {
				return err
			}
		}
	}
	// Version-2 calls have no mailbox/continuation and retain their old conservative recovery.
	if _, err := tx.ExecContext(ctx, `
	 UPDATE model_calls SET state='failed',reason='legacy activation has no resumable mailbox'
	  WHERE state IN ('awaiting_worker','queued') AND NOT EXISTS(SELECT 1 FROM tasks t WHERE t.system_id=model_calls.system_id AND t.task_id=model_calls.task_id);
	 UPDATE goals SET state='failed' WHERE state IN ('running','waiting','queued') AND NOT EXISTS(SELECT 1 FROM tasks t WHERE t.system_id=goals.system_id AND t.goal_id=goals.goal_id);
	 UPDATE systems SET state='stopped' WHERE state IN ('running','stopping') AND NOT EXISTS(SELECT 1 FROM tasks t WHERE t.system_id=systems.system_id AND t.state IN ('queued','running','waiting'));`); err != nil {
		return err
	}
	return tx.Commit()
}

func (engine *executionEngine) taskSnapshot(ctx context.Context, systemID, taskID string) (t taskRecord, err error) {
	tx, err := engine.store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return t, err
	}
	defer rollback(tx, &err)
	return readTask(ctx, tx, systemID, taskID)
}
