//go:build darwin || linux

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// operatorWorker is deliberately a single model turn, not an interpreter for
// returned text. It never loads credentials, opens the database, or calls HTTP.
// The parent binds this private pipe to a persisted activation before sending
// task data; the ID only correlates messages and cannot select another identity.
func operatorWorker(in io.Reader, out io.Writer) error {
	reader := bufio.NewReaderSize(in, maxFrame+1)
	if err := writeMessage(out, message{Type: "ready"}); err != nil {
		return err
	}
	task, err := readMessage(reader)
	if err != nil {
		return err
	}
	if task.Type != "task" || task.ID == "" || task.Data == "" {
		return errors.New("invalid operator task")
	}
	if err := writeMessage(out, message{Type: "model.call", ID: task.ID, Data: task.Data}); err != nil {
		return err
	}
	reply, err := readMessage(reader)
	if err != nil {
		return err
	}
	if reply.ID != task.ID {
		return errors.New("model response correlation mismatch")
	}
	if reply.Type == "model.error" {
		if err := writeMessage(out, message{Type: "task.failed", ID: task.ID}); err != nil {
			return err
		}
		return errors.New("model call failed")
	}
	if reply.Type != "model.result" {
		return errors.New("unexpected model response")
	}
	return writeMessage(out, message{Type: "task.complete", ID: task.ID})
}

type activation struct {
	cancel context.CancelFunc
	callID string
}

type executionEngine struct {
	mu                sync.Mutex
	active            map[string]activation
	wg                sync.WaitGroup
	ctx               context.Context
	cancel            context.CancelFunc
	owner, executable string
	store             *stateStore
	cfg               configuration
	broker            *modelBroker
	logger            *log.Logger
}

func newExecutionEngine(ctx context.Context, store *stateStore, cfg configuration, logger *log.Logger) (*executionEngine, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	owner, err := newID("daemon_")
	if err != nil {
		return nil, err
	}
	if err := store.recoverExecutions(ctx); err != nil {
		return nil, err
	}
	child, cancel := context.WithCancel(ctx)
	broker := newModelBroker(store, cfg, logger)
	broker.lookup = os.LookupEnv
	return &executionEngine{active: make(map[string]activation), ctx: child, cancel: cancel, owner: owner,
		executable: executable, store: store, cfg: cfg, broker: broker, logger: logger}, nil
}

func (engine *executionEngine) close() {
	engine.mu.Lock()
	engine.cancel()
	for _, active := range engine.active {
		active.cancel()
	}
	engine.mu.Unlock()
	engine.wg.Wait()
	engine.broker.client.CloseIdleConnections()
}

func (engine *executionEngine) start(ctx context.Context, principal, key, id string, command startSystemCommand) (systemRecord, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if err := engine.ctx.Err(); err != nil {
		return systemRecord{}, err
	}
	if !engine.broker.available() {
		return systemRecord{}, errBrokerUnavailable
	}
	record, err := engine.store.startSystem(ctx, principal, key, id, engine.owner, command, engine.cfg, time.Now())
	if err != nil {
		return record, err
	}
	checkCtx, cancelCheck := context.WithTimeout(engine.ctx, 5*time.Second)
	defer cancelCheck()
	current, err := engine.store.getSystem(checkCtx, principal, id)
	if err != nil {
		return record, err
	}
	e := current.Execution
	// Replayed control receipts describe the original result, not a fresh launch.
	if e == nil || record.Execution == nil || e.CallID != record.Execution.CallID ||
		e.State != "awaiting_worker" || e.ActivationOwner != engine.owner {
		return record, nil
	}
	if _, exists := engine.active[id]; exists {
		return record, nil
	}
	workerCtx, cancel := context.WithDeadline(engine.ctx, time.UnixMilli(e.Deadline))
	engine.active[id] = activation{cancel: cancel, callID: e.CallID}
	engine.wg.Add(1)
	go engine.execute(workerCtx, current, *e)
	return record, nil
}

func (engine *executionEngine) stop(ctx context.Context, principal, key, id string) (systemRecord, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	record, err := engine.store.stopSystem(ctx, principal, key, id, time.Now())
	var field *fieldError
	// A storage failure must not make emergency cancellation unavailable.
	if err == nil || (!errors.Is(err, errSystemNotFound) && !errors.Is(err, errCommandConflict) && !errors.As(err, &field)) {
		if principal == localAdministrator {
			if active, ok := engine.active[id]; ok {
				if err != nil || (record.State == "stopping" && record.Execution != nil && record.Execution.CallID == active.callID) {
					active.cancel()
				}
			}
		}
	}
	return record, err
}

