package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// MaterializeDeploymentInstructions reads the operator's instructions file and writes
// `# Deployment instructions (<legionID>)`, a blank line, and its contents verbatim to
// `<stateDir>/deployment-instructions.md`, returning that path — the file every pane's one
// `--append-system-prompt` word ends with, `$(cat <this file>)`, so a pane gets exactly what boot
// read and never the operator's own path, which may change underneath a running daemon
// (packages/daemon/src/daemon/deployment-instructions.ts:29-46). legionID is `project` as the
// operator wrote it.
//
// A configured file with nothing in it is a misconfiguration, not an empty fragment: a missing,
// unreadable, or blank file is refused naming the operator's path, and nothing is written. Boot
// calls this once, before the first pane opens.
func MaterializeDeploymentInstructions(instructionsPath, stateDir, legionID string) (string, error) {
	contents, err := os.ReadFile(instructionsPath)
	if err != nil {
		return "", fmt.Errorf("instructions file %s could not be read: %w", instructionsPath, err)
	}
	if strings.TrimSpace(string(contents)) == "" {
		return "", fmt.Errorf("instructions file %s is empty", instructionsPath)
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
