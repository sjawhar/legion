package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyBundleCountsAndRefusesAMissingFile(t *testing.T) {
	dir := t.TempDir()
	writeTestBundle(t, dir)
	v, err := verifyBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := os.ReadFile(filepath.Join(dir, bundleMetaFile))
	if err != nil {
		t.Fatal(err)
	}
	if v.Project != "PRJ" || v.Files != 5 || v.Issues != 1 || v.WindowEvents != 1 || v.Roster != 1 || v.MetaSHA256 != sha256Hex(meta) {
		t.Fatalf("verification %+v", v)
	}
	if err := os.Remove(filepath.Join(dir, "created-docs", "d2.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyBundle(dir); err == nil || !strings.Contains(err.Error(), "created-docs/d2.md") {
		t.Fatalf("a missing file was not refused by name: %v", err)
	}
}
