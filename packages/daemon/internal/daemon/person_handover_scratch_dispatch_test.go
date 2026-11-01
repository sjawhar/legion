package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// LEGION-755's proof that the daemon itself queues a person's handover: a person's legion label on
// an unrecorded root in triage, and a person's approval of a parked labelled root's primary
// document, each become one todo write of the daemon's (a dispatch_status outbox row marked
// personHandover), delivered under the daemon's own actor, so the root is recorded and admitted as
// a person's todo would be and no agent's write is needed. The rig is the blockers proof's
// (blockers_scratch_dispatch_test.go): the real Dispatch server with its events on, a real
// Postgres, a real NATS, and the daemon process-in-process reading Dispatch's events off that
// bus. A person's act is a request with the trusted X-Dispatch-User header; a session's is one
// with the scratch server's agent bearer naming its session in the body (dispatchCallAs). The
// daemon's decisions are read from GET /legion/v1/state, from the outbox rows it queued, from its
// JSON log, and from Dispatch's own event log, where its todo write carries its actor.
//
// Every negative — a write the daemon must not answer with a todo — is anchored on the daemon
// having applied the very event the act produced (processed_events, written in the fact's own
// transaction), on the triage notice row that same event queues, or, for a write Dispatch refused
// outright and made no event of, on the person's later write on the same issue, which the daemon
// answers in order; so "no todo" is read after the act was judged, never during a wait for it.

// scratchIdentity is who a request to the scratch Dispatch is from: the person `smoke`, through the
// trusted identity header the harness starts the server with, or a session, through the server's
// agent bearer with the session named in the body's actor, as Dispatch requires of a bearer caller.
type scratchIdentity int

const (
	asPerson scratchIdentity = iota
	asSession
)

func (identity scratchIdentity) String() string {
	if identity == asSession {
		return "the session " + scratchSession
	}
	return "the person smoke"
}

const (
	// scratchAgentToken is the agent bearer the scratch Dispatch is started with
	// (startScratchDispatchOn's DISPATCH_AGENT_TOKEN); a request a session makes carries it.
	scratchAgentToken = "scratch-dispatch-agent-token"
	// scratchSession is the session a bearer request names in its body's actor.
	scratchSession = "session-smoke"
)

// dispatchAnswerAs sends one request to the scratch Dispatch server as identity — the person's
// trusted header, or the session's agent bearer — with body as given, and answers its status and
// body, whatever they are: the request a proof expects Dispatch to refuse goes through here.
func dispatchAnswerAs(t *testing.T, baseURL, method, path string, body any, identity scratchIdentity) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s %s: %v", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, baseURL+path, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	request.Header.Set("Content-Type", "application/json")
	if identity == asSession {
		request.Header.Set("Authorization", "Bearer "+scratchAgentToken)
	} else {
		request.Header.Set("X-Dispatch-User", "smoke")
	}
	response, err := scratchDispatchClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s as %s: %v", method, path, identity, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the answer to %s %s: %v", method, path, err)
	}
	return response.StatusCode, answer
}

// withSessionActor is body, whatever shape it was given in, as an object naming scratchSession in
// its actor, which Dispatch's create, update, upload and edit routes require of a bearer caller
// (requireActor). A nil body is the actor alone.
func withSessionActor(t *testing.T, body any) map[string]any {
	t.Helper()
	fields := map[string]any{}
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode a session's request: %v", err)
		}
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatalf("a session's request body %s is not an object: %v", encoded, err)
		}
	}
	fields["actor"] = map[string]string{"kind": "session", "id": scratchSession}
	return fields
}

// dispatchCallAs is dispatchCall as identity: one request the scratch Dispatch must answer 200 or
// 201, its answer decoded into `into` when that is not nil. A session's write carries its session
// in the body's actor (withSessionActor), as every route a session writes through requires; a
// session's read sends no body at all.
func dispatchCallAs(t *testing.T, baseURL, method, path string, body, into any, identity scratchIdentity) {
	t.Helper()
	if identity == asSession && method != http.MethodGet {
		body = withSessionActor(t, body)
	}
	status, answer := dispatchAnswerAs(t, baseURL, method, path, body, identity)
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("%s %s as %s = %d: %s", method, path, identity, status, answer)
	}
	if into != nil {
		if err := json.Unmarshal(answer, into); err != nil {
			t.Fatalf("decode the answer to %s %s from %s: %v", method, path, answer, err)
		}
	}
}

// scratchActor is an actor as Dispatch's API serves it.
type scratchActor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// scratchIssue is an issue as the proofs read Dispatch's answer to a create, an update or a read.
type scratchIssue struct {
	Key               string   `json:"key"`
	Status            string   `json:"status"`
	Labels            []string `json:"labels"`
	PrimaryArtifactID string   `json:"primary_artifact_id"`
	LastSeq           int64    `json:"last_seq"`
}

// scratchEvent is one event of an issue's Dispatch event log: its id, which the envelope the
// daemon consumes carries as `dispatch-<id>` and the daemon records as applied under
// (processed_events), its sequence on the issue, its type, its actor and its payload.
type scratchEvent struct {
	ID      int64           `json:"id"`
	Seq     int64           `json:"seq"`
	Type    string          `json:"type"`
	Actor   scratchActor    `json:"actor"`
	Payload json.RawMessage `json:"payload"`
}

// status is the issue status the event's payload carries, "" for an event of another kind.
func (e scratchEvent) status() string {
	var payload struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(e.Payload, &payload)
	return payload.Status
}

// version is the document version the event's payload names, 0 for an event of another kind.
func (e scratchEvent) version() int {
	var payload struct {
		Version int `json:"version"`
	}
	_ = json.Unmarshal(e.Payload, &payload)
	return payload.Version
}

// factID is the event as the daemon's processed_events names it.
func (e scratchEvent) factID() string { return fmt.Sprintf("dispatch-%d", e.ID) }

// scratchReview is one review of a document as Dispatch's review routes serve it.
type scratchReview struct {
	Version int          `json:"version"`
	State   string       `json:"state"`
	Actor   scratchActor `json:"actor"`
	Reason  *string      `json:"reason"`
}

