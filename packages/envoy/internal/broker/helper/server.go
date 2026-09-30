// packages/envoy/internal/broker/helper/server.go
//go:build linux

package helper

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// Server answers the helper socket. PeerOf is a field so tests can present a chosen pid.
type Server struct {
	Registry *Registry
	Broker   *Broker
	Hostname string
	PeerOf   func(*net.UnixConn) (*Peer, error)
	Log      *slog.Logger
	// MinRenew floors the renew cadence; the interval is a third of the lease the broker gave.
	MinRenew time.Duration
}

// Serve accepts until ctx ends. Each connection carries one request.
func (s *Server) Serve(ctx context.Context, ln *net.UnixListener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handle(ctx, conn)
	}
}

func (s *Server) handle(ctx context.Context, conn *net.UnixConn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	peer, err := s.PeerOf(conn)
	if err != nil {
		s.reply(conn, Response{Code: CodeUnidentified, Error: "the kernel did not identify the peer: " + err.Error()})
		return
	}
	line, err := bufio.NewReaderSize(conn, maxLine).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		peer.Close()
		return
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		peer.Close()
		s.reply(conn, Response{Code: CodeBadRequest, Error: "one JSON object per line"})
		return
	}
	pid := peer.PID()
	if pid == 0 {
		peer.Close()
		s.reply(conn, Response{Code: CodeUnidentified, Error: "the peer exited before it was identified"})
		return
	}
	var resp Response
	switch req.Op {
	case "register":
		// A new session keeps its pidfd; every other path closes it before returning.
		wait := time.Duration(req.WaitSeconds) * time.Second
		if wait > 0 {
			_ = conn.SetDeadline(time.Now().Add(wait + 5*time.Second))
		}
		resp = s.register(ctx, peer, pid, wait)
	case "sign":
		// sign and unregister keep peer's pidfd open through Registry.Root's ancestry walk too,
		// for the same pid-reuse reason register does (see register's doc comment below); each
		// closes peer itself once it has re-verified peer.PID() against pid.
		resp = s.sign(peer, pid, req.Method, req.URL)
	case "sign-request":
		// sign-request keeps peer's pidfd open through Registry.Root's ancestry walk exactly
		// like sign, for the same pid-reuse reason (see sign's doc comment below).
		resp = s.signRequest(peer, pid, req.Secrets, req.Reason)
	case "unregister":
		resp = s.unregister(ctx, peer, pid)
	case "sessions":
		peer.Close()
		resp = s.sessions()
	case "login":
		// login, login-status, enroll-box and unenroll-box are pass-through broker calls, not
		// registry-session ops: a box enrollment is not a registry session (see EnrollBox's doc
		// comment), so none of these needs peer's pid at all.
		peer.Close()
		resp = s.login(ctx)
	case "login-status":
		peer.Close()
		resp = s.loginStatus()
	case "enroll-box":
		peer.Close()
		resp = s.enrollBox(ctx, req.RuntimeID, req.Thumbprint, req.SessionID)
	case "unenroll-box":
		peer.Close()
		resp = s.unenrollBox(ctx, req.EnrollmentID)
	default:
		peer.Close()
		resp = Response{Code: CodeBadRequest, Error: "unknown op " + req.Op}
	}
	s.reply(conn, resp)
}

func (s *Server) reply(conn net.Conn, resp Response) {
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	_, _ = conn.Write(append(data, '\n'))
}

