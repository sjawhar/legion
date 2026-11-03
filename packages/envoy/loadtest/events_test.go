package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestEventMixPresetsResolve(t *testing.T) {
	cases := map[string]map[string]float64{
		"production-2026-10-26": {"comment": 3, "anchored": 33, "reply": 35, "edit": 22, "upload": 7},
		"report-2026-10-27":     {"comment": 50, "anchored": 30, "edit": 20},
		"reply=1,upload=3":      {"reply": 1, "upload": 3},
	}
	for spec, want := range cases {
		mix, err := parseEventMix(spec)
		if err != nil {
			t.Fatalf("%s: %v", spec, err)
		}
		if !maps.Equal(mix.Weights, want) {
			t.Errorf("%s resolves to %v, want %v", spec, mix.Weights, want)
		}
	}
}

func TestEventMixRefusals(t *testing.T) {
	cases := map[string]string{
		"comment=50,bogus=10":   "bogus",
		"comment=50,edit=-5":    "edit",
		"comment=1,comment=2":   "comment",
		"anchored=lots":         "anchored",
		"production-2026-10-99": "production-2026-10-99",
	}
	for spec, kind := range cases {
		_, err := parseEventMix(spec)
		var mixErr *eventMixError
		if !errors.As(err, &mixErr) {
			t.Fatalf("%s: accepted or refused without naming an item: %v", spec, err)
		}
		if mixErr.Kind != kind {
			t.Errorf("%s: refusal names %q, want %q", spec, mixErr.Kind, kind)
		}
	}
	if _, err := parseEventMix("comment=0,edit=0"); err == nil {
		t.Error("a mix whose weights are all zero was accepted")
	}
}

func TestEventMixPicksInProportion(t *testing.T) {
	mix, err := parseEventMix("report-2026-10-27")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for i := range 1000 {
		counts[mix.pick(float64(i)/1000)]++
	}
	want := map[string]int{"comment": 500, "anchored": 300, "edit": 200}
	if !maps.Equal(counts, want) {
		t.Errorf("1,000 evenly spaced picks give %v, want %v", counts, want)
	}
}

func TestCurrentRevision(t *testing.T) {
	n, ok := currentRevision("# Spec\n\nRevision token r41\n\n| Revision token r9 |")
	if !ok || n != 41 {
		t.Fatalf("currentRevision = %d, %t; want the first token, 41", n, ok)
	}
	if _, ok := currentRevision("# Spec without a token"); ok {
		t.Fatal("currentRevision found a token in a spec without one")
	}
}

// --- the Verified Linux spec loop's three write kinds ------------------------------------------

// TestLoopEventMixPreset: the loop's preset resolves to the three weights of the issue's plan,
// and the mix draws them in proportion.
func TestLoopEventMixPreset(t *testing.T) {
	mix, err := parseEventMix("spec-loop-2026-11-03")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{kindVerdict: 106, kindRequirement: 48, kindStatus: 50}
	if !maps.Equal(mix.Weights, want) {
		t.Fatalf("the loop preset resolves to %v, want %v", mix.Weights, want)
	}
	counts := map[string]int{}
	const draws = 20_400
	for i := range draws {
		counts[mix.pick(float64(i)/draws)]++
	}
	for kind, w := range want {
		got, share := counts[kind], w/204
		if math.Abs(float64(got)/draws-share) > 0.001 {
			t.Errorf("%s took %d of %d picks, want %.3f of them", kind, got, draws, share)
		}
	}
}

// --- a Dispatch the write kinds can be sent to --------------------------------------------------

// loopWriteCall is one request a loop write sent the stub.
type loopWriteCall struct {
	method, path string
	body         map[string]any
}

