//go:build darwin || linux

package main

import (
	"errors"
	"fmt"
	"io"

	"os"

	"path/filepath"
)

func loadConfiguration(filename string, lookupEnv func(string) (string, bool)) (configuration, error) {
	var cfg configuration
	absolute, err := filepath.Abs(filename)
	if err != nil {
		return cfg, fmt.Errorf("configuration path: %w", err)
	}
	file, err := openOwnedFile(absolute, os.O_RDONLY, false)
	if err != nil {
		return cfg, fmt.Errorf("open configuration: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return cfg, fmt.Errorf("read configuration: %w", err)
	}
	if len(data) > maxConfigBytes {
		return cfg, invalid("configuration", "exceeds 1 MiB")
	}
	if err := decodeJSON(data, &cfg); err != nil {
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

func prepareResources(cfg *configuration) error {
	for name, profile := range cfg.SandboxProfiles {
		if profile.Resources != nil {
			root, err := prepareResourceRoot()
			if err != nil {
				return fmt.Errorf("sandbox_profiles.%s.resources: %w", name, err)
			}
			profile.ResourceRoot = root
			cfg.SandboxProfiles[name] = profile
		}
	}
	return nil
}
