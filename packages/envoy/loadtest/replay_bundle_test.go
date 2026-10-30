package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKeepIssueDeterministicAndProportional(t *testing.T) {
	for _, f := range []float64{0.1, 0.5, 0.9} {
		kept := 0
		const n = 20000
		for i := 1; i <= n; i++ {
			key := fmt.Sprintf("ACME-%d", i)
			k := keepIssue(7, key, f)
			if k != keepIssue(7, key, f) {
				t.Fatalf("keepIssue(%s) not deterministic", key)
			}
			if k {
				kept++
			}
		}
		if share := float64(kept) / n; math.Abs(share-f) > 0.02 {
			t.Errorf("f=%g kept share %.4f, want within 0.02", f, share)
		}
	}
	for i := range 100 {
		key := fmt.Sprintf("ACME-%d", i)
		if !keepIssue(1, key, 1) || keepIssue(1, key, 0) {
			t.Fatalf("f=1 must keep and f=0 drop %s", key)
		}
	}
	differ := false
	for i := 0; i < 100 && !differ; i++ {
		key := fmt.Sprintf("ACME-%d", i)
		differ = keepIssue(1, key, 0.5) != keepIssue(2, key, 0.5)
	}
	if !differ {
		t.Error("the seed does not change the sample")
	}
}

// writeTestBundle writes a small synthetic bundle into dir.
func writeTestBundle(t *testing.T, dir string) {
	t.Helper()
	ev := mkEvent(t, 5, "PRJ-1", "message.created", mins(1), sess("s1"), map[string]any{"id": "m1", "body": "synthetic"})
	st := replayState{Project: "PRJ", At: tAt, Until: tUntil, Issues: []replayIssue{
		{Key: "PRJ-1", Number: 1, Exists: true, Title: "synthetic", Artifacts: []replayArtifact{{ID: "d1", Kind: "doc", Version: 1, TextPath: "docs/d1.md", TextVersion: 1}}},
	}}
	stateJSON, _ := json.Marshal(st)
	rosterJSON, _ := json.Marshal([]replayRosterEntry{{SessionID: "s1", Source: "window"}})
	files := map[string][]byte{
		bundleStateFile:      stateJSON,
		bundleRosterFile:     rosterJSON,
		bundleWindowFile:     append(ev.Raw, '\n'),
		"docs/d1.md":         []byte("# synthetic doc\n"),
		"created-docs/d2.md": []byte("new doc\n"),
		"raw/issues.json":    []byte("[]"),
	}
	for rel, data := range files {
		if err := writeFileAtomic(filepath.Join(dir, filepath.FromSlash(rel)), data); err != nil {
			t.Fatal(err)
		}
	}
	hashes, err := hashBundleFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	metaJSON, _ := json.Marshal(replayMeta{Project: "PRJ", At: tAt, Until: tUntil, Files: hashes})
	if err := os.WriteFile(filepath.Join(dir, bundleMetaFile), metaJSON, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadBundleRoundTripAndRefusal(t *testing.T) {
	dir := t.TempDir()
	writeTestBundle(t, dir)
	b, err := readBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Meta.Files["raw/issues.json"]; ok {
		t.Fatal("raw/ is hashed as a bundle file")
	}
	if b.State.Issues[0].Title != "synthetic" || len(b.Roster) != 1 || len(b.Window) != 1 {
		t.Fatalf("bundle read back wrong: %+v", b)
	}
	w := b.Window[0]
	if w.ID != 5 || w.Type != "message.created" || !w.CreatedAt.Equal(mins(1)) || w.Actor.Origin == nil || !strings.Contains(string(w.Payload), `"m1"`) {
		t.Fatalf("window event %+v", w)
	}
	if text, err := b.readFile(b.State.Issues[0].Artifacts[0].TextPath); err != nil || string(text) != "# synthetic doc\n" {
		t.Fatalf("doc text %q %v", text, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "d1.md"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readBundle(dir); err == nil || !strings.Contains(err.Error(), "docs/d1.md") {
		t.Fatalf("a changed file was not refused by name: %v", err)
	}
}

// TestReplayDecodePayloadNamesTheEvent: a payload its handler cannot read is refused by event id
// and type.
func TestReplayDecodePayloadNamesTheEvent(t *testing.T) {
	if _, err := decodePayload[struct {
		N int `json:"n"`
	}](replayEvent{ID: 7, Type: evCommentCreated, Payload: json.RawMessage(`{"n":"x"}`)}); err == nil ||
		!strings.Contains(err.Error(), "event 7 (comment.created): ") {
		t.Errorf("decodePayload error %v", err)
	}
}

// TestReplayPlannerHandlesEveryTypeItNames sends one event of every type planWrite handles through
// planReplay and requires each to be planned, none counted unhandled. It builds the events from the
// same constants the planner switches on, so it does not catch a constant spelt otherwise than
// Dispatch's log spells it; the tests whose fixtures carry the literal types do.
func TestReplayPlannerHandlesEveryTypeItNames(t *testing.T) {
	size := len(rpDocText) + 16
	payloads := []struct {
		typ, key string
		payload  any
	}{
		{evCommentCreated, "ACME-1", map[string]any{"id": "C9", "body": "b"}},
		{evCommentResolved, "ACME-1", map[string]any{"id": "C1", "resolved": true}},
		{evArtifactVersion, "ACME-1", map[string]any{"artifact_id": "D1", "name": "spec",
			"version": map[string]any{"number": 2, "named": false, "size": size}}},
		{evArtifactCreated, "ACME-1", map[string]any{"artifact": map[string]any{"id": "D9", "name": "notes", "kind": "doc",
			"versions": []any{map[string]any{"number": 1, "size": 512}}}}},
		{evIssueUpdated, "ACME-1", map[string]any{"status": "in_progress"}},
		{evIssueClosed, "ACME-1", map[string]any{"status": "done"}},
		{evIssueClaimed, "ACME-2", map[string]any{"claim": map[string]any{"actor": rpSession}}},
		{evMessageCreated, "ACME-2", map[string]any{"body": "m"}},
		{evAskOpened, "ACME-2", map[string]any{"id": "A9", "state": "open", "kind": "decision", "question": "q", "author": rpSession}},
		{evAskAnswered, "ACME-2", map[string]any{"id": "A9", "state": "answered", "kind": "decision", "author": rpSession}},
		{evAskResolved, "ACME-2", map[string]any{"id": "A9", "state": "resolved", "kind": "decision", "author": rpSession,
			"resolution": map[string]any{"kind": "withdrawn", "reason": "r", "actor": rpSession}}},
		{evIssueCreated, "ACME-3", map[string]any{"title": "three", "status": "open"}},
	}
	var lines []string
	for i, p := range payloads {
		lines = append(lines, rpEvent(int64(i+1), p.typ, p.key, rpSession, time.Duration(i+1)*time.Second, p.payload))
	}
	plan := rpPlan(t, rpOptions(), lines, nil)
	for reason, n := range plan.Counts.NotSent {
		if strings.HasPrefix(reason, "unhandled:") {
			t.Errorf("%d events the planner does not handle: %s", n, reason)
		}
	}
	if plan.Counts.TransactionsPlanned != len(payloads) {
		t.Fatalf("%d transactions planned, want one per type (%d); not sent: %v",
			plan.Counts.TransactionsPlanned, len(payloads), plan.Counts.NotSent)
	}
}
