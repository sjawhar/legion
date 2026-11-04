package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/contracts"
)

// --- the tests' Dispatch and id map ----------------------------------------------------------

// loopStubWrite is one request loop-seed-write sent the stub: the route's template, its
// idempotency key, and its decoded body (nil under quiet).
type loopStubWrite struct {
	route, key string
	body       map[string]any
}

// loopStub is the Dispatch loop-seed-write writes to in these tests: it answers the shapes the
// writer reads and records every request. A full Dispatch is not this package's to start, so what
// it answers is written out here:
//
//	POST /api/v1/projects                     201 {"key": "<the key sent>"}
//	POST /api/v1/issues                       201 {"key": "LOOP-<n>", "primary_artifact_id": <uuid>}
//	POST /api/v1/issues/{key}/artifacts       201 {"artifact": {"id": <uuid>}}
//	POST /api/v1/issues/{key}/comments        201 {"id": <uuid>}
//	POST /api/v1/comments/{id}/resolve        200 {"id": <the comment id in the path>}
//
// and, where refuse chooses one, that refusal's status with Dispatch's error body
// ({"error": ..., "code": ...}, as writeError writes it).
type loopStub struct {
	srv *httptest.Server
	// refuse answers the status and code to refuse one request with, or 0 to answer it as above.
	refuse func(route, key string) (int, string)
	// quiet reads each body without decoding or keeping it, for a run of more bodies than a test
	// wants to hold.
	quiet bool

	mu      sync.Mutex
	n       int
	calls   map[string]int
	writes  []loopStubWrite
	docs    map[string]string
	comment map[string]string
	// made is the write that created each local id the stub answered with, which a test that
	// stores the seed in Postgres rebuilds its rows from.
	made map[string]loopStubWrite
	// bodyBytes is the bytes of every comment body sent, and docBytes every document's content.
	bodyBytes, docBytes int64
}