// register makes pid a session root — unless pid is already inside a session, whose reply it
// then gets — generating the key and starting enrollment in the background. Registry.Root and
// addRootIfAbsent both walk /proc ancestry from pid while peer's pidfd stays open; the kernel
// can still reuse the pid number meanwhile (Peer's doc comment), so each walk's result is
// trusted only once peer.PID() is re-read here and still names pid — otherwise this answers
// CodeUnidentified instead of adopting or handing back a session for a peer that no longer is
// who it was.
func (s *Server) register(ctx context.Context, peer *Peer, pid int, wait time.Duration) Response {
	if existing := s.Registry.Root(pid); existing != nil {
		if peer.PID() != pid {
			peer.Close()
			return Response{Code: CodeUnidentified, Error: "the peer no longer matches the identified pid"}
		}
		peer.Close()
		return s.registerReply(existing, wait)
	}
	ticks, err := StartTicks(pid)
	if err != nil {
		peer.Close()
		return Response{Code: CodeBadRequest, Error: err.Error()}
	}
	sess, err := newSession(pid, ticks, fmt.Sprintf("%s:%d:%d", s.Hostname, pid, ticks), peer)
	if err != nil {
		peer.Close()
		return Response{Code: "KEYGEN", Error: err.Error()}
	}
	existing, won := s.Registry.addRootIfAbsent(pid, sess)
	if peer.PID() != pid {
		if won {
			s.Registry.Remove(sess)
		}
		peer.Close()
		return Response{Code: CodeUnidentified, Error: "the peer no longer matches the identified pid"}
	}
	if !won {
		peer.Close()
		return s.registerReply(existing, wait)
	}
	s.save()
	s.adopt(ctx, sess, "")
	s.Log.Info("session registered", "pid", pid, "runtime_id", sess.RuntimeID)
	return s.registerReply(sess, wait)
}

// registerReply answers a register, first waiting up to wait for the session to enroll. Without a
// launcher credential the enroll loop cannot succeed until a human logs the helper in, so it does
// not wait at all: a launcher's `register --wait N` then costs nothing on a helper that was never
// logged in, or whose credential the broker has refused once. An expired credential stays held
// until a call is refused, so the first register after it expires still waits the full N. A
// session the helper cannot enroll for want of a credential, read after any wait, gets Code
// NO_CREDENTIAL and that reason in Error, so a launcher can say what the session will do.
func (s *Server) registerReply(sess *Session, wait time.Duration) Response {
	if wait > 0 && s.Broker.HasCredential() {
		select {
		case <-sess.ready:
		case <-time.After(wait):
		case <-sess.stop:
		}
	}
	resp := Response{OK: true, EnrollmentID: sess.EnrollmentID(), RuntimeID: sess.RuntimeID, Operator: s.Broker.Operator(), State: sess.State(), Error: sess.LastError()}
	if resp.EnrollmentID == "" && !s.Broker.HasCredential() {
		resp.Code, resp.Error = CodeNoCredential, noCredentialMsg
	}
	return resp
}

// notEnrolled answers sign or sign-request for a registered session with no enrollment yet: it is
// enrolling (NOT_ENROLLED) while the helper holds a launcher credential, and without one the
// helper enrolls no one until a human logs it in, so the session has no broker identity
// (NO_CREDENTIAL).
func (s *Server) notEnrolled(sess *Session) Response {
	if !s.Broker.HasCredential() {
		return Response{Code: CodeNoCredential, Error: noCredentialMsg}
	}
	return Response{Code: CodeNotEnrolled, Error: "this session is not enrolled with the broker yet; last attempt: " + sess.LastError()}
}

// resolveDescendant keeps peer's pidfd open through Registry.Root's ancestry walk, exactly like
// register's "existing session" path, and re-verifies peer.PID() == pid before trusting a
// non-nil match — otherwise a pid the kernel reused mid-walk could tie a signed proof, a signed
// request object, or a forced revoke to the wrong session (impersonation for sign/signRequest,
// denial of service for unregister). peer is closed on every return path, so every caller
// (sign, signRequest, unregister) must return resp unchanged the moment ok is false, never
// touching peer again. notASession is the CodeNotASession error text: sign and signRequest name
// the registration command, unregister does not (its own long-standing wording).
func (s *Server) resolveDescendant(peer *Peer, pid int, notASession string) (*Session, Response, bool) {
	sess := s.Registry.Root(pid)
	if sess == nil {
		peer.Close()
		return nil, Response{Code: CodeNotASession, Error: notASession}, false
	}
	if peer.PID() != pid {
		peer.Close()
		return nil, Response{Code: CodeUnidentified, Error: "the peer no longer matches the identified pid"}, false
	}
	peer.Close()
	return sess, Response{}, true
}

