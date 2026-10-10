package sandbox

import (
	"archive/zip"
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testbin"
)

// uv puts a package into a .venv from its cache, and with the two on one filesystem it hardlinks
// them unless told to copy. The worker container keeps uv's cache on the tree volume beside every
// workspace of the tree (TestUvKeepsItsPythonsAndCacheOnTheTreeVolume), so a hardlinked install
// would hand one agent's in-place edit inside its .venv to the cache and to every other .venv of the
// tree that installed the package: the shared-inode failure TestBunCacheHomeIsMountedFromNoVolume
// keeps bun's cache from. Installed with the worker container's UV_LINK_MODE by the uv the image
// ships, no file of the .venv is an inode the cache holds. The same install with
// UV_LINK_MODE=hardlink shares them, so the check sees the failure it guards.
func TestUvCacheSharesNoInodeWithAVenv(t *testing.T) {
	uv := testbin.UV(t)
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	linkMode := envOf(workerOf(t, r, workerSpec(t), false))["UV_LINK_MODE"]
	wheel := writeWheel(t)
	if shared := uvSharedInodes(t, uv, wheel, linkMode); len(shared) != 0 {
		t.Errorf("installed with the worker container's UV_LINK_MODE %q, these .venv files are inodes the cache holds: %v", linkMode, shared)
	}
	if shared := uvSharedInodes(t, uv, wheel, "hardlink"); len(shared) == 0 {
		t.Fatal("installed with UV_LINK_MODE=hardlink, no .venv file is an inode the cache holds, so this check cannot see a shared cache")
	}
}

// writeWheel writes demo 1.0, a one-module wheel uv installs from its path with no index.
func writeWheel(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "demo-1.0-py3-none-any.whl")
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, entry := range []struct{ name, body string }{
		{"demo/__init__.py", "VALUE = 1\n"},
		{"demo-1.0.dist-info/METADATA", "Metadata-Version: 2.1\nName: demo\nVersion: 1.0\n"},
		{"demo-1.0.dist-info/WHEEL", "Wheel-Version: 1.0\nGenerator: legion-test\nRoot-Is-Purelib: true\nTag: py3-none-any\n"},
		{"demo-1.0.dist-info/RECORD", "demo/__init__.py,,\ndemo-1.0.dist-info/METADATA,,\ndemo-1.0.dist-info/WHEEL,,\ndemo-1.0.dist-info/RECORD,,\n"},
	} {
		part, err := w.Create(entry.name)
		if err == nil {
			_, err = part.Write([]byte(entry.body))
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// uvSharedInodes makes a .venv and a uv cache beside it on one filesystem, as a workspace and the
// tree's cache are on the tree volume, installs wheel into the .venv under linkMode (uv's default
// when empty), and returns the installed package's files that are inodes the cache also holds.
func uvSharedInodes(t *testing.T, uv, wheel, linkMode string) []string {
	t.Helper()
	dir := t.TempDir()
	cache, venv := filepath.Join(dir, "cache"), filepath.Join(dir, "venv")
	run := func(args ...string) {
		cmd := exec.Command(uv, args...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "UV_CACHE_DIR=" + cache}
		if linkMode != "" {
			cmd.Env = append(cmd.Env, "UV_LINK_MODE="+linkMode)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("uv %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("venv", "--quiet", "--no-config", "--offline", "--no-python-downloads", venv)
	run("pip", "install", "--quiet", "--no-config", "--offline", "--python", filepath.Join(venv, "bin", "python"), wheel)
	inodes := func(root string) map[[2]uint64]string {
		found := map[[2]uint64]string{}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			st := info.Sys().(*syscall.Stat_t)
			found[[2]uint64{uint64(st.Dev), st.Ino}] = path
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return found
	}
	packages, err := filepath.Glob(filepath.Join(venv, "lib", "python*", "site-packages", "demo"))
	if err != nil || len(packages) != 1 {
		t.Fatalf("the .venv holds no installed demo package (%v, %v)", packages, err)
	}
	cached := inodes(cache)
	var shared []string
	for key, path := range inodes(packages[0]) {
		if _, ok := cached[key]; ok {
			shared = append(shared, path)
		}
	}
	slices.Sort(shared)
	return shared
}
