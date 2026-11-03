package main

// loadgen loop-seed-write: sends the Verified Linux spec loop's planned seed (loopseed.go) to a
// Dispatch deployment through its HTTP API - the project, one issue per proposal, each proposal's
// part documents in order, the comment threads that anchor on their own quote rows in those
// documents with their replies and their resolves, and each live proposal's review pages and
// review index.
//
// Every created object is recorded in loadtest.loop_ids under the plan's identity the moment it
// exists, as replay-seed records a logged id in loadtest.replay_ids (replay_seed_ids.go), so a
// second run of the same plan against the same database creates nothing again: the map is read
// whole at the start and every write whose logical path it already holds is skipped. Every write
// also carries an idempotency key minted from the plan's identity and that same path, so a retry
// of a write whose answer was lost adds no second object either (api.go callRetryKeyed).
//
// A refusal is data, not a failure: a 413, a 400 CAP_EXCEEDED or any other non-2xx answer is
// counted by route and error code, whatever that object would have held is counted as skipped,
// and the run goes on. Only a failure of the id map itself stops a run, since without it a resume
// would write everything again.
//
// The writer never holds the whole seed. A worker takes one proposal at a time off a channel and
// expands one part at a time (partDocument, partThreads), so what a run holds is one part's
// markdown and one part's threads per worker, never the plan's 334,536 threads or its 564 MB of
// bodies.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/envoy/internal/contracts"
)

// loopIDsDDL is the id map a run writes as it goes and a resumed run reads: one row per created
// object, under the plan that minted it. The path names the object logically and carries its kind,
// so (plan, path) is the key and kind is the column the check counts rows by.
const loopIDsDDL = `create schema if not exists loadtest;
create table if not exists loadtest.loop_ids (
  plan text not null, kind text not null, path text not null, local_id text not null,
  primary key (plan, path));
create index if not exists loop_ids_kind on loadtest.loop_ids (plan, kind)`

// The kinds of loadtest.loop_ids, which are also the kinds loopseed.meta.json counts by, in the
// order the seed writes them.
const (
	loopKindProject = "project"
	loopKindIssue   = "issue"
	loopKindPart    = "part"
	loopKindThread  = "thread"
	loopKindReply   = "reply"
	loopKindResolve = "resolve"
	loopKindPage    = "page"
	loopKindIndex   = "index"
)

var loopKinds = []string{loopKindProject, loopKindIssue, loopKindPart, loopKindThread,
	loopKindReply, loopKindResolve, loopKindPage, loopKindIndex}

// loopSeedActor is the session every write of the seed names in its body; the run sends the
// deployment's agent bearer, as the rest of the harness does (api.go who).
const loopSeedActor = "ses-loop-seed"

// loopProjectKeyPattern is a Dispatch project key (internal/dispatch/store/migrations/
// 0001_init.up.sql: projects.key's check), so -project is refused before the first connection
// rather than by a 400 after 891 issues have nowhere to go.
var loopProjectKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)

// The logical path of each object of the seed: what names it in the id map and in its idempotency
// key, so one object has one path however many runs reach it. The widths are fixed so a path reads
// the same for every proposal; a wider index simply makes a longer path.
func loopIssuePath(proposal int) string { return fmt.Sprintf("%s.%04d", loopKindIssue, proposal) }

func loopPartPath(proposal, part int) string {
	return fmt.Sprintf("%s.%04d.%03d", loopKindPart, proposal, part)
}

func loopThreadPath(proposal, part, thread int) string {
	return fmt.Sprintf("%s.%04d.%03d.%06d", loopKindThread, proposal, part, thread)
}

func loopReplyPath(proposal, part, thread, reply int) string {
	return fmt.Sprintf("%s.%04d.%03d.%06d.%03d", loopKindReply, proposal, part, thread, reply)
}

func loopResolvePath(proposal, part, thread int) string {
	return fmt.Sprintf("%s.%04d.%03d.%06d", loopKindResolve, proposal, part, thread)
}

func loopPagePath(proposal, page int) string {
	return fmt.Sprintf("%s.%04d.%04d", loopKindPage, proposal, page)
}

