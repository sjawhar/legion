package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Synthetic bundles only: every text here is made up.

var rpAt = time.Date(2026, 10, 26, 22, 55, 0, 0, time.UTC)

var (
	rpSession = replayActor{Kind: "session", ID: "s-1"}
	rpDaemon  = replayActor{Kind: "session", ID: "legion-daemon:LINUX"}
	rpUser    = replayActor{Kind: "user", ID: "person-a"}
)

// rpDocText is a document whose sentences fallbackCandidates accepts.
const rpDocText = "# Plan\n\nAlpha bravo charlie delta echo foxtrot golf. Hotel india juliet kilo lima mike november.\n\nOscar papa quebec romeo sierra tango uniform. Victor whiskey xray yankee zulu again here.\n\nThe quick brown fox jumps over the lazy dog today.\n"

// rpEvent is one window.jsonl line.
func rpEvent(id int64, typ, key string, actor replayActor, off time.Duration, payload any) string {
	p, _ := json.Marshal(payload)
	ev := map[string]any{"id": id, "issue_key": key, "project": "LINUX", "seq": id, "type": typ, "actor": actor,
		"notify": false, "created_at": rpAt.Add(off), "payload": json.RawMessage(p)}
	b, _ := json.Marshal(ev)
	return string(b)
}

// rpState is two issues: LINUX-1 with document D1 (rpDocText) and comment C1, LINUX-2 with a file
// F2, claimed by session s-1.
func rpState() replayState {
	size := len(rpDocText)
	fsize := 100
	return replayState{Project: "LINUX", At: rpAt, Until: rpAt.Add(39 * time.Minute), Issues: []replayIssue{
		{Key: "LINUX-1", Number: 1, Exists: true, Title: "one", Status: "open",
			Artifacts: []replayArtifact{{ID: "D1", Name: "spec", Kind: "doc", Primary: true, Version: 1, TextPath: "docs/D1.md", TextVersion: 1, VersionSizes: []int{size}}},
			Comments:  []replayComment{{ID: "C1", Actor: rpSession, Body: "first", Open: true}}},
		{Key: "LINUX-2", Number: 2, Exists: true, Title: "two", Status: "open", Claim: &replayClaim{Actor: rpSession},
			Artifacts: []replayArtifact{{ID: "F2", Name: "blob.bin", Kind: "file", Version: 1, Size: &fsize}}},
	}}
}