// personHandoverRig is one proof's rig: the scratch Dispatch, the daemon's configuration and
// database, the daemon once booted (boot), the fake runtime it launches on, and the JSON log it
// writes. The spec document every issue is created with gives its primary document a settled
// version 1 to approve.
type personHandoverRig struct {
	t       *testing.T
	baseURL string
	cfg     config.Config
	pool    *pgxpool.Pool
	logs    *syncBuffer
	github  string
	rt      *fake.Runtime
	d       *daemon
}

// personHandoverSpec is the spec every issue of these proofs is created with.
const personHandoverSpec = "# Plan\n\nThe service ships behind a flag.\n"

// newPersonHandoverRig takes the NATS server, starts the scratch Dispatch with its events on it,
// and readies the daemon's configuration, its database and the project without booting it: the
// boot proof writes to Dispatch before the first boot. admissionCap is the daemon's cap.
func newPersonHandoverRig(t *testing.T, admissionCap int) *personHandoverRig {
	t.Helper()
	// The NATS server is taken first: testnats.URL empties it of every stream, and Dispatch
	// reconciles ENVOY_NOTIFICATIONS on it on its first connection, which the daemon then consumes
	// from.
	natsURL := testnats.URL(t)
	baseURL, token := startScratchDispatchWithNATS(t, natsURL)
	cfg := workflowConfig(t, natsURL)
	// Dispatch takes a project key of at most ten characters; workflowConfig's is longer.
	cfg.Project = "P" + randomSuffix(t)[:9]
	cfg.Projects = map[string]config.Project{cfg.Project: {}}
	cfg.PostgresDSN = blockersDatabase(t)
	cfg.AdmissionCap = admissionCap
	// No controller tick runs: the first is an hour after the runtime starts (controllerFirstWake,
	// in boot) and the next an hour after that, so every decision below is an event's or the boot
	// listing's. The design gate stays at its default, root-issues, so a registered gate stays
	// registrable and closed until a person approves; EnvoyURL stays the rig's unreachable
	// default, so every notice row the daemon queues stays in its outbox for the proof to count.
	cfg.ControllerWakeInterval = time.Hour
	cfg.DispatchURL = baseURL
	if err := os.WriteFile(cfg.DispatchTokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write the scratch Dispatch bearer: %v", err)
	}
	dispatchCall(t, baseURL, http.MethodPost, "/api/v1/projects",
		map[string]string{"key": cfg.Project, "name": "A person's handover"}, nil)
	dispatchCall(t, baseURL, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets",
		map[string]any{"project": cfg.Project, "default": true}, nil)
	pool, err := pgxpool.New(t.Context(), cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the daemon's database: %v", err)
	}
	t.Cleanup(pool.Close)
	return &personHandoverRig{t: t, baseURL: baseURL, cfg: cfg, pool: pool, logs: &syncBuffer{}, github: blockersGitHub(t)}
}

