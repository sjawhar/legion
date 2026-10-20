package shim_test

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// roleTempDir is a directory as long as the TMPDIR a Legion role container is told —
// `/legion/scratch/<role>/tmp`, 31 bytes for the implementer, the longest role name — made under
// the machine's own temporary directory, which on a machine whose temporary directory is already
// longer is that directory's own length. It exists so a test can be run where a role runs without
// a volume named `/legion` under it.
func roleTempDir(t *testing.T) string {
	t.Helper()
	const want = len("/legion/scratch/implementer/tmp")
	base := os.TempDir()
	size := want - len(base) - 1
	if size < 8 {
		size = 8
	}
	name := make([]byte, (size+1)/2)
	if _, err := rand.Read(name); err != nil {
		t.Fatalf("name a temporary directory: %v", err)
	}
	dir := filepath.Join(base, hex.EncodeToString(name)[:size])
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("make a temporary directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// Every test in this package that listens on its socket must still bind where a Legion role runs.
// A role container's TMPDIR is its own scratch on the issue's volume since INFRA-31
// (`/legion/scratch/<role>/tmp`, 31 bytes), where a role was told none before and every tool fell
// back to `/tmp` (4); Linux's sun_path holds 107 bytes, and socketPath spells the whole test's
// name under TMPDIR, so the longest-named test here lands past the limit and `net.Listen` answers
// `bind: invalid argument` before the test does anything. Measured on 2026-10-20 in this issue's
// own pod: `TMPDIR=/legion/scratch/implementer/tmp go test ./internal/shim/` failed 16 times that
// way, where `/tmp` passes. cmd/legion answers the same break with a socket directory rooted at
// /tmp (socketDir, worker_shim_test.go); this package's socketPath does not. The subtest carries
// the name of the longest test that listens here, since what overflows is the test's own name.
func TestASocketPathBindsWhereTMPDIRIsARoleContainersScratch(t *testing.T) {
	t.Setenv("TMPDIR", roleTempDir(t))
	t.Run("AdoptWorkingCopyRunsTheSharedMetaeditAndAnswersTheDaemon", func(t *testing.T) {
		path := socketPath(t)
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatalf("listen on %s (%d bytes): %v", path, len(path), err)
		}
		t.Cleanup(func() { _ = listener.Close() })
	})
}
