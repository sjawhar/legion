package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Synthetic bundles only: every text here is made up.

// rpDetIssues is how many existing issues the determinism bundle holds.
const rpDetIssues = 24

// rpDetBundle is a bundle whose window exercises every random draw of the plan: issue sampling,
// the upload split, read counts and kinds, search words, comment and document picks, anchor
// substitutes, edit substitutes (on existing, created and window-created documents), synthetic
// specs and documents, and file bytes.
func rpDetBundle(t *testing.T) *replayBundle {
	t.Helper()
	st := replayState{Project: "LINUX", At: rpAt, Until: rpAt.Add(39 * time.Minute)}
	files := map[string]string{}
	var lines []string
	id := int64(0)
	ev := func(typ, key string, actor replayActor, off time.Duration, payload any) {
		id++
		lines = append(lines, rpEvent(id, typ, key, actor, off, payload))
	}
	cut := "--- a\n+++ b\n@@ -1,1 +1,1 @@\n-x\n+y\n… (truncated)\n"
	for i := 1; i <= rpDetIssues; i++ {
		key, n := "LINUX-"+itoa(i), itoa(i)
		text := replaySynthDoc(1500+37*i, uint64(i))
		fsize := 100 + i
		files["docs/DD"+n+".md"] = text
		st.Issues = append(st.Issues, replayIssue{Key: key, Number: i, Exists: true, Title: "t" + n, Status: "open",
			Artifacts: []replayArtifact{
				{ID: "DD" + n, Name: "spec", Kind: "doc", Primary: true, Version: 1, TextPath: "docs/DD" + n + ".md", TextVersion: 1, VersionSizes: []int{len(text)}},
				{ID: "F" + n, Name: "blob.bin", Kind: "file", Version: 1, Size: &fsize},
			},
			Comments: []replayComment{{ID: "C" + n, Actor: rpSession, Body: "c", Open: true}}})
		quote := fallbackCandidates(text)[0]
		child, cn := "LINUX-"+itoa(100+i), itoa(100+i)
		at := func(k int) time.Duration { return time.Duration(i)*time.Millisecond + time.Duration(k)*20*time.Second }
		ev("comment.created", key, rpSession, at(0), map[string]any{"id": "A" + n + "a", "body": "b", "anchor": map[string]any{"artifact_id": "DD" + n, "quote": quote}})
		ev("comment.created", key, rpUser, at(1), map[string]any{"id": "A" + n + "b", "body": "b", "anchor": map[string]any{"artifact_id": "DD" + n, "quote": "No such quote here."}})
		ev("comment.created", key, rpSession, at(2), map[string]any{"id": "R" + n, "body": "b", "reply_to": "C" + n})
		ev("artifact.version", key, rpSession, at(3), map[string]any{"artifact_id": "DD" + n, "name": "spec", "diff": cut,
			"version": map[string]any{"number": 2, "named": true, "size": len(text) + 40, "summary": "s"}})
		ev("artifact.version", key, rpSession, at(4), map[string]any{"artifact_id": "DD" + n, "name": "spec",
			"version": map[string]any{"number": 3, "named": false, "size": len(text) + 25}})
		ev("comment.created", key, rpSession, at(5), map[string]any{"id": "A" + n + "c", "body": "b", "anchor": map[string]any{"artifact_id": "DD" + n, "quote": "Gone."}})
		ev("artifact.version", key, rpSession, at(6), map[string]any{"artifact_id": "F" + n, "name": "blob.bin",
			"version": map[string]any{"number": 2, "named": true, "size": 300 + i, "mime": "application/octet-stream"}})
		ev("issue.created", child, rpSession, at(7), map[string]any{"key": child, "title": "c" + cn, "parent": key, "primary_artifact_id": "DD" + cn})
		ev("artifact.version", child, rpSession, at(8), map[string]any{"artifact_id": "DD" + cn, "name": "spec",
			"version": map[string]any{"number": 2, "named": false, "size": 900 + i}})
		ev("comment.created", child, rpSession, at(9), map[string]any{"id": "P" + n, "body": "b"})
		ev("comment.created", child, rpUser, at(10), map[string]any{"id": "Q" + n, "body": "b", "reply_to": "P" + n})
		ev("artifact.created", key, rpSession, at(11), map[string]any{"artifact": map[string]any{"id": "N" + n, "name": "notes", "kind": "doc",
			"versions": []any{map[string]any{"size": 700 + i}}}})
		ev("artifact.version", key, rpSession, at(12), map[string]any{"artifact_id": "N" + n, "name": "notes",
			"version": map[string]any{"number": 2, "named": false, "size": 690 + i}})
		ev("artifact.created", key, rpSession, at(13), map[string]any{"artifact": map[string]any{"id": "G" + n, "name": "pic.png", "kind": "image",
			"versions": []any{map[string]any{"size": 200 + i, "mime": "image/png"}}}})
		ev("comment.resolved", key, rpSession, at(14), map[string]any{"id": "C" + n})
		ev("issue.updated", key, rpSession, at(15), map[string]any{"key": key, "title": "t" + n, "status": "done", "parent": nil, "priority": nil,
			"labels": []string{}, "route": nil, "blocked_by": []string{}})
	}
	return rpBundle(t, st, lines, files)
}