// boot starts the daemon on the rig's configuration and database, on a fresh fake runtime, its log
// a JSON buffer at Debug the proofs read admission's lines from; a second boot appends to the same
// buffer, so a line counted across boots is counted once.
func (r *personHandoverRig) boot() {
	t := r.t
	t.Helper()
	rt := fake.NewRuntime()
	// A pods runtime provisions each claim's workspace itself, so the daemon clones nothing: these
	// issues name a repository that exists only in this test's GitHub stand-in.
	rt.InPod = true
	o := fakeRuntime(rt, &built{})
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = r.github
	o.controllerFirstWake = time.Hour
	r.rt = rt
	r.d = startDaemonLogging(t, r.cfg, o, slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
}

// create creates an issue of the rig's project through Dispatch's API as identity, with fields
// beside the project, the title and the spec, and answers it as Dispatch made it: in triage, where
// every Dispatch issue starts. Dispatch refuses a new issue whose title near-duplicates one in its
// project, so every title here shares no word with another.
func (r *personHandoverRig) create(identity scratchIdentity, title string, fields map[string]any) scratchIssue {
	r.t.Helper()
	body := map[string]any{"project": r.cfg.Project, "title": title, "spec": personHandoverSpec}
	maps.Copy(body, fields)
	var created scratchIssue
	dispatchCallAs(r.t, r.baseURL, http.MethodPost, "/api/v1/issues", body, &created, identity)
	if created.Key == "" || created.PrimaryArtifactID == "" {
		r.t.Fatalf("Dispatch created %q as %+v; want a key and a primary document", title, created)
	}
	return created
}

// issue reads key as Dispatch shows it now.
func (r *personHandoverRig) issue(key string) scratchIssue {
	r.t.Helper()
	var read scratchIssue
	dispatchCall(r.t, r.baseURL, http.MethodGet, "/api/v1/issues/"+key, nil, &read)
	return read
}

// patch edits key through Dispatch's API as identity and answers the issue as Dispatch shows it
// after the edit, its last_seq the sequence of the event the edit made.
func (r *personHandoverRig) patch(key string, identity scratchIdentity, fields map[string]any) scratchIssue {
	r.t.Helper()
	var patched scratchIssue
	dispatchCallAs(r.t, r.baseURL, http.MethodPatch, "/api/v1/issues/"+key, fields, &patched, identity)
	return patched
}

// addLabel puts labels, the whole set the issue is to carry, on key as identity.
func (r *personHandoverRig) addLabel(key string, identity scratchIdentity, labels ...string) scratchIssue {
	r.t.Helper()
	patched := r.patch(key, identity, map[string]any{"labels": labels})
	if got, want := slices.Sorted(slices.Values(patched.Labels)), slices.Sorted(slices.Values(labels)); !slices.Equal(got, want) {
		r.t.Fatalf("Dispatch answered the labels %v on %s with %v", labels, key, patched.Labels)
	}
	return patched
}

// setStatus moves key as the person at the dashboard does, and answers the issue after the move.
func (r *personHandoverRig) setStatus(key, status string) scratchIssue {
	r.t.Helper()
	patched := r.patch(key, asPerson, map[string]any{"status": status})
	if patched.Status != status {
		r.t.Fatalf("Dispatch answered the move of %s to %s with %s", key, status, patched.Status)
	}
	return patched
}

// comment comments on key as the person, as a note at the dashboard is: an event of the issue that
// is neither its creation nor an update of it.
func (r *personHandoverRig) comment(key, body string) {
	r.t.Helper()
	dispatchCall(r.t, r.baseURL, http.MethodPost, "/api/v1/issues/"+key+"/comments", map[string]any{"body": body}, nil)
}

// approveDocument reviews the latest settled version of artifactID as approved, as identity, and
// answers Dispatch's status with the review it wrote: 201 for the person's, and 403 HUMAN_ONLY for
// a bearer's, since Dispatch's review route is a person's alone. The body is the route's own,
// {state}, under either credential: the route takes no actor, so a bearer is refused for what it
// is, not for a field it did not expect.
func (r *personHandoverRig) approveDocument(artifactID string, identity scratchIdentity) (int, scratchReview) {
	r.t.Helper()
	status, answer := dispatchAnswerAs(r.t, r.baseURL, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/reviews",
		map[string]any{"state": "approved"}, identity)
	var review scratchReview
	if status == http.StatusCreated {
		if err := json.Unmarshal(answer, &review); err != nil {
			r.t.Fatalf("decode the review of %s from %s: %v", artifactID, answer, err)
		}
	}
	return status, review
}

// approve is the person's approval of artifactID, which Dispatch must take, answering the review.
func (r *personHandoverRig) approve(artifactID string) scratchReview {
	r.t.Helper()
	status, review := r.approveDocument(artifactID, asPerson)
	if status != http.StatusCreated || review.State != "approved" || review.Actor.Kind != "user" {
		r.t.Fatalf("the person's approval of %s = %d, review %+v; want 201 and an approved review in the person's name", artifactID, status, review)
	}
	return review
}

// reviews lists artifactID's reviews, newest first.
func (r *personHandoverRig) reviews(artifactID string) []scratchReview {
	r.t.Helper()
	var listed []scratchReview
	dispatchCall(r.t, r.baseURL, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/reviews", nil, &listed)
	return listed
}

// editDocument applies ops to artifactID as a session through Dispatch's edits route, which
// versions the document in the route's own transaction, and answers the version it wrote.
func (r *personHandoverRig) editDocument(artifactID string, ops ...map[string]string) int {
	r.t.Helper()
	var edited struct {
		Version *struct {
			Number int `json:"number"`
		} `json:"version"`
	}
	dispatchCallAs(r.t, r.baseURL, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{"ops": ops}, &edited, asSession)
	if edited.Version == nil {
		r.t.Fatalf("the edit of %s wrote no version", artifactID)
	}
	return edited.Version.Number
}

// revise replaces find with with in artifactID as a session and answers the new version.
func (r *personHandoverRig) revise(artifactID, find, with string) int {
	r.t.Helper()
	return r.editDocument(artifactID, map[string]string{"op": "replace", "find": find, "with": with})
}

// uploadDocument uploads a second document to key as a session and answers its id: a document of
// the issue that is not its primary one.
func (r *personHandoverRig) uploadDocument(key, name, content string) string {
	r.t.Helper()
	var uploaded struct {
		Artifact struct {
			ID      string `json:"id"`
			Primary bool   `json:"primary"`
		} `json:"artifact"`
	}
	dispatchCallAs(r.t, r.baseURL, http.MethodPost, "/api/v1/issues/"+key+"/artifacts", map[string]any{"name": name, "content": content}, &uploaded, asSession)
	if uploaded.Artifact.ID == "" || uploaded.Artifact.Primary {
		r.t.Fatalf("Dispatch uploaded %s to %s as %+v; want a second document with an id, not the primary one", name, key, uploaded.Artifact)
	}
	return uploaded.Artifact.ID
}

// personHandoverBlock is the decision block the carried case writes into a spec, and the option
// the person answers it with.
const (
	personHandoverBlock  = ":::ask{#wave urgency=\"med\" multiple=\"false\"}\nHow many regions take the first wave?\n\n- Two: The two with the fewest tenants.\n- Five: Half of them.\n:::\n"
	personHandoverAnswer = "Two"
)

// blockAsk waits for Dispatch's settlement to index the decision block blockID of key's document
// as an open ask and answers the ask's id.
func (r *personHandoverRig) blockAsk(key, blockID string) string {
	r.t.Helper()
	var found string
	testwait.Eventually(r.t, "Dispatch to index the decision block "+blockID+" of "+key, func() bool {
		var asks []struct {
			ID      string  `json:"id"`
			BlockID *string `json:"block_id"`
			State   string  `json:"state"`
		}
		dispatchCall(r.t, r.baseURL, http.MethodGet, "/api/v1/issues/"+key+"/asks?state=all", nil, &asks)
		for _, ask := range asks {
			if ask.BlockID != nil && *ask.BlockID == blockID && ask.State == "open" {
				found = ask.ID
				return true
			}
		}
		return false
	})
	return found
}

// answerBlock answers the decision block's ask as the person with selected.
func (r *personHandoverRig) answerBlock(askID, selected string) {
	r.t.Helper()
	dispatchCall(r.t, r.baseURL, http.MethodPost, "/api/v1/asks/"+askID+"/answer",
		map[string]any{"selected": []string{selected}, "expected_edited_at": nil}, nil)
}

// carriedReview waits for Dispatch to record its carry of the person's approval of version from on
// artifactID — the newest review, approved in the person's name at a later version with the
// carry's reason (intake.CarriedApprovalReasonPrefix) — and answers it.
func (r *personHandoverRig) carriedReview(artifactID string, from int) scratchReview {
	r.t.Helper()
	var newest scratchReview
	testwait.Eventually(r.t, fmt.Sprintf("Dispatch to carry the approval of version %d of %s forward", from, artifactID), func() bool {
		reviews := r.reviews(artifactID)
		if len(reviews) == 0 {
			return false
		}
		newest = reviews[0]
		return newest.Version > from && newest.Reason != nil && strings.HasPrefix(*newest.Reason, intake.CarriedApprovalReasonPrefix)
	})
	wantPrefix := fmt.Sprintf("%s %d:", intake.CarriedApprovalReasonPrefix, from)
	if newest.State != "approved" || newest.Actor != (scratchActor{Kind: "user", ID: "smoke"}) || !strings.HasPrefix(*newest.Reason, wantPrefix) {
		r.t.Fatalf("the carried review of %s = %+v (reason %q); want approved in the person's name with a reason opening %q", artifactID, newest, *newest.Reason, wantPrefix)
	}
	return newest
}

// issueEvents is key's whole Dispatch event log, oldest first, read with the agent token.
func (r *personHandoverRig) issueEvents(key string) []scratchEvent {
	r.t.Helper()
	var events []scratchEvent
	dispatchCallAs(r.t, r.baseURL, http.MethodGet, "/api/v1/issues/"+key+"/events", nil, &events, asSession)
	return events
}

// newestEvent is the newest event of kind on key, which the log must hold; want, when not nil,
// narrows it to the newest such event want accepts.
func (r *personHandoverRig) newestEvent(key, kind string, want func(scratchEvent) bool) scratchEvent {
	r.t.Helper()
	events := r.issueEvents(key)
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == kind && (want == nil || want(events[i])) {
			return events[i]
		}
	}
	r.t.Fatalf("the event log of %s holds no %s event the proof looks for among %+v", key, kind, events)
	return scratchEvent{}
}

// applied waits until the daemon has applied event — its processed_events row, which ApplyFact
// writes in the fact's own transaction, so once it reads, whatever the event queued, or did not,
// is committed for the proof to read.
func (r *personHandoverRig) applied(event scratchEvent) {
	r.t.Helper()
	testwait.Eventually(r.t, fmt.Sprintf("the daemon to apply Dispatch event %s (%s, seq %d)", event.factID(), event.Type, event.Seq), func() bool {
		var done bool
		if err := r.pool.QueryRow(r.t.Context(), `select exists (select 1 from processed_events where source = 'dispatch' and event_id = $1)`, event.factID()).
			Scan(&done); err != nil {
			r.t.Fatalf("read whether the daemon applied %s: %v", event.factID(), err)
		}
		return done
	})
}

// mirrored waits until the daemon's mirror of key (dispatch_issues) stands at or past seq, the
// sequence Dispatch gave an edit, and answers the status it mirrors.
func (r *personHandoverRig) mirrored(key string, seq int64) string {
	r.t.Helper()
	var status string
	var mirroredSeq int64
	testwait.Eventually(r.t, fmt.Sprintf("the daemon to mirror %s at Dispatch's sequence %d", key, seq), func() bool {
		if err := r.pool.QueryRow(r.t.Context(), `select status, dispatch_seq from dispatch_issues where key = $1`, key).Scan(&status, &mirroredSeq); err != nil {
			return false
		}
		return mirroredSeq >= seq
	})
	return status
}

// statusRows is every dispatch_status row of key still in the daemon's outbox, oldest first: a
// delivered row is deleted, so these are the writes still to run.
func (r *personHandoverRig) statusRows(key string) []record.StatusWrite {
	r.t.Helper()
	rows, err := r.pool.Query(r.t.Context(), `select payload from outbox where kind = 'dispatch_status' and issue = $1 order by id`, key)
	if err != nil {
		r.t.Fatalf("read the status writes queued on %s: %v", key, err)
	}
	defer rows.Close()
	var writes []record.StatusWrite
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			r.t.Fatalf("scan a status write queued on %s: %v", key, err)
		}
		var write record.StatusWrite
		if err := json.Unmarshal(payload, &write); err != nil {
			r.t.Fatalf("decode the status write %s queued on %s: %v", payload, key, err)
		}
		writes = append(writes, write)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read the status writes queued on %s: %v", key, err)
	}
	return writes
}

