//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jplck/microoperator/internal/daemon"
	"github.com/jplck/microoperator/internal/protocol"
	"github.com/jplck/microoperator/internal/sandbox"
	"github.com/jplck/microoperator/internal/state"
	webui "github.com/jplck/microoperator/internal/ui"
	"github.com/jplck/microoperator/internal/worker"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	const usage = "usage: microoperator daemon --config PATH | ui --socket PATH [--listen 127.0.0.1:8080] | toolchain-digest ABSOLUTE_GOROOT"
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "toolchain-digest":
		if len(args) != 2 {
			return errors.New("usage: microoperator toolchain-digest ABSOLUTE_GOROOT")
		}
		digest, err := sandbox.ToolchainDigest(ctx, args[1])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(os.Stdout, digest)
		return err
	case "ui":
		flags := flag.NewFlagSet("microoperator ui", flag.ContinueOnError)
		socket := flags.String("socket", "", "daemon Unix socket")
		listen := flags.String("listen", "127.0.0.1:8080", "loopback HTTP address")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *socket == "" {
			return errors.New("ui requires --socket PATH and no positional arguments")
		}
		return webui.Run(ctx, *socket, *listen, os.Stdout, os.Stderr)
	case "daemon":
		flags := flag.NewFlagSet("microoperator daemon", flag.ContinueOnError)
		config := flags.String("config", "", "user-owned JSON configuration")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *config == "" {
			return errors.New("daemon requires --config PATH and no positional arguments")
		}
		return daemon.Run(ctx, *config, os.Stdout, os.Stderr)
	case "sandbox-exec":
		if len(args) < 3 {
			return errors.New("internal usage: sandbox-exec WORKSPACE EXECUTABLE [ARGS...]")
		}
		return sandbox.Exec(args[1], args[2], args[3:])
	case "sandbox-exec-profile", "sandbox-build-profile":
		if len(args) < 4 || len(args[3]) > protocol.MaxFrame {
			return errors.New("internal usage: sandbox-exec-profile WORKSPACE EXECUTABLE PROFILE [ARGS...]")
		}
		var profile state.SandboxConfig
		if err := protocol.DecodeJSON([]byte(args[3]), &profile); err != nil {
			return err
		}
		if args[0] == "sandbox-build-profile" {
			if len(args) < 5 || profile.Resources == nil {
				return errors.New("builder requires a resource profile and pinned toolchain")
			}
			profile.Toolchain = args[4]
			args = append(args[:4:4], args[5:]...)
		}
		return sandbox.Exec(args[1], args[2], args[4:], profile)
	case "worker":
		if len(args) != 2 || args[1] != "operator" {
			return errors.New(usage)
		}
		return worker.Operator(os.Stdin, os.Stdout)
	case "tool":
		if len(args) != 2 || args[1] != "text-analyze" {
			return errors.New(usage)
		}
		return worker.TextAnalyze(os.Stdin, os.Stdout)
	default:
		return errors.New(usage)
	}
}
