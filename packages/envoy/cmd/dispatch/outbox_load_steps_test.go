//go:build multiprocess

// The outbox's load steps: the before/after numbers for Dispatch's outbox, taken from the outside.
// Real envoy-dispatch binaries on one Postgres and one NATS server; every event is a comment
// written through a process's HTTP API, and every measurement is taken from what a test session
// receives over NATS. Nothing here reads the database, /healthz or anything else a build could add,
// so the same file measures any build of the outbox.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"text/tabwriter"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	envoystore "github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	// loadStepsEnv set to 1 runs TestTheOutboxLoadSteps. It takes minutes of load, so CI never
	// runs it.
	loadStepsEnv = "DISPATCH_OUTBOX_LOAD_STEPS"
	// loadStepsOutEnv names a file TestTheOutboxLoadSteps writes its results to as JSON.
	loadStepsOutEnv = "DISPATCH_OUTBOX_LOAD_STEPS_OUT"
	// loadStepIssues is how many issues a step's comments go to, round robin. The first of them is
	// routed to a role.
	loadStepIssues = 40
	// loadStepLength is how long a step writes for.
	loadStepLength = 60 * time.Second
	// loadDeliveryWithin is how long after its last write a step or a burst waits for every written
	// event to reach every destination it is addressed to, and how long a step's setup waits for
	// its own events.
	loadDeliveryWithin = 60 * time.Second
	// loadSetupQuiet is how long nothing may arrive before a step starts writing, so the events of
	// its own setup (the issues, the route) are not still being published under the step's load.
	loadSetupQuiet = 2 * time.Second
	// loadBurstComments is how many comments a burst writes on its one issue.
	loadBurstComments = 100
	// loadWritesInFlight bounds the writes a step has waiting on an answer. A step whose writes
	// back up past it writes slower than its target, which its written rate shows.
	loadWritesInFlight = 32
	// loadMachineID is the machine the role holder registers from.
	loadMachineID = "outbox-load-steps"
)

// loadStepRates are the write rates of the steps, in events a second: production's evening load
// (4 a second) and ten times it.
var loadStepRates = []int{4, 40}

// Each step writes comments at a fixed rate for a minute, with one process and then with two, on
// 40 issues, one of which is routed to a role whose holder is the test's own session. A test
// session receives every event on the issues' topic families, on the holder's agent subject (where
// it answers a request with the empty receipt a session plugin sends) and on the role's subject,
// and counts each event once, by its envelope's event id, when it first arrives. A burst then
// writes 100 comments on one issue as fast as one process takes them.
//
// Per step it reports the rate written, the publish rate (distinct events first received while the
// step was writing, per second), the latency from the event's commit (the envelope's IssuedAt) to
// its first arrival, p50 and p99, and how many written events had not reached every destination a
// minute after the step's last write, which fails the test. Per burst it reports the drain time:
// from the first write to the last arrival.
func TestTheOutboxLoadSteps(t *testing.T) {
	if os.Getenv(loadStepsEnv) != "1" {
		t.Skipf("set %s=1 to run the outbox load steps; they take minutes and CI does not run them", loadStepsEnv)
	}
	report := loadReport{CPUs: runtime.NumCPU()}
	t.Logf("runtime.NumCPU() = %d", report.CPUs)

	pair := newPair(t)
	rig := newLoadRig(t, pair.nats)
	options := dispatchOptions{NATSURL: pair.nats, SigningKey: multiprocessSigningKey}

	pair.a = startDispatchProcessWith(t, pair.databaseURL(t), options)
	pair.a.waitHealthy(t)
	one := []*dispatchProcess{pair.a}
	for _, rate := range loadStepRates {
		report.Steps = append(report.Steps, rig.step(t, one, rate))
	}
	report.Bursts = append(report.Bursts, rig.burst(t, one))

	pair.b = startDispatchProcessWith(t, pair.databaseURL(t), options)
	pair.b.waitHealthy(t)
	two := []*dispatchProcess{pair.a, pair.b}
	for _, rate := range loadStepRates {
		report.Steps = append(report.Steps, rig.step(t, two, rate))
	}
	report.Bursts = append(report.Bursts, rig.burst(t, two))

	t.Logf("outbox load steps on %d CPUs:\n%s", report.CPUs, report.table())
	if out := os.Getenv(loadStepsOutEnv); out != "" {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Fatalf("encode the results: %v", err)
		}
		if err := os.WriteFile(out, append(encoded, '\n'), 0o644); err != nil {
			t.Fatalf("write the results to %s: %v", out, err)
		}
	}
}

