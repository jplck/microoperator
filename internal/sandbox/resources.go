//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

var WorkspaceName = regexp.MustCompile(`^(activation|tool)-sys_[a-f0-9]{32}-[0-9]+$`)

// Called only under exclusive daemon ownership. Remove runtime workspaces, not
// artifacts/state or caller-selected host paths. RemoveAll does not follow links.
func CleanupWorkspaces(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !WorkspaceName.MatchString(entry.Name()) {
			continue
		}
		if !entry.IsDir() {
			return errors.New("abandoned workspace is not a directory")
		}
		if err := os.RemoveAll(filepath.Join(directory, entry.Name())); err != nil {
			return fmt.Errorf("remove abandoned workspace: %w", err)
		}
	}
	return nil
}
