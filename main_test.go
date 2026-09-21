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

func TestWorkerProtocol(t *testing.T) {
	for _, data := range []string{"ping", "quotes \" and newline\n", strings.Repeat("x", maxFrame-26)} {
		t.Run("round-trip", func(t *testing.T) {
			var in, out bytes.Buffer
			if err := writeMessage(&in, message{Type: "ping", Data: data}); err != nil {
				t.Fatal(err)
			}
			if err := worker(&in, &out); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReaderSize(&out, maxFrame+1)
			ready, err := readMessage(reader)
			if err != nil || ready.Type != "ready" {
				t.Fatalf("readiness = %+v, %v", ready, err)
			}
			reply, err := readMessage(reader)
			if err != nil || reply != (message{Type: "pong", Data: data}) {
				t.Fatalf("reply = %+v, %v", reply, err)
			}
			if _, err := readMessage(reader); !errors.Is(err, io.EOF) {
				t.Fatalf("expected EOF, got %v", err)
			}
		})
	}
	for name, frame := range map[string]string{
		"unknown operation": `{"type":"exec"}` + "\n",
		"unknown field":     `{"type":"ping","command":"ignored"}` + "\n",
		"trailing object":   `{"type":"ping"} {}` + "\n",
		"empty type":        `{"data":"ping"}` + "\n",
		"null":              "null\n",
		"wrong type":        `{"type":42}` + "\n",
		"malformed":         "{\n",
		"unterminated":      `{"type":"ping"}`,
		"oversized":         `{"type":"ping","data":"` + strings.Repeat("x", maxFrame) + "\"}\n",
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if err := worker(strings.NewReader(frame), &out); err == nil {
				t.Fatal("invalid request accepted")
			}
			reader := bufio.NewReaderSize(&out, maxFrame+1)
			if _, err := readMessage(reader); err != nil {
				t.Fatal(err)
			}
			if _, err := readMessage(reader); !errors.Is(err, io.EOF) {
				t.Fatalf("invalid request produced a reply: %v", err)
			}
		})
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestBoundsAndLaunchValidation(t *testing.T) {
	if err := writeMessage(shortWriter{}, message{Type: "ping"}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write = %v", err)
	}
	var out bytes.Buffer
	if err := writeMessage(&out, message{Type: "ping", Data: strings.Repeat("x", maxFrame)}); err == nil || out.Len() != 0 {
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
		{"run", "-timeout", "0s"}, {"run", "unexpected"}, {"sandbox-exec"}, {"worker", "unexpected"}, {"unknown"},
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
