package main

// loadgen replay-seed: recreates a replay bundle's state (replay_bundle.go) on a load-test stack
// through Dispatch's API, as replay_seed_plan.go decides it. Every logged id is mapped to the local
// one in loadtest.replay_ids the moment the local object exists, so a killed seed resumes where it
// stopped. It logs keys, ids, statuses, Dispatch's error codes, sizes and counts, never a text.

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"log"
	"maps"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"
)

// replayIDsDDL is the logged-to-local id map a seed writes as it goes and a resumed seed reads.
const replayIDsDDL = `create schema if not exists loadtest;
create table if not exists loadtest.replay_ids (
  kind text not null, logged_id text not null, local_id text not null,
  primary key (kind, logged_id));
create table if not exists loadtest.replay_anchors (
  kind text not null, logged_id text not null, local_id text not null, logged_doc text not null,
  branch text not null, block_id text not null, span_start int not null, span_end int not null,
  primary key (kind, logged_id));
alter table loadtest.replay_anchors add column if not exists quote_at int not null default -1`

// quoteAt is the byte offset in the text at At of the anchor's quote when the choice pinned an
// occurrence of a repeated quote, -1 otherwise (the check then needs the quote to be unique).
func quoteAt(c anchorChoice) int {
	if c.Occurrence != nil {
		return c.QuoteAt
	}
	return -1
}

// replay_ids kinds.
const (
	idIssue           = "issue"
	idArtifact        = "artifact"
	idComment         = "comment"
	idAsk             = "ask"
	idResolve         = "resolve"
	idAskResolve      = "ask-resolve"
	idAnswer          = "answer"
	idFallbackComment = "fallback-comment"
	idFallbackAsk     = "fallback-ask"
)

func runReplaySeed(args []string) error {
	fs := flag.NewFlagSet("replay-seed", flag.ContinueOnError)
	var ef endpointFlags
	ef.register(fs)
	bundleDir := fs.String("bundle", "", "replay bundle directory")
	db := fs.String("db", "", "database name, overriding the Postgres URL's")
	keysFile := fs.String("keys-file", "", "issue keys to seed (others become placeholders)")
	workers := fs.Int("workers", 3, "issues seeded in parallel")
	opsLog := fs.String("ops-log", "", "CSV file with one row per write")
	rngSeed := fs.Uint64("rng-seed", 1, "seed of the fallback-anchor and file-byte generators")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *bundleDir == "" || *db == "" {
		return fmt.Errorf("-bundle and -db are required")
	}
	if *workers < 1 {
		return fmt.Errorf("-workers must be at least 1")
	}
	ep, err := ef.resolve()
	if err != nil {
		return err
	}
	pgURL, err := withDatabase(ep.pg, *db)
	if err != nil {
		return err
	}
	b, err := readBundle(*bundleDir)
	if err != nil {
		return err
	}
	var keys map[string]bool
	if *keysFile != "" {
		if keys, err = readKeysFile(*keysFile); err != nil {
			return err
		}
	}
	plans, err := planIssues(b.State, keys)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := checkStack(ctx, ep); err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, pgURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	s := &replaySeeder{
		b: b, api: newAPI(ep.dispatch, ep.token, *workers*2+2), pool: pool, rngSeed: *rngSeed,
		dispatchPID: ep.pids["dispatch"], workers: *workers,
		lg: log.New(os.Stderr, "replay-seed ", log.LstdFlags), start: time.Now(),
		marks: map[string]int{}, counts: map[string]int{}, rules: map[string]int{},
	}
	if *opsLog != "" {
		f, err := os.Create(*opsLog)
		if err != nil {
			return err
		}
		defer f.Close()
		s.ops = csv.NewWriter(f)
		_ = s.ops.Write([]string{"kind", "issue", "doc_bytes", "open_marks", "latency_ms", "ticks_before", "ticks_after"})
		defer s.ops.Flush()
	}
	return s.run(ctx, plans)
}

// withDatabase is the Postgres URL with its database replaced.
func withDatabase(pgURL, db string) (string, error) {
	u, err := url.Parse(pgURL)
	if err != nil {
		return "", fmt.Errorf("postgres url: %w", err)
	}
	u.Path = "/" + db
	return u.String(), nil
}

type replaySeeder struct {
	b           *replayBundle
	api         *api
	pool        *pgxpool.Pool
	ids         *idStore
	rngSeed     uint64
	dispatchPID int
	workers     int
	lg          *log.Logger
	start       time.Time

	mu      sync.Mutex
	ops     *csv.Writer
	marks   map[string]int // local document id -> open anchored marks the seed has made on it
	counts  map[string]int // writes by kind
	rules   map[string]int // anchors this run placed, by chooseAnchor branch
	skipped map[string]int
	writes  atomic.Int64
	done    atomic.Int64
}

// seedError stops the seed: a refusal that is not transient, named by step, logged id, status and
// Dispatch's error code, never the body.
type seedError struct {
	step, logged string
	status       int
	code         string
}

func (e *seedError) Error() string {
	return fmt.Sprintf("%s %s: status %d code %q", e.step, e.logged, e.status, e.code)
}

func (s *replaySeeder) run(ctx context.Context, plans []seedIssuePlan) error {
	if _, err := s.pool.Exec(ctx, replayIDsDDL); err != nil {
		return fmt.Errorf("create loadtest.replay_ids: %w", err)
	}
	ids, err := loadIDStore(ctx, s.pool)
	if err != nil {
		return err
	}
	s.ids = ids
	s.skipped = map[string]int{}
	seeded := 0
	for _, p := range plans {
		if !p.Placeholder {
			seeded++
		}
	}
	s.lg.Printf("bundle %s at %s: %d issue numbers, %d seeded, %d placeholders; %d ids already mapped",
		s.b.State.Project, s.b.State.At.Format(time.RFC3339), len(plans), seeded, len(plans)-seeded, ids.len())

	stop := make(chan struct{})
	go s.progress(stop, len(plans))
	defer close(stop)

	// Step 0: the project.
	if _, err := s.write(ctx, "project-create", "", 0, "", "project", s.b.State.Project, "POST", "/api/v1/projects", seedWho(),
		jsonBody(map[string]any{"key": s.b.State.Project, "name": s.b.State.Project, "actor": sessionActor(replaySeedActorID)}), http.StatusConflict); err != nil {
		return err
	}
	// Step 1: every issue number in order, one at a time.
	for _, p := range plans {
		if err := s.createIssue(ctx, p); err != nil {
			return err
		}
	}
	s.lg.Printf("issues created: %d in %s", len(plans), time.Since(s.start).Round(time.Second))
	// Steps 2-5: per issue, issues in parallel.
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(s.workers)
	for _, p := range plans {
		g.Go(func() error {
			var err error
			if p.Placeholder {
				err = s.finishIssue(gctx, p)
			} else {
				err = s.seedIssue(gctx, p)
			}
			s.done.Add(1)
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	fallbacks := ids.count(idFallbackComment) + ids.count(idFallbackAsk)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lg.Printf("seed done in %s: %d writes %s; fallbacks %d (comments %d, asks %d); anchors placed this run by branch %s; skipped %s",
		time.Since(s.start).Round(time.Second), s.writes.Load(), fmtCounts(s.counts), fallbacks,
		ids.count(idFallbackComment), ids.count(idFallbackAsk), fmtCounts(s.rules), fmtCounts(s.skipped))
	return nil
}

func (s *replaySeeder) progress(stop chan struct{}, issues int) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.mu.Lock()
			c := fmtCounts(s.counts)
			s.mu.Unlock()
			s.lg.Printf("progress %s: %d writes %s; %d of %d issues finished", time.Since(s.start).Round(time.Second), s.writes.Load(), c, s.done.Load(), issues)
		}
	}
}

