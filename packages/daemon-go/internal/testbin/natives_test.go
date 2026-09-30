package testbin

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Of eight lookups that race for one binary's natives cache, as test binaries under `go test ./...`
// do, one runs omp and the rest find its copy; each HOME then holds that copy's inode rather than
// bytes of its own, and the copy takes no write through a link. The staging directory of a filler
// that died is gone once the cache is filled. The omp here is a script that extracts a natives file
// into its HOME as the real binary does, and counts its runs.
func TestTheNativesCacheRunsOmpOnceAndEveryHomeLinksTheOneCopy(t *testing.T) {
	dir := t.TempDir()
	runs, omp := filepath.Join(dir, "runs"), filepath.Join(dir, "omp")
	script := "#!/bin/sh\necho run >>" + runs + "\nmkdir -p \"$HOME/.omp/natives/9.9.9\"\n" +
		"printf native >\"$HOME/.omp/natives/9.9.9/pi_natives.test.node\"\n"
	if err := os.WriteFile(omp, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "cache")
	digest, err := contentDigest(omp)
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
