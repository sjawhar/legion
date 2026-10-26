package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// One keyed path for every write (INFRA-258). A caller names the intent behind a POST or PATCH with
// an Idempotency-Key header, the same on every retry of that intent (the IETF Idempotency-Key
// draft; Google AIP-155): the dashboard mints one per press and the agents' client one per command,
// and both retry a failed fetch, a timeout, a 5xx, a 429 and a 409 KEY_IN_FLIGHT under it, none of
// which says whether the first attempt ran. keyedWrites, which register wraps around every POST and
// PATCH route, makes that retry safe with one request_keys row per key (migration 0118):
//
//   - The row is inserted in flight, in a statement of its own, before the handler runs. A repeat
//     that finds it answers from it instead: a different request under the key is 422 KEY_REUSED,
//     naming the request that used it; a first request still running (its lease, IdempotencyLease,
//     renewed every keyedLeaseRenewal while it runs) is 409 KEY_IN_FLIGHT with Retry-After: 1; a
//     finished one is replayed, its stored status and body with Idempotency-Replayed: ?1, and the
//     handler does not run again.
//   - The handler finishes whatever its client does: it runs on the request's context with the
//     client's cancellation taken off, bounded by keyedHandlerLimit and counted in the server's
//     background wait, so a client that gives up and asks again waits on the first run rather than
//     starting over.
//   - The handler runs on a writer that keeps its answer (keyedAnswer). A 2xx or a 4xx is stored and
//     the row finished in one statement; a 5xx, a 401 (a session that lapsed mid-write), a 408, a 429
//     and a panic release the key, so the next attempt runs the handler again - unless the request
//     committed a write (below).
//   - Every transaction the handler commits marks the key committed in that same transaction, when
//     it wrote anything (markRequestKeyCommitted, through the pool's store.WithBeforeCommit). A row
//     whose holder never answered - its process died - is taken over by the first repeat past the
//     lease only when it holds no such mark; one that does is answered 409 KEY_COMMITTED, since the
//     write landed and its answer was lost, and the handler never runs twice. Every write the holder
//     makes to the row is scoped to its own attempt number, so a holder that was only slow and lost
//     its key commits nothing more: its next commit is refused.
//   - A write that also delivers to sessions keeps its own recovery point (Brandur Leach,
//     "Implementing Stripe-like Idempotency Keys in Postgres"): it commits what it made into the
//     row in its own transaction (bindRequestKeyRecovery), and a repeat that finds that point is
//     answered by its route's keyedReplay - whatever delivery is still owed put on its way, then
//     what it made read again - rather than from the stored bytes or KEY_COMMITTED, however its
//     first request ended: the single-session send, the broadcast, an issue message and a comment,
//     whose mentions deliver it. Their first answer shows deliveries the repeat must report as they
//     stand now. A delivery a dead process owed is made once whether anyone asks again or not: the
//     delivery sweep resumes it (resumeOwedDeliveries), and a repeat only sooner.
//   - Every answer to a keyed request the layer judged carries the key back in the Idempotency-Key
//     response header, a 5xx included: an earlier Dispatch ignores the header and answers without
//     it, so a client retries a 5xx under its key only where the echo says the key was judged.
//
// A write without the header runs as it always did, unkeyed, and is logged at DEBUG: a script, a
// test and the Legion daemon's outbox send writes that name no intent, and are expected to.

const (
	// keyedAnswerKept is the largest answer a key keeps. A larger one is still answered in full, but
	// stored as its status alone, and its replay says so (IdempotencyAnswerNotKept).
	keyedAnswerKept = 1 << 20
	// keyedBodyInMemory is how much of a keyed request's body is held in memory while its digest is
	// judged; the rest of an upload's body waits in a temporary file, as the multipart parser's own
	// spill does, so a keyed 25 MiB upload costs no more memory than an unkeyed one.
	keyedBodyInMemory = 1 << 20
	// keyedBodyMax is the largest body an upload route reads: an upload's file and a megabyte of
	// fields (uploadArtifactFor). Every other route reads a JSON body of at most
	// maxJSONRequestBytes (keyedBodyLimit).
	keyedBodyMax = maxArtifactBlobSize + 1<<20
	// keyedBookkeeping bounds each statement the layer makes after the handler: they run on past a
	// client that went away, since its retry needs the answer recorded.
	keyedBookkeeping = 10 * time.Second
	// keyedClaimTries bounds how often one request goes round the claim's insert, read and takeover
	// when another request moves the row between them; each pass is one such move.
	keyedClaimTries = 4
	// keyedHandlerLimit bounds a keyed handler's run once its client's cancellation no longer
	// does (keyedWrites): past it the run's context ends, its work rolls back, and the key is
	// released as for a 5xx. It is far past what any route takes - an architecture refresh's
	// imports each end at two minutes - and only stops a run that would otherwise hold its key,
	// its connections and the shutdown wait for ever.
	keyedHandlerLimit = 10 * time.Minute
	// keyedLeaseRenewal is how often a running keyed handler renews its key's lease, a third of
	// IdempotencyLease, so two renewals can fail in a row before a repeat presumes the run dead.
	keyedLeaseRenewal = 20 * time.Second
)