// personHandoverRows is the person's handover rows (StatusWrite.PersonHandover) still queued on key.
func (r *personHandoverRig) personHandoverRows(key string) []record.StatusWrite {
	r.t.Helper()
	var marked []record.StatusWrite
	for _, write := range r.statusRows(key) {
		if write.PersonHandover {
			marked = append(marked, write)
		}
	}
	return marked
}

// outboxRows counts the rows of kind on key in the daemon's outbox whose payload's kind is
// payloadKind: the notice rows, which stay queued since nothing answers the Envoy address the
// notice runner publishes to.
func (r *personHandoverRig) outboxRows(key string, kind record.OutboxKind, payloadKind string) int {
	r.t.Helper()
	var count int
	if err := r.pool.QueryRow(r.t.Context(), `select count(*) from outbox where kind = $1 and issue = $2 and payload->>'kind' = $3`,
		string(kind), key, payloadKind).Scan(&count); err != nil {
		r.t.Fatalf("count the %s rows of kind %s queued on %s: %v", kind, payloadKind, key, err)
	}
	return count
}

// triageNotices counts the controller notices of kind triage the daemon has queued on key: the
// wake a session's label on an unrecorded triage root gives the controller.
func (r *personHandoverRig) triageNotices(key string) int {
	r.t.Helper()
	return r.outboxRows(key, record.OutboxKindControllerNotice, string(record.TriageNotice))
}

// designApprovedNotices counts the design-approved notices the daemon has queued on key for its
// architect: what an approval that opens a registered gate writes.
func (r *personHandoverRig) designApprovedNotices(key string) int {
	r.t.Helper()
	return r.outboxRows(key, record.OutboxKindNotice, "design-approved")
}

// personHandoverLogLines counts the lines of the daemon's log that say admission queued a person's
// handover of key from source (`event`, `listing` or `approval`), the one message every path
// logs its queue under once it commits.
func (r *personHandoverRig) personHandoverLogLines(key, source string) int {
	r.t.Helper()
	lines := 0
	for _, line := range loggedLines(r.t, r.logs.String(), "admission: queued a person's handover") {
		if line["msg"] == "admission: queued a person's handover" && line["issue"] == key && line["source"] == source {
			lines++
		}
	}
	return lines
}

// waiting waits for admission.waiting to name key and answers the state it did in.
func (r *personHandoverRig) waiting(key string) (api.State, string) {
	r.t.Helper()
	return settled(r.t, r.d, "name "+key+" in its waiting line", func(s api.State) bool {
		return slices.Contains(s.Admission.Waiting, key)
	})
}