func fmtCounts(m map[string]int) string {
	keys := slices.Sorted(maps.Keys(m))
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return "{" + strings.Join(parts, " ") + "}"
}

func (s *replaySeeder) skip(what string, n int) {
	if n == 0 {
		return
	}
	s.mu.Lock()
	s.skipped[what] += n
	s.mu.Unlock()
}

// --- requests ------------------------------------------------------------------------------------

type rawBody struct {
	contentType string
	payload     []byte
}

func jsonBody(v any) rawBody {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // every body is maps, strings and numbers
	}
	return rawBody{"application/json", b}
}

func seedWho() who { return who{} }

// actorOf is how the seed acts as a logged actor: a session through the agent bearer with the
// session named in the body, a user through the identity header.
func actorOf(a *replayActor) (who, map[string]any, error) {
	if a == nil {
		return who{}, map[string]any{"kind": "session", "id": replaySeedActorID}, nil
	}
	switch a.Kind {
	case "session":
		actor := map[string]any{"kind": "session", "id": a.ID}
		if a.Origin != nil {
			actor["origin"] = a.Origin
		}
		return who{}, actor, nil
	case "user":
		return who{person: a.ID}, nil, nil
	}
	return who{}, nil, fmt.Errorf("actor kind %q cannot be acted as", a.Kind)
}

// withActor adds the body's actor when the request acts as a session.
func withActor(body map[string]any, actor map[string]any) map[string]any {
	if actor != nil {
		body["actor"] = actor
	}
	return body
}

// write sends one write, retrying a transport error, 429 or 5xx with backoff, and records it in the
// counts and the ops log. A 2xx or a status in ok returns the body; any other status is a
// *seedError.
func (s *replaySeeder) write(ctx context.Context, kind, issue string, docBytes int, docID, step, logged, method, path string, w who, body rawBody, ok ...int) ([]byte, error) {
	backoff := 250 * time.Millisecond
	for attempt := 0; ; attempt++ {
		before := s.ticks()
		res := s.api.callRaw(ctx, method, path, w, body.contentType, body.payload)
		after := s.ticks()
		transient := res.err != nil || res.status == http.StatusTooManyRequests || res.status >= 500
		if transient && attempt < 10 && ctx.Err() == nil {
			s.lg.Printf("%s %s: transient status %d (%v); retrying in %s", step, logged, res.status, errClass(res.err), backoff)
			time.Sleep(backoff)
			backoff = min(2*backoff, 8*time.Second)
			continue
		}
		if res.err != nil {
			return nil, fmt.Errorf("%s %s: %w", step, logged, res.err)
		}
		accepted := res.ok()
		for _, st := range ok {
			accepted = accepted || res.status == st
		}
		if !accepted {
			return nil, &seedError{step: step, logged: logged, status: res.status, code: dispatchCode(res.body)}
		}
		if res.ok() {
			s.record(kind, issue, docBytes, docID, time.Duration(res.end-res.start), before, after)
		}
		return res.body, nil
	}
}

// errClass names a transport error without the URL it may carry.
func errClass(err error) string {
	if err == nil {
		return "no error"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return fmt.Sprintf("%T", errors.Unwrap(err))
}

// dispatchCode is the `code` of a Dispatch error body.
func dispatchCode(body []byte) string {
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &e)
	return e.Code
}

func (s *replaySeeder) record(kind, issue string, docBytes int, docID string, latency time.Duration, before, after int64) {
	s.writes.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts[kind]++
	if s.ops != nil {
		_ = s.ops.Write([]string{kind, issue, strconv.Itoa(docBytes), strconv.Itoa(s.marks[docID]),
			strconv.FormatFloat(float64(latency.Microseconds())/1000, 'f', 2, 64),
			strconv.FormatInt(before, 10), strconv.FormatInt(after, 10)})
	}
}

