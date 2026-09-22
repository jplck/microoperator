//go:build darwin || linux

package daemon

import (
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
			for _, active := range engine.active {
				if active.systemID == record.ID && strings.HasPrefix(active.callID, "evaluation_") {
					task, taskErr := engine.taskSnapshot(r.Context(), record.ID, active.taskID)
					if taskErr != nil {
						api.logger.Printf("evaluation control lookup: %v", taskErr)
						active.cancel()
					} else if state.TaskTerminal(task.State) || task.Control == "stopped" {
						active.cancel()
					}
				}
				for _, call := range result.Calls {
					if active.callID == call {
						active.cancel()
					}
				}
			}
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