// loopWriteStub is the Dispatch the loop's write kinds are sent to here. It answers the shapes
// the writer reads and records every request:
//
//	POST /api/v1/artifacts/{id}/edits    200 {"version": {"number": n}}
//	GET  /api/v1/artifacts/{id}/text     200 {"markdown": <the text it holds>}
//	POST /api/v1/issues/{key}/comments   201 {"id": <uuid>}
//	POST /api/v1/comments/{id}/resolve   200 {"id": <the comment in the path>}
//	POST /api/v1/issues/{key}/artifacts  201 {"artifact": {"id": <uuid>}}
//
// A document a test gives it (hold) is text the stub keeps: an edits request's replace ops are
// applied to it, as Dispatch applies them, and its text route serves what they left. A document
// no test gave it answers its edits 200 and changes nothing, which is what every test of the
// other kinds asks of it.
//
// refuse, when it is not nil, answers a status and an error code to refuse a request with, as
// Dispatch's writeError writes one.
type loopWriteStub struct {
	srv    *httptest.Server
	refuse func(path string, n int) (int, string)

	mu    sync.Mutex
	n     int
	calls []loopWriteCall
	docs  map[string]string
}

func newLoopWriteStub(t *testing.T) *loopWriteStub {
	t.Helper()
	s := &loopWriteStub{}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *loopWriteStub) serve(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	s.n++
	n, id := s.n, fmt.Sprintf("00000000-0000-4000-8000-%012d", s.n)
	s.calls = append(s.calls, loopWriteCall{method: r.Method, path: r.URL.Path, body: body})
	s.mu.Unlock()
	if s.refuse != nil {
		if status, code := s.refuse(r.URL.Path, n); status != 0 {
			s.writeError(w, status, code)
			return
		}
	}
	answer, status := map[string]any{"id": id}, http.StatusCreated
	switch {
	case strings.HasSuffix(r.URL.Path, "/text"):
		text, held := s.text(loopStubArtifact(r.URL.Path))
		if !held {
			s.writeError(w, http.StatusNotFound, "ARTIFACT_NOT_FOUND")
			return
		}
		answer, status = map[string]any{"markdown": text}, http.StatusOK
	case strings.HasSuffix(r.URL.Path, "/edits"):
		if refusal, code := s.edit(loopStubArtifact(r.URL.Path), body); refusal != 0 {
			s.writeError(w, refusal, code)
			return
		}
		answer, status = map[string]any{"version": map[string]any{"number": n}}, http.StatusOK
	case strings.HasSuffix(r.URL.Path, "/resolve"):
		answer, status = map[string]any{"id": strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/comments/"), "/resolve")}, http.StatusOK
	case strings.HasSuffix(r.URL.Path, "/artifacts"):
		answer = map[string]any{"artifact": map[string]any{"id": id}}
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(answer)
}

func (s *loopWriteStub) writeError(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": "refused by the stub", "code": code})
}

// loopStubArtifact is the document id of /api/v1/artifacts/{id}/<tail>.
func loopStubArtifact(path string) string {
	rest, ok := strings.CutPrefix(path, "/api/v1/artifacts/")
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, "/")
	return id
}

// hold gives the stub one document of its own, which it then serves and rewrites.
func (s *loopWriteStub) hold(artifact, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.docs == nil {
		s.docs = map[string]string{}
	}
	s.docs[artifact] = text
}

func (s *loopWriteStub) text(artifact string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	text, held := s.docs[artifact]
	return text, held
}

// edit applies one edits request to a document the stub holds, and answers the refusal Dispatch
// would answer instead: text the document does not hold is 404 TARGET_NOT_FOUND and text it holds
// twice 409 TARGET_AMBIGUOUS (internal/dispatch/api/server.go:851-869), neither writing anything.
// A document the stub does not hold is answered without being written, as it always was.
func (s *loopWriteStub) edit(artifact string, body map[string]any) (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	text, held := s.docs[artifact]
	if !held {
		return 0, ""
	}
	ops, _ := body["ops"].([]any)
	for _, raw := range ops {
		op, _ := raw.(map[string]any)
		find, _ := op["find"].(string)
		with, _ := op["with"].(string)
		if op["op"] != "replace" || find == "" {
			return http.StatusBadRequest, "INVALID_OP"
		}
		switch strings.Count(text, find) {
		case 1:
			text = strings.Replace(text, find, with, 1)
		case 0:
			return http.StatusNotFound, "TARGET_NOT_FOUND"
		default:
			return http.StatusConflict, "TARGET_AMBIGUOUS"
		}
	}
	s.docs[artifact] = text
	return 0, ""
}

func (s *loopWriteStub) sent() []loopWriteCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]loopWriteCall(nil), s.calls...)
}

