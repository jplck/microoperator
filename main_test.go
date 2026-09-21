//go:build darwin || linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestMessageFraming(t *testing.T) {
	maxPayload := maxFrame - len("{\"type\":\"ready\",\"data\":\"\"}\n")
	for _, data := range []string{"control.sock", "quotes \" and newline\n", strings.Repeat("x", maxPayload)} {
		t.Run("round-trip", func(t *testing.T) {
			var out bytes.Buffer
			value := message{Type: "ready", Data: data}
			if err := writeMessage(&out, value); err != nil {
				t.Fatal(err)
			}
			if len(data) == maxPayload && out.Len() != maxFrame {
				t.Fatalf("boundary frame length = %d; want %d", out.Len(), maxFrame)
			}
			reader := bufio.NewReaderSize(&out, maxFrame+1)
			got, err := readMessage(reader)
			if err != nil || got != value {
				t.Fatalf("decoded message = %+v, %v", got, err)
			}
			if _, err := readMessage(reader); !errors.Is(err, io.EOF) {
				t.Fatalf("expected EOF, got %v", err)
			}
		})
	}
	for name, frame := range map[string]string{
		"unknown field":   `{"type":"ready","command":"ignored"}` + "\n",
		"trailing object": `{"type":"ready"} {}` + "\n",
		"empty type":      `{"data":"control.sock"}` + "\n",
		"null":            "null\n",
		"wrong type":      `{"type":42}` + "\n",
		"malformed":       "{\n",
		"unterminated":    `{"type":"ready"}`,
		"oversized":       `{"type":"ready","data":"` + strings.Repeat("x", maxFrame) + "\"}\n",
	} {
		t.Run(name, func(t *testing.T) {
			reader := bufio.NewReaderSize(strings.NewReader(frame), maxFrame+1)
			if _, err := readMessage(reader); err == nil {
				t.Fatal("invalid message accepted")
			}
		})
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestBoundsAndLaunchValidation(t *testing.T) {
	if err := writeMessage(shortWriter{}, message{Type: "ready"}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write = %v", err)
	}
	var out bytes.Buffer
	oversizedPayload := maxFrame - len("{\"type\":\"ready\",\"data\":\"\"}\n") + 1
	if err := writeMessage(&out, message{Type: "ready", Data: strings.Repeat("x", oversizedPayload)}); err == nil || out.Len() != 0 {
		t.Fatal("oversized frame was written")
	}
	var stderr boundedStderr
	if _, err := stderr.Write(bytes.Repeat([]byte("x"), maxFrame)); err != nil {
		t.Fatal(err)
	}
	if _, err := stderr.Write([]byte("overflow")); err == nil || stderr.Len() != maxFrame {
		t.Fatal("stderr limit not enforced")
	}
	if err := supervise(context.Background(), "", "", "", nil, nil); err == nil {
		t.Fatal("supervision without a deadline accepted")
	}
	for _, args := range [][]string{
		nil, {"run"}, {"run", "-message", "legacy"}, {"worker"}, {"sandbox-exec"}, {"unknown"},
		{"daemon"}, {"daemon", "--config", ""}, {"daemon", "--config", "unused.json", "unexpected"},
	} {
		if err := run(context.Background(), args); err == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
	env := workerEnv("/private/work")
	if len(env) != 4 || strings.Join(env, "\n") != "HOME=/private/work/scratch\nTMPDIR=/private/work/scratch\nLANG=C\nTZ=UTC" {
		t.Fatalf("unexpected worker environment: %v", env)
	}
}