// requestKeyAgentToken is whom a key sent with the shared agent token belongs to: that token names
// no person.
const requestKeyAgentToken = "agent-token"

var requestKeyPattern = regexp.MustCompile(contracts.IdempotencyKeyPattern)

// maskedBoundary stands in a multipart body's digest for every occurrence of its boundary, which a
// client mints afresh for every attempt of one upload. NUL cannot appear in a boundary.
var maskedBoundary = []byte("\x00boundary\x00")

// errRequestKeyTakenOver is a holder's refusal to commit what it made once a repeat past the lease
// has taken its key over: the transaction rolls back, so the key's request is made once.
var errRequestKeyTakenOver = errorf(http.StatusConflict, contracts.IdempotencyKeyInFlight,
	"another attempt under this %s took the request over; this attempt was not run: ask again under the same key",
	contracts.IdempotencyKeyHeader)

// keyedReplay answers a repeat of a keyed write whose first run committed recovery
// (bindRequestKeyRecovery): it puts on its way whatever the write still owes, then answers what the
// write made as it stands now, with the status the route's write answers - a stale success, in
// Google AIP-155's words, never a second write. stored is the first answer's body when the key kept
// one, and nil when the first request never answered.
type keyedReplay func(w http.ResponseWriter, r *http.Request, recovery json.RawMessage, stored []byte)

// keyedReplays is every route whose write commits a recovery point, by method and pattern: the
// writes that also deliver to sessions, whose deliveries a repeat finishes.
func (s *server) keyedReplays() map[string]keyedReplay {
	return map[string]keyedReplay{
		http.MethodPost + " /api/v1/agents/{session_id}/messages": s.replayAgentMessage,
		http.MethodPost + " /api/v1/broadcasts":                   s.replayBroadcast,
		http.MethodPost + " /api/v1/issues/{key}/messages":        s.replayIssueMessage,
		http.MethodPost + " /api/v1/issues/{key}/comments":        s.replayComment,
		http.MethodPost + " /api/v1/artifacts/{id}/comments":      s.replayComment,
	}
}

// writeRefreshed answers a keyed replay with fresh, the resource as it stands now, in the first
// answer's shape: the members stored holds that fresh does not (a write's advice, a reply's
// ask_waiting_on) kept, and every member fresh carries written over them. With no stored answer it
// answers fresh alone. Its status is the write's own 201.
func (s *server) writeRefreshed(w http.ResponseWriter, stored []byte, fresh any) {
	encoded, err := json.Marshal(fresh)
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("encode a replayed resource: %w", err))
		return
	}
	var current map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &current); err != nil {
		s.writeHandlerError(w, fmt.Errorf("read a replayed resource: %w", err))
		return
	}
	var answer map[string]json.RawMessage
	if len(stored) > 0 && json.Unmarshal(stored, &answer) == nil && answer != nil {
		maps.Copy(answer, current)
		current = answer
	}
	WriteJSON(w, http.StatusCreated, current)
}

// keyedMethod reports whether register keys a route of this method: a POST or a PATCH. A PUT and a
// DELETE are idempotent already, and a GET writes nothing.
func keyedMethod(method string) bool {
	return method == http.MethodPost || method == http.MethodPatch
}

// answerHoldsSecret is every route whose answer carries a credential, by method and pattern. A key
// never keeps such an answer: request_keys would hold the plaintext for a day where the route's own
// table holds only its hash. The request is released as a 5xx's is, so a write it committed answers
// a repeat with 409 KEY_COMMITTED - made, and not shown again - and one it did not runs again.
var answerHoldsSecret = map[string]bool{
	http.MethodPost + " /api/v1/me/agent-tokens": true,
}

// requestKeyClaim is a keyed request's hold on its key: whose key, which key, and the attempt that
// holds it now.
type requestKeyClaim struct {
	login   string
	key     string
	attempt int
}

// requestKeyClaimKey carries a request's requestKeyClaim to its handler, for
// bindRequestKeyRecovery.
type requestKeyClaimKey struct{}

// resolvedCallerKey carries the caller keyedWrites resolved to its handler, so optionalActor does
// not verify one bearer twice in a request.
type resolvedCallerKey struct{}

type resolvedCaller struct {
	actor model.Actor
	human bool
}

