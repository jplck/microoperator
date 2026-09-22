package protocol

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const MaxFrame = 64 * 1024

type Message struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
	ID   string `json:"id,omitempty"`
}

func ReadMessage(reader *bufio.Reader) (Message, error) {
	var value Message
	frame, err := reader.ReadSlice('\n')
	if err != nil {
		return value, fmt.Errorf("read JSON frame (limit %d bytes): %w", MaxFrame, err)
	}
	if len(frame) > MaxFrame {
		return value, errors.New("JSON frame exceeds size limit")
	}
	if err := DecodeJSON(frame, &value); err != nil {
		return value, fmt.Errorf("decode JSON frame: %w", err)
	}
	if value.Type == "" {
		return value, errors.New("JSON frame requires a type")
	}
	return value, nil
}

func CheckFrame(value Message) error { return WriteMessage(io.Discard, value) }

func WriteMessage(writer io.Writer, value Message) error {
	frame, err := json.Marshal(value)
	if err != nil {
		return err
	}
	frame = append(frame, '\n')
	if len(frame) > MaxFrame {
		return errors.New("JSON frame exceeds size limit")
	}
	n, err := writer.Write(frame)
	if err == nil && n != len(frame) {
		err = io.ErrShortWrite
	}
	return err
}