// ───── the results ─────────────────────────────────────────────────────────

// loadReport is every step's and every burst's results, and the machine's core count.
type loadReport struct {
	CPUs   int               `json:"cpus"`
	Steps  []loadStepResult  `json:"steps"`
	Bursts []loadBurstResult `json:"bursts"`
}

// loadStepResult is one step's results. Received counts the written events that reached every
// destination they are addressed to by the end of the delivery wait; PublishRate counts the
// distinct written events first received while the step was writing, per second of writing.
type loadStepResult struct {
	Processes    int     `json:"processes"`
	TargetRate   int     `json:"target_events_per_second"`
	WriteSeconds float64 `json:"write_seconds"`
	Written      int     `json:"written"`
	WriteErrors  int     `json:"write_errors"`
	WriteRate    float64 `json:"written_per_second"`
	PublishRate  float64 `json:"published_per_second"`
	Received     int     `json:"received"`
	Undelivered  int     `json:"undelivered"`
	RoleWritten  int     `json:"role_written"`
	RoleReceived int     `json:"role_received"`
	LatencyP50   float64 `json:"latency_p50_ms"`
	LatencyP99   float64 `json:"latency_p99_ms"`
}

// loadBurstResult is one burst's results. Drain is from the burst's first write to the last
// arrival of any of its events on any of their destinations.
type loadBurstResult struct {
	Processes   int     `json:"processes"`
	Written     int     `json:"written"`
	WriteErrors int     `json:"write_errors"`
	Received    int     `json:"received"`
	Undelivered int     `json:"undelivered"`
	DrainMS     float64 `json:"drain_ms"`
	LatencyP50  float64 `json:"latency_p50_ms"`
	LatencyP99  float64 `json:"latency_p99_ms"`
}

// table is the report as one aligned table, a row per step and per burst.
func (r loadReport) table() string {
	var out strings.Builder
	w := tabwriter.NewWriter(&out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "run\tprocesses\ttarget/s\twritten\twritten/s\tpublished/s\treceived\tundelivered\trole received\tp50 ms\tp99 ms\tdrain ms")
	for _, s := range r.Steps {
		fmt.Fprintf(w, "step %d/s\t%d\t%d\t%d\t%.2f\t%.2f\t%d\t%d\t%d/%d\t%.1f\t%.1f\t-\n",
			s.TargetRate, s.Processes, s.TargetRate, s.Written, s.WriteRate, s.PublishRate,
			s.Received, s.Undelivered, s.RoleReceived, s.RoleWritten, s.LatencyP50, s.LatencyP99)
	}
	for _, b := range r.Bursts {
		fmt.Fprintf(w, "burst %d on one issue\t%d\t-\t%d\t-\t-\t%d\t%d\t%d/%d\t%.1f\t%.1f\t%.0f\n",
			loadBurstComments, b.Processes, b.Written, b.Received, b.Undelivered, b.Received, b.Written,
			b.LatencyP50, b.LatencyP99, b.DrainMS)
	}
	_ = w.Flush()
	return out.String()
}

// ───── the rig ─────────────────────────────────────────────────────────────

// loadRig is the test session's NATS connection, the registries a role holder is written to, and
// the HTTP client the comments are written through.
type loadRig struct {
	conn     *natsgo.Conn
	roles    *envoystore.Registry
	sessions *session.SessionRegistry
	client   *http.Client
}