func newLoopStub(t *testing.T) *loopStub {
	t.Helper()
	s := &loopStub{calls: map[string]int{}, docs: map[string]string{}, comment: map[string]string{},
		made: map[string]loopStubWrite{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

// loopStubRoute is the route template of one request, the name loop-seed-write counts refusals by.
func loopStubRoute(r *http.Request) string {
	switch {
	case r.URL.Path == "/api/v1/projects":
		return "POST /api/v1/projects"
	case r.URL.Path == "/api/v1/issues":
		return "POST /api/v1/issues"
	case strings.HasSuffix(r.URL.Path, "/artifacts"):
		return "POST /api/v1/issues/{key}/artifacts"
	case strings.HasSuffix(r.URL.Path, "/comments"):
		return "POST /api/v1/issues/{key}/comments"
	case strings.HasSuffix(r.URL.Path, "/resolve"):
		return "POST /api/v1/comments/{id}/resolve"
	}
	return r.Method + " " + r.URL.Path
}

func (s *loopStub) serve(w http.ResponseWriter, r *http.Request) {
	route := loopStubRoute(r)
	key := r.Header.Get(contracts.IdempotencyKeyHeader)
	var body map[string]any
	if s.quiet {
		_, _ = io.Copy(io.Discard, r.Body)
	} else {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	if s.refuse != nil {
		if status, code := s.refuse(route, key); status != 0 {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "refused by the stub", "code": code})
			s.record(route, key, body, nil)
			return
		}
	}
	s.mu.Lock()
	s.n++
	// The number this answer is built from is taken under the lock: two concurrent workers must
	// not read one another's, and the issue's key and its artifact id are one answer's.
	n := s.n
	id := fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
	s.mu.Unlock()
	answer := map[string]any{}
	status := http.StatusCreated
	switch route {
	case "POST /api/v1/projects":
		answer["key"] = body["key"]
	case "POST /api/v1/issues":
		answer["key"] = fmt.Sprintf("LOOP-%d", n)
		answer["primary_artifact_id"] = id
	case "POST /api/v1/issues/{key}/artifacts":
		answer["artifact"] = map[string]any{"id": id}
	case "POST /api/v1/issues/{key}/comments":
		answer["id"] = id
	case "POST /api/v1/comments/{id}/resolve":
		status = http.StatusOK
		id = strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/comments/"), "/resolve")
		answer["id"] = id
	}
	s.record(route, key, body, answer)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(answer)
}

func (s *loopStub) record(route, key string, body, answer map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[route]++
	if s.quiet {
		return
	}
	write := loopStubWrite{route: route, key: key, body: body}
	s.writes = append(s.writes, write)
	// The local id the writer reads from this answer: an issue's and a project's is its key, a
	// document's the artifact it answers with, anything else's the top-level id.
	id, _ := answer["id"].(string)
	if key, ok := answer["key"].(string); ok {
		id = key
	}
	if a, ok := answer["artifact"].(map[string]any); ok {
		id, _ = a["id"].(string)
	}
	// A resolve creates nothing: it answers the comment's own id, whose creating write stands.
	if id != "" && route != "POST /api/v1/comments/{id}/resolve" {
		s.made[id] = write
	}
	if content, ok := body["content"].(string); ok {
		s.docBytes += int64(len(content))
		s.docs[id] = content
	}
	if text, ok := body["body"].(string); ok && route == "POST /api/v1/issues/{key}/comments" {
		s.bodyBytes += int64(len(text))
		s.comment[id] = text
	}
}

func (s *loopStub) count(route string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[route]
}

// keysOf is every idempotency key the stub was sent on route, in order.
func (s *loopStub) keysOf(route string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for _, w := range s.writes {
		if w.route == route {
			keys = append(keys, w.key)
		}
	}
	return keys
}

// loopMemIDs is a loopIDMap in memory: the id map of a test with no Postgres. reload stands for
// the whole-map read a new run makes, which is what settles what that run reports as resumed.
type loopMemIDs struct {
	mu     sync.Mutex
	m      map[string]string
	kinds  map[string]int
	loaded map[string]int
}

func newLoopMemIDs() *loopMemIDs {
	return &loopMemIDs{m: map[string]string{}, kinds: map[string]int{}, loaded: map[string]int{}}
}

func (s *loopMemIDs) get(path string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	local, ok := s.m[path]
	return local, ok
}

func (s *loopMemIDs) put(_ context.Context, kind, path, local string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[path]; !ok {
		s.kinds[kind]++
	}
	s.m[path] = local
	return nil
}

// all is the whole id map the run built, which buildLoopSeed joins a plan to.
func (s *loopMemIDs) all() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.m)
}

func (s *loopMemIDs) held() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for kind, n := range s.loaded {
		out[kind] = n
	}
	return out
}

func (s *loopMemIDs) reload() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loaded = map[string]int{}
	for kind, n := range s.kinds {
		s.loaded[kind] = n
	}
}

// loopTestPlanOf plans the rows given, as planLoopSeed does, and gives the live proposals the
// review pages pages names: a plan of a handful of proposals, which a stub can be written whole.
func loopTestPlanOf(t *testing.T, seed uint64, rows []loopRow, pages map[int]int) *loopPlan {
	t.Helper()
	p := &loopPlan{Source: "loopseed_write_test.go", SourceSHA256: strings.Repeat("ab", 32), Seed: seed}
	for i, row := range rows {
		pr, err := planLoopProposal(seed, i, row)
		if err != nil {
			t.Fatalf("plan proposal %d: %v", i, err)
		}
		p.Proposals = append(p.Proposals, pr)
	}
	for i, row := range rows {
		if !row.LiveInDriver {
			continue
		}
		for page := range pages[i] {
			p.ReviewPages = append(p.ReviewPages, loopReviewPage{Proposal: i, Index: page, Bytes: int64(2000 + 37*page)})
		}
		p.ReviewIndexes = append(p.ReviewIndexes, loopReviewIndex{
			Proposal: i, Pages: pages[i], Bytes: loopReviewIndexBytes(i, pages[i])})
	}
	p.Totals = loopPlanTotals(p)
	return p
}

