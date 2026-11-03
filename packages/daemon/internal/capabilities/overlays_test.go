package capabilities

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// OverlayEntries leaves out the empty entries Oh My Pi leaves out.
func TestOverlayEntriesSkipsEmptyEntries(t *testing.T) {
	for value, want := range map[string][]string{
		"":                  nil,
		"/a.yml":            {"/a.yml"},
		"/a.yml:":           {"/a.yml"},
		":/a.yml":           {"/a.yml"},
		"/a.yml::/b.yml":    {"/a.yml", "/b.yml"},
		"/a.yml:/b.yml:::/": {"/a.yml", "/b.yml", "/"},
	} {
		if got := OverlayEntries(value); !slices.Equal(got, want) {
			t.Errorf("OverlayEntries(%q) = %q, want %q", value, got, want)
		}
	}
}

// CheckOverlays refuses only an overlay a run of Oh My Pi exits non-zero on, or one it cannot read,
// naming the file and Oh My Pi's own error line however long the output before it; an answer it
// cannot read as JSON, on exit 0, is no refusal, since the overlays started.
func TestCheckOverlaysRefusesOnlyANonZeroExit(t *testing.T) {
	dir := t.TempDir()
	omp := filepath.Join(dir, "omp")
	script := "#!/bin/sh\nIFS=:\nfor f in $PI_CONFIG_FILES; do case \"$f\" in *bad*)\n" +
		"  echo '" + strings.Repeat("y", 500) + "' >&2; echo \"error: Failed to parse config overlay $f: SyntaxError\" >&2; exit 1;; esac; done\n" +
		"echo not-json\n"
	if err := os.WriteFile(omp, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	good, bad, missing := filepath.Join(dir, "good.yml"), filepath.Join(dir, "bad.yml"), filepath.Join(dir, "missing.yml")
	for _, file := range []string{good, bad} {
		if err := os.WriteFile(file, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	img := Image{Launch: omp, Env: []string{"PATH=" + os.Getenv("PATH"), "PI_CONFIG_FILES=/elsewhere.yml"}, WorkDir: dir}
	for _, tc := range []struct {
		name          string
		overlays, own []string
		want          string
	}{
		{"a file that starts", []string{good}, []string{good}, ""},
		{"a missing file", []string{good, missing}, []string{missing}, missing + " cannot be read: stat " + missing + ": no such file or directory"},
		{"a file Oh My Pi refuses", []string{good, bad}, []string{bad}, "Oh My Pi refuses " + bad + ": error: Failed to parse config overlay " + bad + ": SyntaxError"},
		{"the list Oh My Pi refuses", []string{bad, good}, []string{good}, "Oh My Pi refuses PI_CONFIG_FILES " + bad + ":" + good + ": error: Failed to parse config overlay " + bad + ": SyntaxError"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CheckOverlays(context.Background(), img, tc.overlays, tc.own)
			if err != nil || got != tc.want {
				t.Fatalf("CheckOverlays = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// A run that never reaches an exit of its own (here a timeout) does not stop the check: it is
// returned as err, and a role file Oh My Pi refuses after it is still refused.
func TestCheckOverlaysKeepsCheckingAfterATimeout(t *testing.T) {
	was := commandTimeout
	commandTimeout = 300 * time.Millisecond
	t.Cleanup(func() { commandTimeout = was })
	dir := t.TempDir()
	omp := filepath.Join(dir, "omp")
	script := "#!/bin/sh\ncase \"$PI_CONFIG_FILES\" in *slow.yml) sleep 5;; *bad.yml) echo \"error: Failed to parse config overlay $PI_CONFIG_FILES\" >&2; exit 1;; esac\necho '{\"value\": false}'\n"
	if err := os.WriteFile(omp, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	slow, bad := filepath.Join(dir, "slow.yml"), filepath.Join(dir, "bad.yml")
	for _, file := range []string{slow, bad} {
		if err := os.WriteFile(file, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	img := Image{Launch: omp, Env: []string{"PATH=" + os.Getenv("PATH")}, WorkDir: dir}
	got, err := CheckOverlays(context.Background(), img, []string{slow, bad}, []string{slow, bad})
	if want := "Oh My Pi refuses " + bad + ": error: Failed to parse config overlay " + bad; got != want {
		t.Errorf("CheckOverlays refused %q, want %q", got, want)
	}
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("CheckOverlays err = %v, want the slow file's timeout", err)
	}
}