// loopWriteTestWriter is a writer against that stub, on the test loop seed.
func loopWriteTestWriter(t *testing.T, stub *loopWriteStub, ls *loopSeed) *writer {
	t.Helper()
	return newWriter(newAPI(stub.srv.URL, "token", 4), ls.seedFile(), ls, 42, log.New(io.Discard, "", 0))
}

// loopRequestCount is how many requests of one class a writer recorded.
func loopRequestCount(t *testing.T, w *writer, class string) int {
	t.Helper()
	for _, c := range w.loopRecord().Requests {
		if c.Class == class {
			return c.Requests
		}
	}
	return 0
}

// loopVerdictOp is the one replace op of a verdict's request: the text it looked for and the text
// it wrote in its place.
func loopVerdictOp(t *testing.T, call loopWriteCall) (find, with string) {
	t.Helper()
	ops, _ := call.body["ops"].([]any)
	if len(ops) != 1 {
		t.Fatalf("a verdict sent %d ops, want one: %+v", len(ops), call.body)
	}
	op, _ := ops[0].(map[string]any)
	if op["op"] != "replace" {
		t.Fatalf("the verdict's op is %+v, want one replace of the page's text", op)
	}
	find, _ = op["find"].(string)
	with, _ = op["with"].(string)
	return find, with
}

// TestLoopVerdictWritesAReviewPageVersion: this run's first verdict on a page reads it for the
// generation it stands at, then sends one POST to the review page's edits route that replaces
// the page's paragraph with the next generation of it, and its summary carries the run's
// marker, so the artifact.version it commits is the write's own event.
func TestLoopVerdictWritesAReviewPageVersion(t *testing.T) {
	ls := loopTestSeed(t)
	page := ls.Proposals[0].ReviewPages[1]
	stub := newLoopWriteStub(t)
	stub.hold(page.ArtifactID, loopPageText(0, page.Index, 0, page.Bytes))
	w := loopWriteTestWriter(t, stub, ls)
	rec := w.do(context.Background(), eventPlan{marker: 3, kind: kindVerdict, issue: 0, page: 1})
	if rec.Status != http.StatusOK || rec.TransportErr != "" {
		t.Fatalf("the verdict answered %d %q", rec.Status, rec.TransportErr)
	}
	calls := stub.sent()
	if len(calls) != 2 {
		t.Fatalf("a first verdict sent %d requests, want the page's read and the write: %+v", len(calls), calls)
	}
	if calls[0].method != "GET" || calls[0].path != "/api/v1/artifacts/"+page.ArtifactID+"/text" {
		t.Fatalf("the first verdict's first request is %s %s, want the page's text", calls[0].method, calls[0].path)
	}
	if n := w.loopRecord().PageReads; n != 1 {
		t.Errorf("page reads = %d, want one", n)
	}
	if calls[1].method != "POST" || calls[1].path != "/api/v1/artifacts/"+page.ArtifactID+"/edits" {
		t.Fatalf("the verdict sent %s %s", calls[1].method, calls[1].path)
	}
	tag := markerTag(42, 3)
	summary, _ := calls[1].body["summary"].(string)
	if !strings.Contains(summary, tag) {
		t.Errorf("the verdict's summary %q carries no marker", summary)
	}
	find, with := loopVerdictOp(t, calls[1])
	if want := loopPageBody(0, page.Index, 0, page.Bytes); find != want {
		t.Errorf("the verdict looked for %d bytes, want the page's %d at generation 0", len(find), len(want))
	}
	if want := loopPageBody(0, page.Index, 1, page.Bytes); with != want {
		t.Errorf("the verdict wrote %d bytes, want the page's %d at generation 1", len(with), len(want))
	}
	if text, _ := stub.text(page.ArtifactID); text != loopPageText(0, page.Index, 1, page.Bytes) {
		t.Errorf("the page the verdict left is %d bytes at generation %v", len(text), generationOf(text))
	}
	if n := loopRequestCount(t, w, loopClassVerdict); n != 1 {
		t.Errorf("the verdict class counts %d requests, want 1", n)
	}
}

// generationOf is what loopPageGeneration read of a page, for a failure to name.
func generationOf(text string) any {
	if n, ok := loopPageGeneration(text); ok {
		return n
	}
	return "none"
}

