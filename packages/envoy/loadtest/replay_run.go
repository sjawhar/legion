package main

// Sending a replay plan (loadgen run -source replay): the setup loadgen run takes from the bundle
// before the step (replaySetup), the step that sends it, id resolution through loadtest.replay_ids
// and the run's own responses, and one connection and one 60 s deadline per action. The plan it
// sends is pure and lives in replay_plan.go; nothing here changes it apart from readBaseTexts.
//
// Nothing here logs a body, title, quote or document text: routes are templates, failures are
// statuses, counts are counts.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// --- sending ---------------------------------------------------------------------------------------

// replaySetup is what loadgen run takes from the bundle before the step: the plan, the roster and
// the inputs record.
type replaySetup struct {
	opt    replayOptions
	plan   *replayPlan
	inputs replayInputs
	// bundle is the plan's bundle, kept so the run can replan it against the build's own
	// document texts (readBaseTexts).
	bundle *replayBundle
	// roster is the session ids registered with the listener: the bundle's roster ids the listener
	// accepts as keys (natsKey), then synthetic ones; rosterSkipped counts the roster ids it would
	// refuse (an actor id such as legion-daemon:LINUX is no session id); homes maps a roster session
	// to the kept issue its claim holds at the instant.
	roster        []string
	rosterSkipped int
	homes         map[string]string
}

// natsKeyAlphabet is the alphabet of a NATS KV key nats.go accepts (its validKeyRe); a key also
// neither starts nor ends with a dot. The listener keys a session by its id and refuses any other
// (internal/bus ErrInvalidKey).
var natsKeyAlphabet = regexp.MustCompile(`^[-/_=.a-zA-Z0-9]+$`)

// natsKey says whether the listener accepts id as a session's key.
func natsKey(id string) bool {
	return natsKeyAlphabet.MatchString(id) && !strings.HasPrefix(id, ".") && !strings.HasSuffix(id, ".")
}

func newReplaySetup(o replayOptions) (*replaySetup, error) {
	b, p, err := loadReplay(o)
	if err != nil {
		return nil, err
	}
	s := &replaySetup{opt: o, plan: p, inputs: o.inputsRecord(b), bundle: b, homes: map[string]string{}}
	for _, r := range b.Roster {
		if len(s.roster) == o.roster {
			break
		}
		if !natsKey(r.SessionID) {
			s.rosterSkipped++
			continue
		}
		s.roster = append(s.roster, r.SessionID)
	}
	for i := len(s.roster); i < o.roster; i++ {
		s.roster = append(s.roster, fmt.Sprintf("ses-replay-%04d", i))
	}
	keys := map[string]bool{}
	for _, k := range p.IssueKeys {
		keys[k] = true
	}
	for i := range b.State.Issues {
		is := &b.State.Issues[i]
		if is.Exists && is.Claim != nil && is.Claim.Actor.Kind == "session" && keys[is.Key] {
			if _, ok := s.homes[is.Claim.Actor.ID]; !ok {
				s.homes[is.Claim.Actor.ID] = is.Key
			}
		}
	}
	if len(p.IssueKeys) == 0 {
		// No existing issue kept: the sessions watch the bundle's issues instead.
		for i := range b.State.Issues {
			if b.State.Issues[i].Exists {
				p.IssueKeys = append(p.IssueKeys, b.State.Issues[i].Key)
			}
		}
	}
	return s, nil
}

