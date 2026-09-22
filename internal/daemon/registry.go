//go:build linux

package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jplck/microoperator/internal/protocol"
	"github.com/jplck/microoperator/internal/sandbox"
	"github.com/jplck/microoperator/internal/state"
)

func (engine *executionEngine) runTextTool(ctx context.Context, e state.ExecutionRecord, args state.TextArguments, profile state.SandboxConfig) (result state.TextResult, artifact []byte, err error) {
	digest, err := executableDigest(engine.executable)
	if err != nil {
		return result, nil, err
	}
	if digest != engine.cfg.RuntimeDigest {
		return result, nil, state.Invalid("executable", "reviewed binary changed; restart and explicitly revise its assignments")
	}
	root, err := os.MkdirTemp(engine.cfg.DataDir, "tool-"+e.SystemID+"-")
	if err != nil {
		return result, nil, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(root)) }()
	paths := append([]string{"inputs", "scratch", "output"}, profile.Read...)
	paths = append(paths, profile.ReadWrite...)
	for _, name := range paths {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			return result, nil, err
		}
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return result, nil, err
	}
	data, err := json.Marshal(args)
	if err != nil {
		return result, nil, err
	}
	toolCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err = sandbox.SuperviseWorkspace(toolCtx, engine.executable, root, engine.executable, []string{"tool", "text-analyze"}, func(in io.Writer, out io.Reader) error {
		reader := bufio.NewReaderSize(out, protocol.MaxFrame+1)
		ready, err := protocol.ReadMessage(reader)
		if err != nil {
			return err
		}
		if ready != (protocol.Message{Type: "ready"}) {
			return errors.New("invalid tool readiness")
		}
		if err := protocol.WriteMessage(in, protocol.Message{Type: "tool.input", ID: e.CallID, Data: string(data)}); err != nil {
			return err
		}
		reply, err := protocol.ReadMessage(reader)
		if err != nil {
			return err
		}
		if reply.Type != "tool.result" || reply.ID != e.CallID {
			return errors.New("tool response correlation mismatch")
		}
		if err := protocol.DecodeJSON([]byte(reply.Data), &result); err != nil {
			return err
		}
		if result.Runes != utf8.RuneCountInString(args.Text) || result.Words != len(strings.Fields(args.Text)) {
			return errors.New("invalid tool result")
		}
		return nil
	}, func(workspace *os.Root) error {
		if args.Save {
			if result.Artifact != "output/report.json" {
				return errors.New("tool artifact escaped its manifest")
			}
			info, err := workspace.Lstat(result.Artifact)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
				return errors.New("tool artifact is not a private regular file")
			}
			file, err := workspace.Open(result.Artifact)
			if err != nil {
				return err
			}
			artifact, err = io.ReadAll(io.LimitReader(file, 4097))
			err = errors.Join(err, file.Close())
			if err != nil || len(artifact) > 4096 {
				return errors.Join(err, errors.New("invalid artifact size"))
			}
			var report state.TextResult
			if err := protocol.DecodeJSON(artifact, &report); err != nil || report.Runes != result.Runes || report.Words != result.Words || report.Artifact != "" {
				return errors.New("invalid report artifact")
			}
		} else if result.Artifact != "" {
			return errors.New("unexpected tool artifact")
		}
		return nil
	}, profile)
	if err != nil {
		return result, nil, err
	}
	return result, artifact, nil
}

func executableDigest(filename string) (string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, err = io.Copy(hash, file)
	if err = errors.Join(err, file.Close()); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