// TestLoopVerdictCarriesThePageAndLeavesItTheSameSize: a verdict's request carries the page's own
// bytes - the text it finds and the text it writes are each the page's size - and the page it
// leaves is the size the plan drew, after ten verdicts as after one. The loop's verdict rewrites
// a review page; a step whose verdicts grew it would send a load the loop never sends and would
// walk the page towards the 1 MiB markdown cap.
func TestLoopVerdictCarriesThePageAndLeavesItTheSameSize(t *testing.T) {
	ls := loopTestSeed(t)
	page := ls.Proposals[0].ReviewPages[1]
	stub := newLoopWriteStub(t)
	stub.hold(page.ArtifactID, loopPageText(0, page.Index, 0, page.Bytes))
	w := loopWriteTestWriter(t, stub, ls)
	ctx := context.Background()
	var afterFirst int
	for i := range 10 {
		rec := w.do(ctx, eventPlan{marker: int64(i + 1), kind: kindVerdict, issue: 0, page: 1})
		if rec.Status != http.StatusOK {
			t.Fatalf("verdict %d answered %d %q", i, rec.Status, rec.TransportErr)
		}
		text, _ := stub.text(page.ArtifactID)
		if i == 0 {
			afterFirst = len(text)
		}
		if len(text) != afterFirst {
			t.Fatalf("the page is %d bytes after verdict %d and was %d after the first", len(text), i, afterFirst)
		}
	}
	if int64(afterFirst) != page.Bytes {
		t.Errorf("the page is %d bytes, want the %d the plan drew for it", afterFirst, page.Bytes)
	}
	// Each request carries the page twice, within a few percent of its planned size: the
	// heading the op leaves alone is the only part of the page it does not carry. The first
	// call is the page's read, which carries nothing.
	for i, call := range stub.sent() {
		if call.method != "POST" {
			continue
		}
		find, with := loopVerdictOp(t, call)
		for what, text := range map[string]string{"find": find, "with": with} {
			if off := math.Abs(float64(len(text))-float64(page.Bytes)) / float64(page.Bytes); off > 0.03 {
				t.Errorf("verdict %d sent a %s of %d bytes, want the page's %d within 3%%",
					i, what, len(text), page.Bytes)
			}
		}
	}
	if n := w.loopRecord().PageReads; n != 1 {
		t.Errorf("ten verdicts on one page read it %d times, want once", n)
	}
	if n := loopRequestCount(t, w, loopClassVerdict); n != 10 {
		t.Errorf("the verdict class counts %d requests, want 10", n)
	}
	if n := w.loopRecord().PageResyncs; n != 0 {
		t.Errorf("ten verdicts on a page this run alone wrote resynchronised %d times", n)
	}
}

// TestLoopVerdictResynchronisesAPageItsFindMissed: an edit whose find the page does not hold - a
// response this run lost, a page another run wrote - re-reads the page for the generation it
// stands at, so the next verdict on it finds its text; the step's summary counts that.
func TestLoopVerdictResynchronisesAPageItsFindMissed(t *testing.T) {
	ls := loopTestSeed(t)
	page := ls.Proposals[0].ReviewPages[1]
	stub := newLoopWriteStub(t)
	// The page stands five generations on from the seed's, where this run's verdict will not
	// look, and the first edit is refused outright.
	stub.hold(page.ArtifactID, loopPageText(0, page.Index, 5, page.Bytes))
	edits := 0
	stub.refuse = func(path string, _ int) (int, string) {
		if !strings.HasSuffix(path, "/edits") {
			return 0, ""
		}
		if edits++; edits == 1 {
			return http.StatusConflict, "PRECONDITION_FAILED"
		}
		return 0, ""
	}
	w := loopWriteTestWriter(t, stub, ls)
	ctx := context.Background()
	if rec := w.do(ctx, eventPlan{marker: 1, kind: kindVerdict, issue: 0, page: 1}); rec.Status != http.StatusConflict {
		t.Fatalf("the refused verdict answered %d %q", rec.Status, rec.TransportErr)
	}
	rec := w.do(ctx, eventPlan{marker: 2, kind: kindVerdict, issue: 0, page: 1})
	if rec.Status != http.StatusOK || rec.TransportErr != "" {
		t.Fatalf("the verdict after the resynchronisation answered %d %q", rec.Status, rec.TransportErr)
	}
	if text, _ := stub.text(page.ArtifactID); text != loopPageText(0, page.Index, 6, page.Bytes) {
		t.Errorf("the page is at generation %v of %d bytes, want 6 of %d",
			generationOf(text), len(text), page.Bytes)
	}
	if n := w.loopRecord().PageResyncs; n != 1 {
		t.Errorf("the loop record counts %d resynchronisations, want 1", n)
	}
	// The run reads the page once before its first verdict and once more after the refusal; the
	// re-read is the page's text route, and neither read is a verdict: the class counts the two
	// verdicts and no more.
	calls := stub.sent()
	if len(calls) != 4 || calls[0].method != "GET" || calls[2].method != "GET" ||
		calls[2].path != "/api/v1/artifacts/"+page.ArtifactID+"/text" {
		t.Fatalf("the writer sent %d requests, want the page's read, the refused edit, the re-read and the edit: %+v", len(calls), calls)
	}
	if n := loopRequestCount(t, w, loopClassVerdict); n != 2 {
		t.Errorf("the verdict class counts %d requests, want 2", n)
	}
}

