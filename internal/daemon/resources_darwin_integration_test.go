//go:build integration && darwin

package daemon

import (
	"fmt"
)

func platformResourceProbe(mode, value string) (string, error) {
	return "", fmt.Errorf("unknown or unsupported resource probe %q", mode)
}
