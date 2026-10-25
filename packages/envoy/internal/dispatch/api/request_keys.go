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
	"mime"
	"net/http"
	"os"
	"regexp"
	"strings"
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
// PATCH route, makes that retry safe with one request_keys row per key (migration 0116):
//
//   - The row is inserted in flight, in a statement of its own, before the handler runs. A repeat
//     that finds it answers from it instead: a different request under the key is 422 KEY_REUSED,
//     naming the request that used it; a first request still running (inside IdempotencyLease) is
//     409 KEY_IN_FLIGHT with Retry-After: 1; a finished one is replayed, its stored status and body
//     with Idempotency-Replayed: ?1, and the handler does not run again.
//   - The handler runs on a writer that keeps its answer (keyedAnswer). A 2xx or a 4xx is stored and
//     the row finished in one statement; a 5xx, a 401 (a session that lapsed mid-write), a 408, a 429
//     and a panic delete the row, so the next attempt runs the handler again.
//   - A row whose holder never finished - its process died - is taken over by the first repeat past
//     the lease. Every write the holder makes to the row is scoped to its own attempt number, so a
//     holder that was only slow changes nothing once it has lost the key.
//   - A write that also mutates foreign state keeps its own recovery point (Brandur Leach,
//     "Implementing Stripe-like Idempotency Keys in Postgres"): it commits what it made into the
//     row in its own transaction (bindRequestKeyRecovery), and a repeat that finds that point is
//     answered by its route's keyedReplay - whatever delivery is still owed resumed first, then what
//     it made read again - rather than from the stored bytes, however its first request ended. The
//     single-session send and the broadcast are the two: their first answer shows deliveries the
//     repeat must report as they stand now, and a send must never be made twice, even across a
//     crash between its commit and its stored answer.
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
	// keyedBodyMax is the largest body any route reads: an upload's file and a megabyte of fields
	// (uploadArtifactFor). A body past it is refused by its handler whatever its key, so it runs
	// unkeyed rather than spooled.
	keyedBodyMax = maxArtifactBlobSize + 1<<20
	// keyedBookkeeping bounds each statement the layer makes after the handler: they run on past a
	// client that went away, since its retry needs the answer recorded.
	keyedBookkeeping = 10 * time.Second
	// keyedClaimTries bounds how often one request goes round the claim's insert, read and takeover
	// when another request moves the row between them; each pass is one such move.
	keyedClaimTries = 4
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
// write made as it stands now, with the status the route's write answers.
type keyedReplay func(w http.ResponseWriter, r *http.Request, recovery json.RawMessage)

// keyedReplays is every route whose write commits a recovery point, by method and pattern.
func (s *server) keyedReplays() map[string]keyedReplay {
	return map[string]keyedReplay{
		http.MethodPost + " /api/v1/agents/{session_id}/messages": s.replayAgentMessage,
		http.MethodPost + " /api/v1/broadcasts":                   s.replayBroadcast,
	}
}