func newLoadRig(t *testing.T, natsURL string) *loadRig {
	t.Helper()
	conn := testnats.Connect(t, natsURL)
	roles, err := envoystore.Open(conn)
	if err != nil {
		t.Fatalf("open the role registry: %v", err)
	}
	t.Cleanup(roles.StopWatch)
	sessions, err := session.OpenSessionRegistry(conn)
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = loadWritesInFlight
	t.Cleanup(transport.CloseIdleConnections)
	return &loadRig{
		conn:     conn,
		roles:    roles,
		sessions: sessions,
		client:   &http.Client{Transport: transport, Timeout: 30 * time.Second},
	}
}

// loadTarget is the project one step or burst writes to: its issues, the first of which is routed
// to role, whose holder is the test session, and the recorder of what the session received.
type loadTarget struct {
	issues   []string
	role     string
	recorder *loadRecorder
	subs     []*natsgo.Subscription
}

// prepare creates a fresh project with issues issues through writer, subscribes the test session
// to their topic families, to the holder's agent subject and to the role's subject, registers the
// holder, routes the first issue to the role, and returns once the setup's own events are
// published.
func (r *loadRig) prepare(t *testing.T, writer *dispatchProcess, issues int) *loadTarget {
	t.Helper()
	project := uniqueProjectKey(t, "LS")
	writer.createProject(t, project, "Outbox load "+project)
	target := &loadTarget{role: "load-" + strings.ToLower(project)}
	for index := range issues {
		key, _ := writer.createIssueIn(t, project, fmt.Sprintf("Load %d", index+1), "Load.")
		target.issues = append(target.issues, key)
	}
	holder := "load-holder-" + strings.ToLower(project)
	target.recorder = &loadRecorder{roleIssue: target.issues[0], events: map[string]*loadArrival{}, seen: map[string]bool{}}

	subscribe := func(subject string, destination loadDestination) {
		sub, err := r.conn.Subscribe(subject, target.recorder.receive(destination))
		if err != nil {
			t.Fatalf("subscribe to %s: %v", subject, err)
		}
		target.subs = append(target.subs, sub)
	}
	for _, key := range target.issues {
		subscribe("notifications.dispatch.issue."+key+".>", loadTopic)
	}
	subscribe(contracts.AgentSubject(holder), loadRole)
	subscribe(contracts.RoleTopicPrefix+target.role, loadRole)
	t.Cleanup(target.unsubscribe)
	if err := r.conn.Flush(); err != nil {
		t.Fatalf("flush the test session's subscriptions: %v", err)
	}
	r.registerHolder(t, target.role, holder)

	writer.call(t, http.MethodPatch, "/api/v1/issues/"+target.issues[0],
		fmt.Sprintf(`{"route":"role:%s"}`, target.role), http.StatusOK)
	target.recorder.waitForSetup(t, target.issues[0])
	return target
}

// registerHolder makes holder role's holder as a session plugin's own registration does: the
// claim in the role-claim bucket, and a self-subscribed session with no port, updated now, in the
// session bucket.
func (r *loadRig) registerHolder(t *testing.T, role, holder string) {
	t.Helper()
	if _, err := r.roles.SetRole(holder, loadMachineID, role, false); err != nil {
		t.Fatalf("claim role %s for %s: %v", role, holder, err)
	}
	entry := session.SessionEntry{
		MachineID:      loadMachineID,
		Title:          "Outbox load steps",
		UpdatedAt:      time.Now().UnixMilli(),
		SelfSubscribed: true,
	}
	if err := r.sessions.Put(holder, entry); err != nil {
		t.Fatalf("register session %s: %v", holder, err)
	}
}

// unsubscribe ends the target's subscriptions; a step's session stops receiving once it is done.
func (target *loadTarget) unsubscribe() {
	for _, sub := range target.subs {
		_ = sub.Unsubscribe()
	}
	target.subs = nil
}

// loadWrite is one comment written: its id, or the error that kept it from being written.
type loadWrite struct {
	commentID string
	err       error
}

