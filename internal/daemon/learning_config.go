//go:build darwin || linux

package daemon

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/jplck/microoperator/internal/sandbox"
	"github.com/jplck/microoperator/internal/state"
)

func prepareLearning(cfg state.Configuration, ctx context.Context) error {
	if cfg.Learning == nil {
		return nil
	}
	if err := state.ValidateLearningConfig(*cfg.Learning); err != nil {
		return err
	}
	qualified := false
	for _, profile := range cfg.SandboxProfiles {
		if state.GeneratedProfile(profile) == nil {
			qualified = true
		}
	}
	if !qualified {
		return state.Invalid("learning", "building requires a qualified resource profile")
	}
	dataDir, err := filepath.EvalSymlinks(cfg.DataDir)
	if err != nil {
		return err
	}
	root := cfg.Learning.ToolchainRoot
	if dataDir == root || strings.HasPrefix(dataDir, root+string(filepath.Separator)) || strings.HasPrefix(root, dataDir+string(filepath.Separator)) {
		return state.Invalid("learning.toolchain_root", "toolchain and daemon state must not overlap")
	}
	digest, err := sandbox.ToolchainDigest(ctx, root)
	if err != nil {
		return err
	}
	if digest != cfg.Learning.ToolchainDigest {
		return state.Invalid("learning.toolchain_digest", "pinned local toolchain changed")
	}
	return nil
}
