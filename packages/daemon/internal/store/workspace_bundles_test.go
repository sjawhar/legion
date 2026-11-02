package store

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A released issue's bundle round-trips whole — its bytes, the commit its working copy sat on, the
// heads it carries, the bookmarks that pointed into it and the release that saved it — survives a
// second release replacing it, and goes when the restore is confirmed.
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
	if _, ok, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-0"); err != nil || ok {
		t.Fatalf("an issue never released reads %v, %v; want no bundle", ok, err)
	}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, saved); err != nil {
		t.Fatal(err)
	}
	read, ok, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-1")
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
	if read.Release != 11 {
		t.Fatalf("the bundle reads back as release %d, want the 11 that saved it", read.Release)
	}

	// A second release of the same issue replaces the row: the older work must not be restored
	// in place of the newer, and the new bundle has had no restore attempt, whatever became of
	// the one the row it replaced had.
	again := runtime.WorkspaceBundle{Bundle: []byte("PACK newer"), Head: "0a8f0aee63b13682fefe975ba0cc51da6f2ba9e9", At: at.Add(time.Hour)}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 12, again); err != nil {
		t.Fatal(err)
	}
	read, ok, err = s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-2")
	if err != nil || !ok || string(read.Bundle) != "PACK newer" || read.Head != again.Head || read.Release != 12 {
		t.Fatalf("after a second release the bundle is %q at %s of release %d (%v, %v), want the newer one", read.Bundle, read.Head, read.Release, ok, err)
	}
	if len(read.Tips) != 0 || len(read.Bookmarks) != 0 {
		t.Fatalf("the newer bundle reads tips %v and bookmarks %v, want neither", read.Tips, read.Bookmarks)
	}

	if err := s.DeleteWorkspaceBundle(ctx, "INFRA-945"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-3"); err != nil || ok {
		t.Fatalf("after the launcher confirmed the restore the bundle reads %v, %v; want none", ok, err)
	}
	// A confirmation that arrives twice finishes rather than failing.
	if err := s.DeleteWorkspaceBundle(ctx, "INFRA-945"); err != nil {
		t.Fatalf("a repeated delete: %v", err)
	}
	if err := s.PutWorkspaceBundle(ctx, "", 11, saved); err == nil {
		t.Fatal("a bundle saved under no issue was accepted")
	}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 0, saved); err == nil {
		t.Fatal("a bundle saved under no release was accepted")
	}
}

// A launcher holds its restore report until some connection carries it, so a confirmation can
// reach the daemon after the issue's next release has saved a bundle of its own. The delete names
// the release it confirms: the bundle of that release goes, and a bundle the issue's newer release
// saved stays, since the work in it is on no disk any more — the teardown took the one it came off.
func TestARestoreConfirmationDeletesOnlyTheBundleOfTheReleaseItNames(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	first := runtime.WorkspaceBundle{Bundle: []byte("PACK first"), Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", At: at}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, first); err != nil {
		t.Fatal(err)
	}
	second := runtime.WorkspaceBundle{Bundle: []byte("PACK second"), Head: "0a8f0aee63b13682fefe975ba0cc51da6f2ba9e9", At: at.Add(time.Hour)}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 12, second); err != nil {
		t.Fatal(err)
	}
	// The first release's restore is confirmed now, by a launcher that held its report through
	// the pod that saved the second.
	if err := s.DeleteRestoredWorkspaceBundle(ctx, "INFRA-945", 11); err != nil {
		t.Fatal(err)
	}
	kept, ok, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-2")
	if err != nil || !ok || string(kept.Bundle) != "PACK second" || kept.Release != 12 {
		t.Fatalf("a report of release 11 left %q of release %d (%v, %v), want the second release's bundle kept",
			kept.Bundle, kept.Release, ok, err)
	}
	// The second release's own confirmation drops it, and a report the daemon reads twice
	// finishes rather than failing.
	if err := s.DeleteRestoredWorkspaceBundle(ctx, "INFRA-945", 12); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-3"); err != nil || ok {
		t.Fatalf("after the report of release 12 the bundle reads %v, %v; want none", ok, err)
	}
	if err := s.DeleteRestoredWorkspaceBundle(ctx, "INFRA-945", 12); err != nil {
		t.Fatalf("a repeated confirmation: %v", err)
	}
}

