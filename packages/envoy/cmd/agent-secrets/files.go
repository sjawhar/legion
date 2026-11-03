// packages/envoy/cmd/agent-secrets/files.go
package main

import (
	"fmt"
	"os"
	"path/filepath"
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

// writeFileAtomic writes data to path by renaming a finished temporary file into place, so no
// reader ever sees path half-written: a launcher writes key.pem and enrollment into a box's key
// directory while a call inside the box is already waiting for each to appear (awaitEnrollment),
// and that call must read a whole key or none. internal/brokerclient keeps its own copy for the
// sign-in records it writes, which is the one other place in this repository that needs it: two
// short copies cost less than a general file-writing helper exported across a package boundary
// for one caller each.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename has taken it
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