// postComment writes one comment on issue key through p, as the test's login, and returns its id.
func (r *loadRig) postComment(p *dispatchProcess, key, body string) loadWrite {
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return loadWrite{err: err}
	}
	request, err := http.NewRequest(http.MethodPost, p.url("/api/v1/issues/"+key+"/comments"), bytes.NewReader(payload))
	if err != nil {
		return loadWrite{err: err}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", dispatchTestLogin)
	response, err := r.client.Do(request)
	if err != nil {
		return loadWrite{err: err}
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusCreated {
		return loadWrite{err: fmt.Errorf("comment on %s: status %d: %s", key, response.StatusCode, answer)}
	}
	var comment struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(answer, &comment); err != nil || comment.ID == "" {
		return loadWrite{err: fmt.Errorf("comment on %s: no id in %s: %v", key, answer, err)}
	}
	return loadWrite{commentID: comment.ID}
}

// written is the ids of the writes that succeeded; the failures are reported as errors on t,
// counted and with the first of them.
func written(t *testing.T, what string, writes []loadWrite) (ids []string, failures int) {
	t.Helper()
	var first error
	for _, write := range writes {
		if write.err != nil {
			failures++
			if first == nil {
				first = write.err
			}
			continue
		}
		ids = append(ids, write.commentID)
	}
	if failures > 0 {
		t.Errorf("%s: %d of %d writes failed; the first: %v", what, failures, len(writes), first)
	}
	return ids, failures
}

// ───── a step and a burst ──────────────────────────────────────────────────

// step writes comments at rate a second for loadStepLength on a fresh project's issues, round
// robin, through writers in turn: consecutive writes go to different processes, and with two
// processes each issue's writes go through each of them from one round to the next.
func (r *loadRig) step(t *testing.T, writers []*dispatchProcess, rate int) loadStepResult {
	t.Helper()
	what := fmt.Sprintf("step %d/s with %d processes", rate, len(writers))
	target := r.prepare(t, writers[0], loadStepIssues)
	defer target.unsubscribe()

	interval := time.Second / time.Duration(rate)
	total := int(loadStepLength / interval)
	writes := make([]loadWrite, total)
	roleWrites := make([]bool, total)
	slots := make(chan struct{}, loadWritesInFlight)
	var inFlight sync.WaitGroup
	started := time.Now()
	for index := range total {
		if wait := time.Until(started.Add(time.Duration(index) * interval)); wait > 0 {
			time.Sleep(wait)
		}
		issue := index % len(target.issues)
		writer := writers[(index+index/len(target.issues))%len(writers)]
		roleWrites[index] = issue == 0
		slots <- struct{}{}
		inFlight.Go(func() {
			defer func() { <-slots }()
			writes[index] = r.postComment(writer, target.issues[issue], fmt.Sprintf("Load step comment %d.", index))
		})
	}
	inFlight.Wait()
	finished := time.Now()

	ids, failures := written(t, what, writes)
	roleIDs := make(map[string]bool)
	for index, write := range writes {
		if roleWrites[index] && write.err == nil {
			roleIDs[write.commentID] = true
		}
	}
	target.recorder.waitForDelivery(ids, finished.Add(loadDeliveryWithin))
	outcome := target.recorder.outcome(ids, started, finished)
	outcome.requireDelivered(t, what, len(ids))

	roleReceived := 0
	for id := range roleIDs {
		if outcome.delivered[id] {
			roleReceived++
		}
	}
	seconds := finished.Sub(started).Seconds()
	result := loadStepResult{
		Processes:    len(writers),
		TargetRate:   rate,
		WriteSeconds: seconds,
		Written:      len(ids),
		WriteErrors:  failures,
		WriteRate:    float64(len(ids)) / seconds,
		PublishRate:  float64(outcome.firstInWindow) / seconds,
		Received:     len(outcome.delivered),
		Undelivered:  len(ids) - len(outcome.delivered),
		RoleWritten:  len(roleIDs),
		RoleReceived: roleReceived,
		LatencyP50:   percentileMS(outcome.latencies, 0.50),
		LatencyP99:   percentileMS(outcome.latencies, 0.99),
	}
	t.Logf("%s: %+v", what, result)
	return result
}

// burst writes loadBurstComments comments on one fresh, role-routed issue, one after another as
// fast as the first of writers answers, while every process of writers runs.
func (r *loadRig) burst(t *testing.T, writers []*dispatchProcess) loadBurstResult {
	t.Helper()
	what := fmt.Sprintf("burst of %d comments on one issue with %d processes", loadBurstComments, len(writers))
	target := r.prepare(t, writers[0], 1)
	defer target.unsubscribe()

	writes := make([]loadWrite, loadBurstComments)
	started := time.Now()
	for index := range writes {
		writes[index] = r.postComment(writers[0], target.issues[0], fmt.Sprintf("Load burst comment %d.", index))
	}
	finished := time.Now()

	ids, failures := written(t, what, writes)
	target.recorder.waitForDelivery(ids, finished.Add(loadDeliveryWithin))
	outcome := target.recorder.outcome(ids, started, finished)
	outcome.requireDelivered(t, what, len(ids))
	drain := 0.0
	if !outcome.lastArrival.IsZero() {
		drain = float64(outcome.lastArrival.Sub(started)) / float64(time.Millisecond)
	}
	result := loadBurstResult{
		Processes:   len(writers),
		Written:     len(ids),
		WriteErrors: failures,
		Received:    len(outcome.delivered),
		Undelivered: len(ids) - len(outcome.delivered),
		DrainMS:     drain,
		LatencyP50:  percentileMS(outcome.latencies, 0.50),
		LatencyP99:  percentileMS(outcome.latencies, 0.99),
	}
	t.Logf("%s: %+v", what, result)
	return result
}

// ───── what the test session received ──────────────────────────────────────

// loadDestination is which of an event's destinations a message arrived on: its issue's topic
// family, or the role holder (the role's subject, or the holder's agent subject).
type loadDestination int

const (
	loadTopic loadDestination = iota
	loadRole
)

// loadArrival is one comment.created event as the test session received it.
type loadArrival struct {
	commentID string
	issue     string
	issuedAt  time.Time
	// first is its first arrival on any destination; topic and role are its first arrival on each.
	first, topic, role time.Time
}

// reached is when the event had reached every destination it is addressed to, and whether it
// has: its issue's topic family, and the role holder too for the role-routed issue's events.
func (a *loadArrival) reached(roleIssue string) (time.Time, bool) {
	if a.topic.IsZero() {
		return time.Time{}, false
	}
	if a.issue != roleIssue {
		return a.topic, true
	}
	if a.role.IsZero() {
		return time.Time{}, false
	}
	if a.role.After(a.topic) {
		return a.role, true
	}
	return a.topic, true
}

// loadRecorder keeps every comment.created event the test session received, by envelope event
// id, and which (issue, event type) pairs it has received at all.
type loadRecorder struct {
	roleIssue string

	mu     sync.Mutex
	events map[string]*loadArrival
	seen   map[string]bool
	last   time.Time
}

// receive records each message as it arrives on destination, and answers one that carries a
// reply subject with an empty message, the receipt a session plugin sends for a delivery.
func (r *loadRecorder) receive(destination loadDestination) natsgo.MsgHandler {
	return func(msg *natsgo.Msg) {
		at := time.Now()
		if msg.Reply != "" {
			_ = msg.Respond(nil)
		}
		r.record(destination, msg.Data, at)
	}
}

func (r *loadRecorder) record(destination loadDestination, data []byte, at time.Time) {
	var item contracts.Envelope
	if err := json.Unmarshal(data, &item); err != nil {
		return
	}
	var event struct {
		Type     string  `json:"type"`
		IssueKey *string `json:"issue_key"`
		Payload  struct {
			ID string `json:"id"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(item.Payload), &event); err != nil || event.IssueKey == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = at
	r.seen[*event.IssueKey+" "+event.Type] = true
	if event.Type != "comment.created" {
		return
	}
	arrival := r.events[item.EventID]
	if arrival == nil {
		arrival = &loadArrival{
			commentID: event.Payload.ID,
			issue:     *event.IssueKey,
			issuedAt:  time.UnixMilli(item.IssuedAt),
			first:     at,
		}
		r.events[item.EventID] = arrival
	}
	switch {
	case destination == loadTopic && arrival.topic.IsZero():
		arrival.topic = at
	case destination == loadRole && arrival.role.IsZero():
		arrival.role = at
	}
}

// waitForSetup waits for the route change on issue, the setup's last write, to arrive, and then
// for loadSetupQuiet with nothing arriving, so a step starts with its setup published.
func (r *loadRecorder) waitForSetup(t *testing.T, issue string) {
	t.Helper()
	deadline := time.Now().Add(loadDeliveryWithin)
	for {
		r.mu.Lock()
		routed, last := r.seen[issue+" issue.updated"], r.last
		r.mu.Unlock()
		if routed && time.Since(last) >= loadSetupQuiet {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the setup's events were not published within %v (route change on %s received: %v)",
				loadDeliveryWithin, issue, routed)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitForDelivery waits until every comment of ids has reached every destination it is addressed
// to, or until deadline.
func (r *loadRecorder) waitForDelivery(ids []string, deadline time.Time) {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	for time.Now().Before(deadline) {
		r.mu.Lock()
		delivered := 0
		for _, arrival := range r.events {
			if _, ok := arrival.reached(r.roleIssue); ok && want[arrival.commentID] {
				delivered++
			}
		}
		r.mu.Unlock()
		if delivered == len(want) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// loadOutcome is what the test session received of a step's or a burst's written comments.
type loadOutcome struct {
	// delivered is every written comment whose event reached every destination it is addressed to.
	delivered map[string]bool
	// arrived counts the written comments whose event arrived anywhere; roleShort counts those of
	// them that reached their issue's topic family but not the role holder.
	arrived, roleShort int
	// firstInWindow counts the written comments whose event first arrived between the first write
	// and the last write's answer.
	firstInWindow int
	// latencies are commit-to-first-arrival times, sorted.
	latencies []time.Duration
	// lastArrival is the latest time any written comment's event reached a destination.
	lastArrival time.Time
}

func (r *loadRecorder) outcome(ids []string, started, finished time.Time) loadOutcome {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	outcome := loadOutcome{delivered: map[string]bool{}}
	for _, arrival := range r.events {
		if !want[arrival.commentID] {
			continue
		}
		outcome.arrived++
		outcome.latencies = append(outcome.latencies, arrival.first.Sub(arrival.issuedAt))
		if !arrival.first.Before(started) && !arrival.first.After(finished) {
			outcome.firstInWindow++
		}
		if at, ok := arrival.reached(r.roleIssue); ok {
			outcome.delivered[arrival.commentID] = true
			if at.After(outcome.lastArrival) {
				outcome.lastArrival = at
			}
		} else if !arrival.topic.IsZero() {
			outcome.roleShort++
		}
		for _, at := range []time.Time{arrival.topic, arrival.role} {
			if at.After(outcome.lastArrival) {
				outcome.lastArrival = at
			}
		}
	}
	slices.Sort(outcome.latencies)
	return outcome
}

// requireDelivered fails the test when any of the written comments' events had not reached every
// destination it is addressed to within loadDeliveryWithin of the last write, naming how many.
func (o loadOutcome) requireDelivered(t *testing.T, what string, written int) {
	t.Helper()
	if missing := written - len(o.delivered); missing > 0 {
		t.Errorf("%s: %d of %d written events did not reach every destination within %v of the last write: "+
			"%d never arrived, %d reached their issue's topic family but not the role holder",
			what, missing, written, loadDeliveryWithin, written-o.arrived, o.roleShort)
	}
}

// percentileMS is the nearest-rank p-th percentile of sorted, in milliseconds, or 0 for none.
func percentileMS(sorted []time.Duration, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := min(max(int(math.Ceil(p*float64(len(sorted))))-1, 0), len(sorted)-1)
	return float64(sorted[rank]) / float64(time.Millisecond)
}
