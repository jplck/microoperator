//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/jplck/microoperator/internal/protocol"
)

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestBoundsAndLaunchValidation(t *testing.T) {
	if err := protocol.WriteMessage(shortWriter{}, protocol.Message{Type: "ready"}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write = %v", err)
	}
	var out bytes.Buffer
	oversizedPayload := protocol.MaxFrame - len("{\"type\":\"ready\",\"data\":\"\"}\n") + 1
	if err := protocol.WriteMessage(&out, protocol.Message{Type: "ready", Data: strings.Repeat("x", oversizedPayload)}); err == nil || out.Len() != 0 {
		t.Fatal("oversized frame was written")
	}
	var stderr BoundedStderr
	if _, err := stderr.Write(bytes.Repeat([]byte("x"), protocol.MaxFrame)); err != nil {
		t.Fatal(err)
	}
	if _, err := stderr.Write([]byte("overflow")); err == nil || stderr.Len() != protocol.MaxFrame {
		t.Fatal("stderr limit not enforced")
	}
	if err := Supervise(context.Background(), "", "", "", nil, nil); err == nil {
		t.Fatal("supervision without a deadline accepted")
	}
	env := Environment("/private/work")
	if len(env) != 4 || strings.Join(env, "\n") != "HOME=/private/work/scratch\nTMPDIR=/private/work/scratch\nLANG=C\nTZ=UTC" {
		t.Fatalf("unexpected worker environment: %v", env)
	}
}