// loopTestRows are the proposals the stub tests write: one live in the driver with replies and
// resolves, one with no reply, and one with no requirement id at all (336 of the loop's 891 rows
// have none).
func loopTestRows() []loopRow {
	return []loopRow{
		{Name: "alpha", IDs: 5, LiveIDs: 2, Met: 2, Rejected: 1, ReplyLines: 4, ThreadBytes: 24_000, DocBytes: 3_000, LiveInDriver: true},
		{Name: "beta", IDs: 3, Met: 3, ThreadBytes: 9_000, DocBytes: 1_200},
		{Name: "gamma", DocBytes: 700},
	}
}

func loopTestWriter(t *testing.T, p *loopPlan, stub *loopStub, ids loopIDMap) *loopWriter {
	t.Helper()
	return newLoopWriter(p, newAPI(stub.srv.URL, "token", 8), ids, "LOOP", log.New(io.Discard, "", 0))
}

// --- the keys --------------------------------------------------------------------------------

// TestLoopSeedWriteKeyShape: every key a run mints is a whole idempotency key Dispatch accepts
// (contracts.IdempotencyKeyPattern, IdempotencyKeyMax), names the plan that minted it, and is one
// object's own.
func TestLoopSeedWriteKeyShape(t *testing.T) {
	p := loopTestPlanOf(t, 7, loopTestRows(), map[int]int{0: 3})
	planID := loopPlanID(p)
	if want := p.SourceSHA256 + ".7"; planID != want {
		t.Fatalf("plan id %q, want %q: the source checksum and the seed", planID, want)
	}
	whole := regexp.MustCompile(contracts.IdempotencyKeyPattern)
	paths := []string{loopKindProject, loopIssuePath(3), loopPartPath(3, 2), loopThreadPath(3, 2, 41),
		loopReplyPath(3, 2, 41, 1), loopResolvePath(3, 2, 41), loopPagePath(3, 9), loopIndexPath(3)}
	seen := map[string]bool{}
	for _, path := range paths {
		key := loopKey(planID, path)
		if !strings.HasPrefix(key, planID+".") {
			t.Errorf("key %q does not name the plan", key)
		}
		if !whole.MatchString(key) {
			t.Errorf("key %q is not a whole idempotency key (%s)", key, contracts.IdempotencyKeyPattern)
		}
		if seen[key] {
			t.Errorf("key %q is minted for two objects", key)
		}
		seen[key] = true
	}
	// The one key shape that must not collide with another kind's: a resolve's path and its
	// thread's differ only in the kind they name.
	if loopResolvePath(3, 2, 41) == loopThreadPath(3, 2, 41) {
		t.Error("a thread and its resolve share a path, so one key would stand for both")
	}
	// The real plan's longest key, which is a reply's at its largest indexes, fits as well.
	if err := loopKeyFits(loopPlanned(t), loopPlanID(loopPlanned(t))); err != nil {
		t.Errorf("the embedded source file's plan mints a key Dispatch would refuse: %v", err)
	}
	if err := loopKeyFits(p, strings.Repeat("c", contracts.IdempotencyKeyMax)); err == nil {
		t.Error("a plan whose keys are too long was not refused")
	}
}

