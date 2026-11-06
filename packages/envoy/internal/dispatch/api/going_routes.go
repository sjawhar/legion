package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The three routes of an answer: the POST that asks a question, the stream that carries the
// answer, and the capability read. They are two requests rather than one because the server
// wraps every POST in its idempotency layer and that layer refuses to stream (keyedWrites), and
// because a deployment of more than one pod spreads the two across pods: everything a reader of
// the stream needs is the record's (going.go), so any pod serves the stream of a run another pod
// is answering, or of a run whose pod is gone.

// codeGoingRequestNotFound is the one answer the stream gives for every run a caller may not
// read: a run the record does not hold, an id that is not a uuid, and a run that is somebody
// else's. One answer for all three is what keeps an id from telling a caller anything it did not
// already know - whether that run exists, and whether it is anyone's.
const codeGoingRequestNotFound = "GOING_REQUEST_NOT_FOUND"

// codeGoingAskerNamesNobody refuses the POST of a credential that names nobody: the
// deployment's shared token, which resolves as {} (server.go). The stream serves a run to the
// credential that asked it and to no other, and comparing identities cannot tell two holders
// of one shared token apart, so such a run would be readable by nobody - the model called, the
// deployment's paced quota spent, and every reader of it answered 404. It is refused where
// that is cheap and plain, before anything is recorded.
const codeGoingAskerNamesNobody = "GOING_ASKER_NAMES_NOBODY"

// goingStreamPoll is how often an open stream reads the record for frames the run has stored
// since its last read. The frames are rows, so a reader learns of one by reading: this is the
// delay a reader may see on a frame, and at a few reads a second over a run's window it is a
// handful of reads per open stream.
const goingStreamPoll = 200 * time.Millisecond

// goingStreamGrace is how long past a run's ends_at a stream and the writes of a run may still
// run: enough for the terminal frame of a run reaching its deadline to be stored and written to
// whoever is reading, and no more, so neither outlives its run by much. A stream of a run
// already past its ends_at - the pod that took the POST is gone - gets that grace from now,
// which is the time this reader needs to end the run itself.
const goingStreamGrace = 5 * time.Second

// Answerer generates one answer from what Dispatch holds. It is declared here, with the one
// method the server calls, rather than taken as *going.Answerer, so a test injects a fake
// through Deps without an HTTP service behind it; nil is a deployment with no model configured
// (cmd/dispatch: DISPATCH_ANSWERER unset), where a run is still recorded and its stream carries
// the error frame contracts.GoingErrorNoModelConfigured.
type Answerer interface {
	// Run answers the request, handing emit each frame of the stream in order with exactly one
	// terminal frame last; the error it answers is emit's own.
	Run(ctx context.Context, request going.Request, emit func(going.Event) error) error
}

// createGoingRequestInput is one question as a caller asks it: the words, and the project they
// are asked of. An empty project asks of every project, as search reads its own filter.
type createGoingRequestInput struct {
	Words   string `json:"words"`
	Project string `json:"project"`
}

// goingCapability is what the capability route answers, and all it answers: whether a model is
// configured. No endpoint, deployment name or credential has a field here, so none can appear
// in the answer.
type goingCapability struct {
	ModelConfigured bool `json:"model_configured"`
}