// ticks is Dispatch's utime+stime in clock ticks, -1 when its pid is unknown or gone.
func (s *replaySeeder) ticks() int64 {
	if s.ops == nil || s.dispatchPID == 0 {
		return -1
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", s.dispatchPID))
	if err != nil {
		return -1
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return -1
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 13 {
		return -1
	}
	u, _ := strconv.ParseInt(f[11], 10, 64)
	st, _ := strconv.ParseInt(f[12], 10, 64)
	return u + st
}

func (s *replaySeeder) addMark(docID string, n int) {
	s.mu.Lock()
	s.marks[docID] += n
	s.mu.Unlock()
}

// idOf decodes the id of a created object: top-level "id", or the "artifact" an upload answers
// with, or the "ask" an approval request answers with.
func idOf(body []byte) string {
	type withID struct {
		ID string `json:"id"`
	}
	var r struct {
		ID       string  `json:"id"`
		Artifact *withID `json:"artifact"`
		Ask      *withID `json:"ask"`
	}
	_ = json.Unmarshal(body, &r)
	switch {
	case r.ID != "":
		return r.ID
	case r.Artifact != nil:
		return r.Artifact.ID
	case r.Ask != nil:
		return r.Ask.ID
	}
	return ""
}

// --- step 1: issues ------------------------------------------------------------------------------

func (s *replaySeeder) createIssue(ctx context.Context, p seedIssuePlan) error {
	if _, ok := s.ids.get(idIssue, p.Key); ok {
		return nil
	}
	// A seed killed after Dispatch created the issue but before the mapping was written left it
	// unmapped: adopt it.
	var key, primary string
	err := s.pool.QueryRow(ctx, `select key, coalesce((select id::text from artifacts where issue_key = issues.key and is_primary), '')
		from issues where project_key = $1 and number = $2`, s.b.State.Project, p.Number).Scan(&key, &primary)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil {
		return s.mapIssue(ctx, p, key, primary)
	}
	var w who
	var actor map[string]any
	body := map[string]any{"project": s.b.State.Project, "title": placeholderTitle, "force": true}
	docBytes := 0
	if p.Placeholder {
		actor = sessionActorAny(replaySeedActorID)
	} else {
		if w, actor, err = actorOf(p.Issue.CreatedBy); err != nil {
			return fmt.Errorf("issue %s: %w", p.Key, err)
		}
		pa := primaryArtifact(p.Issue)
		if pa == nil {
			return fmt.Errorf("issue %s has no primary document in the bundle", p.Key)
		}
		text, err := s.docText(pa)
		if err != nil {
			return err
		}
		body["title"] = p.Issue.Title
		plan, err := docPlan(p.Issue, pa, text)
		if err != nil {
			return err
		}
		body["spec"] = firstVersionText(p.Issue, pa, text, plan)
		docBytes = len(body["spec"].(string))
	}
	res, err := s.write(ctx, "issue-create", p.Key, docBytes, "", "issue-create", p.Key, "POST", "/api/v1/issues", w, jsonBody(withActor(body, actor)))
	if err != nil {
		return err
	}
	var created struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}
	if err := json.Unmarshal(res, &created); err != nil || created.Key == "" {
		return fmt.Errorf("issue-create %s: undecodable response (%d bytes)", p.Key, len(res))
	}
	return s.mapIssue(ctx, p, created.Key, created.PrimaryArtifactID)
}

func (s *replaySeeder) mapIssue(ctx context.Context, p seedIssuePlan, key, primary string) error {
	if key != p.Key {
		return fmt.Errorf("issue number %d was created as %s, want %s: the numbering no longer matches the bundle", p.Number, key, p.Key)
	}
	if !p.Placeholder {
		if err := s.ids.put(ctx, idArtifact, primaryArtifact(p.Issue).ID, primary); err != nil {
			return err
		}
	}
	return s.ids.put(ctx, idIssue, p.Key, key)
}

func sessionActorAny(id string) map[string]any { return map[string]any{"kind": "session", "id": id} }

func primaryArtifact(is *replayIssue) *replayArtifact {
	for i := range is.Artifacts {
		if is.Artifacts[i].Primary && is.Artifacts[i].Kind == "doc" {
			return &is.Artifacts[i]
		}
	}
	return nil
}

// docText is a document's markdown at the bundle's instant.
func (s *replaySeeder) docText(a *replayArtifact) (string, error) {
	if a.TextPath == "" {
		return "", fmt.Errorf("document %s has no text in the bundle", a.ID)
	}
	if a.TextVersion != a.Version {
		return "", fmt.Errorf("document %s holds the text of version %d, not of its version %d", a.ID, a.TextVersion, a.Version)
	}
	b, err := s.b.readFile(a.TextPath)
	if err != nil {
		return "", err
	}
	if strings.Contains(string(b), padToken) {
		return "", fmt.Errorf("document %s already holds the padding token", a.ID)
	}
	return string(b), nil
}

// --- steps 2-5: one issue ------------------------------------------------------------------------

// issueDoc is one of an issue's documents as the seed tracks it, with its history plan.
type issueDoc struct {
	a     *replayArtifact
	local string
	text  string
	plan  historyPlan
	is    *replayIssue
}

// blockSteps are the ask steps of the typed ask blocks on document a, each with the version it
// lands at: an answered block's answer; a resolved block's answer, if it was answered, then its
// resolve.
func blockSteps(is *replayIssue, a *replayArtifact) (steps []askStep, versions []int) {
	for i, ask := range is.Asks {
		if ask.BlockID == nil || ask.BlockArtifactID == nil || *ask.BlockArtifactID != a.ID {
			continue
		}
		if ask.State == "answered" || (ask.State == "resolved" && ask.Answer != nil) {
			steps = append(steps, askStep{Ask: i})
			versions = append(versions, ask.AnswerVersion)
		}
		if ask.State == "resolved" {
			steps = append(steps, askStep{Ask: i, Resolve: true})
			versions = append(versions, ask.ResolveVersion)
		}
	}
	return steps, versions
}

func docPlan(is *replayIssue, a *replayArtifact, text string) (historyPlan, error) {
	steps, versions := blockSteps(is, a)
	p, err := planHistory(text, a.VersionSizes, a.Version, steps, versions)
	if err != nil {
		return p, fmt.Errorf("document %s: %w", a.ID, err)
	}
	return p, nil
}

// blockIDs are the block ids of the steps' asks.
func blockIDs(is *replayIssue, steps []askStep) map[string]bool {
	ids := map[string]bool{}
	for _, st := range steps {
		ids[*is.Asks[st.Ask].BlockID] = true
	}
	return ids
}

// firstVersionText is what a document is created with: its text at At when its whole history is
// that version, else the base (the text less the plan's run, every answered or resolved block
// stripped to open) and a padding paragraph that makes version 1 plan.Sizes[0] bytes.
func firstVersionText(is *replayIssue, a *replayArtifact, text string, p historyPlan) string {
	steps, _ := blockSteps(is, a)
	if p.Restore == 1 {
		return text
	}
	base := stripAnswers(baseText(text, p.RunStart, p.RunEnd), blockIDs(is, steps))
	pad := padText(p.Sizes[0]-len(base)-padOverhead(len(base)), 1)
	if base == "" {
		return pad + "\n"
	}
	return base + "\n" + pad + "\n"
}

func (s *replaySeeder) seedIssue(ctx context.Context, p seedIssuePlan) error {
	is := p.Issue
	docs := map[string]*issueDoc{} // logged id -> document
	var order []*issueDoc
	for i := range is.Artifacts {
		a := &is.Artifacts[i]
		if a.Kind != "doc" {
			continue
		}
		text, err := s.docText(a)
		if err != nil {
			return err
		}
		plan, err := docPlan(is, a, text)
		if err != nil {
			return err
		}
		d := &issueDoc{a: a, text: text, plan: plan, is: is}
		docs[a.ID] = d
		if a.Primary {
			order = append([]*issueDoc{d}, order...)
		} else {
			order = append(order, d)
		}
	}
	// Step 2: the other documents, then the files, then each document's history.
	for _, d := range order {
		if d.a.Primary {
			local, ok := s.ids.get(idArtifact, d.a.ID)
			if !ok {
				return fmt.Errorf("issue %s: primary document %s is unmapped", p.Key, d.a.ID)
			}
			d.local = local
			continue
		}
		if err := s.ensureDoc(ctx, p.Key, d); err != nil {
			return err
		}
	}
	for i := range is.Artifacts {
		if a := &is.Artifacts[i]; a.Kind != "doc" {
			if err := s.ensureFile(ctx, p.Key, a); err != nil {
				return err
			}
		}
	}
	for _, d := range order {
		if err := s.history(ctx, p.Key, docs, d); err != nil {
			return err
		}
	}
	locals := make([]string, 0, len(order))
	for _, d := range order {
		locals = append(locals, d.local)
	}
	if err := s.waitSettled(ctx, p.Key, locals); err != nil {
		return err
	}
	if err := s.loadMarks(ctx, locals); err != nil {
		return err
	}
	// Step 3: comments.
	plan := planComments(is)
	s.skip("comments-of-resolved-threads", plan.SkippedResolved)
	s.skip("comments-on-asks-not-open", plan.SkippedAsk)
	s.skip("replies-without-root", plan.SkippedOrphan)
	orphans, err := s.unmappedComments(ctx, p.Key)
	if err != nil {
		return err
	}
	for _, st := range plan.Main {
		if err := s.comment(ctx, p.Key, docs, st, &orphans); err != nil {
			return err
		}
	}
	for _, c := range plan.Resolve {
		if err := s.resolve(ctx, p.Key, docs, c); err != nil {
			return err
		}
	}
	// Step 4: asks, each with its thread.
	for i := range is.Asks {
		if err := s.ask(ctx, p.Key, docs, &is.Asks[i], plan.ByAsk[is.Asks[i].ID], &orphans); err != nil {
			return err
		}
	}
	// Step 5: the issue's fields, then its claim. The marks the comments and asks left are settled
	// first: a closed issue's documents settle nothing, so closing it with a settlement owed would
	// leave that settlement owed for good.
	if err := s.waitSettled(ctx, p.Key, locals); err != nil {
		return err
	}
	return s.finishIssue(ctx, p)
}

func (s *replaySeeder) ensureDoc(ctx context.Context, key string, d *issueDoc) error {
	if local, ok := s.ids.get(idArtifact, d.a.ID); ok {
		d.local = local
		return nil
	}
	local, err := s.adoptArtifact(ctx, key, d.a.Name)
	if err != nil {
		return err
	}
	if local == "" {
		text := firstVersionText(d.is, d.a, d.text, d.plan)
		res, err := s.write(ctx, "doc-create", key, len(text), "", "doc-create", d.a.ID, "POST", "/api/v1/issues/"+key+"/artifacts", seedWho(),
			jsonBody(map[string]any{"name": d.a.Name, "content": text, "actor": sessionActorAny(replaySeedActorID)}))
		if err != nil {
			return err
		}
		if local = idOf(res); local == "" {
			return fmt.Errorf("doc-create %s: no id in the response", d.a.ID)
		}
	}
	d.local = local
	return s.ids.put(ctx, idArtifact, d.a.ID, local)
}

// adoptArtifact finds an artifact of the issue by name that no logged id maps to yet: one a killed
// seed created without recording it.
func (s *replaySeeder) adoptArtifact(ctx context.Context, key, name string) (string, error) {
	rows, err := s.pool.Query(ctx, `select id::text from artifacts where issue_key = $1 and name = $2 order by created_at`, key, name)
	if err != nil {
		return "", err
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", err
	}
	mapped := s.ids.locals(idArtifact)
	for _, id := range found {
		if !mapped[id] {
			return id, nil
		}
	}
	return "", nil
}

// fileBytes are a file's stand-in bytes: size bytes from a PCG keyed by the rng seed and the
// file's logged id.
func fileBytes(rngSeed uint64, id string, size int) []byte {
	h := fnv.New64a()
	h.Write([]byte(id))
	r := rand.New(rand.NewPCG(rngSeed, h.Sum64()))
	out := make([]byte, size)
	for i := 0; i < size; i += 8 {
		v := r.Uint64()
		for j := 0; j < 8 && i+j < size; j++ {
			out[i+j] = byte(v >> (8 * j))
		}
	}
	return out
}

func (s *replaySeeder) ensureFile(ctx context.Context, key string, a *replayArtifact) error {
	if _, ok := s.ids.get(idArtifact, a.ID); ok {
		return nil
	}
	local, err := s.adoptArtifact(ctx, key, a.Name)
	if err != nil {
		return err
	}
	if local == "" {
		if a.Size == nil {
			return fmt.Errorf("file %s has no logged size", a.ID)
		}
		mimeType := "application/octet-stream"
		if a.MIME != nil && *a.MIME != "" {
			mimeType = *a.MIME
		}
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("name", a.Name)
		actor, _ := json.Marshal(sessionActorAny(replaySeedActorID))
		_ = mw.WriteField("actor", string(actor))
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", multipart.FileContentDisposition("file", a.Name))
		h.Set("Content-Type", mimeType)
		part, err := mw.CreatePart(h)
		if err != nil {
			return err
		}
		if _, err := part.Write(fileBytes(s.rngSeed, a.ID, *a.Size)); err != nil {
			return err
		}
		if err := mw.Close(); err != nil {
			return err
		}
		res, err := s.write(ctx, "file-create", key, 0, "", "file-create", a.ID, "POST", "/api/v1/issues/"+key+"/artifacts", seedWho(),
			rawBody{mw.FormDataContentType(), buf.Bytes()})
		if err != nil {
			return err
		}
		if local = idOf(res); local == "" {
			return fmt.Errorf("file-create %s: no id in the response", a.ID)
		}
	}
	return s.ids.put(ctx, idArtifact, a.ID, local)
}

// history takes a document from the version it stands at to its logged version as its plan says
// (planHistory): each padded version resizes the padding paragraph so the version changes size by
// its planned change from the version Dispatch stored before it (historyPlan.padLen; a version
// whose size does not change gets padding of the same length with other words); an ask step
// version re-sends that typed ask block's answer or resolve, a change Dispatch makes; the restore
// version removes the padding and puts the run back. Version 1's own size is free: no change is
// compared for it. Every step must write exactly one version, and each padded or restored version
// must change size by exactly its planned change.
func (s *replaySeeder) history(ctx context.Context, key string, docs map[string]*issueDoc, d *issueDoc) error {
	p := d.plan
	cur, err := s.maxVersion(ctx, d.local)
	if err != nil {
		return err
	}
	if cur < 1 || cur > p.Final {
		return fmt.Errorf("history %s: document stands at version %d, logged version %d", d.a.ID, cur, p.Final)
	}
	edit := func(n int, ops ...any) error {
		size, _, err := s.versionSize(ctx, d.local, n-1)
		if err != nil {
			return err
		}
		if _, err := s.write(ctx, "history-edit", key, size, d.local, "history-edit", fmt.Sprintf("%s@%d", d.a.ID, n), "POST", "/api/v1/artifacts/"+d.local+"/edits", seedWho(),
			jsonBody(map[string]any{"ops": ops, "actor": sessionActorAny(replaySeedActorID)})); err != nil {
			return err
		}
		if got, err := s.maxVersion(ctx, d.local); err != nil {
			return err
		} else if got != n {
			return fmt.Errorf("history %s: the edit to version %d left the document at version %d", d.a.ID, n, got)
		}
		return nil
	}
	for n := cur + 1; n <= p.Final; n++ {
		size, markdown, err := s.versionSize(ctx, d.local, n-1)
		if err != nil {
			return err
		}
		pad := currentPad(markdown)
		switch {
		case len(p.AskAt[n]) > 0:
			// The version's answers and resolves are sent back to back, inside Dispatch's settle
			// delay, so that together they write exactly one version, as production's did.
			if err := s.waitSettled(ctx, key, []string{d.local}); err != nil {
				return err
			}
			for _, st := range p.AskAt[n] {
				a := &d.is.Asks[st.Ask]
				local, err := s.ensureAsk(ctx, key, docs, a)
				if err != nil {
					return err
				}
				step := s.answer
				if st.Resolve {
					step = s.resolveAsk
				}
				if err := step(ctx, key, docs, a, local); err != nil {
					return err
				}
			}
			if err := s.waitVersion(ctx, d, n); err != nil {
				return fmt.Errorf("history %s: the answers and resolves of version %d: %w", d.a.ID, n, err)
			}
			continue
		case n == p.Restore:
			var ops []any
			if pad != "" {
				ops = append(ops, map[string]any{"op": "delete", "find": pad})
			}
			if p.RunEnd > p.RunStart {
				insert := map[string]any{"op": "insert", "markdown": d.text[p.RunStart:p.RunEnd], "after": "end"}
				if p.RunEnd < len(d.text) {
					block, err := s.headingBlock(ctx, d, markdown)
					if err != nil {
						return err
					}
					delete(insert, "after")
					insert["before"] = "block:" + block
				}
				ops = append(ops, insert)
			}
			if err := edit(n, ops...); err != nil {
				return err
			}
			_, got, err := s.versionSize(ctx, d.local, n)
			if err != nil {
				return err
			}
			var later []askStep
			for v, steps := range p.AskAt {
				if v > n {
					later = append(later, steps...)
				}
			}
			want := stripAnswers(d.text, blockIDs(d.is, later))
			if !sameMasked(got, want) {
				return fmt.Errorf("history %s: the restored version %d has %d bytes and differs from the text at At less its later answers and resolves (%d bytes)", d.a.ID, n, len(got), len(want))
			}
			if len(later) > 0 {
				// Its size rests on the later answers' and resolves' changes matching
				// production's; the check compares them.
				continue
			}
		default:
			if pad == "" {
				return fmt.Errorf("history %s: version %d holds no padding paragraph", d.a.ID, n-1)
			}
			l := p.padLen(n, len(pad))
			if l < padMin {
				return fmt.Errorf("history %s: version %d would hold padding of %d bytes, under %d", d.a.ID, n, l, padMin)
			}
			next := nextPad(pad, l, n)
			if err := edit(n, map[string]any{"op": "replace", "find": pad, "with": next}); err != nil {
				return err
			}
		}
		if got, _, err := s.versionSize(ctx, d.local, n); err != nil {
			return err
		} else if got-size != p.Sizes[n-1]-p.Sizes[n-2] {
			return fmt.Errorf("history %s: version %d changes size by %d bytes (%d to %d), planned %d", d.a.ID, n, got-size, size, got, p.Sizes[n-1]-p.Sizes[n-2])
		}
	}
	return nil
}

// versionSize is a version's size as Dispatch serves it (coalesce(size, octet_length(markdown)))
// and its markdown.
func (s *replaySeeder) versionSize(ctx context.Context, artifactID string, n int) (int, string, error) {
	var size int
	var markdown string
	err := s.pool.QueryRow(ctx, `select coalesce(size, octet_length(markdown)), coalesce(markdown, '')
		from artifact_versions where artifact_id = $1 and number = $2`, artifactID, n).Scan(&size, &markdown)
	return size, markdown, err
}

// currentPad is the padding paragraph a padded version ends with, empty when it has none.
func currentPad(markdown string) string {
	lines := strings.Split(strings.TrimRight(markdown, "\n"), "\n")
	if last := lines[len(lines)-1]; strings.HasPrefix(last, padText(5, 0)) {
		return last
	}
	return ""
}

// headingBlock is the local block id of the heading the plan's run goes back before: the block
// whose range starts at that heading line's occurrence nearest the run's offset in markdown.
func (s *replaySeeder) headingBlock(ctx context.Context, d *issueDoc, markdown string) (string, error) {
	heading := d.text[d.plan.RunEnd:]
	if i := strings.IndexByte(heading, '\n'); i >= 0 {
		heading = heading[:i]
	}
	best := -1
	// Searched in "\n"+markdown, so a heading on the first line is found; an index there is the
	// heading's offset in markdown.
	padded := "\n" + markdown
	for off := 0; ; {
		i := strings.Index(padded[off:], "\n"+heading+"\n")
		if i < 0 {
			break
		}
		at := off + i
		if best < 0 || abs(at-d.plan.RunStart) < abs(best-d.plan.RunStart) {
			best = at
		}
		off = at + 1
	}
	if best < 0 {
		return "", fmt.Errorf("history %s: the heading after the run is not in the document", d.a.ID)
	}
	res := s.api.call(ctx, "GET", "/api/v1/artifacts/"+d.local+"/blocks", seedWho(), nil)
	if !res.ok() {
		return "", &seedError{step: "blocks-read", logged: d.a.ID, status: res.status, code: dispatchCode(res.body)}
	}
	var blocks []struct {
		ID   string `json:"id"`
		From int    `json:"from"`
	}
	if err := json.Unmarshal(res.body, &blocks); err != nil {
		return "", fmt.Errorf("blocks-read %s: undecodable response", d.a.ID)
	}
	for _, b := range blocks {
		if b.From == best {
			return b.ID, nil
		}
	}
	return "", fmt.Errorf("history %s: no block starts at the heading after the run (offset %d)", d.a.ID, best)
}

// sameMasked says whether two markdowns are equal once answered_at is masked.
func sameMasked(a, b string) bool {
	ma, _ := maskAnsweredAt(a)
	mb, _ := maskAnsweredAt(b)
	return ma == mb
}
func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func (s *replaySeeder) maxVersion(ctx context.Context, artifactID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `select coalesce(max(number), 0) from artifact_versions where artifact_id = $1`, artifactID).Scan(&n)
	return n, err
}

