package auth

import (
	"path/filepath"
	"testing"
)

func TestWriteAndReadTokens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	in := &Tokens{
		AccessToken:      "access",
		RefreshToken:     "refresh",
		AccessExpiresAt:  1000,
		RefreshExpiresAt: 2000,
		GithubLogin:      "sjawhar",
	}
	if err := WriteTokens(path, in); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := ReadTokens(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out == nil || *out != *in {
		t.Errorf("round trip failed: got %+v want %+v", out, in)
	}
}

func TestReadTokensMissing(t *testing.T) {
	out, err := ReadTokens(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != nil {
		t.Errorf("expected nil tokens for missing file, got %+v", out)
	}
}

func TestRemoveTokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	if err := WriteTokens(path, &Tokens{}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := RemoveTokenFile(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// Removing a missing file is a no-op.
	if err := RemoveTokenFile(path); err != nil {
		t.Fatalf("second remove: %v", err)
	}
}
