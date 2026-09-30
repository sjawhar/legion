package pmdoc

import (
	"bytes"
	"encoding/base64"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"golang.org/x/sys/unix"
)

// A gen script's result reaches the test that runs it whole, however late that test reads it. A
// loaded machine can leave the pipe unread for longer than a script takes to fill it, and a script
// that drops what a full pipe cannot take, as console.log does on the non-blocking stdout the
// headless editor's imports leave Bun with, prints a line cut short and still exits 0. Each case
// here prints more than a page into a pipe cut to one page that nothing reads until the script
// has exited or has held the pipe full for a second, so a script that does not wait for its reader
// is caught on every run, at any load.
func TestGenScriptsWaitForAStalledReader(t *testing.T) {
	if _, err := exec.LookPath("bun"); err != nil {
		if os.Getenv("CI") == "" {
			t.Skip("bun is not on PATH; the gen scripts are required in CI")
		}
		t.Fatalf("bun is required in CI: %v", err)
	}

	t.Run("decode.ts", func(t *testing.T) {
		fx := fixtureNamed(t, "footnote-placement")
		want, err := FromJSON(fx.PMJSON)
		if err != nil {
			t.Fatal(err)
		}
		doc := crdt.New(crdt.WithClientID(1))
		frag := doc.GetXmlFragment("prosemirror")
		doc.Transact(func(txn *crdt.Transaction) {
			err = Update(txn, frag, want)
		})
		if err != nil {
			t.Fatal(err)
		}
		line := stalledReaderLine(t, "decode.ts", base64.StdEncoding.EncodeToString(crdt.EncodeStateAsUpdateV1(doc, nil)))
		decoded, err := FromJSON(line)
		if err != nil {
			t.Fatalf("decode.ts output: %v", err)
		}
		if !decoded.Equal(asTheLiveDocumentHoldsIt(want)) {
			t.Fatal("decode.ts decodes the Go-authored bytes differently through a stalled reader")
		}
	})

	// TestNullAttributesSurviveABrowserEdit's document, with a paragraph long enough to fill the pipe.
	t.Run("edit-blocks.ts", func(t *testing.T) {
		long := strings.TrimSpace(strings.Repeat("filler ", 3000))
		written, err := Parse("| a | b | c |\n| --- | :---: | ---: |\n| d | e | f |\n\n```\ncode\n```\n\n![alt](src.png) tail\n\n" + long + "\n")
		if err != nil {
			t.Fatal(err)
		}
		doc := crdt.New(crdt.WithClientID(1))
		frag := doc.GetXmlFragment("prosemirror")
		doc.Transact(func(txn *crdt.Transaction) {
			err = Update(txn, frag, written)
		})
		if err != nil {
			t.Fatal(err)
		}
		line := stalledReaderLine(t, "edit-blocks.ts", base64.StdEncoding.EncodeToString(crdt.EncodeStateAsUpdateV1(doc, nil)))
		update, err := base64.StdEncoding.DecodeString(string(line))
		if err != nil {
			t.Fatalf("edit-blocks.ts output: %v", err)
		}
		edited := crdt.New()
		if err := crdt.ApplyUpdateV1(edited, update, nil); err != nil {
			t.Fatal(err)
		}
		tree, err := Read(edited.GetXmlFragment("prosemirror"))
		if err != nil {
			t.Fatal(err)
		}
		markdown, err := Render(tree)
		if err != nil {
			t.Fatal(err)
		}
		if want := "| ax | b | c |\n| --- | :---: | ---: |\n| dy | e | f |\n\n```\ncodez\n```\n\n![alt2](src.png) tail\n\n" + long + "\n"; markdown != want {
			t.Fatalf("after edit-blocks.ts's edits through a stalled reader, the document renders %q, want %q", markdown, want)
		}
	})
}

// stalledReaderLine runs gen/<script> on arg with its stdout a pipe cut to one page, which nothing
// reads until the script has exited or has held the pipe full for a second, then reads the pipe
// to its end and returns the line the script printed, refused as the tests' runner refuses it
// (wholeLine).
func stalledReaderLine(t *testing.T, script, arg string) []byte {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	if _, err := unix.FcntlInt(write.Fd(), unix.F_SETPIPE_SZ, os.Getpagesize()); err != nil {
		t.Fatalf("cut the pipe to one page: %v", err)
	}
	capacity, err := unix.FcntlInt(write.Fd(), unix.F_GETPIPE_SZ, 0)
	if err != nil {
		t.Fatal(err)
	}
	cmd := genScript(script, arg)
	cmd.Stdout = write
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	write.Close()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	var waitErr error
	var fullSince time.Time
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
stall:
	for {
		select {
		case waitErr = <-exited:
			exited = nil
			break stall
		case now := <-tick.C:
			// The bytes the pipe holds (TIOCINQ is Linux's FIONREAD).
			queued, err := unix.IoctlGetInt(int(read.Fd()), unix.TIOCINQ)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case queued < capacity:
				fullSince = time.Time{}
			case fullSince.IsZero():
				fullSince = now
			case now.Sub(fullSince) >= time.Second:
				break stall
			}
		}
	}
	type drain struct {
		output []byte
		err    error
	}
	drained := make(chan drain, 1)
	go func() {
		output, err := io.ReadAll(read)
		drained <- drain{output, err}
	}()
	if exited != nil {
		select {
		case waitErr = <-exited:
		case <-time.After(time.Minute):
			_ = cmd.Process.Kill()
			t.Fatalf("%s did not exit in the minute after its reader began draining the pipe", script)
		}
	}
	got := <-drained
	if got.err != nil {
		t.Fatal(got.err)
	}
	output := got.output
	if waitErr != nil {
		t.Fatalf("%s: %v\nstderr:\n%s", script, waitErr, stderr.Bytes())
	}
	line, err := wholeLine(script, output)
	if err != nil {
		t.Fatalf("through a stalled reader: %v", err)
	}
	if len(output) <= capacity {
		t.Fatalf("%s printed %d bytes, no more than the %d-byte pipe holds, so the reader never stalled it", script, len(output), capacity)
	}
	return line
}