// waitSettled waits until Dispatch owes no settlement on the documents, so their typed ask blocks
// are indexed and no settlement version lands after the history.
func (s *replaySeeder) waitSettled(ctx context.Context, key string, docs []string) error {
	deadline := time.Now().Add(5 * time.Minute)
	for {
		var n int
		if err := s.pool.QueryRow(ctx, `select count(*) from doc_settlements_pending where artifact_id::text = any($1)`, docs).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("issue %s: %d documents still owe a settlement after 5 minutes", key, n)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// loadMarks reads the open anchored items on the documents (a resumed seed's earlier writes).
func (s *replaySeeder) loadMarks(ctx context.Context, docs []string) error {
	for _, d := range docs {
		n, err := openAnchored(ctx, s.pool, d)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.marks[d] = n
		s.mu.Unlock()
	}
	return nil
}

// openAnchored counts a document's open anchored items: unresolved anchored root comments and
// anchored asks not resolved.
func openAnchored(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, doc string) (int, error) {
	var n int
	err := q.QueryRow(ctx, `select
		(select count(*) from comments where anchor->>'artifact_id' = $1 and reply_to is null and not resolved) +
		(select count(*) from asks where anchor->>'artifact_id' = $1 and state <> 'resolved')`, doc).Scan(&n)
	return n, err
}

// --- step 3: comments ----------------------------------------------------------------------------

// localComment is a comment of the issue no logged id maps to yet.
type localComment struct {
	id, body, author, quote string
}

func (s *replaySeeder) unmappedComments(ctx context.Context, key string) ([]localComment, error) {
	rows, err := s.pool.Query(ctx, `select id::text, body, coalesce(author->>'id', ''), coalesce(anchor->>'quote', '')
		from comments where issue_key = $1 order by created_at, id`, key)
	if err != nil {
		return nil, err
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (localComment, error) {
		var c localComment
		return c, r.Scan(&c.id, &c.body, &c.author, &c.quote)
	})
	if err != nil {
		return nil, err
	}
	mapped := s.ids.locals(idComment)
	var out []localComment
	for _, c := range all {
		if !mapped[c.id] {
			out = append(out, c)
		}
	}
	return out, nil
}

// adoptComment maps a comment a killed seed created without recording it: the oldest unmapped
// comment, when it is this one (same body and author).
func (s *replaySeeder) adoptComment(ctx context.Context, c *replayComment, d *issueDoc, orphans *[]localComment) (bool, error) {
	if len(*orphans) == 0 {
		return false, nil
	}
	o := (*orphans)[0]
	if o.body != c.Body || o.author != c.Actor.ID {
		return false, nil
	}
	*orphans = (*orphans)[1:]
	if c.Anchor != nil && d != nil {
		choice := s.choose(d, c.ID, c.Anchor, o.quote != c.Anchor.Quote)
		if err := s.recordAnchor(ctx, idComment, c.ID, o.id, d, choice); err != nil {
			return false, err
		}
		if o.quote != c.Anchor.Quote {
			if err := s.ids.put(ctx, idFallbackComment, c.ID, o.id); err != nil {
				return false, err
			}
		}
	}
	return true, s.ids.put(ctx, idComment, c.ID, o.id)
}

func (s *replaySeeder) comment(ctx context.Context, key string, docs map[string]*issueDoc, st commentStep, orphans *[]localComment) error {
	c := &st.C
	if _, ok := s.ids.get(idComment, c.ID); ok {
		return nil
	}
	var anchorDoc *issueDoc
	if c.Anchor != nil && c.ReplyTo == nil {
		anchorDoc = docs[c.Anchor.ArtifactID]
	}
	if ok, err := s.adoptComment(ctx, c, anchorDoc, orphans); ok || err != nil {
		return err
	}
	w, actor, err := actorOf(&c.Actor)
	if err != nil {
		return fmt.Errorf("comment %s: %w", c.ID, err)
	}
	body := withActor(map[string]any{"body": c.Body}, actor)
	if c.ReplyTo != nil {
		parent, ok := s.ids.get(idComment, *c.ReplyTo)
		if !ok {
			return fmt.Errorf("comment %s replies to %s, which is not seeded", c.ID, *c.ReplyTo)
		}
		body["reply_to"] = parent
	}
	if c.AskID != nil && c.ReplyTo == nil {
		ask, ok := s.ids.get(idAsk, *c.AskID)
		if !ok {
			return fmt.Errorf("comment %s is on ask %s, which is not seeded", c.ID, *c.AskID)
		}
		body["ask_id"] = ask
	}
	docBytes, docID := 0, ""
	if d := docs[st.RootDoc]; d != nil && st.Kind == commentReply {
		docBytes, docID = len(d.text), d.local
	}
	if st.Kind != commentAnchored {
		res, err := s.write(ctx, string(st.Kind), key, docBytes, docID, string(st.Kind), c.ID, "POST", "/api/v1/issues/"+key+"/comments", w, jsonBody(body))
		if err != nil {
			return err
		}
		return s.mapCreated(ctx, idComment, c.ID, res)
	}
	d := docs[c.Anchor.ArtifactID]
	if d == nil {
		return fmt.Errorf("comment %s is anchored on %s, which is not one of the issue's seeded documents", c.ID, c.Anchor.ArtifactID)
	}
	local, fellBack, err := s.anchored(ctx, string(commentAnchored), key, d, c.ID, c.Anchor, w, "/api/v1/issues/"+key+"/comments", body)
	if err != nil {
		return err
	}
	if fellBack {
		if err := s.ids.put(ctx, idFallbackComment, c.ID, local); err != nil {
			return err
		}
	}
	s.addMark(d.local, 1)
	return s.ids.put(ctx, idComment, c.ID, local)
}

func (s *replaySeeder) mapCreated(ctx context.Context, kind, logged string, res []byte) error {
	local := idOf(res)
	if local == "" {
		return fmt.Errorf("%s %s: no id in the response", kind, logged)
	}
	return s.ids.put(ctx, kind, logged, local)
}

// choose is chooseAnchor for a logged anchor on a seeded document.
func (s *replaySeeder) choose(d *issueDoc, logged string, a *replayAnchor, skipLogged bool) anchorChoice {
	blockID := ""
	if a.BlockID != nil {
		blockID = *a.BlockID
	}
	return chooseAnchor(d.text, s.b.State.BlockMaps[d.a.ID], blockID, a.Quote, a.Orphaned, skipLogged, s.rngSeed, d.a.ID, logged)
}

// recordAnchor keeps the branch and the chosen block's span of a seeded anchor in
// loadtest.replay_anchors, where replay-check-seed reads them.
func (s *replaySeeder) recordAnchor(ctx context.Context, kind, logged, local string, d *issueDoc, c anchorChoice) error {
	_, err := s.pool.Exec(ctx, `insert into loadtest.replay_anchors (kind, logged_id, local_id, logged_doc, branch, block_id, span_start, span_end, quote_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9) on conflict (kind, logged_id) do update set local_id = excluded.local_id,
		logged_doc = excluded.logged_doc, branch = excluded.branch, block_id = excluded.block_id,
		span_start = excluded.span_start, span_end = excluded.span_end, quote_at = excluded.quote_at`,
		kind, logged, local, d.a.ID, c.Branch, c.BlockID, c.Start, c.End, quoteAt(c))
	if err != nil {
		return fmt.Errorf("record anchor %s: %w", logged, err)
	}
	s.mu.Lock()
	s.rules[c.Branch]++
	s.mu.Unlock()
	return nil
}

// anchored posts an anchored comment or ask where chooseAnchor puts it: the logged quote, or a
// sentence of the logged block or of the nearest surviving block, trying the choice's candidates
// in order; when Dispatch refuses the logged quote, the choice is made again without it. It
// records the branch and returns the local id and whether the quote is not the logged one.
func (s *replaySeeder) anchored(ctx context.Context, kind, key string, d *issueDoc, logged string, a *replayAnchor, w who, path string, body map[string]any) (string, bool, error) {
	rowKind := idComment
	if kind == "ask" {
		rowKind = idAsk
	}
	var last error
	for _, skip := range []bool{false, true} {
		choice := s.choose(d, logged, a, skip)
		if choice.Branch == ruleUnanchorable {
			break
		}
		for _, quote := range choice.Candidates {
			anchor := map[string]any{"artifact": d.local, "quote": quote}
			if choice.Occurrence != nil {
				anchor["occurrence"] = *choice.Occurrence
			}
			body["anchor"] = anchor
			res, err := s.write(ctx, kind, key, len(d.text), d.local, kind, logged, "POST", path, w, jsonBody(body))
			if err == nil {
				local := idOf(res)
				if local == "" {
					return "", false, fmt.Errorf("%s %s: no id in the response", kind, logged)
				}
				return local, quote != a.Quote, s.recordAnchor(ctx, rowKind, logged, local, d, choice)
			}
			var se *seedError
			if !errors.As(err, &se) || se.status/100 != 4 {
				return "", false, err
			}
			s.lg.Printf("%s %s: quote refused (branch %s, status %d code %q)", kind, logged, choice.Branch, se.status, se.code)
			last = err
		}
		// Only a refused logged quote (or its occurrence) is chosen again, without it.
		if choice.Branch != ruleLoggedQuote && choice.Occurrence == nil {
			break
		}
	}
	if last == nil {
		return "", false, fmt.Errorf("%s %s cannot be anchored: document %s has no usable sentence", kind, logged, d.a.ID)
	}
	return "", false, fmt.Errorf("%s %s cannot be anchored: every candidate was refused, the last with %w", kind, logged, last)
}

func (s *replaySeeder) resolve(ctx context.Context, key string, docs map[string]*issueDoc, c replayComment) error {
	if _, ok := s.ids.get(idResolve, c.ID); ok {
		return nil
	}
	local, ok := s.ids.get(idComment, c.ID)
	if !ok {
		return fmt.Errorf("resolve %s: the comment is not seeded", c.ID)
	}
	var resolved bool
	if err := s.pool.QueryRow(ctx, `select resolved from comments where id = $1`, local).Scan(&resolved); err != nil {
		return err
	}
	if !resolved {
		w, actor, err := actorOf(c.ResolvedBy)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", c.ID, err)
		}
		var body rawBody
		if actor != nil {
			body = jsonBody(map[string]any{"actor": actor})
		}
		docID := ""
		if c.Anchor != nil && docs[c.Anchor.ArtifactID] != nil {
			docID = docs[c.Anchor.ArtifactID].local
		}
		if _, err := s.write(ctx, "resolve", key, 0, docID, "resolve", c.ID, "POST", "/api/v1/comments/"+local+"/resolve", w, body); err != nil {
			return err
		}
		if c.Anchor != nil && docID != "" {
			s.addMark(docID, -1)
		}
	}
	return s.ids.put(ctx, idResolve, c.ID, local)
}

// --- step 4: asks --------------------------------------------------------------------------------

func (s *replaySeeder) ask(ctx context.Context, key string, docs map[string]*issueDoc, a *replayAsk, thread []commentStep, orphans *[]localComment) error {
	local, err := s.ensureAsk(ctx, key, docs, a)
	if err != nil {
		return err
	}
	before, after := splitAtAnswer(thread, a.Answer)
	for _, st := range before {
		if err := s.comment(ctx, key, docs, st, orphans); err != nil {
			return err
		}
	}
	if a.State == "answered" {
		if err := s.answer(ctx, key, docs, a, local); err != nil {
			return err
		}
	}
	for _, st := range after {
		if err := s.comment(ctx, key, docs, st, orphans); err != nil {
			return err
		}
	}
	return nil
}

func (s *replaySeeder) ensureAsk(ctx context.Context, key string, docs map[string]*issueDoc, a *replayAsk) (string, error) {
	if local, ok := s.ids.get(idAsk, a.ID); ok {
		return local, nil
	}
	mapped := s.ids.locals(idAsk)
	adopt := func(query string, args ...any) (string, error) {
		rows, err := s.pool.Query(ctx, query, args...)
		if err != nil {
			return "", err
		}
		found, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return "", err
		}
		for _, id := range found {
			if !mapped[id] {
				return id, nil
			}
		}
		return "", nil
	}
	switch {
	case a.BlockID != nil:
		// A typed ask block lives in its document's text, which the history wrote; settlement
		// indexed it.
		local, err := adopt(`select id::text from asks where issue_key = $1 and block_id = $2 order by created_at, id`, key, *a.BlockID)
		if err != nil {
			return "", err
		}
		if local == "" {
			return "", fmt.Errorf("ask %s: no ask was indexed from its typed block %s", a.ID, *a.BlockID)
		}
		return local, s.ids.put(ctx, idAsk, a.ID, local)
	case a.Approval != nil:
		d := docs[a.Approval.ArtifactID]
		if d == nil {
			return "", fmt.Errorf("ask %s asks to approve %s, which is not one of the issue's seeded documents", a.ID, a.Approval.ArtifactID)
		}
		local, err := adopt(`select id::text from asks where issue_key = $1 and kind = 'approval' and approval->>'artifact_id' = $2 order by created_at, id`, key, d.local)
		if err != nil {
			return "", err
		}
		if local == "" {
			w, actor, err := actorOf(&a.Author)
			if err != nil {
				return "", fmt.Errorf("ask %s: %w", a.ID, err)
			}
			body := map[string]any{}
			if summary := approvalSummary(a.Question); summary != "" {
				body["summary"] = summary
			}
			res, err := s.write(ctx, "ask", key, len(d.text), d.local, "approval-request", a.ID, "POST", "/api/v1/artifacts/"+d.local+"/approval-requests", w, jsonBody(withActor(body, actor)))
			if err != nil {
				return "", err
			}
			if local = idOf(res); local == "" {
				return "", fmt.Errorf("approval-request %s: no ask in the response", a.ID)
			}
		}
		return local, s.ids.put(ctx, idAsk, a.ID, local)
	}
	local, err := adopt(`select id::text from asks where issue_key = $1 and kind = 'question' and block_id is null and question = $2 order by created_at, id`, key, a.Question)
	if err != nil {
		return "", err
	}
	if local != "" {
		return local, s.ids.put(ctx, idAsk, a.ID, local)
	}
	w, actor, err := actorOf(&a.Author)
	if err != nil {
		return "", fmt.Errorf("ask %s: %w", a.ID, err)
	}
	options := a.Options
	if options == nil {
		options = []replayAskOption{}
	}
	body := withActor(map[string]any{"question": a.Question, "kind": "question", "options": options, "multiple": a.Multiple}, actor)
	if a.Urgency != "" {
		body["urgency"] = a.Urgency
	}
	path := "/api/v1/issues/" + key + "/asks"
	if a.Anchor == nil {
		res, err := s.write(ctx, "ask", key, 0, "", "ask-create", a.ID, "POST", path, w, jsonBody(body))
		if err != nil {
			return "", err
		}
		return idOf(res), s.mapCreated(ctx, idAsk, a.ID, res)
	}
	d := docs[a.Anchor.ArtifactID]
	if d == nil {
		return "", fmt.Errorf("ask %s is anchored on %s, which is not one of the issue's seeded documents", a.ID, a.Anchor.ArtifactID)
	}
	local, fellBack, err := s.anchored(ctx, "ask", key, d, a.ID, a.Anchor, w, path, body)
	if err != nil {
		return "", err
	}
	if fellBack {
		if err := s.ids.put(ctx, idFallbackAsk, a.ID, local); err != nil {
			return "", err
		}
	}
	s.addMark(d.local, 1)
	return local, s.ids.put(ctx, idAsk, a.ID, local)
}