// TestLoopSeedWriteKeyStable: one logical object keeps one key. A thread the deployment refuses
// is written again by the next run of the same plan, under the key the first run sent, so a write
// Dispatch did apply and lost the answer of is not duplicated.
func TestLoopSeedWriteKeyStable(t *testing.T) {
	p := loopTestPlanOf(t, 1, loopTestRows(), map[int]int{0: 2})
	ids := newLoopMemIDs()
	refused := loopThreadPath(1, 0, 0)
	first := newLoopStub(t)
	first.refuse = func(route, key string) (int, string) {
		if key == loopKey(loopPlanID(p), refused) {
			return http.StatusRequestEntityTooLarge, "CAP_EXCEEDED"
		}
		return 0, ""
	}
	if err := loopTestWriter(t, p, first, ids).run(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	second := newLoopStub(t)
	ids.reload()
	w := loopTestWriter(t, p, second, ids)
	if err := w.run(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	// The second run writes the one object the first was refused, and nothing else.
	if got := len(second.keysOf("POST /api/v1/issues/{key}/comments")); got != 1 {
		t.Fatalf("the second run sent %d comments, want the one refused thread", got)
	}
	want := loopKey(loopPlanID(p), refused)
	if got := second.keysOf("POST /api/v1/issues/{key}/comments")[0]; got != want {
		t.Errorf("the retried thread's key is %q, want the first run's %q", got, want)
	}
	// Its replies are written under it, so the thread's whole record is there after the resume.
	if got := w.meta().Written; len(got) == 0 {
		t.Fatal("the second run recorded no write")
	}
}

// --- the whole small plan --------------------------------------------------------------------

// TestLoopSeedWriteWholePlan: a run sends every record the plan counted - the project, an issue
// per proposal, each part document, each thread anchored on its own quote row in that document
// with its replies, a resolve for every thread that did not stay live, and each live proposal's
// review pages and index - and nothing else.
func TestLoopSeedWriteWholePlan(t *testing.T) {
	p := loopTestPlanOf(t, 3, loopTestRows(), map[int]int{0: 4})
	stub := newLoopStub(t)
	ids := newLoopMemIDs()
	w := loopTestWriter(t, p, stub, ids)
	if err := w.run(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	meta := w.meta()
	written := map[string]loopWritten{}
	for _, k := range meta.Written {
		written[k.Kind] = k
	}
	resolves := 0
	for _, pr := range p.Proposals {
		resolves += pr.IDs - pr.LiveIDs
	}
	for _, c := range []struct {
		kind string
		want int
	}{
		{loopKindProject, 1},
		{loopKindIssue, len(p.Proposals)},
		{loopKindPart, p.Totals.Parts},
		{loopKindThread, p.Totals.Threads},
		{loopKindReply, p.Totals.Replies},
		{loopKindResolve, resolves},
		{loopKindPage, p.Totals.ReviewPages},
		{loopKindIndex, p.Totals.ReviewIndexes},
	} {
		if got := written[c.kind].Objects; got != c.want {
			t.Errorf("wrote %d %s, want %d", got, c.kind, c.want)
		}
	}
	if len(meta.Refusals) != 0 || len(meta.Skipped) != 0 {
		t.Errorf("a run nothing refused recorded %v and skipped %v", meta.Refusals, meta.Skipped)
	}
	// The bytes the stub was sent are the bytes the plan counted.
	if stub.bodyBytes != p.Totals.ThreadBytes {
		t.Errorf("the comment bodies hold %d bytes, want the plan's %d", stub.bodyBytes, p.Totals.ThreadBytes)
	}
	if got := written[loopKindThread].Bytes + written[loopKindReply].Bytes; got != p.Totals.ThreadBytes {
		t.Errorf("the run counted %d thread bytes, want %d", got, p.Totals.ThreadBytes)
	}
	var pageBytes int64
	for _, page := range p.ReviewPages {
		pageBytes += page.Bytes
	}
	if got := written[loopKindPage].Bytes; got != pageBytes {
		t.Errorf("the review pages hold %d bytes, want the plan's %d", got, pageBytes)
	}
	if got, want := written[loopKindPart].Bytes, p.Totals.DocumentBytes-pageBytes-p.ReviewIndexes[0].Bytes; got != want {
		t.Errorf("the part documents hold %d bytes, want the plan's %d", got, want)
	}
	if got, want := written[loopKindIndex].Bytes, p.ReviewIndexes[0].Bytes; got != want {
		t.Errorf("the review index holds %d bytes, want the plan's %d", got, want)
	}
	// Every thread anchors on a line of the document of the part it belongs to, and every reply
	// names its root.
	roots := map[string]bool{}
	for _, write := range stub.writes {
		if write.route != "POST /api/v1/issues/{key}/comments" {
			continue
		}
		anchor, _ := write.body["anchor"].(map[string]any)
		reply, _ := write.body["reply_to"].(string)
		switch {
		case anchor != nil:
			doc, _ := anchor["artifact"].(string)
			quote, _ := anchor["quote"].(string)
			if !strings.Contains(stub.docs[doc], quote+"\n") {
				t.Errorf("thread anchored on %q, which is not a line of document %s", quote, doc)
			}
		case reply != "":
			if _, ok := stub.comment[reply]; !ok {
				t.Errorf("a reply names %q, which is no comment this run created", reply)
			}
		default:
			t.Error("a comment was written with neither an anchor nor a reply_to")
		}
		if anchor != nil {
			roots[write.body["body"].(string)[:16]] = true
		}
	}
	if len(roots) != p.Totals.Threads {
		t.Errorf("%d distinct thread roots, want %d", len(roots), p.Totals.Threads)
	}
}

// --- the review page's text --------------------------------------------------------------------

// TestLoopPageTextIsOneGenerator: a review page's text is a pure function of the page, the size
// the plan drew for it and its generation, so two draws of one generation are one text and two
// generations are not. Each page is its heading and the paragraph under it, which is the part a
// verdict replaces, and each names the generation it was drawn at, which is how a run that lost
// an answer reads back where the page stands.
func TestLoopPageTextIsOneGenerator(t *testing.T) {
	const proposal, page, size = 4, 2, loopPageMean
	first := loopPageText(proposal, page, 0, size)
	if len(first) != size || loopPageText(proposal, page, 0, size) != first {
		t.Fatalf("one page at one generation draws %d bytes, want the planned %d twice over", len(first), size)
	}
	for gen := range 3 {
		text := loopPageText(proposal, page, gen, size)
		if len(text) != size {
			t.Errorf("generation %d of the page is %d bytes, want %d", gen, len(text), size)
		}
		if n, ok := loopPageGeneration(text); !ok || n != gen {
			t.Errorf("generation %d of the page reads back as %v", gen, generationOf(text))
		}
		head := loopPageHead(proposal, page)
		if !strings.HasPrefix(text, head) || text[len(head):] != loopPageBody(proposal, page, gen, size) {
			t.Errorf("generation %d of the page is not its heading and the paragraph a verdict replaces", gen)
		}
		if gen > 0 && text == first {
			t.Errorf("generation %d of the page is generation 0 over again", gen)
		}
	}
	if loopPageText(proposal, page+1, 0, size) == first {
		t.Error("two pages of one proposal draw one text")
	}
	if loopPageText(proposal+1, page, 0, size) == first {
		t.Error("one page of two proposals draws one text")
	}
}

// TestLoopVerdictFindsThePageTheSeedWriterWrote: the seed writer and a run draw a page's text from
// the one generator, so a verdict reproduces generation 0 of a page it has never read. The proof
// is from both call sites: the seed writer writes the page here, and the run's verdict is sent
// against that very text, which the stub rewrites only for a find the document holds.
func TestLoopVerdictFindsThePageTheSeedWriterWrote(t *testing.T) {
	p := loopTestPlanOf(t, 3, loopTestRows(), map[int]int{0: 4})
	ctx := context.Background()
	seed := newLoopStub(t)
	ids := newLoopMemIDs()
	if err := loopTestWriter(t, p, seed, ids).run(ctx, 2); err != nil {
		t.Fatal(err)
	}
	ls, err := buildLoopSeed("loopseed.plan.json", p, ids.all(), loopThreadSample)
	if err != nil {
		t.Fatal(err)
	}
	if dropped := ls.prepare(); dropped != 0 {
		t.Fatalf("prepare dropped %d proposals of a seed written whole", dropped)
	}
	rec := ls.Proposals[0].ReviewPages[1]
	written := seed.docs[rec.ArtifactID]
	if want := loopPageText(0, rec.Index, 0, rec.Bytes); written != want {
		t.Fatalf("the seed wrote %d bytes of review page %d, and the run draws %d at generation 0",
			len(written), rec.Index, len(want))
	}
	run := newLoopWriteStub(t)
	run.hold(rec.ArtifactID, written)
	w := loopWriteTestWriter(t, run, ls)
	got := w.do(ctx, eventPlan{marker: 1, kind: kindVerdict, issue: 0, page: 1})
	if got.Status != http.StatusOK || got.TransportErr != "" {
		t.Fatalf("a verdict on the page the seed wrote answered %d %q", got.Status, got.TransportErr)
	}
	if text, _ := run.text(rec.ArtifactID); text != loopPageText(0, rec.Index, 1, rec.Bytes) {
		t.Errorf("the verdict left %d bytes at generation %v, want %d at generation 1",
			len(text), generationOf(text), rec.Bytes)
	}
	if n := w.loopRecord().PageResyncs; n != 0 {
		t.Errorf("a verdict on a page it drew right resynchronised %d times", n)
	}
}

// --- the refusals ------------------------------------------------------------------------------

// TestLoopSeedWriteRefusals: a 413 and a 400 CAP_EXCEEDED are counted by route and error code, the
// objects they leave unwritable are counted as skipped, and the run finishes and writes the rest.
func TestLoopSeedWriteRefusals(t *testing.T) {
	p := loopTestPlanOf(t, 5, loopTestRows(), map[int]int{0: 2})
	planID := loopPlanID(p)
	part := loopKey(planID, loopPartPath(1, 0))
	thread := loopKey(planID, loopThreadPath(0, 0, 0))
	stub := newLoopStub(t)
	stub.refuse = func(route, key string) (int, string) {
		switch key {
		case part:
			return http.StatusRequestEntityTooLarge, "CAP_EXCEEDED"
		case thread:
			return http.StatusBadRequest, "CAP_EXCEEDED"
		}
		return 0, ""
	}
	w := loopTestWriter(t, p, stub, newLoopMemIDs())
	if err := w.run(context.Background(), 2); err != nil {
		t.Fatalf("a refused write stopped the run: %v", err)
	}
	meta := w.meta()
	if len(meta.Refusals) != 2 {
		t.Fatalf("refusals %+v, want one per route", meta.Refusals)
	}
	want := []loopRefusal{
		{Route: "POST /api/v1/issues/{key}/artifacts", Status: 413, Code: "CAP_EXCEEDED", Count: 1, Example: loopPartPath(1, 0)},
		{Route: "POST /api/v1/issues/{key}/comments", Status: 400, Code: "CAP_EXCEEDED", Count: 1, Example: loopThreadPath(0, 0, 0)},
	}
	for i, r := range meta.Refusals {
		if r != want[i] {
			t.Errorf("refusal %d is %+v, want %+v", i, r, want[i])
		}
	}
	// The refused part document leaves its threads, their replies and their resolves unwritable;
	// the refused thread leaves its own replies and, since it did not stay live, its resolve.
	pr := &p.Proposals[1]
	refusedThread := p.partThreads(0, 0)[0]
	wantSkipped := map[string]int{
		loopKindThread:  pr.Parts[0].Threads,
		loopKindReply:   pr.Parts[0].Replies + len(refusedThread.Bodies) - 1,
		loopKindResolve: pr.IDs - pr.LiveIDs,
	}
	if refusedThread.Open {
		t.Fatal("the plan's first thread stayed live; this test needs a resolved one to count")
	}
	wantSkipped[loopKindResolve]++
	for kind, n := range wantSkipped {
		if meta.Skipped[kind] != n {
			t.Errorf("skipped %d %s, want %d (all: %v)", meta.Skipped[kind], kind, n, meta.Skipped)
		}
	}
	// Everything else was written: the other proposals' parts and threads.
	written := map[string]loopWritten{}
	for _, k := range meta.Written {
		written[k.Kind] = k
	}
	if got, want := written[loopKindPart].Objects, p.Totals.Parts-1; got != want {
		t.Errorf("wrote %d part documents, want %d", got, want)
	}
	if got, want := written[loopKindThread].Objects, p.Totals.Threads-pr.Parts[0].Threads-1; got != want {
		t.Errorf("wrote %d threads, want %d", got, want)
	}
	if written[loopKindIndex].Objects != 1 {
		t.Error("the live proposal's review index was not written")
	}
}

// TestLoopSeedWriteRefusedIssue: an issue nothing could create leaves its whole proposal
// unwritable, counted by kind, and the run still writes every other proposal.
func TestLoopSeedWriteRefusedIssue(t *testing.T) {
	p := loopTestPlanOf(t, 9, loopTestRows(), map[int]int{0: 3})
	issue := loopKey(loopPlanID(p), loopIssuePath(0))
	stub := newLoopStub(t)
	stub.refuse = func(route, key string) (int, string) {
		if key == issue {
			return http.StatusRequestEntityTooLarge, "CAP_EXCEEDED"
		}
		return 0, ""
	}
	w := loopTestWriter(t, p, stub, newLoopMemIDs())
	if err := w.run(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	meta := w.meta()
	pr := &p.Proposals[0]
	want := map[string]int{
		loopKindPart:    len(pr.Parts),
		loopKindThread:  pr.IDs,
		loopKindReply:   pr.ReplyLines,
		loopKindResolve: pr.IDs - pr.LiveIDs,
		loopKindPage:    3,
		loopKindIndex:   1,
	}
	for kind, n := range want {
		if meta.Skipped[kind] != n {
			t.Errorf("skipped %d %s, want %d (all: %v)", meta.Skipped[kind], kind, n, meta.Skipped)
		}
	}
	if stub.count("POST /api/v1/issues") != len(p.Proposals) {
		t.Error("the run stopped at the refused issue")
	}
	if got := stub.count("POST /api/v1/comments/{id}/resolve"); got == 0 {
		t.Error("no other proposal's threads were resolved")
	}
}

// TestLoopSeedWriteNoID: a 2xx answer that names no id leaves the object unrecorded - a resume
// would write it again, and nothing may hang off an id the run does not have - and is counted
// like any other answer the run cannot use.
func TestLoopSeedWriteNoID(t *testing.T) {
	p := loopTestPlanOf(t, 2, loopTestRows()[:1], map[int]int{0: 1})
	stub := newLoopStub(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/artifacts") {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{}`)
			return
		}
		stub.serve(w, r)
	}))
	defer srv.Close()
	w := newLoopWriter(p, newAPI(srv.URL, "token", 4), newLoopMemIDs(), "LOOP", log.New(io.Discard, "", 0))
	if err := w.run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	meta := w.meta()
	if len(meta.Refusals) != 1 || meta.Refusals[0].Code != "NO_ID" || meta.Refusals[0].Status != 201 {
		t.Fatalf("refusals %+v, want one 201 NO_ID", meta.Refusals)
	}
	if meta.Skipped[loopKindThread] != p.Totals.Threads {
		t.Errorf("skipped %d threads, want all %d", meta.Skipped[loopKindThread], p.Totals.Threads)
	}
}

// --- the resume --------------------------------------------------------------------------------

// TestLoopSeedWriteResumes: a second run of the same plan against the same id map sends no
// request at all and reports what it skipped.
func TestLoopSeedWriteResumes(t *testing.T) {
	p := loopTestPlanOf(t, 11, loopTestRows(), map[int]int{0: 2})
	ids := newLoopMemIDs()
	first := newLoopStub(t)
	w := loopTestWriter(t, p, first, ids)
	if err := w.run(context.Background(), 4); err != nil {
		t.Fatal(err)
	}
	objects := 0
	for _, k := range w.meta().Written {
		objects += k.Objects
	}
	if objects == 0 {
		t.Fatal("the first run wrote nothing")
	}

	ids.reload()
	second := newLoopStub(t)
	w2 := loopTestWriter(t, p, second, ids)
	if err := w2.run(context.Background(), 4); err != nil {
		t.Fatal(err)
	}
	meta := w2.meta()
	if len(meta.Written) != 0 {
		t.Errorf("the second run created %+v, want nothing", meta.Written)
	}
	if len(second.writes) != 0 {
		t.Errorf("the second run sent %d requests, want none", len(second.writes))
	}
	if meta.ResumedObjects != objects {
		t.Errorf("the second run reports %d objects already written, want the first run's %d",
			meta.ResumedObjects, objects)
	}
	if meta.Resumed[loopKindThread] != p.Totals.Threads {
		t.Errorf("resumed threads %d, want %d", meta.Resumed[loopKindThread], p.Totals.Threads)
	}
}

// --- a live Postgres -------------------------------------------------------------------------

// loopTestPG is the Postgres a test that needs one writes to: $LEGION_TEST_PG_DSN, the pod's own
// test server. A test skips without it, since this package's checks run where there is none.
func loopTestPG(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LEGION_TEST_PG_DSN must be set to run the loop seed's Postgres tests")
	}
	return dsn
}

