// packages/envoy/cmd/agent-secrets/files.go
package main

import (
	"fmt"
	"os"
	"strings"
)

// readTrimmed reads the file at path whole and answers its text with surrounding white space
// removed, naming path in any error.
func readTrimmed(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// readPodToken reads a projected pod token through readTrimmed and refuses an empty trimmed value.
// The caller never converts a blank file into a bodyless renewal: a pod given --pod-token-file
// either sends a token or makes no request.
func readPodToken(path string) (string, error) {
	token, err := readTrimmed(path)
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", fmt.Errorf("pod token file %s is empty", path)
	}
	return token, nil
}
