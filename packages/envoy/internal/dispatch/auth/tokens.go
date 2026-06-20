// Package auth handles GitHub OAuth device flow, on-disk token storage, and
// HMAC-signed session cookies for the Dispatch server.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// Tokens is the persisted GitHub OAuth state for the logged-in dashboard user.
type Tokens struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	AccessExpiresAt  int64  `json:"accessExpiresAt"`
	RefreshExpiresAt int64  `json:"refreshExpiresAt"`
	GithubLogin      string `json:"githubLogin"`
}

// DefaultTokenPath returns ~/.local/share/dispatch/auth.json.
func DefaultTokenPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home: %w", err)
	}
	return filepath.Join(home, ".local", "share", "dispatch", "auth.json"), nil
}

// ReadTokens returns nil, nil when the file does not exist; other read/parse
// errors are logged and return nil, nil so callers behave like the TS version
// (treat malformed file as "not logged in").
func ReadTokens(path string) (*Tokens, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		slog.Warn("dispatch: failed to read tokens", "path", path, "error", err)
		return nil, nil
	}
	var t Tokens
	if err := json.Unmarshal(data, &t); err != nil {
		slog.Warn("dispatch: failed to parse tokens", "path", path, "error", err)
		return nil, nil
	}
	return &t, nil
}

// WriteTokens writes tokens atomically with mode 0600 (Unix). The parent
// directory is created if missing.
func WriteTokens(path string, tokens *Tokens) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create token dir: %w", err)
	}
	data, err := json.MarshalIndent(tokens, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal tokens: %w", err)
	}
	return os.WriteFile(path, data, 0o600)
}

// RemoveTokenFile deletes the token file if present. Missing file is a no-op.
func RemoveTokenFile(path string) error {
	err := os.Remove(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
