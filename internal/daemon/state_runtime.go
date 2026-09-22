//go:build linux

package daemon

import (
	"context"
	"time"

	"github.com/jplck/microoperator/internal/state"
)

func (api *controlAPI) stateOwner() string {
	if api.engine == nil {
		return ""
	}
	return api.engine.owner
}

func (engine *executionEngine) claim(ctx context.Context, now time.Time) (state.SystemRecord, state.ExecutionRecord, bool, error) {
	active := make(map[string]string, len(engine.active))
	for id, activation := range engine.active {
		active[id] = activation.systemID
	}
	return engine.store.Claim(ctx, engine.cfg, engine.owner, active, now)
}

func (engine *executionEngine) finishDelivery(ctx context.Context, session state.ExecutionRecord, workerErr error, canceled bool) error {
	shuttingDown := engine.ctx != nil && engine.ctx.Err() != nil
	return engine.store.FinishDelivery(ctx, engine.cfg, engine.owner, session, workerErr, canceled, shuttingDown)
}

func (engine *executionEngine) taskSnapshot(ctx context.Context, systemID, taskID string) (state.TaskRecord, error) {
	return engine.store.Task(ctx, systemID, taskID)
}