// createGoingRequest records one question and starts its answer: POST /api/v1/going/requests. It
// answers the stored run, whose id addresses the stream, as soon as the row is committed, and
// the answer itself runs behind it on s.detached, which shutdown waits for (server.detached): a
// run takes contracts.GoingRunWindow, which is far too long to hold an answer for.
//
// A credential that names nobody is refused here, before anything is recorded
// (codeGoingAskerNamesNobody): the stream would serve its run to no reader at all.
//
// A deployment with no model configured records the question exactly the same way and stores the
// run's error frame instead of answering it (startGoingRun), so a caller learns that from the
// stream, where every other failure of a run is reported, rather than from a status of this
// route's own.
func (s *server) createGoingRequest(w http.ResponseWriter, r *http.Request) {
	// The caller's own credential names the writer: this route takes no `actor`, so a question
	// is recorded for whoever asked it - the person at the browser, the session a broker-issued
	// token proved, or the person a personal token belongs to - and the stream compares the
	// reader with it. Recorded, as every actor a row holds is (model.Actor.Recorded).
	caller, _, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	actor := caller.Recorded()
	// A credential that names nobody asks nothing: see codeGoingAskerNamesNobody. Refused here,
	// before the record, so no run is stored and no quota is spent on an answer no reader of it
	// would ever be served.
	if !goingActorNamesSomebody(actor) {
		writeError(w, codeGoingAskerNamesNobody, http.StatusForbidden,
			"this credential names nobody, so the run it asked for would be nobody's to read: ask under a credential that names a session, a person or a workload")
		return
	}
	var input createGoingRequestInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	words := strings.TrimSpace(input.Words)
	if words == "" {
		writeError(w, "GOING_INPUT", http.StatusBadRequest, "words is required: the question to answer")
		return
	}
	// Counted as search counts its own query, after trimming, so a question this route accepts
	// is one the answerer can send whole.
	if length := itemblocks.Units16(words); length > contracts.GoingWordsMax {
		capExceeded(w, "words", length, contracts.GoingWordsMax)
		return
	}
	project := strings.TrimSpace(input.Project)
	if project != "" && !projectKeyPattern.MatchString(project) {
		writeError(w, "INVALID_PROJECT", http.StatusBadRequest, projectKeyProblem("project", project))
		return
	}
	// The project comes from the body, not from the address, so this row is classed
	// scopeProjectInBody and withinProjectScope leaves the check here, as it leaves createIssue's
	// (issue_create.go). It runs before the row is written: a credential that may write one
	// project asks only of that project, and asking of every project is asking outside it.
	if covers, scoped := s.callerProject(r); scoped && covers != project {
		refuseWriteOutsideProjectScope(w, covers, project)
		return
	}
	// Detached from the request, as a broadcast's deliveries are (broadcasts.go): SIGTERM
	// cancels the server's lifetime before the HTTP drain begins, so a run bound to either the
	// request or the lifetime would be cut off with frames it had not stored, and a reader would
	// wait out the deadline for frames nothing was writing.
	//
	// On a connection mark of the run's own (store.ForConcurrentRead) and never the request's,
	// which context.WithoutCancel keeps: the run outlives this handler, and its first search
	// holds one of the pool's connections while its cursor is open (store.Pool.Query). This
	// request's own mark is still in use at that moment - keyedWrites settles the
	// Idempotency-Key with an Exec on it the moment this handler returns - and the pool refuses
	// a second connection taken under a held mark (store.ErrNestedAcquire), so a shared mark
	// would lose that settle to the run's first search and the key would store no answer: a
	// repeat would then be recorded as a second question instead of being answered this run's
	// id.
	//
	// Taken before the question is recorded, as every other caller of ForConcurrentRead answers
	// its refusal (comment_create.go): a run that cannot be detached is a run nothing would
	// answer, so recording the question first would leave the caller a 201 and a stream that
	// only runs out its deadline.
	answering, err := store.ForConcurrentRead(context.WithoutCancel(r.Context()))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	request, err := s.recordGoingRequest(r.Context(), actor, project, words)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.detached.goDetached(func() { s.startGoingRun(answering, request) })
	WriteJSON(w, http.StatusCreated, request)
}

// startGoingRun answers one recorded run and stores its frames. The run itself is bounded by the
// window the record gave it (goingRequest.EndsAt), which is what every reader of the run reads
// too; the writes are bounded by that window and goingStreamGrace, because the terminal frame of
// a run reaching its deadline is stored at the moment the deadline passes and a context that
// ended with the run could not store it.
func (s *server) startGoingRun(ctx context.Context, request goingRequest) {
	storing, endStoring := context.WithDeadline(ctx, request.EndsAt.Add(goingStreamGrace))
	defer endStoring()
	if s.deps.Answerer == nil {
		if _, err := s.endGoingRun(storing, request.ID, going.Fault(contracts.GoingErrorNoModelConfigured)); err != nil {
			slog.Warn("dispatch going: store the no-model-configured frame", "request", request.ID, "error", err)
		}
		return
	}
	answering, endAnswering := context.WithDeadline(storing, request.EndsAt)
	defer endAnswering()
	emit := func(event going.Event) error {
		if event.Terminal() {
			_, err := s.endGoingRun(storing, request.ID, event)
			return err
		}
		_, err := s.appendGoingEvent(storing, request.ID, event)
		return err
	}
	err := s.deps.Answerer.Run(answering, going.Request{
		Words:  request.Words,
		Search: s.goingSearch(request.Project),
		Bodies: s.goingBodies,
	}, emit)
	switch {
	case err == nil:
	case errors.Is(err, pgx.ErrNoRows):
		// A frame the record stored none of: a reader at the deadline or the sweep ended this
		// run under the answer (going.go). The run has its one ending, which is the one every
		// reader of it is written, so there is nothing to store and nothing to report.
	default:
		// The run holds no terminal frame, so whoever reads it past its ends_at ends it with the
		// deadline's error, exactly as it ends a run whose pod died.
		slog.Warn("dispatch going: the answer's frames could not be stored", "request", request.ID, "error", err)
	}
}