// A bundle is sent to one pod's launcher once. The launcher's own record of the restore it ran
// is in the process that ran it, so a roles container that restarts comes back to the same disk
// with that record gone and the workspace hours of work past the save: a bundle sent there a
// second time would put the saved head and the saved bookmarks over that work. The daemon's
// record is the one that survives, and it is written in the read itself, before a slice leaves
// the daemon.
func TestABundleIsTakenForOneRestoreAttemptAndThenKept(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	saved := runtime.WorkspaceBundle{Bundle: []byte("PACK the released work"), Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", At: at}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, saved); err != nil {
		t.Fatal(err)
	}
	first, ok, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-uid-1")
	if err != nil || !ok || string(first.Bundle) != string(saved.Bundle) {
		t.Fatalf("the pod that replaces the released one was answered %q (%v, %v), want the saved work", first.Bundle, ok, err)
	}
	// The same pod again, which is its roles container restarting, and then the pod that
	// replaces it, and then a third after an eviction.
	for _, pod := range []string{"pod-uid-1", "pod-uid-2", "pod-uid-3"} {
		again, ok, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", pod)
		if err != nil || ok {
			t.Fatalf("%s was answered %q (%v, %v) after the bundle's one attempt, want nothing to send", pod, again.Bundle, ok, err)
		}
	}
	// The work is kept, and it holds the issue's pod until the issue's work is over.
	size, held, err := s.WorkspaceBundleSize(ctx, "INFRA-945")
	if err != nil || !held || size != len(saved.Bundle) {
		t.Fatalf("after the attempt the store holds %d bytes (held=%t, %v), want the %d saved kept", size, held, err, len(saved.Bundle))
	}
	// The issue's next release saves work of its own, which has an attempt of its own.
	newer := runtime.WorkspaceBundle{Bundle: []byte("PACK the work of the next release"), Head: "0a8f0aee63b13682fefe975ba0cc51da6f2ba9e9", At: at.Add(time.Hour)}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 12, newer); err != nil {
		t.Fatal(err)
	}
	read, ok, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-uid-4")
	if err != nil || !ok || string(read.Bundle) != string(newer.Bundle) || read.Release != 12 {
		t.Fatalf("the pod after the next release was answered %q of release %d (%v, %v), want that release's own work", read.Bundle, read.Release, ok, err)
	}
	// A send with no pod to name is refused: the record is what the rule rests on, and a row
	// marked by no pod names nothing an operator can read.
	if _, ok, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", ""); err == nil || ok {
		t.Fatalf("a take that named no pod was answered %v, %v; want it refused", ok, err)
	}
}

// The idle rule releases no pod of an issue whose stored bundle no pod has restored, and the row's
// own presence is that record: it is there from the save until a launcher's report deletes it.
func TestAStoredBundleReadsAsUnrestoredUntilItsConfirmation(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	unrestored := func() bool {
		t.Helper()
		var held bool
		if err := s.Tx(ctx, func(tx pgx.Tx) error {
			var err error
			held, err = workspaceBundleUnrestored(ctx, tx, "INFRA-945")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return held
	}
	if unrestored() {
		t.Fatal("an issue that was never released reads as holding work no pod has restored")
	}
	saved := runtime.WorkspaceBundle{Bundle: []byte("PACK"), Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", At: time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, saved); err != nil {
		t.Fatal(err)
	}
	if !unrestored() {
		t.Fatal("an issue whose release saved a bundle reads as holding nothing")
	}
	if err := s.DeleteRestoredWorkspaceBundle(ctx, "INFRA-945", 11); err != nil {
		t.Fatal(err)
	}
	if unrestored() {
		t.Fatal("an issue whose restore was confirmed still reads as holding work no pod has restored")
	}
}
