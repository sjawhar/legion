package main

import (
	"context"
	"slices"
	"testing"
)

func TestResumeSkipsMappedIDs(t *testing.T) {
	// A seeder with no API client and no database: any step that sent a request or ran a query
	// would panic, so each step below passing proves that a logged id replay_ids holds is skipped.
	ids := &idStore{m: map[[2]string]string{
		{idIssue, "PRJ-1"}: "PRJ-1", {idArtifact, "d1"}: "local-d1", {idArtifact, "f1"}: "local-f1",
		{idComment, "c1"}: "local-c1", {idResolve, "c1"}: "local-c1", {idAsk, "a1"}: "local-a1", {idAnswer, "a1"}: "local-a1",
	}}
	s := &replaySeeder{ids: ids}
	ctx := context.Background()
	is := &replayIssue{Key: "PRJ-1", Number: 1, Exists: true}
	if err := s.createIssue(ctx, seedIssuePlan{Key: "PRJ-1", Number: 1, Issue: is}); err != nil {
		t.Fatal(err)
	}
	d := &issueDoc{a: &replayArtifact{ID: "d1", Kind: "doc", Version: 2}, text: "synthetic\n", plan: historyPlan{Final: 2, Restore: 2}, is: is}
	if err := s.ensureDoc(ctx, "PRJ-1", d); err != nil || d.local != "local-d1" {
		t.Fatalf("ensureDoc: %v, local %q", err, d.local)
	}
	if err := s.ensureFile(ctx, "PRJ-1", &replayArtifact{ID: "f1", Kind: "file"}); err != nil {
		t.Fatal(err)
	}
	docs := map[string]*issueDoc{"d1": d}
	c := cmt("c1", 1, anchoredOn("d1", "synthetic"), resolvedC(true))
	var orphans []localComment
	if err := s.comment(ctx, "PRJ-1", docs, commentStep{C: c, Kind: commentAnchored}, &orphans); err != nil {
		t.Fatal(err)
	}
	if err := s.resolve(ctx, "PRJ-1", docs, c); err != nil {
		t.Fatal(err)
	}
	a := &replayAsk{ID: "a1", Kind: "question", State: "answered", Answer: &replayAskAnswer{User: "u@example.com"}}
	if local, err := s.ensureAsk(ctx, "PRJ-1", docs, a); err != nil || local != "local-a1" {
		t.Fatalf("ensureAsk: %v, local %q", err, local)
	}
	if err := s.answer(ctx, "PRJ-1", docs, a, "local-a1"); err != nil {
		t.Fatal(err)
	}
	if !ids.locals(idArtifact)["local-f1"] || ids.count(idArtifact) != 2 || ids.len() != 7 {
		t.Fatal("locals or counts wrong")
	}
}

func TestFileBytesDeterministic(t *testing.T) {
	a := fileBytes(1, "f1", 37)
	if len(a) != 37 || !slices.Equal(a, fileBytes(1, "f1", 37)) || slices.Equal(a, fileBytes(2, "f1", 37)) {
		t.Fatal("file bytes must be deterministic in seed and id, and sized")
	}
}

func TestIDOfAndDispatchCode(t *testing.T) {
	for body, want := range map[string]string{
		`{"id":"c1","body":"x"}`:                 "c1",
		`{"artifact":{"id":"d1"},"version":1}`:   "d1",
		`{"ask":{"id":"a1"},"artifact_id":"d1"}`: "a1",
		`{"error":"x"}`:                          "",
	} {
		if got := idOf([]byte(body)); got != want {
			t.Errorf("idOf(%s) = %q, want %q", body, got, want)
		}
	}
	if dispatchCode([]byte(`{"code":"ANCHOR_MISSING","error":"synthetic"}`)) != "ANCHOR_MISSING" || dispatchCode([]byte("not json")) != "" {
		t.Fatal("dispatchCode")
	}
}

func TestWithDatabase(t *testing.T) {
	got, err := withDatabase("postgres://postgres@127.0.0.1:15432/dispatch?sslmode=disable", "dispatch_replay_build")
	if err != nil || got != "postgres://postgres@127.0.0.1:15432/dispatch_replay_build?sslmode=disable" {
		t.Fatalf("withDatabase = %q, %v", got, err)
	}
}
