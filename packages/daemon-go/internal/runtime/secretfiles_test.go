package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A secret file's write failure names the variable its reader is handed the file through, not the
// file's own name: the Dispatch bearer's is DISPATCH_TOKEN, though its file is dispatch-token.
func TestAFailedSecretFileWriteNamesItsVariable(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stateDir, "secrets", DispatchTokenFileName), 0o700); err != nil {
		t.Fatal(err)
	}

	_, err := WriteDispatchTokenFile(stateDir, "token")

	if err == nil || !strings.Contains(err.Error(), "write the DISPATCH_TOKEN file") {
		t.Fatalf("WriteDispatchTokenFile over a directory = %v, want it to name DISPATCH_TOKEN", err)
	}
}
