//go:build linux

package worker

import (
	"bufio"
	"errors"
	"io"

	"github.com/jplck/microoperator/internal/protocol"
)

// Operator is deliberately a single model turn, not an interpreter for
// returned text. It never loads credentials, opens the database, or calls HTTP.
// The parent binds this private pipe to a persisted activation before sending
// task data; the ID only correlates messages and cannot select another identity.
func Operator(in io.Reader, out io.Writer) error {
	reader := bufio.NewReaderSize(in, protocol.MaxFrame+1)
	if err := protocol.WriteMessage(out, protocol.Message{Type: "ready"}); err != nil {
		return err
	}
	task, err := protocol.ReadMessage(reader)
	if err != nil {
		return err
	}
	if task.Type != "task" || task.ID == "" || task.Data == "" {
		return errors.New("invalid operator task")
	}
	if err := protocol.WriteMessage(out, protocol.Message{Type: "model.call", ID: task.ID, Data: task.Data}); err != nil {
		return err
	}
	reply, err := protocol.ReadMessage(reader)
	if err != nil {
		return err
	}
	if reply.ID != task.ID {
		return errors.New("model response correlation mismatch")
	}
	switch reply.Type {
	case "model.error":
		if err := protocol.WriteMessage(out, protocol.Message{Type: "task.failed", ID: task.ID}); err != nil {
			return err
		}
		return errors.New("model call failed")
	case "model.action":
		if err := protocol.WriteMessage(out, protocol.Message{Type: "tool.call", ID: task.ID, Data: reply.Data}); err != nil {
			return err
		}
		result, err := protocol.ReadMessage(reader)
		if err != nil {
			return err
		}
		if result.ID != task.ID {
			return errors.New("tool response correlation mismatch")
		}
		if result.Type == "tool.error" {
			return errors.New("tool operation denied or failed")
		}
		kind := "task.continue"
		if result.Type == "tool.wait" {
			kind = "task.waiting"
		} else if result.Type != "tool.result" {
			return errors.New("invalid tool response")
		}
		return protocol.WriteMessage(out, protocol.Message{Type: kind, ID: task.ID})
	case "model.result":
		return protocol.WriteMessage(out, protocol.Message{Type: "task.complete", ID: task.ID})
	default:
		return errors.New("unexpected model response")
	}
}
