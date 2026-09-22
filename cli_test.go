//go:build darwin || linux

package main

import (
	"context"
	"testing"
)

func TestCommandLineValidation(t *testing.T) {
	for _, args := range [][]string{
		nil, {"run"}, {"run", "-message", "legacy"}, {"worker"}, {"sandbox-exec"}, {"unknown"},
		{"daemon"}, {"daemon", "--config", ""}, {"daemon", "--config", "unused.json", "unexpected"},
	} {
		if err := run(context.Background(), args); err == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
}
