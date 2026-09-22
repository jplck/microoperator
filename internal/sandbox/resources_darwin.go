package sandbox

import (
	"errors"
	"os"
	"os/exec"

	"github.com/jplck/microoperator/internal/state"
)

func PrepareResourceRoot() (string, error) {
	return "", errors.New("hard resource confinement and strict resolver isolation are not qualified on macOS")
}

func prepareResourceProcess(_ *exec.Cmd, profile state.SandboxConfig) (func() error, error) {
	if profile.Resources != nil {
		_, err := PrepareResourceRoot()
		return nil, err
	}
	return func() error { return nil }, nil
}

func prepareResourceWorkspace(_ string, profile state.SandboxConfig) error {
	if profile.Resources != nil {
		_, err := PrepareResourceRoot()
		return err
	}
	return nil
}

func openResourceWorkspace(_ int, _ string) (*os.Root, error) {
	_, err := PrepareResourceRoot()
	return nil, err
}
