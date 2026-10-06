// packages/envoy/cmd/agent-secrets/secret_prompt_other.go
//go:build !linux && !darwin

package main

import "errors"

// readHiddenAtTerminal refuses: agent-secrets ships for Linux and macOS, whose terminal calls the
// value prompt makes (secret_prompt_unix.go), and a value is piped in anywhere else.
func readHiddenAtTerminal(int) ([]byte, error) {
	return nil, errors.New("the value prompt needs a Linux or macOS terminal: pipe the value in")
}
