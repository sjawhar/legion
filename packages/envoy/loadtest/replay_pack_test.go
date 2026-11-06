package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestPackUnpackRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeTestBundle(t, dir)
	// A larger incompressible-ish file so the stream spans several parts.
	big := make([]byte, 64<<10)
	x := uint32(1)
	for i := range big {
		x = x*1664525 + 1013904223
		big[i] = byte(x >> 24)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "big.md"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	a, err := packBundle(dir, filepath.Join(outDir, "a"), 16<<10)
	if err != nil {
		t.Fatal(err)
	}
	b, err := packBundle(dir, filepath.Join(outDir, "b"), 16<<10)
	if err != nil {
		t.Fatal(err)
	}
	if a.TarZstSHA256 != b.TarZstSHA256 || a.Bytes != b.Bytes || len(a.Parts) < 2 {
		t.Fatalf("packs differ or did not split: %+v %+v", a, b)
	}
	var parts []string
	var total int64
	for i, p := range a.Parts {
		if p.SHA256 != b.Parts[i].SHA256 || p.Bytes > 16<<10 {
			t.Fatalf("part %d: %+v vs %+v", i, p, b.Parts[i])
		}
		total += p.Bytes
		parts = append(parts, filepath.Join(outDir, p.File))
	}
	if total != a.Bytes || a.Parts[0].File != "a.part00" {
		t.Fatalf("parts %+v", a.Parts)
	}
	dst := t.TempDir()
	if err := unpackBundle(dst, parts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "raw")); !os.IsNotExist(err) {
		t.Fatal("raw/ was packed")
	}
	if _, err := readBundle(dst); err != nil {
		t.Fatalf("unpacked bundle does not verify: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "docs", "big.md"))
	if err != nil || !bytes.Equal(got, big) {
		t.Fatal("big file did not round-trip")
	}
}

// tarZst is a one-entry zstd tar.
func tarZst(t *testing.T, hdr *tar.Header, body []byte) string {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(enc)
	hdr.Size = int64(len(body))
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "evil.part00")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUnpackRefusesEscapes(t *testing.T) {
	for _, name := range []string{"../x", "/abs/x", "docs/../../x"} {
		root := t.TempDir()
		out := filepath.Join(root, "out")
		p := tarZst(t, &tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644}, []byte("x"))
		if err := unpackBundle(out, []string{p}); err == nil {
			t.Errorf("%s: not refused", name)
		}
		if _, err := os.Stat(filepath.Join(root, "x")); err == nil {
			t.Errorf("%s: wrote outside -out", name)
		}
	}
	p := tarZst(t, &tar.Header{Typeflag: tar.TypeSymlink, Name: "link", Linkname: "/etc/passwd"}, nil)
	if err := unpackBundle(t.TempDir(), []string{p}); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("symlink not refused: %v", err)
	}
}

func TestParseByteSize(t *testing.T) {
	for in, want := range map[string]int64{"24MiB": 24 << 20, "512KiB": 512 << 10, "1GiB": 1 << 30, "100": 100} {
		if got, err := parseByteSize(in); err != nil || got != want {
			t.Errorf("parseByteSize(%q) = %d %v, want %d", in, got, err, want)
		}
	}
	if _, err := parseByteSize("0"); err == nil {
		t.Error("0 accepted")
	}
}
