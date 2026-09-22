package protocol

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestMessageFraming(t *testing.T) {
	maxPayload := MaxFrame - len("{\"type\":\"ready\",\"data\":\"\"}\n")
	for _, data := range []string{"control.sock", "quotes \" and newline\n", strings.Repeat("x", maxPayload)} {
		t.Run("round-trip", func(t *testing.T) {
			var out bytes.Buffer
			value := Message{Type: "ready", Data: data}
			if err := WriteMessage(&out, value); err != nil {
				t.Fatal(err)
			}
			if len(data) == maxPayload && out.Len() != MaxFrame {
				t.Fatalf("boundary frame length = %d; want %d", out.Len(), MaxFrame)
			}
			reader := bufio.NewReaderSize(&out, MaxFrame+1)
			got, err := ReadMessage(reader)
			if err != nil || got != value {
				t.Fatalf("decoded message = %+v, %v", got, err)
			}
			if _, err := ReadMessage(reader); !errors.Is(err, io.EOF) {
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
		"oversized":       `{"type":"ready","data":"` + strings.Repeat("x", MaxFrame) + "\"}\n",
	} {
		t.Run(name, func(t *testing.T) {
			reader := bufio.NewReaderSize(strings.NewReader(frame), MaxFrame+1)
			if _, err := ReadMessage(reader); err == nil {
				t.Fatal("invalid message accepted")
			}
		})
	}
}