func (engine *executionEngine) execute(ctx context.Context, record systemRecord, e executionRecord) {
	defer engine.wg.Done()
	err := engine.runActivation(ctx, record, e)
	engine.mu.Lock()
	defer engine.mu.Unlock()
	wasCanceled := ctx.Err() != nil
	engine.active[e.SystemID].cancel()
	delete(engine.active, e.SystemID)
	finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if finishErr := engine.finish(finishCtx, e, err, wasCanceled); finishErr != nil {
		engine.broker.mu.Lock()
		engine.broker.failLocked(finishErr)
		engine.broker.mu.Unlock()
	}
	if err != nil {
		engine.logger.Printf("system %s activation %s: %v", e.SystemID, e.ActivationID, err)
	}
}

func (engine *executionEngine) runActivation(ctx context.Context, record systemRecord, e executionRecord) (err error) {
	// Each activation owns a new empty tree; profile directories are prepared
	// before the worker exists, never by following worker-created symlinks.
	root, err := os.MkdirTemp(engine.cfg.DataDir, "activation-"+e.SystemID+"-")
	if err != nil {
		return fmt.Errorf("create activation workspace: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(root)) }()
	profile := engine.cfg.SandboxProfiles[record.Grants.SandboxProfile]
	paths := append([]string{"inputs", "scratch", "output"}, profile.Read...)
	paths = append(paths, profile.ReadWrite...)
	for _, name := range paths {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			return err
		}
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	return supervise(ctx, engine.executable, root, engine.executable, []string{"worker", "operator"},
		func(in io.Writer, out io.Reader) error {
			reader := bufio.NewReaderSize(out, maxFrame+1)
			ready, err := readMessage(reader)
			if err != nil {
				return err
			}
			if ready != (message{Type: "ready"}) {
				return errors.New("invalid worker readiness")
			}
			if err := writeMessage(in, message{Type: "task", ID: e.CallID, Data: e.Prompt}); err != nil {
				return err
			}
			request, err := readMessage(reader)
			if err != nil {
				return err
			}
			if request != (message{Type: "model.call", ID: e.CallID, Data: e.Prompt}) {
				return errors.New("worker requested an operation outside its scoped task")
			}
			result, err := engine.broker.call(ctx, e)
			if err != nil {
				return err
			}
			reply := message{Type: "model.result", ID: e.CallID, Data: result.Response}
			if result.State != "completed" {
				reply.Type, reply.Data = "model.error", result.Reason
			}
			if err := writeMessage(in, reply); err != nil {
				return err
			}
			ack, err := readMessage(reader)
			if err != nil {
				return err
			}
			if result.State != "completed" {
				return fmt.Errorf("model call ended %s: %s", result.State, result.Reason)
			}
			if ack != (message{Type: "task.complete", ID: e.CallID}) {
				return errors.New("missing scoped task completion acknowledgement")
			}
			return nil
		}, profile)
}

func (engine *executionEngine) finish(ctx context.Context, session executionRecord, workerErr error, canceled bool) (err error) {
	tx, err := engine.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx, &err)
	e, err := authorizeCall(ctx, tx, session)
	if err != nil {
		return err
	}
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM systems WHERE system_id=?`, e.SystemID).Scan(&state); err != nil {
		return err
	}
	taskState := "failed"
	switch {
	case state == "stopping" || canceled:
		taskState = "canceled"
	case workerErr == nil && e.State == "completed":
		taskState = "completed"
	case e.State == "rejected":
		taskState = "rejected"
	}
	if e.State == "awaiting_worker" || e.State == "queued" {
		callState := "failed"
		if taskState == "canceled" {
			callState = "canceled"
		}
		if err := terminalCall(ctx, tx, e, callState, "activation ended before dispatch"); err != nil {
			return err
		}
	}
	if workerErr != nil && e.Reason == "" {
		if _, err := tx.ExecContext(ctx, `UPDATE model_calls SET reason='worker failed before acknowledging task completion' WHERE call_id=?`, e.CallID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE goals SET state=? WHERE system_id=? AND goal_id=?`, taskState, e.SystemID, e.GoalID); err != nil {
		return err
	}
	systemState := "inactive"
	if state == "stopping" || canceled {
		systemState = "stopped"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE systems SET state=? WHERE system_id=?`, systemState, e.SystemID); err != nil {
		return err
	}
	if err := auditExecution(ctx, tx, e.SystemID, e.ActivationID, "task."+taskState, e.Revision, time.Now()); err != nil {
		return err
	}
	return tx.Commit()
}
