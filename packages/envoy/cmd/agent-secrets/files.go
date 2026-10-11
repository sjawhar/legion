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

// writeFileAtomic writes data to path by renaming a finished temporary file into place, so no
// reader ever sees path half-written: a call waiting out a box's enrollment (awaitEnrollment)
// reads key.pem and enrollment the moment each appears.
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
