//go:build darwin || linux

package main

import (
	"context"
	"time"
)

func (api *controlAPI) stateOwner() string {
	if api.engine == nil {
		return ""
	}
	return api.engine.owner
}

func (engine *executionEngine) claim(ctx context.Context, now time.Time) (systemRecord, executionRecord, bool, error) {
	active := make(map[string]string, len(engine.active))
	for id, activation := range engine.active {
		active[id] = activation.systemID
	}
	return engine.store.Claim(ctx, engine.cfg, engine.owner, active, now)
}

func (engine *executionEngine) finishDelivery(ctx context.Context, session executionRecord, workerErr error, canceled bool) error {
	shuttingDown := engine.ctx != nil && engine.ctx.Err() != nil
	return engine.store.FinishDelivery(ctx, engine.cfg, engine.owner, session, workerErr, canceled, shuttingDown)
}

func (engine *executionEngine) taskSnapshot(ctx context.Context, systemID, taskID string) (taskRecord, error) {
	return engine.store.Task(ctx, systemID, taskID)
}
