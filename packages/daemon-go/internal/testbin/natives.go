package testbin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

// OMPHome puts in home, a fresh HOME a test runs omp under, the native modules omp would otherwise
// write there on its first run: two .node files of about 177 MiB each at the pin, which omp
// extracts from its own binary into .omp/natives/<version> under any HOME that lacks them. Oh My
// Pi has no setting for that directory short of XDG_DATA_HOME, which moves its whole profile. Each
// file is instead a hardlink to one copy per omp binary under the user's cache directory, so a case
// writes none of them and its HOME holds nothing of the operator's own ~/.omp. The copy's files
// are read-only: every test HOME shares their inodes, so a write through one link would reach all.
//
// It assumes the test's environment names no XDG_DATA_HOME, as no test that runs omp does: omp
// keeps its natives under $XDG_DATA_HOME/omp when that directory exists.
func OMPHome(t *testing.T, omp, home string) {
	t.Helper()
	cache, err := nativesCache(omp)
	if err != nil {
		t.Fatalf("the natives cache for %s: %v", omp, err)
	}
	if err := linkTree(cache, filepath.Join(home, ".omp", "natives")); err != nil {
		t.Fatalf("link %s's natives into %s: %v", omp, home, err)
	}
}

// nativesCaches holds each omp path's one lookup in this process, since naming the cache reads the
// whole binary.
var nativesCaches sync.Map

func nativesCache(omp string) (string, error) {
	lookup, _ := nativesCaches.LoadOrStore(omp, sync.OnceValues(func() (string, error) {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		return fillNativesCache(filepath.Join(cache, "legion", "test-omp-natives"), omp)
	}))
	return lookup.(func() (string, error))()
}

// fillNativesCache returns the directory under root holding the natives omp extracts, named for the
// binary's content, so another pin or another build of one version gets its own. The first process
// to ask fills it under an exclusive lock on its own lock file, so of the test binaries that ask at
// once (`go test ./...` runs packages together, and checkouts on one machine share the cache) one
// runs omp and the rest wait and find it filled. omp extracts into a staging HOME whose natives
// directory is then renamed into place whole, so a directory that exists is complete, and a staging
// directory the lock's holder finds is a dead filler's, which it removes.
func fillNativesCache(root, omp string) (string, error) {
	digest, err := contentDigest(omp)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	cache := filepath.Join(root, digest)
	lock, err := os.OpenFile(cache+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return "", fmt.Errorf("lock %s: %w", lock.Name(), err)
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	if _, err := os.Stat(cache); err == nil {
		return cache, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	stale, err := filepath.Glob(cache + ".fill-*")
	if err != nil {
		return "", err
	}
	for _, dead := range stale {
		if err := os.RemoveAll(dead); err != nil {
			return "", err
		}
	}
	staging, err := os.MkdirTemp(root, digest+".fill-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	home := filepath.Join(staging, "home")
	// Any command that loads the natives extracts them; this one reads a setting and calls nothing.
	extract := exec.Command(omp, "config", "get", "compaction.remoteEndpoint", "--json")
	extract.Dir, extract.Env = staging, []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	if out, err := extract.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%s config get under the fresh HOME %s: %w\n%s", omp, home, err, out)
	}
	natives := filepath.Join(home, ".omp", "natives")
	files := 0
	if err := filepath.WalkDir(natives, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		files++
		return os.Chmod(path, 0o500)
	}); err != nil {
		return "", fmt.Errorf("the natives %s extracted: %w", omp, err)
	}
	if files == 0 {
		return "", fmt.Errorf("%s extracted no natives under %s", omp, natives)
	}
	if err := os.Rename(natives, cache); err != nil {
		return "", err
	}
	return cache, nil
}

func contentDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return hex.EncodeToString(hash.Sum(nil)[:16]), nil
}

// linkTree makes each directory under source at destination and hardlinks each file into it.
func linkTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		err = os.Link(path, target)
		if errors.Is(err, syscall.EXDEV) {
			return fmt.Errorf("%w: the cache and the test's temporary directory are on different filesystems; point XDG_CACHE_HOME or TMPDIR so they share one", err)
		}
		return err
	})
}