// answer re-sends an answered ask's answer as the answering user: an approval through a review, a
// question (a typed ask block's too) through POST /asks/{id}/answer. A typed ask block's answer
// rewrites its block, which Dispatch versions once settled; history waits for that version.
func (s *replaySeeder) answer(ctx context.Context, key string, docs map[string]*issueDoc, a *replayAsk, local string) error {
	if _, ok := s.ids.get(idAnswer, a.ID); ok {
		return nil
	}
	if a.Answer == nil {
		return fmt.Errorf("ask %s is answered with no answer in the bundle", a.ID)
	}
	var state string
	if err := s.pool.QueryRow(ctx, `select state from asks where id = $1`, local).Scan(&state); err != nil {
		return err
	}
	if state == "open" {
		w := who{person: a.Answer.User}
		if a.Approval != nil {
			body, err := reviewFor(a.Answer)
			if err != nil {
				return fmt.Errorf("ask %s: %w", a.ID, err)
			}
			d := docs[a.Approval.ArtifactID]
			if _, err := s.write(ctx, "answer", key, len(d.text), d.local, "review", a.ID, "POST", "/api/v1/artifacts/"+d.local+"/reviews", w, jsonBody(body)); err != nil {
				return err
			}
			return s.ids.put(ctx, idAnswer, a.ID, local)
		}
		var block *issueDoc
		if a.BlockID != nil {
			if a.BlockArtifactID == nil || docs[*a.BlockArtifactID] == nil {
				return fmt.Errorf("ask %s: an answered typed ask block names no seeded document", a.ID)
			}
			block = docs[*a.BlockArtifactID]
		}
		editedAt, err := s.askEditedAt(ctx, a.ID, local)
		if err != nil {
			return err
		}
		body := map[string]any{"selected": nonNil(a.Answer.Selected), "expected_edited_at": editedAt}
		if a.Answer.Text != nil {
			body["text"] = *a.Answer.Text
		}
		docBytes, docID := 0, ""
		if block != nil {
			docBytes, docID = len(block.text), block.local
		}
		if _, err := s.write(ctx, "answer", key, docBytes, docID, "answer", a.ID, "POST", "/api/v1/asks/"+local+"/answer", w, jsonBody(body)); err != nil {
			return err
		}
	}
	return s.ids.put(ctx, idAnswer, a.ID, local)
}

