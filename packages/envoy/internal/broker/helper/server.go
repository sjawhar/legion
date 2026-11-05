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
	neturl "net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/broker/leasetime"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/redact"
)

// Server answers the helper socket. PeerOf is a field so tests can present a chosen pid.
type Server struct {
	Registry *Registry
	Broker   *Broker
	Hostname string
	PeerOf   func(*net.UnixConn) (*Peer, error)
	Log      *slog.Logger
	// MinRenew floors the renew cadence; the interval is a third of the lease the broker gave,
	// never below it (leasetime.HelperRenewAfter). Every caller sets it:
	// cmd/agent-secrets-helper sets leasetime.HelperMinRenew, the wait the broker's lease bound
	// counts this renewer by, and a test sets a shorter one to drive the loop in real time.
	MinRenew time.Duration
	// dormantGraceNS is DefaultDormantGrace unless SetDormantGrace names another: how long a
	// named session's identity is kept for it to come back to before SweepDormant revokes it.
	// Atomic, because a test sets it while the Server's own goroutines (retire, Recover's log
	// line) read it — a plain field there is a data race, and `go test -race` caught it.
	dormantGraceNS atomic.Int64
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
		sessionID := ""
		if req.SessionID != nil {
			sessionID = *req.SessionID
		}
		resp = s.register(ctx, peer, pid, sessionID, wait)
	case "sign":
		// sign and unregister keep peer's pidfd open through Registry.Root's ancestry walk too,
		// for the same pid-reuse reason register does (see register's doc comment below); each
		// closes peer itself once it has re-verified peer.PID() against pid.
		resp = s.sign(peer, pid, req.Method, req.URL)
	case "sign-request":
		// sign-request keeps peer's pidfd open through Registry.Root's ancestry walk exactly
		// like sign, for the same pid-reuse reason (see sign's doc comment below).
		resp = s.signRequest(peer, pid, req.Secrets, req.Reason, req.LifetimeSeconds)
	case "sign-launcher":
		// sign-launcher keeps peer's pidfd open through Registry.Root's ancestry walk too, and
		// trusts its "no session" only once peer.PID() still names pid (see signLauncher).
		resp = s.signLauncher(peer, pid, req.Method, req.URL)
	case "unregister":
		resp = s.unregister(ctx, peer, pid)
	case "sessions":
		peer.Close()
		resp = s.sessions()
	case "drain":
		// drain is `agent-secrets-helper install`'s restart guard: the sessions, and with none,
		// a helper that refuses every later register until it exits.
		peer.Close()
		resp = s.drain()
	case "undrain":
		// undrain is install's undo of a drain whose restart then failed: the helper takes
		// registrations again.
		peer.Close()
		s.Registry.Undrain()
		s.Log.Info("undrained: taking new sessions again")
		resp = Response{OK: true}
	case "login":
		// login, login-status, enroll-session-key and unenroll-session-key are pass-through broker
		// calls, not registry-session ops: a key the helper does not hold is not a registry session
		// (see Broker.EnrollSessionKey), so none of these needs peer's pid at all.
		peer.Close()
		resp = s.login(ctx)
	case "login-status":
		peer.Close()
		resp = s.loginStatus()
	case "enroll-session-key":
		peer.Close()
		if s.Registry.Draining() {
			resp = drainingResponse
			break
		}
		resp = s.enrollSessionKey(ctx, req.RuntimeID, req.Thumbprint, req.SessionID)
	case "unenroll-session-key":
		peer.Close()
		resp = s.unenrollSessionKey(ctx, req.EnrollmentID)
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
// Registry.AdmitRoot both walk /proc ancestry from pid while peer's pidfd stays open; the kernel
// can still reuse the pid number meanwhile (Peer's doc comment), so each walk's result is
// trusted only once peer.PID() is re-read here and still names pid — otherwise this answers
// CodeUnidentified instead of adopting or handing back a session for a peer that no longer is
// who it was.
//
// sessionID is the omp session id the launcher names, or "" for a session with none. With one,
// the runtime id is `session-<sessionID>` rather than `<host>:<pid>:<start ticks>`, and the
// identity a dormant record holds for that session — its key AND its enrollment — is taken back
// rather than made afresh, so the broker answers the enrollment it already has (its conflict is
// keyed on (launcher credential, runtime id), and the same key under the same runtime id gets
// the same row). That is what carries a session's pending requests and its grants across a
// reboot. A session already registered without an id takes one here (adoptSessionID), which is
// how a launcher that learns the id only after the agent starts gives it.
//
// The record is consumed where the session is inserted and nowhere else: AdmitRoot takes it,
// re-reads the peer's pid and inserts the session, all under one lock, so a registration that
// does not happen — the helper draining, another root winning the pid, a key that will not
// generate, a peer the kernel replaced mid-walk — leaves the record exactly where it was and
// never publishes a session another request or a Save beside it could see. An identity taken
// by a registration that then fails is an identity nothing can ever reclaim, and a session
// published before it is certain is one a restart could re-pin for a stranger.
func (s *Server) register(ctx context.Context, peer *Peer, pid int, sessionID string, wait time.Duration) Response {
	if existing := s.Registry.Root(pid); existing != nil {
		if peer.PID() != pid {
			peer.Close()
			return Response{Code: CodeUnidentified, Error: "the peer no longer matches the identified pid"}
		}
		peer.Close()
		if resp, refused := s.adoptSessionID(ctx, existing, sessionID); refused {
			return resp
		}
		return s.registerReply(existing, wait)
	}
	ticks, err := StartTicks(pid)
	if err != nil {
		peer.Close()
		return Response{Code: CodeBadRequest, Error: err.Error()}
	}
	// orphans are dead enrollments the reclaimed record named that the new session's one
	// lapsed slot could not hold; the bounded revoke ends them once the admission stands.
	var orphans []string
	adm := s.Registry.AdmitRoot(pid, sessionID, func(rec Record, kept bool) (*Session, error) {
		got := s.reclaimIdentity(sessionID, rec, kept)
		if got.RuntimeID == "" {
			got.RuntimeID = fmt.Sprintf("%s:%d:%d", s.Hostname, pid, ticks)
		}
		sess, err := newSession(pid, ticks, got.RuntimeID, sessionID, got.Key, peer)
		if err != nil {
			return nil, err
		}
		if got.EnrollmentID != "" {
			// The broker holds this enrollment for this key under this runtime id, so the
			// session carries it from the moment it registers — a helper restart before the
			// first enroll still finds the id on its record rather than an empty one — but
			// it does NOT count as enrolled until an enroll confirms the broker still
			// honours it (Session.inherit).
			sess.inherit(got.EnrollmentID)
		}
		orphans = takeLapsed(sess, got.Revoke, got.Blocking)
		return sess, nil
	}, func() bool { return peer.PID() == pid })
	switch {
	case adm.Draining:
		peer.Close()
		return drainingResponse
	case adm.Err != nil:
		peer.Close()
		return Response{Code: CodeKeygen, Error: adm.Err.Error()}
	case adm.Held:
		peer.Close()
		return Response{Code: CodeBadRequest, Error: fmt.Sprintf("session %s is already registered as pid %d; a second process may not take its broker identity", sessionID, adm.Session.PID)}
	}
	if adm.Unidentified {
		peer.Close()
		return Response{Code: CodeUnidentified, Error: "the peer no longer matches the identified pid"}
	}
	sess := adm.Session
	if !adm.Won {
		peer.Close()
		// The pid turned out to be inside a session another registration won between this
		// one's own Root walk and its admission. It gets that session's reply, and its
		// --session-id must still be applied to it, exactly as on the Root path above:
		// dropping the name here would answer OK to a register whose name nothing took, and
		// leave the identity kept for it to the sweep.
		if resp, refused := s.adoptSessionID(ctx, sess, sessionID); refused {
			return resp
		}
		return s.registerReply(sess, wait)
	}
	s.save()
	s.adopt(ctx, sess)
	for _, id := range orphans {
		go s.revoke(ctx, id)
	}
	if adm.Took {
		s.Log.Info("session registered, taking back the identity it had before", "pid", pid, "runtime_id", sess.RuntimeID(), "session_id", sessionID, "enrollment_id", adm.Record.EnrollmentID)
	} else {
		s.Log.Info("session registered", "pid", pid, "runtime_id", sess.RuntimeID())
	}
	return s.registerReply(sess, wait)
}

// dormantRecord is the park function Registry.Retire takes: the record a named session leaves
// behind, built from the identity reading Retire makes under its own lock, so nothing can land
// between the reading and the record. A session with no omp session id leaves none — nothing
// could ever claim it — and its enrollment is revoked by the caller instead.
//
// A named session is parked whether or not it has an enrollment yet. One that went away while
// it was still enrolling — the helper holding no launcher credential, a resume interrupted —
// would otherwise have its KEY dropped, and the identity the broker still holds could never be
// reclaimed by the session coming back.
func (s *Server) dormantRecord(sess *Session) func(identity) (Record, bool) {
	return func(cur identity) (Record, bool) {
		if cur.SessionID == "" {
			return Record{}, false
		}
		gone := time.Now()
		return Record{
			PID: sess.PID, StartTicks: sess.StartTicks, BootID: s.Registry.bootID, RuntimeID: cur.RuntimeID,
			EnrollmentID: cur.EnrollmentID, LapsedEnrollmentID: cur.LapsedID, SessionID: cur.SessionID,
			Key: encodeKey(cur.Key), GoneAt: &gone,
		}, true
	}
}

// reclaimIdentity is the identity a session named sessionID takes: the dormant record's runtime
// id, key and enrollment when there is a record whose key reads, and the session-scoped runtime
// id with a fresh key otherwise. A session with no id at all gets an empty runtime id, and its
// caller builds the pid-keyed one.
//
// A record whose key will not parse (a records file edited or truncated) costs the session its
// continuity, never its ability to run: it gets a new identity, which is what it got before
// records carried keys at all. The enrollments that record named are then nobody's to keep —
// their key is unreadable — and the live one sits under the very runtime id the fresh key is
// about to enrol, which the broker refuses outright (409 ALREADY_ENROLLED, its conflict being
// keyed on (launcher credential, runtime id)). Both go to Revoke, that one first, so the
// session revokes it before enrolling rather than retrying until its lease lapses.
//
// It takes the record rather than reading it, so the registry decides under ONE lock whether the
// record is consumed and whether the session that consumes it is admitted (Registry.AdmitRoot,
// Registry.ClaimName). It must touch no registry state of its own.
func (s *Server) reclaimIdentity(sessionID string, rec Record, kept bool) reclaimed {
	if sessionID == "" {
		return reclaimed{}
	}
	if !kept {
		return reclaimed{RuntimeID: SessionRuntimeID(sessionID)}
	}
	key, err := rec.PrivateKey()
	if err != nil || key == nil {
		if err != nil {
			s.Log.Warn("the recorded key of a session coming back could not be read; it gets a new identity and its pending requests end with the old one", "session_id", sessionID, "error", err)
		}
		// The live one sits under the runtime id the fresh key is about to enrol, so it
		// blocks it: it earns the lapsed slot's retry-until-gone loop over anything the
		// session already had there (takeLapsed).
		return reclaimed{RuntimeID: SessionRuntimeID(sessionID), Revoke: rec.enrollments(), Blocking: rec.EnrollmentID != ""}
	}
	return reclaimed{RuntimeID: rec.RuntimeID, Key: key, EnrollmentID: rec.EnrollmentID,
		Revoke: nonEmpty(rec.LapsedEnrollmentID)}
}

// adoptSessionID gives a session that registered without an omp session id the one its launcher
// has since learnt: omp opens its transcript seconds after it starts, so a launcher cannot name
// the id before the exec of a fresh session, only of a resumed one. The session's runtime id
// becomes session-scoped and its current enrollment becomes its lapsed one, so enrollLoop
// revokes that and enrolls the new runtime id — the same path a refused renew takes. It happens
// seconds into a session, before any grant exists, which is why discarding that first enrollment
// costs nothing. A session that already has a DIFFERENT id is refused: an identity a session is
// signing with is not another caller's to change.
//
// Whether the name is free, whether the record kept for it is consumed, and the rename itself
// are one registry-level step (Registry.ClaimName): two sessions adopting one id must not both
// be told it was theirs.
//
// Every path through it requires that the session is still the registry's own — the ones with
// no name to claim included, which is what Registry.Holds is for. A register answering OK for a
// session that has gone tells its launcher it has a broker identity that nothing live holds.
func (s *Server) adoptSessionID(ctx context.Context, sess *Session, sessionID string) (Response, bool) {
	cur := sess.identity()
	if sessionID == "" || cur.SessionID == sessionID {
		// Nothing to claim, so ClaimName — which is where a retired session is caught — never
		// runs. The walk that found this session is a reading of a moment, and an unregister
		// or its exit watch can retire it before the reply goes out; answering OK then would
		// tell a launcher its session has a broker identity when nothing live holds the name
		// and the identity is parked for the sweep.
		if !s.Registry.Holds(sess) {
			return Response{Code: CodeNotASession, Error: "the session this process was in ended before this register could answer for it; run: agent-secrets register"}, true
		}
		return Response{}, false
	}
	if cur.SessionID != "" {
		return Response{Code: CodeBadRequest, Error: fmt.Sprintf("this process is in session %s, which cannot become session %s", cur.SessionID, sessionID)}, true
	}
	got := s.Registry.ClaimName(sess, sessionID, func(rec Record, kept bool) reclaimed {
		return s.reclaimIdentity(sessionID, rec, kept)
	})
	if got.Held != nil {
		return Response{Code: CodeBadRequest, Error: fmt.Sprintf("session %s is already registered as pid %d; a second process may not take its broker identity", sessionID, got.Held.PID)}, true
	}
	if got.Gone {
		// The session was retired — unregistered, or its process exited — while this call was
		// naming it. Nothing live holds the name, so this is not a registration that
		// succeeded under it: saying OK would let a launcher run on believing its session has
		// an identity, and leave the one kept for that name to the sweep.
		return Response{Code: CodeNotASession, Error: fmt.Sprintf("the session this process was in ended before it could be named %s; run: agent-secrets register", sessionID)}, true
	}
	if !got.Named {
		// The session took a name between this call's own reading and the claim: another
		// adoptSessionID won the race. A rename nobody made is not a registration that
		// succeeded under this name, so re-read and answer for what the session IS — the same
		// refusal the reading above would have given had it seen this identity.
		now := sess.identity()
		if now.SessionID != "" && now.SessionID != sessionID {
			return Response{Code: CodeBadRequest, Error: fmt.Sprintf("this process is in session %s, which cannot become session %s", now.SessionID, sessionID)}, true
		}
		return Response{}, false
	}
	s.save()
	for _, id := range got.Orphans {
		go s.revoke(ctx, id)
	}
	s.Log.Info("session named by its launcher; enrolling it under that name", "pid", sess.PID, "runtime_id", sess.RuntimeID(), "session_id", sessionID)
	return Response{}, false
}

// registerReply answers a register, first waiting up to wait for the session to enroll. Without a
// launcher credential the enroll loop cannot succeed until a human logs the helper in, so it does
// not wait at all: a launcher's `register --wait N` then costs nothing on a helper that was never
// logged in, or whose credential the broker has refused or that reached its expiry. A credential
// the broker revoked early, or one an older broker named no expiry for, stays held until a call is
// refused, so the first register after that still waits the full N. A
// session the helper cannot enroll for want of a credential gets Code NO_CREDENTIAL and that reason
// in Error, so a launcher can say what the session will do. The credential is read once, before
// the wait, and that one reading decides both: an unenrolled session's reply says NO_CREDENTIAL
// exactly when the wait was skipped for want of a credential, whatever a login or a refusal
// changed meanwhile. A session whose enrollment lapsed waits for its re-enrollment, since a lapse
// reopens its ready channel (markLapsed). The reply's id, state and error come from one snapshot,
// so a lapse landing after the wait cannot give it an id from before and a state from after.
//
// The wait ends early when the session is retired (its stop closes), and a retire can also land
// just as the wait runs out, so the reply is built only after a last membership check: a session
// the registry no longer holds is answered NOT_A_SESSION, not OK with an identity nothing live
// has. registerReply answers a session the caller has already found, so it takes the registry it
// belongs to as well.
func (s *Server) registerReply(sess *Session, wait time.Duration) Response {
	credential := s.Broker.HasCredential()
	if wait > 0 && credential {
		select {
		case <-sess.readyCh():
		case <-time.After(wait):
		case <-sess.stop:
		}
	}
	if !s.Registry.Holds(sess) {
		return Response{Code: CodeNotASession, Error: "the session this process was in ended before this register could answer for it; run: agent-secrets register"}
	}
	st := sess.snapshot()
	resp := Response{OK: true, EnrollmentID: st.EnrollmentID, RuntimeID: sess.RuntimeID(), Operator: s.Broker.Operator(), State: st.State, Error: st.LastError}
	// A session that is still enrolling — including one holding an id inherited from a record
	// that no enroll has confirmed — cannot reach the broker at all while the helper holds no
	// credential, so the launcher is told that rather than left with an id it cannot use.
	if resp.State != "enrolled" && !credential {
		resp.Code, resp.Error = CodeNoCredential, noCredentialMsg
	}
	return resp
}

// notEnrolled answers sign or sign-request for a registered session with no enrollment yet: it is
// enrolling (NOT_ENROLLED) while the helper holds a launcher credential, and without one the
// helper enrolls no one until a human logs it in, so the session has no broker identity
// (NO_CREDENTIAL). NOT_ENROLLED names the last attempt's failure when there is one: an enroll
// error, or a lapse's refused renew (markLapsed). st is the snapshot the caller found unenrolled,
// so the failure named is the one that goes with that answer.
func (s *Server) notEnrolled(st enrollmentState) Response {
	if !s.Broker.HasCredential() {
		return Response{Code: CodeNoCredential, Error: noCredentialMsg}
	}
	msg := "this session is not enrolled with the broker yet"
	if st.LastError != "" {
		msg += "; last attempt: " + st.LastError
	}
	return Response{Code: CodeNotEnrolled, Error: msg}
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
	sess, resp, ok := s.resolveDescendant(peer, pid, fmt.Sprintf("pid %d is not inside a registered machine session; a session root is started with `agent-secrets register --exec -- <agent argv>`", pid))
	if !ok {
		return resp
	}
	// One reading: the key that signs must be the key of the enrollment the proof names, even
	// if a rename lands between the two.
	cur := sess.identity()
	st := sess.snapshot()
	id := cur.EnrollmentID
	if id == "" || st.EnrollmentID != id {
		return s.notEnrolled(st)
	}
	compact, err := proof.Sign(cur.Key, id, method, url, time.Now())
	if err != nil {
		return Response{Code: CodeSign, Error: err.Error()}
	}
	return Response{OK: true, Proof: compact, EnrollmentID: id, RuntimeID: cur.RuntimeID}
}

// signRequest builds and signs a credential-request object naming one agent_secret
// authorization_detail per requested name (POST /v1/requests embeds this signed object
// rather than carrying "secrets"/"reason" as plain fields), and lifetimeSeconds, the grant's
// lifetime, when it is not nil, for a pid resolveDescendant ties to a registered session. The
// audience is always s.Broker.URL, never a value the peer supplies: a request object's aud claim
// must be the broker's own public URL for the broker to accept it, and there is no reason to trust
// an untrusted local peer's opinion of that value over the helper's own configuration.
func (s *Server) signRequest(peer *Peer, pid int, names []string, reason string, lifetimeSeconds *int) Response {
	if len(names) == 0 {
		peer.Close()
		return Response{Code: CodeBadRequest, Error: "sign-request needs at least one secret name"}
	}
	sess, resp, ok := s.resolveDescendant(peer, pid, fmt.Sprintf("pid %d is not inside a registered machine session; a session root is started with `agent-secrets register --exec -- <agent argv>`", pid))
	if !ok {
		return resp
	}
	if st := sess.snapshot(); st.EnrollmentID == "" {
		return s.notEnrolled(st)
	}
	details := make([]record.AuthorizationDetail, len(names))
	for i, name := range names {
		details[i] = record.AuthorizationDetail{Type: record.KindAgentSecret, Identifier: name, Actions: []string{"inject"}}
	}
	compact, err := record.Sign(sess.identity().Key, s.Broker.URL, details, reason, "", lifetimeSeconds, time.Now())
	if err != nil {
		return Response{Code: CodeSign, Error: err.Error()}
	}
	return Response{OK: true, RequestObject: compact}
}

// signLauncher signs a launcher proof with the machine credential for the operator's own machine
// and grant commands: the four operator routes under this helper's broker (operatorRoute; Broker.URL
// is never a value the peer supplies, as signRequest's audience is not), and nothing else. It is
// refused (IN_SESSION) to a process inside a registered session's process tree, and to one whose
// ancestry walk cannot show it is outside every such tree (Registry.RootOrUnknown), so a session's
// own commands cannot act as its operator. A process that leaves its session's tree (a double
// fork or setsid, reparented to init or a subreaper) is outside every check this makes, and the
// operator's user can stop the helper anyway: the refusal guards a session's own process tree, not
// a boundary against code running as that user. The walks run while peer's pidfd stays open, and
// their answer is trusted only once peer.PID() still names pid, the pid-reuse rule sign follows
// (resolveDescendant), so a session process the kernel replaced mid-walk is never handed a proof.
// With no credential it answers NO_CREDENTIAL. Beside the proof it answers the credential's id
// (not secret: the broker lists it), so a machine revoke can tell this machine's own login. The
// broker's answer to the call goes to the peer, not here: a refused credential is dropped at the
// helper's own next launcher call.
func (s *Server) signLauncher(peer *Peer, pid int, method, url string) Response {
	if method == "" || url == "" {
		peer.Close()
		return Response{Code: CodeBadRequest, Error: "sign-launcher needs method and url"}
	}
	if !operatorRoute(s.Broker.URL, method, url) {
		peer.Close()
		b := redact.URL(s.Broker.URL)
		return Response{Code: CodeBadRequest, Error: fmt.Sprintf("sign-launcher signs only GET %[1]s/v1/operator/machines, POST %[1]s/v1/operator/machines/{id}/revoke, GET %[1]s/v1/operator/grants and POST %[1]s/v1/operator/grants/{id}/revoke; not %[2]s %[3]s", b, method, redact.URL(url))}
	}
	sess, unknown := s.Registry.RootOrUnknown(pid)
	if sess != nil {
		peer.Close()
		return Response{Code: CodeInSession, Error: fmt.Sprintf("pid %d is inside a registered machine session, which acts on itself alone; run machine and grant commands from your own shell", pid)}
	}
	if unknown != nil {
		peer.Close()
		return Response{Code: CodeInSession, Error: fmt.Sprintf("pid %d may be inside a registered machine session: %v; run machine and grant commands from your own shell", pid, unknown)}
	}
	if peer.PID() != pid {
		peer.Close()
		return Response{Code: CodeUnidentified, Error: "the peer no longer matches the identified pid"}
	}
	peer.Close()
	compact, cred, err := s.Broker.launcherProof(method, url)
	if errors.Is(err, errNoCredential) {
		return Response{Code: CodeNoCredential, Error: noCredentialMsg}
	}
	if err != nil {
		return Response{Code: CodeSign, Error: err.Error()}
	}
	return Response{OK: true, Proof: compact, CredentialID: cred.id}
}

// operatorRoute reports whether method and url are one of the broker's four operator routes
// under broker: GET /v1/operator/machines, POST /v1/operator/machines/{id}/revoke,
// GET /v1/operator/grants or POST /v1/operator/grants/{id}/revoke, with no query or fragment
// (the broker binds a proof to its path alone) and {id} one path segment that does not unescape
// to empty, ".", ".." or a "/". The id need not be a UUID: the broker's own answer names one
// that is not.
func operatorRoute(broker, method, url string) bool {
	path, ok := strings.CutPrefix(url, broker)
	if !ok {
		return false
	}
	switch path {
	case "/v1/operator/machines", "/v1/operator/grants":
		return method == http.MethodGet
	}
	if method != http.MethodPost {
		return false
	}
	for _, list := range []string{"/v1/operator/machines/", "/v1/operator/grants/"} {
		if id, ok := strings.CutPrefix(path, list); ok {
			id, ok = strings.CutSuffix(id, "/revoke")
			return ok && pathSegment(id)
		}
	}
	return false
}

// pathSegment reports whether seg is one path segment naming something: no '/', '?' or '#', and
// an unescaped form that is not empty, ".", ".." and holds no '/'.
func pathSegment(seg string) bool {
	if strings.ContainsAny(seg, "/?#") {
		return false
	}
	raw, err := neturl.PathUnescape(seg)
	return err == nil && raw != "" && raw != "." && raw != ".." && !strings.Contains(raw, "/")
}

// unregister retires a pid resolveDescendant ties to a registered session.
func (s *Server) unregister(ctx context.Context, peer *Peer, pid int) Response {
	sess, resp, ok := s.resolveDescendant(peer, pid, fmt.Sprintf("pid %d is not inside a registered machine session", pid))
	if !ok {
		return resp
	}
	s.retire(ctx, sess, "unregistered")
	return Response{OK: true, RuntimeID: sess.RuntimeID()}
}

func (s *Server) sessions() Response {
	return Response{OK: true, Sessions: sessionInfos(s.Registry.List())}
}

// sessionInfos is list as the sessions a sessions or drain answer carries.
func sessionInfos(list []*Session) []SessionInfo {
	infos := make([]SessionInfo, 0, len(list))
	for _, sess := range list {
		infos = append(infos, sess.Info())
	}
	return infos
}

// drainingResponse answers a register or enroll-session-key on a helper that drain has marked
// draining.
var drainingResponse = Response{Code: CodeDraining, Error: "the helper is restarting; retry"}

func (s *Server) drain() Response {
	infos := sessionInfos(s.Registry.Drain())
	if len(infos) == 0 {
		s.Log.Info("draining: refusing new sessions until restart")
	}
	return Response{OK: true, Sessions: infos}
}

// login starts (or reports the running) machine login and returns its human-facing
// confirmation code and the page where a person types it (the broker's verification_uri). A pass-through broker call: it needs no registered session, so the
// dispatcher closes peer before calling it.
func (s *Server) login(ctx context.Context) Response {
	code, err := s.Broker.Login(ctx, s.Hostname)
	if err != nil {
		return Response{Code: CodeLoginFailed, Error: err.Error()}
	}
	ls := s.Broker.LoginStatus()
	return Response{OK: true, Code: code, LoginState: ls.State, VerificationURI: ls.VerificationURI}
}

// loginStatus reports the most recent machine login, current or settled, whether the helper holds
// a launcher credential and when it expires; an empty LoginState means none has ever run.
// CredentialHeld stays true through a re-login that is denied, expires or is pending while an
// earlier login's credential is held, and LoginRefused marks a credential the helper dropped
// rather than a login nobody approved, with CredentialDropped saying why (the broker refused it,
// or it reached its expiry).
func (s *Server) loginStatus() Response {
	ls := s.Broker.LoginStatus()
	resp := Response{OK: true, Code: ls.Code, LoginState: ls.State, CredentialHeld: ls.CredentialHeld, LoginRefused: ls.Refused, CredentialDropped: ls.Dropped}
	if !ls.CredentialExpiresAt.IsZero() {
		resp.CredentialExpiresAt = ls.CredentialExpiresAt.UTC().Format(time.RFC3339)
	}
	return resp
}

// cannotEnrollMsg is the ERROR an enrollment (a machine session's or a box's) logs while the
// helper holds no launcher credential to attempt it with, beside why (noCredentialReason).
const cannotEnrollMsg = "session cannot enroll: the helper holds no launcher credential; run: agent-secrets machine login, and have a human approve it"

// lacksCredential reports whether err failed for want of a launcher credential the helper still
// lacks: errNoCredential, unless a login has installed one since the attempt failed. That login
// woke the retry, which uses the new credential at once, so the failure is an ordinary retry
// rather than a session that cannot enroll.
func (s *Server) lacksCredential(err error) bool {
	return errors.Is(err, errNoCredential) && !s.Broker.HasCredential()
}

// enrollSessionKey enrolls a key a launcher holds for a session on this machine — a pass-through
// broker call requiring no registered session (see Broker.EnrollSessionKey), so unlike sign it
// does not check descendancy at all. An enrollment the helper cannot attempt for want of a
// launcher credential logs cannotEnrollMsg, as a registered session's does.
func (s *Server) enrollSessionKey(ctx context.Context, runtimeID, thumbprint string, sessionID *string) Response {
	if runtimeID == "" || thumbprint == "" {
		return Response{Code: CodeBadRequest, Error: "enroll-session-key needs runtime_id and thumbprint"}
	}
	id, lease, err := s.Broker.EnrollSessionKey(ctx, runtimeID, thumbprint, sessionID)
	if err != nil {
		if s.lacksCredential(err) {
			s.Log.Error(cannotEnrollMsg, "runtime_id", runtimeID, "why", s.Broker.noCredentialReason())
		}
		return Response{Code: CodeEnrollFailed, Error: err.Error()}
	}
	return Response{OK: true, EnrollmentID: id, LeaseExpires: lease.UTC().Format(time.RFC3339Nano)}
}

// unenrollSessionKey ends an enrollment enroll-session-key made; idempotent like Broker.Revoke
// (204/404 both succeed).
func (s *Server) unenrollSessionKey(ctx context.Context, enrollmentID string) Response {
	if enrollmentID == "" {
		return Response{Code: CodeBadRequest, Error: "unenroll-session-key needs enrollment_id"}
	}
	if err := s.Broker.Revoke(ctx, enrollmentID); err != nil {
		return Response{Code: CodeUnenrollFailed, Error: err.Error()}
	}
	return Response{OK: true}
}

// adopt starts the two goroutines every session has: enrollment with renewal, and the exit
// watch. A session Recover re-pinned arrives with its recorded old enrollment as its lapsed id
// (Session.setLapsed), which enrollLoop revokes before its first Broker.Enroll call.
func (s *Server) adopt(ctx context.Context, sess *Session) {
	go s.enrollLoop(ctx, sess)
	go s.watchExit(ctx, sess)
}

// errFinal marks an error no retry can mend: the attempt's precondition no longer holds, or the
// broker has answered and its answer will not change, so retryUntilStop ends at once without
// success and the caller decides what happens next rather than spinning here.
var errFinal = errors.New("no retry can mend this")

// errRenamed, returned by a try, ends retryUntilStop at once without success: the identity moved
// under the attempt, so the caller must run its own loop again. enrollLoop returns it when a
// rename lands under an enroll retry, and confirmInherited when one lands under its renew.
var errRenamed = fmt.Errorf("the session was renamed while it was enrolling: %w", errFinal)

// errRefused, returned by confirmInherited's try, ends its retry: the broker ANSWERED and would
// not honour the inherited enrollment, which is the one outcome that justifies enrolling afresh.
var errRefused = fmt.Errorf("the broker will not honour this enrollment: %w", errFinal)

// retryUntilStop retries try with backoff until it succeeds, stop closes, ctx ends, or try
// answers an error wrapping errFinal. Delay
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
		if errors.Is(err, errFinal) {
			onFail(attempt, err, 0, false)
			return false
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
// renew comes back here and enrolls again with the same key. Each pass first revokes the
// session's lapsed id, if it has one: a renew-refused enrollment (renewLoop's markLapsed), or a
// re-pinned session's recorded old enrollment (Recover's setLapsed). So a fresh enroll for the
// same runtime_id never races the old row's revoke. That race matters against the real broker,
// whatever it looks like against the fake one here: the real broker's idempotent-enroll conflict
// is keyed on (launcher_credential_id, runtime_id), not the session's signing-key thumbprint, so
// a still-live old row for this runtime_id refuses a fresh enroll outright (409
// ALREADY_ENROLLED, a hard error) even though the new session presents a different key. And the
// broker's idempotent-enroll conflict path can otherwise keep answering the same dead id forever
// (a base-branch bug tracked separately). Revoking first, with revokeLapsed's
// retry-until-gone loop, makes the old row's absence (or the session ending first) a
// precondition of the enroll attempt rather than a race with it; while the session lives the
// lapsed id stays on its record until then, so a helper restart meanwhile still revokes it. A
// session that ends before that revoke succeeds hands the id to the bounded revoke (revokeLapsed),
// and if that fails too the id ends with its lease.
func (s *Server) enrollLoop(ctx context.Context, sess *Session) {
	for {
		if lapsed := sess.lapsed(); lapsed != "" {
			if !s.revokeLapsed(ctx, sess, lapsed) {
				return
			}
			// If the slot no longer holds the id just revoked, a rename landed under the
			// revoke and put the enrollment BLOCKING the new runtime id there. clearLapsedIf
			// leaves it, and the loop must start again to revoke THAT before enrolling: the
			// broker refuses a second row for the runtime id this session is about to enrol
			// while the blocker lives, so proceeding would 409 until its lease ends.
			if !sess.clearLapsedIf(lapsed) {
				continue
			}
		}
		// An identity reclaimed from a record, or one a restart re-pinned, arrives holding an
		// enrollment the broker made for THIS key under THIS runtime id but which nothing has
		// confirmed since. Renew it before enrolling: a renew authenticates with the session's
		// own key alone, so it confirms that very row whatever launcher credential the helper
		// holds now, while an enroll is matched on (launcher credential, runtime id) and a
		// machine that has logged in again would be handed a NEW enrollment, abandoning the
		// pending requests and grants of the one the session came back for. Only the broker
		// REFUSING it falls through to the enroll below; see confirmInherited.
		if pass := sess.identity(); pass.EnrollmentID != "" && !pass.Confirmed {
			lease, outcome := s.confirmInherited(ctx, sess, pass)
			switch outcome {
			case confirmStopped:
				return
			case confirmMoved:
				continue
			case confirmKept:
				if !sess.setEnrolledAt(pass.EnrollmentID, pass.Generation) {
					// The identity moved between the renew and this write. The new one
					// decides what happens next — it may itself be an inherited enrollment
					// to confirm rather than one to enrol — so start the pass again.
					continue
				}
				s.save()
				s.Log.Info("session kept the enrollment its record held; the broker still honours it",
					"runtime_id", pass.RuntimeID, "enrollment_id", pass.EnrollmentID)
				if !s.renewLoop(ctx, sess, lease, pass.Generation) {
					return
				}
				continue
			}
			// confirmRefused: the broker answered that it will not honour that enrollment,
			// so the session needs a new one.
		}
		var id string
		var lease time.Time
		var generation uint64
		// renamed ends the retry at once when the identity moves under it: the pass above
		// revoked the lapsed id this identity had, and a rename brings a NEW one that must be
		// revoked before the new runtime id can enrol at all (the broker refuses a second row
		// for one runtime id outright). Retrying on here would spin against that refusal,
		// since this retry waits only on a login, never on the rename's wake.
		renamed := false
		pass := sess.identity().Generation
		// A login wakes the retry, so a session registered before it enrolls within about a second
		// rather than when a backoff of up to a minute comes round.
		enrolled := retryUntilStop(ctx, sess.stop, s.Broker.CredentialInstalled, 0, time.Second, time.Minute, func(attempt int, err error, delay time.Duration, retrying bool) {
			if renamed {
				return
			}
			sess.setError(err.Error())
			if s.lacksCredential(err) {
				s.Log.Error(cannotEnrollMsg, "runtime_id", sess.RuntimeID(), "why", s.Broker.noCredentialReason(), "in", delay)
				return
			}
			s.Log.Warn("enroll failed; retrying", "runtime_id", sess.RuntimeID(), "error", err, "in", delay)
		}, func() error {
			// ONE reading per attempt, and the generation of that very reading: the loop can
			// retry across a rename, and an attempt that enrolled the NEW identity must be
			// recognised as the new one's. Reading the generation once outside this closure
			// would make such an answer look superseded, and the id it carries — which may be
			// the enrollment the new identity was just handed back — would be lapsed and
			// revoked, destroying the grant this change exists to keep.
			cur := sess.identity()
			if cur.Generation != pass {
				renamed = true
				return errRenamed
			}
			generation = cur.Generation
			var err error
			id, lease, err = s.Broker.EnrollIdentity(ctx, cur)
			return err
		})
		if renamed {
			s.Log.Info("this session was named while it was enrolling; starting again under its new name", "runtime_id", sess.RuntimeID())
			continue
		}
		if !enrolled {
			return
		}
		if !sess.setEnrolledAt(id, generation) {
			// A rename landed while this enroll was in flight. The id belongs to the name this
			// session no longer has, so it blocks nothing the session is about to enrol: it
			// takes the lapsed slot only while that slot is free. The rename may have put the
			// enrollment BLOCKING the new runtime id there (takeLapsed), and displacing that
			// one would leave the next enroll refused 409 for as long as its row lives; this
			// id goes to the bounded revoke instead.
			s.Log.Info("an enrollment arrived for a name this session no longer has; enrolling again under its new one",
				"runtime_id", sess.RuntimeID(), "enrollment_id", id)
			if !sess.adoptLapsed(id) {
				go s.revoke(ctx, id)
			}
			continue
		}
		s.save()
		s.Log.Info("session enrolled", "runtime_id", sess.RuntimeID(), "enrollment_id", id)
		if !s.renewLoop(ctx, sess, lease, generation) {
			return
		}
	}
}

// confirmation is what confirmInherited concluded about an inherited enrollment.
type confirmation int

const (
	confirmStopped confirmation = iota // the session ended, or the daemon is exiting
	confirmKept                        // the broker still honours it; the lease is the renew's
	confirmRefused                     // the broker answered that it does not; enrol afresh
	confirmMoved                       // the identity moved under the renew; start the pass again
)

// confirmInherited proves that the broker still honours the enrollment an inherited identity
// arrived holding, by renewing it with that identity's own retained key.
//
// Only the broker ANSWERING and refusing means the row is no longer this session's, and only
// that justifies enrolling afresh: an enroll is matched on (launcher credential, runtime id), so
// a machine that has logged in again is handed a NEW enrollment, and the pending requests and
// grants of the one the session came back for end with it. A renew that did not reach the
// broker, or that the broker could not answer, says nothing about the row — so it is retried
// with the enroll's own backoff rather than read as a refusal. A single blip between a record
// and the broker must not cost the session the grants that record exists to keep.
func (s *Server) confirmInherited(ctx context.Context, sess *Session, pass identity) (time.Time, confirmation) {
	var lease time.Time
	outcome := confirmStopped
	kept := retryUntilStop(ctx, sess.stop, s.Broker.CredentialInstalled, 0, time.Second, time.Minute, func(attempt int, err error, delay time.Duration, retrying bool) {
		switch {
		case errors.Is(err, errRenamed):
			outcome = confirmMoved
		case errors.Is(err, errRefused):
			outcome = confirmRefused
			s.Log.Info("the broker will not honour the enrollment this session's record held; enrolling instead",
				"runtime_id", pass.RuntimeID, "enrollment_id", pass.EnrollmentID, "error", err)
		default:
			s.Log.Warn("the renew that would keep the enrollment this session's record held did not reach the broker; retrying",
				"runtime_id", pass.RuntimeID, "enrollment_id", pass.EnrollmentID, "error", err, "in", delay)
		}
	}, func() error {
		// The renew signs for ONE reading. A rename landing under it has given the session
		// another identity, and this answer speaks for neither confirming nor refusing that.
		if cur := sess.identity(); cur.Generation != pass.Generation || cur.EnrollmentID != pass.EnrollmentID {
			return errRenamed
		}
		next, _, err := s.Broker.renewIdentity(ctx, pass)
		if err != nil {
			if _, ended := enrollmentNotLive(err); ended {
				return fmt.Errorf("%w (%w)", errRefused, err)
			}
			return err
		}
		lease = next
		return nil
	})
	if kept {
		return lease, confirmKept
	}
	return time.Time{}, outcome
}

// enrollmentNotLive reports the broker's one verdict that an enrollment has ended:
// ENROLLMENT_NOT_LIVE, with the exact PROOF_INVALID "not live" fallback older brokers still
// answer, retained until they are retired. It is deliberately narrow. Every other refusal —
// a skewed clock's PROOF_INVALID above all, which a corrected clock mends — says nothing about
// the row, and reading one as the end of an enrollment costs a session every pending request
// and grant that enrollment carries. renewLoop and confirmInherited draw the line in the same
// place, so a renewal and a confirmation cannot disagree about what ended an enrollment.
func enrollmentNotLive(err error) (string, bool) {
	var be *BrokerError
	if !errors.As(err, &be) {
		return "", false
	}
	if be.Code == "ENROLLMENT_NOT_LIVE" || (be.Code == "PROOF_INVALID" && strings.Contains(be.Message, "not live")) {
		return be.Code, true
	}
	return be.Code, false
}

// renewLoop renews at a third of the lease until the session ends (false) or the broker says this
// enrollment ended (true: the caller enrolls again). ENROLLMENT_NOT_LIVE is that final new-broker
// verdict. An older broker still says PROOF_INVALID with "not live"; retain that exact fallback
// until old brokers are retired. Other proof failures, including clock skew, keep the lease and
// retry: a corrected clock can make the next proof valid.
//
// generation is the identity reading this loop renews. The wake is buffered one and nobody has
// to be waiting when it is sent, so a rename that landed BEFORE this enrollment was made or
// confirmed leaves one sitting in the channel for this loop to find. Acting on such a wake would
// enrol again and abandon the live enrollment the session just proved, which is the whole of what
// this change exists to prevent — so a wake counts only while the identity has in fact moved on
// from the one being renewed.
func (s *Server) renewLoop(ctx context.Context, sess *Session, lease time.Time, generation uint64) bool {
	for {
		interval := leasetime.HelperRenewAfter(time.Until(lease), s.MinRenew)
		select {
		case <-time.After(interval):
		case <-sess.wakeCh():
			if sess.identity().Generation == generation {
				continue
			}
			// Rename lapsed this session's enrollment: enrol again now, under its new runtime
			// id, rather than at the next renewal. enrollLoop revokes the lapsed id first.
			return true
		case <-sess.stop:
			return false
		case <-ctx.Done():
			return false
		}
		next, renewed, err := s.Broker.Renew(ctx, sess)
		if code, ended := enrollmentNotLive(err); ended {
			// The refusal speaks for the enrollment this renew signed, not for whatever the
			// session holds now. A rename landing while the request was out has given it
			// ANOTHER identity — possibly one just reclaimed from a dormant record, with the
			// grants this change exists to keep — and lapsing that one here would have
			// enrollLoop revoke it. markLapsedAt applies the refusal only while the session
			// still holds the very enrollment that was refused.
			id, applied := sess.markLapsedAt(renewed, "the broker refused this session's renew ("+code+"); enrolling again")
			if !applied {
				s.Log.Info("a renew was refused for an enrollment this session no longer holds; leaving its current one alone",
					"runtime_id", sess.RuntimeID(), "code", code, "enrollment_id", renewed.EnrollmentID)
				return true
			}
			s.Log.Warn("renew refused; revoking the lapsed enrollment before enrolling again", "runtime_id", sess.RuntimeID(), "code", code, "enrollment_id", id)
			return true
		}
		if err != nil {
			s.Log.Warn("renew failed; keeping the current lease", "runtime_id", sess.RuntimeID(), "error", err)
			continue
		}
		lease = next
	}
}

// revokeLapsed retries a lapsed enrollment's revoke (revokeOnce) until it succeeds, the session
// ends, or ctx is done. Backoff matches enrollLoop's: 1 s doubling to a 1-minute cap, retried
// indefinitely rather than a fixed number of times — giving up would hand the dead id straight
// back to the broker's idempotent-enroll conflict path, which can keep answering it forever —
// and, like enrollLoop's, woken by a login, since a revoke needs the credential too.
//
// Returns false only when the session ended or ctx was canceled first. A session that ends
// mid-backoff cannot leave the id to retire, which revokes only sess.EnrollmentID(), and a
// lapsed id is never that. So the id goes to the same bounded, independent revoke retire uses,
// decoupled from the session: three tries, 2 s apart. They need a launcher credential like any
// revoke, so before a login they fail, and the id is left to its lease; nothing renews that row
// again, since the session's key went with it. ctx done means the whole daemon is exiting, and
// nothing further is attempted.
func (s *Server) revokeLapsed(ctx context.Context, sess *Session, id string) bool {
	revoked := retryUntilStop(ctx, sess.stop, s.Broker.CredentialInstalled, 0, time.Second, time.Minute, func(attempt int, err error, delay time.Duration, retrying bool) {
		if s.lacksCredential(err) {
			s.Log.Error("session cannot enroll: revoking its lapsed enrollment first needs a launcher credential, and the helper holds none; run: agent-secrets machine login, and have a human approve it",
				"runtime_id", sess.RuntimeID(), "enrollment_id", id, "why", s.Broker.noCredentialReason(), "in", delay)
			return
		}
		s.Log.Warn("revoking the lapsed enrollment failed; retrying", "runtime_id", sess.RuntimeID(), "enrollment_id", id, "error", err, "in", delay)
	}, func() error { return s.revokeOnce(ctx, id) })
	if !revoked && ctx.Err() == nil {
		go s.revoke(ctx, id)
	}
	return revoked
}

// revokeOnce revokes an enrollment once. One refusal counts as done rather than failed: 403
// OPERATOR_MISMATCH, when the enrollment was made under another operator's launcher credential
// (a helper logged back in as someone else). This credential can never revoke it, and it cannot
// block a fresh enrollment either, since the broker's conflict is keyed on the launcher
// credential; the old enrollment ends with its own lease. revokeLapsed and the bounded revoke it
// hands ids to both use it, so a mismatch is final wherever a lapsed id is revoked.
func (s *Server) revokeOnce(ctx context.Context, id string) error {
	err := s.Broker.Revoke(ctx, id)
	var be *BrokerError
	if errors.As(err, &be) && be.Status == http.StatusForbidden && be.Code == "OPERATOR_MISMATCH" {
		s.Log.Warn("the enrollment belongs to another operator's launcher credential; leaving it to its lease", "enrollment_id", id)
		return nil
	}
	return err
}

func (s *Server) watchExit(ctx context.Context, sess *Session) {
	if sess.peer == nil || !sess.peer.WaitExit(sess.stop) {
		return
	}
	s.retire(ctx, sess, "process exited")
}

// DefaultDormantGrace is how long a named session's identity is kept after its process goes
// away, before its enrollment is revoked. A session the launcher named can come back — an omp
// restarted in its pane, an `agentbox move`, a reboot's resume — and ending its enrollment
// cancels every request its approver has not yet answered and revokes every grant they have
// given, which is what dispatch://INFRA-1167 is about. It is well under the broker's own
// 15-minute lease, so a session that never comes back loses its grants here, not at the lease;
// and a session with no omp session id is revoked at once, exactly as before, since nothing
// could claim it.
//
// It belongs to the Server, not to a package variable, so a test can choose it without writing
// a global that the Server's own goroutines read — which is a data race, and was one: `go test
// -race` caught it in TestANamedSessionThatEndsIsKeptForTheGraceThenRevoked.
const DefaultDormantGrace = 3 * time.Minute

// SetDormantGrace chooses this Server's grace; zero restores DefaultDormantGrace. Safe to call
// while the Server runs, which is why the field behind it is atomic.
func (s *Server) SetDormantGrace(d time.Duration) { s.dormantGraceNS.Store(int64(d)) }

// dormantGrace is the Server's grace, DefaultDormantGrace when it names none.
func (s *Server) dormantGrace() time.Duration {
	if ns := s.dormantGraceNS.Load(); ns > 0 {
		return time.Duration(ns)
	}
	return DefaultDormantGrace
}

// retire forgets the session. One with no omp session id has its enrollment revoked at once
// (three tries; the lease is the fallback), as before. A named one is parked instead, for
// DormantGrace, so the same session coming back takes its identity — and its pending requests
// and grants — rather than a new one; sweepDormant revokes it if it does not.
// Idempotent: an unregister followed by the exit watch firing retires once.
//
// Leaving the live set and entering the dormant one are ONE registry step (Registry.Retire),
// and the identity the record is built from is read inside it. Done in two, a process coming
// back in between would find neither a live owner of the name nor a record kept for it, make
// itself a fresh identity, and leave the old one parked beside it where nothing can reclaim it
// and only the sweep can end it.
func (s *Server) retire(ctx context.Context, sess *Session, why string) {
	var cur identity
	park := s.dormantRecord(sess)
	retired := s.Registry.Retire(sess, func(read identity) (Record, bool) {
		cur = read
		return park(read)
	})
	if !retired {
		return
	}
	if sess.peer != nil {
		sess.peer.Close()
	}
	s.save()
	if cur.SessionID != "" {
		s.Log.Info("session retired; its identity is kept for it to come back to", "runtime_id", cur.RuntimeID, "why", why, "for", s.dormantGrace())
		return
	}
	// Nothing can claim an unnamed session's identity, so both its enrollments end now: the
	// live one and any still awaiting its revoke.
	for _, id := range nonEmpty(cur.EnrollmentID, cur.LapsedID) {
		s.revoke(ctx, id)
	}
	s.Log.Info("session retired", "runtime_id", cur.RuntimeID, "why", why)
}

// SweepDormant revokes the enrollments of every session that went dormant more than
// DormantGrace ago and did not come back, and forgets it: the one its key holds and any it was
// still to revoke. The helper runs it on a ticker (SweepLoop); a test calls it directly.
func (s *Server) SweepDormant(ctx context.Context) {
	expired := s.Registry.ExpiredDormant(time.Now().Add(-s.dormantGrace()))
	if len(expired) == 0 {
		return
	}
	for _, rec := range expired {
		ids := rec.enrollments()
		s.Log.Info("a session did not come back; ending its enrollment", "runtime_id", rec.RuntimeID, "session_id", rec.SessionID, "enrollment_ids", ids)
		for _, id := range ids {
			s.revoke(ctx, id)
		}
	}
	s.save()
}

// SweepLoop runs SweepDormant until ctx ends. every is the tick; cmd/agent-secrets-helper passes
// a minute, and a test a short one.
func (s *Server) SweepLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.SweepDormant(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// revoke is the bounded revoke: three tries, 2 s apart, then the lease is the fallback. It uses
// revokeOnce, so an enrollment of another operator counts as done here too.
func (s *Server) revoke(ctx context.Context, id string) {
	retryUntilStop(ctx, nil, nil, 3, 2*time.Second, 2*time.Second, func(attempt int, err error, delay time.Duration, retrying bool) {
		if !retrying {
			s.Log.Warn("revoke failed; the lease lapses on its own", "enrollment_id", id, "error", err)
		}
	}, func() error { return s.revokeOnce(ctx, id) })
}

func (s *Server) save() {
	if err := s.Registry.Save(); err != nil {
		s.Log.Error("saving session records", "error", err)
	}
}

// Recover runs at start, after the Broker has resumed any machine login it keeps (Broker.Start):
// every recorded session whose process is still the same incarnation is pinned again and adopted
// (enrolled and exit-watched).
//
// A record that carries a key — one of a session its launcher named — is re-pinned with THAT
// key under THAT runtime id, and keeps its enrollment: the broker's idempotent enroll answers
// the same key under the same runtime id with the row it already has, so the session's pending
// requests and its grants survive the restart. Nothing is lapsed, and nothing is revoked.
//
// A record with no key is re-pinned with a fresh one, as before, and takes its recorded
// enrollment as its lapsed id (setLapsed), so its own enrollLoop revokes that id before
// enrolling the fresh key, exactly as after a refused renew: the real broker's idempotent-enroll
// conflict is keyed on (launcher_credential_id, runtime_id), so a fresh enroll racing ahead of
// the old row's revoke can be refused outright (409 ALREADY_ENROLLED) even though it carries a
// fresh key. While the re-pinned session lives, the lapsed id is on its record from the save
// below until the revoke lands, so a second restart before then — with no machine login kept on
// disk, no revoke can succeed before the next login — still revokes it. That revoke runs inside
// the one goroutine adopt starts for the re-pinned session, never on this loop's own goroutine,
// so one session waiting on its own revoke never blocks Recover from moving on to the next
// recorded session. Should that session end while the revoke is still retrying, its record goes
// with it, and revokeLapsed hands the id to the bounded independent revoke; with no login those
// three tries cannot succeed either, and the id ends with its lease (see revokeLapsed's doc
// comment).
//
// A recorded process that is gone — every one of them after a reboot — is parked dormant when
// the record names an omp session id, so the session resuming under that id takes its identity
// back (reclaimIdentity) and SweepDormant revokes it only if DormantGrace passes without it; one
// with no session id has its enrollment revoked here, as before, since nothing could claim it.
//
// "Still the same incarnation" means the record's BOOT too, not just its pid and start ticks.
// Both of those are counted from boot and both repeat after one, so after a reboot an unrelated
// process can match the recorded pair exactly; records are durable, so matching one would re-pin
// that stranger with the recorded private key and the recorded enrollment, handing it the
// session's grants without it ever knowing the session id — the one secret this rests on. A
// record from any other boot, and one written before records named a boot at all, therefore
// takes the gone-process branch (Registry.SameBoot), which parks a named identity for the
// session itself to reclaim and revokes an unnamed one.
func (s *Server) Recover(ctx context.Context) {
	recs, err := LoadRecords(s.Registry.path)
	if err != nil {
		s.Log.Error("reading recorded sessions", "error", err)
		return
	}
	s.Registry.LoadDormant(recs)
	for _, rec := range recs {
		if rec.GoneAt != nil {
			continue // already dormant when the helper stopped; LoadDormant has it
		}
		var peer *Peer
		alive := s.Registry.SameBoot(rec)
		if alive {
			peer, err = PinPID(rec.PID)
			alive = err == nil && peer.PID() == rec.PID
		}
		if alive {
			ticks, err := StartTicks(rec.PID)
			alive = err == nil && ticks == rec.StartTicks
		}
		if !alive {
			if peer != nil {
				peer.Close()
			}
			gone := time.Now()
			rec.GoneAt = &gone
			// A named record is parked whether or not it has an enrollment yet, exactly as
			// retire parks one: a session saved before its first enroll landed — the helper
			// holding no launcher credential, a resume interrupted — still has a KEY the
			// broker may already know, and dropping it here would leave an identity nothing
			// could ever reclaim.
			if s.Registry.Park(rec) {
				s.Log.Info("recorded session is gone; its identity is kept for it to come back to", "runtime_id", rec.RuntimeID, "session_id", rec.SessionID, "for", s.dormantGrace())
				continue
			}
			s.Log.Info("recorded session is gone", "runtime_id", rec.RuntimeID)
			for _, id := range rec.enrollments() {
				go s.revoke(ctx, id)
			}
			continue
		}
		key, err := rec.PrivateKey()
		if err != nil {
			s.Log.Warn("the recorded key of a live session could not be read; it is re-pinned with a fresh one and its previous grants are revoked", "runtime_id", rec.RuntimeID, "error", err)
			key = nil
		}
		sess, err := newSession(rec.PID, rec.StartTicks, rec.RuntimeID, rec.SessionID, key, peer)
		if err != nil {
			peer.Close()
			s.Log.Error("key generation", "runtime_id", rec.RuntimeID, "error", err)
			continue
		}
		if key != nil {
			// The recorded key holds this enrollment at the broker, so the session carries it
			// — but inherited, not confirmed: it may have been revoked while the helper was
			// down, and only the enroll this session is about to make shows otherwise.
			if rec.EnrollmentID != "" {
				sess.inherit(rec.EnrollmentID)
			}
			orphans := takeLapsed(sess, nonEmpty(rec.LapsedEnrollmentID), false)
			s.Registry.Add(sess)
			s.Log.Info("session re-pinned after a restart with the key it had; it keeps its enrollment", "runtime_id", rec.RuntimeID, "enrollment_id", rec.EnrollmentID)
			s.adopt(ctx, sess)
			for _, id := range orphans {
				go s.revoke(ctx, id)
			}
			continue
		}
		// A fresh key: neither recorded enrollment is this session's, and the live one sits
		// under the very runtime id it is about to enrol, which the broker would refuse (409
		// ALREADY_ENROLLED). That one goes in the lapsed slot, to be revoked first with the
		// retry-until-gone loop; a second id belongs to a name this session no longer has, so
		// it blocks nothing and the bounded revoke ends it.
		for _, id := range takeLapsed(sess, rec.enrollments(), rec.EnrollmentID != "") {
			go s.revoke(ctx, id)
		}
		s.Registry.Add(sess)
		s.Log.Warn("session re-pinned after a restart with a fresh key; its previous grants are revoked", "runtime_id", rec.RuntimeID)
		s.adopt(ctx, sess)
	}
	s.save()
}
