package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ProviderEnvDir is the daemon-held directory of provider keys, under the state directory's
// secrets: one 0600 file per key, named for the key. Every pane's shim is pointed at it
// (`--provider-env-dir`) and exports each file into OMP's environment alone.
func ProviderEnvDir(stateDir string) string {
	return filepath.Join(stateDir, "secrets", "provider-env")
}

// MaterializeProviderKeys resolves each provider key and writes its value to a file in
// ProviderEnvDir named for the variable OMP reads, returning that directory — or "" when there are
// none, having removed any a previous boot left. Boot calls it once, before any pane can launch.
// A pane never reaches the secret store: under tmux, identity reaches panes as daemon-held files
// (LEGION-208 spec, Identity), because the store's age identity would decrypt every agent-tier
// secret on the box.
//
// Each secretsd key is resolved the way the shipped daemon resolves an App key it holds in
// secretsd (packages/daemon/src/daemon/config.ts:834-902): `secrets get <KEY> --no-request` first,
// which reports the key's tier without costing a tap, then `secrets get <KEY> --value`. A
// human-tier key is announced before its value is requested, since a YubiKey tap may be needed.
// The daemon runs `secrets` for itself — it is never a pane — under its own environment (environ)
// less SECRETSD_SESSION_TOKEN_FILE, with its own stdin: secretsd scopes a tokenless caller by its
// terminal, so the request is the launcher's and never lands on an agent session the daemon was
// started from. There is no daemon-imposed timeout; secretsd's own approval window is the failure.
//
// Every key is resolved before any file is written, so a boot that refuses writes nothing. Each
// file is written atomically (a temporary file renamed into place), the directory is 0700 and
// each file 0600, and a file for a variable no longer configured is removed: the directory holds
// exactly the configured variables. Every refusal names the entry — the variable and its secretsd
// key — and never the value.
func MaterializeProviderKeys(keys []ProviderKey, stateDir string, environ []string, log *slog.Logger) (string, error) {
	dir := ProviderEnvDir(stateDir)
	if len(keys) == 0 {
		if err := os.RemoveAll(dir); err != nil {
			return "", fmt.Errorf("provider_keys: remove %s: %w", dir, err)
		}
		return "", nil
	}
	env := make([]string, 0, len(environ))
	for _, entry := range environ {
		if !strings.HasPrefix(entry, "SECRETSD_SESSION_TOKEN_FILE=") {
			env = append(env, entry)
		}
	}
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		value, err := resolveProviderKey(key, env, log)
		if err != nil {
			return "", err
		}
		values[key.Env] = value
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("provider_keys: create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("provider_keys: %w", err)
	}
	for _, key := range keys {
		if err := writeAtomically(filepath.Join(dir, key.Env), values[key.Env]); err != nil {
			return "", fmt.Errorf("provider_keys.%s: write its file: %w", key.Env, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("provider_keys: list %s: %w", dir, err)
	}
	for _, entry := range entries {
		if _, configured := values[entry.Name()]; !configured {
			if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
				return "", fmt.Errorf("provider_keys: remove %s, which no configured variable names: %w", entry.Name(), err)
			}
		}
	}
	return dir, nil
}

// secretsStatus is what `secrets get <KEY> --no-request` prints: the key and its tier.
type secretsStatus struct {
	Key  string `json:"key"`
	Tier string `json:"tier"`
}

// resolveProviderKey is one entry's value: its secretsd key's tier checked, then the value read.
func resolveProviderKey(key ProviderKey, env []string, log *slog.Logger) (string, error) {
	statusText, err := runSecretsGet(key, "--no-request", env)
	if err != nil {
		return "", err
	}
	unparsable := fmt.Errorf(`provider_keys.%s: secrets get %s --no-request printed an unparsable status (expected {"key","tier"})`, key.Env, key.Secret)
	var status secretsStatus
	if err := json.Unmarshal(bytes.TrimSpace(statusText), &status); err != nil || status.Key == "" || status.Tier == "" {
		return "", unparsable
	}
	// A status for some other key is not a status for this one.
	if status.Key != key.Secret {
		return "", unparsable
	}
	switch status.Tier {
	case "agent":
	case "human":
		log.Warn(fmt.Sprintf("requesting %s from secretsd (human tier; a YubiKey tap may be needed)", key.Secret))
	default:
		return "", fmt.Errorf("provider_keys.%s: secrets get %s --no-request reported tier %q, which is neither agent nor human", key.Env, key.Secret, status.Tier)
	}
	value, err := runSecretsGet(key, "--value", env)
	if err != nil {
		return "", err
	}
	trimmed := strings.TrimSpace(string(value))
	if trimmed == "" {
		return "", fmt.Errorf("provider_keys.%s: secrets get %s --value printed no value", key.Env, key.Secret)
	}
	return trimmed, nil
}

// runSecretsGet runs `secrets get <key.Secret> <flag>` — found on env's PATH — with the daemon's
// stdin and env, returning its stdout. A failure carries the exit status and the child's stderr,
// never its stdout.
func runSecretsGet(key ProviderKey, flag string, env []string) ([]byte, error) {
	args := []string{"get", key.Secret, flag}
	binary, err := lookPathIn("secrets", env)
	if err != nil {
		return nil, fmt.Errorf("provider_keys.%s: the secrets command is not on PATH, so %s cannot be read", key.Env, key.Secret)
	}
	cmd := exec.Command(binary, args...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return nil, fmt.Errorf("provider_keys.%s: could not run secrets %s: %w", key.Env, strings.Join(args, " "), err)
		}
		detail := ""
		if text := strings.TrimSpace(stderr.String()); text != "" {
			detail = ": " + text
		}
		return nil, fmt.Errorf("provider_keys.%s: secrets %s failed (exit %d)%s", key.Env, strings.Join(args, " "), exit.ExitCode(), detail)
	}
	return stdout.Bytes(), nil
}

// lookPathIn finds an executable command on the PATH env carries — the daemon's own, which the
// child runs under — rather than on this process's PATH.
func lookPathIn(command string, env []string) (string, error) {
	path := ""
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			path = value
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, command)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s is not on PATH", command)
}

// writeAtomically replaces path with a 0600 file holding value: a temporary file beside it, synced
// and renamed into place, so a shim reading the directory never sees half a key.
func writeAtomically(path, value string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(value); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
