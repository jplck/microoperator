//go:build linux

package main

import (
	"context"
	"go/build"
	"testing"
)

func TestCLIRequiresLinux(t *testing.T) {
	for _, platform := range []string{"linux", "darwin", "windows", "freebsd"} {
		t.Run(platform, func(t *testing.T) {
			buildContext := build.Default
			buildContext.GOOS = platform
			included, err := buildContext.MatchFile(".", "main.go")
			if err != nil {
				t.Fatal(err)
			}
			if included != (platform == "linux") {
				t.Fatalf("CLI selected for %s: %v", platform, included)
			}
		})
	}
}

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
