package state

import (
	"crypto/sha256"

	"encoding/hex"

	"go/parser"
	"go/token"

	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const MaxGeneratedBinary = 32 << 20

type LearningConfig struct {
	ToolchainRoot   string `json:"toolchain_root"`
	ToolchainDigest string `json:"toolchain_digest"`
}

func ValidateLearningConfig(cfg LearningConfig) error {
	digest, err := hex.DecodeString(cfg.ToolchainDigest)
	if !filepath.IsAbs(cfg.ToolchainRoot) || filepath.Clean(cfg.ToolchainRoot) != cfg.ToolchainRoot || err != nil || len(digest) != sha256.Size {
		return Invalid("learning", "requires an absolute canonical toolchain_root and its SHA-256 tree digest")
	}

	return nil
}

func ValidateGeneratedSource(source string) error {
	if len(source) == 0 || len(source) > MaxEventBytes {
		return Invalid("source", "requires 1-8192 bytes of Go source")
	}
	file, err := parser.ParseFile(token.NewFileSet(), "candidate.go", source, 0)
	if err != nil || file.Name.Name != "main" {
		return Invalid("source", "requires syntactically valid package main")
	}
	for _, imported := range file.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil || path == "C" || strings.Contains(path, ".") || strings.Contains(path, "\\") ||
			strings.HasPrefix(path, "/") || strings.HasPrefix(path, "vendor/") || strings.HasPrefix(path, "internal/") {
			return Invalid("source", "only standard-library imports are supported")
		}
	}
	return nil
}

func GeneratedProfile(profile SandboxConfig) error {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" || profile.Resources == nil || profile.ResourceRoot == "" {
		return Invalid("sandbox", "generated execution requires the qualified Linux/amd64 delegated resource profile")
	}
	if err := ValidateSandbox("sandbox", profile); err != nil {
		return err
	}
	if len(profile.Read) != 1 || profile.Read[0] != "inputs" || len(profile.ReadWrite) != 2 ||
		!((profile.ReadWrite[0] == "scratch" && profile.ReadWrite[1] == "output") || (profile.ReadWrite[1] == "scratch" && profile.ReadWrite[0] == "output")) {
		return Invalid("sandbox", "generated work requires only read-only inputs and bounded scratch/output")
	}
	return nil
}
