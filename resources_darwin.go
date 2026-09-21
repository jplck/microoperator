package main

import (
	"errors"
	"os"
	"os/exec"
)

func prepareResourceRoot() (string, error) {
	return "", errors.New("hard resource confinement and strict resolver isolation are not qualified on macOS")
}
func prepareResourceProcess(_ *exec.Cmd, profile sandboxConfig) (func() error, error) {
	if profile.Resources != nil {
		_, err := prepareResourceRoot()
		return nil, err
	}
	return func() error { return nil }, nil
}
func prepareResourceWorkspace(_ string, profile sandboxConfig) error {
	if profile.Resources != nil {
		_, err := prepareResourceRoot()
		return err
	}
	return nil
}
func openResourceWorkspace(_ int, _ string) (*os.Root, error) {
	_, err := prepareResourceRoot()
	return nil, err
}