// resolveAsk resolves a resolved typed ask block as its resolving actor through POST
// /asks/{id}/resolve with the logged resolution's kind and reason. Dispatch writes state="resolved"
// into the block, which it versions once settled; history waits for that version.
func (s *replaySeeder) resolveAsk(ctx context.Context, key string, docs map[string]*issueDoc, a *replayAsk, local string) error {
	if _, ok := s.ids.get(idAskResolve, a.ID); ok {
		return nil
	}
	if a.Resolution == nil {
		return fmt.Errorf("ask %s is resolved with no resolution in the bundle", a.ID)
	}
	if a.BlockArtifactID == nil || docs[*a.BlockArtifactID] == nil {
		return fmt.Errorf("ask %s: a resolved typed ask block names no seeded document", a.ID)
	}
	block := docs[*a.BlockArtifactID]
	var state string
	if err := s.pool.QueryRow(ctx, `select state from asks where id = $1`, local).Scan(&state); err != nil {
		return err
	}
	if state != "resolved" {
		w, actor, err := actorOf(&a.Resolution.By)
		if err != nil {
			return fmt.Errorf("ask-resolve %s: %w", a.ID, err)
		}
		body := withActor(map[string]any{"kind": a.Resolution.Kind, "reason": a.Resolution.Reason}, actor)
		if _, err := s.write(ctx, "resolve", key, len(block.text), block.local, "ask-resolve", a.ID, "POST", "/api/v1/asks/"+local+"/resolve", w, jsonBody(body)); err != nil {
			return err
		}
	}
	return s.ids.put(ctx, idAskResolve, a.ID, local)
}

