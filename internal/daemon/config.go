//go:build linux

package daemon

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jplck/microoperator/internal/protocol"
	"github.com/jplck/microoperator/internal/sandbox"
	"github.com/jplck/microoperator/internal/state"
)

func loadConfiguration(filename string, lookupEnv func(string) (string, bool)) (state.Configuration, error) {
	var cfg state.Configuration
	absolute, err := filepath.Abs(filename)
	if err != nil {
		return cfg, fmt.Errorf("configuration path: %w", err)
	}
	file, err := state.OpenOwnedFile(absolute, os.O_RDONLY, false)
	if err != nil {
		return cfg, fmt.Errorf("open configuration: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, state.MaxConfigBytes+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return cfg, fmt.Errorf("read configuration: %w", err)
	}
	if len(data) > state.MaxConfigBytes {
		return cfg, state.Invalid("configuration", "exceeds 1 MiB")
	}
	if err := protocol.DecodeJSON(data, &cfg); err != nil {
		return cfg, fmt.Errorf("configuration: %w", err)
	}
	if err := cfg.Validate(lookupEnv); err != nil {
		return cfg, err
	}
	if !filepath.IsAbs(cfg.DataDir) {
		cfg.DataDir = filepath.Join(filepath.Dir(absolute), cfg.DataDir)
	}
	cfg.DataDir = filepath.Clean(cfg.DataDir)
	executable, err := os.Executable()
	if err != nil {
		return cfg, err
	}
	cfg.RuntimeDigest, err = executableDigest(executable)
	if err != nil {
		return cfg, fmt.Errorf("fingerprint reviewed executable: %w", err)
	}
	return cfg, nil
}

func prepareResources(cfg *state.Configuration) error {
	for name, profile := range cfg.SandboxProfiles {
		if profile.Resources != nil {
			root, err := sandbox.PrepareResourceRoot()
			if err != nil {
				return fmt.Errorf("sandbox_profiles.%s.resources: %w", name, err)
			}
			profile.ResourceRoot = root
			cfg.SandboxProfiles[name] = profile
		}
	}
	return nil
}