// queuedByThePerson is what every positive asserts of key once the daemon queued the person's
// handover from source and delivered it: Dispatch shows the issue todo, the newest issue.updated
// event of its log is the daemon's own write under its actor (dispatch.DaemonSession) carrying
// that todo, admission.waiting names it (the cap's one slot is held throughout), the daemon logged
// one queue of it from source so far — lines counts the queues the proof expects on the key by
// now — and no person's handover row is left in the outbox.
func (r *personHandoverRig) queuedByThePerson(key, source string, lines int) {
	t := r.t
	t.Helper()
	awaitDispatchStatus(t, r.baseURL, key, "todo")
	written := r.newestEvent(key, "issue.updated", nil)
	if written.Actor != (scratchActor{Kind: "session", ID: dispatch.DaemonSession(key)}) || written.status() != "todo" {
		t.Fatalf("the newest issue.updated of %s = %+v; want the daemon's own todo under %s", key, written, dispatch.DaemonSession(key))
	}
	_, body := r.waiting(key)
	t.Logf("the daemon queued the person's handover of %s from %s, wrote its todo as %s, and holds it in its waiting line:\n%s",
		key, source, dispatch.DaemonSession(key), body)
	if got := r.personHandoverLogLines(key, source); got != lines {
		t.Fatalf("the daemon logged %d queues of a person's handover of %s from %s, want %d; its log:\n%s", got, key, source, lines, r.logs.String())
	}
	if rows := r.personHandoverRows(key); len(rows) != 0 {
		t.Fatalf("the person's handover rows still queued on %s = %+v after its todo was written; want none", key, rows)
	}
}

// notQueued is what every negative asserts of key once the daemon applied the act (applied, or the
// triage row the act queues): Dispatch shows it at status, not todo, no person's handover row is
// queued on it, and the daemon logged no more queues of it than lines, the queues earlier steps
// expected.
func (r *personHandoverRig) notQueued(key, status string, lines int) {
	t := r.t
	t.Helper()
	if shown := r.issue(key).Status; shown != status {
		t.Fatalf("Dispatch shows %s %s; want %s, with no todo of the daemon's", key, shown, status)
	}
	if rows := r.personHandoverRows(key); len(rows) != 0 {
		t.Fatalf("the daemon queued a person's handover on %s: %+v; want none", key, rows)
	}
	total := 0
	for _, source := range []string{"event", "listing", "approval"} {
		total += r.personHandoverLogLines(key, source)
	}
	if total != lines {
		t.Fatalf("the daemon logged %d queues of a person's handover of %s, want %d; its log:\n%s", total, key, lines, r.logs.String())
	}
}

// admit hands key, a labelled root, to Legion as the person's todo and waits for it to take a slot
// and for Dispatch to show the daemon's in_progress; it answers a minter of the grants its launched
// architect's registered session holds, as architectGrants does for the blockers proof.
func (r *personHandoverRig) admit(key string) func() string {
	t := r.t
	t.Helper()
	setDispatchStatus(t, r.baseURL, key, "todo")
	settled(t, r.d, "admit "+key+" into a slot", func(s api.State) bool {
		return slices.Contains(s.Admission.Active, key)
	})
	project, err := claim.ProjectToken(r.cfg.Project)
	if err != nil {
		t.Fatalf("the project token of %s: %v", r.cfg.Project, err)
	}
	architect, err := claim.NewToken(project, key, claim.RoleArchitect)
	if err != nil {
		t.Fatalf("the architect claim token of %s: %v", key, err)
	}
	testwait.Eventually(t, "the daemon to launch the architect of "+key, func() bool {
		for _, call := range r.rt.Calls() {
			if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == architect {
				return true
			}
		}
		return false
	})
	grant := architectGrants(t, r.d, lastLaunch(t, r.rt, architect), key)
	awaitDispatchStatus(t, r.baseURL, key, "in_progress")
	return grant
}

// registerGate registers artifactID, key's primary document, at version 1 as key's architect, as
// the architect's plugin does, and waits for the daemon to record the gate; with the design gate
// at its default the gate stays closed until a person approves the document.
func (r *personHandoverRig) registerGate(key, artifactID string, grant func() string) {
	t := r.t
	t.Helper()
	if status, answer := r.d.request(http.MethodPost, "/legion/v1/gates/register", api.GateRegisterRequest{
		GrantID: grant(), Issue: key, ArtifactID: artifactID, Version: 1,
	}, false); status != http.StatusOK {
		t.Fatalf("the architect's design-gate registration on %s = %d; body %s", key, status, answer)
	}
	state, body := settled(t, r.d, "record the design gate of "+key, func(s api.State) bool {
		return s.Issues[key].DesignGate != nil
	})
	if gate := state.Issues[key].DesignGate; gate.ApprovedVersion != nil {
		t.Fatalf("the design gate of %s opened at its registration, approved at %d, though nobody approved its document\nstate %s", key, *gate.ApprovedVersion, body)
	}
}

// cleaned waits for the cleanup of key's tree, which its park began, to be confirmed
// (tree_lifecycles), as the blockers proof waits before it takes a tree back: a root handed back
// while its cleanup still runs waits for it, and is promoted at the next fact.
func (r *personHandoverRig) cleaned(key string) {
	r.t.Helper()
	testwait.Eventually(r.t, "the cleanup of "+key+"'s tree to be confirmed", func() bool {
		var cleaned bool
		if err := r.pool.QueryRow(r.t.Context(), `select cleanup_confirmed_at is not null from tree_lifecycles where tree = $1`, key).Scan(&cleaned); err != nil {
			return false
		}
		return cleaned
	})
}