// waitVersion waits (up to two minutes) until the document owes no settlement and stands at
// version want; standing past it is an error at once.
func (s *replaySeeder) waitVersion(ctx context.Context, d *issueDoc, want int) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var pending, got int
		if err := s.pool.QueryRow(ctx, `select (select count(*) from doc_settlements_pending where artifact_id = $1),
			(select coalesce(max(number), 0) from artifact_versions where artifact_id = $1)`, d.local).Scan(&pending, &got); err != nil {
			return err
		}
		if got > want {
			return fmt.Errorf("document %s stands at version %d, want %d", d.a.ID, got, want)
		}
		if got == want && pending == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("document %s stands at version %d with %d settlements owed after 2 minutes, want version %d", d.a.ID, got, pending, want)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// askEditedAt is the ask's edited_at as Dispatch serves it (GET /api/v1/asks/{id}), which an
// answer must send back as expected_edited_at: a string, or null for an ask never edited.
func (s *replaySeeder) askEditedAt(ctx context.Context, logged, local string) (json.RawMessage, error) {
	res := s.api.call(ctx, "GET", "/api/v1/asks/"+local, seedWho(), nil)
	if !res.ok() {
		return nil, &seedError{step: "ask-read", logged: logged, status: res.status, code: dispatchCode(res.body)}
	}
	var r struct {
		EditedAt json.RawMessage `json:"edited_at"`
		Ask      *struct {
			EditedAt json.RawMessage `json:"edited_at"`
		} `json:"ask"`
	}
	if err := json.Unmarshal(res.body, &r); err != nil {
		return nil, fmt.Errorf("ask-read %s: undecodable response", logged)
	}
	if r.Ask != nil && len(r.Ask.EditedAt) > 0 {
		return r.Ask.EditedAt, nil
	}
	if len(r.EditedAt) > 0 {
		return r.EditedAt, nil
	}
	return json.RawMessage("null"), nil
}