// newReplayStep lays out the roster: session i registers and subscribes its home issue (the issue
// its claim holds, else issue i mod the kept issues) and further issues 37 apart until its
// registration body reaches about replayRosterEntryBytes.
func newReplayStep(cfg runConfig, lg *log.Logger, nonce int64) *step {
	rs := cfg.replay
	sf := seedFile{Projects: []string{rs.plan.Project}}
	index := map[string]int{}
	for i, k := range rs.plan.IssueKeys {
		sf.Issues = append(sf.Issues, seedIssue{Key: k, Project: rs.plan.Project})
		index[k] = i
	}
	n := len(sf.Issues)
	st := &step{cfg: cfg, sf: sf, lg: lg, nonce: nonce, active: n, sessions: make([]session, len(rs.roster)), watchers: make([][]int, n)}
	for i, id := range rs.roster {
		if n == 0 {
			st.sessions[i] = session{id: id}
			continue
		}
		home := i % n
		if k, ok := rs.homes[id]; ok {
			home = index[k]
		}
		s := session{id: id, home: home, watched: []int{home}}
		size := len(st.registrationBody(s))
		for k := 1; k < n && size < replayRosterEntryBytes; k++ {
			w := (home + k*37) % n
			if slices.Contains(s.watched, w) {
				continue
			}
			next := append(slices.Clone(s.watched), w)
			nb := len(st.registrationBody(session{id: id, watched: next}))
			if nb > replayRosterEntryBytes+20 {
				break
			}
			s.watched, size = next, nb
		}
		for _, w := range s.watched {
			st.watchers[w] = append(st.watchers[w], i)
		}
		st.sessions[i] = s
	}
	return st
}

// replayFuture is a logged object the run creates: done closes once its action has finished.
type replayFuture struct {
	done  chan struct{}
	local string
	ok    bool
}

// replayTurn is an action's place in its document's write order: its write waits for prev (the
// previous planned write on the document; nil for the first), and done closes once it returned.
type replayTurn struct {
	prev <-chan struct{}
	done chan struct{}
}

// replayTurns chains each document's write actions in plan order.
func replayTurns(actions []*replayAction) map[*replayAction]*replayTurn {
	turns := map[*replayAction]*replayTurn{}
	last := map[string]chan struct{}{}
	for _, a := range actions {
		if !a.Write || a.Doc == "" {
			continue
		}
		t := &replayTurn{done: make(chan struct{}), prev: last[a.Doc]}
		last[a.Doc] = t.done
		turns[a] = t
	}
	return turns
}

// replayRun is one run's replay state.
type replayRun struct {
	setup   *replaySetup
	ids     map[[2]string]string // loadtest.replay_ids
	futures map[replayRef]*replayFuture
	pid     int
	pprof   string
	pg      string // the stack's Postgres URL
	out     string
	lg      *log.Logger
	// turns orders each document's writes (replayTurns); read-only once the run starts.
	turns map[*replayAction]*replayTurn

	mu      sync.Mutex
	writes  map[string]*replayWriteCount
	skipped map[string]int
	subs    replaySubstitutes
	edits   replayEdits
	// docWaits are the waits (s) of the writes that found the previous write on their document
	// still running; wrote the logged documents written to; endState what readEndState read.
	docWaits []float64
	wrote    map[string]bool
	endState *replayEndState
	// baseDocs records how the client's copy of the documents was matched to the build under
	// test (readBaseTexts); nil when the plan modelled no document.
	baseDocs *replayBaseDocs

	inflight      atomic.Int64
	timeouts      atomic.Int64
	inflightAtEnd int
	cpu           replayCPU
	meta          replayMetaRecord
	endRead       chan struct{}
}

