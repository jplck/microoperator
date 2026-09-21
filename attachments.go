//go:build darwin || linux

package main

import (
	"net/http"
)

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
	record, err := api.store.AddAttachment(r.Context(), key, r.PathValue("system_id"), command)
	if err == nil {
		api.engine.notify()
	}
	api.systemResponse(w, 202, record, err)
}
