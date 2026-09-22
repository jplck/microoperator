//go:build linux

package daemon

import (
	"context"
	"time"

	"github.com/jplck/microoperator/internal/state"
)

func (engine *executionEngine) notify() {
	if engine.wake != nil {
		select {
		case engine.wake <- struct{}{}:
		default:
		}
	}
}

func (engine *executionEngine) schedule() {
	defer close(engine.schedulerDone)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-engine.ctx.Done():
			return
		case <-engine.wake:
		case <-tick.C:
		}
		engine.mu.Lock()
		if engine.ctx.Err() != nil {
			engine.mu.Unlock()
			return
		}
		for len(engine.active) < state.MaxOperators && engine.broker.available() {
			record, e, found, err := engine.claim(engine.ctx, time.Now())
			if err != nil {
				if engine.ctx.Err() == nil {
					engine.broker.mu.Lock()
					engine.broker.failLocked(err)
					engine.broker.mu.Unlock()
				}
				break
			}
			if !found {
				break
			}
			ctx, cancel := context.WithDeadline(engine.ctx, time.UnixMilli(e.Deadline))
			engine.active[e.AgentID] = activation{cancel: cancel, callID: e.CallID, systemID: e.SystemID, goalID: e.GoalID, agentID: e.AgentID, eventID: e.EventID, taskID: e.TaskID}
			engine.wg.Add(1)
			go engine.execute(ctx, record, e)
		}
		engine.mu.Unlock()
	}
}