// sign signs a per-call proof for a pid resolveDescendant ties to a registered session.
func (s *Server) sign(peer *Peer, pid int, method, url string) Response {
	if method == "" || url == "" {
		peer.Close()
		return Response{Code: CodeBadRequest, Error: "sign needs method and url"}
	}
	sess, resp, ok := s.resolveDescendant(peer, pid, fmt.Sprintf("pid %d is not inside a registered host session; a session root is started with `agent-secrets register --exec -- <agent argv>` (shims/omp does this)", pid))
	if !ok {
		return resp
	}
	id := sess.EnrollmentID()
	if id == "" {
		return s.notEnrolled(sess)
	}
	compact, err := proof.Sign(sess.Key, id, method, url, time.Now())
	if err != nil {
		return Response{Code: "SIGN", Error: err.Error()}
	}
	return Response{OK: true, Proof: compact, EnrollmentID: id, RuntimeID: sess.RuntimeID}
}

// signRequest builds and signs a credential-request object naming one agent_secret
// authorization_detail per requested name (contract v9: POST /v1/requests embeds this signed
// object rather than carrying "secrets"/"reason" as plain fields), for a pid resolveDescendant
// ties to a registered session. The audience is always s.Broker.URL, never a value the peer
// supplies: a request object's aud claim must be the broker's own public URL for the broker to
// accept it, and there is no reason to trust an untrusted local peer's opinion of that value
// over the helper's own configuration.
func (s *Server) signRequest(peer *Peer, pid int, names []string, reason string) Response {
	if len(names) == 0 {
		peer.Close()
		return Response{Code: CodeBadRequest, Error: "sign-request needs at least one secret name"}
	}
	sess, resp, ok := s.resolveDescendant(peer, pid, fmt.Sprintf("pid %d is not inside a registered host session; a session root is started with `agent-secrets register --exec -- <agent argv>` (shims/omp does this)", pid))
	if !ok {
		return resp
	}
	if sess.EnrollmentID() == "" {
		return s.notEnrolled(sess)
	}
	details := make([]record.AuthorizationDetail, len(names))
	for i, name := range names {
		details[i] = record.AuthorizationDetail{Type: "agent_secret", Identifier: name, Actions: []string{"inject"}}
	}
	compact, err := record.Sign(sess.Key, s.Broker.URL, details, reason, "", time.Now())
	if err != nil {
		return Response{Code: "SIGN", Error: err.Error()}
	}
	return Response{OK: true, RequestObject: compact}
}

// unregister retires a pid resolveDescendant ties to a registered session.
func (s *Server) unregister(ctx context.Context, peer *Peer, pid int) Response {
	sess, resp, ok := s.resolveDescendant(peer, pid, fmt.Sprintf("pid %d is not inside a registered host session", pid))
	if !ok {
		return resp
	}
	s.retire(ctx, sess, "unregistered")
	return Response{OK: true, RuntimeID: sess.RuntimeID}
}

func (s *Server) sessions() Response {
	list := s.Registry.List()
	infos := make([]SessionInfo, 0, len(list))
	for _, sess := range list {
		infos = append(infos, sess.Info())
	}
	return Response{OK: true, Sessions: infos}
}

// login starts (or reports the running) machine login and returns its human-facing
// confirmation code. A pass-through broker call: it needs no registered session, so the
// dispatcher closes peer before calling it.
func (s *Server) login(ctx context.Context) Response {
	code, err := s.Broker.Login(ctx, s.Hostname)
	if err != nil {
		return Response{Code: CodeLoginFailed, Error: err.Error()}
	}
	return Response{OK: true, Code: code, LoginState: s.Broker.LoginStatus().State}
}

// loginStatus reports the current (or most recently settled) machine login; an empty
// LoginState means none has ever run.
func (s *Server) loginStatus() Response {
	ls := s.Broker.LoginStatus()
	return Response{OK: true, Code: ls.Code, LoginState: ls.State}
}

// enrollBox registers a box's key as kind box — a pass-through broker call requiring no
// registered session (a box enrollment is not a registry session; see Broker.EnrollBox's doc
// comment), so unlike sign it does not check descendancy at all.
func (s *Server) enrollBox(ctx context.Context, runtimeID, thumbprint string, sessionID *string) Response {
	if runtimeID == "" || thumbprint == "" {
		return Response{Code: CodeBadRequest, Error: "enroll-box needs runtime_id and thumbprint"}
	}
	id, lease, err := s.Broker.EnrollBox(ctx, runtimeID, thumbprint, sessionID)
	if err != nil {
		return Response{Code: CodeEnrollFailed, Error: err.Error()}
	}
	return Response{OK: true, EnrollmentID: id, LeaseExpires: lease.UTC().Format(time.RFC3339Nano)}
}

