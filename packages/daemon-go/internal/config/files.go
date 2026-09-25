package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// DeploymentInstructionsFile is the copy of the operator's instructions every pane's prompt reads,
// under the state directory.
const DeploymentInstructionsFile = "deployment-instructions.md"

// ReadSecretPointer is the trimmed contents of the file a pointer key names — `variable` is the
// key as the operator wrote it (`envoy_token_file`, `operator_token_file`), so the refusal reads
// in their words. A set pointer is authoritative: a missing, unreadable, or blank file is a
// refusal naming the key and the path, never a fallback to another source
// (packages/daemon/src/daemon/secrets.ts:10-22). The contents never appear in an error.
func ReadSecretPointer(variable, file string) (string, error) {
	contents, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("%s names %s, which could not be read: %w", variable, file, err)
	}
	secret := strings.TrimSpace(string(contents))
	if secret == "" {
		return "", fmt.Errorf("%s names %s, which is empty", variable, file)
	}
	return secret, nil
}

// ReadOperatorTokenFile is the operator bearer the file names — `variable` is the key or flag as
// the operator wrote it (`operator_token_file`, `--operator-token-file`). The bearer buys a
// controller capability and opens every operator route, so a group- or other-readable copy is a
// second way in: the file is opened once, and the open descriptor must be a regular file whose
// mode grants its group and others nothing before its trimmed, non-empty contents are read from
// that same descriptor, so nothing swapped in between a check and a read is ever read
// (packages/daemon/src/cli/controller-start.ts:177-199, which stats and reads separately). The
// open does not block, so a FIFO is refused rather than waited on. The contents never appear in an
// error.
func ReadOperatorTokenFile(variable, file string) (string, error) {
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("%s names %s, which could not be read: %w", variable, file, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("%s names %s, which could not be read: %w", variable, file, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s names %s, which is not a regular file", variable, file)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return "", fmt.Errorf("%s %s is readable by its group or others (mode %#o); chmod 0600 it", variable, file, mode)
	}
	contents, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("%s names %s, which could not be read: %w", variable, file, err)
	}
	token := strings.TrimSpace(string(contents))
	if token == "" {
		return "", fmt.Errorf("%s names %s, which is empty", variable, file)
	}
	return token, nil
}

// ReadDeploymentInstructions is the operator's instructions file, refused naming the operator's
// path when it is missing, unreadable, or blank: a configured file with nothing in it is a
// misconfiguration, not an empty fragment. It writes nothing, so `legion controller start` runs it
// before the one daemon call it makes (packages/daemon/src/daemon/deployment-instructions.ts).
func ReadDeploymentInstructions(instructionsPath string) ([]byte, error) {
	contents, err := os.ReadFile(instructionsPath)
	if err != nil {
		return nil, fmt.Errorf("instructions file %s could not be read: %w", instructionsPath, err)
	}
	if strings.TrimSpace(string(contents)) == "" {
		return nil, fmt.Errorf("instructions file %s is empty", instructionsPath)
	}
	return contents, nil
}

// MaterializeDeploymentInstructions reads the operator's instructions file and writes
// `# Deployment instructions (<legionID>)`, a blank line, and its contents verbatim to
// `<stateDir>/deployment-instructions.md`, returning that path — the file every pane's one
// `--append-system-prompt` word ends with, `$(cat <this file>)`, so a pane gets exactly what boot
// read and never the operator's own path, which may change underneath a running daemon
// (packages/daemon/src/daemon/deployment-instructions.ts:29-46). legionID is `project` as the
// operator wrote it.
//
// A file ReadDeploymentInstructions refuses writes nothing. Boot calls this once, before the first
// pane opens.
func MaterializeDeploymentInstructions(instructionsPath, stateDir, legionID string) (string, error) {
	contents, err := ReadDeploymentInstructions(instructionsPath)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", fmt.Errorf("create state directory %s: %w", stateDir, err)
	}
	target := filepath.Join(stateDir, DeploymentInstructionsFile)
	body := fmt.Sprintf("# Deployment instructions (%s)\n\n%s", legionID, contents)
	if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", target, err)
	}
	return target, nil
}