// goingSearch is the one search the answerer makes, within the project the question was asked
// of: the route supplies the project, the limit and the offset, so the model cannot widen the
// search past what the caller asked of.
func (s *server) goingSearch(project string) func(context.Context, string) ([]going.Hit, error) {
	return func(ctx context.Context, q string) ([]going.Hit, error) {
		found, err := s.runSearch(ctx, q, project, searchDefaultLimit, 0)
		if err != nil {
			return nil, err
		}
		hits := make([]going.Hit, 0, len(found.Results))
		for _, result := range found.Results {
			hits = append(hits, going.Hit{Kind: result.Kind, ID: result.ID, Title: goingHitTitle(result), Href: result.Href})
		}
		return hits, nil
	}
}

// goingHitTitle names one hit to the model and to the reader: an issue's own title for a hit the
// issue owns, and the document's name for every other, which is what search shows each of them
// under.
func goingHitTitle(result model.SearchResult) string {
	if result.Owner.Kind == "issue" {
		return result.Owner.Title
	}
	return result.Owner.Name
}

// goingBodies is each hit's own stored text, the text search ranked, cut to
// contracts.GoingHitRuneMax runes a hit and keyed as going.Hit.Key keys it (goingItemText).
func (s *server) goingBodies(ctx context.Context, hits []going.Hit) (map[string]string, error) {
	return goingItemText(ctx, s.deps.Store.Pool, hits, contracts.GoingHitRuneMax)
}

