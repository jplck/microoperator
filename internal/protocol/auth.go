package protocol

import (
	"strings"
)

const ControlTokenEnv = "MICROOPERATOR_CONTROL_TOKEN"

func ValidAPIToken(token string) bool {
	return len(token) >= 32 && len(token) <= 256 && strings.IndexFunc(token, func(r rune) bool { return r < 33 || r > 126 }) < 0
}