// prepareReplay reads loadtest.replay_ids over the stack's Postgres URL, matches the client's copy
// of each document to the build under test (readBaseTexts) and records the run's inputs: the seed
// dump, Dispatch's GOMAXPROCS and build. It runs before the window opens, so its reads and any
// replan cost the measurement nothing.
func (st *step) prepareReplay(ctx context.Context, a *api) error {
	rs := st.cfg.replay
	rr := &replayRun{setup: rs, futures: map[replayRef]*replayFuture{}, pid: st.cfg.ep.pids["dispatch"], pprof: st.cfg.ep.pprof, pg: st.cfg.ep.pg,
		out: st.cfg.out, lg: st.lg, writes: map[string]*replayWriteCount{}, skipped: map[string]int{}, endRead: make(chan struct{}),
		wrote: map[string]bool{}}
	conn, err := pgx.Connect(ctx, st.cfg.ep.pg)
	if err != nil {
		return fmt.Errorf("replay: postgres: %w", err)
	}
	rr.ids, err = readReplayIDs(ctx, conn)
	conn.Close(context.Background())
	if err != nil {
		return fmt.Errorf("replay: read loadtest.replay_ids (is the replay seed restored?): %w", err)
	}
	if err := rr.readBaseTexts(ctx, a); err != nil {
		return err
	}
	rr.turns = replayTurns(rs.plan.Actions)
	for _, act := range rs.plan.Actions {
		if act.Write {
			wc := rr.writes[act.Kind]
			if wc == nil {
				wc = &replayWriteCount{}
				rr.writes[act.Kind] = wc
			}
			wc.Planned++
		}
		for _, stg := range act.Stages {
			for _, r := range stg {
				for _, p := range r.Produces {
					rr.futures[p.Ref] = &replayFuture{done: make(chan struct{})}
				}
			}
		}
	}
	rr.meta = collectReplayMeta(rs.inputs, rr.pid, loadtestRoot())
	st.lg.Printf("replay: %d actions (%d writes, %d reads), %d requests, %d issues kept; %d logged ids mapped; GOMAXPROCS %d",
		rs.plan.Counts.Actions, rs.plan.Counts.WriteActions, rs.plan.Counts.ReadActions, rs.plan.Counts.Requests, rs.plan.Counts.IssuesKept,
		len(rr.ids), rr.meta.Inputs.GOMAXPROCS)
	st.replay = rr
	return nil
}

// readBaseTexts matches the client's copy of each document the plan modelled to the build under
// test: it reads the document's current markdown (GET /api/v1/artifacts/{id}/text, the agent
// bearer) and replans against the texts that differ from the bundle's. A build renders a stored
// document its own way, so the markdown an older build served can hold a construct this one
// refuses (a table row whose cells past the table's width come from an unescaped pipe, say), and
// an upload sends the client's whole text; planning against the build's own text keeps the replay
// measuring the build instead of that difference. Where the build serves the bundle's text, every
// document included, nothing is replanned and the plan stays byte for byte the bundle's.
func (rr *replayRun) readBaseTexts(ctx context.Context, a *api) error {
	rs := rr.setup
	if len(rs.plan.BaseDocs) == 0 {
		return nil
	}
	rec := &replayBaseDocs{Documents: []string{}}
	texts := map[string]string{}
	for _, logged := range slices.Sorted(maps.Keys(rs.plan.BaseDocs)) {
		local, ok := rr.ids[[2]string{idArtifact, logged}]
		if !ok {
			rec.Unread++
			continue
		}
		res := a.callRetry(ctx, "GET", "/api/v1/artifacts/"+url.PathEscape(local)+"/text", who{}, nil)
		var got struct {
			Markdown string `json:"markdown"`
		}
		if !res.ok() || json.Unmarshal(res.body, &got) != nil {
			rec.Unread++
			rr.lg.Printf("replay: base text of document %s not read: status %d %s %v", logged, res.status, res.errorCode(), res.err)
			continue
		}
		rec.Read++
		if got.Markdown != rs.plan.BaseDocs[logged] {
			rec.Differed++
			rec.Documents = append(rec.Documents, logged)
			texts[logged] = got.Markdown
		}
	}
	if rec.Read == 0 {
		return fmt.Errorf("replay: no document of the plan's %d could be read from Dispatch (base texts)", len(rs.plan.BaseDocs))
	}
	if len(texts) > 0 {
		rs.opt.docTexts = texts
		p, err := planReplay(rs.bundle, rs.opt)
		if err != nil {
			return fmt.Errorf("replay: replan against the build's document texts: %w", err)
		}
		// The sample of issues does not depend on a document's text, and the roster already
		// subscribes the setup's keys (newReplaySetup's fallback included), so they are kept.
		p.IssueKeys = rs.plan.IssueKeys
		rs.plan, rec.Replanned = p, true
	}
	rr.baseDocs = rec
	rr.lg.Printf("replay: base texts of %d documents read (%d unread), %d differ from the bundle, replanned %v",
		rec.Read, rec.Unread, rec.Differed, rec.Replanned)
	return nil
}

