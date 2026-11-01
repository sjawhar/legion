package store

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A released issue's bundle round-trips whole — its bytes, the commit its working copy sat on, the
// heads it carries and the bookmarks that pointed into it — survives a second release replacing
// it, and goes when the restore is confirmed.
func TestAWorkspaceBundleRoundTripsAndIsDeletedOnConfirmation(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	at := time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)
	saved := runtime.WorkspaceBundle{
		Bundle:    bytes.Repeat([]byte{0, 1, 2, 3, 0xff}, 1024),
		Head:      "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		Tips:      []string{"da4f9f492e9f8349389c243743e0a000cd1c676f", "f2f134541b30dfa5160a5d1ce5e17b76c967c661"},
		Bookmarks: map[string]string{"legion/INFRA-945": "dffe88ae2f793e13436cef3921adc988433fbd4e", "side": "da4f9f492e9f8349389c243743e0a000cd1c676f"},
		At:        at,
	}
	if _, ok, err := s.WorkspaceBundle(ctx, "INFRA-945"); err != nil || ok {
		t.Fatalf("an issue never released reads %v, %v; want no bundle", ok, err)
	}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", saved); err != nil {
		t.Fatal(err)
	}
	read, ok, err := s.WorkspaceBundle(ctx, "INFRA-945")
	if err != nil || !ok {
		t.Fatalf("read the saved bundle: %v, %v", ok, err)
	}
	if !bytes.Equal(read.Bundle, saved.Bundle) || read.Size() != saved.Size() {
		t.Fatalf("the bundle read back is %d bytes, want the %d saved", read.Size(), saved.Size())
	}
	if read.Head != saved.Head || !slices.Equal(read.Tips, saved.Tips) || !maps.Equal(read.Bookmarks, saved.Bookmarks) {
		t.Fatalf("the bundle read back is %+v, want %+v", read, saved)
	}
	if !read.At.Equal(at) {
		t.Fatalf("the bundle was taken at %s, want %s", read.At, at)
	}

	// A second release of the same issue replaces the row: the older work must not be restored
	// in place of the newer.
	again := runtime.WorkspaceBundle{Bundle: []byte("PACK newer"), Head: "0a8f0aee63b13682fefe975ba0cc51da6f2ba9e9", At: at.Add(time.Hour)}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", again); err != nil {
		t.Fatal(err)
	}
	read, ok, err = s.WorkspaceBundle(ctx, "INFRA-945")
	if err != nil || !ok || string(read.Bundle) != "PACK newer" || read.Head != again.Head {
		t.Fatalf("after a second release the bundle is %q at %s (%v, %v), want the newer one", read.Bundle, read.Head, ok, err)
	}
	if len(read.Tips) != 0 || len(read.Bookmarks) != 0 {
		t.Fatalf("the newer bundle reads tips %v and bookmarks %v, want neither", read.Tips, read.Bookmarks)
	}

	if err := s.DeleteWorkspaceBundle(ctx, "INFRA-945"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.WorkspaceBundle(ctx, "INFRA-945"); err != nil || ok {
		t.Fatalf("after the launcher confirmed the restore the bundle reads %v, %v; want none", ok, err)
	}
	// A confirmation that arrives twice finishes rather than failing.
	if err := s.DeleteWorkspaceBundle(ctx, "INFRA-945"); err != nil {
		t.Fatalf("a repeated delete: %v", err)
	}
	if err := s.PutWorkspaceBundle(ctx, "", saved); err == nil {
		t.Fatal("a bundle saved under no issue was accepted")
	}
}