// keyedMethod reports whether register keys a route of this method: a POST or a PATCH. A PUT and a
// DELETE are idempotent already, and a GET writes nothing.
func keyedMethod(method string) bool {
	return method == http.MethodPost || method == http.MethodPatch
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
func (s *server) keyedWrites(method, pattern string, handler http.HandlerFunc, replay keyedReplay) http.HandlerFunc {
	wildcards := patternWildcards(pattern)
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
		if r.ContentLength > keyedBodyMax {
			handler(w, r)
			return
		}
		digest := sha256.New()
		writeRequestFields(digest, r, method, pattern, wildcards)
		body, err := readKeyedBody(r.Body, digest, r.Header.Get("Content-Type"))
		defer body.close()
		if err != nil {
			writeError(w, "REQUEST_BODY_UNREADABLE", http.StatusBadRequest, "could not read the request body: "+err.Error())
			return
		}
		replayable, err := body.reader()
		if err != nil {
			s.writeHandlerError(w, fmt.Errorf("reread a keyed request's body: %w", err))
			return
		}
		if body.size > keyedBodyMax {
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
		answer := &keyedAnswer{w: w}
		settled := false
		defer func() {
			// A handler that panicked answered nothing worth keeping: the next attempt runs again.
			// The panic itself carries on to net/http with its own stack.
			if !settled {
				s.releaseRequestKey(r.Context(), claim)
			}
		}()
		handler(answer, r.WithContext(context.WithValue(r.Context(), requestKeyClaimKey{}, claim)))
		s.settleRequestKey(r.Context(), claim, answer)
		settled = true
		answer.deliver()
	}
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

// readKeyedBody reads body into a keyedBody, at most one byte past keyedBodyMax, writing it to
// digest as it goes; for a multipart contentType, with its boundary masked (boundaryMask).
func readKeyedBody(body io.Reader, digest io.Writer, contentType string) (*keyedBody, error) {
	read := &keyedBody{}
	target := digest
	var mask *boundaryMask
	if mediaType, parameters, err := mime.ParseMediaType(contentType); err == nil &&
		strings.HasPrefix(mediaType, "multipart/") && parameters["boundary"] != "" {
		mask = &boundaryMask{w: digest, boundary: []byte(parameters["boundary"])}
		target = mask
	}
	source := io.TeeReader(io.LimitReader(body, keyedBodyMax+1), target)
	var head bytes.Buffer
	n, err := io.CopyN(&head, source, keyedBodyInMemory)
	read.head, read.size = head.Bytes(), n
	if err == nil {
		spill, err := os.CreateTemp("", "dispatch-keyed-body-*")
		if err != nil {
			return read, err
		}
		read.spill = spill
		rest, err := io.Copy(spill, source)
		read.size += rest
		if err != nil {
			return read, err
		}
	} else if !errors.Is(err, io.EOF) {
		return read, err
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
	lapsed      bool
	expired     bool
}

// claimRequestKey takes login's key for r, whose digest is digest, and answers whatever the key
// already holds instead: answered reports that it did, and that the handler must not run. Every
// statement is its own transaction, so no lock outlives it; a row another request moved between
// two of them sends this one round again.
func (s *server) claimRequestKey(
	w http.ResponseWriter, r *http.Request, login, key, digest string, replay keyedReplay,
) (claim requestKeyClaim, answered bool) {
	ctx, pool := r.Context(), s.deps.Store.Pool
	lease, ttl := contracts.IdempotencyLease.Seconds(), contracts.IdempotencyKeyTTL.Seconds()
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
			       response is not null, recovery,
			       leased_at is null or leased_at < now() - make_interval(secs => $3),
			       created_at < now() - make_interval(secs => $4)
			from request_keys where login = $1 and idempotency_key = $2
		`, login, key, lease, ttl).Scan(
			&held.digest, &held.method, &held.path, &held.attempt, &held.status, &held.contentType,
			&held.response, &held.kept, &held.recovery, &held.lapsed, &held.expired,
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
			replay(w, r, held.recovery)
			return claim, true
		case held.status != nil:
			replayStoredAnswer(w, held)
			return claim, true
		case !held.lapsed:
			writeKeyInFlight(w, key)
			return claim, true
		}
		// The holder's lease lapsed with nothing committed: its process died before its write did,
		// so this attempt takes the key over and runs the request.
		tag, err := pool.Exec(ctx, `
			update request_keys set attempt = attempt + 1, leased_at = now()
			where login = $1 and idempotency_key = $2 and attempt = $3 and status is null and recovery is null
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
// it, and any other releases the key (releaseRequestKey). A row another attempt has taken over is
// left as it is. A failure is logged and the caller is still answered, since the handler has run:
// its row stays in flight until a repeat past the lease takes it over.
func (s *server) settleRequestKey(ctx context.Context, claim requestKeyClaim, answer *keyedAnswer) {
	status := answer.finalStatus()
	if !storedStatus(status) {
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
// with no recovery point is deleted, so the next attempt runs the request again; one whose request
// committed its recovery point keeps it, and is released at once, so the next attempt is answered
// from that point rather than making the write a second time.
func (s *server) releaseRequestKey(ctx context.Context, claim requestKeyClaim) {
	bookkeeping, cancel := context.WithTimeout(context.WithoutCancel(ctx), keyedBookkeeping)
	defer cancel()
	if _, err := s.deps.Store.Pool.Exec(bookkeeping, `
		with released as (
			update request_keys set leased_at = null
			where login = $1 and idempotency_key = $2 and attempt = $3 and status is null and recovery is not null
		)
		delete from request_keys
		where login = $1 and idempotency_key = $2 and attempt = $3 and status is null and recovery is null
	`, claim.login, claim.key, claim.attempt); err != nil {
		slog.Error("dispatch: release a keyed request's key", "error", err)
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