// keyedWrites wraps the handler of a POST or PATCH route, method and pattern, in the keyed path
// above. replay is the route's keyedReplay, nil for a route that commits no recovery point.
//
// A keyed request's handler finishes on the server whatever its client does: it runs on its
// request's context with the client's cancellation taken off (context.WithoutCancel), bounded by
// keyedHandlerLimit instead, and counted in the server's background wait, so a shutdown lets it
// finish as it lets a delivery finish. A client that gives up - the page's attempt cut off by its
// own silence bound, a closed tab, a proxy's timeout - asks again under its key and is answered
// 409 KEY_IN_FLIGHT for as long as the first run lives, then that run's stored answer. While the
// handler runs its key's lease is renewed every keyedLeaseRenewal, so only a run whose process
// died is taken over, once its lease lapses. A handler cut off with its client would roll its
// transactions back and leave the next attempt to start again from nothing, so a write that takes
// the server longer than the client waits could never land.
func (s *server) keyedWrites(method, pattern string, handler http.HandlerFunc, replay keyedReplay) http.HandlerFunc {
	wildcards := patternWildcards(pattern)
	keepsAnswer := !answerHoldsSecret[method+" "+pattern]
	bodyLimit := keyedBodyLimit(method, pattern)
	run := handler
	if s.deps.TestHooksEnabled {
		run = s.slowedByTestHook(handler)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		key, present, err := requestKeyHeader(r)
		if !present {
			slog.Debug("dispatch: a write without an Idempotency-Key runs unkeyed", "method", method, "route", pattern)
			handler(w, r)
			return
		}
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		actor, human, err := s.optionalActor(r)
		if err != nil {
			// The handler refuses a caller it cannot resolve as it always has, and nothing is kept.
			handler(w, r)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), resolvedCallerKey{}, resolvedCaller{actor: actor, human: human}))
		if r.ContentLength > bodyLimit {
			handler(w, r)
			return
		}
		digest := sha256.New()
		writeRequestFields(digest, r, method, pattern, wildcards)
		body, err := readKeyedBody(r.Body, digest, r.Header.Get("Content-Type"), bodyLimit)
		defer body.close()
		var spill *keyedSpillError
		switch {
		case errors.As(err, &spill):
			// Dispatch's own failure, not the request's: answered as a 5xx under the key, which
			// nothing has claimed, so the client asks again under it.
			w.Header().Set(contracts.IdempotencyKeyHeader, key)
			s.writeHandlerError(w, err)
			return
		case err != nil:
			writeError(w, "REQUEST_BODY_UNREADABLE", http.StatusBadRequest, "could not read the request body: "+err.Error())
			return
		}
		replayable, err := body.reader()
		if err != nil {
			w.Header().Set(contracts.IdempotencyKeyHeader, key)
			s.writeHandlerError(w, fmt.Errorf("reread a keyed request's body: %w", err))
			return
		}
		if body.size > bodyLimit {
			r.Body = io.NopCloser(io.MultiReader(replayable, r.Body))
			handler(w, r)
			return
		}
		r.Body = io.NopCloser(replayable)

		// Every answer from here on - run, replayed or refused, stored or not, a 5xx included - names
		// the key back, so a client can tell a Dispatch that judged its key from one that ignored the
		// header, and retries a 5xx under the key only against the first: an earlier Dispatch would
		// run the write again.
		w.Header().Set(contracts.IdempotencyKeyHeader, key)
		claim, answered := s.claimRequestKey(w, r, requestKeyLogin(actor, human), key, hex.EncodeToString(digest.Sum(nil)), replay)
		if answered {
			return
		}
		s.detached.start()
		defer s.detached.done()
		detached, stop := context.WithTimeout(context.WithoutCancel(r.Context()), keyedHandlerLimit)
		defer stop()
		stopRenewing := s.renewRequestKeyLease(claim)
		answer := &keyedAnswer{w: w}
		// running bounds the commit step to the handler: a delivery the handler detached (a
		// broadcast's recipients) commits after the answer, and is not the request's write.
		var running atomic.Bool
		running.Store(true)
		settled := false
		defer func() {
			stopRenewing()
			running.Store(false)
			// A handler that panicked answered nothing worth keeping: the key is released, so the
			// next attempt runs again unless the request had committed a write. The panic itself
			// carries on to net/http with its own stack.
			if !settled {
				s.releaseRequestKey(detached, claim)
			}
		}()
		runs := context.WithValue(detached, requestKeyClaimKey{}, claim)
		runs = store.WithBeforeCommit(runs, markRequestKeyCommitted(claim, &running))
		run(answer, r.WithContext(runs))
		// The lease stops moving before the answer is kept, so a renewal never lands on a row the
		// answer released and holds a repeat at KEY_IN_FLIGHT for a lease it no longer has.
		stopRenewing()
		running.Store(false)
		s.settleRequestKey(detached, claim, answer, keepsAnswer)
		settled = true
		answer.deliver()
	}
}

