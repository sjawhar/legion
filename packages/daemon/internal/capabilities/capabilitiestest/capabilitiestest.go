// Package capabilitiestest is the worker image in miniature that internal/capabilities and
// cmd/legion's probe-image tests each build: a bin of stub binaries for the image rows that check
// PATH alone, and the plugin lock `omp plugin install` writes in the worker image, with the
// CodeGraph plugin enabled.
package capabilitiestest

import (
	"os"
	"path/filepath"
	"testing"
)

// binaries are the binaries the capability check's image rows look for.
var binaries = []string{"gopls", "typescript-language-server", "pyright-langserver", "codegraph", "go", "curl", "wget", "python3", "node", "bun", "uv", "kubectl", "jq", "ssh-keygen", "chromium"}

// BinaryScript is what a stubbed binary runs: `go version` answers a Go version, `chromium
// --version` a Chromium one.
const BinaryScript = `#!/bin/sh
case "$1" in
version) echo "go version go1.26.8 linux/amd64" ;;
--version) echo "Chromium 141.0.0.0" ;;
esac
`

// CodeGraphLock is the plugin lock `omp plugin install` writes in the worker image, with the
// CodeGraph plugin enabled.
const CodeGraphLock = `{"plugins":{"@bopstack/pi-codegraph":{"version":"0.1.1","enabledFeatures":null,"enabled":true}}}`

// LockPath is where the legion profile under home keeps its plugin lock.
func LockPath(home string) string {
	return filepath.Join(home, ".omp", "profiles", "legion", "plugins", "omp-plugins.lock.json")
}

// InstallBinaries writes BinaryScript, executable, as every binary the image rows look for, under bin.
func InstallBinaries(t testing.TB, bin string) {
	t.Helper()
	for _, name := range binaries {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(BinaryScript), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// WriteLock writes content to LockPath(home), making the directories it needs.
func WriteLock(t testing.TB, home, content string) {
	t.Helper()
	path := LockPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
