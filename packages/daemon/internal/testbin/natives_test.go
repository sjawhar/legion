package testbin

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// standIn writes, as omp under dir, a script that embeds the natives list lines (in the format the
// real binary carries them) and runs body.
func standIn(t *testing.T, dir string, list []string, body string) string {
	t.Helper()
	omp := filepath.Join(dir, "omp")
	script := "#!/bin/sh\n"
	for _, line := range list {
		script += "# " + line + "\n"
	}
	if err := os.WriteFile(omp, []byte(script+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return omp
}

// Of eight lookups that race for one binary's natives cache, as test binaries under `go test ./...`
// do, one runs omp and the rest find its copy; each HOME then holds that copy's inode rather than
// bytes of its own, and the copy takes no write through a link. The staging directory of a filler
// that died is gone once the cache is filled. The omp here is a script that extracts the one natives
// file it embeds into its HOME, as the real binary does, and counts its runs.
func TestTheNativesCacheRunsOmpOnceAndEveryHomeLinksTheOneCopy(t *testing.T) {
	dir := t.TempDir()
	runs := filepath.Join(dir, "runs")
	omp := standIn(t, dir, []string{`{ variant: "default", filename: "pi_natives.test.node", size: 6 },`},
		"echo run >>"+runs+"\nmkdir -p \"$HOME/.omp/natives/9.9.9\"\nprintf native >\"$HOME/.omp/natives/9.9.9/pi_natives.test.node\"\n")
	root := filepath.Join(dir, "cache")
	digest, _, err := readBinary(omp)
	if err != nil {
		t.Fatal(err)
	}
	dead := filepath.Join(root, digest+".fill-dead")
	if err := os.MkdirAll(filepath.Join(dead, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	caches, errs := make([]string, 8), make([]error, 8)
	var lookups sync.WaitGroup
	for i := range caches {
		lookups.Go(func() { caches[i], errs[i] = fillNativesCache(root, omp) })
	}
	lookups.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("lookup %d: %v", i, err)
		}
		if caches[i] != caches[0] {
			t.Fatalf("lookups named %s and %s, want one cache", caches[0], caches[i])
		}
	}
	if got, err := os.ReadFile(runs); err != nil || string(got) != "run\n" {
		t.Fatalf("omp's runs read %q (%v), want one run", got, err)
	}
	if _, err := os.Stat(dead); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the dead filler's staging directory %s stats %v, want it removed", dead, err)
	}

	cached, err := os.Stat(filepath.Join(caches[0], "9.9.9", "pi_natives.test.node"))
	if err != nil {
		t.Fatal(err)
	}
	if cached.Mode().Perm()&0o222 != 0 {
		t.Errorf("the cached natives file is %v, want it read-only", cached.Mode().Perm())
	}
	for _, home := range []string{filepath.Join(dir, "a"), filepath.Join(dir, "b")} {
		if err := linkTree(caches[0], filepath.Join(home, ".omp", "natives")); err != nil {
			t.Fatal(err)
		}
		linked, err := os.Stat(filepath.Join(home, ".omp", "natives", "9.9.9", "pi_natives.test.node"))
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(cached, linked) {
			t.Errorf("%s's natives file is a file of its own, want the cache's", home)
		}
	}
}

// A fill whose extraction lacks a file the binary embeds is refused and caches nothing, as when a
// full disk fails omp's second write and omp still exits 0; the next fill, whose extraction holds
// both files, is cached. The stand-in embeds two files, writes one on its first run and both after.
func TestANativesFillMissingAnEmbeddedFileCachesNothing(t *testing.T) {
	dir := t.TempDir()
	runs := filepath.Join(dir, "runs")
	omp := standIn(t, dir, []string{
		`{ variant: "modern", filename: "pi_natives.test-modern.node", size: 6 },`,
		`{ variant: "baseline", filename: "pi_natives.test-baseline.node", size: 8 },`,
	}, "natives=\"$HOME/.omp/natives/9.9.9\"\nmkdir -p \"$natives\"\nprintf modern >\"$natives/pi_natives.test-modern.node\"\n"+
		"[ -e "+runs+" ] && printf baseline >\"$natives/pi_natives.test-baseline.node\"\necho run >>"+runs+"\n")
	root := filepath.Join(dir, "cache")
	digest, _, err := readBinary(omp)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := fillNativesCache(root, omp); err == nil || !strings.Contains(err.Error(), "embeds pi_natives.test-baseline.node, but its extraction") {
		t.Fatalf("the fill whose extraction lacks the baseline file = %v, want it refused naming that file", err)
	}
	if _, err := os.Stat(filepath.Join(root, digest)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("after the refused fill the cache stats %v, want no cache", err)
	}
	cache, err := fillNativesCache(root, omp)
	if err != nil {
		t.Fatalf("the fill whose extraction holds both files = %v, want it cached", err)
	}
	for _, name := range []string{"pi_natives.test-modern.node", "pi_natives.test-baseline.node"} {
		if _, err := os.Stat(filepath.Join(cache, "9.9.9", name)); err != nil {
			t.Errorf("the cache lacks %s: %v", name, err)
		}
	}
}

// A binary that lists no embedded natives is refused before it runs: with nothing to check an
// extraction against, a partial one would be cached.
func TestANativesFillRefusesABinaryThatListsNoEmbeddedNatives(t *testing.T) {
	dir := t.TempDir()
	runs := filepath.Join(dir, "runs")
	omp := standIn(t, dir, nil, "echo run >>"+runs+"\nmkdir -p \"$HOME/.omp/natives/9.9.9\"\nprintf native >\"$HOME/.omp/natives/9.9.9/pi_natives.test.node\"\n")
	if _, err := fillNativesCache(filepath.Join(dir, "cache"), omp); err == nil || !strings.Contains(err.Error(), "lists no embedded natives") {
		t.Fatalf("the fill of a binary that lists no natives = %v, want it refused", err)
	}
	if _, err := os.Stat(runs); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refused binary ran (%v), want it never run", err)
	}
}
