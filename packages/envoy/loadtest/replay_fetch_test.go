package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

var (
	tAt    = time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	tUntil = tAt.Add(30 * time.Minute)
)

func mins(m float64) time.Time { return tAt.Add(time.Duration(m * float64(time.Minute))) }

func sess(id string) map[string]any {
	return map[string]any{"kind": "session", "id": id, "origin": map[string]any{"session_title": "title-" + id, "machine": "m1", "cwd": "/w"}}
}

// mkEvent builds a served event as Dispatch would send it.
func mkEvent(t *testing.T, id int64, key, typ string, at time.Time, actor map[string]any, payload any) replayEvent {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"id": id, "issue_key": key, "artifact_id": nil, "project": "PRJ", "seq": id, "type": typ,
		"actor": actor, "notify": false, "created_at": at.Format(time.RFC3339Nano), "payload": payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := parseReplayEvent(b)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func issueSnap(key, title, status string, extra map[string]any) map[string]any {
	m := map[string]any{"key": key, "title": title, "status": status, "labels": []string{"a"}, "claim": nil,
		"created_at": mins(-100).Format(time.RFC3339Nano), "created_by": sess("s0"), "primary_artifact_id": "art-1"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func comment(id string, extra map[string]any) map[string]any {
	m := map[string]any{"id": id, "body": "body " + id, "author": map[string]any{"kind": "session", "id": "s1"},
		"anchor": nil, "reply_to": nil, "resolved": false, "created_at": mins(-50).Format(time.RFC3339Nano)}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestBuildStateMergesSnapshotsAndClaims(t *testing.T) {
	claim := map[string]any{"actor": sess("s2"), "at": mins(-30).Format(time.RFC3339Nano)}
	lg := issueLog{Key: "PRJ-1", Number: 1, Events: []replayEvent{
		mkEvent(t, 1, "PRJ-1", "issue.created", mins(-100), sess("s0"), issueSnap("PRJ-1", "first", "open", nil)),
		mkEvent(t, 2, "PRJ-1", "issue.updated", mins(-90), sess("s0"), issueSnap("PRJ-1", "second", "open", map[string]any{"priority": 2, "route": "session:s9"})),
		mkEvent(t, 3, "PRJ-1", "issue.claimed", mins(-30), sess("s2"), map[string]any{"key": "PRJ-1", "claim": claim, "status": "in_progress"}),
	}}
	rec, err := buildState("PRJ", tAt, tUntil, []issueLog{lg}, false)
	if err != nil {
		t.Fatal(err)
	}
	is := rec.State.Issues[0]
	if !is.Exists || is.Title != "second" || is.Status != "in_progress" || is.Priority == nil || *is.Priority != 2 {
		t.Fatalf("merged issue = %+v", is)
	}
	if is.Claim == nil || is.Claim.Actor.ID != "s2" || !is.Claim.At.Equal(mins(-30)) {
		t.Fatalf("claim = %+v", is.Claim)
	}
	if is.Route == nil || *is.Route != "session:s9" || is.CreatedBy == nil || is.CreatedBy.ID != "s0" || !is.CreatedAt.Equal(mins(-100)) {
		t.Fatalf("route/created = %+v", is)
	}

	lg.Events = append(lg.Events, mkEvent(t, 4, "PRJ-1", "issue.released", mins(-10), sess("s2"),
		map[string]any{"key": "PRJ-1", "claim": nil, "previous_claim": claim, "status": "open"}))
	rec, err = buildState("PRJ", tAt, tUntil, []issueLog{lg}, false)
	if err != nil {
		t.Fatal(err)
	}
	if is := rec.State.Issues[0]; is.Claim != nil || is.Status != "open" {
		t.Fatalf("after release: claim %+v status %s", is.Claim, is.Status)
	}
}

func TestVersionAtPicksLargestBeforeAt(t *testing.T) {
	vs := []listedVersion{{Number: 1, CreatedAt: mins(-60)}, {Number: 3, CreatedAt: mins(-1)}, {Number: 2, CreatedAt: mins(-30)}, {Number: 4, CreatedAt: tAt}}
	v, ok := versionAt(vs, tAt)
	if !ok || v.Number != 3 {
		t.Fatalf("versionAt = %d %v, want 3", v.Number, ok)
	}
	if _, ok := versionAt(vs[3:], tAt); ok {
		t.Fatal("a version created at at counted as before it")
	}
}

func TestAttachArtifactsAndUnlisted(t *testing.T) {
	st := replayState{Issues: []replayIssue{{Key: "PRJ-1", Number: 1, Exists: true}}}
	size := 10
	listings := map[string][]listedArtifact{"PRJ-1": {
		{ID: "d1", Kind: "doc", CreatedAt: mins(-60), Versions: []listedVersion{{Number: 1, CreatedAt: mins(-60), Size: &size}, {Number: 2, CreatedAt: mins(5)}}},
		{ID: "d2", Kind: "doc", CreatedAt: mins(5), Versions: []listedVersion{{Number: 1, CreatedAt: mins(5)}}},
		{ID: "f1", Kind: "file", CreatedAt: mins(-60), Versions: []listedVersion{{Number: 1, CreatedAt: mins(-60)}}},
	}}
	unlisted := attachArtifacts(&st, listings, map[string][]string{"PRJ-1": {"d1", "gone"}}, tAt)
	arts := st.Issues[0].Artifacts
	if unlisted != 1 || len(arts) != 2 || arts[0].ID != "d1" || arts[0].Version != 1 || *arts[0].Size != 10 || arts[1].ID != "f1" {
		t.Fatalf("unlisted %d artifacts %+v", unlisted, arts)
	}
}

func TestCommentResolveReopenResolve(t *testing.T) {
	evs := []replayEvent{
		mkEvent(t, 1, "PRJ-1", "issue.created", mins(-100), sess("s0"), issueSnap("PRJ-1", "t", "open", nil)),
		mkEvent(t, 2, "PRJ-1", "comment.created", mins(-50), sess("s1"), comment("c1", nil)),
		mkEvent(t, 3, "PRJ-1", "comment.resolved", mins(-40), sess("s1"), comment("c1", map[string]any{"resolved": true, "resolved_at": "r1", "resolved_by": sess("s1")})),
		mkEvent(t, 4, "PRJ-1", "comment.reopened", mins(-30), sess("s1"), comment("c1", nil)),
	}
	rec, err := buildState("PRJ", tAt, tUntil, []issueLog{{Key: "PRJ-1", Number: 1, Events: evs}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if c := rec.State.Issues[0].Comments[0]; !c.Open || c.ResolvedAt != nil || c.ResolvedBy != nil {
		t.Fatalf("after reopen: %+v", c)
	}
	evs = append(evs, mkEvent(t, 5, "PRJ-1", "comment.resolved", mins(-20), map[string]any{"kind": "user", "id": "u1"},
		comment("c1", map[string]any{"resolved": true, "resolved_at": "r2", "resolved_by": map[string]any{"kind": "user", "id": "u1"}})))
	// A reopen in the window does not count toward the state at at.
	evs = append(evs, mkEvent(t, 6, "PRJ-1", "comment.reopened", mins(1), sess("s1"), comment("c1", nil)))
	rec, err = buildState("PRJ", tAt, tUntil, []issueLog{{Key: "PRJ-1", Number: 1, Events: evs}}, false)
	if err != nil {
		t.Fatal(err)
	}
	c := rec.State.Issues[0].Comments[0]
	if c.Open || c.ResolvedAt == nil || *c.ResolvedAt != "r2" || c.ResolvedBy.ID != "u1" || !c.WindowRefs {
		t.Fatalf("after second resolve: %+v", c)
	}
	if c.Actor.Origin == nil || c.Actor.Origin.SessionTitle != "title-s1" {
		t.Fatalf("comment actor lost its origin: %+v", c.Actor)
	}
}

func TestThreadsAndWindowRefs(t *testing.T) {
	anchor := map[string]any{"artifact_id": "d1", "quote": "q", "mark_id": "m", "version": 1, "block_id": "b1", "orphaned": false}
	refreshed := map[string]any{"artifact_id": "d1", "quote": "q2", "mark_id": "m", "version": 2, "block_id": "b2", "orphaned": true}
	evs := []replayEvent{
		mkEvent(t, 1, "PRJ-1", "issue.created", mins(-100), sess("s0"), issueSnap("PRJ-1", "t", "open", nil)),
		mkEvent(t, 2, "PRJ-1", "comment.created", mins(-50), sess("s1"), comment("root", map[string]any{"anchor": anchor})),
		mkEvent(t, 3, "PRJ-1", "comment.created", mins(-49), sess("s1"), comment("reply", map[string]any{"reply_to": "root"})),
		mkEvent(t, 4, "PRJ-1", "comment.created", mins(-48), sess("s1"), comment("deep", map[string]any{"reply_to": "reply"})),
		mkEvent(t, 5, "PRJ-1", "comment.anchor_refreshed", mins(-47), sess("s1"), comment("root", map[string]any{"anchor": refreshed})),
		mkEvent(t, 6, "PRJ-1", "comment.created", mins(-46), sess("s1"), comment("other", nil)),
		mkEvent(t, 7, "PRJ-1", "comment.created", mins(-45), sess("s1"), comment("third", nil)),
		// Window: a reply into root's thread through "deep", and an edit of "third".
		mkEvent(t, 8, "PRJ-1", "comment.created", mins(2), sess("s3"), comment("late", map[string]any{"reply_to": "deep"})),
		mkEvent(t, 9, "PRJ-1", "comment.edited", mins(3), sess("s1"), comment("third", nil)),
	}
	rec, err := buildState("PRJ", tAt, tUntil, []issueLog{{Key: "PRJ-1", Number: 1, Events: evs}}, false)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]replayComment{}
	var order []string
	for _, c := range rec.State.Issues[0].Comments {
		got[c.ID] = c
		order = append(order, c.ID)
	}
	if len(order) != 5 || order[0] != "root" || order[4] != "third" {
		t.Fatalf("comment order %v", order)
	}
	if got["root"].ThreadRootID != nil || *got["reply"].ThreadRootID != "root" || *got["deep"].ThreadRootID != "root" {
		t.Fatal("thread roots not resolved through the reply_to chain")
	}
	a := got["root"].Anchor
	if a == nil || a.Quote != "q2" || !a.Orphaned || *a.BlockID != "b2" {
		t.Fatalf("anchor refresh not applied: %+v", a)
	}
	for id, want := range map[string]bool{"root": true, "reply": false, "deep": true, "other": false, "third": true} {
		if got[id].WindowRefs != want {
			t.Errorf("%s WindowRefs = %v, want %v", id, got[id].WindowRefs, want)
		}
	}
	c := stateCounts(rec.State, rec.Window)
	if c.CommentsOpen != 5 || c.AnchoredOpen != 1 {
		t.Fatalf("counts %+v", c)
	}
}

func TestPlaceholdersAndSubset(t *testing.T) {
	created := func(key string, n int, at time.Time) issueLog {
		return issueLog{Key: key, Number: n, Events: []replayEvent{mkEvent(t, int64(n), key, "issue.created", at, sess("s0"), issueSnap(key, "t", "open", nil))}}
	}
	logs := []issueLog{created("PRJ-1", 1, mins(-10)), created("PRJ-3", 3, mins(-5)), created("PRJ-4", 4, mins(5))}
	rec, err := buildState("PRJ", tAt, tUntil, logs, false)
	if err != nil {
		t.Fatal(err)
	}
	is := rec.State.Issues
	if len(is) != 3 || !is[0].Exists || is[1].Exists || is[1].Key != "PRJ-2" || !is[2].Exists {
		t.Fatalf("issues %+v", is)
	}
	if c := stateCounts(rec.State, rec.Window); c.Issues != 2 || c.Placeholders != 1 || c.WindowEvents != 1 {
		t.Fatalf("counts %+v", c)
	}
	rec, err = buildState("PRJ", tAt, tUntil, []issueLog{logs[1], logs[2]}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.State.Issues) != 1 || rec.State.Issues[0].Key != "PRJ-3" {
		t.Fatalf("subset issues %+v", rec.State.Issues)
	}
}

func TestWindowBoundaries(t *testing.T) {
	logs := []issueLog{
		{Key: "PRJ-1", Number: 1, Events: []replayEvent{
			mkEvent(t, 10, "PRJ-1", "issue.created", mins(-10), sess("s0"), issueSnap("PRJ-1", "t", "open", nil)),
			mkEvent(t, 11, "PRJ-1", "message.created", tAt, sess("s0"), map[string]any{"id": "m1"}),
			mkEvent(t, 30, "PRJ-1", "message.created", mins(5), sess("s0"), map[string]any{"id": "m3"}),
			mkEvent(t, 12, "PRJ-1", "message.created", tUntil, sess("s0"), map[string]any{"id": "m4"}),
		}},
		{Key: "PRJ-2", Number: 2, Events: []replayEvent{
			mkEvent(t, 20, "PRJ-2", "issue.created", mins(-10), sess("s0"), issueSnap("PRJ-2", "t", "open", nil)),
			mkEvent(t, 25, "PRJ-2", "message.created", mins(5), sess("s0"), map[string]any{"id": "m2"}),
		}},
	}
	rec, err := buildState("PRJ", tAt, tUntil, logs, false)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, ev := range rec.Window {
		ids = append(ids, ev.ID)
	}
	if len(ids) != 3 || ids[0] != 11 || ids[1] != 25 || ids[2] != 30 {
		t.Fatalf("window ids %v, want [11 25 30]", ids)
	}
	if string(rec.Window[0].Raw) == "" || rec.Window[0].Raw[0] != '{' {
		t.Fatal("window event lost its raw bytes")
	}
}

func TestOpenAsks(t *testing.T) {
	ask := func(id, state string, extra map[string]any) map[string]any {
		m := map[string]any{"id": id, "kind": "question", "state": state, "question": "q?", "options": []map[string]any{{"label": "yes"}},
			"multiple": false, "urgency": "normal", "author": sess("s1"), "created_at": mins(-20).Format(time.RFC3339Nano), "anchor": nil, "block_id": nil}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	evs := []replayEvent{
		mkEvent(t, 1, "PRJ-1", "issue.created", mins(-100), sess("s0"), issueSnap("PRJ-1", "t", "open", nil)),
		mkEvent(t, 2, "PRJ-1", "ask.opened", mins(-20), sess("s1"), ask("a1", "open", nil)),
		mkEvent(t, 3, "PRJ-1", "ask.opened", mins(-19), sess("s1"), ask("a2", "open", map[string]any{
			"kind": "approval", "approval": map[string]any{"artifact_id": "d1", "name": "n", "version": 3, "requested_version": 2},
			"block_id": "b1", "block_artifact": map[string]any{"id": "d1", "slug": "s", "primary": true}})),
		mkEvent(t, 4, "PRJ-1", "ask.opened", mins(-18), sess("s1"), ask("a3", "open", nil)),
		mkEvent(t, 5, "PRJ-1", "ask.answered", mins(-17), map[string]any{"kind": "user", "id": "u1"}, ask("a1", "answered", map[string]any{
			"answer": map[string]any{"user": "u1", "selected": []string{"yes"}, "text": nil, "at": mins(-17).Format(time.RFC3339Nano)}})),
		mkEvent(t, 6, "PRJ-1", "ask.follower_added", mins(-16), sess("s1"), map[string]any{"ask_id": "a3", "session_id": "s4"}),
		mkEvent(t, 7, "PRJ-1", "ask.resolved", mins(-15), sess("s1"), ask("a3", "resolved", nil)),
		mkEvent(t, 8, "PRJ-1", "ask.resolved", mins(1), sess("s1"), ask("a1", "resolved", nil)),
		// A typed ask block resolved before at is kept with its resolution; one resolved in the
		// window stands as it was at at.
		mkEvent(t, 9, "PRJ-1", "ask.opened", mins(-14), sess("s1"), ask("a4", "open", map[string]any{"block_id": "b4"})),
		mkEvent(t, 10, "PRJ-1", "ask.resolved", mins(-13), sess("s2"), ask("a4", "resolved", map[string]any{
			"block_id": "b4", "block_artifact": map[string]any{"id": "d1", "slug": "s", "primary": true},
			"resolution": map[string]any{"kind": "retracted", "reason": "why", "actor": sess("s2"), "at": mins(-13).Format(time.RFC3339Nano)}})),
		mkEvent(t, 11, "PRJ-1", "ask.opened", mins(-12), sess("s1"), ask("a5", "open", map[string]any{"block_id": "b5"})),
		mkEvent(t, 12, "PRJ-1", "ask.resolved", mins(2), sess("s2"), ask("a5", "resolved", map[string]any{
			"block_id": "b5", "resolution": map[string]any{"kind": "resolved", "reason": "why", "actor": sess("s2"), "at": mins(2).Format(time.RFC3339Nano)}})),
	}
	rec, err := buildState("PRJ", tAt, tUntil, []issueLog{{Key: "PRJ-1", Number: 1, Events: evs}}, false)
	if err != nil {
		t.Fatal(err)
	}
	asks := rec.State.Issues[0].Asks
	if len(asks) != 4 || asks[0].ID != "a1" || asks[0].State != "answered" || asks[0].Answer == nil || asks[0].Answer.User != "u1" {
		t.Fatalf("asks %+v", asks)
	}
	a2 := asks[1]
	if a2.Kind != "approval" || a2.Approval == nil || a2.Approval.Version != 3 || a2.Approval.RequestedVersion != 2 ||
		a2.BlockArtifactID == nil || *a2.BlockArtifactID != "d1" || *a2.BlockID != "b1" {
		t.Fatalf("approval ask %+v", a2)
	}
	a4 := asks[2]
	if a4.ID != "a4" || a4.State != "resolved" || a4.Resolution == nil || a4.Resolution.Kind != "retracted" || a4.Resolution.Reason != "why" ||
		a4.Resolution.By.ID != "s2" || !a4.Resolution.At.Equal(mins(-13)) || a4.BlockArtifactID == nil || *a4.BlockArtifactID != "d1" {
		t.Fatalf("resolved typed ask block %+v", a4)
	}
	if a5 := asks[3]; a5.ID != "a5" || a5.State != "open" || a5.Resolution != nil {
		t.Fatalf("typed ask block resolved in the window %+v", a5)
	}
}

func TestRosterOrderAndDedupe(t *testing.T) {
	route := func(s string) *string { return &s }
	st := replayState{Issues: []replayIssue{
		{Key: "PRJ-1", Exists: true, Route: route("session:r1"), Claim: &replayClaim{Actor: replayActor{Kind: "session", ID: "c2", Origin: &replayOrigin{SessionTitle: "own"}}}},
		{Key: "PRJ-2", Exists: true, Route: route("role:x"), Claim: &replayClaim{Actor: replayActor{Kind: "session", ID: "c1"}}},
		{Key: "PRJ-3", Exists: true, Route: route("session:c1"), Claim: &replayClaim{Actor: replayActor{Kind: "user", ID: "u"}}},
	}}
	window := []replayEvent{
		{Actor: replayActor{Kind: "session", ID: "w1"}},
		{Actor: replayActor{Kind: "user", ID: "u2"}},
		{Actor: replayActor{Kind: "session", ID: "r1"}},
		{Actor: replayActor{Kind: "session", ID: "w1"}},
	}
	got := buildRoster(st, window, map[string]replayOrigin{"c1": {Machine: "mx"}})
	want := []replayRosterEntry{
		{SessionID: "c2", Source: "claim", SessionTitle: "own"},
		{SessionID: "c1", Source: "claim", Machine: "mx"},
		{SessionID: "r1", Source: "route"},
		{SessionID: "w1", Source: "window"},
	}
	if len(got) != len(want) {
		t.Fatalf("roster %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("roster[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestStateCountsWrittenDocs(t *testing.T) {
	anchored := func(doc string) *replayAnchor { return &replayAnchor{ArtifactID: doc} }
	reply := "x"
	st := replayState{Issues: []replayIssue{{Key: "PRJ-1", Exists: true,
		Artifacts: []replayArtifact{{ID: "d1", Kind: "doc"}, {ID: "d2", Kind: "doc"}, {ID: "d3", Kind: "doc"}},
		Comments: []replayComment{
			{ID: "1", Open: true, Anchor: anchored("d1")}, {ID: "2", Open: true, Anchor: anchored("d1")},
			{ID: "3", Open: true, Anchor: anchored("d1"), ReplyTo: &reply}, {ID: "4", Anchor: anchored("d1")},
			{ID: "5", Open: true, Anchor: anchored("d2")}, {ID: "6", Open: true, Anchor: anchored("d3")},
		}}}}
	window := []replayEvent{
		{Type: "artifact.version", Payload: json.RawMessage(`{"artifact_id":"d1"}`)},
		{Type: "comment.created", Payload: json.RawMessage(`{"anchor":{"artifact_id":"d2"}}`)},
		{Type: "comment.created", Payload: json.RawMessage(`{"anchor":null}`)},
	}
	c := stateCounts(st, window)
	if c.WrittenDocs != 2 || c.WrittenDocsOpenRootsSum != 3 || c.WrittenDocsOpenRootsMedian != 1.5 {
		t.Fatalf("written docs counts %+v", c)
	}
	if c.WindowByType["comment.created"] != 2 || c.CommentsResolved != 1 || c.AnchoredOpen != 5 {
		t.Fatalf("counts %+v", c)
	}
}

// TestFetchCreatedSpecs fetches version 1 of each window-created issue's spec into created-docs/:
// by the event's primary_artifact_id, else by the issue's listing, and records the readable ones.
func TestFetchCreatedSpecs(t *testing.T) {
	raw := func(evs ...replayEvent) []byte {
		var out []json.RawMessage
		for _, ev := range evs {
			out = append(out, ev.Raw)
		}
		b, _ := json.Marshal(out)
		return b
	}
	version := func(text string) []byte { b, _ := json.Marshal(map[string]any{"markdown": text}); return b }
	listing := func(id string) []byte {
		b, _ := json.Marshal([]map[string]any{{"id": id, "name": "spec", "kind": "doc", "primary": true,
			"created_at": mins(-100), "versions": []map[string]any{{"number": 1, "created_at": mins(-100)}}}})
		return b
	}
	routes := map[string][]byte{
		"/api/v1/issues":              []byte(`[{"key":"PRJ-1"},{"key":"PRJ-2"},{"key":"PRJ-3"}]`),
		"/api/v1/issues/PRJ-1/events": raw(mkEvent(t, 1, "PRJ-1", "issue.created", mins(-100), sess("s0"), issueSnap("PRJ-1", "t", "open", nil))),
		"/api/v1/issues/PRJ-2/events": raw(mkEvent(t, 2, "PRJ-2", "issue.created", mins(5), sess("s0"), issueSnap("PRJ-2", "t", "open", map[string]any{"primary_artifact_id": "spec-2"})),
			mkEvent(t, 3, "PRJ-2", "artifact.created", mins(6), sess("s0"), map[string]any{"artifact": map[string]any{"id": "doc-9", "kind": "doc"}}),
			mkEvent(t, 4, "PRJ-2", "artifact.created", mins(7), sess("s0"), map[string]any{"artifact": map[string]any{"id": "spec-2", "kind": "doc"}})),
		"/api/v1/issues/PRJ-3/events":         raw(mkEvent(t, 5, "PRJ-3", "issue.created", mins(8), sess("s0"), issueSnap("PRJ-3", "t", "open", map[string]any{"primary_artifact_id": nil}))),
		"/api/v1/issues/PRJ-1/artifacts":      listing("art-1"),
		"/api/v1/issues/PRJ-3/artifacts":      listing("spec-3"),
		"/api/v1/artifacts/art-1/versions/1":  version("state spec\n"),
		"/api/v1/artifacts/spec-2/versions/1": version("created spec two\n"),
		"/api/v1/artifacts/doc-9/versions/1":  version("created doc nine\n"),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	out := t.TempDir()
	f := &replayFetcher{api: newAPI(srv.URL, "tok", 2), raw: filepath.Join(out, bundleRawDir), conc: 2, lg: log.New(io.Discard, "", 0)}
	counts, err := f.run(context.Background(), "PRJ", tAt, tUntil, out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if counts.CreatedDocs != 2 || counts.CreatedDocsUnreadable != 0 || counts.CreatedSpecs != 1 || counts.CreatedSpecsUnreadable != 1 {
		t.Fatalf("counts %+v", counts)
	}
	b, err := readBundle(out)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"PRJ-2": "spec-2"}; !reflect.DeepEqual(b.Meta.CreatedSpecs, want) {
		t.Fatalf("created specs %v, want %v", b.Meta.CreatedSpecs, want)
	}
	for rel, want := range map[string]string{"created-docs/spec-2.md": "created spec two\n", "created-docs/doc-9.md": "created doc nine\n"} {
		got, err := b.readFile(rel)
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v", rel, got, err)
		}
		if _, ok := b.Meta.Files[rel]; !ok {
			t.Fatalf("bundle.meta.json does not list %s", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(out, bundleCreatedDir, "spec-3.md")); !os.IsNotExist(err) {
		t.Fatalf("unreadable spec written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, bundleRawDir, "versions", "spec-3", "1.unreadable")); err != nil {
		t.Fatalf("unreadable spec not cached as such: %v", err)
	}
}
