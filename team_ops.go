//go:build darwin || linux

package main

import (
	"context"
	"time"
)

// The store commits the dispatch receipt before any subprocess starts. Runtime
// owns execution; completion records known results or an unretryable unknown.
func (engine *executionEngine) invokeTool(ctx context.Context, session executionRecord, action modelToolCall) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	plan, err := engine.store.PrepareTool(ctx, engine.cfg, engine.owner, session, action)
	if err != nil || !plan.Dispatch {
		return plan.Result, err
	}
	var output textResult
	var artifact []byte
	var generatedOutput string
	var runErr error
	if plan.Generated {
		generatedOutput, runErr = runGenerated(ctx, engine.executable, engine.cfg.DataDir, session.SystemID, plan.Binary, plan.Arguments.Text, plan.Profile)
	} else {
		output, artifact, runErr = engine.runTextTool(ctx, session, plan.Arguments, plan.Profile)
	}
	finishCtx, finishCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer finishCancel()
	return engine.store.CompleteTool(finishCtx, plan, output, artifact, generatedOutput, runErr)
}
