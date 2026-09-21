//go:build darwin || linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const maxGeneratedBinary = 32 << 20

type learningConfig struct {
	ToolchainRoot   string `json:"toolchain_root"`
	ToolchainDigest string `json:"toolchain_digest"`
}

func validateLearningConfig(cfg learningConfig) error {
	digest, err := hex.DecodeString(cfg.ToolchainDigest)
	if !filepath.IsAbs(cfg.ToolchainRoot) || filepath.Clean(cfg.ToolchainRoot) != cfg.ToolchainRoot || err != nil || len(digest) != sha256.Size {
		return invalid("learning", "requires an absolute canonical toolchain_root and its SHA-256 tree digest")
	}

	return nil
}

func (cfg configuration) prepareLearning(ctx context.Context) error {
	if cfg.Learning == nil {
		return nil
	}
	if err := validateLearningConfig(*cfg.Learning); err != nil {
		return err
	}
	qualified := false
	for _, profile := range cfg.SandboxProfiles {
		if generatedProfile(profile) == nil {
			qualified = true
		}
	}
	if !qualified {
		return invalid("learning", "building requires a qualified resource profile")
	}
	dataDir, err := filepath.EvalSymlinks(cfg.DataDir)
	if err != nil {
		return err
	}
	root := cfg.Learning.ToolchainRoot
	if dataDir == root || strings.HasPrefix(dataDir, root+string(filepath.Separator)) || strings.HasPrefix(root, dataDir+string(filepath.Separator)) {
		return invalid("learning.toolchain_root", "toolchain and daemon state must not overlap")
	}
	digest, err := toolchainDigest(ctx, root)
	if err != nil {
		return err
	}
	if digest != cfg.Learning.ToolchainDigest {
		return invalid("learning.toolchain_digest", "pinned local toolchain changed")
	}
	return nil
}

// Pin the complete local toolchain, including compiler, linker and library
// sources, rather than trusting the version string reported by bin/go alone.
func toolchainDigest(ctx context.Context, root string) (string, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || resolved != root || !filepath.IsAbs(root) {
		return "", errors.New("toolchain root must be an existing absolute nonsymlink directory")
	}
	hash := sha256.New()
	var size int64
	var count int
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > 65536 {
			return errors.New("toolchain file count exceeds limit")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0022 != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return errors.New("toolchain must contain only non-group/world-writable regular files and directories")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(hash, "%s\x00%d\x00%d\x00", filepath.ToSlash(relative), info.Mode(), info.Size())
		if info.IsDir() {
			return nil
		}
		size += info.Size()
		if size > 1<<30 {
			return errors.New("toolchain exceeds 1 GiB")
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.CopyN(hash, file, info.Size())
		return errors.Join(err, file.Close())
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func buildEnvironment(root, toolchain string) []string {
	return []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64", "GOAMD64=v1",
		"GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GOENV=off", "GOWORK=off",
		"GO111MODULE=off", "GOMAXPROCS=2", "GOROOT=" + toolchain,
		"GOCACHE=" + filepath.Join(root, "scratch", "cache"), "GOPATH=" + filepath.Join(root, "scratch", "gopath")}
}

func validateGeneratedSource(source string) error {
	if len(source) == 0 || len(source) > maxEventBytes {
		return invalid("source", "requires 1-8192 bytes of Go source")
	}
	file, err := parser.ParseFile(token.NewFileSet(), "candidate.go", source, 0)
	if err != nil || file.Name.Name != "main" {
		return invalid("source", "requires syntactically valid package main")
	}
	for _, imported := range file.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil || path == "C" || strings.Contains(path, ".") || strings.Contains(path, "\\") ||
			strings.HasPrefix(path, "/") || strings.HasPrefix(path, "vendor/") || strings.HasPrefix(path, "internal/") {
			return invalid("source", "only standard-library imports are supported")
		}
	}
	return nil
}

const generatedRunner = `package main
import("encoding/json";"fmt";"io";"os")
func main(){
 fmt.Fprintln(os.Stdout,"{\"type\":\"ready\"}")
 var input struct{Text string ` + "`json:\"text\"`" + `}
 decoder:=json.NewDecoder(io.LimitReader(os.Stdin,8193));decoder.DisallowUnknownFields()
 if err:=decoder.Decode(&input);err!=nil || len(input.Text)>4096{fmt.Fprintln(os.Stderr,"invalid input");os.Exit(1)}
 text,err:=Process(input.Text)
 if err!=nil || len(text)>4096{fmt.Fprintln(os.Stderr,"processing failed or output limit exceeded");os.Exit(1)}
 if err:=json.NewEncoder(os.Stdout).Encode(struct{Text string ` + "`json:\"text\"`" + `}{text});err!=nil{os.Exit(1)}
}
`

func generatedProfile(profile sandboxConfig) error {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" || profile.Resources == nil || profile.resourceRoot == "" {
		return invalid("sandbox", "generated execution requires the qualified Linux/amd64 delegated resource profile")
	}
	if err := validateSandbox("sandbox", profile); err != nil {
		return err
	}
	if len(profile.Read) != 1 || profile.Read[0] != "inputs" || len(profile.ReadWrite) != 2 ||
		!((profile.ReadWrite[0] == "scratch" && profile.ReadWrite[1] == "output") || (profile.ReadWrite[1] == "scratch" && profile.ReadWrite[0] == "output")) {
		return invalid("sandbox", "generated work requires only read-only inputs and bounded scratch/output")
	}
	return nil
}