func rpDetOptions(seed uint64) replayOptions {
	o := rpOptions()
	o.rate, o.reads, o.uploads, o.rngSeed = 0.75, 1.5, 0.5, seed
	return o
}

// actionDigests are each action's digest: its JSON (kind, offset, every request's method, path
// template, parameters, body and file), plus, when sent, what the server received for it.
func actionDigests(t *testing.T, p *replayPlan, sent [][]string) []string {
	t.Helper()
	out := make([]string, len(p.Actions))
	for i, a := range p.Actions {
		raw, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.New()
		h.Write(raw)
		if sent != nil {
			for _, r := range sent[i] {
				h.Write([]byte{0})
				h.Write([]byte(r))
			}
		}
		out[i] = hex.EncodeToString(h.Sum(nil))
	}
	return out
}

// sendShuffled sends every action of p, started in a random order, to a fake Dispatch that
// answers after a random delay with ids drawn from the request itself. Each action's base URL is
// /a/<index>, so the server files each request under its action. It returns, per action, its
// requests (method, resolved path, identity, content type, body hash), sorted: the requests of one
// stage go in parallel.
func sendShuffled(t *testing.T, p *replayPlan, o replayOptions, b *replayBundle) [][]string {
	t.Helper()
	var mu sync.Mutex
	sent := make([][]string, len(p.Actions))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		uri := strings.TrimPrefix(r.URL.RequestURI(), "/a/")
		n, rest, _ := strings.Cut(uri, "/")
		i, err := strconv.Atoi(n)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		rest = "/" + rest
		sum := sha256.Sum256(body)
		rec := strings.Join([]string{r.Method, rest, r.Header.Get("X-Dispatch-User"), r.Header.Get("Content-Type"), hex.EncodeToString(sum[:])}, " ")
		mu.Lock()
		sent[i] = append(sent[i], rec)
		mu.Unlock()
		time.Sleep(rand.N(3 * time.Millisecond))
		h := sha256.Sum256([]byte(r.Method + " " + rest + " " + string(body)))
		x := hex.EncodeToString(h[:])
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "loc-" + x[:12], "key": "LINUX-L" + x[:6], "primary_artifact_id": "pa-" + x[:12],
			"last_seq": 25, "edited_at": "2026-10-26T23:00:00Z"})
	}))
	defer srv.Close()

	ids := map[[2]string]string{}
	for _, is := range b.State.Issues {
		ids[[2]string{idIssue, is.Key}] = is.Key
		for _, a := range is.Artifacts {
			ids[[2]string{idArtifact, a.ID}] = "l-" + a.ID
		}
		for _, c := range is.Comments {
			ids[[2]string{idComment, c.ID}] = "l-" + c.ID
		}
	}
	rr := &replayRun{setup: &replaySetup{plan: p, opt: o}, futures: map[replayRef]*replayFuture{}, ids: ids,
		writes: map[string]*replayWriteCount{}, skipped: map[string]int{}}
	for _, a := range p.Actions {
		if rr.writes[a.Kind] == nil {
			rr.writes[a.Kind] = &replayWriteCount{}
		}
		for _, st := range a.Stages {
			for _, r := range st {
				for _, pr := range r.Produces {
					rr.futures[pr.Ref] = &replayFuture{done: make(chan struct{})}
				}
			}
		}
	}
	recs := newCollector[reqRec](1 << 12)
	var wg sync.WaitGroup
	for _, i := range rand.Perm(len(p.Actions)) {
		a := p.Actions[i]
		base := srv.URL + "/a/" + itoa(i)
		wg.Go(func() { rr.do(context.Background(), base, "tok", a, recs) })
	}
	wg.Wait()
	for k, wc := range rr.writes {
		if wc.Failed != 0 {
			t.Fatalf("%d %s writes failed", wc.Failed, k)
		}
	}
	if len(rr.skipped) != 0 {
		t.Fatalf("skipped %v: every logged object must resolve", rr.skipped)
	}
	for i := range sent {
		slices.Sort(sent[i])
	}
	return sent
}