// runReplay sends the plan: at the window start one /proc read of Dispatch and heap-start.pprof,
// then each action at winStart + (offset - from) on its own goroutine; at the window end
// heap-end.pprof and a /proc read, then the drain, then the end state of the documents written.
// done closes once every action has returned and the drain and the end state are read.
func (st *step) runReplay(ctx context.Context, a *api) (load *requestLoad, done <-chan struct{}) {
	rr := st.replay
	load = &requestLoad{recs: newCollector[reqRec](1 << 16)}
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		defer func() {
			select {
			case <-rr.endRead:
			default:
				close(rr.endRead)
			}
		}()
		idleFrom := st.winStart.Add(-replayIdleBefore)
		if idleFrom.Before(st.start) {
			idleFrom = st.start
		}
		if !sleepUntil(ctx, idleFrom) {
			return
		}
		rr.cpu.idle = rr.readCPU()
		if !sleepUntil(ctx, st.winStart) {
			return
		}
		rr.cpu.start = rr.readCPU()
		rr.meta.HeapStartAt = time.Now()
		fetchTo(rr.pprof+"/debug/pprof/heap", rr.out+"/heap-start.pprof", 60*time.Second, rr.lg)

		actionsDone := make(chan struct{})
		endDone := make(chan struct{})
		go func() {
			defer close(endDone)
			if !sleepUntil(ctx, st.winEnd) {
				close(rr.endRead)
				return
			}
			rr.meta.HeapEndAt = time.Now()
			fetchTo(rr.pprof+"/debug/pprof/heap", rr.out+"/heap-end.pprof", 60*time.Second, rr.lg)
			rr.cpu.end = rr.readCPU()
			rr.inflightAtEnd = int(rr.inflight.Load())
			close(rr.endRead)
			select {
			case <-actionsDone:
			case <-time.After(replayDrainCap):
			case <-ctx.Done():
			}
			rr.cpu.drain = rr.readCPU()
		}()
		var wg sync.WaitGroup
		from := rr.setup.opt.from
		for _, act := range rr.setup.plan.Actions {
			at := st.winStart.Add(act.Offset - from)
			if !sleepUntil(ctx, at) {
				break
			}
			wg.Go(func() { rr.do(ctx, a.base, a.token, act, at, load.recs) })
		}
		wg.Wait()
		close(actionsDone)
		<-endDone
		rr.readEndState(a)
	}()
	return load, closed
}

// readCPU is Dispatch's utime+stime now; ok is false without its pid or /proc entry.
func (rr *replayRun) readCPU() cpuReading {
	r := cpuReading{at: time.Now()}
	if rr.pid > 0 {
		if p, ok := readProcStat(rr.pid); ok {
			r.ticks, r.ok = p.utime+p.stime, true
		}
	}
	return r
}

// resolve is the local id of a logged object: the response of the run's own action that created
// it (waited for), else loadtest.replay_ids.
func (rr *replayRun) resolve(ctx context.Context, ref replayRef) (string, bool) {
	if f, ok := rr.futures[ref]; ok {
		select {
		case <-f.done:
			return f.local, f.ok
		case <-ctx.Done():
			return "", false
		}
	}
	v, ok := rr.ids[[2]string{ref.Kind, ref.Logged}]
	return v, ok
}

// settle publishes what the action created; on failure its objects are missing to dependents.
func (rr *replayRun) settle(act *replayAction, produced map[replayRef]string) {
	for _, stg := range act.Stages {
		for _, r := range stg {
			for _, p := range r.Produces {
				f := rr.futures[p.Ref]
				if f == nil {
					continue
				}
				select {
				case <-f.done:
					continue
				default:
				}
				f.local, f.ok = produced[p.Ref], produced[p.Ref] != ""
				close(f.done)
			}
		}
	}
}

