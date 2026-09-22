package state

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

func TestRuntimeMetadataDoesNotChangePersistedJSON(t *testing.T) {
	cfg := fixtureConfiguration(t)
	before, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeDigest = "runtime-only"
	profile := cfg.SandboxProfiles["worker"]
	profile.ResourceRoot, profile.Toolchain = "/runtime/cgroup", "/runtime/toolchain"
	cfg.SandboxProfiles["worker"] = profile
	after, err := json.Marshal(cfg)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("runtime metadata changed configuration encoding: %s, %v", after, err)
	}
	record := SystemRecord{}
	before, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	record.LocalTools = map[string]ToolConfig{"local.fixture": {Content: "runtime-only"}}
	after, err = json.Marshal(record)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("resolved local tools leaked into the response/receipt: %s, %v", after, err)
	}
}

func TestWorkflowInputsRejectInvalidCommandsBeforeWrites(t *testing.T) {
	cfg := fixtureConfiguration(t)
	store, configID := fixtureStore(t, cfg)
	record, err := store.CreateSystem(context.Background(), localAdministrator, "create", CreateSystemCommand{Name: "research", Goal: fixtureGoal()}, cfg, configID)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, run := range map[string]func() error{
		"memory action": func() error {
			_, err := store.ChangeMemoryState(ctx, cfg, "owner", "bad-memory", record.ID, ChangeMemoryStateCommand{Revision: 1}, "memory", "unknown")
			return err
		},
		"draft state": func() error {
			_, err := store.ChangeDraftState(ctx, "bad-draft", record.ID, ChangeDraftStateCommand{Version: 1, State: "active"}, "local.fixture")
			return err
		},
		"control scope": func() error {
			_, err := store.Control(ctx, cfg, "bad-control", record.ID, "unknown", "stop", record.ID)
			return err
		},
		"checks": func() error {
			_, err := store.CreateChecks(ctx, "bad-checks", record.ID, CreateChecksCommand{})
			return err
		},
		"input": func() error { _, err := store.AddInput(ctx, "bad-input", record.ID, AddInputCommand{}); return err },
		"attachment": func() error {
			_, err := store.AddAttachment(ctx, "bad-attachment", record.ID, AttachmentCommand{Name: "../escape", Content: "text"})
			return err
		},
		"feedback": func() error {
			_, err := store.AddFeedback(ctx, "bad-feedback", record.ID, AddFeedbackCommand{})
			return err
		},
		"approval": func() error {
			_, err := store.ApproveLearning(ctx, cfg, "bad-approval", record.ID, ApproveLearningCommand{}, "evaluation")
			return err
		},
		"tool completion": func() error {
			_, err := store.CompleteTool(ctx, PreparedTool{}, TextResult{}, nil, "", nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(); err == nil {
				t.Fatal("invalid command succeeded")
			}
		})
	}
	assertCount(t, store, "command_receipts", 1)
	assertCount(t, store, "audit", 1)
}