// keyedBodyLimit is the largest body a keyed route reads, the most the layer spools before its
// handler runs: an upload's file and a megabyte of fields (uploadArtifactFor) for an upload route,
// and a JSON body (decodeJSON) for every other. A body past it is refused by the route's handler
// whatever its key, so it runs unkeyed rather than spooled.
func keyedBodyLimit(method, pattern string) int64 {
	if keyedUploads[method+" "+pattern] {
		return keyedBodyMax
	}
	return maxJSONRequestBytes
}

// keyedUploads is every keyed route whose body may hold a file (uploadArtifactFor).
var keyedUploads = map[string]bool{
	http.MethodPost + " /api/v1/issues/{key}/artifacts":        true,
	http.MethodPost + " /api/v1/projects/{key}/artifacts":      true,
	http.MethodPost + " /api/v1/agents/{session_id}/artifacts": true,
}

// keyLease is how long a keyed request holds its key without a renewal before a repeat presumes
// its process died (IdempotencyLease; requestKeyLease in a test).
func (s *server) keyLease() time.Duration {
	if s.requestKeyLease > 0 {
		return s.requestKeyLease
	}
	return contracts.IdempotencyLease
}

// renewRequestKeyLease renews claim's lease every keyedLeaseRenewal until the returned stop is
// called, and stops by itself once a renewal finds the key no longer its own. stop waits for a
// renewal under way and may be called more than once. Each renewal is its own statement on a
// connection of its own, beside whatever transaction the handler holds.
func (s *server) renewRequestKeyLease(claim requestKeyClaim) (stop func()) {
	every := keyedLeaseRenewal
	if s.requestKeyRenewal > 0 {
		every = s.requestKeyRenewal
	}
	stopped, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-stopped:
				return
			case <-ticker.C:
				if !s.renewRequestKey(claim) {
					return
				}
			}
		}
	}()
	return sync.OnceFunc(func() {
		close(stopped)
		<-finished
	})
}

// renewRequestKey moves claim's lease to now and reports whether the key is still claim's: a row
// another attempt took over, or one already answered or released, is left as it is. A renewal that
// fails is logged and tried again at the next tick: the lease lapses only once renewals have
// failed for all of it.
func (s *server) renewRequestKey(claim requestKeyClaim) bool {
	ctx, cancel := context.WithTimeout(store.WithTransactionTracking(context.Background()), keyedBookkeeping)
	defer cancel()
	tag, err := s.deps.Store.Pool.Exec(ctx, `
		update request_keys set leased_at = now()
		where login = $1 and idempotency_key = $2 and attempt = $3 and status is null and leased_at is not null
	`, claim.login, claim.key, claim.attempt)
	if err != nil {
		slog.Warn("dispatch: renew a keyed request's lease", "error", err)
		return true
	}
	return tag.RowsAffected() == 1
}

// requestKeyHeader reads a request's Idempotency-Key, bare or as a structured-field string (in
// double quotes). present is false when the request names none; a header sent twice, or one that
// is not 1 to IdempotencyKeyMax letters, digits, '.', '_', ':' or '-', is
// 400 IdempotencyKeyInvalid.
func requestKeyHeader(r *http.Request) (key string, present bool, err error) {
	values := r.Header.Values(contracts.IdempotencyKeyHeader)
	if len(values) == 0 {
		return "", false, nil
	}
	invalid := errorf(http.StatusBadRequest, contracts.IdempotencyKeyInvalid,
		"%s must be sent once, as 1 to %d letters, digits, '.', '_', ':' or '-', bare or in double quotes",
		contracts.IdempotencyKeyHeader, contracts.IdempotencyKeyMax)
	if len(values) != 1 {
		return "", true, invalid
	}
	key = strings.TrimSpace(values[0])
	if len(key) >= 2 && key[0] == '"' && key[len(key)-1] == '"' {
		key = key[1 : len(key)-1]
	}
	if !requestKeyPattern.MatchString(key) {
		return "", true, invalid
	}
	return key, true, nil
}

// requestKeyLogin is whom a caller's keys belong to: a person's canonical login, whether they write
// from the dashboard or through an agent's personal token, a service token's subject, or the shared
// agent token, which names nobody.
func requestKeyLogin(actor model.Actor, human bool) string {
	switch {
	case human:
		return model.CanonicalLogin(actor.ID)
	case actor.Owner != nil:
		return model.CanonicalLogin(*actor.Owner)
	case actor.Service != nil:
		return "service:" + *actor.Service
	default:
		return requestKeyAgentToken
	}
}

// patternWildcards names a route pattern's wildcards in order, the path values a request's digest
// covers.
func patternWildcards(pattern string) []string {
	var names []string
	for segment := range strings.SplitSeq(pattern, "/") {
		if len(segment) < 3 || segment[0] != '{' || segment[len(segment)-1] != '}' {
			continue
		}
		if name := strings.TrimSuffix(segment[1:len(segment)-1], "..."); name != "$" {
			names = append(names, name)
		}
	}
	return names
}