// do sends one action, scheduled at at, on its own connection under one deadline over all its
// requests (at + replayActionDeadline), with no retry; the first failed request ends it. A write
// on a document first waits, after its reads, for the previous planned write on that document to
// return; that wait is inside the deadline, and the write's latency counts from at.
func (rr *replayRun) do(ctx context.Context, base, token string, act *replayAction, at time.Time, recs *collector[reqRec]) {
	rr.inflight.Add(1)
	defer rr.inflight.Add(-1)
	turn := rr.turns[act]
	if turn != nil {
		defer close(turn.done)
	}
	ctx, cancel := context.WithDeadline(ctx, at.Add(replayActionDeadline))
	defer cancel()
	produced := map[replayRef]string{}
	defer func() { rr.settle(act, produced) }()
	local := map[replayRef]string{}
	for _, ref := range act.refs() {
		v, ok := rr.resolve(ctx, ref)
		if !ok {
			rr.mu.Lock()
			rr.skipped[act.Kind]++
			rr.mu.Unlock()
			return
		}
		local[ref] = v
	}
	if act.Write {
		rr.mu.Lock()
		rr.writes[act.Kind].Sent++
		act.tally(&rr.subs, &rr.edits)
		rr.mu.Unlock()
	}
	tr := &http.Transport{DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, MaxIdleConnsPerHost: 4, IdleConnTimeout: replayActionDeadline}
	defer tr.CloseIdleConnections()
	ac := &api{base: base, token: token, client: &http.Client{Transport: tr}}
	var state replayActionState
	ok := true
	for _, stg := range act.Stages {
		from := int64(0) // when the stage's latency counts from; 0 for each request's arrival
		if turn != nil && writeStage(stg) {
			waited, alive := rr.awaitTurn(ctx, turn)
			turn = nil
			if !alive {
				ok = false
				break
			}
			if waited {
				from = at.UnixNano()
			}
			rr.mu.Lock()
			rr.wrote[act.Doc] = true
			rr.mu.Unlock()
		}
		results := make([]callResult, len(stg))
		if len(stg) == 1 {
			results[0] = rr.send(ctx, ac, stg[0], local, &state)
		} else {
			var wg sync.WaitGroup
			for i, r := range stg {
				wg.Go(func() { results[i] = rr.send(ctx, ac, r, local, &state) })
			}
			wg.Wait()
		}
		for i, res := range results {
			r := stg[i]
			start := res.arrival()
			if from != 0 {
				start = from
			}
			tolerated := !res.ok() && r.Tolerate != 0 && res.status == r.Tolerate && res.err == nil
			recs.add(reqRec{route: r.route(act.Kind), status: res.status, code: res.errorCode(), tolerated: tolerated, transport: res.err != nil,
				latency: time.Duration(res.end - start), connWait: res.connWait})
			if !res.ok() && !tolerated {
				ok = false
				continue
			}
			state.observe(res.body)
			for _, p := range r.Produces {
				if v := responseField(res.body, p.Field); v != "" {
					produced[p.Ref] = v
				}
			}
		}
		if !ok {
			break
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		rr.timeouts.Add(1)
		ok = false
	}
	if act.Write && !ok {
		rr.mu.Lock()
		rr.writes[act.Kind].Failed++
		rr.mu.Unlock()
	}
	if !ok {
		clear(produced)
	}
}

// writeStage says whether a stage sends a write (any request but a GET).
func writeStage(stg []*replayReq) bool {
	return slices.ContainsFunc(stg, func(r *replayReq) bool { return r.Method != "GET" })
}

// awaitTurn waits until the previous write on the action's document has returned: waited is
// whether it was still running (the wait is then counted in docWaits), alive false when the
// action's deadline came first.
func (rr *replayRun) awaitTurn(ctx context.Context, t *replayTurn) (waited, alive bool) {
	if t.prev == nil {
		return false, true
	}
	select {
	case <-t.prev:
		return false, true
	default:
	}
	t0 := time.Now()
	select {
	case <-t.prev:
		alive = true
	case <-ctx.Done():
	}
	rr.mu.Lock()
	rr.docWaits = append(rr.docWaits, time.Since(t0).Seconds())
	rr.mu.Unlock()
	return true, alive
}

// replayActionState is what an action's earlier responses tell its later requests.
type replayActionState struct {
	mu       sync.Mutex
	lastSeq  *int64
	editedAt json.RawMessage
}

func (s *replayActionState) observe(body []byte) {
	var r struct {
		LastSeq  *int64          `json:"last_seq"`
		EditedAt json.RawMessage `json:"edited_at"`
		Ask      *struct {
			EditedAt json.RawMessage `json:"edited_at"`
		} `json:"ask"`
	}
	if json.Unmarshal(body, &r) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.LastSeq != nil {
		s.lastSeq = r.LastSeq
	}
	if r.Ask != nil && len(r.Ask.EditedAt) > 0 {
		s.editedAt = r.Ask.EditedAt
	} else if len(r.EditedAt) > 0 && s.editedAt == nil {
		s.editedAt = r.EditedAt
	}
}

// responseField is a created object's id (idOf), key or primary_artifact_id in a response.
func responseField(body []byte, field string) string {
	if field == "id" {
		return idOf(body)
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	var s string
	_ = json.Unmarshal(m[field], &s)
	return s
}

// send renders one request (path, body, file) with the local ids and sends it.
func (rr *replayRun) send(ctx context.Context, ac *api, r *replayReq, local map[replayRef]string, state *replayActionState) callResult {
	path := r.Path
	for _, name := range slices.Sorted(maps.Keys(r.Params)) {
		path = strings.ReplaceAll(path, "{"+name+"}", renderArg(r.Params[name], local, state))
	}
	w := who{person: r.Person}
	switch {
	case r.File != nil:
		ct, payload, err := multipartFile(rr.setup.opt.rngSeed, r.File)
		if err != nil {
			return callResult{err: err}
		}
		return ac.callRaw(ctx, r.Method, path, w, ct, payload)
	case r.Body != nil:
		return ac.call(ctx, r.Method, path, w, renderBody(r.Body, local, state))
	}
	return ac.call(ctx, r.Method, path, w, nil)
}

func renderArg(a replayArg, local map[replayRef]string, state *replayActionState) string {
	switch {
	case a.After:
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.lastSeq == nil {
			return "0"
		}
		return strconv.FormatInt(max(0, *state.lastSeq-10), 10)
	case a.URI && a.Sub != nil:
		return url.QueryEscape("dispatch://" + local[*a.Ref] + "/comment/" + local[*a.Sub])
	case a.URI:
		return url.QueryEscape("dispatch://" + local[*a.Ref])
	case a.Ref != nil:
		return url.PathEscape(local[*a.Ref])
	}
	return a.Lit
}

// renderBody is the body with each logged reference replaced by its local id.
func renderBody(v any, local map[replayRef]string, state *replayActionState) any {
	switch x := v.(type) {
	case replayRef:
		return local[x]
	case replayPrevEditedAt:
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.editedAt == nil {
			return json.RawMessage("null")
		}
		return state.editedAt
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = renderBody(e, local, state)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = renderBody(e, local, state)
		}
		return out
	}
	return v
}

// multipartFile is the multipart body of a file upload: name, actor and the file's bytes, under a
// boundary drawn from the rng seed and the file's seed (multipart.NewWriter's own is random), so
// one plan sends one body.
func multipartFile(rngSeed uint64, f *replayFile) (string, []byte, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	boundary := fmt.Sprintf("replay%016x%016x", replayHash(rngSeed, "boundary", f.Seed), replayHash(rngSeed, "boundary2", f.Seed))
	if err := mw.SetBoundary(boundary); err != nil {
		return "", nil, err
	}
	_ = mw.WriteField("name", f.Name)
	if f.Actor != nil {
		actor, _ := json.Marshal(f.Actor)
		_ = mw.WriteField("actor", string(actor))
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", multipart.FileContentDisposition("file", f.Name))
	h.Set("Content-Type", f.MIME)
	part, err := mw.CreatePart(h)
	if err != nil {
		return "", nil, err
	}
	if _, err := part.Write(fileBytes(rngSeed, f.Seed, f.Size)); err != nil {
		return "", nil, err
	}
	if err := mw.Close(); err != nil {
		return "", nil, err
	}
	return mw.FormDataContentType(), buf.Bytes(), nil
}
