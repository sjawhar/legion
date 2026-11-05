package procread

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A volume's used bytes come from the mount's own statfs, so any path on a live filesystem
// answers a positive number: a filesystem holding nothing at all still holds its own metadata,
// and the directory the test was given is itself on one.
func TestUsedBytesIsThePositiveReadingOfALiveFilesystem(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "payload"), make([]byte, 64*1024), 0o644); err != nil {
		t.Fatalf("write a file on the filesystem under test: %v", err)
	}
	used, err := UsedBytes(dir)
	if err != nil {
		t.Fatalf("UsedBytes(%s) = %v", dir, err)
	}
	if used <= 0 {
		t.Errorf("UsedBytes(%s) = %d, want a positive count of bytes in use", dir, used)
	}
}

// A path that is not there is an error naming it: a volume the sampler was told to watch and
// cannot find is something to report, never a zero to publish as an empty disk.
func TestUsedBytesRefusesAPathThatIsNotThere(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-volume")
	used, err := UsedBytes(missing)
	if err == nil {
		t.Fatalf("UsedBytes(%s) = %d, want an error", missing, used)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("UsedBytes() error = %v, want it to name %s", err, missing)
	}
}
