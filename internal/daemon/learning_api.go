//go:build darwin || linux

package daemon

import (
	"net/http"

	"github.com/jplck/microoperator/internal/state"
)

func (api *controlAPI) registerLearningRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/systems/{system_id}/learning", api.learningView)
	mux.HandleFunc("GET /v1/systems/{system_id}/learning/{section}", api.learningView)
	mux.HandleFunc("POST /v1/systems/{system_id}/learning/checks", api.learningChecks)
	mux.HandleFunc("POST /v1/systems/{system_id}/learning/feedback", api.learningFeedback)
	mux.HandleFunc("POST /v1/systems/{system_id}/learning/evaluate", api.learningEvaluate)
	mux.HandleFunc("POST /v1/systems/{system_id}/learning/{evaluation_id}/approve", api.learningApprove)
}

func (api *controlAPI) learningChecks(w http.ResponseWriter, r *http.Request) {
	var command state.CreateChecksCommand
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	record, err := api.store.CreateChecks(r.Context(), key, r.PathValue("system_id"), command)
	api.systemResponse(w, 201, record, err)
}

func (api *controlAPI) learningFeedback(w http.ResponseWriter, r *http.Request) {
	var command state.AddFeedbackCommand
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	record, err := api.store.AddFeedback(r.Context(), key, r.PathValue("system_id"), command)
	api.systemResponse(w, 201, record, err)
}

func (api *controlAPI) learningEvaluate(w http.ResponseWriter, r *http.Request) {
	if !api.executionAvailable(w) {
		return
	}
	var command state.EvaluateCommand
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	record, err := api.store.EvaluateLearning(r.Context(), api.cfg, api.stateOwner(), key, r.PathValue("system_id"), command)
	if err == nil {
		api.engine.notify()
	}
	api.systemResponse(w, 202, record, err)
}

func (api *controlAPI) learningApprove(w http.ResponseWriter, r *http.Request) {
	var command state.ApproveLearningCommand
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	id := r.PathValue("evaluation_id")
	record, err := api.store.ApproveLearning(r.Context(), api.cfg, key, r.PathValue("system_id"), command, id)
	api.systemResponse(w, 200, record, err)
}

func (api *controlAPI) learningView(w http.ResponseWriter, r *http.Request) {
	after, err := runtimeCursor(r)
	if err != nil {
		api.failure(w, err)
		return
	}
	result, err := api.store.LearningView(r.Context(), r.PathValue("system_id"), after, r.PathValue("section"))
	if err != nil {
		api.failure(w, err)
		return
	}
	api.respond(w, 200, result)
}
