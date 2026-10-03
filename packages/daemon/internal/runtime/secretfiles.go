package runtime

import (
	"fmt"
	"os"
)

// SecretFile is one secret a process reads through a `<Variable>_FILE` pointer: the variable the
// pointer is named after, the file's path, and the value.
type SecretFile struct {
	Variable, Path, Value string
}

// WriteSecretFiles writes each secret to its 0600 file in the 0700 SecretsDir under stateDir,
// re-applying both modes on every write — a directory's mkdir mode is masked and ignored when it
// exists, and a file's is applied only on create (secrets.ts:83-98). A failed write names the
// file's variable. The caller owns the files' lifetimes.
func WriteSecretFiles(stateDir string, files []SecretFile) error {
	dir := SecretsDir(stateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	for _, file := range files {
		if err := os.WriteFile(file.Path, []byte(file.Value), 0o600); err != nil {
			return fmt.Errorf("write the %s file: %w", file.Variable, err)
		}
		if err := os.Chmod(file.Path, 0o600); err != nil {
			return fmt.Errorf("write the %s file: %w", file.Variable, err)
		}
	}
	return nil
}

// WriteSecretFile writes value as SecretFilePath(stateDir, name), the file a process is handed as
// variable's `_FILE` pointer, never the value, and returns its path: the Dispatch bearer every
// pane reads, and `legion controller start`'s controller secret.
func WriteSecretFile(stateDir, name, variable, value string) (string, error) {
	path := SecretFilePath(stateDir, name)
	if err := WriteSecretFiles(stateDir, []SecretFile{{Variable: variable, Path: path, Value: value}}); err != nil {
		return "", err
	}
	return path, nil
}

// DispatchTokenFileName is the daemon-held Dispatch bearer every pane reads through
// DISPATCH_TOKEN_FILE, as the shipped daemon names it under `<state_dir>/secrets`.
const DispatchTokenFileName = "dispatch-token"

// WriteDispatchTokenFile writes the Dispatch bearer as DispatchTokenFileName, returning the path
// every pane receives as DISPATCH_TOKEN_FILE. The value never enters tmux's argv or a pane's
// environment.
func WriteDispatchTokenFile(stateDir, token string) (string, error) {
	return WriteSecretFile(stateDir, DispatchTokenFileName, "DISPATCH_TOKEN", token)
}