func generatedWorkspace(dataDir, systemID string) (string, error) {
	root, err := os.MkdirTemp(dataDir, "tool-"+systemID+"-")
	if err != nil {
		return "", err
	}
	for _, name := range []string{"inputs", "scratch", "output"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			return "", errors.Join(err, os.RemoveAll(root))
		}
	}
	return root, nil
}

func boundedGeneratedFile(workspace *os.Root, name string) ([]byte, error) {
	info, err := workspace.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxGeneratedBinary {
		return nil, errors.New("generated artifact is not a bounded regular file")
	}
	file, err := workspace.Open(name)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxGeneratedBinary+1))
	err = errors.Join(err, file.Close())
	if len(data) > maxGeneratedBinary {
		err = errors.Join(err, errors.New("generated artifact exceeds size limit"))
	}
	return data, err
}

func buildGenerated(ctx context.Context, launcher, dataDir, systemID, source string, cfg learningConfig, profile sandboxConfig) (binary []byte, err error) {
	if err := generatedProfile(profile); err != nil {
		return nil, err
	}
	if err := validateGeneratedSource(source); err != nil {
		return nil, err
	}
	if err := validateLearningConfig(cfg); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	digest, err := toolchainDigest(ctx, cfg.ToolchainRoot)
	if err != nil {
		return nil, err
	}
	if digest != cfg.ToolchainDigest {
		return nil, invalid("toolchain", "pinned contents changed")
	}
	root, err := generatedWorkspace(dataDir, systemID)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(root)) }()
	if err := os.WriteFile(filepath.Join(root, "inputs", "candidate.go"), []byte(source), 0600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(root, "inputs", "runner.go"), []byte(generatedRunner), 0600); err != nil {
		return nil, err
	}
	profile.toolchain = cfg.ToolchainRoot
	err = superviseWorkspace(ctx, launcher, root, filepath.Join(cfg.ToolchainRoot, "bin", "go"),
		[]string{"build", "-trimpath", "-buildvcs=false", "-p=2", "-o", "output/tool", "inputs/candidate.go", "inputs/runner.go"},
		func(_ io.Writer, out io.Reader) error {
			data, err := io.ReadAll(io.LimitReader(out, maxEventBytes+1))
			if len(data) > maxEventBytes {
				return errors.New("builder output exceeds limit")
			}
			return err
		}, func(workspace *os.Root) error {
			var err error
			binary, err = boundedGeneratedFile(workspace, "output/tool")
			return err
		}, profile)
	if err != nil {
		return nil, err
	}
	digest, err = toolchainDigest(ctx, cfg.ToolchainRoot)
	if err != nil || digest != cfg.ToolchainDigest {
		return nil, errors.Join(err, errors.New("toolchain changed during build"))
	}
	info, err := buildinfo.Read(bytes.NewReader(binary))
	if err != nil {
		return nil, fmt.Errorf("read generated build identity: %w", err)
	}
	settings := map[string]string{}
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["CGO_ENABLED"] != "0" || settings["GOOS"] != "linux" || settings["GOARCH"] != "amd64" || len(info.Deps) != 0 {
		return nil, errors.New("generated binary violates pinned pure-Go standard-library build settings")
	}
	return binary, nil
}

func runGenerated(ctx context.Context, launcher, dataDir, systemID string, binary []byte, input string, profile sandboxConfig) (output string, err error) {
	if err := generatedProfile(profile); err != nil {
		return "", err
	}
	if len(binary) == 0 || len(binary) > maxGeneratedBinary || len(input) > 4096 {
		return "", invalid("generated tool", "binary or input exceeds limit")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	root, err := generatedWorkspace(dataDir, systemID)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(root)) }()
	target := filepath.Join(root, "inputs", "tool")
	if err := os.WriteFile(target, binary, 0700); err != nil {
		return "", err
	}
	data, err := json.Marshal(struct {
		Text string `json:"text"`
	}{input})
	if err != nil {
		return "", err
	}
	err = supervise(ctx, launcher, root, target, nil, func(in io.Writer, out io.Reader) error {
		// Wait for exec before sending input: the launcher's bootstrap reader
		// must not prefetch workload bytes into a buffer discarded by exec.
		reader := bufio.NewReaderSize(out, maxFrame+1)
		ready, err := readMessage(reader)
		if err != nil {
			return err
		}
		if ready != (message{Type: "ready"}) {
			return errors.New("invalid generated tool readiness")
		}
		if _, err := in.Write(append(data, '\n')); err != nil {
			return err
		}
		result, err := io.ReadAll(io.LimitReader(reader, maxEventBytes+1))
		if err != nil {
			return err
		}
		if len(result) > maxEventBytes {
			return errors.New("generated output exceeds limit")
		}
		var value struct {
			Text string `json:"text"`
		}
		if err := decodeJSON(result, &value); err != nil {
			return err
		}
		if len(value.Text) > 4096 {
			return errors.New("generated text exceeds limit")
		}
		output = value.Text
		return nil
	}, profile)
	return output, err
}
