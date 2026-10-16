package lookpath

import (
	"os"
	"path/filepath"
	"testing"
)

// On resolves an executable on the PATH it is given, skipping a relative element (as
// exec.LookPath refuses the result it would give), an empty one, a directory sharing the name,
// and a file without an execute bit; among several matches the first element wins; a name on no
// element answers false.
func TestOnReadsThePATHItIsGiven(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "notes"), []byte("text"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(bin, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	t.Chdir(workdir)
	if err := os.MkdirAll(filepath.Join(workdir, "relative", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "relative", "bin", "tool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	searchPath := "relative/bin:" + bin
	if got, ok := On(searchPath, "tool"); !ok || got != filepath.Join(bin, "tool") {
		t.Errorf("On(tool) = %q, %t, want %s", got, ok, filepath.Join(bin, "tool"))
	}
	for _, name := range []string{"notes", "dir", "sh"} {
		if got, ok := On(searchPath, name); ok {
			t.Errorf("On(%s) = %q, want none: not an executable on the given PATH", name, got)
		}
	}
	if _, ok := On("", "tool"); ok {
		t.Error("On found tool on an empty PATH")
	}
	if got, ok := On("relative/bin", "tool"); ok {
		t.Errorf("On(relative/bin) = %q, %t, want none: a relative PATH element alone is skipped even though it resolves", got, ok)
	}
	absBin := filepath.Join(workdir, "relative", "bin")
	if got, ok := On(absBin, "tool"); !ok || got != filepath.Join(absBin, "tool") {
		t.Errorf("On(%s) = %q, %t, want %s", absBin, got, ok, filepath.Join(absBin, "tool"))
	}
}

// Among two elements both carrying the name, the first wins.
func TestOnPrefersTheFirstMatchingElement(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(first, "tool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "tool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	searchPath := first + string(filepath.ListSeparator) + second
	if got, ok := On(searchPath, "tool"); !ok || got != filepath.Join(first, "tool") {
		t.Errorf("On(tool) = %q, %t, want the first element's %s", got, ok, filepath.Join(first, "tool"))
	}
}
