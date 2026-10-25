// Package capabilitiestest is the worker image in miniature that internal/capabilities and
// cmd/legion's probe-image tests each build: a bin of stub binaries for the image rows that check
// PATH alone, the plugin lock `omp plugin install` writes in the worker image, with the CodeGraph
// plugin enabled, and the claude-envoy root and Claude Code pin the claude-code row reads.
package capabilitiestest

import (
	"os"
	"path/filepath"
	"testing"
)

// binaries are the binaries the capability check's image rows look for.
var binaries = []string{"gopls", "typescript-language-server", "pyright-langserver", "codegraph", "go", "curl", "wget", "python3", "node", "bun", "uv", "kubectl", "jq", "ssh-keygen", "flock", "psql", "redis-cli", "sqlite3", "nc", "dig", "chromium", "npm"}

// BinaryScript is what a stubbed binary runs: `go version` answers a Go version, `chromium
// --version` a Chromium one.
const BinaryScript = `#!/bin/sh
case "$1" in
version) echo "go version go1.27.1 linux/amd64" ;;
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

// ClaudeCodeVersion is the version ClaudeCodePackageJSON pins and ClaudeCodeLock locks.
const ClaudeCodeVersion = "2.1.310"

// ClaudeCodePackageJSON is the image's Claude Code pin's package.json, as
// packages/daemon/docker/claude-code commits it: @anthropic-ai/claude-code at one exact version.
const ClaudeCodePackageJSON = `{
  "name": "legion-worker-claude-code",
  "private": true,
  "dependencies": {
    "@anthropic-ai/claude-code": "2.1.310"
  }
}
`

// ClaudeCodeLock is the pin's package-lock.json, cut to what a pod's install of it reads: the
// root package recording the pin, and @anthropic-ai/claude-code and the linux-x64 package whose
// binary that install runs, each at the pinned version with its sha512 integrity.
const ClaudeCodeLock = `{
  "name": "legion-worker-claude-code",
  "lockfileVersion": 3,
  "requires": true,
  "packages": {
    "": {
      "name": "legion-worker-claude-code",
      "dependencies": {
        "@anthropic-ai/claude-code": "2.1.310"
      }
    },
    "node_modules/@anthropic-ai/claude-code": {
      "version": "2.1.310",
      "resolved": "https://registry.npmjs.org/@anthropic-ai/claude-code/-/claude-code-2.1.310.tgz",
      "integrity": "sha512-EySpBwaocdU1nc9r7JH+GOlnMsbXc764d3HX7NL8fr7qqdxz2MiXI9hqf/rzeE/XgU23HHFltIqhb+Fq6G1CVw==",
      "optionalDependencies": {
        "@anthropic-ai/claude-code-linux-x64": "2.1.310"
      }
    },
    "node_modules/@anthropic-ai/claude-code-linux-x64": {
      "version": "2.1.310",
      "resolved": "https://registry.npmjs.org/@anthropic-ai/claude-code-linux-x64/-/claude-code-linux-x64-2.1.310.tgz",
      "integrity": "sha512-CuzpJrMBbqaR3+1SiLrBtPV3w/cFvcqBfhe9kNskzZ2Fm6KXa7v0l7tM+Ln6l5QLb79WoU0CcacXnYtsygbe6A==",
      "optional": true
    }
  }
}
`

// ClaudeRoot writes the claude-envoy root the worker image carries under a fresh directory and
// returns it: its manifest declaring contract (a daemon API version, as the manifest writes it),
// the built channel server a worker runs as its MCP server, and the generated Legion plugin's
// manifest.
func ClaudeRoot(t testing.TB, contract string) string {
	t.Helper()
	return writeTree(t, map[string]string{
		"package.json":                      `{"name":"@sjawhar/claude-envoy","version":"1.0.0","legion":{"daemonApiVersion":` + contract + `}}`,
		"dist/envoy-channel.js":             "// the channel server\n",
		"legion/.claude-plugin/plugin.json": `{"name":"legion"}`,
	})
}

// ClaudeCodePin writes the image's Claude Code pin, ClaudeCodePackageJSON and ClaudeCodeLock,
// under a fresh directory and returns it.
func ClaudeCodePin(t testing.TB) string {
	t.Helper()
	return writeTree(t, map[string]string{"package.json": ClaudeCodePackageJSON, "package-lock.json": ClaudeCodeLock})
}

// writeTree writes files, slash-separated paths to their contents, under a fresh directory.
func writeTree(t testing.TB, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
