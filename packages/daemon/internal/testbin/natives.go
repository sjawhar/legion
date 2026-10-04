package testbin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
//
// pi-envoy's real-binary tests keep the same copy
// (packages/pi-envoy/extensions/test-omp-natives.ts): the same directory, name, layout, lock and
// fill, so either language fills it for the other. A change to one is a change to both.
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
// runs omp and the rest wait and find it filled. omp extracts into a staging HOME, and its natives
// directory is renamed into place whole once it holds every file the binary embeds at its embedded
// size, so a directory that exists is complete. A staging directory the lock's holder finds is a
// dead filler's, which it removes.
//
// omp writes the embedded files one after another, and when a later write fails (a full disk) it
// loads one it did write and still exits 0; a cache missing the other would have every later HOME
// extract it. The check against the embedded list is what refuses that fill.
func fillNativesCache(root, omp string) (string, error) {
	digest, embedded, err := readBinary(omp)
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
	extracted := map[string]int64{}
	if err := filepath.WalkDir(natives, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		extracted[entry.Name()] = info.Size()
		return os.Chmod(path, 0o500)
	}); err != nil {
		return "", fmt.Errorf("the natives %s extracted: %w", omp, err)
	}
	for _, name := range slices.Sorted(maps.Keys(embedded)) {
		size, ok := extracted[name]
		if !ok {
			return "", fmt.Errorf("%s embeds %s, but its extraction under %s lacks it (a write failed, as on a full disk), so nothing is cached", omp, name, natives)
		}
		if size != embedded[name] {
			return "", fmt.Errorf("%s embeds %s at %d bytes, but extracted it at %d under %s, so nothing is cached", omp, name, embedded[name], size, natives)
		}
	}
	if err := os.Rename(natives, cache); err != nil {
		return "", err
	}
	return cache, nil
}

// embeddedNative is one native file omp embeds, as pi-natives' scripts/embed-native.ts writes each
// into the compiled binary: `{ variant: "modern", filename: "pi_natives.linux-x64-modern.node",
// size: 185671184 },`. The scan finds each by its marker with bytes.Index, since a regexp over the
// whole binary takes seconds.
var (
	embeddedMarker = []byte(`filename: "pi_natives.`)
	embeddedNative = regexp.MustCompile(`^filename: "(pi_natives\.[^"/]+)", size: ([0-9]+)`)
)

// readBinary returns the digest that names omp's cache and the native files omp embeds, each with
// its size: what a complete extraction holds. A binary that lists none is refused, since then no
// extraction could be told from a partial one.
func readBinary(path string) (string, map[string]int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", nil, err
	}
	data, err := syscall.Mmap(int(file.Fd()), 0, int(info.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return "", nil, fmt.Errorf("map %s: %w", path, err)
	}
	defer func() { _ = syscall.Munmap(data) }()
	embedded := map[string]int64{}
	for rest := data; ; {
		at := bytes.Index(rest, embeddedMarker)
		if at < 0 {
			break
		}
		match := embeddedNative.FindSubmatch(rest[at:min(len(rest), at+256)])
		rest = rest[at+len(embeddedMarker):]
		if match == nil {
			continue
		}
		name := string(match[1])
		size, err := strconv.ParseInt(string(match[2]), 10, 64)
		if err != nil {
			return "", nil, fmt.Errorf("%s lists %s at size %q: %w", path, name, match[2], err)
		}
		if listed, ok := embedded[name]; ok && listed != size {
			return "", nil, fmt.Errorf("%s lists %s at %d bytes and at %d", path, name, listed, size)
		}
		embedded[name] = size
	}
	if len(embedded) == 0 {
		return "", nil, fmt.Errorf("%s lists no embedded natives (%s), so a fill could not tell a complete extraction from a partial one", path, embeddedNative)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16]), embedded, nil
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