// TestReplayDeterministic: one bundle, one set of inputs and one rng seed send the same requests,
// action by action, whatever order the actions start in and however long Dispatch takes; another
// rng seed sends other content.
func TestReplayDeterministic(t *testing.T) {
	o := rpDetOptions(7)
	b1, b2 := rpDetBundle(t), rpDetBundle(t)
	p1, err := planReplay(b1, o)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := planReplay(b2, o)
	if err != nil {
		t.Fatal(err)
	}
	// The window reaches every draw.
	c := p1.Counts
	for _, k := range []string{rkAnchored, rkReply, rkUpload, rkEdit, rkEditUnnamed, rkFileVersion, rkFileCreate, rkIssueCreate, rkDocCreate, rkResolve, rkStatus} {
		if c.WritesByKind[k] == 0 {
			t.Fatalf("no %s write planned: %v", k, c.WritesByKind)
		}
	}
	for _, k := range replayReadKinds {
		if c.ReadsByKind[k] == 0 {
			t.Fatalf("no %s read planned: %v", k, c.ReadsByKind)
		}
	}
	if c.Substitutes.Quote == 0 || c.Substitutes.Edit == 0 || c.Edits.Substitute == 0 || c.IssuesKept == rpDetIssues*2 || c.DroppedTransactions == 0 {
		t.Fatalf("counts %+v", c)
	}

	d1, d2 := actionDigests(t, p1, nil), actionDigests(t, p2, nil)
	if !slices.Equal(d1, d2) {
		t.Fatal("two plans of one bundle, inputs and rng seed differ")
	}
	s1 := actionDigests(t, p1, sendShuffled(t, p1, o, b1))
	s2 := actionDigests(t, p2, sendShuffled(t, p2, o, b2))
	for i := range s1 {
		if s1[i] != s2[i] {
			a := p1.Actions[i]
			t.Fatalf("action %d (%s on %s at %s) sent other requests in the second run", i, a.Kind, a.Issue, a.Offset)
		}
	}

	p3, err := planReplay(b1, rpDetOptions(8))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Equal(d1, actionDigests(t, p3, nil)) {
		t.Fatal("rng seeds 7 and 8 plan the same requests")
	}
	// Every issue kept, so only the draws differ: the substitute quotes and edits still differ.
	o.rate = 1
	pa, _ := planReplay(b1, o)
	o.rngSeed = 8
	pb, _ := planReplay(b1, o)
	quotes := func(p *replayPlan) []string {
		var out []string
		for _, a := range p.Actions {
			if a.QuoteSub {
				out = append(out, lastBody(a)["anchor"].(map[string]any)["quote"].(string))
			}
		}
		return out
	}
	if qa, qb := quotes(pa), quotes(pb); len(qa) == 0 || slices.Equal(qa, qb) {
		t.Fatalf("with every issue kept, rng seeds 7 and 8 substitute the same %d quotes", len(qa))
	}
}

// TestReplayReadsPrecedeTheirWrite: the reads before a write never pick the comment or document
// that write creates.
func TestReplayReadsPrecedeTheirWrite(t *testing.T) {
	o := rpOptions()
	o.reads = 2
	o.mix, _ = parseReadMix("commentref=1,docread=1")
	var lines []string
	for i := range 40 {
		off := time.Duration(i+1) * time.Second
		if i%2 == 0 {
			lines = append(lines, rpEvent(int64(i+1), "comment.created", "LINUX-1", rpSession, off, map[string]any{"id": "W" + itoa(i), "body": "b"}))
		} else {
			lines = append(lines, rpEvent(int64(i+1), "artifact.created", "LINUX-1", rpSession, off,
				map[string]any{"artifact": map[string]any{"id": "W" + itoa(i), "name": "n" + itoa(i), "kind": "doc", "versions": []any{map[string]any{"size": 50}}}}))
		}
	}
	p := rpPlan(t, o, lines, nil)
	created := map[string]bool{}
	for _, a := range p.Actions {
		if a.Write {
			created["W"+itoa(int(a.EventID-1))] = true
			continue
		}
		for _, ref := range a.refs() {
			if strings.HasPrefix(ref.Logged, "W") && !created[ref.Logged] {
				t.Fatalf("a %s read at %s names %s before the write that creates it", a.Kind, a.Offset, ref.Logged)
			}
		}
	}
}