// --- step 5: fields and claim --------------------------------------------------------------------

func (s *replaySeeder) finishIssue(ctx context.Context, p seedIssuePlan) error {
	res := s.api.call(ctx, "GET", "/api/v1/issues/"+p.Key, seedWho(), nil)
	if !res.ok() {
		return &seedError{step: "issue-read", logged: p.Key, status: res.status, code: dispatchCode(res.body)}
	}
	var cur liveIssue
	if err := json.Unmarshal(res.body, &cur); err != nil {
		return fmt.Errorf("issue-read %s: undecodable response", p.Key)
	}
	if patch := issuePatch(cur, p.Issue); patch != nil {
		patch["actor"] = sessionActorAny(replaySeedActorID)
		if _, err := s.write(ctx, "patch", p.Key, 0, "", "patch", p.Key, "PATCH", "/api/v1/issues/"+p.Key, seedWho(), jsonBody(patch)); err != nil {
			return err
		}
	}
	if claimWanted(cur, p.Issue) {
		w, actor, err := actorOf(&p.Issue.Claim.Actor)
		if err != nil {
			return fmt.Errorf("claim %s: %w", p.Key, err)
		}
		if _, err := s.write(ctx, "claim", p.Key, 0, "", "claim", p.Key, "POST", "/api/v1/issues/"+p.Key+"/claim", w, jsonBody(withActor(map[string]any{}, actor))); err != nil {
			return err
		}
	}
	return nil
}

// --- replay_ids ----------------------------------------------------------------------------------

// idStore is loadtest.replay_ids, cached.
type idStore struct {
	pool *pgxpool.Pool
	mu   sync.Mutex
	m    map[[2]string]string
}

func loadIDStore(ctx context.Context, pool *pgxpool.Pool) (*idStore, error) {
	s := &idStore{pool: pool, m: map[[2]string]string{}}
	rows, err := pool.Query(ctx, `select kind, logged_id, local_id from loadtest.replay_ids`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, l, v string
		if err := rows.Scan(&k, &l, &v); err != nil {
			return nil, err
		}
		s.m[[2]string{k, l}] = v
	}
	return s, rows.Err()
}

func (s *idStore) get(kind, logged string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[[2]string{kind, logged}]
	return v, ok
}

func (s *idStore) put(ctx context.Context, kind, logged, local string) error {
	if _, err := s.pool.Exec(ctx, `insert into loadtest.replay_ids (kind, logged_id, local_id) values ($1, $2, $3) on conflict do nothing`, kind, logged, local); err != nil {
		return fmt.Errorf("record %s %s: %w", kind, logged, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[[2]string{kind, logged}] = local
	return nil
}

func (s *idStore) locals(kind string) map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]bool{}
	for k, v := range s.m {
		if k[0] == kind {
			out[v] = true
		}
	}
	return out
}

func (s *idStore) count(kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k := range s.m {
		if k[0] == kind {
			n++
		}
	}
	return n
}

func (s *idStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}
