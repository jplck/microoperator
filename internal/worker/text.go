//go:build darwin || linux

package worker

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/jplck/microoperator/internal/protocol"
	"github.com/jplck/microoperator/internal/state"
)

// TextAnalyze is fixed Go code. Agent-authored source never reaches this
// entry point; it has no shell, network, arbitrary path, or code-loading option.
func TextAnalyze(in io.Reader, out io.Writer) error {
	if err := protocol.WriteMessage(out, protocol.Message{Type: "ready"}); err != nil {
		return err
	}
	request, err := protocol.ReadMessage(bufio.NewReaderSize(in, protocol.MaxFrame+1))
	if err != nil {
		return err
	}
	var args state.TextArguments
	if request.Type != "tool.input" || protocol.DecodeJSON([]byte(request.Data), &args) != nil || len(args.Text) > 4096 {
		return errors.New("invalid text analysis arguments")
	}
	result := state.TextResult{Runes: utf8.RuneCountInString(args.Text), Words: len(strings.Fields(args.Text))}
	if args.Save {
		data, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if err := os.WriteFile("output/report.json", data, 0600); err != nil {
			return err
		}
		result.Artifact = "output/report.json"
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return protocol.WriteMessage(out, protocol.Message{Type: "tool.result", ID: request.ID, Data: string(data)})
}
