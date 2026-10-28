package main

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// replay-pack moves a bundle as zstd-compressed tar parts small enough to attach anywhere;
// replay-unpack reassembles them. Packing is deterministic: sorted paths, mtime 0, mode 0644,
// uid/gid 0 and a single-threaded encoder, so identical bundles pack to identical bytes.

// packResult is what replay-pack prints.
type packResult struct {
	TarZstSHA256 string     `json:"tar_zst_sha256"`
	Bytes        int64      `json:"bytes"`
	Parts        []packPart `json:"parts"`
}

type packPart struct {
	File   string `json:"file"` // base name, <prefix base>.partNN
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

func runReplayPack(args []string) error {
	fl := flag.NewFlagSet("replay-pack", flag.ContinueOnError)
	in := fl.String("in", "", "bundle directory")
	out := fl.String("out", "", "part prefix: parts are <prefix>.part00, .part01, ...")
	partSize := fl.String("part-size", "24MiB", "largest part, in bytes or with a KiB, MiB or GiB suffix")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *in == "" || *out == "" {
		return fmt.Errorf("-in and -out are required")
	}
	size, err := parseByteSize(*partSize)
	if err != nil {
		return fmt.Errorf("-part-size: %w", err)
	}
	res, err := packBundle(*in, *out, size)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(b))
	return nil
}

// parseByteSize reads "1048576", "512KiB", "24MiB" or "1GiB".
func parseByteSize(s string) (int64, error) {
	mult := int64(1)
	for suffix, m := range map[string]int64{"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30} {
		if strings.HasSuffix(s, suffix) {
			s, mult = strings.TrimSuffix(s, suffix), m
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not a positive size", s)
	}
	return n * mult, nil
}

// packBundle writes dir's bundle files (raw/ excluded) as a tar, zstd-compressed and split into
// parts of at most partSize bytes.
func packBundle(dir, prefix string, partSize int64) (*packResult, error) {
	sp := &splitWriter{prefix: prefix, size: partSize, whole: sha256.New()}
	enc, err := zstd.NewWriter(sp, zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(enc)
	err = walkBundle(dir, func(rel, abs string) error {
		f, err := os.Open(abs)
		if err != nil {
			return err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Typeflag: tar.TypeReg, Name: rel, Size: st.Size(), Mode: 0o644,
			ModTime: time.Unix(0, 0),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		return err
	})
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = enc.Close()
	}
	if err == nil {
		err = sp.closePart()
	}
	if err != nil {
		sp.abort()
		return nil, err
	}
	return &packResult{TarZstSHA256: hex.EncodeToString(sp.whole.Sum(nil)), Bytes: sp.total, Parts: sp.parts}, nil
}

// splitWriter writes a stream as <prefix>.partNN files of at most size bytes each.
type splitWriter struct {
	prefix string
	size   int64
	whole  hash.Hash
	total  int64
	cur    *os.File
	curSum hash.Hash
	curLen int64
	parts  []packPart
}

func (s *splitWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if s.cur == nil || s.curLen == s.size {
			if err := s.closePart(); err != nil {
				return written, err
			}
			name := fmt.Sprintf("%s.part%02d", s.prefix, len(s.parts))
			f, err := os.Create(name)
			if err != nil {
				return written, err
			}
			s.cur, s.curSum, s.curLen = f, sha256.New(), 0
		}
		chunk := p
		if room := s.size - s.curLen; int64(len(chunk)) > room {
			chunk = chunk[:room]
		}
		n, err := s.cur.Write(chunk)
		s.curSum.Write(chunk[:n])
		s.whole.Write(chunk[:n])
		s.curLen += int64(n)
		s.total += int64(n)
		written += n
		if err != nil {
			return written, err
		}
		p = p[n:]
	}
	return written, nil
}

func (s *splitWriter) closePart() error {
	if s.cur == nil {
		return nil
	}
	err := s.cur.Close()
	s.parts = append(s.parts, packPart{File: filepath.Base(s.cur.Name()), Bytes: s.curLen, SHA256: hex.EncodeToString(s.curSum.Sum(nil))})
	s.cur = nil
	return err
}

// abort removes the parts written so far.
func (s *splitWriter) abort() {
	_ = s.closePart()
	for _, p := range s.parts {
		_ = os.Remove(filepath.Join(filepath.Dir(s.prefix), p.File))
	}
}

func runReplayUnpack(args []string) error {
	fl := flag.NewFlagSet("replay-unpack", flag.ContinueOnError)
	out := fl.String("out", "", "directory to unpack into")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *out == "" || fl.NArg() == 0 {
		return fmt.Errorf("usage: loadgen replay-unpack -out <dir> <parts in order...>")
	}
	return unpackBundle(*out, fl.Args())
}

// unpackBundle extracts the parts, in order, into out. It refuses an entry whose path is absolute
// or leaves out, and any entry but a regular file or a directory.
func unpackBundle(out string, parts []string) error {
	var readers []io.Reader
	for _, p := range parts {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		readers = append(readers, f)
	}
	dec, err := zstd.NewReader(io.MultiReader(readers...), zstd.WithDecoderConcurrency(1))
	if err != nil {
		return err
	}
	defer dec.Close()
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(dec)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		rel, err := safeEntryPath(hdr.Name)
		if err != nil {
			return err
		}
		dst := filepath.Join(out, filepath.FromSlash(rel))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("tar entry %q: type %q is not a regular file or directory", hdr.Name, hdr.Typeflag)
		}
	}
}

// safeEntryPath is a tar entry's cleaned slash path, refused when absolute or escaping the root.
func safeEntryPath(name string) (string, error) {
	if name == "" || path.IsAbs(name) || filepath.IsAbs(name) || strings.Contains(name, `\`) {
		return "", fmt.Errorf("tar entry %q: absolute or not a slash path", name)
	}
	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("tar entry %q escapes the output directory", name)
	}
	return clean, nil
}
