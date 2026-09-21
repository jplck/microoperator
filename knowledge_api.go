//go:build darwin || linux

package main

import (
	"net/http"
	"net/url"
	"strconv"
	"time"
)

func (api *controlAPI) registerKnowledgeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/systems/{system_id}/memory", api.memory)
	mux.HandleFunc("POST /v1/systems/{system_id}/memory", api.memory)
	mux.HandleFunc("POST /v1/systems/{system_id}/memory/{memory_id}/{action}", api.memoryState)
	mux.HandleFunc("GET /v1/systems/{system_id}/schedules", api.schedules)
	mux.HandleFunc("POST /v1/systems/{system_id}/schedules", api.schedules)
	mux.HandleFunc("POST /v1/systems/{system_id}/schedules/{schedule_id}/cancel", api.cancelSchedule)
	mux.HandleFunc("GET /v1/systems/{system_id}/subscriptions", api.subscriptions)
	mux.HandleFunc("POST /v1/systems/{system_id}/subscriptions", api.subscriptions)
	mux.HandleFunc("POST /v1/systems/{system_id}/subscriptions/{subscription_id}/cancel", api.cancelSubscription)
}

func (api *controlAPI) memory(w http.ResponseWriter, r *http.Request) {
	if !api.executionAvailable(w) {
		return
	}
	id := r.PathValue("system_id")
	if r.Method == "POST" {
		var command memoryCommand
		key, err := readCommand(w, r, &command)
		if err != nil {
			api.failure(w, err)
			return
		}
		record, err := api.store.PutMemory(r.Context(), api.cfg, api.stateOwner(), key, id, command)
		if err == nil {
			api.engine.notify()
		}
		api.systemResponse(w, 201, record, err)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		api.failure(w, invalid("query", "invalid memory query"))
		return
	}
	for key, values := range query {
		if len(values) != 1 || (key != "scope" && key != "owner_id" && key != "query" && key != "after" && key != "include_pending") {
			api.failure(w, invalid("query", "unknown or repeated memory query field"))
			return
		}
	}
	search := memoryQuery{Scope: query.Get("scope"), Owner: query.Get("owner_id"), Query: query.Get("query"), After: query.Get("after")}
	if search.Scope == "" {
		search.Scope = "system"
	}
	if value := query.Get("include_pending"); value != "" {
		search.Pending, err = strconv.ParseBool(value)
		if err != nil {
			api.failure(w, invalid("include_pending", "requires a boolean"))
			return
		}
	}
	result, err := api.store.SearchMemory(r.Context(), id, search, time.Now())
	if err != nil {
		api.failure(w, err)
		return
	}
	api.respond(w, 200, result)
}

func (api *controlAPI) memoryState(w http.ResponseWriter, r *http.Request) {
	if !api.executionAvailable(w) {
		return
	}
	action := r.PathValue("action")
	if action != "approve" && action != "delete" {
		api.failure(w, errSystemNotFound)
		return
	}
	var command ChangeMemoryStateCommand
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	id := r.PathValue("memory_id")
	record, err := api.store.ChangeMemoryState(r.Context(), api.cfg, api.stateOwner(), key, r.PathValue("system_id"), command, id, action)
	if err == nil {
		api.engine.notify()
	}
	api.systemResponse(w, 200, record, err)
}

func (api *controlAPI) schedules(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		api.triggerList(w, r, true)
		return
	}
	if !api.executionAvailable(w) {
		return
	}
	var command scheduleCommand
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	record, err := api.store.CreateSchedule(r.Context(), api.cfg, key, r.PathValue("system_id"), command)
	if err == nil {
		api.engine.notify()
	}
	api.systemResponse(w, 201, record, err)
}

func (api *controlAPI) subscriptions(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		api.triggerList(w, r, false)
		return
	}
	if !api.executionAvailable(w) {
		return
	}
	var command subscriptionCommand
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	record, err := api.store.CreateSubscription(r.Context(), api.cfg, key, r.PathValue("system_id"), command)
	if err == nil {
		api.engine.notify()
	}
	api.systemResponse(w, 201, record, err)
}

func (api *controlAPI) cancelSchedule(w http.ResponseWriter, r *http.Request) {
	api.cancelTrigger(w, r, true)
}
func (api *controlAPI) cancelSubscription(w http.ResponseWriter, r *http.Request) {
	api.cancelTrigger(w, r, false)
}

func (api *controlAPI) cancelTrigger(w http.ResponseWriter, r *http.Request, schedule bool) {
	var command struct{}
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	id := r.PathValue("subscription_id")
	if schedule {
		id = r.PathValue("schedule_id")
	}
	record, err := api.store.CancelTrigger(r.Context(), key, r.PathValue("system_id"), id, schedule)
	api.systemResponse(w, 200, record, err)
}

func (api *controlAPI) triggerList(w http.ResponseWriter, r *http.Request, schedule bool) {
	after, err := runtimeCursor(r)
	if err != nil {
		api.failure(w, err)
		return
	}
	result, err := api.store.ListTriggers(r.Context(), r.PathValue("system_id"), after, schedule)
	if err != nil {
		api.failure(w, err)
		return
	}
	api.respond(w, 200, result)
}