// rpBundle writes a synthetic bundle and reads it back.
func rpBundle(t *testing.T, st replayState, lines []string, files map[string]string) *replayBundle {
	t.Helper()
	dir := t.TempDir()
	meta := replayMeta{Project: st.Project, At: st.At, Until: st.Until, Files: map[string]string{}}
	put := func(rel string, data []byte) {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		meta.Files[rel] = sha256Hex(data)
	}
	stateJSON, _ := json.Marshal(st)
	put(bundleStateFile, stateJSON)
	put(bundleRosterFile, []byte(`[{"session_id":"s-1","source":"claim"}]`))
	put(bundleWindowFile, []byte(strings.Join(lines, "\n")+"\n"))
	put("docs/D1.md", []byte(rpDocText))
	for rel, text := range files {
		put(rel, []byte(text))
	}
	metaJSON, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(dir, bundleMetaFile), metaJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := readBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func rpOptions() replayOptions {
	mix, _ := parseReadMix(defaultReplayReadMix)
	return replayOptions{rate: 1, reads: 0, mix: mix, uploads: 0, from: 0, to: 39 * time.Minute, roster: 5, rngSeed: 1}
}

func rpPlan(t *testing.T, o replayOptions, lines []string, files map[string]string) *replayPlan {
	t.Helper()
	p, err := planReplay(rpBundle(t, rpState(), lines, files), o)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// routes are an action's requests by stage.
func routes(a *replayAction) [][]string {
	var out [][]string
	for _, st := range a.Stages {
		var s []string
		for _, r := range st {
			s = append(s, r.route(a.Kind))
		}
		out = append(out, s)
	}
	return out
}

func onlyWrite(t *testing.T, p *replayPlan) *replayAction {
	t.Helper()
	var ws []*replayAction
	for _, a := range p.Actions {
		if a.Write {
			ws = append(ws, a)
		}
	}
	if len(ws) != 1 {
		t.Fatalf("%d write actions, want 1 (not sent: %v)", len(ws), p.Counts.NotSent)
	}
	return ws[0]
}

func wantRoutes(t *testing.T, a *replayAction, want ...[]string) {
	t.Helper()
	if got := routes(a); !reflect.DeepEqual(got, want) {
		t.Fatalf("routes\n got %q\nwant %q", got, want)
	}
}

func lastBody(a *replayAction) map[string]any {
	st := a.Stages[len(a.Stages)-1]
	return st[len(st)-1].Body
}

func TestReplayAnchoredRoot(t *testing.T) {
	quote := "Oscar papa quebec romeo sierra tango uniform."
	p := rpPlan(t, rpOptions(), []string{rpEvent(1, "comment.created", "LINUX-1", rpSession, time.Second,
		map[string]any{"id": "C9", "body": "b", "anchor": map[string]any{"artifact_id": "D1", "quote": quote}})}, nil)
	a := onlyWrite(t, p)
	wantRoutes(t, a, []string{"GET /api/v1/issues/{key} [anchored]"}, []string{"POST /api/v1/issues/{key}/comments [anchored]"})
	anchor := lastBody(a)["anchor"].(map[string]any)
	if anchor["artifact"] != (replayRef{idArtifact, "D1"}) || anchor["quote"] != quote || a.QuoteSub {
		t.Fatalf("anchor %v (substitute %v), want the logged quote on D1", anchor, a.QuoteSub)
	}

	// A quote the client's text lacks is substituted by one it holds exactly once.
	p = rpPlan(t, rpOptions(), []string{rpEvent(1, "comment.created", "LINUX-1", rpSession, time.Second,
		map[string]any{"id": "C9", "body": "b", "anchor": map[string]any{"artifact_id": "D1", "quote": "not in the text at all"}})}, nil)
	a = onlyWrite(t, p)
	q := lastBody(a)["anchor"].(map[string]any)["quote"].(string)
	if !a.QuoteSub || strings.Count(rpDocText, q) != 1 || p.Counts.Substitutes.Quote != 1 {
		t.Fatalf("substitute %v, quote found %d times, counted %d", a.QuoteSub, strings.Count(rpDocText, q), p.Counts.Substitutes.Quote)
	}
}

func TestReplayReply(t *testing.T) {
	p := rpPlan(t, rpOptions(), []string{rpEvent(1, "comment.created", "LINUX-1", rpSession, time.Second,
		map[string]any{"id": "C9", "body": "b", "reply_to": "C1"})}, nil)
	a := onlyWrite(t, p)
	wantRoutes(t, a, []string{"POST /api/v1/issues/{key}/comments [reply]"})
	if lastBody(a)["reply_to"] != (replayRef{idComment, "C1"}) {
		t.Fatalf("reply_to %v", lastBody(a)["reply_to"])
	}
	if got := a.Stages[0][0].Produces; len(got) != 1 || got[0].Ref != (replayRef{idComment, "C9"}) {
		t.Fatalf("produces %v", got)
	}
}

func TestReplayPlainRoot(t *testing.T) {
	p := rpPlan(t, rpOptions(), []string{rpEvent(1, "comment.created", "LINUX-1", rpSession, time.Second, map[string]any{"id": "C9", "body": "b"})}, nil)
	a := onlyWrite(t, p)
	wantRoutes(t, a, []string{"POST /api/v1/issues/{key}/comments [plain]"})
	body := lastBody(a)
	if _, ok := body["anchor"]; ok || body["actor"] == nil {
		t.Fatalf("body keys %v", slices.Sorted(maps.Keys(body)))
	}
}

// rpDiff is a complete diff of rpDocText replacing its last sentence.
const rpDiff = "--- v1\n+++ v2\n@@ -6,2 +6,2 @@\n \n-The quick brown fox jumps over the lazy dog today.\n+The quick brown fox jumps over the lazy cat today.\n"

func rpVersionEvent(id int64, actor replayActor, named bool, diff *string, summary *string, size int) string {
	return rpEvent(id, "artifact.version", "LINUX-1", actor, time.Second, map[string]any{"artifact_id": "D1", "name": "spec", "diff": diff,
		"version": map[string]any{"number": 2, "named": named, "size": size, "summary": summary}})
}

func TestReplayNamedVersionUploadOrEdit(t *testing.T) {
	diff, summary := rpDiff, "s"
	line := rpVersionEvent(1, rpSession, true, &diff, &summary, len(rpDocText))
	want := strings.Replace(rpDocText, "lazy dog", "lazy cat", 1)

	o := rpOptions()
	o.uploads = 1
	a := onlyWrite(t, rpPlan(t, o, []string{line}, nil))
	wantRoutes(t, a, []string{"POST /api/v1/issues/{key}/artifacts [upload]"})
	if b := lastBody(a); b["content"] != want || b["name"] != "spec" || b["summary"] != "s" {
		t.Fatalf("upload body: content matches %v, name %v", b["content"] == want, b["name"])
	}

	o.uploads = 0
	a = onlyWrite(t, rpPlan(t, o, []string{line}, nil))
	wantRoutes(t, a, []string{"GET /api/v1/issues/{key} [edit]"}, []string{"POST /api/v1/artifacts/{id}/edits [edit]"})
	b := lastBody(a)
	ops := b["ops"].([]replayEditOp)
	if !a.EditDiff || len(ops) != 1 || ops[0].Op != "replace" || ops[0].Find != "dog" || *ops[0].With != "cat" || b["summary"] != "s" {
		t.Fatalf("edit: diff %v ops %+v summary %v", a.EditDiff, ops, b["summary"])
	}
	if a.Stages[1][0].Params["id"].Ref == nil || *a.Stages[1][0].Params["id"].Ref != (replayRef{idArtifact, "D1"}) {
		t.Fatal("the edit does not name D1")
	}
}

// rpBaseTextRun is a run whose plan came from bundle b, against a fake Dispatch serving served as
// document D1's current markdown.
func rpBaseTextRun(t *testing.T, o replayOptions, b *replayBundle, p *replayPlan, served string) (*replayRun, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/artifacts/local-D1/text" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := json.Marshal(map[string]any{"markdown": served, "version": 1})
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	rr := &replayRun{setup: &replaySetup{plan: p, opt: o, bundle: b}, lg: log.New(io.Discard, "", 0),
		ids: map[[2]string]string{{idArtifact, "D1"}: "local-D1"}}
	return rr, srv.URL
}

// TestReplayBaseTextsMatchTheBuild: before the window the run reads each modelled document's
// current markdown from the build under test. Where the build serves the bundle's text the plan
// is untouched; where it serves another text the plan is built again from it, so an upload sends
// the build's own text with the edit applied, not the bundle's.
func TestReplayBaseTextsMatchTheBuild(t *testing.T) {
	diff, summary := rpDiff, "s"
	lines := []string{rpVersionEvent(1, rpSession, true, &diff, &summary, len(rpDocText))}
	o := rpOptions()
	o.uploads = 1
	b := rpBundle(t, rpState(), lines, nil)
	p, err := planReplay(b, o)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.BaseDocs["D1"]; got != rpDocText {
		t.Fatalf("the plan modelled D1 from %d bytes, not the bundle's %d", len(got), len(rpDocText))
	}
	want := actionDigests(t, p, nil)

	rr, base := rpBaseTextRun(t, o, b, p, rpDocText)
	if err := rr.readBaseTexts(context.Background(), newAPI(base, "tok", 2)); err != nil {
		t.Fatal(err)
	}
	if rec := rr.baseDocs; rec == nil || rec.Read != 1 || rec.Unread != 0 || rec.Differed != 0 || rec.Replanned {
		t.Fatalf("base docs %+v", rr.baseDocs)
	}
	if got := actionDigests(t, rr.setup.plan, nil); !slices.Equal(got, want) {
		t.Fatal("the plan changed though the build serves the bundle's text")
	}

	served := strings.Replace(rpDocText, "# Plan", "# Plan of record", 1)
	rr, base = rpBaseTextRun(t, o, b, p, served)
	if err := rr.readBaseTexts(context.Background(), newAPI(base, "tok", 2)); err != nil {
		t.Fatal(err)
	}
	rec := rr.baseDocs
	if rec == nil || rec.Read != 1 || rec.Differed != 1 || !rec.Replanned || !slices.Equal(rec.Documents, []string{"D1"}) {
		t.Fatalf("base docs %+v", rec)
	}
	if got := rr.setup.plan.BaseDocs["D1"]; got != served {
		t.Fatalf("the replan modelled D1 from %d bytes, not the build's %d", len(got), len(served))
	}
	if got := lastBody(onlyWrite(t, rr.setup.plan))["content"]; got != strings.Replace(served, "lazy dog", "lazy cat", 1) {
		t.Fatalf("the upload does not send the build's text with the edit applied (%d bytes)", len(got.(string)))
	}
	if slices.Equal(actionDigests(t, rr.setup.plan, nil), want) {
		t.Fatal("the replanned upload sends the bundle's text")
	}
}

// TestReplayBaseTextsUnreadable: a document the build will not serve keeps the bundle's text and
// is counted unread; a plan whose documents are all unreadable is refused.
func TestReplayBaseTextsUnreadable(t *testing.T) {
	diff, summary := rpDiff, "s"
	lines := []string{rpVersionEvent(1, rpSession, true, &diff, &summary, len(rpDocText))}
	o := rpOptions()
	o.uploads = 1
	b := rpBundle(t, rpState(), lines, nil)
	p, err := planReplay(b, o)
	if err != nil {
		t.Fatal(err)
	}
	rr, base := rpBaseTextRun(t, o, b, p, rpDocText)
	rr.ids = map[[2]string]string{}
	err = rr.readBaseTexts(context.Background(), newAPI(base, "tok", 2))
	if err == nil || !strings.Contains(err.Error(), "base texts") {
		t.Fatalf("an unmapped document: %v", err)
	}
	if got := actionDigests(t, rr.setup.plan, nil); !slices.Equal(got, actionDigests(t, p, nil)) {
		t.Fatal("the plan changed though no text was read")
	}
}

func TestReplayUnnamedVersionEdit(t *testing.T) {
	a := onlyWrite(t, rpPlan(t, rpOptions(), []string{rpVersionEvent(1, rpSession, false, nil, nil, len(rpDocText)+7)}, nil))
	wantRoutes(t, a, []string{"GET /api/v1/issues/{key} [edit-unnamed]"}, []string{"POST /api/v1/artifacts/{id}/edits [edit-unnamed]"})
	b := lastBody(a)
	if _, ok := b["summary"]; ok || !a.EditSub {
		t.Fatalf("summary present %v, substitute %v", ok, a.EditSub)
	}
	op := b["ops"].([]replayEditOp)[0]
	if len(*op.With)-len(op.Find) != 7 {
		t.Fatalf("substitute changes %d bytes, want 7", len(*op.With)-len(op.Find))
	}
}

func TestReplayVersionsNotSent(t *testing.T) {
	s := "s"
	p := rpPlan(t, rpOptions(), []string{
		rpVersionEvent(1, rpSession, true, nil, &s, len(rpDocText)),
		rpEvent(2, "artifact.version", "LINUX-1", rpUser, 2*time.Second, map[string]any{"artifact_id": "D1", "name": "spec", "version": map[string]any{"number": 3, "size": 10}}),
	}, nil)
	if p.Counts.WriteActions != 0 || p.Counts.NotSent["settlement:artifact.version"] != 1 || p.Counts.NotSent["browser:artifact.version"] != 1 {
		t.Fatalf("writes %d, not sent %v", p.Counts.WriteActions, p.Counts.NotSent)
	}
}

func rpIssueUpdate(id int64, actor replayActor, fields map[string]any) string {
	payload := map[string]any{"key": "LINUX-1", "title": "one", "status": "open", "parent": nil, "priority": nil, "labels": []string{}, "route": nil, "blocked_by": []string{}}
	for k, v := range fields {
		payload[k] = v
	}
	return rpEvent(id, "issue.updated", "LINUX-1", actor, time.Second, payload)
}

func TestReplayStatusChange(t *testing.T) {
	a := onlyWrite(t, rpPlan(t, rpOptions(), []string{rpIssueUpdate(1, rpSession, map[string]any{"status": "in_progress"})}, nil))
	wantRoutes(t, a, []string{"GET /api/v1/issues/{key} [status]"}, []string{"PATCH /api/v1/issues/{key} [status]"})
	if b := lastBody(a); string(b["status"].(json.RawMessage)) != `"in_progress"` || len(b) != 2 {
		t.Fatalf("patch keys %v", slices.Sorted(maps.Keys(b)))
	}
}

func TestReplayDaemonWrites(t *testing.T) {
	a := onlyWrite(t, rpPlan(t, rpOptions(), []string{rpIssueUpdate(1, rpDaemon, map[string]any{"status": "done"})}, nil))
	wantRoutes(t, a, []string{"GET /api/v1/issues/{key} [daemon]"}, []string{"PATCH /api/v1/issues/{key} [daemon]"})
	a = onlyWrite(t, rpPlan(t, rpOptions(), []string{rpEvent(1, "message.created", "LINUX-1", rpDaemon, time.Second, map[string]any{"body": "m"})}, nil))
	wantRoutes(t, a, []string{"GET /api/v1/issues/{key}/events?order=desc&limit=200 [daemon-message]"}, []string{"POST /api/v1/issues/{key}/messages [daemon-message]"})
	// An issue.closed alone is its PATCH's only event.
	a = onlyWrite(t, rpPlan(t, rpOptions(), []string{
		rpEvent(1, "issue.closed", "LINUX-1", rpDaemon, time.Second, map[string]any{"key": "LINUX-1", "status": "done", "title": "one"}),
		rpEvent(2, "child.status", "LINUX-9", rpDaemon, time.Second, map[string]any{}),
	}, nil))
	wantRoutes(t, a, []string{"GET /api/v1/issues/{key} [daemon]"}, []string{"PATCH /api/v1/issues/{key} [daemon]"})
}

func TestReplayOtherUpdateSendsChangedFields(t *testing.T) {
	p := rpPlan(t, rpOptions(), []string{
		rpIssueUpdate(1, rpSession, map[string]any{"title": "renamed", "assignee": "x"}),
		rpEvent(2, "issue.updated", "LINUX-1", rpSession, 2*time.Second, map[string]any{"key": "LINUX-1", "title": "renamed", "status": "open",
			"parent": nil, "priority": nil, "labels": []string{"a"}, "route": nil, "blocked_by": []string{}, "assignee": "y"}),
	}, nil)
	if len(p.Actions) != 2 {
		t.Fatalf("%d actions", len(p.Actions))
	}
	// The first compares with the state, which has no assignee; the second with the first.
	for i, want := range [][]string{{"actor", "title"}, {"actor", "assignee", "labels"}} {
		a := p.Actions[i]
		wantRoutes(t, a, []string{"GET /api/v1/issues/{key} [fields]"}, []string{"PATCH /api/v1/issues/{key} [fields]"})
		if got := slices.Sorted(maps.Keys(lastBody(a))); !slices.Equal(got, want) {
			t.Fatalf("action %d patches %v, want %v", i, got, want)
		}
	}
}

func TestReplayIssueCreated(t *testing.T) {
	p := rpPlan(t, rpOptions(), []string{
		rpEvent(1, "issue.created", "LINUX-3", rpSession, time.Second, map[string]any{"key": "LINUX-3", "title": "new", "parent": "LINUX-1", "primary_artifact_id": "D3"}),
		rpEvent(2, "artifact.version", "LINUX-3", rpSession, 2*time.Second, map[string]any{"artifact_id": "D3", "name": "spec",
			"version": map[string]any{"number": 2, "named": false, "size": 777}}),
	}, nil)
	a := p.Actions[0]
	if a.Kind != rkIssueCreate {
		t.Fatalf("first action %s", a.Kind)
	}
	wantRoutes(t, a, []string{"POST /api/v1/issues [issue-create]"}, []string{"GET /api/v1/projects/{project}/architecture-sources [issue-create]"},
		[]string{"GET /api/v1/projects/{project}/architecture-source [issue-create]"})
	b := a.Stages[0][0].Body
	if len(b["spec"].(string)) != 777 || b["parent"] != (replayRef{idIssue, "LINUX-1"}) || b["force"] != true || b["project"] != "LINUX" {
		t.Fatalf("spec %d bytes, parent %v", len(b["spec"].(string)), b["parent"])
	}
	if a.Stages[1][0].Tolerate != http.StatusNotFound || p.Counts.SpecSizes["first_version"] != 1 {
		t.Fatal("architecture-sources must tolerate 404; the spec's size is its first logged version's")
	}
	wantProduce := []replayProduce{{Ref: replayRef{idIssue, "LINUX-3"}, Field: "key"}, {Ref: replayRef{idArtifact, "D3"}, Field: "primary_artifact_id"}}
	if !reflect.DeepEqual(a.Stages[0][0].Produces, wantProduce) {
		t.Fatalf("produces %v", a.Stages[0][0].Produces)
	}
}

func TestReplayIssueCreatedVersion1Spec(t *testing.T) {
	text := "Created spec version one, for the test.\n"
	lines := []string{
		rpEvent(1, "issue.created", "LINUX-3", rpSession, time.Second, map[string]any{"key": "LINUX-3", "title": "new", "primary_artifact_id": "D3"}),
		rpEvent(2, "artifact.version", "LINUX-3", rpSession, 2*time.Second, map[string]any{"artifact_id": "D3", "name": "spec",
			"version": map[string]any{"number": 2, "named": false, "size": 777}}),
		// No primary_artifact_id: the bundle's created_specs names the spec.
		rpEvent(3, "issue.created", "LINUX-4", rpSession, 3*time.Second, map[string]any{"key": "LINUX-4", "title": "newer"}),
		// No readable version 1: a synthetic spec of the created documents' median.
		rpEvent(4, "issue.created", "LINUX-5", rpSession, 4*time.Second, map[string]any{"key": "LINUX-5", "title": "newest", "primary_artifact_id": "D5"}),
	}
	b := rpBundle(t, rpState(), lines, map[string]string{"created-docs/D3.md": text, "created-docs/D4.md": text + text})
	b.Meta.CreatedSpecs = map[string]string{"LINUX-3": "D3", "LINUX-4": "D4"}
	p, err := planReplay(b, rpOptions())
	if err != nil {
		t.Fatal(err)
	}
	creates := map[string]*replayAction{}
	for _, a := range p.Actions {
		if a.Kind == rkIssueCreate {
			creates[a.Issue] = a
		}
	}
	if got := creates["LINUX-3"].Stages[0][0].Body["spec"]; got != text {
		t.Fatalf("LINUX-3 spec %q, want its version 1", got)
	}
	if got := creates["LINUX-4"].Stages[0][0].Body["spec"]; got != text+text {
		t.Fatalf("LINUX-4 spec %q, want its version 1", got)
	}
	if got := creates["LINUX-4"].Stages[0][0].Produces[1]; got.Ref != (replayRef{idArtifact, "D4"}) {
		t.Fatalf("LINUX-4 produces %v, want the created_specs id", got)
	}
	if got := len(creates["LINUX-5"].Stages[0][0].Body["spec"].(string)); got != 4096 {
		t.Fatalf("LINUX-5 spec %d bytes, want the 4096-byte default", got)
	}
	if want := map[string]int{"version1_text": 2, "created_docs_median": 1}; !reflect.DeepEqual(p.Counts.SpecSizes, want) {
		t.Fatalf("spec sizes %v, want %v", p.Counts.SpecSizes, want)
	}
}

func TestReplayDocCreated(t *testing.T) {
	text := "Created document text for the test, one sentence.\n"
	a := onlyWrite(t, rpPlan(t, rpOptions(), []string{rpEvent(1, "artifact.created", "LINUX-1", rpSession, time.Second,
		map[string]any{"artifact": map[string]any{"id": "D5", "name": "notes", "kind": "doc", "versions": []any{map[string]any{"size": len(text)}}}})},
		map[string]string{"created-docs/D5.md": text}))
	wantRoutes(t, a, []string{"POST /api/v1/issues/{key}/artifacts [doc-create]"})
	if b := lastBody(a); b["content"] != text || b["name"] != "notes" {
		t.Fatal("the created document is not sent as its created-docs version 1")
	}
}

func TestReplayFileWrites(t *testing.T) {
	mime := "image/png"
	p := rpPlan(t, rpOptions(), []string{
		rpEvent(1, "artifact.created", "LINUX-2", rpSession, time.Second,
			map[string]any{"artifact": map[string]any{"id": "F7", "name": "pic.png", "kind": "image", "versions": []any{map[string]any{"size": 300, "mime": mime}}}}),
		rpEvent(2, "artifact.version", "LINUX-2", rpSession, 2*time.Second, map[string]any{"artifact_id": "F2", "name": "blob.bin",
			"version": map[string]any{"number": 2, "named": true, "size": 500, "mime": "application/octet-stream"}}),
	}, nil)
	if len(p.Actions) != 2 {
		t.Fatalf("%d actions", len(p.Actions))
	}
	wantRoutes(t, p.Actions[0], []string{"POST /api/v1/issues/{key}/artifacts [file-create]"})
	wantRoutes(t, p.Actions[1], []string{"POST /api/v1/issues/{key}/artifacts [file-version]"})
	f := p.Actions[0].Stages[0][0].File
	if f == nil || f.Size != 300 || f.MIME != mime {
		t.Fatalf("file %+v", f)
	}
	ct, body, err := multipartFile(1, f)
	if err != nil || !strings.HasPrefix(ct, "multipart/form-data") || len(body) < 300 {
		t.Fatalf("multipart %q %d bytes %v", ct, len(body), err)
	}
	if p.Actions[1].Stages[0][0].File.Size != 500 {
		t.Fatal("the file version's size is not the logged one")
	}
}

func TestReplayResolve(t *testing.T) {
	a := onlyWrite(t, rpPlan(t, rpOptions(), []string{rpEvent(1, "comment.resolved", "LINUX-1", rpSession, time.Second, map[string]any{"id": "C1"})}, nil))
	wantRoutes(t, a, []string{"POST /api/v1/comments/{id}/resolve [resolve]"})
	if *a.Stages[0][0].Params["id"].Ref != (replayRef{idComment, "C1"}) {
		t.Fatal("resolve does not name C1")
	}
}

func TestReplaySessionMessage(t *testing.T) {
	a := onlyWrite(t, rpPlan(t, rpOptions(), []string{rpEvent(1, "message.created", "LINUX-1", rpSession, time.Second, map[string]any{"body": "m"})}, nil))
	wantRoutes(t, a, []string{"POST /api/v1/issues/{key}/messages [message]"})
}

func TestReplayAsksAndClaim(t *testing.T) {
	p := rpPlan(t, rpOptions(), []string{
		rpEvent(1, "ask.opened", "LINUX-1", rpSession, 1*time.Second, map[string]any{"id": "A1", "question": "q", "kind": "question",
			"options": []any{map[string]any{"label": "x"}}, "urgency": "high"}),
		rpEvent(2, "ask.answered", "LINUX-1", rpUser, 2*time.Second, map[string]any{"id": "A1", "answer": map[string]any{"user": "person-a", "selected": []string{"x"}}}),
		rpEvent(3, "ask.resolved", "LINUX-1", rpSession, 3*time.Second, map[string]any{"id": "A1", "resolution": map[string]any{"kind": "done", "reason": "r"}}),
		rpEvent(4, "issue.claimed", "LINUX-1", rpSession, 4*time.Second, map[string]any{"claim": map[string]any{"actor": rpSession}}),
	}, nil)
	want := [][][]string{
		{{"POST /api/v1/issues/{key}/asks [ask-open]"}},
		{{"GET /api/v1/asks/{id} [ask-answer]"}, {"POST /api/v1/asks/{id}/answer [ask-answer]"}},
		{{"POST /api/v1/asks/{id}/resolve [ask-resolve]"}},
		{{"POST /api/v1/issues/{key}/claim [claim]"}},
	}
	if len(p.Actions) != len(want) {
		t.Fatalf("%d actions", len(p.Actions))
	}
	for i, w := range want {
		wantRoutes(t, p.Actions[i], w...)
	}
	answer := p.Actions[1].Stages[1][0]
	if answer.Person != "person-a" || answer.Body["expected_edited_at"] != (replayPrevEditedAt{}) || p.Actions[1].Stages[0][0].Person != "person-a" {
		t.Fatal("the answer is not sent as the answering person with the ask's edited_at")
	}
}

func TestReplayDerivedNotSent(t *testing.T) {
	p := rpPlan(t, rpOptions(), []string{
		rpEvent(1, "child.status", "LINUX-1", rpSession, time.Second, map[string]any{}),
		rpEvent(2, "comment.anchor_refreshed", "LINUX-1", rpSession, 2*time.Second, map[string]any{"id": "C1"}),
		rpEvent(3, "comment.reopened", "LINUX-1", rpSession, 3*time.Second, map[string]any{"id": "C1"}),
		rpEvent(4, "message.delivery", "LINUX-1", rpSession, 4*time.Second, map[string]any{}),
		rpEvent(5, "user_state.read", "LINUX-1", rpUser, 5*time.Second, map[string]any{}),
	}, nil)
	if p.Counts.Actions != 0 || len(p.Counts.NotSent) != 5 {
		t.Fatalf("actions %d, not sent %v", p.Counts.Actions, p.Counts.NotSent)
	}
}

func TestReplayTransactionGrouping(t *testing.T) {
	// One actor at one instant is one write; the same instant by another actor is another.
	p := rpPlan(t, rpOptions(), []string{
		rpIssueUpdate(1, rpSession, map[string]any{"status": "done"}),
		rpEvent(2, "child.status", "LINUX-9", rpSession, time.Second, map[string]any{}),
		rpEvent(3, "comment.created", "LINUX-1", rpUser, time.Second, map[string]any{"id": "C9", "body": "b"}),
		rpEvent(4, "comment.created", "LINUX-1", rpSession, 2*time.Second, map[string]any{"id": "C10", "body": "b", "reply_to": "C1"}),
		rpEvent(5, "comment.reopened", "LINUX-1", rpSession, 2*time.Second, map[string]any{"id": "C1"}),
	}, nil)
	if p.Counts.TransactionsPlanned != 3 || p.Counts.WriteActions != 3 || p.Counts.NotSent["derived:child.status"] != 1 || p.Counts.NotSent["derived:comment.reopened"] != 1 {
		t.Fatalf("transactions %d writes %d not sent %v", p.Counts.TransactionsPlanned, p.Counts.WriteActions, p.Counts.NotSent)
	}
	kinds := []string{}
	for _, a := range p.Actions {
		kinds = append(kinds, a.Kind)
	}
	if !slices.Equal(kinds, []string{rkStatus, rkPlain, rkReply}) {
		t.Fatalf("kinds %v", kinds)
	}
}

// rpManyIssues is a state of n issues, each with one plain comment per minute for three minutes.
func rpManyIssues(t *testing.T, n int) (*replayBundle, map[string]int) {
	st := rpState()
	var lines []string
	per := map[string]int{}
	id := int64(0)
	for i := 3; i < 3+n; i++ {
		key := "LINUX-" + itoa(i)
		st.Issues = append(st.Issues, replayIssue{Key: key, Number: i, Exists: true, Title: "t", Status: "open"})
		for m := range 3 {
			id++
			lines = append(lines, rpEvent(id, "comment.created", key, rpSession, time.Duration(m)*time.Minute+time.Duration(i)*time.Millisecond, map[string]any{"id": "K" + itoa(int(id)), "body": "b"}))
			per[key]++
		}
	}
	return rpBundle(t, st, lines, nil), per
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestReplaySamplingKeepsIssuesWhole(t *testing.T) {
	b, per := rpManyIssues(t, 400)
	o := rpOptions()
	o.rate = 0.25
	p1, err := planReplay(b, o)
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := planReplay(b, o)
	got := map[string]int{}
	for _, a := range p1.Actions {
		got[a.Issue]++
	}
	for k, n := range got {
		if n != per[k] {
			t.Fatalf("issue %s kept %d of its %d writes", k, n, per[k])
		}
	}
	if len(got) < 70 || len(got) > 130 {
		t.Fatalf("kept %d of 400 issues at 0.25", len(got))
	}
	if !reflect.DeepEqual(routesAll(p1), routesAll(p2)) {
		t.Fatal("one seed planned twice differs")
	}
	o.rngSeed = 2
	p3, _ := planReplay(b, o)
	if reflect.DeepEqual(issuesOf(p1), issuesOf(p3)) {
		t.Fatal("another seed kept the same issues")
	}
}

func routesAll(p *replayPlan) []string {
	var out []string
	for _, a := range p.Actions {
		out = append(out, a.Issue+" "+a.Offset.String()+" "+strings.Join(slices.Concat(routes(a)...), ","))
	}
	return out
}

func issuesOf(p *replayPlan) []string {
	var out []string
	for _, a := range p.Actions {
		if !slices.Contains(out, a.Issue) {
			out = append(out, a.Issue)
		}
	}
	return out
}

func TestReplaySubWindow(t *testing.T) {
	line := func(id int64, off time.Duration) string {
		return rpEvent(id, "comment.created", "LINUX-1", rpSession, off, map[string]any{"id": "X" + itoa(int(id)), "body": "b"})
	}
	o := rpOptions()
	o.from, o.to = time.Minute, 2*time.Minute
	p := rpPlan(t, o, []string{line(1, time.Minute-time.Millisecond), line(2, time.Minute), line(3, 2*time.Minute-time.Millisecond), line(4, 2*time.Minute)}, nil)
	var ids []int64
	for _, a := range p.Actions {
		ids = append(ids, a.EventID)
	}
	if !slices.Equal(ids, []int64{2, 3}) || p.Counts.OutsideWindow != 2 {
		t.Fatalf("kept events %v, outside %d; want [2 3] and 2", ids, p.Counts.OutsideWindow)
	}
	// Reads before the first write never start before the sub-window.
	o.reads = 3
	p = rpPlan(t, o, []string{line(2, time.Minute)}, nil)
	for _, a := range p.Actions {
		if a.Offset < o.from {
			t.Fatalf("%s action at %s, before the sub-window", a.Kind, a.Offset)
		}
	}
}

func TestReplayUploadSplitDeterministic(t *testing.T) {
	share := func(seed uint64) (int, []bool) {
		var picks []bool
		n := 0
		for i := range 4000 {
			up := replayUnit(seed, "upload", itoa(i)) < 0.25
			picks = append(picks, up)
			if up {
				n++
			}
		}
		return n, picks
	}
	n1, a := share(1)
	_, b := share(1)
	_, c := share(2)
	if !slices.Equal(a, b) || slices.Equal(a, c) {
		t.Fatal("the upload split is not one per seed")
	}
	if math.Abs(float64(n1)/4000-0.25) > 0.03 {
		t.Fatalf("upload share %.3f, want 0.25", float64(n1)/4000)
	}
	// In a plan: U=1 uploads every named diff version, U=0 none.
	diff, s := rpDiff, "s"
	o := rpOptions()
	o.uploads = 1
	if a := onlyWrite(t, rpPlan(t, o, []string{rpVersionEvent(1, rpSession, true, &diff, &s, 1)}, nil)); a.Kind != rkUpload {
		t.Fatalf("U=1 planned %s", a.Kind)
	}
}

func TestReplayReadsWithFractionAndMix(t *testing.T) {
	b, per := rpManyIssues(t, 300)
	writes := 0
	for _, n := range per {
		writes += n
	}
	o := rpOptions()
	o.reads = 1.5
	p, err := planReplay(b, o)
	if err != nil {
		t.Fatal(err)
	}
	mean := float64(p.Counts.ReadActions) / float64(p.Counts.WriteActions)
	if p.Counts.WriteActions != writes || math.Abs(mean-1.5) > 0.06 {
		t.Fatalf("%d reads over %d writes (%.3f per write), want 1.5", p.Counts.ReadActions, p.Counts.WriteActions, mean)
	}
	// Each write has 1 or 2 reads before it, on its issue, at most 2 s before it.
	byEvent := map[int64]int{}
	var lastWrite = map[string]time.Duration{}
	for i := len(p.Actions) - 1; i >= 0; i-- {
		a := p.Actions[i]
		if a.Write {
			lastWrite[a.Issue] = a.Offset
			continue
		}
		if d := lastWrite[a.Issue] - a.Offset; d <= 0 || d > 2*replayReadSpacing {
			t.Fatalf("read on %s %s before its write", a.Issue, d)
		}
		byEvent[a.EventID]++
	}
	// Proportions of the default mix; docread and commentref fall back to read on these issues,
	// which have no document or comment, so count those three together.
	o.reads = 4
	p, _ = planReplay(b, o)
	n := float64(p.Counts.ReadActions)
	if s := float64(p.Counts.ReadsByKind[rkSearch]) / n; math.Abs(s-0.05) > 0.015 {
		t.Fatalf("search share %.3f, want 0.05", s)
	}
	// On an issue with documents and comments the mix holds for every kind.
	var lines []string
	for i := range 2000 {
		lines = append(lines, rpEvent(int64(i+1), "comment.created", "LINUX-1", rpSession, time.Duration(i)*time.Second, map[string]any{"id": "Z" + itoa(i), "body": "b"}))
	}
	o.reads = 1
	p = rpPlan(t, o, lines, nil)
	n = float64(p.Counts.ReadActions)
	for k, want := range map[string]float64{rkRead: 0.55, rkDocRead: 0.30, rkCommentRef: 0.10, rkSearch: 0.05} {
		if got := float64(p.Counts.ReadsByKind[k]) / n; math.Abs(got-want) > 0.03 {
			t.Fatalf("%s share %.3f, want %.2f", k, got, want)
		}
	}
	// The read sequences.
	seen := map[string][][]string{}
	for _, a := range p.Actions {
		if !a.Write {
			seen[a.Kind] = routes(a)
		}
	}
	want := map[string][][]string{
		rkRead: {{"GET /api/v1/issues/{key} [read]", "GET /api/v1/issues/{key}/references [read]"}, {"GET /api/v1/issues/{key}/events?after={after}&limit=10 [read]"},
			{"GET /api/v1/references?to={key_ref} [read]", "GET /api/v1/references?from={key_ref} [read]"}},
		rkDocRead:    {{"GET /api/v1/issues/{key} [docread]"}, {"GET /api/v1/artifacts/{id}/text [docread]", "GET /api/v1/issues/{key}/comments?artifact={id} [docread]"}},
		rkCommentRef: {{"GET /api/v1/comments/{id} [commentref]"}, {"GET /api/v1/references?to={comment_ref} [commentref]", "GET /api/v1/references?from={comment_ref} [commentref]"}},
		rkSearch:     {{"GET /api/v1/search?q={q}&project={project}&limit=20&offset=0 [search]"}},
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("read sequences\n got %q\nwant %q", seen, want)
	}
	// A claimed issue's read also lists the agents.
	p = rpPlan(t, o, []string{rpEvent(1, "comment.created", "LINUX-2", rpSession, time.Second, map[string]any{"id": "Q", "body": "b"})}, nil)
	if r := p.Actions[0]; r.Kind == rkRead && !slices.Contains(routes(r)[2], "GET /api/v1/agents [read]") {
		t.Fatalf("read of a session-claimed issue: %q", routes(r))
	}
}

func TestReplaySynthDoc(t *testing.T) {
	for _, n := range []int{1, 50, 4096, 33277} {
		d := replaySynthDoc(n, 9)
		if len(d) != n {
			t.Fatalf("synthetic document of %d bytes for %d", len(d), n)
		}
	}
	if len(fallbackCandidates(replaySynthDoc(4096, 9))) < 10 {
		t.Fatal("a synthetic document holds too few unique sentences for substitute edits")
	}
}

func TestReplayFlagsRefuseBadInputs(t *testing.T) {
	cases := map[string][]string{
		"-replay-inputs":   {},
		"-replay-rate":     {"-replay-inputs", "x", "-replay-rate", "0"},
		"-replay-reads":    {"-replay-inputs", "x", "-replay-reads", "-1"},
		"-replay-uploads":  {"-replay-inputs", "x", "-replay-uploads", "1.5"},
		"-replay-from":     {"-replay-inputs", "x", "-replay-from", "-1s"},
		"-replay-to":       {"-replay-inputs", "x", "-replay-from", "2m", "-replay-to", "1m"},
		"-replay-roster":   {"-replay-inputs", "x", "-replay-roster", "0"},
		"-replay-read-mix": {"-replay-inputs", "x", "-replay-read-mix", "read=1,write=2"},
	}
	for name, args := range cases {
		fs := flag.NewFlagSet("replay", flag.ContinueOnError)
		rf := registerReplayFlags(fs)
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		_, err := rf.options(1)
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s: error %v does not name it", name, err)
		}
	}
	m, err := parseReadMix("read=2,search=2")
	if err != nil || m.asMap()["read"] != 0.5 || m.pick(0.49) != rkRead || m.pick(0.51) != rkSearch {
		t.Fatalf("parseReadMix: %v %v", m, err)
	}
}

// TestReplaySendResolvesAndSkips sends a small plan to a fake Dispatch: a reply resolves its parent
// from the response that created it, a write naming an unknown object is skipped, and a failed
// creation makes its dependents skipped too.
func TestReplaySendResolvesAndSkips(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.RequestURI()+" "+string(body))
		mu.Unlock()
		switch {
		case strings.Contains(string(body), `"fail"`):
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"X"}`))
		case r.Method == "POST":
			_, _ = w.Write([]byte(`{"id":"local-new"}`))
		default:
			_, _ = w.Write([]byte(`{"last_seq":25}`))
		}
	}))
	defer srv.Close()
	p := rpPlan(t, rpOptions(), []string{
		rpEvent(1, "comment.created", "LINUX-1", rpSession, time.Second, map[string]any{"id": "N1", "body": "b"}),
		rpEvent(2, "comment.created", "LINUX-1", rpSession, 2*time.Second, map[string]any{"id": "N2", "body": "b", "reply_to": "N1"}),
		rpEvent(3, "comment.created", "LINUX-1", rpSession, 3*time.Second, map[string]any{"id": "N3", "body": "b", "reply_to": "UNKNOWN"}),
		rpEvent(4, "comment.created", "LINUX-1", rpSession, 4*time.Second, map[string]any{"id": "N4", "body": "fail"}),
		rpEvent(5, "comment.created", "LINUX-1", rpSession, 5*time.Second, map[string]any{"id": "N5", "body": "b", "reply_to": "N4"}),
	}, nil)
	rr := &replayRun{setup: &replaySetup{plan: p, opt: rpOptions()}, futures: map[replayRef]*replayFuture{},
		ids: map[[2]string]string{{idIssue, "LINUX-1"}: "LINUX-1"}, writes: map[string]*replayWriteCount{}, skipped: map[string]int{}}
	for _, a := range p.Actions {
		if rr.writes[a.Kind] == nil {
			rr.writes[a.Kind] = &replayWriteCount{}
		}
		rr.writes[a.Kind].Planned++
		for _, st := range a.Stages {
			for _, r := range st {
				for _, pr := range r.Produces {
					rr.futures[pr.Ref] = &replayFuture{done: make(chan struct{})}
				}
			}
		}
	}
	recs := newCollector[reqRec](16)
	var wg sync.WaitGroup
	for _, a := range p.Actions {
		wg.Go(func() { rr.do(context.Background(), srv.URL, "tok", a, time.Now(), recs) })
	}
	wg.Wait()
	reply := false
	for _, g := range got {
		if strings.Contains(g, `"reply_to":"local-new"`) {
			reply = true
		}
	}
	if !reply {
		t.Fatalf("the reply did not name its parent's local id: %d requests", len(got))
	}
	if rr.skipped[rkReply] != 2 || rr.writes[rkReply].Sent != 1 || rr.writes[rkPlain].Failed != 1 || rr.writes[rkPlain].Sent != 2 {
		t.Fatalf("skipped %v writes %+v %+v", rr.skipped, *rr.writes[rkReply], *rr.writes[rkPlain])
	}
	if n := len(recs.snapshot()); n != 3 {
		t.Fatalf("%d requests recorded, want 3", n)
	}
}

func TestReplayRenderArgs(t *testing.T) {
	is, c := replayRef{idIssue, "LINUX-1"}, replayRef{idComment, "C1"}
	local := map[replayRef]string{is: "LINUX-1", c: "l-c1"}
	state := &replayActionState{}
	state.observe([]byte(`{"last_seq":42,"ask":{"edited_at":"2026-01-01T00:00:00Z"}}`))
	if got := renderArg(replayArg{After: true}, local, state); got != "32" {
		t.Fatalf("after %s", got)
	}
	if got := renderArg(replayArg{Ref: &is, Sub: &c, URI: true}, local, state); got != "dispatch%3A%2F%2FLINUX-1%2Fcomment%2Fl-c1" {
		t.Fatalf("comment ref %s", got)
	}
	body := renderBody(map[string]any{"reply_to": c, "e": replayPrevEditedAt{}, "a": map[string]any{"artifact": is}}, local, state).(map[string]any)
	if body["reply_to"] != "l-c1" || string(body["e"].(json.RawMessage)) != `"2026-01-01T00:00:00Z"` || body["a"].(map[string]any)["artifact"] != "LINUX-1" {
		t.Fatalf("body %v", body)
	}
}