// A2. A person's legion label on an unrecorded root in triage is that person's handover, and the
// daemon queues the root's todo itself; a session's label is the controller's triage wake, as it
// always was. The cap is one slot, held throughout by a root a session labelled and the person set
// todo, so every root the daemon queues stands visibly in admission.waiting.
//
//  1. a session's labelled root is set todo by the person and takes the one slot;
//  2. the person creates a root, which Dispatch starts in triage, and adds the legion label: within
//     60 s Dispatch shows it todo, the newest issue.updated of its log is the daemon's own write
//     under legion-daemon:<project>, admission.waiting names it, no controller notice of kind
//     triage names it, and the daemon logged the one queue, from source event;
//  3. the same two writes as a session — the create, then the label — leave the root in triage and
//     queue exactly one triage notice, the row the label's event produces, which is the anchor;
//  4. on that root the person comments, then adds an unrelated label beside the legion label: it
//     stays in triage, the edit's event queuing a second triage notice (the anchor), since the
//     newest event that added the legion label is still the session's, and the comment between
//     them is an event of another kind, which the setter reads past;
//  5. the root of step 2, which the daemon recorded, is moved back to triage by the person and then
//     retitled by the person: no todo is written, the daemon's mirror of Dispatch standing at the
//     retitle's own sequence, no handover row is queued, and the one queue of step 2 is all the log
//     holds.
func TestAPersonsLabelOnATriageRootIsQueuedByTheDaemonAgainstScratchDispatch(t *testing.T) {
	r := newPersonHandoverRig(t, 1)
	r.boot()

	// 1. The slot is taken: a session's label hands nothing over, and the person's todo admits it.
	taker := r.create(asSession, "Occupant keeps the cap", map[string]any{"labels": []string{"legion"}})
	r.admit(taker.Key)

	// 2. The person creates a root and labels it.
	handed := r.create(asPerson, "Rollout handed over by hand", nil)
	labelled := r.addLabel(handed.Key, asPerson, "legion")
	r.queuedByThePerson(handed.Key, "event", 1)
	if notices := r.triageNotices(handed.Key); notices != 0 {
		t.Fatalf("the daemon queued %d triage notices on %s, a root a person labelled; want none: the daemon queued its todo instead", notices, handed.Key)
	}
	if lines := r.personHandoverLogLines(handed.Key, "listing") + r.personHandoverLogLines(handed.Key, "approval"); lines != 0 {
		t.Fatalf("the daemon logged %d queues of %s from a source other than its label's event; its log:\n%s", lines, handed.Key, r.logs.String())
	}
	t.Logf("the person's label on %s (Dispatch sequence %d) was queued by the daemon as the root's todo", handed.Key, labelled.LastSeq)

	// 3. The same two writes as a session.
	agentRoot := r.create(asSession, "Sibling an agent files", nil)
	r.addLabel(agentRoot.Key, asSession, "legion")
	testwait.Eventually(t, "the daemon to queue the triage wake of "+agentRoot.Key, func() bool {
		return r.triageNotices(agentRoot.Key) >= 1
	})
	if notices := r.triageNotices(agentRoot.Key); notices != 1 {
		t.Fatalf("the daemon queued %d triage notices on %s for a session's label; want one", notices, agentRoot.Key)
	}
	r.notQueued(agentRoot.Key, "triage", 0)
	t.Logf("a session's label on %s woke the controller once and queued no todo", agentRoot.Key)

	// 4. A person's comment, then a person's unrelated label beside the session's.
	r.comment(agentRoot.Key, "Looks like the docs need a pass first.")
	r.addLabel(agentRoot.Key, asPerson, "legion", "docs")
	testwait.Eventually(t, "the daemon to queue the second triage wake of "+agentRoot.Key, func() bool {
		return r.triageNotices(agentRoot.Key) >= 2
	})
	if notices := r.triageNotices(agentRoot.Key); notices != 2 {
		t.Fatalf("the daemon queued %d triage notices on %s once a person added an unrelated label; want two, one per labelled observation", notices, agentRoot.Key)
	}
	r.notQueued(agentRoot.Key, "triage", 0)
	t.Logf("a person's unrelated label on %s, after a comment of theirs, left the session's label the session's: a triage wake again, no todo", agentRoot.Key)

	// 5. The recorded root, moved back to triage by the person and retitled.
	back := r.setStatus(handed.Key, "triage")
	r.applied(r.newestEvent(handed.Key, "issue.updated", func(e scratchEvent) bool { return e.Seq == back.LastSeq }))
	settled(t, r.d, "take "+handed.Key+" out of the waiting line once it went back to triage", func(s api.State) bool {
		return !slices.Contains(s.Admission.Waiting, handed.Key)
	})
	retitled := r.patch(handed.Key, asPerson, map[string]any{"title": "Rollout handed over by hand, renamed"})
	if status := r.mirrored(handed.Key, retitled.LastSeq); status != "triage" {
		t.Fatalf("the daemon mirrors %s %s at the retitle's sequence %d; want triage", handed.Key, status, retitled.LastSeq)
	}
	r.notQueued(handed.Key, "triage", 1)
	state, body := settled(t, r.d, "answer its own state", func(api.State) bool { return true })
	if slices.Contains(state.Admission.Waiting, handed.Key) || slices.Contains(state.Admission.Active, handed.Key) {
		t.Fatalf("Legion holds %s, moved back to triage and retitled by a person, in its line or a slot\nstate %s", handed.Key, body)
	}
	t.Logf("the recorded root %s, moved back to triage and retitled by the person, got no todo:\n%s", handed.Key, body)
}