// writeRequestFields writes to digest everything a keyed request is judged by but its body: the
// method, the route pattern, its path values, the query, and the media type with its parameters,
// less a multipart boundary. Each is length-prefixed, so no two requests' fields run together into
// one digest.
func writeRequestFields(digest hash.Hash, r *http.Request, method, pattern string, wildcards []string) {
	field := func(value string) {
		var length [binary.MaxVarintLen64]byte
		digest.Write(length[:binary.PutUvarint(length[:], uint64(len(value)))])
		io.WriteString(digest, value)
	}
	field(method)
	field(pattern)
	for _, name := range wildcards {
		field(r.PathValue(name))
	}
	field(r.URL.Query().Encode())
	contentType := strings.TrimSpace(r.Header.Get("Content-Type"))
	if mediaType, parameters, err := mime.ParseMediaType(contentType); err == nil {
		delete(parameters, "boundary")
		contentType = mime.FormatMediaType(mediaType, parameters)
	}
	field(contentType)
}

// keyedBody is a keyed request's body, read whole before its handler runs so its digest can be
// judged: its first keyedBodyInMemory bytes in memory, and the rest, if any, in a temporary file.
type keyedBody struct {
	head  []byte
	spill *os.File
	size  int64
}

// keyedSpillError is a keyed body's spill to its temporary file failing: Dispatch's own fault (a
// full disk, a temporary directory it cannot write), answered as a 5xx, where a body the client
// did not finish sending is the request's and answered 400.
type keyedSpillError struct{ err error }

func (e *keyedSpillError) Error() string {
	return "spool a keyed request's body to a temporary file: " + e.err.Error()
}

func (e *keyedSpillError) Unwrap() error { return e.err }

// spillWriter is a keyed body's temporary file, whose write failures are keyedSpillErrors.
type spillWriter struct{ file *os.File }

func (w spillWriter) Write(p []byte) (int, error) {
	n, err := w.file.Write(p)
	if err != nil {
		return n, &keyedSpillError{err: err}
	}
	return n, nil
}

// readKeyedBody reads body into a keyedBody, at most one byte past limit, the route's own
// (keyedBodyLimit), writing it to digest as it goes; for a multipart contentType, with its boundary
// masked (boundaryMask). A body that fits in keyedBodyInMemory never touches the disk: a route
// whose limit is no more than that holds even its one byte past the limit in memory.
func readKeyedBody(body io.Reader, digest io.Writer, contentType string, limit int64) (*keyedBody, error) {
	read := &keyedBody{}
	target := digest
	var mask *boundaryMask
	if mediaType, parameters, err := mime.ParseMediaType(contentType); err == nil &&
		strings.HasPrefix(mediaType, "multipart/") && parameters["boundary"] != "" {
		mask = &boundaryMask{w: digest, boundary: []byte(parameters["boundary"])}
		target = mask
	}
	source := io.TeeReader(io.LimitReader(body, limit+1), target)
	inMemory := int64(keyedBodyInMemory)
	if limit <= inMemory {
		inMemory = limit + 1
	}
	var head bytes.Buffer
	n, err := io.CopyN(&head, source, inMemory)
	read.head, read.size = head.Bytes(), n
	switch {
	case err != nil && !errors.Is(err, io.EOF):
		return read, err
	case err == nil && n <= limit:
		// The head is full and the limit not reached: the rest waits in a temporary file.
		spill, err := os.CreateTemp("", "dispatch-keyed-body-*")
		if err != nil {
			return read, &keyedSpillError{err: err}
		}
		read.spill = spill
		rest, err := io.Copy(spillWriter{file: spill}, source)
		read.size += rest
		if err != nil {
			return read, err
		}
	}
	if mask != nil {
		mask.flush()
	}
	return read, nil
}

// reader is the body again, from its first byte.
func (b *keyedBody) reader() (io.Reader, error) {
	if b.spill == nil {
		return bytes.NewReader(b.head), nil
	}
	if _, err := b.spill.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return io.MultiReader(bytes.NewReader(b.head), b.spill), nil
}

// close removes the body's temporary file, if it has one.
func (b *keyedBody) close() {
	if b == nil || b.spill == nil {
		return
	}
	name := b.spill.Name()
	_ = b.spill.Close()
	_ = os.Remove(name)
}

// boundaryMask writes what it is given to w with every occurrence of boundary replaced by
// maskedBoundary, holding back the tail that could still begin one until the next write or flush.
type boundaryMask struct {
	w        io.Writer
	boundary []byte
	pending  []byte
}

func (m *boundaryMask) Write(p []byte) (int, error) {
	m.pending = append(m.pending, p...)
	for {
		at := bytes.Index(m.pending, m.boundary)
		if at < 0 {
			break
		}
		m.w.Write(m.pending[:at])
		m.w.Write(maskedBoundary)
		m.pending = m.pending[at+len(m.boundary):]
	}
	if keep := len(m.boundary) - 1; len(m.pending) > keep {
		m.w.Write(m.pending[:len(m.pending)-keep])
		m.pending = append(m.pending[:0], m.pending[len(m.pending)-keep:]...)
	}
	return len(p), nil
}

