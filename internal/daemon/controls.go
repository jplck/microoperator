//go:build linux

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jplck/microoperator/internal/state"
)

func (api *controlAPI) control(w http.ResponseWriter, r *http.Request) {
	if !api.executionAvailable(w) {
		return
	}
	action := r.PathValue("action")
	if action != "pause" && action != "resume" && action != "stop" {
		api.failure(w, state.ErrSystemNotFound)
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
	record, err := api.store.Control(r.Context(), api.cfg, key, r.PathValue("system_id"), scope, action, id)
	if action == "stop" {
		var result state.ControlResult
		if err == nil {
			if decodeErr := json.Unmarshal(record.CommandResult, &result); decodeErr != nil {
				api.failure(w, decodeErr)
				return
			}
			engine.cancelControlledCalls(r.Context(), record.ID, result.Calls)
		} else if !errors.Is(err, state.ErrSystemNotFound) && !errors.Is(err, state.ErrCommandConflict) {
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

// The caller holds engine.mu; durable stop state is committed before cancellation.
func (engine *executionEngine) cancelControlledCalls(ctx context.Context, systemID string, calls []string) {
	for _, active := range engine.active {
		if active.systemID != systemID {
			continue
		}
		if strings.HasPrefix(active.callID, "evaluation_") {
			task, err := engine.store.Task(ctx, systemID, active.taskID)
			if err != nil {
				engine.logger.Printf("evaluation control lookup: %v", err)
				active.cancel()
			} else if state.TaskTerminal(task.State) || task.Control == "stopped" {
				active.cancel()
			}
		}
		for _, call := range calls {
			if active.callID == call {
				active.cancel()
				break
			}
		}
	}
}
