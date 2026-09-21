//go:build darwin || linux

package main

import (
	"context"
	"time"
)

func (engine *executionEngine) executeLearning(ctx context.Context, record systemRecord, session executionRecord) {
	evidence, binary, runErr := engine.evaluateGenerated(ctx, session)
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if active, ok := engine.active[session.AgentID]; ok {
		active.cancel()
		delete(engine.active, session.AgentID)
	}
	finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runErr, err := engine.store.CompleteEvaluation(finishCtx, engine.cfg, engine.owner, session, evidence, binary, runErr)
	if err != nil {
		engine.broker.mu.Lock()
		engine.broker.failLocked(err)
		engine.broker.mu.Unlock()
	}
	if runErr != nil {
		engine.logger.Printf("system %s evaluation %s: %v", record.ID, session.LearningID, runErr)
	}
	engine.notify()
}

func (engine *executionEngine) evaluateGenerated(ctx context.Context, session executionRecord) (evidence []learningEvidence, binary []byte, err error) {
	plan, err := engine.store.PrepareEvaluation(ctx, session)
	if err != nil {
		return nil, nil, err
	}
	e := plan.Evaluation
	if engine.cfg.Learning == nil || engine.cfg.Learning.ToolchainDigest != e.ToolchainDigest {
		return nil, nil, invalid("toolchain", "evaluation toolchain changed")
	}
	profile := engine.cfg.SandboxProfiles[plan.ProfileName]
	binary, err = buildGenerated(ctx, engine.executable, engine.cfg.DataDir, session.SystemID, e.Draft.Content, *engine.cfg.Learning, profile)
	if err != nil {
		return nil, nil, err
	}
	var baselineBinary []byte
	if e.Baseline.Tool != nil {
		baselineBinary, err = readGeneratedArtifact(engine.cfg.DataDir, session.SystemID, plan.BaselineTool.BinaryDigest)
		if err != nil {
			return nil, nil, err
		}
	}
	for _, check := range e.Cases {
		value := learningEvidence{Input: check.Input, Expected: check.Expected, Baseline: check.Input}
		if baselineBinary != nil {
			value.Baseline, err = runGenerated(ctx, engine.executable, engine.cfg.DataDir, session.SystemID, baselineBinary, check.Input, profile)
			if err != nil {
				return evidence, nil, err
			}
		}
		value.Candidate, err = runGenerated(ctx, engine.executable, engine.cfg.DataDir, session.SystemID, binary, check.Input, profile)
		value.BaselinePassed = value.Baseline == check.Expected
		value.CandidatePassed = err == nil && value.Candidate == check.Expected
		evidence = append(evidence, value)
		if err != nil {
			return evidence, nil, err
		}
	}
	return evidence, binary, nil
}
