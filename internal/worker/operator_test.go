//go:build linux

package worker

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/jplck/microoperator/internal/protocol"
)

func TestOperatorProtocolCorrelationAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name, kind, id string
		success        bool
	}{
		{"completed", "model.result", "call-fixture", true},
		{"foreign correlation", "model.result", "another-call", false},
		{"provider error", "model.error", "call-fixture", false},
		{"unsupported operation", "tool.call", "call-fixture", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var in, out bytes.Buffer
			for _, frame := range []protocol.Message{
				{Type: "task", ID: "call-fixture", Data: "a real model goal"},
				{Type: tc.kind, ID: tc.id, Data: "bounded response"},
			} {
				if err := protocol.WriteMessage(&in, frame); err != nil {
					t.Fatal(err)
				}
			}
			err := Operator(&in, &out)
			if (err == nil) != tc.success {
				t.Fatalf("worker success=%v, err=%v", tc.success, err)
			}
			reader := bufio.NewReaderSize(&out, protocol.MaxFrame+1)
			ready, err := protocol.ReadMessage(reader)
			if err != nil || ready != (protocol.Message{Type: "ready"}) {
				t.Fatal("missing readiness")
			}
			call, err := protocol.ReadMessage(reader)
			if err != nil || call != (protocol.Message{Type: "model.call", ID: "call-fixture", Data: "a real model goal"}) {
				t.Fatalf("unscoped model request: %+v %v", call, err)
			}
			if tc.success || tc.kind == "model.error" {
				want := "task.complete"
				if !tc.success {
					want = "task.failed"
				}
				ack, err := protocol.ReadMessage(reader)
				if err != nil || ack != (protocol.Message{Type: want, ID: "call-fixture"}) {
					t.Fatalf("ack: %+v %v", ack, err)
				}
			}
			if _, err := protocol.ReadMessage(reader); !errors.Is(err, io.EOF) {
				t.Fatal("unexpected extra worker operation")
			}
		})
	}
}