// loopTestDB opens a database of this test's own on that server and drops it afterwards, so a
// test's loadtest.loop_ids rows and its dispatch-shaped rows are nobody else's.
func loopTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, loopTestPG(t))
	if err != nil {
		t.Fatalf("open the test server: %v", err)
	}
	defer admin.Close()
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	name := "loopseed_test_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(ctx, "create database "+name); err != nil {
		t.Fatalf("create the test database: %v", err)
	}
	u, err := url.Parse(loopTestPG(t))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropper, err := pgxpool.New(context.Background(), loopTestPG(t))
		if err != nil {
			t.Errorf("open the test server to drop %s: %v", name, err)
			return
		}
		defer dropper.Close()
		if _, err := dropper.Exec(context.Background(), "drop database "+name+" with (force)"); err != nil {
			t.Errorf("drop the test database: %v", err)
		}
	})
	return pool
}

// TestLoopSeedWriteLiveResume: the whole resume, over a real loadtest.loop_ids. The first run
// creates the plan's objects and records each in the table; a second run, whose id map is read
// from that table and not from the first run's memory, sends no request and reports that it
// created nothing.
func TestLoopSeedWriteLiveResume(t *testing.T) {
	pool := loopTestDB(t)
	ctx := context.Background()
	p := loopTestPlanOf(t, 13, loopTestRows(), map[int]int{0: 3})
	planID := loopPlanID(p)

	ids, err := loadLoopIDStore(ctx, pool, planID)
	if err != nil {
		t.Fatalf("load the id map: %v", err)
	}
	if n := len(ids.held()); n != 0 {
		t.Fatalf("a fresh database already holds %d kinds of this plan's objects", n)
	}
	first := newLoopStub(t)
	w := loopTestWriter(t, p, first, ids)
	if err := w.run(ctx, 4); err != nil {
		t.Fatal(err)
	}
	meta := w.meta()
	objects := 0
	for _, k := range meta.Written {
		objects += k.Objects
	}
	var rows int
	if err := pool.QueryRow(ctx, `select count(*) from loadtest.loop_ids where plan = $1`, planID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != objects {
		t.Fatalf("the id map holds %d rows for %d objects created", rows, objects)
	}

	resumed, err := loadLoopIDStore(ctx, pool, planID)
	if err != nil {
		t.Fatal(err)
	}
	second := newLoopStub(t)
	w2 := loopTestWriter(t, p, second, resumed)
	if err := w2.run(ctx, 4); err != nil {
		t.Fatal(err)
	}
	meta2 := w2.meta()
	if len(meta2.Written) != 0 || len(second.writes) != 0 {
		t.Fatalf("the resumed run created %+v and sent %d requests, want neither", meta2.Written, len(second.writes))
	}
	if meta2.ResumedObjects != objects {
		t.Errorf("the resumed run reports %d objects already written, want %d", meta2.ResumedObjects, objects)
	}
	after := 0
	if err := pool.QueryRow(ctx, `select count(*) from loadtest.loop_ids where plan = $1`, planID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != rows {
		t.Errorf("the id map holds %d rows after the resume, want the %d it held", after, rows)
	}
	t.Logf("first run created %d objects (%v); resumed run created nothing and skipped %d",
		objects, meta.Resumed, meta2.ResumedObjects)

	// A plan of another seed is another seed: its rows are its own, and it resumes nothing.
	other := loopTestPlanOf(t, 14, loopTestRows(), map[int]int{0: 3})
	otherIDs, err := loadLoopIDStore(ctx, pool, loopPlanID(other))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(otherIDs.held()); n != 0 {
		t.Errorf("a plan of another seed resumed %d kinds of this one's objects", n)
	}
}