// unenrollBox deletes a box enrollment; idempotent like Broker.Revoke (204/404 both succeed).
func (s *Server) unenrollBox(ctx context.Context, enrollmentID string) Response {
	if enrollmentID == "" {
		return Response{Code: CodeBadRequest, Error: "unenroll-box needs enrollment_id"}
	}
	if err := s.Broker.UnenrollBox(ctx, enrollmentID); err != nil {
		return Response{Code: CodeUnenrollFailed, Error: err.Error()}
	}
	return Response{OK: true}
}

// adopt starts the two goroutines every session has: enrollment with renewal, and the exit
// watch. priorID, when non-empty, names a still-live enrollment that must be revoked before
// enrollLoop's first Broker.Enroll call — Recover's re-pinned-session path passes its recorded
// old enrollment id here so the revoke and the fresh enroll for the same runtime_id can never
// race (see enrollLoop's doc comment); every other caller of adopt passes "". That revoke-first
// ordering couples priorID's fate to this session's lifecycle until the first successful
// Enroll, so enrollLoop's own doc comment covers the fallback for the session ending first.
func (s *Server) adopt(ctx context.Context, sess *Session, priorID string) {
	go s.enrollLoop(ctx, sess, priorID)
	go s.watchExit(ctx, sess)
}

// retryUntilStop retries try with backoff until it succeeds, stop closes, or ctx ends. Delay
// starts at start and doubles toward maxDelay after each failure (start == maxDelay gives a
// caller a fixed delay instead of a growing one); attempts caps the number of tries (0 means
// unlimited), and try's last permitted attempt failing ends the loop at once, without waiting.
// wake, when non-nil, gives a channel taken before each attempt: its closing cuts the wait short
// and starts the backoff over, so a retry that failed for want of something retries the moment
// it arrives (the broker's CredentialInstalled). onFail runs after every failed attempt with its
// 1-based number, the error, the delay before the next attempt (meaningless once retrying is
// false), and whether the loop is about to retry, so a caller expresses only what it retries —
// its logging and any per-failure side effect — not how retrying works. Returns whether try
// eventually succeeded.
func retryUntilStop(ctx context.Context, stop <-chan struct{}, wake func() <-chan struct{}, attempts int, start, maxDelay time.Duration, onFail func(attempt int, err error, delay time.Duration, retrying bool), try func() error) bool {
	delay := start
	for attempt := 1; attempts <= 0 || attempt <= attempts; attempt++ {
		var woken <-chan struct{}
		if wake != nil {
			woken = wake()
		}
		err := try()
		if err == nil {
			return true
		}
		retrying := attempts <= 0 || attempt < attempts
		onFail(attempt, err, delay, retrying)
		if !retrying {
			return false
		}
		select {
		case <-time.After(delay):
			delay = min(delay*2, maxDelay)
		case <-woken:
			delay = start
		case <-stop:
			return false
		case <-ctx.Done():
			return false
		}
	}
	return false
}

