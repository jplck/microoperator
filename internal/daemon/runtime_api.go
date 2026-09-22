//go:build linux

package daemon

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jplck/microoperator/internal/state"
)

func (api *controlAPI) registerRuntimeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/systems/{system_id}/activity", api.activity)
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
	mux.HandleFunc("POST /v1/systems/{system_id}/attachments", api.attachment)
	mux.HandleFunc("POST /v1/systems/{system_id}/{action}", api.control)
	mux.HandleFunc("POST /v1/systems/{system_id}/agents/{agent_id}/{action}", api.control)
	mux.HandleFunc("POST /v1/systems/{system_id}/goals/{goal_id}/{action}", api.control)
}

func (api *controlAPI) activity(w http.ResponseWriter, r *http.Request) {
	snapshot, err := api.store.Activity(r.Context(), localAdministrator, r.PathValue("system_id"))
	if err == nil {
		snapshot.System, err = api.cfg.Inspect(snapshot.System)
	}
	if err != nil {
		api.failure(w, err)
		return
	}
	api.respond(w, http.StatusOK, snapshot)
}

func (api *controlAPI) tools(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("system_id")
	entries, err := api.store.Catalog(r.Context(), api.cfg, localAdministrator, id)
	if err != nil {
		api.failure(w, err)
		return
	}
	if toolID := r.PathValue("tool_id"); toolID != "" {
		var selected *state.RegistryEntry
		for _, entry := range entries {
			if entry.ID == toolID {
				if selected == nil || entry.Version > selected.Version {
					copy := entry
					selected = &copy
				}
			}
		}
		if selected != nil {
			api.respond(w, 200, selected)
			return
		}
		api.failure(w, state.ErrSystemNotFound)
		return
	}
	api.respond(w, 200, struct {
		Tools []state.RegistryEntry `json:"tools"`
	}{entries})
}

func (api *controlAPI) draft(w http.ResponseWriter, r *http.Request) {
	var command state.DraftCommand
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	record, err := api.store.ProposeTool(r.Context(), api.cfg, key, r.PathValue("system_id"), command)
	api.systemResponse(w, 201, record, err)
}

func (api *controlAPI) draftState(w http.ResponseWriter, r *http.Request) {
	var command state.ChangeDraftStateCommand
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	id := r.PathValue("tool_id")
	record, err := api.store.ChangeDraftState(r.Context(), key, r.PathValue("system_id"), command, id)
	api.systemResponse(w, 200, record, err)
}

func (api *controlAPI) revoke(w http.ResponseWriter, r *http.Request) {
	var command state.RevokeToolCommand
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	if api.engine != nil {
		api.engine.mu.Lock()
		defer api.engine.mu.Unlock()
	}
	record, err := api.store.RevokeTool(r.Context(), api.cfg, key, r.PathValue("system_id"), command)
	if err == nil && api.engine != nil {
		for _, active := range api.engine.active {
			if active.systemID != record.ID {
				continue
			}
			if strings.HasPrefix(active.callID, "evaluation_") {
				active.cancel()
				continue
			}
			task, taskErr := api.engine.store.Task(r.Context(), record.ID, active.taskID)
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
		return "", state.Invalid("after", "invalid cursor")
	}
	return query.Get("after"), nil
}

func (api *controlAPI) directory(w http.ResponseWriter, r *http.Request) {
	after, err := runtimeCursor(r)
	if err != nil {
		api.failure(w, err)
		return
	}
	result, err := api.store.ListAgents(r.Context(), r.PathValue("system_id"), after)
	if err != nil {
		api.failure(w, err)
		return
	}
	api.respond(w, 200, result)
}

func (api *controlAPI) tasks(w http.ResponseWriter, r *http.Request) {
	after, err := runtimeCursor(r)
	if err != nil {
		api.failure(w, err)
		return
	}
	result, err := api.store.ListTasks(r.Context(), r.PathValue("system_id"), after)
	if err != nil {
		api.failure(w, err)
		return
	}
	api.respond(w, 200, result)
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
		api.failure(w, state.Invalid("after", "requires a nonnegative event sequence"))
		return
	}
	result, err := api.store.ListEvents(r.Context(), r.PathValue("system_id"), after)
	if err != nil {
		api.failure(w, err)
		return
	}
	api.respond(w, 200, result)
}

func (api *controlAPI) assignTools(w http.ResponseWriter, r *http.Request) {
	var command state.AssignToolsCommand
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	agentID := r.PathValue("agent_id")
	record, err := api.store.AssignTools(r.Context(), api.cfg, key, r.PathValue("system_id"), command, agentID)
	api.systemResponse(w, 200, record, err)
}

func (api *controlAPI) artifacts(w http.ResponseWriter, r *http.Request) {
	after, err := runtimeCursor(r)
	if err != nil {
		api.failure(w, err)
		return
	}
	result, err := api.store.ListArtifacts(r.Context(), r.PathValue("system_id"), after)
	if err != nil {
		api.failure(w, err)
		return
	}
	api.respond(w, 200, result)
}

func (api *controlAPI) toolCalls(w http.ResponseWriter, r *http.Request) {
	after, err := runtimeCursor(r)
	if err != nil {
		api.failure(w, err)
		return
	}
	result, err := api.store.ListToolCalls(r.Context(), r.PathValue("system_id"), after)
	if err != nil {
		api.failure(w, err)
		return
	}
	api.respond(w, 200, result)
}

func (api *controlAPI) artifact(w http.ResponseWriter, r *http.Request) {
	result, err := api.store.Artifact(r.Context(), r.PathValue("system_id"), r.PathValue("artifact_id"))
	if err != nil {
		api.failure(w, err)
		return
	}
	api.respond(w, 200, result)
}

func (api *controlAPI) input(w http.ResponseWriter, r *http.Request) {
	if !api.executionAvailable(w) {
		return
	}
	var command state.AddInputCommand
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	record, err := api.store.AddInput(r.Context(), key, r.PathValue("system_id"), command)
	if err == nil {
		api.engine.notify()
	}
	api.systemResponse(w, 202, record, err)
}