func (m *boundaryMask) flush() {
	m.w.Write(m.pending)
	m.pending = nil
}

// heldRequestKey is the row a repeat found under its key.
type heldRequestKey struct {
	digest      string
	method      string
	path        string
	attempt     int
	status      *int
	contentType *string
	response    []byte
	kept        bool
	recovery    json.RawMessage
	committed   bool
	lapsed      bool
	expired     bool
}

// storedAnswer is the first request's body as the key kept it, or nil when it holds none.
func (held heldRequestKey) storedAnswer() []byte {
	if held.status == nil || !held.kept {
		return nil
	}
	return held.response
}

// claimRequestKey takes login's key for r, whose digest is digest, and answers whatever the key
// already holds instead: answered reports that it did, and that the handler must not run. Every
// statement is its own transaction, so no lock outlives it; a row another request moved between
// two of them sends this one round again.
func (s *server) claimRequestKey(
	w http.ResponseWriter, r *http.Request, login, key, digest string, replay keyedReplay,
) (claim requestKeyClaim, answered bool) {
	ctx, pool := r.Context(), s.deps.Store.Pool
	lease, ttl := s.keyLease().Seconds(), contracts.IdempotencyKeyTTL.Seconds()
	for range keyedClaimTries {
		claim = requestKeyClaim{login: login, key: key}
		err := pool.QueryRow(ctx, `
			insert into request_keys (login, idempotency_key, request_digest, method, path)
			values ($1, $2, $3, $4, $5)
			on conflict do nothing
			returning attempt
		`, login, key, digest, r.Method, r.URL.Path).Scan(&claim.attempt)
		if err == nil {
			return claim, false
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			s.writeHandlerError(w, fmt.Errorf("claim an idempotency key: %w", err))
			return claim, true
		}
		var held heldRequestKey
		err = pool.QueryRow(ctx, `
			select request_digest, method, path, attempt, status, content_type, response,
			       response is not null, recovery, committed_at is not null,
			       leased_at is null or leased_at < now() - make_interval(secs => $3),
			       created_at < now() - make_interval(secs => $4)
			from request_keys where login = $1 and idempotency_key = $2
		`, login, key, lease, ttl).Scan(
			&held.digest, &held.method, &held.path, &held.attempt, &held.status, &held.contentType,
			&held.response, &held.kept, &held.recovery, &held.committed, &held.lapsed, &held.expired,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			s.writeHandlerError(w, fmt.Errorf("read an idempotency key: %w", err))
			return claim, true
		}
		switch {
		case held.expired:
			// Past its window and not yet swept: the key is unused again.
			if _, err := pool.Exec(ctx, `
				delete from request_keys
				where login = $1 and idempotency_key = $2 and created_at < now() - make_interval(secs => $3)
			`, login, key, ttl); err != nil {
				s.writeHandlerError(w, fmt.Errorf("expire an idempotency key: %w", err))
				return claim, true
			}
			continue
		case held.digest != digest:
			writeError(w, contracts.IdempotencyKeyReused, http.StatusUnprocessableEntity, fmt.Sprintf(
				"%s %q was already used for %s %s with different content; this request was not run: a changed request needs a new key",
				contracts.IdempotencyKeyHeader, key, held.method, held.path))
			return claim, true
		case held.recovery != nil && replay != nil && (held.status != nil || held.lapsed):
			w.Header().Set(contracts.IdempotencyReplayedHeader, "?1")
			replay(w, r, held.recovery, held.storedAnswer())
			return claim, true
		case held.status != nil:
			replayStoredAnswer(w, held)
			return claim, true
		case !held.lapsed:
			writeKeyInFlight(w, key)
			return claim, true
		case held.committed || held.recovery != nil:
			// The holder committed its write and its process died, or it failed, before its answer
			// was kept: running the request again would make the write twice.
			writeKeyCommitted(w, key, held)
			return claim, true
		}
		// The holder's lease lapsed with nothing committed: its process died before its write did,
		// so this attempt takes the key over and runs the request.
		tag, err := pool.Exec(ctx, `
			update request_keys set attempt = attempt + 1, leased_at = now()
			where login = $1 and idempotency_key = $2 and attempt = $3
			  and status is null and recovery is null and committed_at is null
		`, login, key, held.attempt)
		if err != nil {
			s.writeHandlerError(w, fmt.Errorf("take over an idempotency key: %w", err))
			return claim, true
		}
		if tag.RowsAffected() == 1 {
			claim.attempt = held.attempt + 1
			return claim, false
		}
	}
	writeKeyInFlight(w, key)
	return claim, true
}