// enrollLoop enrolls with backoff for as long as the session lives, then renews; a refused
// renew comes back here and enrolls again with the same key. priorID, when non-empty, names an
// enrollment that must be revoked before this loop's first Broker.Enroll call — Recover's
// re-pinned-session path passes its recorded old enrollment id, so a fresh enroll for the same
// runtime_id never races the old row's revoke. That race matters against the real broker,
// whatever it looks like against the fake one here: the real broker's idempotent-enroll conflict
// is keyed on (launcher_credential_id, runtime_id), not the session's signing-key thumbprint, so
// a still-live prior row for this runtime_id refuses a fresh enroll outright (409
// ALREADY_ENROLLED, a hard error) even though the new session presents a different key —
// exactly the case a concurrent "adopt now, revoke independently" ordering could hit. Revoking
// first, with the same revokeLapsed retry-until-gone loop a refused renew already uses below,
// makes the old row's absence (or the session ending first) a precondition of the first enroll
// attempt rather than a race with it. A later refused renew still revokes its own lapsed id the
// same way once it returns here — the broker's idempotent-enroll conflict path can otherwise
// keep answering the same dead id forever (a base-branch bug tracked separately) — so by the
// time control returns here that way the old id is actually gone and this enroll call mints a
// fresh one.
//
// If the session ends (sess.stop closes) before that revoke ever succeeds — the recovered
// process exits or is unregistered while the broker is unreachable and revokeLapsed is
// mid-backoff — retire's own revoke cannot help: it only ever revokes sess.EnrollmentID(),
// still empty here since this session never reached its first successful Enroll, so priorID
// would otherwise be abandoned for good the moment Registry.Remove drops this session's
// record. revokeLapsed returns false both when the session ends and when ctx is done, and
// ctx.Err() tells them apart: only the former still has anywhere useful to send priorID (ctx
// done means the whole daemon is exiting, and nothing further should be attempted), so this
// loop then falls back to firing the same bounded, independent revoke retire uses for its own
// enrollment id — decoupled from this session exactly like the pre-fix "adopt now, revoke
// independently" ordering — so priorID still gets its guaranteed best-effort attempts even
// though the new session is gone.
func (s *Server) enrollLoop(ctx context.Context, sess *Session, priorID string) {
	if priorID != "" && !s.revokeLapsed(ctx, sess, priorID) {
		if ctx.Err() == nil {
			go s.revoke(ctx, priorID)
		}
		return
	}
	for {
		var id string
		var lease time.Time
		// A login wakes the retry, so a session registered before it enrolls within about a second
		// rather than when a backoff of up to a minute comes round.
		enrolled := retryUntilStop(ctx, sess.stop, s.Broker.CredentialInstalled, 0, time.Second, time.Minute, func(attempt int, err error, delay time.Duration, retrying bool) {
			sess.setError(err.Error())
			s.Log.Warn("enroll failed; retrying", "runtime_id", sess.RuntimeID, "error", err, "in", delay)
		}, func() error {
			var err error
			id, lease, err = s.Broker.Enroll(ctx, sess)
			return err
		})
		if !enrolled {
			return
		}
		sess.setEnrolled(id)
		s.save()
		s.Log.Info("session enrolled", "runtime_id", sess.RuntimeID, "enrollment_id", id)
		if !s.renewLoop(ctx, sess, lease) {
			return
		}
	}
}

// renewLoop renews at a third of the lease until the session ends (false) or the broker
// refuses the proof (true: the caller enrolls again). A refused renew means the lease has
// lapsed; the broker's idempotent-enroll conflict path can otherwise keep answering the same
// dead enrollment id forever (its root cause is in the base branch, handled separately), so the
// lapsed id is explicitly revoked — retried with backoff until it succeeds or ctx ends, never
// giving up after a fixed number of tries the way retire's revoke does, since returning early
// here would leave the session stuck re-enrolling onto a broker that keeps handing back the
// same dead id — before the enrollment is cleared and enrollLoop is told to enroll fresh.
func (s *Server) renewLoop(ctx context.Context, sess *Session, lease time.Time) bool {
	for {
		interval := max(s.MinRenew, time.Until(lease)/3)
		select {
		case <-time.After(interval):
		case <-sess.stop:
			return false
		case <-ctx.Done():
			return false
		}
		next, err := s.Broker.Renew(ctx, sess)
		var be *BrokerError
		if errors.As(err, &be) && be.Status == http.StatusUnauthorized {
			id := sess.EnrollmentID()
			s.Log.Warn("renew refused; revoking the lapsed enrollment before enrolling again", "runtime_id", sess.RuntimeID, "code", be.Code, "enrollment_id", id)
			if !s.revokeLapsed(ctx, sess, id) {
				return false
			}
			sess.clearEnrollment()
			return true
		}
		if err != nil {
			s.Log.Warn("renew failed; keeping the current lease", "runtime_id", sess.RuntimeID, "error", err)
			continue
		}
		lease = next
	}
}

// revokeLapsed retries Broker.Revoke for a renew-refused enrollment until it succeeds, the
// session ends, or ctx is done. Backoff matches enrollLoop's: 1 s doubling to a 1-minute cap,
// retried indefinitely rather than a fixed number of times — giving up would hand the dead id
// straight back to the broker's idempotent-enroll conflict path, which can keep answering it
// forever — and, like enrollLoop's, woken by a login, since a revoke needs the credential too.
// Returns false only when the session ended or ctx was canceled first.
func (s *Server) revokeLapsed(ctx context.Context, sess *Session, id string) bool {
	return retryUntilStop(ctx, sess.stop, s.Broker.CredentialInstalled, 0, time.Second, time.Minute, func(attempt int, err error, delay time.Duration, retrying bool) {
		s.Log.Warn("revoking the lapsed enrollment failed; retrying", "runtime_id", sess.RuntimeID, "enrollment_id", id, "error", err, "in", delay)
	}, func() error {
		return s.Broker.Revoke(ctx, id)
	})
}