func loopIndexPath(proposal int) string { return fmt.Sprintf("%s.%04d", loopKindIndex, proposal) }

// loopPlanID is the plan that minted a key or an id-map row: the checksum of the source file the
// plan was counted from and the seed every size was drawn under, which together settle every byte
// a run sends. Two plans that differ in either are different seeds, and neither resumes the other.
func loopPlanID(p *loopPlan) string {
	return p.SourceSHA256 + "." + strconv.FormatUint(p.Seed, 10)
}

// loopKey is one write's idempotency key: the plan's identity and the object's logical path, so
// every retry of that one write carries the same key and Dispatch runs it once
// (contracts.IdempotencyKeyHeader; api.go callRetryKeyed). The parts are joined with '.' and never
// '/', which IdempotencyKeyPattern does not hold (internal/contracts/generated.go:270).
func loopKey(planID, path string) string { return planID + "." + path }

// loopKeyFits refuses a plan whose longest key would be over IdempotencyKeyMax, which Dispatch
// answers 400 KEY_INVALID to: better to say so before the first write than to have every reply of
// the largest proposal refused. The longest key is the longest path, which is a reply's, at the
// plan's largest indexes.
func loopKeyFits(p *loopPlan, planID string) error {
	proposal, part, thread, reply := len(p.Proposals)-1, 0, 0, 0
	for i := range p.Proposals {
		pr := &p.Proposals[i]
		part = max(part, len(pr.Parts)-1)
		thread = max(thread, pr.IDs-1)
		reply = max(reply, pr.ReplyLines)
	}
	key := loopKey(planID, loopReplyPath(proposal, part, thread, reply))
	if len(key) > contracts.IdempotencyKeyMax {
		return fmt.Errorf("the plan's longest idempotency key is %d bytes, past the %d one may hold: %s",
			len(key), contracts.IdempotencyKeyMax, key)
	}
	return nil
}

// loopIDMap is the writer's resumable id map: the local id of every object of the plan this
// database already holds. loopIDStore is the one a run uses; a test maps in memory.
type loopIDMap interface {
	// get is the local id recorded for a logical path.
	get(path string) (string, bool)
	// put records one created object, durably before it is cached, so a run killed between the
	// two reads the row and not an empty map.
	put(ctx context.Context, kind, path, local string) error
	// held is what the map held when it was loaded, by kind: what a resume skips.
	held() map[string]int
}

// loopIDStore is loadtest.loop_ids for one plan, read whole at the start and cached, as the
// replay seed's idStore is (replay_seed_ids.go): 464,602 comment rows are 464,602 lookups, and one
// query at the start costs less than one round trip per object.
type loopIDStore struct {
	pool *pgxpool.Pool
	plan string
	// loaded is what the map held at load time, by kind, which never changes afterwards.
	loaded map[string]int
	mu     sync.Mutex
	m      map[string]string
}

// loadLoopIDStore creates the id map's table if it is not there and reads the plan's rows whole.
func loadLoopIDStore(ctx context.Context, pool *pgxpool.Pool, planID string) (*loopIDStore, error) {
	if _, err := pool.Exec(ctx, loopIDsDDL); err != nil {
		return nil, fmt.Errorf("create loadtest.loop_ids: %w", err)
	}
	s := &loopIDStore{pool: pool, plan: planID, loaded: map[string]int{}, m: map[string]string{}}
	rows, err := pool.Query(ctx, `select kind, path, local_id from loadtest.loop_ids where plan = $1`, planID)
	if err != nil {
		return nil, fmt.Errorf("read loadtest.loop_ids: %w", err)
	}
	defer rows.Close()
	var kind, path, local string
	for rows.Next() {
		if err := rows.Scan(&kind, &path, &local); err != nil {
			return nil, fmt.Errorf("read loadtest.loop_ids: %w", err)
		}
		s.m[path] = local
		s.loaded[kind]++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read loadtest.loop_ids: %w", err)
	}
	return s, nil
}

func (s *loopIDStore) get(path string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	local, ok := s.m[path]
	return local, ok
}

