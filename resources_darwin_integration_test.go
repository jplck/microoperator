//go:build integration && darwin

package main

import "fmt"

func platformResourceProbe(mode, value string) (string, error) {
	return "", fmt.Errorf("unknown or unsupported resource probe %q", mode)
}