// streamGoingAnswer writes one run's answer as server-sent events: GET
// /api/v1/going/requests/{id}/stream. It serves the record and not the process that took the
// POST, so the stream of a run this pod never ran is the same stream, and a reader that drops
// resumes from the frame it holds (Last-Event-ID).
//
// A run past its ends_at with no terminal frame stored is ended by this reader, with the
// deadline's error, and the stream ends on whatever terminal frame that write answers
// (endGoingRun settles which one). That is how a stream ends when the pod that took the POST is
// gone: nothing else would ever store that run's ending until the sweep ran.
func (s *server) streamGoingAnswer(w http.ResponseWriter, r *http.Request) {
	// Resolved once rather than through requireAuthenticated and a second resolution: the actor
	// is what this route compares the run's author with, and a credential resolves at a cost -
	// a token row, the sign-in issuer's confirmation - worth paying once.
	actor, _, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	id := r.PathValue("id")
	// Decided before the read, not after: readGoingRequest casts $1::uuid, so an id that is not
	// a uuid comes back as a Postgres error rather than pgx.ErrNoRows, and this route answers an
	// id nobody could hold exactly as it answers one nobody does.
	if _, err := uuid.Parse(id); err != nil {
		writeGoingRequestNotFound(w)
		return
	}
	request, err := s.readGoingRequest(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeGoingRequestNotFound(w)
		return
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The whole credential, through model.Actor.SameCredential and never SameAs alone, which
	// would make any two personal tokens one writer: a run is read by the credential that
	// asked it. A run whose author names nobody is read by nobody, and that is a test of its
	// own because SameCredential calls two credentials that name nobody the one credential:
	// without it, such a row would be served to every holder of the deployment's shared token,
	// which resolves as {} exactly as that author does.
	if !goingActorNamesSomebody(request.Author) || !request.Author.SameCredential(actor) {
		writeGoingRequestNotFound(w)
		return
	}
	after := goingResumeFrom(r)

	// Past this point the answer is a stream: its status and headers are written before the
	// first frame is read, and a failure after them can only end the stream.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	stream := http.NewResponseController(w)
	if err := stream.Flush(); err != nil {
		return
	}

	// The stream lives as long as the run and goingStreamGrace, and at least that grace from
	// now, so a run already past its ends_at has the time this reader needs to end it; the
	// caller's own going away ends it sooner.
	until := request.EndsAt.Add(goingStreamGrace)
	if soonest := time.Now().Add(goingStreamGrace); until.Before(soonest) {
		until = soonest
	}
	ctx, done := context.WithDeadline(r.Context(), until)
	defer done()
	for {
		frames, state, err := s.readGoingStream(ctx, id, after)
		if err != nil {
			// pgx.ErrNoRows is the sweep having deleted the run under this reader
			// (contracts.GoingRequestTTL); either way there is nothing left to read.
			return
		}
		for _, frame := range frames {
			if err := writeGoingFrame(w, frame); err != nil {
				return
			}
			after = frame.Sequence
			if err := stream.Flush(); err != nil {
				return
			}
			if frame.Event.Terminal() {
				return
			}
		}
		switch {
		case state.Terminal > 0:
			// The run has stored its one ending. This reader stops on having written that
			// frame, never on the run merely holding one: a terminal frame committed between
			// the read of the frames and the read of this state is not in hand yet, and one
			// more poll answers it, with every frame before it. A reader already past that
			// sequence is at the end of a stream that is over.
			if after >= state.Terminal {
				return
			}
		case state.PastDeadline:
			if _, err := s.endGoingRun(ctx, id, going.Fault(contracts.GoingErrorAnswerDidNotArrive)); err != nil {
				slog.Warn("dispatch going: end the run at its deadline", "request", id, "error", err)
				return
			}
			// Read again rather than writing that frame here: the answer may have stored frames
			// between this reader's last read and its ending, and they belong to the stream
			// before the frame that ends it.
		default:
			select {
			case <-ctx.Done():
				return
			case <-time.After(goingStreamPoll):
			}
		}
	}
}

// goingActorNamesSomebody reports whether an actor names any caller at all: a session or person
// id, the person a personal token belongs to, or the workload a service-account token proved. A
// credential that names none of them - the deployment's shared token, which resolves as {} -
// is nobody: comparing identities cannot tell two holders of that one token apart, so a run it
// asked for would be a run every holder could claim and none is served.
//
// It is asked twice, of the asker and of the run's author. The POST refuses an asker that
// names nobody (codeGoingAskerNamesNobody) before anything is recorded, so no row this route
// writes has such an author. The stream asks it of the author it read all the same, because
// model.Actor.SameCredential reports two credentials that name nobody as one: that comparison
// alone would serve a row authored by {} to every holder of the shared token, and the stream
// serves a run to the credential that asked it and to no other.
func goingActorNamesSomebody(actor model.Actor) bool {
	return actor.ID != "" || actor.Owner != nil || actor.Service != nil
}

// goingResumeFrom is the sequence a reader holds, from the Last-Event-ID header the stream's own
// `id:` lines set: the stream answers the frames past it. Anything that is not a sequence this
// stream could have written is no resumption point, and the whole stream is answered, which is
// what a reader with nothing in hand is answered too.
func goingResumeFrom(r *http.Request) int {
	resume, err := strconv.Atoi(strings.TrimSpace(r.Header.Get("Last-Event-ID")))
	// Past math.MaxInt32 is no sequence this stream could have written either: going_events
	// holds its sequence as an integer, so a larger value cannot bind against that column and
	// the read that carries the stream would fail rather than answer the frames past it.
	if err != nil || resume < 0 || resume > math.MaxInt32 {
		return 0
	}
	return resume
}

// writeGoingFrame writes one stored frame as server-sent events: its sequence as the frame's id,
// so a reader that drops resumes after it, its name as the event, and the stored JSON as the
// data. The payload is the bytes the record holds (scanGoingEvent keeps them), so the frame a
// reader is written is the frame that was stored.
func writeGoingFrame(w http.ResponseWriter, frame goingEvent) error {
	data, err := json.Marshal(frame.Event.Data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", frame.Sequence, frame.Event.Name, data)
	return err
}

// writeGoingRequestNotFound is the stream's one refusal; see codeGoingRequestNotFound for why
// every run a caller may not read is answered with it.
func writeGoingRequestNotFound(w http.ResponseWriter) {
	writeError(w, codeGoingRequestNotFound, http.StatusNotFound, "no answer run of that id is yours to read")
}

// getGoingCapability answers whether this deployment has a model configured, and nothing else:
// GET /api/v1/going/capability. Any caller Dispatch admits may read it, which is why the answer
// names no endpoint, no deployment and no credential - a reader learns that a question can be
// asked, never where it is answered.
func (s *server) getGoingCapability(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	WriteJSON(w, http.StatusOK, goingCapability{ModelConfigured: s.deps.Answerer != nil})
}