func (s *loopIDStore) put(ctx context.Context, kind, path, local string) error {
	_, err := s.pool.Exec(ctx, `insert into loadtest.loop_ids (plan, kind, path, local_id) values ($1, $2, $3, $4) on conflict do nothing`,
		s.plan, kind, path, local)
	if err != nil {
		return fmt.Errorf("record %s %s: %w", kind, path, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[path] = local
	return nil
}

func (s *loopIDStore) held() map[string]int { return maps.Clone(s.loaded) }

// --- what the seed sends -------------------------------------------------------------------

// loopIssueTitle names one proposal's issue. The proposal's own name is the loop's, and the index
// makes two proposals of one name two issues.
func loopIssueTitle(pr *loopProposal) string {
	return fmt.Sprintf("%s (proposal %04d)", pr.Name, pr.Index)
}

// loopIssueSpec is the issue's own primary document (spec.md, which every Dispatch issue is
// created with): one line naming what the proposal holds. The plan does not count it - its
// documents are the part documents, the review pages and the review indexes - so it stays one
// line, far under every cap.
func loopIssueSpec(pr *loopProposal) string {
	return fmt.Sprintf("# %s\n\nProposal %04d of the Verified Linux spec loop holds %d requirement ids over %d part documents.\n",
		pr.Name, pr.Index, pr.IDs, len(pr.Parts))
}

// loopPartName is a part document's file name; loopPageName a review page's and loopIndexName the
// review index's. Each is short, so no name and no reference key <issue>/<slug> it gives reaches
// the artifact name bound (POST /api/v1/issues/{key}/artifacts, 400 INVALID_ARTIFACT).
func loopPartName(part int) string { return fmt.Sprintf("part-%03d.md", part+1) }

func loopPageName(page int) string { return fmt.Sprintf("review-%04d.md", page+1) }

const loopIndexName = "review-index.md"

// loopFillText is the deterministic ASCII text of exactly n bytes that one planned size is
// written as: head, which names the object so no two texts are equal, then loopFiller until the
// size is met. A size shorter than head is head cut to it, as loopTextParagraph does. The text is
// ASCII, so its bytes are its UTF-16 units against loopBodyCap.
func loopFillText(head string, n int) string {
	if n <= len(head) {
		return head[:max(n, 0)]
	}
	var b strings.Builder
	b.Grow(n)
	b.WriteString(head)
	for b.Len() < n {
		b.WriteString(loopFiller[:min(len(loopFiller), n-b.Len())])
	}
	return b.String()
}

// loopCommentBody is the body of one comment row: the root of thread t when reply is 0, else its
// reply number reply, of exactly the bytes the plan drew for it (loopThread.Bodies).
func loopCommentBody(proposal, thread, reply int, n int64) string {
	return loopFillText(fmt.Sprintf("REQ-%04d-%06d.%03d ", proposal, thread, reply), int(n))
}

// loopPageText is one review page's markdown, of exactly the bytes the plan drew for it: a
// heading and one paragraph.
func loopPageText(proposal, page int, n int64) string {
	return loopFillText(fmt.Sprintf("# review page %04d of proposal %04d\n\n", page+1, proposal), int(n))
}

// loopIndexText is one live proposal's review index: the document loopReviewIndexBytes counts, its
// title and one line per review page.
func loopIndexText(proposal, pages int) string {
	var b strings.Builder
	b.Grow(int(loopReviewIndexBytes(proposal, pages)))
	fmt.Fprintf(&b, "# review index of proposal %04d", proposal)
	b.WriteString("\n\n")
	for page := range pages {
		b.WriteString(loopReviewIndexLine(proposal, page))
		b.WriteString("\n\n")
	}
	return b.String()
}

// loopDocBody is the body that creates a document on an issue, or adds a version to the one of
// that name (POST /api/v1/issues/{key}/artifacts {name, content, summary}).
func loopDocBody(name, content, summary string) map[string]any {
	return map[string]any{"name": name, "content": content, "summary": summary,
		"actor": sessionActor(loopSeedActor)}
}

// --- the writer ------------------------------------------------------------------------------

// loopWritten is what one kind of object cost: how many were created, how many bytes of markdown
// or body they carry, and how long their writes were in flight. The duration is summed over the
// workers, so it is about workers times the run's wall clock.
type loopWritten struct {
	Kind       string `json:"kind"`
	Objects    int    `json:"objects"`
	Bytes      int64  `json:"bytes"`
	DurationMS int64  `json:"duration_ms"`
}

// loopRefusal is one class of answer the seed could not use: a route, a status and the error code
// that answer named, with how many writes met it and the logical path of the first.
type loopRefusal struct {
	Route   string `json:"route"`
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Count   int    `json:"count"`
	Example string `json:"example"`
}

type loopRefusalKey struct {
	route, code string
	status      int
}

// loopMeta is loopseed.meta.json: what one run of loop-seed-write wrote, what it was refused, and
// what it found already written.
type loopMeta struct {
	Plan         string `json:"plan"`
	Source       string `json:"source"`
	SourceSHA256 string `json:"source_sha256"`
	Seed         uint64 `json:"seed"`
	PlanID       string `json:"plan_id"`
	// Endpoint and Postgres are redacted as every recorded endpoint of this package is
	// (guard.go redactURL, redactEndpoint).
	Endpoint  string `json:"endpoint"`
	Postgres  string `json:"postgres"`
	Project   string `json:"project"`
	Workers   int    `json:"workers"`
	StartedAt string `json:"started_at"`
	// DurationMS is the whole run's wall clock, and each Written row's that kind's.
	DurationMS int64 `json:"duration_ms"`
	// ResumedObjects is what the id map already held when the run started - what this run skipped
	// without a request - and Resumed the same by kind.
	ResumedObjects int            `json:"resumed_objects"`
	Resumed        map[string]int `json:"resumed_by_kind"`
	Written        []loopWritten  `json:"written"`
	// Skipped counts, by kind, the objects a refusal left unwritable: the threads of a part
	// document that was refused, the replies of a thread that was, and so on.
	Skipped  map[string]int `json:"skipped_by_kind"`
	Refusals []loopRefusal  `json:"refusals"`
}

// loopWriter sends one plan. Its counters are shared by every worker.
type loopWriter struct {
	plan    *loopPlan
	planID  string
	api     *api
	ids     loopIDMap
	project string
	lg      *log.Logger
	start   time.Time

	// pages and indexes are each live proposal's review records, taken from the plan's flat
	// slices once rather than searched for per proposal; the pages are a window into the plan's
	// own slice and copy nothing.
	pages   map[int][]loopReviewPage
	indexes map[int]loopReviewIndex

	mu       sync.Mutex
	written  map[string]*loopWritten
	skipped  map[string]int
	refusals map[loopRefusalKey]*loopRefusal
	done     int
}

func newLoopWriter(plan *loopPlan, a *api, ids loopIDMap, project string, lg *log.Logger) *loopWriter {
	w := &loopWriter{
		plan: plan, planID: loopPlanID(plan), api: a, ids: ids, project: project, lg: lg,
		start:   time.Now(),
		pages:   map[int][]loopReviewPage{},
		indexes: map[int]loopReviewIndex{},
		written: map[string]*loopWritten{}, skipped: map[string]int{},
		refusals: map[loopRefusalKey]*loopRefusal{},
	}
	for i := 0; i < len(plan.ReviewPages); {
		j := i
		for j < len(plan.ReviewPages) && plan.ReviewPages[j].Proposal == plan.ReviewPages[i].Proposal {
			j++
		}
		w.pages[plan.ReviewPages[i].Proposal] = plan.ReviewPages[i:j:j]
		i = j
	}
	for _, ix := range plan.ReviewIndexes {
		w.indexes[ix.Proposal] = ix
	}
	return w
}

// loopSend is one write: the object it creates, the route that creates it, and how its answer
// names the object's local id.
type loopSend struct {
	kind, path  string
	route       string
	method, url string
	body        any
	bytes       int64
	// accept lists the statuses other than 2xx this write takes as the object existing.
	accept  []int
	localID func(body []byte) string
}

// send writes one object the id map does not hold yet, under its idempotency key, and records it.
// It answers the object's local id and whether the object exists now; a refusal is counted by
// route and code and answers false, which leaves the caller to count what the object would have
// held as skipped. The error it returns is the run's context or the id map's: nothing else stops
// a run.
func (w *loopWriter) send(ctx context.Context, s loopSend) (string, bool, error) {
	if local, ok := w.ids.get(s.path); ok {
		return local, true, nil
	}
	res := w.api.callRetryKeyed(ctx, s.method, s.url, who{}, s.body, loopKey(w.planID, s.path))
	if ctx.Err() != nil {
		// The run is over (another worker's failure, or a signal): this answer says nothing
		// about the write, so it is not counted as a refusal.
		return "", false, ctx.Err()
	}
	if !res.ok() && !(res.err == nil && slices.Contains(s.accept, res.status)) {
		w.refuse(s, res.status, loopAnswerCode(res))
		return "", false, nil
	}
	local := s.localID(res.body)
	if local == "" {
		w.refuse(s, res.status, "NO_ID")
		return "", false, nil
	}
	if err := w.ids.put(ctx, s.kind, s.path, local); err != nil {
		return "", false, err
	}
	w.record(s.kind, s.bytes, time.Duration(res.end-res.start))
	return local, true, nil
}

// loopAnswerCode names an answer the seed could not use: the error code Dispatch's body names, the
// class of a transport error that outlived every retry, or the status alone.
func loopAnswerCode(res callResult) string {
	if res.err != nil {
		return errClass(res.err)
	}
	if code := res.errorCode(); code != "" {
		return code
	}
	return "NO_CODE"
}

func (w *loopWriter) record(kind string, bytes int64, in time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	k := w.written[kind]
	if k == nil {
		k = &loopWritten{Kind: kind}
		w.written[kind] = k
	}
	k.Objects++
	k.Bytes += bytes
	k.DurationMS += in.Milliseconds()
}

func (w *loopWriter) refuse(s loopSend, status int, code string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	key := loopRefusalKey{route: s.route, code: code, status: status}
	r := w.refusals[key]
	if r == nil {
		r = &loopRefusal{Route: s.route, Status: status, Code: code, Example: s.path}
		w.refusals[key] = r
	}
	r.Count++
}

func (w *loopWriter) skip(kind string, n int) {
	if n == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.skipped[kind] += n
}

// localOfKey reads the key an issue create answers with; every other route names what it created
// with an id, which idOf reads (replay_seed.go).
func localOfKey(body []byte) string {
	var created struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(body, &created)
	return created.Key
}

// run sends the whole plan: the project, then every proposal over workers goroutines. A proposal
// is one job, so a worker holds one proposal's parts and one part's threads at a time and the
// plan's own counts are all that is held for the rest.
func (w *loopWriter) run(ctx context.Context, workers int) error {
	if err := w.createProject(ctx); err != nil {
		return err
	}
	jobs := make(chan int)
	g, gctx := errgroup.WithContext(ctx)
	for range workers {
		g.Go(func() error {
			for i := range jobs {
				if err := w.proposal(gctx, i); err != nil {
					return err
				}
				w.progress()
			}
			return nil
		})
	}
	g.Go(func() error {
		defer close(jobs)
		for i := range w.plan.Proposals {
			select {
			case jobs <- i:
			case <-gctx.Done():
				return nil
			}
		}
		return nil
	})
	return g.Wait()
}

// loopProgressEvery is how many proposals a run logs its progress after.
const loopProgressEvery = 25

func (w *loopWriter) progress() {
	w.mu.Lock()
	w.done++
	done := w.done
	comments := 0
	if k := w.written[loopKindThread]; k != nil {
		comments += k.Objects
	}
	if k := w.written[loopKindReply]; k != nil {
		comments += k.Objects
	}
	w.mu.Unlock()
	if done%loopProgressEvery == 0 || done == len(w.plan.Proposals) {
		w.lg.Printf("%d of %d proposals, %d comment rows, in %s",
			done, len(w.plan.Proposals), comments, time.Since(w.start).Round(time.Second))
	}
}

// createProject creates the project every issue is created in. A project that is already there
// answers 409, which the existing seed takes the same way (seed.go:122-128): the project exists,
// which is all this write wanted.
func (w *loopWriter) createProject(ctx context.Context) error {
	_, _, err := w.send(ctx, loopSend{
		kind: loopKindProject, path: loopKindProject, route: "POST /api/v1/projects",
		method: "POST", url: "/api/v1/projects",
		body: map[string]any{"key": w.project, "name": "Verified Linux spec loop " + w.project,
			"actor": sessionActor(loopSeedActor)},
		accept:  []int{http.StatusConflict},
		localID: func([]byte) string { return w.project },
	})
	return err
}

// proposal writes one issue and everything on it.
func (w *loopWriter) proposal(ctx context.Context, index int) error {
	pr := &w.plan.Proposals[index]
	key, ok, err := w.send(ctx, loopSend{
		kind: loopKindIssue, path: loopIssuePath(index), route: "POST /api/v1/issues",
		method: "POST", url: "/api/v1/issues",
		body: map[string]any{"project": w.project, "title": loopIssueTitle(pr),
			"spec": loopIssueSpec(pr), "force": true, "actor": sessionActor(loopSeedActor)},
		localID: localOfKey,
	})
	if err != nil || !ok {
		if err == nil {
			w.skipProposal(index, pr)
		}
		return err
	}
	for part := range pr.Parts {
		if err := w.part(ctx, key, index, part); err != nil {
			return err
		}
	}
	if !pr.LiveInDriver {
		return nil
	}
	return w.review(ctx, key, index)
}

// skipProposal counts what an issue that was refused leaves unwritable. The resolves are the
// proposal's ids less its live ids: a live id's thread is left open and every other is resolved
// (loopStatuses), so they need no expansion of the proposal's threads to count.
func (w *loopWriter) skipProposal(index int, pr *loopProposal) {
	w.skip(loopKindPart, len(pr.Parts))
	w.skip(loopKindThread, pr.IDs)
	w.skip(loopKindReply, pr.ReplyLines)
	w.skip(loopKindResolve, pr.IDs-pr.LiveIDs)
	if pr.LiveInDriver {
		w.skip(loopKindPage, len(w.pages[index]))
		w.skip(loopKindIndex, 1)
	}
}

// part writes one part document and the threads that anchor in it. The threads are expanded
// before the document is sent so that a document the deployment refuses can still say exactly
// what it left unwritable, and dropped as soon as the part is done.
func (w *loopWriter) part(ctx context.Context, key string, proposal, part int) error {
	threads := w.plan.partThreads(proposal, part)
	text := w.plan.partDocument(proposal, part)
	doc, ok, err := w.send(ctx, loopSend{
		kind: loopKindPart, path: loopPartPath(proposal, part),
		route:  "POST /api/v1/issues/{key}/artifacts",
		method: "POST", url: "/api/v1/issues/" + key + "/artifacts",
		body: loopDocBody(loopPartName(part), text,
			fmt.Sprintf("part %03d of the Verified Linux spec loop's seed", part+1)),
		bytes:   int64(len(text)),
		localID: idOf,
	})
	if err != nil {
		return err
	}
	if !ok {
		w.skipThreads(threads)
		return nil
	}
	for _, th := range threads {
		if err := w.thread(ctx, key, doc, th); err != nil {
			return err
		}
	}
	return nil
}

// skipThreads counts the threads, replies and resolves a part document that was refused leaves
// unwritable.
func (w *loopWriter) skipThreads(threads []loopThread) {
	replies, resolves := 0, 0
	for _, th := range threads {
		replies += len(th.Bodies) - 1
		if !th.Open {
			resolves++
		}
	}
	w.skip(loopKindThread, len(threads))
	w.skip(loopKindReply, replies)
	w.skip(loopKindResolve, resolves)
}

// thread writes one requirement id's thread: its root, anchored on its own quote row in the part
// document it belongs to, then its replies, then its resolve when the id did not stay live.
func (w *loopWriter) thread(ctx context.Context, key, doc string, th loopThread) error {
	body := loopCommentBody(th.Proposal, th.Index, 0, th.Bodies[0])
	root, ok, err := w.send(ctx, loopSend{
		kind: loopKindThread, path: loopThreadPath(th.Proposal, th.Part, th.Index),
		route:  "POST /api/v1/issues/{key}/comments",
		method: "POST", url: "/api/v1/issues/" + key + "/comments",
		body: map[string]any{"body": body, "actor": sessionActor(loopSeedActor),
			"anchor": map[string]any{"artifact": doc, "quote": th.Quote}},
		bytes:   int64(len(body)),
		localID: idOf,
	})
	if err != nil {
		return err
	}
	if !ok {
		w.skip(loopKindReply, len(th.Bodies)-1)
		if !th.Open {
			w.skip(loopKindResolve, 1)
		}
		return nil
	}
	for i, size := range th.Bodies[1:] {
		reply := loopCommentBody(th.Proposal, th.Index, i+1, size)
		if _, _, err := w.send(ctx, loopSend{
			kind: loopKindReply, path: loopReplyPath(th.Proposal, th.Part, th.Index, i+1),
			route:  "POST /api/v1/issues/{key}/comments",
			method: "POST", url: "/api/v1/issues/" + key + "/comments",
			body: map[string]any{"body": reply, "reply_to": root,
				"actor": sessionActor(loopSeedActor)},
			bytes:   int64(len(reply)),
			localID: idOf,
		}); err != nil {
			return err
		}
	}
	if th.Open {
		return nil
	}
	_, _, err = w.send(ctx, loopSend{
		kind: loopKindResolve, path: loopResolvePath(th.Proposal, th.Part, th.Index),
		route:  "POST /api/v1/comments/{id}/resolve",
		method: "POST", url: "/api/v1/comments/" + root + "/resolve",
		body:    map[string]any{"actor": sessionActor(loopSeedActor)},
		localID: func([]byte) string { return root },
	})
	return err
}

// review writes one live proposal's review pages and then its review index, which names them.
func (w *loopWriter) review(ctx context.Context, key string, proposal int) error {
	for _, page := range w.pages[proposal] {
		text := loopPageText(proposal, page.Index, page.Bytes)
		if _, _, err := w.send(ctx, loopSend{
			kind: loopKindPage, path: loopPagePath(proposal, page.Index),
			route:  "POST /api/v1/issues/{key}/artifacts",
			method: "POST", url: "/api/v1/issues/" + key + "/artifacts",
			body: loopDocBody(loopPageName(page.Index), text,
				fmt.Sprintf("review page %04d of proposal %04d", page.Index+1, proposal)),
			bytes:   int64(len(text)),
			localID: idOf,
		}); err != nil {
			return err
		}
	}
	ix, ok := w.indexes[proposal]
	if !ok {
		return nil
	}
	text := loopIndexText(proposal, ix.Pages)
	_, _, err := w.send(ctx, loopSend{
		kind: loopKindIndex, path: loopIndexPath(proposal),
		route:  "POST /api/v1/issues/{key}/artifacts",
		method: "POST", url: "/api/v1/issues/" + key + "/artifacts",
		body: loopDocBody(loopIndexName, text,
			fmt.Sprintf("the review index of proposal %04d", proposal)),
		bytes:   int64(len(text)),
		localID: idOf,
	})
	return err
}

// meta is what the run wrote, for loopseed.meta.json. The kinds are in the order the seed writes
// them, so a reader sees issues before the threads that hang off them.
func (w *loopWriter) meta() loopMeta {
	w.mu.Lock()
	defer w.mu.Unlock()
	m := loopMeta{
		Source: w.plan.Source, SourceSHA256: w.plan.SourceSHA256, Seed: w.plan.Seed,
		PlanID: w.planID, Project: w.project,
		StartedAt:  w.start.UTC().Format(time.RFC3339),
		DurationMS: time.Since(w.start).Milliseconds(),
		Resumed:    w.ids.held(), Skipped: maps.Clone(w.skipped),
		// A run that created nothing and a run nothing refused each record an empty list rather
		// than a null, so every field of this file reads the same whatever the run met.
		Written:  []loopWritten{},
		Refusals: []loopRefusal{},
	}
	for _, n := range m.Resumed {
		m.ResumedObjects += n
	}
	for _, kind := range loopKinds {
		if k := w.written[kind]; k != nil {
			m.Written = append(m.Written, *k)
		}
	}
	for _, r := range w.refusals {
		m.Refusals = append(m.Refusals, *r)
	}
	slices.SortFunc(m.Refusals, func(a, b loopRefusal) int {
		if a.Route != b.Route {
			return strings.Compare(a.Route, b.Route)
		}
		if a.Status != b.Status {
			return a.Status - b.Status
		}
		return strings.Compare(a.Code, b.Code)
	})
	return m
}

// --- the subcommand --------------------------------------------------------------------------

// readLoopPlan reads a plan loop-seed-plan wrote and refuses one that names no source checksum or
// holds no proposal: both the id map and every idempotency key are minted from the checksum, and a
// plan without one would resume nothing.
func readLoopPlan(path string) (*loopPlan, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read plan: %w", err)
	}
	var p loopPlan
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("plan %s: %w", path, err)
	}
	if p.SourceSHA256 == "" {
		return nil, fmt.Errorf("plan %s names no source_sha256", path)
	}
	if len(p.Proposals) == 0 {
		return nil, fmt.Errorf("plan %s holds no proposal", path)
	}
	return &p, nil
}