func (s *Server) watchExit(ctx context.Context, sess *Session) {
	if sess.peer == nil || !sess.peer.WaitExit(sess.stop) {
		return
	}
	s.retire(ctx, sess, "process exited")
}

// retire forgets the session, then revokes its enrollment (three tries; the lease is the
// fallback). Idempotent: an unregister followed by the exit watch firing retires once.
func (s *Server) retire(ctx context.Context, sess *Session, why string) {
	if !s.Registry.Remove(sess) {
		return
	}
	if sess.peer != nil {
		sess.peer.Close()
	}
	s.save()
	if id := sess.EnrollmentID(); id != "" {
		s.revoke(ctx, id)
	}
	s.Log.Info("session retired", "runtime_id", sess.RuntimeID, "why", why)
}

func (s *Server) revoke(ctx context.Context, id string) {
	retryUntilStop(ctx, nil, nil, 3, 2*time.Second, 2*time.Second, func(attempt int, err error, delay time.Duration, retrying bool) {
		if !retrying {
			s.Log.Warn("revoke failed; the lease lapses on its own", "enrollment_id", id, "error", err)
		}
	}, func() error {
		return s.Broker.Revoke(ctx, id)
	})
}

func (s *Server) save() {
	if err := s.Registry.Save(); err != nil {
		s.Log.Error("saving session records", "error", err)
	}
}

// Recover runs at start: every recorded session whose process is still the same incarnation is
// pinned again with a fresh key and adopted (enrolled and exit-watched); a recorded process that
// is gone just has its enrollment revoked. A re-pinned session's own goroutine revokes its prior
// enrollment before enrolling the fresh key — adopt's priorID, threaded into enrollLoop —
// mirroring renewLoop's own revoke-before-re-enroll on a refused renew: the real broker's
// idempotent-enroll conflict is keyed on (launcher_credential_id, runtime_id), so a fresh enroll
// racing ahead of the old row's revoke can be refused outright (409 ALREADY_ENROLLED) even though
// it carries a fresh key, which adopting first and revoking independently in a separate goroutine
// used to allow. That revoke runs inside the one goroutine adopt starts for the re-pinned
// session, never on this loop's own goroutine, so one session waiting on its own revoke never
// blocks Recover from moving on to the next recorded session. Should that re-pinned session end
// (its process exits, or it is unregistered) while its prior enrollment's revoke is still
// retrying against a failing broker, enrollLoop falls back to an independent bounded revoke of
// the prior id instead of abandoning it — see enrollLoop's doc comment for why retire's own
// revoke cannot reach it there.
func (s *Server) Recover(ctx context.Context) {
	recs, err := LoadRecords(s.Registry.path)
	if err != nil {
		s.Log.Error("reading recorded sessions", "error", err)
		return
	}
	for _, rec := range recs {
		peer, err := PinPID(rec.PID)
		alive := err == nil && peer.PID() == rec.PID
		if alive {
			ticks, err := StartTicks(rec.PID)
			alive = err == nil && ticks == rec.StartTicks
		}
		if !alive {
			if peer != nil {
				peer.Close()
			}
			s.Log.Info("recorded session is gone", "runtime_id", rec.RuntimeID)
			if rec.EnrollmentID != "" {
				go s.revoke(ctx, rec.EnrollmentID)
			}
			continue
		}
		sess, err := newSession(rec.PID, rec.StartTicks, rec.RuntimeID, peer)
		if err != nil {
			peer.Close()
			s.Log.Error("key generation", "runtime_id", rec.RuntimeID, "error", err)
			continue
		}
		s.Registry.Add(sess)
		s.Log.Warn("session re-pinned after a restart with a fresh key; its previous grants are revoked", "runtime_id", rec.RuntimeID)
		s.adopt(ctx, sess, rec.EnrollmentID)
	}
	s.save()
}
