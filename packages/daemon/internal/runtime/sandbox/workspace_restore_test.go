package sandbox

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// savedWorkspaceBundle is a release's saved work as the store holds it: more than one
// shimwire.LauncherBundlePayloadBytes of git bundle, so the hop to the launcher is a sequence of
// slices and not one frame, with the head, the tips and the bookmark a restore needs beside it.
func savedWorkspaceBundle() runtime.WorkspaceBundle {
	return runtime.WorkspaceBundle{
		Bundle:    bytes.Repeat([]byte("PACKjj\x00\x02"), (600<<10)/8),
		Head:      "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		Tips:      []string{"da4f9f492e9f8349389c243743e0a000cd1c676f"},
		Bookmarks: map[string]string{"legion/" + testTree: "dffe88ae2f793e13436cef3921adc988433fbd4e"},
		At:        time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC),
	}
}

// frameTypes names what a launcher was sent, for a failure an operator reads.
func frameTypes(frames []shimwire.Frame) []string {
	types := make([]string, 0, len(frames))
	for _, frame := range frames {
		types = append(types, frame.FrameType())
	}
	return types
}

// A released issue's pod comes back with the work the release took off its disk: the daemon reads
// the bundle its store kept, sends it to the new pod's launcher in launcher_bundle slices, and
// drops its own copy on the Held that says the launcher holds the whole of it — all of it before
// the launcher is told to start any role, since the launcher restores the bundle into the
// workspace ahead of the role's command (internal/launcher).
func TestASavedWorkspaceBundleReachesThePodsLauncherBeforeItStartsAnyRole(t *testing.T) {
	st := migratedStore(t)
	saved := savedWorkspaceBundle()
	if err := st.PutWorkspaceBundle(context.Background(), testTree, saved); err != nil {
		t.Fatalf("save the released workspace's bundle: %v", err)
	}
	g := newRig(t, nil, withOptions(func(o *Options) {
		o.SavedWorkspaceBundle, o.WorkspaceBundleRestored = st.WorkspaceBundle, st.DeleteWorkspaceBundle
	}))
	spec := workerSpec(t)
	g.spawn(spec)

	frames := g.sent(spec.Claim)
	start := slices.IndexFunc(frames, func(f shimwire.Frame) bool { _, ok := f.(shimwire.LauncherStart); return ok })
	if start < 1 {
		t.Fatalf("the launcher was sent %v, want the bundle's slices and then the role's start", frameTypes(frames))
	}
	for _, frame := range frames[start+1:] {
		if _, ok := frame.(shimwire.LauncherBundle); ok {
			t.Fatalf("the launcher was sent %v: a slice crossed after the role's start", frameTypes(frames))
		}
	}
	var body []byte
	var sequence string
	for i, frame := range frames[:start] {
		slice, ok := frame.(shimwire.LauncherBundle)
		if !ok {
			t.Fatalf("the launcher was sent %v, want the bundle's slices before the role's start", frameTypes(frames))
		}
		if i == 0 {
			sequence = slice.ID
		}
		if slice.ID != sequence || slice.Index != i || slice.Count != start {
			t.Fatalf("slice %d of %d under %q arrived at position %d of the %d sent before the start",
				slice.Index, slice.Count, slice.ID, i, start)
		}
		body = append(body, slice.Payload...)
	}
	read, err := shimwire.ParseWorkspaceBundle(body)
	if err != nil {
		t.Fatalf("the %d slices rebuilt no workspace bundle: %v", start, err)
	}
	if !bytes.Equal(read.Bundle, saved.Bundle) {
		t.Fatalf("the bundle crossed as %d bytes, want the %d saved", len(read.Bundle), len(saved.Bundle))
	}
	if read.Head != saved.Head || !slices.Equal(read.Tips, saved.Tips) || !maps.Equal(read.Bookmarks, saved.Bookmarks) {
		t.Fatalf("the bundle crossed as head %s, tips %v and bookmarks %v, want %s, %v and %v",
			read.Head, read.Tips, read.Bookmarks, saved.Head, saved.Tips, saved.Bookmarks)
	}
	if !read.At.Equal(saved.At) {
		t.Fatalf("the bundle crossed as taken at %s, want %s", read.At, saved.At)
	}
	// The launcher held the whole of it, so the daemon's copy has done its work.
	if _, held, err := st.WorkspaceBundle(g.ctx, testTree); err != nil || held {
		t.Fatalf("after the launcher held the bundle the store holds it = %t (%v), want it deleted", held, err)
	}
}

// A launcher that refuses a slice costs the restore and nothing else: the role starts on the
// workspace the pod's own fetch provisioned, the launcher writes the agent the notice of what was
// lost, and the saved work stays in the daemon's database for the issue's next pod. A pod that
// never starts would be the worse mistake.
func TestARefusedBundleSliceStillStartsTheRoleAndKeepsTheSavedWork(t *testing.T) {
	st := migratedStore(t)
	saved := savedWorkspaceBundle()
	if err := st.PutWorkspaceBundle(context.Background(), testTree, saved); err != nil {
		t.Fatalf("save the released workspace's bundle: %v", err)
	}
	const refusal = "launcher_bundle sequence rebuilt to another digest than its slices declared"
	g := newRig(t, nil, withRefusedBundle(refusal), withOptions(func(o *Options) {
		o.SavedWorkspaceBundle, o.WorkspaceBundleRestored = st.WorkspaceBundle, st.DeleteWorkspaceBundle
	}))
	spec := workerSpec(t)
	loc := g.spawn(spec)
	if loc.Sandbox.Generation != spec.Generation {
		t.Fatalf("the refused bundle left the launch at %+v, want generation %d", loc, spec.Generation)
	}

	frames := g.sent(spec.Claim)
	bundleSlices, starts := 0, 0
	for _, frame := range frames {
		switch frame.(type) {
		case shimwire.LauncherBundle:
			bundleSlices++
		case shimwire.LauncherStart:
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("the launcher was sent %v, want the role's start all the same", frameTypes(frames))
	}
	// A refusal ends the sequence: the rest of a bundle the launcher will not take is bytes no
	// pod reads.
	if bundleSlices != 1 {
		t.Fatalf("the launcher was sent %v, want one slice and no more after its refusal", frameTypes(frames))
	}
	kept, held, err := st.WorkspaceBundle(g.ctx, testTree)
	if err != nil {
		t.Fatal(err)
	}
	if !held || !bytes.Equal(kept.Bundle, saved.Bundle) {
		t.Fatalf("after the refusal the store holds %d bytes (held=%t), want the %d saved kept for the next pod",
			len(kept.Bundle), held, len(saved.Bundle))
	}
}