// runLoopSeedWrite writes a whole planned seed to a Dispatch deployment, resumably. It does not
// check the stack (guard.go checkStack): the seed goes to a Dispatch, which need not run an Envoy
// listener, and its first write proves the endpoint answers.
func runLoopSeedWrite(args []string) error {
	fs := flag.NewFlagSet("loop-seed-write", flag.ExitOnError)
	var ef endpointFlags
	ef.register(fs)
	plan := fs.String("plan", "", "the plan loop-seed-plan wrote (loopseed.plan.json)")
	out := fs.String("out", "", "the run's record (default $LOADTEST_ROOT/loopseed.meta.json)")
	project := fs.String("project", "LOOP", "the project key every proposal's issue is created in")
	workers := fs.Int("workers", 8, "proposals written in parallel")
	_ = fs.Parse(args)

	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *plan == "" {
		return fmt.Errorf("-plan is required")
	}
	if *workers < 1 {
		return fmt.Errorf("-workers must be at least 1")
	}
	if !loopProjectKeyPattern.MatchString(*project) {
		return fmt.Errorf("-project %q is not a project key (%s)", *project, loopProjectKeyPattern)
	}
	if *out == "" {
		*out = filepath.Join(loadtestRoot(), "loopseed.meta.json")
	}
	p, err := readLoopPlan(*plan)
	if err != nil {
		return err
	}
	planID := loopPlanID(p)
	if err := loopKeyFits(p, planID); err != nil {
		return err
	}
	ep, err := ef.resolve()
	if err != nil {
		return err
	}
	if ep.pg == "" {
		return fmt.Errorf("-pg is required: the id map that makes a run resumable lives in it")
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, ep.pg)
	if err != nil {
		return err
	}
	defer pool.Close()
	ids, err := loadLoopIDStore(ctx, pool, planID)
	if err != nil {
		return err
	}
	lg := log.New(os.Stderr, "loadgen loop-seed-write: ", log.LstdFlags)
	w := newLoopWriter(p, newAPI(ep.dispatch, ep.token, *workers*2+2), ids, *project, lg)
	held := ids.held()
	resumed := 0
	for _, n := range held {
		resumed += n
	}
	lg.Printf("%d proposals, %d parts, %d comment rows to write to %s; the id map holds %d objects of this plan",
		p.Totals.Issues, p.Totals.Parts, p.Totals.CommentRows, redactURL(ep.dispatch), resumed)

	runErr := w.run(ctx, *workers)
	meta := w.meta()
	meta.Plan = *plan
	meta.Endpoint = redactURL(ep.dispatch)
	meta.Postgres = redactEndpoint("-pg", ep.pg)
	meta.Workers = *workers
	writeJSONFile(*out, meta, 0o644, lg)
	wrote := 0
	for _, k := range meta.Written {
		wrote += k.Objects
	}
	lg.Printf("wrote %d objects, skipped %d already written, %d refusals, in %s: %s",
		wrote, meta.ResumedObjects, len(meta.Refusals), time.Duration(meta.DurationMS)*time.Millisecond, *out)
	return runErr
}