// A3. A person's approval of the primary document of a parked labelled root with no parent is that
// person's handover, and the daemon queues the root's todo itself, whether the root is unrecorded
// or recorded from an earlier run; nothing else a person approves, and nothing a session does, is.
// The cap is one slot, held from step 2 on by a running root whose architect registered its gate,
// so every root the daemon queues stands in admission.waiting. Every labelled root is created by a
// session, so its label hands nothing over (A2), and the person parks it in backlog: the parked
// labelled unrecorded root A3 starts from.
//
//  1. a labelled root the person set todo takes the slot, its architect registers its gate, and
//     the person parks it in backlog: a recorded root with a gate from its earlier run, whose tree's
//     cleanup the proof waits out;
//  2. a second labelled root takes the slot for good, its architect registering its gate: the
//     running root;
//  3. a parked labelled root: a bearer's approval of its spec is refused 403 HUMAN_ONLY; a session
//     uploads a second document, which the person approves, and the daemon applies that approval
//     writing nothing; the person then approves the spec itself, and within 60 s Dispatch shows the
//     root todo under the daemon's actor, admission.waiting names it, and the log holds the one
//     queue from source approval;
//  4. the person approves the spec of a parked root nobody handed to Legion: nothing;
//  5. the person approves the spec of a parked labelled child, under that root: nothing;
//  6. the person approves the running root's registered gate document: its gate opens as today,
//     the state's design gate approved at that version, and no handover is queued;
//  7. the carried case: on a parked labelled root a session writes a decision block into the spec;
//     the person approves it, which queues the root (todo, waiting), and parks it again; the
//     person then answers the block, and Dispatch records a carried approval of the version the
//     answer wrote, in the person's name: the daemon applies it writing nothing;
//  8. the recorded parked root of step 1, its spec revised by a session, is approved by the person
//     at the new version: todo within 60 s, admission.waiting names it, and no design-approved
//     notice is queued for it, since its own gate is not applied;
//  9. the root of step 3, parked again by the person and its spec revised by a session, is approved
//     again by the person at the new version: todo again, a second queue.
func TestAPersonsApprovalOfAParkedRootsSpecIsQueuedByTheDaemonAgainstScratchDispatch(t *testing.T) {
	r := newPersonHandoverRig(t, 1)
	r.boot()
	labelled := map[string]any{"labels": []string{"legion"}}

	// 1. A recorded root with a registered gate from its earlier run, parked.
	recorded := r.create(asSession, "Earlier run keeps its gate", labelled)
	r.registerGate(recorded.Key, recorded.PrimaryArtifactID, r.admit(recorded.Key))
	parkedRecorded := r.setStatus(recorded.Key, "backlog")
	settled(t, r.d, "release the slot of "+recorded.Key+" once the person parked it", func(s api.State) bool {
		return !slices.Contains(s.Admission.Active, recorded.Key)
	})
	r.cleaned(recorded.Key)
	t.Logf("%s ran, registered its gate at version 1, and was parked by the person at Dispatch's sequence %d; its tree's cleanup is confirmed", recorded.Key, parkedRecorded.LastSeq)

	// 2. The running root takes the slot for good.
	running := r.create(asSession, "Flagship tree occupies everything", labelled)
	r.registerGate(running.Key, running.PrimaryArtifactID, r.admit(running.Key))

	// 3. A parked labelled root: the bearer's approval, a non-primary document's, then the spec's.
	parked := r.create(asSession, "Parked proposal awaiting approval", labelled)
	r.setStatus(parked.Key, "backlog")
	if status, _ := r.approveDocument(parked.PrimaryArtifactID, asSession); status != http.StatusForbidden {
		t.Fatalf("a bearer's approval of %s's spec = %d; want 403 HUMAN_ONLY", parked.Key, status)
	}
	notes := r.uploadDocument(parked.Key, "notes.md", "# Notes\n\nSide notes, not the spec.\n")
	notesReview := r.approve(notes)
	r.applied(r.newestEvent(parked.Key, "artifact.approved", func(e scratchEvent) bool { return e.version() == notesReview.Version }))
	r.notQueued(parked.Key, "backlog", 0)
	t.Logf("a bearer's approval of %s's spec was refused, and the person's approval of its second document %s queued nothing", parked.Key, notes)
	first := r.approve(parked.PrimaryArtifactID)
	r.queuedByThePerson(parked.Key, "approval", 1)
	t.Logf("the person's approval of %s's spec at version %d queued the root", parked.Key, first.Version)

	// 4. A parked root nobody handed to Legion.
	unlabelled := r.create(asPerson, "Nobody delegated this chore", nil)
	r.setStatus(unlabelled.Key, "backlog")
	review := r.approve(unlabelled.PrimaryArtifactID)
	r.applied(r.newestEvent(unlabelled.Key, "artifact.approved", func(e scratchEvent) bool { return e.version() == review.Version }))
	r.notQueued(unlabelled.Key, "backlog", 0)

	// 5. A parked labelled child.
	child := r.create(asSession, "Subtask beneath undelegated work", map[string]any{"labels": []string{"legion"}, "parent": unlabelled.Key})
	r.setStatus(child.Key, "backlog")
	review = r.approve(child.PrimaryArtifactID)
	r.applied(r.newestEvent(child.Key, "artifact.approved", func(e scratchEvent) bool { return e.version() == review.Version }))
	r.notQueued(child.Key, "backlog", 0)
	t.Logf("the person's approvals of %s, which nobody handed over, and of its labelled child %s queued nothing", unlabelled.Key, child.Key)

	// 6. The running root's registered gate document: its gate opens as today.
	review = r.approve(running.PrimaryArtifactID)
	state, body := settled(t, r.d, "open the design gate of "+running.Key, func(s api.State) bool {
		gate := s.Issues[running.Key].DesignGate
		return gate != nil && gate.ApprovedVersion != nil
	})
	if got := *state.Issues[running.Key].DesignGate.ApprovedVersion; got != review.Version {
		t.Fatalf("the design gate of %s opened at version %d; want %d, the version the person approved\nstate %s", running.Key, got, review.Version, body)
	}
	r.notQueued(running.Key, "in_progress", 0)
	t.Logf("the person's approval of the running root %s's registered gate document opened its gate and queued no handover:\n%s", running.Key, body)

	// 7. The carried case.
	carried := r.create(asSession, "Regional wave decision pending", labelled)
	r.setStatus(carried.Key, "backlog")
	blockVersion := r.editDocument(carried.PrimaryArtifactID, map[string]string{"op": "insert", "after": "end", "markdown": personHandoverBlock})
	askID := r.blockAsk(carried.Key, "wave")
	approved := r.approve(carried.PrimaryArtifactID)
	if approved.Version != blockVersion {
		t.Fatalf("the person approved %s's spec at version %d; want %d, the version holding the decision block", carried.Key, approved.Version, blockVersion)
	}
	r.queuedByThePerson(carried.Key, "approval", 1)
	reparked := r.setStatus(carried.Key, "backlog")
	r.applied(r.newestEvent(carried.Key, "issue.updated", func(e scratchEvent) bool { return e.Seq == reparked.LastSeq }))
	r.answerBlock(askID, personHandoverAnswer)
	carry := r.carriedReview(carried.PrimaryArtifactID, approved.Version)
	r.applied(r.newestEvent(carried.Key, "artifact.approved", func(e scratchEvent) bool { return e.version() == carry.Version }))
	r.notQueued(carried.Key, "backlog", 1)
	t.Logf("the person's answer to the decision block of %s carried their approval of version %d to version %d (%q), and the daemon queued nothing for it",
		carried.Key, approved.Version, carry.Version, *carry.Reason)

	// 8. The recorded parked root with a gate from its earlier run, approved at a new version.
	revised := r.revise(recorded.PrimaryArtifactID, "behind a flag", "behind a flag, in two waves")
	noticesBefore := r.designApprovedNotices(recorded.Key)
	review = r.approve(recorded.PrimaryArtifactID)
	if review.Version != revised {
		t.Fatalf("the person approved %s's spec at version %d; want %d, the revised version", recorded.Key, review.Version, revised)
	}
	r.queuedByThePerson(recorded.Key, "approval", 1)
	if after := r.designApprovedNotices(recorded.Key); after != noticesBefore {
		t.Fatalf("the daemon queued %d design-approved notices on %s for the approval that queued its todo; want none: a recorded parked root's own gate is not applied",
			after-noticesBefore, recorded.Key)
	}
	t.Logf("the person's approval of the recorded parked root %s at version %d queued it again with no design-approved notice", recorded.Key, review.Version)

	// 9. A second approval of a later version, after the person parked the root of step 3 again.
	again := r.setStatus(parked.Key, "backlog")
	r.applied(r.newestEvent(parked.Key, "issue.updated", func(e scratchEvent) bool { return e.Seq == again.LastSeq }))
	revised = r.revise(parked.PrimaryArtifactID, "behind a flag", "behind a flag, gradually")
	review = r.approve(parked.PrimaryArtifactID)
	if review.Version != revised || review.Version <= first.Version {
		t.Fatalf("the person approved %s's spec at version %d; want %d, a version later than the first approval's %d", parked.Key, review.Version, revised, first.Version)
	}
	r.queuedByThePerson(parked.Key, "approval", 2)
	t.Logf("the person's second approval of %s, at version %d after a second park, queued it again", parked.Key, review.Version)
}

