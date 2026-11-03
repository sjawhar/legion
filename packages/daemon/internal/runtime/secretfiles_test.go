package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// A process reading its secret file while the daemon rewrites it — a launch for the claim again, a
// new Dispatch token — reads the old value or the new, never an empty or short file: a shim that
// reads its boot token blank exits, and its pane closes with it.
func TestARewrittenSecretFileIsNeverReadEmptyOrShort(t *testing.T) {
	stateDir := t.TempDir()
	path := SecretFilePath(stateDir, "claim")
	values := [2]string{strings.Repeat("a", 4096), strings.Repeat("b", 4096)}
	write := func(value string) {
		t.Helper()
		if err := WriteSecretFiles(stateDir, []SecretFile{{Variable: "LEGION_BOOT_TOKEN", Path: path, Value: value}}); err != nil {
			t.Fatal(err)
		}
	}
	write(values[0])

	reading, done := make(chan struct{}), make(chan struct{})
	var torn string
	var wg sync.WaitGroup
	wg.Go(func() {
		for first := true; ; first = false {
			got, err := os.ReadFile(path)
			if first {
				close(reading)
			}
			switch {
			case err != nil:
				torn = err.Error()
				return
			case string(got) != values[0] && string(got) != values[1]:
				torn = fmt.Sprintf("%d bytes that are neither value whole", len(got))
				return
			}
			select {
			case <-done:
				return
			default:
			}
		}
	})
	<-reading
	func() {
		defer close(done)
		for i := range 500 {
			write(values[(i+1)%2])
		}
	}()
	wg.Wait()
	if torn != "" {
		t.Fatalf("a reader of the secret file while it was rewritten read %s, want the old value or the new", torn)
	}
}
