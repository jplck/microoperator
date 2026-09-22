//go:build darwin || linux

package sandbox

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jplck/microoperator/internal/state"
)

func TestResourceValidationAndNoFallback(t *testing.T) {
	profile := state.SandboxConfig{Read: []string{"inputs"}, ReadWrite: []string{"scratch", "output"}, Network: "blocked",
		Resources: &state.ResourceLimits{MemoryBytes: 128 << 20, WorkspaceBytes: 8 << 20, Processes: 64, CPUPercent: 50}}
	if err := state.ValidateSandbox("worker", profile); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*state.ResourceLimits){
		func(l *state.ResourceLimits) { l.MemoryBytes = 1 }, func(l *state.ResourceLimits) { l.WorkspaceBytes = 0 },
		func(l *state.ResourceLimits) { l.WorkspaceBytes = l.MemoryBytes + 1 }, func(l *state.ResourceLimits) { l.Processes = 1 },
		func(l *state.ResourceLimits) { l.CPUPercent = 0 },
	} {
		bad := profile
		limits := *profile.Resources
		bad.Resources = &limits
		change(bad.Resources)
		if err := state.ValidateSandbox("worker", bad); err == nil {
			t.Fatalf("invalid limits accepted: %+v", bad.Resources)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	called := false
	if err := Supervise(ctx, "unused", t.TempDir(), "unused", nil, func(io.Writer, io.Reader) error { called = true; return nil }, profile); err == nil || called {
		t.Fatal("missing resource enforcement fell back to ordinary execution")
	}
}

func TestAbandonedWorkspaceCleanupIsScoped(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	name := "activation-sys_" + strings.Repeat("a", 32) + "-12345"
	old := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(old, "output"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(old, "output", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "unrelated"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := CleanupWorkspaces(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("abandoned workspace not removed")
	}
	for _, path := range []string{filepath.Join(outside, "keep"), filepath.Join(root, "unrelated")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("cleanup crossed owned workspace: %v", err)
		}
	}
	if err := os.Symlink(outside, old); err != nil {
		t.Fatal(err)
	}
	if err := CleanupWorkspaces(root); err == nil {
		t.Fatal("workspace symlink accepted")
	}
}
