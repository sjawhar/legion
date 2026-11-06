package claude

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// `.claude-code-pin` is the repository's one copy of the Claude Code release every reader of it
// installs (.github/actions/install-claude-code), so PinnedVersion, which the appender writes into
// every line it adds and the contract tests hold the real binary to, is that file's version and no
// second copy of it: a pin raised in the file alone fails here rather than in a contract test that
// needs the binary.
func TestPinnedVersionIsTheRepositorysPin(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate this test's source")
	}
	pin, err := os.ReadFile(filepath.Join(filepath.Dir(source), "../../../../../.claude-code-pin"))
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSpace(string(pin)); want != PinnedVersion {
		t.Errorf("PinnedVersion = %q, want %q, the version .claude-code-pin names", PinnedVersion, want)
	}
}