// A4. The boot listing queues a person's label the daemon's Dispatch consumer never sees. The
// premise is intake's consumer policy (D6): createConsumer makes a durable consumer with no stored
// stream position under DeliverNewPolicy (intake/consume.go), so a daemon booted for the first time
// after a person's label never receives that label's event, and only the boot listing, which
// reads every status, can see the root. A consumer created to start at the stream's first message
// instead would deliver the event, and this proof would read the queue from source event.
//
//  1. before any daemon boots, the person creates a root in triage and adds the legion label;
//  2. the daemon boots: its listing finds the labelled triage root nobody recorded and whose label
//     the person set, and queues its todo — one log line from source listing — which its outbox
//     writes under the daemon's actor and which its consumer, live by then, records;
//  3. the daemon stops and boots again on the same store and configuration: its listing finds the
//     root recorded, queues no second row and logs no second line, and Dispatch holds the one todo
//     write of the daemon's.
//
// It shows the boot path. It does not show a daemon down for longer than the event stream keeps a
// message.
func TestABootListingQueuesAPersonsLabelAgainstScratchDispatch(t *testing.T) {
	r := newPersonHandoverRig(t, 1)

	// 1. The person's label falls while no daemon runs.
	booted := r.create(asPerson, "Nightly rotation configured offline", nil)
	labelled := r.addLabel(booted.Key, asPerson, "legion")
	t.Logf("the person labelled %s at Dispatch's sequence %d before the daemon's first boot", booted.Key, labelled.LastSeq)

	// 2. The first boot: the listing queues it, the outbox writes it, the consumer records it.
	r.boot()
	if lines := r.personHandoverLogLines(booted.Key, "listing"); lines != 1 {
		t.Fatalf("the first boot logged %d queues of %s from its listing; want one; its log:\n%s", lines, booted.Key, r.logs.String())
	}
	daemonsTodo := func(e scratchEvent) bool {
		return e.Actor == (scratchActor{Kind: "session", ID: dispatch.DaemonSession(booted.Key)}) && e.status() == "todo"
	}
	testwait.Eventually(t, "the daemon to write the todo of "+booted.Key, func() bool {
		return slices.ContainsFunc(r.issueEvents(booted.Key), func(e scratchEvent) bool { return e.Type == "issue.updated" && daemonsTodo(e) })
	})
	written := r.newestEvent(booted.Key, "issue.updated", daemonsTodo)
	_, body := settled(t, r.d, "record "+booted.Key+" from its own todo write", func(s api.State) bool {
		_, recorded := s.Issues[booted.Key]
		return recorded
	})
	t.Logf("the first boot's listing queued the person's handover of %s, written as %s at Dispatch's sequence %d:\n%s",
		booted.Key, dispatch.DaemonSession(booted.Key), written.Seq, body)
	if lines := r.personHandoverLogLines(booted.Key, "event"); lines != 0 {
		t.Fatalf("the first boot's consumer saw the person's label of %s and queued it from the event: the consumer was not positioned after it; its log:\n%s", booted.Key, r.logs.String())
	}
	if notices := r.triageNotices(booted.Key); notices != 0 {
		t.Fatalf("the daemon queued %d triage notices on %s, a root a person labelled; want none", notices, booted.Key)
	}
	testwait.Eventually(t, "the outbox to finish the handover row of "+booted.Key, func() bool {
		return len(r.personHandoverRows(booted.Key)) == 0
	})

	// 3. The second boot on the same store writes nothing more.
	r.d.stop()
	rebindHeldPorts(t, &r.cfg)
	r.boot()
	if count, _ := boots(t, r.cfg); count != 2 {
		t.Fatalf("the store records %d boots; want two", count)
	}
	if lines := r.personHandoverLogLines(booted.Key, "listing"); lines != 1 {
		t.Fatalf("across two boots the daemon logged %d queues of %s from its listing; want the first boot's one; its log:\n%s", lines, booted.Key, r.logs.String())
	}
	if rows := r.personHandoverRows(booted.Key); len(rows) != 0 {
		t.Fatalf("the second boot queued a person's handover on %s, a root already recorded: %+v", booted.Key, rows)
	}
	todos := 0
	for _, event := range r.issueEvents(booted.Key) {
		if event.Type == "issue.updated" && daemonsTodo(event) {
			todos++
		}
	}
	if todos != 1 {
		t.Fatalf("Dispatch holds %d todo writes of the daemon's on %s; want the first boot's one", todos, booted.Key)
	}
	t.Logf("the second boot found %s recorded and wrote nothing more", booted.Key)
}
