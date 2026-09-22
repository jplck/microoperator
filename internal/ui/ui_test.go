//go:build darwin || linux

package ui

import (
	"context"
	"io"
	"testing"

	"github.com/jplck/microoperator/internal/protocol"
)

func TestUIRejectsNonLoopbackAndSharedCredential(t *testing.T) {
	const fixtureControlToken = "fixture-control-token-not-real-0123456789"
	if err := Run(context.Background(), "unused", "0.0.0.0:0", io.Discard, io.Discard); err == nil {
		t.Fatal("UI bound a public address")
	}
	t.Setenv(protocol.ControlTokenEnv, fixtureControlToken)
	t.Setenv(TokenEnv, fixtureControlToken)
	if err := Run(context.Background(), "unused", "127.0.0.1:0", io.Discard, io.Discard); err == nil {
		t.Fatal("daemon credential was accepted as browser password")
	}
}
