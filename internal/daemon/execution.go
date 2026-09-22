//go:build linux

package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jplck/microoperator/internal/protocol"
	"github.com/jplck/microoperator/internal/sandbox"
	"github.com/jplck/microoperator/internal/state"
)

type activation struct {
	cancel                                     context.CancelFunc
	callID                                     string
	systemID, goalID, agentID, eventID, taskID string
}

type executionEngine struct {
	mu                sync.Mutex
	active            map[string]activation
	wg                sync.WaitGroup
	ctx               context.Context
	cancel            context.CancelFunc
	owner, executable string
	store             *state.Store
	cfg               state.Configuration
	broker            *modelBroker
	logger            *log.Logger
	wake              chan struct{}
	schedulerDone     chan struct{}
}

func newExecutionEngine(ctx context.Context, store *state.Store, cfg state.Configuration, logger *log.Logger) (*executionEngine, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	owner, err := state.NewID("daemon_")
	if err != nil {
		return nil, err
	}
	child, cancel := context.WithCancel(ctx)
	broker := newModelBroker(store, cfg, logger)
	broker.lookup = os.LookupEnv
	engine := &executionEngine{active: make(map[string]activation), ctx: child, cancel: cancel, owner: owner,
		executable: executable, store: store, cfg: cfg, broker: broker, logger: logger, wake: make(chan struct{}, 1), schedulerDone: make(chan struct{})}
	if err := store.RecoverExecutions(ctx); err != nil {
		cancel()
		return nil, err
	}
	go engine.schedule()
	return engine, nil
}

func (engine *executionEngine) close() {
	engine.mu.Lock()
	engine.cancel()
	for _, active := range engine.active {
		active.cancel()
	}
	engine.mu.Unlock()
	if engine.schedulerDone != nil {
		<-engine.schedulerDone
	}
	engine.wg.Wait()
	engine.broker.client.CloseIdleConnections()
}

func (engine *executionEngine) start(ctx context.Context, principal, key, id string, command state.StartSystemCommand) (state.SystemRecord, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if err := engine.ctx.Err(); err != nil {
		return state.SystemRecord{}, err
	}
	if !engine.broker.available() {
		return state.SystemRecord{}, errBrokerUnavailable
	}
	record, err := engine.store.StartSystem(ctx, principal, key, id, engine.owner, command, engine.cfg, time.Now())
	if err != nil {
		return record, err
	}
	engine.notify()
	return record, nil
}

func (engine *executionEngine) stop(ctx context.Context, principal, key, id string) (state.SystemRecord, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	record, err := engine.store.StopSystem(ctx, principal, key, id, time.Now())
	var field *state.FieldError
	// A storage failure must not make emergency cancellation unavailable.
	if err == nil || (!errors.Is(err, state.ErrSystemNotFound) && !errors.Is(err, state.ErrCommandConflict) && !errors.As(err, &field)) {
		if principal == localAdministrator {
			for key, active := range engine.active {
				if active.systemID == id || (active.systemID == "" && key == id) {
					if err != nil || ((record.State == "stopping" || record.State == "stopped") && record.Execution != nil && (active.goalID == record.Execution.GoalID || active.callID == record.Execution.CallID)) {
						active.cancel()
					}
				}
			}
		}
	}
	return record, err
}

func (engine *executionEngine) execute(ctx context.Context, record state.SystemRecord, e state.ExecutionRecord) {
	defer engine.wg.Done()
	if e.LearningID != "" {
		engine.executeLearning(ctx, record, e)
		return
	}
	err := engine.runActivation(ctx, record, e)
	engine.mu.Lock()
	defer engine.mu.Unlock()
	wasCanceled := ctx.Err() != nil
	if active, ok := engine.active[e.AgentID]; ok {
		active.cancel()
		delete(engine.active, e.AgentID)
	}
	finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if finishErr := engine.finishDelivery(finishCtx, e, err, wasCanceled); finishErr != nil {
		engine.broker.mu.Lock()
		engine.broker.failLocked(finishErr)
		engine.broker.mu.Unlock()
	}
	if err != nil {
		engine.logger.Printf("system %s activation %s: %v", e.SystemID, e.ActivationID, err)
	}
	engine.notify()
}

func (engine *executionEngine) runActivation(ctx context.Context, record state.SystemRecord, e state.ExecutionRecord) (err error) {
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
	return sandbox.Supervise(ctx, engine.executable, root, engine.executable, []string{"worker", "operator"},
		func(in io.Writer, out io.Reader) error {
			reader := bufio.NewReaderSize(out, protocol.MaxFrame+1)
			ready, err := protocol.ReadMessage(reader)
			if err != nil {
				return err
			}
			if ready != (protocol.Message{Type: "ready"}) {
				return errors.New("invalid worker readiness")
			}
			if err := protocol.WriteMessage(in, protocol.Message{Type: "task", ID: e.CallID, Data: e.Prompt}); err != nil {
				return err
			}
			request, err := protocol.ReadMessage(reader)
			if err != nil {
				return err
			}
			if request != (protocol.Message{Type: "model.call", ID: e.CallID, Data: e.Prompt}) {
				return errors.New("worker requested an operation outside its scoped task")
			}
			result, err := engine.broker.call(ctx, e)
			if err != nil {
				return err
			}
			reply := protocol.Message{Type: "model.result", ID: e.CallID, Data: result.Response}
			if len(result.Actions) == 1 {
				data, err := json.Marshal(result.Actions[0])
				if err != nil {
					return err
				}
				if err := protocol.WriteMessage(in, protocol.Message{Type: "model.action", ID: e.CallID, Data: string(data)}); err != nil {
					return err
				}
				request, err := protocol.ReadMessage(reader)
				if err != nil {
					return err
				}
				var action state.ModelToolCall
				if request.Type != "tool.call" || request.ID != e.CallID || protocol.DecodeJSON([]byte(request.Data), &action) != nil {
					return errors.New("invalid scoped tool invocation")
				}
				result.EventID = e.EventID
				value, toolErr := engine.invokeTool(ctx, result, action)
				responseType := "tool.result"
				if toolErr != nil {
					responseType, value = "tool.error", state.TaskFailureReason(toolErr)
				}
				if toolErr == nil && (action.Function.Name == state.WireToolName("runtime.task.delegate") || action.Function.Name == state.WireToolName("runtime.task.wait")) {
					responseType = "tool.wait"
				}
				if err := protocol.WriteMessage(in, protocol.Message{Type: responseType, ID: e.CallID, Data: value}); err != nil {
					return errors.Join(toolErr, err)
				}
				if toolErr != nil {
					return toolErr
				}
				ack, err := protocol.ReadMessage(reader)
				if err != nil {
					return err
				}
				expected := "task.continue"
				if responseType == "tool.wait" {
					expected = "task.waiting"
				}
				if ack != (protocol.Message{Type: expected, ID: e.CallID}) {
					return errors.New("invalid continuation acknowledgement")
				}
				return nil
			}
			if result.State != "completed" {
				reply.Type, reply.Data = "model.error", result.Reason
			}
			if err := protocol.WriteMessage(in, reply); err != nil {
				return err
			}
			ack, err := protocol.ReadMessage(reader)
			if err != nil {
				return err
			}
			if result.State != "completed" {
				return fmt.Errorf("model call ended %s: %s", result.State, result.Reason)
			}
			if ack != (protocol.Message{Type: "task.complete", ID: e.CallID}) {
				return errors.New("missing scoped task completion acknowledgement")
			}
			return nil
		}, profile)
}