// TestLoopVerdictsOnOnePageSerialise: two verdicts drawn on one page do not interleave. Each
// looks for the text the one before it wrote, which is why the page's lock is held across the
// request and not only across the draw.
func TestLoopVerdictsOnOnePageSerialise(t *testing.T) {
	ls := loopTestSeed(t)
	page := ls.Proposals[0].ReviewPages[1]
	stub := newLoopWriteStub(t)
	stub.hold(page.ArtifactID, loopPageText(0, page.Index, 0, page.Bytes))
	w := loopWriteTestWriter(t, stub, ls)
	ctx := context.Background()
	recs := make([]writeRec, 2)
	var wg sync.WaitGroup
	for i := range recs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recs[i] = w.do(ctx, eventPlan{marker: int64(i + 1), kind: kindVerdict, issue: 0, page: 1})
		}()
	}
	wg.Wait()
	for i, rec := range recs {
		if rec.Status != http.StatusOK {
			t.Fatalf("verdict %d answered %d %q", i, rec.Status, rec.TransportErr)
		}
	}
	calls := stub.sent()
	if len(calls) != 3 || calls[0].method != "GET" {
		t.Fatalf("two verdicts sent %d requests, want the page's read and two edits: %+v", len(calls), calls)
	}
	firstFind, firstWith := loopVerdictOp(t, calls[1])
	secondFind, secondWith := loopVerdictOp(t, calls[2])
	if firstFind != loopPageBody(0, page.Index, 0, page.Bytes) {
		t.Errorf("the first verdict looked for text that is no generation of the page")
	}
	if secondFind != firstWith {
		t.Error("the second verdict looked for text the first had not written: the two interleaved")
	}
	if text, _ := stub.text(page.ArtifactID); text != loopPageHead(0, page.Index)+secondWith {
		t.Errorf("the page the two verdicts left is %d bytes at generation %v", len(text), generationOf(text))
	}
	if n := w.loopRecord().PageResyncs; n != 0 {
		t.Errorf("two serialised verdicts resynchronised %d times", n)
	}
}

