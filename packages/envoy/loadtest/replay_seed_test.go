package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sync/errgroup"
)

// TestWriteCanceledByAnotherWrite: when one issue's write fails, errgroup cancels the others'
// context with that error as its cause, and net/http returns the cause for a request the context
// ended. A write ended so must not read as its own refusal: the full seed logged five "quote
// refused (413 REQUEST_TOO_LARGE)" for a comment whose request was only canceled by another
// issue's history edit, Dispatch having created the comment.
func TestWriteCanceledByAnotherWrite(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"c1"}`)
	}))
	defer srv.Close()
	s := &replaySeeder{api: newAPI(srv.URL, "t", 1), lg: log.New(io.Discard, "", 0), counts: map[string]int{}, marks: map[string]int{}}
	g, gctx := errgroup.WithContext(context.Background())
	g.Go(func() error {
		return &seedError{step: "history-edit", logged: "d1@7", status: http.StatusRequestEntityTooLarge, code: "REQUEST_TOO_LARGE"}
	})
	_ = g.Wait()
	_, err := s.write(gctx, seedWrite{kind: "comment-anchored", issue: "PRJ-1", step: "comment-anchored", logged: "c1",
		method: "POST", path: "/api/v1/issues/PRJ-1/comments", body: jsonBody(map[string]any{"body": "x"})})
	var se *seedError
	if err == nil || errors.As(err, &se) || !errors.Is(err, context.Canceled) {
		t.Fatalf("a write the group canceled returns %v, want context.Canceled and no refusal", err)
	}
}

func TestResumeSkipsMappedIDs(t *testing.T) {
	// A seeder with no API client and no database: any step that sent a request or ran a query
	// would panic, so each step below passing proves that a logged id replay_ids holds is skipped.
	ids := newIDStore(nil, map[[2]string]string{
		{idIssue, "PRJ-1"}: "PRJ-1", {idArtifact, "d1"}: "local-d1", {idArtifact, "f1"}: "local-f1",
		{idComment, "c1"}: "local-c1", {idResolve, "c1"}: "local-c1", {idAsk, "a1"}: "local-a1", {idAnswer, "a1"}: "local-a1",
	})
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
	if !ids.mapped(idArtifact, "local-f1") || ids.count(idArtifact) != 2 || ids.len() != 7 {
		t.Fatal("mapped or counts wrong")
	}
}

func TestFileBytesDeterministic(t *testing.T) {
	a := fileBytes(1, "f1", 37)
	if len(a) != 37 || !slices.Equal(a, fileBytes(1, "f1", 37)) || slices.Equal(a, fileBytes(2, "f1", 37)) {
		t.Fatal("file bytes must be deterministic in seed and id, and sized")
	}
}

func TestIDOfAndErrorCode(t *testing.T) {
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
	if errorCode([]byte(`{"code":"ANCHOR_MISSING","error":"synthetic"}`)) != "ANCHOR_MISSING" || errorCode([]byte("not json")) != "" {
		t.Fatal("errorCode")
	}
}

func TestWithDatabase(t *testing.T) {
	got, err := withDatabase("postgres://postgres@127.0.0.1:15432/dispatch?sslmode=disable", "dispatch_replay_build")
	if err != nil || got != "postgres://postgres@127.0.0.1:15432/dispatch_replay_build?sslmode=disable" {
		t.Fatalf("withDatabase = %q, %v", got, err)
	}
}

// TestHistoryPadsFromStoredSizes walks a document's history as history does, against a fake
// Dispatch that stores every version shift bytes off what was sent (a base it canonicalises to
// other bytes) and an answer drift bytes off production's change: version 1 lands off its plan,
// yet every padded and restored version changes size by exactly its planned change, only the
// answer version itself carries the drift, and the resolve version changes by the resolve's own
// bytes (state "open" to "resolved").
func TestHistoryPadsFromStoredSizes(t *testing.T) {
	blockID, resolvedID, docID := "b1", "b2", "d1"
	answered := `:::ask{#b1 urgency="med" multiple="false" state="answered" answered_by="u@example.com" answered_at="2026-10-25T10:00:00.5Z" selected="[]" answer="yes"}`
	resolved := `:::ask{#b2 urgency="high" multiple="false" state="resolved"}`
	text := "Intro paragraph.\n\n" + answered + "\nShip it?\n:::\n\n" + resolved + "\nStill needed?\n:::\n\n## One\n\n" + strings.Repeat("first section words. ", 20) +
		"\n\n## Two\n\n" + strings.Repeat("second section words. ", 20) + "\n"
	stripped := stripAnswers(text, map[string]bool{blockID: true})
	grow := len(text) - len(stripped)
	real := []int{300, 340, 340 + grow, 344 + grow, 200, 260, len(text) + 40}
	is := &replayIssue{Key: "PRJ-1", Asks: []replayAsk{
		{ID: "a1", BlockID: &blockID, BlockArtifactID: &docID, State: "answered", AnswerVersion: 3},
		{ID: "a2", BlockID: &resolvedID, BlockArtifactID: &docID, State: "resolved", ResolveVersion: 4,
			Resolution: &replayAskResolution{Kind: "resolved", Reason: "done", By: replayActor{Kind: "session", ID: "s1"}}},
	}}
	a := &replayArtifact{ID: docID, Kind: "doc", Version: len(real), VersionSizes: real}
	p, err := docPlan(is, a, text)
	if err != nil {
		t.Fatal(err)
	}
	if p.Restore != 7 || !slices.Equal(p.AskAt[3], []askStep{{Ask: 0}}) || !slices.Equal(p.AskAt[4], []askStep{{Ask: 1, Resolve: true}}) || p.Unreachable != 0 {
		t.Fatalf("plan %+v", p)
	}
	strippedLine := stripAnswers(answered, map[string]bool{blockID: true})
	openLine := stripAnswers(resolved, map[string]bool{resolvedID: true})
	if first := firstVersionText(is, a, text, p); !strings.Contains(first, openLine) || strings.Contains(first, resolved) || !strings.Contains(first, strippedLine) {
		t.Fatal("version 1 must hold both blocks open")
	}
	for _, c := range []struct{ shift, drift int }{{-4, 0}, {3, 0}, {-2, 1}} {
		md := firstVersionText(is, a, text, p)
		drift := 0
		stored := func() int { return len(md) + c.shift + drift }
		sizes := []int{stored()}
		if sizes[0] == p.Sizes[0] {
			t.Fatalf("shift %d: version 1 must land off its plan", c.shift)
		}
		for n := 2; n <= p.Final; n++ {
			answer := false
			switch {
			case len(p.AskAt[n]) > 0:
				for _, st := range p.AskAt[n] {
					if st.Resolve {
						md = strings.Replace(md, openLine, resolved, 1)
					} else {
						md = strings.Replace(md, strippedLine, answered, 1)
						drift += c.drift
						answer = true
					}
				}
			case n == p.Restore:
				md = text
			default:
				pad := currentPad(md)
				l := p.padLen(n, len(pad))
				if pad == "" || l < padMin {
					t.Fatalf("shift %d: version %d: padding %q, next %d bytes", c.shift, n, pad, l)
				}
				find, with := padEdit(pad, padNext(pad, l))
				if strings.Count(md, find) != 1 {
					t.Fatalf("shift %d: version %d: the padding edit's find occurs %d times", c.shift, n, strings.Count(md, find))
				}
				md = strings.Replace(md, find, with, 1)
			}
			sizes = append(sizes, stored())
			want := p.Sizes[n-1] - p.Sizes[n-2]
			if answer {
				want += c.drift
			}
			if got := sizes[n-1] - sizes[n-2]; got != want {
				t.Fatalf("shift %d drift %d: version %d changes %d, want %d", c.shift, c.drift, n, got, want)
			}
		}
		if md != text {
			t.Fatalf("shift %d: the history ends off the text at At", c.shift)
		}
	}
}