// writeKeyInFlight answers a repeat whose first request still holds the key.
func writeKeyInFlight(w http.ResponseWriter, key string) {
	w.Header().Set("Retry-After", "1")
	writeError(w, contracts.IdempotencyKeyInFlight, http.StatusConflict, fmt.Sprintf(
		"the first request under %s %q is still running; this one was not run: ask again under the same key",
		contracts.IdempotencyKeyHeader, key))
}

// writeKeyCommitted answers a repeat whose first request committed its write and lost its answer:
// Dispatch does not run the request again, and has nothing to replay (Google AIP-155's lost
// answer; a route that commits a recovery point answers the resource instead).
func writeKeyCommitted(w http.ResponseWriter, key string, held heldRequestKey) {
	writeError(w, contracts.IdempotencyKeyCommitted, http.StatusConflict, fmt.Sprintf(
		"the write under %s %q (%s %s) landed, but Dispatch lost its answer, so it is not run again and has no answer to replay: read what it wrote",
		contracts.IdempotencyKeyHeader, key, held.method, held.path))
}

// replayStoredAnswer answers a repeat with the first request's stored answer.
func replayStoredAnswer(w http.ResponseWriter, held heldRequestKey) {
	w.Header().Set(contracts.IdempotencyReplayedHeader, "?1")
	if !held.kept {
		writeError(w, contracts.IdempotencyAnswerNotKept, *held.status, fmt.Sprintf(
			"Dispatch ran this request and answered %d, but its answer was larger than the %d bytes a key keeps, so it is not replayed: read what the request wrote to see it",
			*held.status, keyedAnswerKept))
		return
	}
	if held.contentType != nil {
		w.Header().Set("Content-Type", *held.contentType)
	}
	w.WriteHeader(*held.status)
	_, _ = w.Write(held.response)
}

// storedStatus reports whether an answer of this status is a key's answer. A 5xx says nothing of
// whether the write could succeed, and a 401 (a session that lapsed mid-write), a 408 and a 429 say
// only that this attempt was not heard: each leaves the next attempt to run the request again.
func storedStatus(status int) bool {
	return status < http.StatusInternalServerError && status != http.StatusUnauthorized &&
		status != http.StatusRequestTimeout && status != http.StatusTooManyRequests
}

// settleRequestKey records what claim's handler answered: a stored status finishes the row with
// it, and any other - or any answer of a route that never keeps one (answerHoldsSecret) - releases
// the key (releaseRequestKey). A row another attempt has taken over is left as it is. A failure is
// logged and the caller is still answered, since the handler has run: its row stays in flight, and
// a repeat past the lease is answered KEY_COMMITTED (or from its recovery point) when the request
// committed a write, and runs it again when it did not.
func (s *server) settleRequestKey(ctx context.Context, claim requestKeyClaim, answer *keyedAnswer, keepsAnswer bool) {
	status := answer.finalStatus()
	if !storedStatus(status) || !keepsAnswer {
		s.releaseRequestKey(ctx, claim)
		return
	}
	var response []byte
	if !answer.passed {
		// Non-nil, so an empty answer is stored as empty rather than as one not kept.
		response = append([]byte{}, answer.body.Bytes()...)
	}
	var contentType *string
	if value := answer.Header().Get("Content-Type"); value != "" {
		contentType = &value
	}
	bookkeeping, cancel := context.WithTimeout(context.WithoutCancel(ctx), keyedBookkeeping)
	defer cancel()
	if _, err := s.deps.Store.Pool.Exec(bookkeeping, `
		update request_keys set status = $4, content_type = $5, response = $6, leased_at = null
		where login = $1 and idempotency_key = $2 and attempt = $3 and status is null
	`, claim.login, claim.key, claim.attempt, status, contentType, response); err != nil {
		slog.Error("dispatch: store a keyed request's answer", "status", status, "error", err)
	}
}

// releaseRequestKey gives up claim's key when its handler answered nothing worth keeping. A row
// whose request committed nothing is deleted, so the next attempt runs the request again. One
// whose request committed a write - its recovery point, or any write markRequestKeyCommitted
// marked - keeps it, and is released at once, so the next attempt is answered from that point, or
// KEY_COMMITTED, rather than making the write a second time.
func (s *server) releaseRequestKey(ctx context.Context, claim requestKeyClaim) {
	bookkeeping, cancel := context.WithTimeout(context.WithoutCancel(ctx), keyedBookkeeping)
	defer cancel()
	if _, err := s.deps.Store.Pool.Exec(bookkeeping, `
		with released as (
			update request_keys set leased_at = null
			where login = $1 and idempotency_key = $2 and attempt = $3 and status is null
			  and (recovery is not null or committed_at is not null)
		)
		delete from request_keys
		where login = $1 and idempotency_key = $2 and attempt = $3 and status is null
		  and recovery is null and committed_at is null
	`, claim.login, claim.key, claim.attempt); err != nil {
		slog.Error("dispatch: release a keyed request's key", "error", err)
	}
}