// TestLoopRequirementAnchorsOnASpareQuoteRow: a requirement thread is one anchored comment on the
// proposal's last part document, on a row no write has taken yet, of the size the plan drew.
func TestLoopRequirementAnchorsOnASpareQuoteRow(t *testing.T) {
	ls := loopTestSeed(t)
	stub := newLoopWriteStub(t)
	w := loopWriteTestWriter(t, stub, ls)
	ctx := context.Background()
	for i := range 2 {
		rec := w.do(ctx, eventPlan{marker: int64(i + 1), kind: kindRequirement, issue: 0, body: 1200})
		if rec.Status != http.StatusCreated {
			t.Fatalf("requirement %d answered %d %q", i, rec.Status, rec.TransportErr)
		}
	}
	calls := stub.sent()
	if len(calls) != 2 {
		t.Fatalf("two requirement writes sent %d requests, want two: %+v", len(calls), calls)
	}
	for i, call := range calls {
		if call.path != "/api/v1/issues/LOOP-1/comments" {
			t.Fatalf("requirement %d sent %s", i, call.path)
		}
		anchor, _ := call.body["anchor"].(map[string]any)
		if anchor["artifact"] != "part-a1" {
			t.Errorf("requirement %d anchors in %v, want the last part document part-a1", i, anchor["artifact"])
		}
		if want := loopSpareRow(0, i); anchor["quote"] != want {
			t.Errorf("requirement %d anchors on %q, want the unused row %q", i, anchor["quote"], want)
		}
		body, _ := call.body["body"].(string)
		if len(body) != 1200 || !strings.HasPrefix(body, markerTag(42, int64(i+1))) {
			t.Errorf("requirement %d sent a body of %d bytes starting %q, want 1,200 bytes starting with its marker",
				i, len(body), body[:min(len(body), 24)])
		}
	}
	if n := loopRequestCount(t, w, loopClassRequirement); n != 2 {
		t.Errorf("the requirement class counts %d requests, want 2", n)
	}
}

// TestLoopRequirementOpensANewPartOnCapExceeded: a part document whose margin is full answers 413,
// which is counted by its code; the next requirement write opens a part document of its own, with
// a fresh inventory of quote rows, rather than anchoring into the full one again.
func TestLoopRequirementOpensANewPartOnCapExceeded(t *testing.T) {
	ls := loopTestSeed(t)
	stub := newLoopWriteStub(t)
	stub.refuse = func(path string, n int) (int, string) {
		if n == 1 {
			return http.StatusRequestEntityTooLarge, "CAP_EXCEEDED"
		}
		return 0, ""
	}
	w := loopWriteTestWriter(t, stub, ls)
	ctx := context.Background()
	for i := range 2 {
		w.do(ctx, eventPlan{marker: int64(i + 1), kind: kindRequirement, issue: 0, body: 600})
	}
	calls := stub.sent()
	if len(calls) != 3 {
		t.Fatalf("a refused requirement and the next one sent %d requests, want three: %+v", len(calls), calls)
	}
	if calls[1].path != "/api/v1/issues/LOOP-1/artifacts" {
		t.Fatalf("the write after the refusal sent %s, want a new part document", calls[1].path)
	}
	name, _ := calls[1].body["name"].(string)
	content, _ := calls[1].body["content"].(string)
	if want := loopRunPartName(42, 0); name != want {
		t.Errorf("the new part document is named %q, want %q", name, want)
	}
	row := loopRunSpareRow(42, 0, 0, 0)
	if !strings.Contains(content, row) || strings.Count(content, "REQ-") != loopSpareQuotes {
		t.Errorf("the new part document carries %d quote rows, want %d of the shape %q",
			strings.Count(content, "REQ-"), loopSpareQuotes, row)
	}
	anchor, _ := calls[2].body["anchor"].(map[string]any)
	if anchor["artifact"] != "00000000-0000-4000-8000-000000000002" || anchor["quote"] != row {
		t.Fatalf("the requirement after the refusal anchors %v, want the new document's first row", anchor)
	}
	rec := w.loopRecord()
	if rec.CapExceeded != 1 || rec.NewParts != 1 {
		t.Errorf("the loop record counts %d refusals and %d new part documents, want 1 and 1", rec.CapExceeded, rec.NewParts)
	}
	if n := loopRequestCount(t, w, loopClassRequirementPart); n != 1 {
		t.Errorf("the requirement-part class counts %d requests, want 1", n)
	}
	var codes map[string]int
	for _, c := range rec.Requests {
		if c.Class == loopClassRequirement {
			codes = c.Codes
		}
	}
	if codes["413 CAP_EXCEEDED"] != 1 {
		t.Errorf("the requirement class counts the refusal by code as %v", codes)
	}
}

