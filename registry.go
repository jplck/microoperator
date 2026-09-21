//go:build darwin || linux

package main

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
)

// reviewedTextTool is fixed Go code. Agent-authored source never reaches this
// entry point; it has no shell, network, arbitrary path, or code-loading option.
func reviewedTextTool(in io.Reader, out io.Writer) error {
	if err := writeMessage(out, message{Type: "ready"}); err != nil {
		return err
	}
	request, err := readMessage(bufio.NewReaderSize(in, maxFrame+1))
	if err != nil {
		return err
	}
	var args textArguments
	if request.Type != "tool.input" || decodeJSON([]byte(request.Data), &args) != nil || len(args.Text) > 4096 {
		return errors.New("invalid text analysis arguments")
	}
	result := textResult{Runes: utf8.RuneCountInString(args.Text), Words: len(strings.Fields(args.Text))}
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
	return writeMessage(out, message{Type: "tool.result", ID: request.ID, Data: string(data)})
}

func (engine *executionEngine) runTextTool(ctx context.Context, e executionRecord, args textArguments, profile sandboxConfig) (result textResult, artifact []byte, err error) {
	digest, err := executableDigest(engine.executable)
	if err != nil {
		return result, nil, err
	}
	if digest != engine.cfg.RuntimeDigest {
		return result, nil, invalid("executable", "reviewed binary changed; restart and explicitly revise its assignments")
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
	err = superviseWorkspace(toolCtx, engine.executable, root, engine.executable, []string{"tool", "text-analyze"}, func(in io.Writer, out io.Reader) error {
		reader := bufio.NewReaderSize(out, maxFrame+1)
		ready, err := readMessage(reader)
		if err != nil {
			return err
		}
		if ready != (message{Type: "ready"}) {
			return errors.New("invalid tool readiness")
		}
		if err := writeMessage(in, message{Type: "tool.input", ID: e.CallID, Data: string(data)}); err != nil {
			return err
		}
		reply, err := readMessage(reader)
		if err != nil {
			return err
		}
		if reply.Type != "tool.result" || reply.ID != e.CallID {
			return errors.New("tool response correlation mismatch")
		}
		if err := decodeJSON([]byte(reply.Data), &result); err != nil {
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
			var report textResult
			if err := decodeJSON(artifact, &report); err != nil || report.Runes != result.Runes || report.Words != result.Words || report.Artifact != "" {
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