// markRequestKeyCommitted is the step every transaction of claim's handler takes before its COMMIT
// (store.WithBeforeCommit), while running holds: a transaction that wrote anything - Postgres
// assigned it a transaction id - marks the key committed in that same transaction, so the mark
// commits with the request's write or not at all, and a repeat that finds the key in flight and
// its holder gone is answered KEY_COMMITTED instead of running the write again. A transaction that
// only read marks nothing. A holder whose key a repeat has taken over is refused
// (errRequestKeyTakenOver) and its write rolls back, so the request's write is made once.
//
// running ends when the handler returns: a delivery the handler detached commits after its
// answer, and is no write of the request's.
//
// Only a transaction the pool opens takes the step: a write a handler makes as a bare pool
// statement commits on its own and marks nothing, so a keyed route writes inside s.begin. Every
// POST and PATCH route does (the token mint was the one bare insert, and now begins its own).
func markRequestKeyCommitted(claim requestKeyClaim, running *atomic.Bool) store.BeforeCommit {
	return func(ctx context.Context, tx pgx.Tx) error {
		if !running.Load() {
			return nil
		}
		// One statement: an update whose condition holds only for a transaction that wrote, and
		// the same question asked beside it, so a read-only transaction writes and asks nothing
		// more, and a written one learns whether its key is still its own.
		var wrote, marked bool
		if err := tx.QueryRow(ctx, `
			with marked as (
				update request_keys set committed_at = coalesce(committed_at, now())
				where login = $1 and idempotency_key = $2 and attempt = $3
				  and txid_current_if_assigned() is not null
				returning 1
			)
			select txid_current_if_assigned() is not null, exists (select 1 from marked)
		`, claim.login, claim.key, claim.attempt).Scan(&wrote, &marked); err != nil {
			return fmt.Errorf("mark an idempotency key committed: %w", err)
		}
		if wrote && !marked {
			return errRequestKeyTakenOver
		}
		return nil
	}
}

// bindRequestKeyRecovery commits point as the recovery point of the keyed request ctx carries, in
// tx, the transaction that commits what the request made, so the two commit together or not at all.
// A request without a key binds nothing. A request whose key a repeat has taken over is refused
// (errRequestKeyTakenOver), and its transaction must roll back: the repeat makes the write instead.
func bindRequestKeyRecovery(ctx context.Context, tx pgx.Tx, point any) error {
	claim, ok := ctx.Value(requestKeyClaimKey{}).(requestKeyClaim)
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(point)
	if err != nil {
		return fmt.Errorf("encode a recovery point: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		update request_keys set recovery = $4
		where login = $1 and idempotency_key = $2 and attempt = $3 and status is null and recovery is null
	`, claim.login, claim.key, claim.attempt, encoded)
	if err != nil {
		return fmt.Errorf("commit a recovery point: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errRequestKeyTakenOver
	}
	return nil
}

// keyedAnswer is the writer a keyed request's handler answers on. It holds the answer until the
// key has stored it, so a client that is answered finds its key answered too; an answer past
// keyedAnswerKept is written through as it comes, and kept as its status alone.
type keyedAnswer struct {
	w      http.ResponseWriter
	status int
	body   bytes.Buffer
	passed bool
}

func (a *keyedAnswer) Header() http.Header { return a.w.Header() }

func (a *keyedAnswer) WriteHeader(status int) {
	if a.status == 0 {
		a.status = status
	}
}

func (a *keyedAnswer) Write(p []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	if a.passed {
		return a.w.Write(p)
	}
	if a.body.Len()+len(p) <= keyedAnswerKept {
		return a.body.Write(p)
	}
	a.passed = true
	a.w.WriteHeader(a.status)
	if _, err := a.w.Write(a.body.Bytes()); err != nil {
		return 0, err
	}
	a.body = bytes.Buffer{}
	return a.w.Write(p)
}

// finalStatus is the status the handler answered, 200 for one that wrote nothing.
func (a *keyedAnswer) finalStatus() int {
	if a.status == 0 {
		return http.StatusOK
	}
	return a.status
}

// deliver writes the held answer to the client.
func (a *keyedAnswer) deliver() {
	if a.passed {
		return
	}
	a.w.WriteHeader(a.finalStatus())
	_, _ = a.w.Write(a.body.Bytes())
}

// sweepRequestKeys deletes every key older than IdempotencyKeyTTL, the window a key is kept for,
// and every row a process that died left in flight with it.
func sweepRequestKeys(ctx context.Context, pool *store.Pool) error {
	_, err := pool.Exec(ctx, `
		delete from request_keys where created_at < now() - make_interval(secs => $1)
	`, contracts.IdempotencyKeyTTL.Seconds())
	return err
}