// TestLoopRequirementOpensANewPartWhenTheRowsRunOut: an inventory with no row left is replaced the
// same way, so a step never anchors two threads on one row.
func TestLoopRequirementOpensANewPartWhenTheRowsRunOut(t *testing.T) {
	ls := loopTestSeed(t)
	stub := newLoopWriteStub(t)
	w := loopWriteTestWriter(t, stub, ls)
	ctx := context.Background()
	// Proposal 1 carries one spare row: the second requirement write needs a document of its own.
	for i := range 2 {
		w.do(ctx, eventPlan{marker: int64(i + 1), kind: kindRequirement, issue: 1, body: 300})
	}
	calls := stub.sent()
	if len(calls) != 3 || calls[1].path != "/api/v1/issues/LOOP-2/artifacts" {
		t.Fatalf("an exhausted inventory sent %d requests (%v), want the second write to open a part document",
			len(calls), calls)
	}
	quotes := map[any]bool{}
	for _, call := range []loopWriteCall{calls[0], calls[2]} {
		anchor, _ := call.body["anchor"].(map[string]any)
		if quotes[anchor["quote"]] {
			t.Fatalf("two requirement threads anchor on %v", anchor["quote"])
		}
		quotes[anchor["quote"]] = true
	}
	if rec := w.loopRecord(); rec.NewParts != 1 || rec.CapExceeded != 0 {
		t.Errorf("the loop record counts %d new part documents and %d refusals, want 1 and 0", rec.NewParts, rec.CapExceeded)
	}
}

// TestLoopStatusRepliesAndResolves: a status change is a reply in an open thread and then its
// resolve, two requests, both counted; the reply alone is the form that reopens a resolved thread.
func TestLoopStatusRepliesAndResolves(t *testing.T) {
	ls := loopTestSeed(t)
	stub := newLoopWriteStub(t)
	w := loopWriteTestWriter(t, stub, ls)
	rec := w.do(context.Background(), eventPlan{marker: 5, kind: kindStatus, issue: 0, replyTo: "open-a1", body: 124})
	if rec.Status != http.StatusCreated {
		t.Fatalf("the status change answered %d %q", rec.Status, rec.TransportErr)
	}
	calls := stub.sent()
	if len(calls) != 2 {
		t.Fatalf("a status change sent %d requests, want the reply and the resolve: %+v", len(calls), calls)
	}
	if calls[0].path != "/api/v1/issues/LOOP-1/comments" || calls[0].body["reply_to"] != "open-a1" {
		t.Fatalf("the reply is %s %v", calls[0].path, calls[0].body)
	}
	body, _ := calls[0].body["body"].(string)
	if len(body) != 124 || !strings.HasPrefix(body, markerTag(42, 5)) {
		t.Errorf("the status note is %d bytes starting %q, want 124 starting with its marker", len(body), body[:min(len(body), 24)])
	}
	if calls[1].path != "/api/v1/comments/open-a1/resolve" {
		t.Fatalf("the second request is %s, want the thread's resolve", calls[1].path)
	}
	if reply, resolve := loopRequestCount(t, w, loopClassStatusReply), loopRequestCount(t, w, loopClassStatusResolve); reply != 1 || resolve != 1 {
		t.Errorf("the status classes count %d replies and %d resolves, want one of each", reply, resolve)
	}
	if n := w.loopRecord().Reopens; n != 0 {
		t.Errorf("a status change that resolved its thread counts %d reopens", n)
	}
}

// TestLoopStatusReopensAResolvedThread: the 1.2% form is the reply alone, in a thread Dispatch
// holds resolved, which reopens it in the reply's own transaction
// (internal/dispatch/api/comment_create.go:737-760); it sends no resolve.
func TestLoopStatusReopensAResolvedThread(t *testing.T) {
	ls := loopTestSeed(t)
	stub := newLoopWriteStub(t)
	w := loopWriteTestWriter(t, stub, ls)
	w.do(context.Background(), eventPlan{marker: 6, kind: kindStatus, issue: 0, replyTo: "done-a0", body: 124, reopen: true})
	calls := stub.sent()
	if len(calls) != 1 {
		t.Fatalf("a reopening status change sent %d requests, want the reply alone: %+v", len(calls), calls)
	}
	if calls[0].body["reply_to"] != "done-a0" {
		t.Fatalf("the reply answers %v, want the resolved thread", calls[0].body["reply_to"])
	}
	rec := w.loopRecord()
	if rec.Reopens != 1 {
		t.Errorf("the loop record counts %d reopens, want 1", rec.Reopens)
	}
	if n := loopRequestCount(t, w, loopClassStatusResolve); n != 0 {
		t.Errorf("a reopening status change sent %d resolves", n)
	}
}

