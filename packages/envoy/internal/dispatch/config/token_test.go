package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// The server reads no token: a value starting with `!` is a command only the clients run, so Load
// accepts it, as written, from the user file or the checkout's, and runs nothing.
func TestLoadRunsNoTokenCommand(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	command := "!touch " + marker
	mustWrite(t, userFile(home), `{"dispatch": {"token": "user-token"}, "envoy": {"token": "`+command+`", "natsSeed": "`+command+`"}}`)
	mustWrite(t, checkoutFile(cwd), `{"dispatch": {"token": "`+command+`"}}`)
	if _, err := Load(environment(nil), LoadOptions{CWD: cwd, HomeDir: home}); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat %s: %v; Load ran a token command", marker, err)
	}
}
