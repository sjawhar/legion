package settings

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// watcherFor is a watcher over path at test speed, already running, and the changes it applies.
// The keeper keeps nothing: what the file arm does with the store is the keeper's own test.
func watcherFor(t *testing.T, path string) (*Keeper, chan []config.Change) {
	t.Helper()
	keeper := keeperOn(t, path, nil)
	watcher := NewWatcher(keeper, quiet())
	watcher.Interval, watcher.Debounce = 10*time.Millisecond, 10*time.Millisecond
	settled := make(chan []config.Change, 8)
	watcher.Settled = func(_ *config.Config, applied []config.Change) { settled <- applied }
	// The watcher's baseline is the state it reads first, and a change made before that reading
	// is part of it: the test waits for that reading before it changes anything.
	read := make(chan struct{}, 64)
	watcher.watched = func() {
		select {
		case read <- struct{}{}:
		default:
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		watcher.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	<-read
	return keeper, settled
}

// awaitSettled is the next apply the watcher made, or a failure naming what was expected.
func awaitSettled(t *testing.T, settled chan []config.Change, why string) []config.Change {
	t.Helper()
	select {
	case applied := <-settled:
		return applied
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: the watcher applied nothing within 10s", why)
		return nil
	}
}

// A file written in place is read again: the ordinary edit of a legion.yaml on a host.
func TestTheWatcherSettlesAFileWrittenInPlace(t *testing.T) {
	path := configFile(t, minimalFile)
	keeper, settled := watcherFor(t, path)

	rewrite(t, path, minimalFile+"max_fix_attempts: 9\n")

	applied := awaitSettled(t, settled, "a file written in place")
	if len(applied) != 1 || applied[0].Path != "max_fix_attempts" || applied[0].New != "9" {
		t.Fatalf("the watcher applied %+v, want max_fix_attempts 9", applied)
	}
	if got := keeper.Live().Snapshot().MaxFixAttempts; got != 9 {
		t.Fatalf("max_fix_attempts = %d, want the file's new 9", got)
	}
}

// A Kubernetes ConfigMap mounted as a volume is updated by writing a new timestamped directory
// beside the old one and swapping the `..data` symlink: the file the daemon opened is never
// written. This is that mount, written exactly as the kubelet writes one, and the watcher must
// see it — a watch bound to the file's own inode would see nothing at all, which is why the
// watcher watches the directory.
func TestTheWatcherSettlesAConfigMapWhoseDataSymlinkWasSwapped(t *testing.T) {
	mount := t.TempDir()
	write := func(stamp, body string) {
		t.Helper()
		dir := filepath.Join(mount, stamp)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("make %s: %v", dir, err)
		}
		rewrite(t, filepath.Join(dir, "legion.yaml"), body)
		link := filepath.Join(mount, "..data_tmp")
		if err := os.Symlink(stamp, link); err != nil {
			t.Fatalf("link %s: %v", link, err)
		}
		if err := os.Rename(link, filepath.Join(mount, "..data")); err != nil {
			t.Fatalf("swap ..data: %v", err)
		}
	}
	write("..2026_11_03_05_04_00.1", minimalFile)
	path := filepath.Join(mount, "legion.yaml")
	if err := os.Symlink(filepath.Join("..data", "legion.yaml"), path); err != nil {
		t.Fatalf("link %s: %v", path, err)
	}
	keeper, settled := watcherFor(t, path)

	write("..2026_11_03_06_00_00.2", minimalFile+"admission_cap: 11\n")

	applied := awaitSettled(t, settled, "a ConfigMap whose ..data symlink was swapped")
	if len(applied) != 1 || applied[0].Path != "admission_cap" || applied[0].New != "11" {
		t.Fatalf("the watcher applied %+v, want admission_cap 11", applied)
	}
	if got := keeper.Live().Snapshot().AdmissionCap; got != 11 {
		t.Fatalf("admission_cap = %d, want the mounted file's new 11", got)
	}
}

// A file a setting names is watched through its own directory too, so a credential rotated in
// place — which names no changed setting, since its path has not moved — still reaches the
// daemon, which is what asks its holders to read their files again (Watcher.Settled).
func TestTheWatcherSeesARotatedFileASettingNames(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "operator-token")
	rewrite(t, token, "first-token")
	path := configFile(t, minimalFile+"operator_token_file: "+token+"\n")
	keeper, settled := watcherFor(t, path)
	if got := WatchedFiles(keeper.Live().Snapshot()); len(got) != 1 || got[0] != token {
		t.Fatalf("the watched files are %v, want the operator token file alone", got)
	}

	rewrite(t, token, "second-token")

	if applied := awaitSettled(t, settled, "a credential rotated in place"); len(applied) != 0 {
		t.Fatalf("the rotation applied %+v, want no changed setting: the path did not move", applied)
	}
}

// The watcher applies nothing for the state it starts in: that state is what the daemon booted
// on, and an apply of it would announce a change nobody made.
func TestTheWatcherAppliesNothingUntilSomethingMoves(t *testing.T) {
	_, settled := watcherFor(t, configFile(t, minimalFile))

	select {
	case applied := <-settled:
		t.Fatalf("the watcher applied %+v with nothing changed", applied)
	case <-time.After(200 * time.Millisecond):
	}
}