// TestLoopStatusSendsNoResolveAfterARefusedReply: the resolve follows the reply, so a reply the
// deployment refused resolves nothing.
func TestLoopStatusSendsNoResolveAfterARefusedReply(t *testing.T) {
	ls := loopTestSeed(t)
	stub := newLoopWriteStub(t)
	stub.refuse = func(string, int) (int, string) { return http.StatusBadRequest, "INVALID_COMMENT" }
	w := loopWriteTestWriter(t, stub, ls)
	w.do(context.Background(), eventPlan{marker: 7, kind: kindStatus, issue: 0, replyTo: "open-a0", body: 124})
	if calls := stub.sent(); len(calls) != 1 {
		t.Fatalf("a refused status reply sent %d requests, want the reply alone: %+v", len(calls), calls)
	}
	if n := loopRequestCount(t, w, loopClassStatusResolve); n != 0 {
		t.Errorf("a refused status reply sent %d resolves", n)
	}
}

// TestLoopEventPlanDrawsItsShares: the planner draws the reopen form at its measured share and
// never lands a kind on a proposal that holds nothing for it.
func TestLoopEventPlanDrawsItsShares(t *testing.T) {
	ls := loopTestSeed(t)
	mix, err := parseEventMix("spec-loop-2026-11-03")
	if err != nil {
		t.Fatal(err)
	}
	cfg := runConfig{mix: mix, loop: ls, human: 0.1}
	sessions := []session{{id: sessionID(0), watched: []int{0}}}
	r := rand.New(rand.NewPCG(9, 1))
	kinds, reopens, statuses := map[string]int{}, 0, 0
	const draws = 100_000
	for i := range draws {
		p := planEvent(r, cfg, sessions, ls.seedFile(), int64(i+1))
		kinds[p.kind]++
		pr := &ls.Proposals[p.issue]
		switch p.kind {
		case kindVerdict:
			if p.page < 0 || p.page >= len(pr.ReviewPages) {
				t.Fatalf("a verdict drew page %d of proposal %s, which holds %d", p.page, pr.Name, len(pr.ReviewPages))
			}
		case kindRequirement:
			if pr.LastPart < 0 || p.body < 1 {
				t.Fatalf("a requirement drew proposal %s (last part %d) and %d bytes", pr.Name, pr.LastPart, p.body)
			}
		case kindStatus:
			statuses++
			roots := pr.OpenThreads
			if p.reopen {
				reopens++
				roots = pr.ResolvedThreads
			}
			if !slices.Contains(roots, p.replyTo) {
				t.Fatalf("a status change drew root %q, which proposal %s does not hold open=%t", p.replyTo, pr.Name, !p.reopen)
			}
		default:
			t.Fatalf("the loop mix drew %q", p.kind)
		}
	}
	if share := float64(reopens) / float64(statuses); math.Abs(share-loopReopenShare) > 0.003 {
		t.Errorf("%d of %d status changes reopen a thread (%.4f), want %g", reopens, statuses, share, loopReopenShare)
	}
	if got := float64(kinds[kindVerdict]) / draws; math.Abs(got-106.0/204) > 0.01 {
		t.Errorf("verdicts took %.3f of the writes, want %.3f", got, 106.0/204)
	}
}

// TestLoopDrawBytesHasItsMean: the sizes a kind's text is drawn at average the loop's measured
// mean, and none is past the cap one comment body holds.
func TestLoopDrawBytesHasItsMean(t *testing.T) {
	r := rand.New(rand.NewPCG(4, 2))
	for _, mean := range []float64{loopRequirementMeanBytes, loopStatusMeanBytes} {
		sum, worst := 0, 0
		const draws = 100_000
		for range draws {
			n := loopDrawBytes(r, mean)
			if n < 1 || n > loopBodyCap {
				t.Fatalf("a draw of %d bytes is outside 1..%d", n, loopBodyCap)
			}
			sum += n
			worst = max(worst, n)
		}
		if got := float64(sum) / draws; math.Abs(got-mean)/mean > 0.02 {
			t.Errorf("sizes drawn at a mean of %g average %.1f bytes (largest %d)", mean, got, worst)
		}
	}
}
