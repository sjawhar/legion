// packages/envoy/internal/broker/helper/registry.go
//go:build linux

package helper

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
)

// Session is one registered host process and the broker identity the helper holds for it. Key
// never leaves this process beyond the records file, which holds it only for a session that has
// an omp session id and is 0600 under a 0700 directory; Record is what survives a restart.
//
// SessionID is the omp session id the launcher named with `register --session-id`, and "" for a
// session registered without one. It is what makes an identity outlive the process: the runtime
// id of a session that has one is `session-<SessionID>`, which is the same string after a reboot,
// while a pid-keyed session's `<host>:<pid>:<start ticks>` is nobody's to come back to: both pids
// and start ticks are counted from boot and both REPEAT after one, so that string names a process
// only within the boot that made it, and a record carries that boot's id (Record.BootID) to say
// which. Paired with the recorded key, the session id is what lets the broker hand the SAME enrollment
// back — and with it the requests Sami has not yet answered and the grants he has given, all of
// which are bound to the enrollment id alone and are cancelled when it ends (dispatch://INFRA-1167).
type Session struct {
	PID          int
	StartTicks   uint64
	RegisteredAt time.Time

	mu sync.Mutex
	// The four identity fields are mutable: Rename replaces all four at once when a launcher
	// names a session. Every reader takes them together through identity(), under this mutex,
	// never field by field — an enrollment must not pair one generation's runtime id with
	// another's thumbprint, and a signature must not pair an old enrollment id with a
	// replacement key. They are unexported so the compiler finds any reader that tries.
	runtimeID  string
	sessionID  string
	key        *ecdsa.PrivateKey
	thumbprint string
	// generation counts the identities this session has had. An enroll or a renew carries the
	// generation it started under, and its result is dropped when a rename has moved on since
	// (setEnrolledAt): otherwise an enroll in flight when a rename lands would install the old
	// identity's enrollment on the new one.
	generation   uint64
	enrollmentID string
	// confirmed says the broker itself answered with enrollmentID in this process's lifetime
	// (setEnrolledAt), rather than the helper having INHERITED it from a record (Session.inherit,
	// from a reclaim or a re-pin). An inherited id is the right one to sign with and the right
	// one to record — the broker holds it for this key under this runtime id — but nothing has
	// shown it is still live: it may have been revoked while the session was away. So the
	// session reads "enrolling" and `register --wait` waits for the enroll that confirms it,
	// rather than answering "enrolled" with an id the broker may refuse.
	confirmed bool
	// lapsedID is an enrollment of this session's runtime that must be revoked before it enrolls
	// again: one whose renew the broker refused (markLapsed), or, after a helper restart, the
	// re-pinned session's prior one, the one its record named (setLapsed, from Recover).
	// enrollLoop revokes it first (clearLapsed once done). While the session lives it stays on the
	// session's record, so a restart in between still revokes it once the helper is logged in. A
	// session that ends first leaves the record without it and hands the id to the bounded revoke
	// (revokeLapsed); if that fails too, as it must before a login, the id ends with its lease.
	lapsedID  string
	lastError string
	// ready closes when the session enrolls. A lapse (markLapsed) puts an open one in its place,
	// which the next enrollment closes, so `register --wait` waits for the re-enrollment too.
	ready chan struct{}
	stop  chan struct{} // closed when the session is removed
	// wake ends the renew wait at once when something OUTSIDE the loops lapses the enrollment —
	// today only a rename. markLapsed alone does not reach renewLoop, which otherwise waits a
	// third of the lease and acts only on a refused renew, so without this a renamed session
	// would keep signing as its old runtime id until that renewal came round. Buffered one and
	// sent to without blocking: a wake nobody is waiting for is one the next wait consumes.
	wake chan struct{}
	peer *Peer
}

// identity is one coherent reading of a session's broker identity: the four fields a rename
// replaces together, the generation that reading belongs to, and both enrollment ids — the live
// one and the lapsed one awaiting revocation — live at the same moment. Every caller that needs
// more than one of them takes an identity, so none can pair values from either side of a rename
// or of a lapse.
type identity struct {
	RuntimeID    string
	SessionID    string
	Key          *ecdsa.PrivateKey
	Thumbprint   string
	Generation   uint64
	EnrollmentID string
	// Confirmed says the broker itself answered with EnrollmentID; an inherited one has not
	// been shown to be live yet (Session.confirmed).
	Confirmed bool
	// LapsedID is the enrollment of this session's runtime that must still be revoked: one
	// whose renew the broker refused, or one a restart or a reclaim inherited. A record keeps
	// it in its own field, never folded into the live one, so a restart can tell the
	// enrollment the recorded key holds from one it only has to end.
	LapsedID string
}

func (s *Session) identity() identity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return identity{RuntimeID: s.runtimeID, SessionID: s.sessionID, Key: s.key, Thumbprint: s.thumbprint,
		Generation: s.generation, EnrollmentID: s.enrollmentID, Confirmed: s.confirmed, LapsedID: s.lapsedID}
}

// RuntimeID and SessionID are the single-field readings a log line or a reply needs; anything
// that needs two takes identity().
func (s *Session) RuntimeID() string { return s.identity().RuntimeID }
func (s *Session) SessionID() string { return s.identity().SessionID }

// newSession makes a session around key, or around a fresh key when key is nil. A caller passes
// one in only to keep an identity the broker already knows: Recover re-pinning a recorded
// session, or a register reclaiming a dormant one.
func newSession(pid int, ticks uint64, runtimeID, sessionID string, key *ecdsa.PrivateKey, peer *Peer) (*Session, error) {
	if key == nil {
		generated, err := proof.NewKey()
		if err != nil {
			return nil, err
		}
		key = generated
	}
	tp, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	return &Session{
		PID: pid, StartTicks: ticks, runtimeID: runtimeID, sessionID: sessionID, key: key, thumbprint: tp,
		RegisteredAt: time.Now(), ready: make(chan struct{}), stop: make(chan struct{}),
		wake: make(chan struct{}, 1), peer: peer,
	}, nil
}

// rename replaces the session's identity AND retires the enrollment that identity had, in one
// critical section, bumping the generation so an enroll or a renew already in flight cannot
// install its result afterwards. Both halves must be one step: between a new key and a cleared
// enrollment id, an identity() reading would pair the replacement key with the old enrollment,
// and sign and Renew would make a proof that enrollment's key cannot verify.
//
// enrollmentID is the enrollment the REPLACEMENT key already holds at the broker, when the key
// comes from a dormant record: the session takes it as its own, so the record's enrollment is
// not lost between the rename and the first enroll, and a sign in that window pairs the
// reclaimed key with the enrollment that key really holds. It is installed after the old one
// has been lapsed, so the record keeps both in their own fields — the live one and the one
// enrollLoop must still revoke — and a restart in between can tell them apart. "" leaves the
// session enrolling, as a fresh key must.
//
// Only Registry.ClaimName calls this, having already decided under its own mutex that the name
// is free. A session that already has an id answers false and is left untouched.
func (s *Session) rename(sessionID, runtimeID string, key *ecdsa.PrivateKey, enrollmentID, reason string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionID != "" {
		return false
	}
	s.sessionID, s.runtimeID, s.generation = sessionID, runtimeID, s.generation+1
	if key != nil {
		tp, err := proof.Thumbprint(&key.PublicKey)
		if err != nil {
			// The key cannot be used, so the enrollment it holds is not this session's either;
			// it keeps the key it had and enrols afresh under the new name.
			enrollmentID = ""
		} else {
			s.key, s.thumbprint = key, tp
		}
	}
	s.lapseLocked(reason)
	if enrollmentID != "" {
		// Inherited, not confirmed: the broker holds it for this key under this runtime id,
		// so it is the right id to sign with and to record, but nothing has shown it is still
		// live. The session's own enroll, woken now, settles that (Session.inherit).
		s.enrollmentID, s.confirmed = enrollmentID, false
	}
	return true
}

// wakeEnroll tells the session's loops to enrol again now; wakeCh is what they wait on.
func (s *Session) wakeEnroll() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Session) wakeCh() <-chan struct{} { return s.wake }

// SessionRuntimeID is the runtime id of a session named by an omp session id. It is the one place
// the shape is written; scripts/agentbox builds the same string for an agent box, so a session
// that moves between a box and the host keeps one enrollment.
func SessionRuntimeID(sessionID string) string { return "session-" + sessionID }

func (s *Session) EnrollmentID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enrollmentID
}

// enrollmentState is a session's enrollment as one read sees it: its id, the state that id means,
// and the last attempt's error. It is the only way to read the state or the error, so every reply
// built from them takes them from one moment.
type enrollmentState struct {
	EnrollmentID string
	State        string
	LastError    string
}

// snapshot reads the session's enrollment under one lock, so a reply built from it cannot pair
// one moment's id with another's state or error, however a lapse or an enrollment interleaves.
// The state is "enrolled" only for an id the broker itself answered with (confirmed); an id
// inherited from a record reads "enrolling" until an enroll confirms it, since nothing has
// shown the broker still honours it.
func (s *Session) snapshot() enrollmentState {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := "enrolled"
	if s.enrollmentID == "" || !s.confirmed {
		state = "enrolling"
	}
	return enrollmentState{EnrollmentID: s.enrollmentID, State: state, LastError: s.lastError}
}

// inherit gives the session an enrollment a record named for the key it is signing with: the
// right id to sign with and to record, but not one this helper has seen the broker honour, so
// it does not count as enrolled and does not close the ready channel. The session's own enroll,
// a second later, confirms it (the broker's idempotent enroll answers the same key under the
// same runtime id with the row it already has) or replaces it.
func (s *Session) inherit(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enrollmentID, s.confirmed = id, false
}

func (s *Session) setEnrolled(id string) {
	s.setEnrolledAt(id, s.identity().Generation)
}

// setEnrolledAt records an enrollment an attempt that began at generation produced. A rename
// since then has made that attempt the OLD identity's, whose enrollment this session must not
// take, so the result is dropped and false returned; the caller enrols again under the name it
// now has. Nothing else can change the generation, so a true answer means this id is this
// session's own. This is the only place an id becomes confirmed: the broker has just answered
// with it.
func (s *Session) setEnrolledAt(id string, generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.generation != generation {
		return false
	}
	s.enrollmentID, s.confirmed, s.lastError = id, true, ""
	select {
	case <-s.ready:
	default:
		close(s.ready)
	}
	return true
}

// readyCh is the channel that closes when the session next enrolls (or already has).
func (s *Session) readyCh() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

func (s *Session) setError(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastError = msg
}

// markLapsed is what a refused renew does: the broker no longer honours this enrollment (its lease
// lapsed, or it was revoked), so the session stops counting as enrolled at once — sign and
// sign-request answer as for any session still enrolling — and the enrollment becomes the lapsed
// id. reason becomes the session's last error, which those answers and a register reply report,
// until the next attempt replaces it. The session gets an open ready channel, which its
// re-enrollment closes. Returns the lapsed id.
func (s *Session) markLapsed(reason string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lapseLocked(reason)
	return s.lapsedID
}

// markLapsedAt is markLapsed for a refusal that speaks for ONE enrollment: the reading that
// signed the renew. It lapses the session only while that identity is still the one it has —
// same generation, same enrollment id — and otherwise changes nothing and answers false. A
// rename landing while the renew was in flight has given the session another identity, often
// one just reclaimed from a dormant record; lapsing that one on the old enrollment's refusal
// would have enrollLoop revoke it, destroying the grants the reclaim exists to keep.
func (s *Session) markLapsedAt(renewed identity, reason string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.generation != renewed.Generation || s.enrollmentID != renewed.EnrollmentID {
		return "", false
	}
	s.lapseLocked(reason)
	return s.lapsedID, true
}

// lapseLocked is markLapsed's body, for a caller already holding s.mu. An enrollment id this
// session holds becomes the lapsed one; an EMPTY one leaves the lapsed id where it is, since
// overwriting it with nothing would drop an enrollment the helper has promised to revoke and
// leave it to its lease.
func (s *Session) lapseLocked(reason string) {
	if s.enrollmentID != "" {
		s.lapsedID = s.enrollmentID
	}
	s.enrollmentID, s.confirmed, s.lastError = "", false, reason
	select {
	case <-s.ready:
		s.ready = make(chan struct{})
	default:
	}
}

// setLapsed gives a re-pinned session its recorded enrollment as the lapsed id.
func (s *Session) setLapsed(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lapsedID = id
}

// swapLapsed puts id in the lapsed slot whatever is there, and answers what it displaced ("" when
// the slot was free). Only an id that BLOCKS the session's next enroll earns that (takeLapsed).
func (s *Session) swapLapsed(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	displaced := s.lapsedID
	s.lapsedID = id
	if displaced == id {
		return ""
	}
	return displaced
}

// adoptLapsed takes id as the enrollment this session must revoke before it enrols again, but
// only when the slot is free: an id already there is one this session's own runtime holds, and
// dropping it would leave an enrollment the helper has promised to end with nothing but its
// lease. A false answer hands id back to the caller, which revokes it independently.
func (s *Session) adoptLapsed(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lapsedID != "" {
		return false
	}
	s.lapsedID = id
	return true
}

// lapsed is the session's lapsed id, "" when it has none.
func (s *Session) lapsed() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lapsedID
}

// clearLapsedIf forgets the lapsed id once its revoke is done — but only while it is still the
// one that was revoked. A rename landing under that revoke puts the enrollment BLOCKING the
// session's new runtime id in the slot (takeLapsed), and forgetting that one would leave the
// next enroll refused 409 ALREADY_ENROLLED for as long as its row lives, with nothing left to
// revoke it. Answers whether it cleared.
func (s *Session) clearLapsedIf(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lapsedID != id {
		return false
	}
	s.lapsedID = ""
	return true
}

func (s *Session) Info() SessionInfo {
	st := s.snapshot()
	return SessionInfo{PID: s.PID, RuntimeID: s.RuntimeID(), EnrollmentID: st.EnrollmentID, State: st.State, RegisteredAt: s.RegisteredAt.UTC().Format(time.RFC3339)}
}

// Record is a Session as the records file holds it: enough to re-pin the process after a helper
// restart and to revoke the enrollment it had, and, for a session the launcher named with an omp
// session id, enough to give that identity back to the same session when it comes back — the id
// and the key, as PKCS#8 DER, base64 (standard, padded), the same shape machine.json uses. A
// session with no SessionID carries no Key: its runtime id dies with the process anyway, so
// keeping the key would be exposure that buys nothing.
//
// GoneAt is when the helper last found the session's process gone (Recover) or watched it exit
// (retire): the record is dormant from then, and DormantGrace later its enrollment is revoked.
//
// EnrollmentID is the enrollment the recorded key holds, and LapsedEnrollmentID one of this
// session's runtimes that must still be revoked (a refused renew, or one a rename or a restart
// inherited). They are separate fields because a restart must tell them apart: folded into one,
// a record of a renamed session would name an enrollment its recorded key does NOT hold, and
// Recover would install it as live and never revoke it.
//
// BootID is the boot the PID and StartTicks were read on (readBootID). Both of those are counted
// from boot and both repeat after one, so only together with the boot id do they name one process
// incarnation; Recover re-pins a record only when its boot id is this boot's (Registry.SameBoot)
// and treats every other record as a process that is gone. A record written before this field
// existed has "" and is treated the same way.
type Record struct {
	PID                int        `json:"pid"`
	StartTicks         uint64     `json:"start_ticks"`
	BootID             string     `json:"boot_id,omitempty"`
	RuntimeID          string     `json:"runtime_id"`
	EnrollmentID       string     `json:"enrollment_id"`
	LapsedEnrollmentID string     `json:"lapsed_enrollment_id,omitempty"`
	SessionID          string     `json:"session_id,omitempty"`
	Key                string     `json:"key,omitempty"`
	GoneAt             *time.Time `json:"gone_at,omitempty"`
}

// enrollments is every enrollment the record names that is still the helper's to end: the live
// one and the lapsed one, in that order, without the empties.
func (rec Record) enrollments() []string { return nonEmpty(rec.EnrollmentID, rec.LapsedEnrollmentID) }

// nonEmpty is ids without the empty ones, keeping their order; nil when none is left.
func nonEmpty(ids ...string) []string {
	var out []string
	for _, id := range ids {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

// Key reads a record's signing key, nil when it has none and an error when what it has is not a
// P-256 PKCS#8 key (a records file edited or truncated): the caller then makes a fresh key, as
// it did before records carried one at all.
func (rec Record) PrivateKey() (*ecdsa.PrivateKey, error) {
	if rec.Key == "" {
		return nil, nil
	}
	der, err := base64.StdEncoding.DecodeString(rec.Key)
	if err != nil {
		return nil, fmt.Errorf("session %s: key: %w", rec.RuntimeID, err)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("session %s: key: %w", rec.RuntimeID, err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("session %s: key is not an ECDSA key", rec.RuntimeID)
	}
	return key, nil
}

// encodeKey is PrivateKey's inverse; a key that will not marshal is recorded as none, which costs
// the session its continuity and nothing else.
func encodeKey(key *ecdsa.PrivateKey) string {
	if key == nil {
		return ""
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(der)
}

// Registry is the live sessions, the dormant records of sessions that may come back, and the
// path both are saved to.
type Registry struct {
	mu     sync.Mutex
	saveMu sync.Mutex // serializes Save so the last rename is always the newest snapshot
	// closed, set by Close under saveMu, makes every later Save write nothing.
	closed bool
	path   string
	byPID  map[int]*Session
	// dormant is, by omp session id, the identity of a session whose process is gone but which
	// may come back: the key, the runtime id and the enrollment, kept until the same session id
	// registers again (reclaimDormant) or DormantGrace passes (sweepDormant, which revokes).
	// Only a session the launcher named can be here, since only its runtime id is the same
	// after a reboot.
	dormant map[string]Record
	// parent reads a process's parent for the ancestry walks: procParent, or a test's fake.
	parent func(int) (int, error)
	// bootID is the boot this registry is running on, read once at construction. Every record
	// Save writes carries it, and SameBoot compares a record's against it; a test sets it
	// before the registry is shared with any goroutine.
	bootID string
	// draining is set by Drain on a registry with no sessions and cleared only by Undrain: the
	// helper is about to be restarted, and a session registered now would lose its grants to the
	// restart.
	draining bool
}

func NewRegistry(path string) *Registry {
	return &Registry{path: path, byPID: map[int]*Session{}, dormant: map[string]Record{}, parent: procParent, bootID: readBootID()}
}

// SameBoot reports whether rec was written on the boot this registry is running on, which is
// what makes its PID and StartTicks mean one process incarnation (readBootID). It is false for a
// record from another boot, for one written before records named a boot at all, and for every
// record when this boot's id could not be read: a stranger that happens to match a recorded pid
// and start-ticks pair after a reboot must never be handed the recorded key.
func (r *Registry) SameBoot(rec Record) bool {
	return r.bootID != "" && rec.BootID == r.bootID
}

// Dormant is the record kept for sessionID, if any.
func (r *Registry) Dormant(sessionID string) (Record, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.dormant[sessionID]
	return rec, ok
}

// Park keeps rec for its session id, so the same session coming back gets its identity rather
// than a new one. A record with no session id is not parked: nothing could ever claim it.
func (r *Registry) Park(rec Record) bool {
	if rec.SessionID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dormant[rec.SessionID] = rec
	return true
}

// ExpiredDormant removes and returns every dormant record that went dormant before cutoff: the
// sessions that did not come back, whose enrollments the caller revokes.
func (r *Registry) ExpiredDormant(cutoff time.Time) []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Record
	for id, rec := range r.dormant {
		if rec.GoneAt == nil || rec.GoneAt.Before(cutoff) {
			out = append(out, rec)
			delete(r.dormant, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}

// claim is what ClaimName decided about one rename.
type claim struct {
	Named   bool     // the session now holds the name
	Held    *Session // another live session holds it, and nothing was changed
	Gone    bool     // this session has left the registry, and nothing was changed
	Orphans []string // dead ids the session's one lapsed slot could not hold
}

// reclaimed is the identity a session named sessionID takes: the runtime id and key it signs
// with, and the enrollments the dormant record it came from named. A fresh identity names
// neither EnrollmentID nor anything to revoke.
type reclaimed struct {
	RuntimeID string
	Key       *ecdsa.PrivateKey
	// EnrollmentID is the enrollment Key still holds at the broker, so the session takes it as
	// its own; "" for a fresh key, which must enrol before it has one.
	EnrollmentID string
	// Revoke are dead enrollments of this session's runtimes, the one that would BLOCK its
	// next enroll first (the broker's conflict is keyed on (launcher credential, runtime id),
	// so a live row under the runtime id it is about to enrol refuses it outright). The first
	// that fits becomes the session's lapsed id, which enrollLoop revokes with its
	// retry-until-gone loop before enrolling; the rest come back as orphans.
	Revoke []string
	// Blocking says Revoke[0] sits under RuntimeID itself, so the session cannot enrol at all
	// until it is gone: it takes the lapsed slot even from an id already there (takeLapsed).
	Blocking bool
}

// takeLapsed gives s as many of ids as its single lapsed slot holds — one — and answers the
// rest, which the caller ends with the bounded revoke instead.
//
// blocking says ids[0] is an enrollment under the runtime id s is ABOUT TO ENROL, which the
// broker would refuse a second row for outright (409 ALREADY_ENROLLED, its conflict keyed on
// (launcher credential, runtime id)). That one takes the slot even from an id already there,
// because only the slot's retry-until-gone loop is a precondition of the enroll; the id it
// displaces belongs to a runtime this session will never enrol again, so it blocks nothing and
// three bounded tries are enough for it.
func takeLapsed(s *Session, ids []string, blocking bool) []string {
	var orphans []string
	for i, id := range ids {
		if i == 0 && blocking {
			if displaced := s.swapLapsed(id); displaced != "" {
				orphans = append(orphans, displaced)
			}
			continue
		}
		if !s.adoptLapsed(id) {
			orphans = append(orphans, id)
		}
	}
	return orphans
}

// ClaimName gives a live session the omp session id sessionID and, with it, the identity a
// dormant record keeps for that name. Everything that decides the name's ownership happens
// under the registry mutex in ONE step: that this session is still a member, that no OTHER live
// session already holds the name, that the record kept for it is consumed, and that the
// session's identity fields are replaced (Session.rename). Split across two locks, two roots
// asking for one name could both find it free and both take it, and a record could be consumed
// by a rename that then does not happen.
//
// take turns the record — and whether there was one — into the identity the session claims; it
// runs under the mutex and must touch nothing else of the registry. The enrollment it names
// becomes the session's own: the broker's idempotent enroll answers the same key under the same
// runtime id with the row it already has, so what the session comes back to is the whole
// identity, its grants included. The enrollment the session held under its OLD name becomes its
// lapsed one, which enrollLoop revokes before enrolling the new runtime id, and the wake makes
// that happen now rather than at the next renewal.
//
// orphans are ids the record named that the session's one lapsed slot could not hold, because
// its own old enrollment is there; each belongs to a runtime this session will never enrol
// again, so none blocks anything, and the caller ends them with the bounded revoke.
//
// Held is the other live session holding the name, and then nothing is changed. Gone says this
// session has left the registry — an unregister or its exit watch retired it while the name was
// being read — so no live session took the name and the caller must not report success for one.
// Neither Named, Held nor Gone means the session took an id of its own meanwhile; the caller
// re-reads it and answers for what it now is.
func (r *Registry) ClaimName(s *Session, sessionID string, take func(rec Record, kept bool) reclaimed) claim {
	r.mu.Lock()
	if cur, member := r.byPID[s.PID]; !member || cur != s {
		r.mu.Unlock()
		return claim{Gone: true}
	}
	for _, other := range r.byPID {
		if other != s && other.SessionID() == sessionID {
			r.mu.Unlock()
			return claim{Held: other}
		}
	}
	rec, kept := r.dormant[sessionID]
	got := take(rec, kept)
	renamed := s.rename(sessionID, got.RuntimeID, got.Key, got.EnrollmentID,
		"this session has been named "+sessionID+"; enrolling it under that name")
	if !renamed {
		r.mu.Unlock()
		return claim{} // the session already has an id of its own; the caller re-reads it
	}
	if kept {
		delete(r.dormant, sessionID)
	}
	orphans := takeLapsed(s, got.Revoke, got.Blocking)
	r.mu.Unlock()
	s.wakeEnroll()
	return claim{Named: true, Orphans: orphans}
}

// Holds says whether this very session is still the registry's live one for its pid. A session
// found by a walk is a reading of a moment, and a retire — an unregister, or its exit watch —
// can land before the caller answers for it; ClaimName makes the same check inside its one
// locked step, and this is it for a caller that has no claim to make.
func (r *Registry) Holds(s *Session) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, member := r.byPID[s.PID]
	return member && cur == s
}

func (r *Registry) Add(s *Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byPID[s.PID] = s
}

// Retire takes s out of the live sessions and, when park answers a record to keep, puts that
// record in the dormant set — both in ONE critical section. Between the two a process coming
// back under the same omp session id would find neither a live owner nor a dormant record, make
// itself a new identity, and strand the old one beside it where nothing could ever reclaim it.
// park is given the session's identity read under the same mutex, so a rename cannot land
// between that reading and the record built from it.
//
// It closes s.stop, which ends the session's enroll and exit-watch goroutines, and it removes
// only that very session (a later session may hold the same pid once the pidfd is released), so
// retiring twice — an unregister followed by the exit watch firing — does nothing the second
// time, which is what the false answer says.
func (r *Registry) Retire(s *Session, park func(cur identity) (Record, bool)) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.byPID[s.PID]; !ok || cur != s {
		return false
	}
	close(s.stop)
	delete(r.byPID, s.PID)
	if park == nil {
		return true
	}
	if rec, keep := park(s.identity()); keep && rec.SessionID != "" {
		r.dormant[rec.SessionID] = rec
	}
	return true
}

// Remove is Retire for a caller with nothing to park: an unnamed session ending, a registration
// rolled back before it ever reached the dormant set, and a test's cleanup.
func (r *Registry) Remove(s *Session) bool { return r.Retire(s, nil) }

func (r *Registry) Get(pid int) *Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byPID[pid]
}

func (r *Registry) List() []*Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Session, 0, len(r.byPID))
	for _, s := range r.byPID {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// Root is the session pid belongs to: the one registered for pid itself or for one of its
// ancestors. nil when pid is in no session, or when a walk could not tell, which sign,
// sign-request and unregister refuse (resolveDescendant) and register treats as a new root.
func (r *Registry) Root(pid int) *Session {
	sess, _ := r.RootOrUnknown(pid)
	return sess
}

// RootOrUnknown is Root for a caller that must show pid is in no session: beside a nil session
// it answers an error when some walk from pid ended short of init (descendsFrom: an unreadable
// parent, a loop, the hop limit), since pid may then be inside that session.
func (r *Registry) RootOrUnknown(pid int) (*Session, error) {
	var unknown error
	for _, s := range r.List() {
		in, err := descendsFrom(pid, s.PID, r.parent)
		if in {
			return s, nil
		}
		if err != nil && unknown == nil {
			unknown = err
		}
	}
	return nil, unknown
}

// admission is what AdmitRoot decided about one registration.
type admission struct {
	// Session is the session the pid belongs to now: the newly inserted one when Won, else the
	// one it turned out to be inside already, or the live holder of the name when Held.
	Session *Session
	Won     bool // build's session was inserted; it is the pid's root from now on
	Held    bool // another live session already holds sessionID, and nothing was changed
	// Draining: the registry is about to be restarted and took nothing.
	Draining bool
	// Unidentified: commit refused the registration, and nothing was changed.
	Unidentified bool
	Err          error // build's error; nothing was changed
	// Record and Took are the dormant record the new session was built from, for a caller's
	// log line. Nothing has to be put back: a registration that does not commit consumes it.
	Record Record
	Took   bool
}

// AdmitRoot makes pid a session root, deciding the whole registration under the registry mutex
// in ONE step: that the registry is not draining, that pid is inside no live session already,
// that no live session holds sessionID, that the peer still names pid, that the dormant record
// kept for sessionID is consumed, and that the new session is inserted. Each of those split
// into its own lock is a race a registration can lose: two roots both finding a name free and
// both taking it, two registrations for one unregistered pid both winning, or a record consumed
// by a registration that then does not happen.
//
// build turns the dormant record — and whether there was one — into the session to insert. It
// runs under the mutex and must touch nothing else of the registry. Its error is returned
// unchanged with nothing consumed and nothing inserted, so a key that will not generate costs
// the session nothing it could have kept.
//
// commit is the caller's last word before the session becomes visible: register re-reads the
// peer's pid there, since the ancestry walk above and the build both ran while the kernel could
// have reused the pid number (Peer's doc comment). It is the LAST thing done before the insert,
// under the same lock, so a refused registration is never published at all — never found by
// another request, and never written to the records file by a Save running beside it, which a
// rollback after the fact cannot promise.
func (r *Registry) AdmitRoot(pid int, sessionID string, build func(rec Record, kept bool) (*Session, error), commit func() bool) admission {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.draining {
		return admission{Draining: true}
	}
	for _, s := range r.byPID {
		if in, _ := descendsFrom(pid, s.PID, r.parent); in {
			// This walk is as much the kernel's to invalidate as the one before the insert:
			// the pid may have been reused since the caller's own Root walk, and the session
			// found is then a STRANGER's, which must not be handed the name — or the dormant
			// identity — this registration asked for.
			if commit != nil && !commit() {
				return admission{Unidentified: true}
			}
			return admission{Session: s}
		}
	}
	if sessionID != "" {
		for _, s := range r.byPID {
			if s.SessionID() == sessionID {
				return admission{Session: s, Held: true}
			}
		}
	}
	var rec Record
	kept := false
	if sessionID != "" {
		rec, kept = r.dormant[sessionID]
	}
	sess, err := build(rec, kept)
	if err != nil {
		return admission{Err: err}
	}
	if commit != nil && !commit() {
		return admission{Unidentified: true}
	}
	if kept {
		delete(r.dormant, sessionID)
	}
	r.byPID[sess.PID] = sess
	return admission{Session: sess, Won: true, Record: rec, Took: kept}
}

// Drain returns the live sessions and, only when there are none, marks the registry draining,
// atomically with the check, so no session can register between an empty answer and the
// restart it licenses.
func (r *Registry) Drain() []*Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Session, 0, len(r.byPID))
	for _, s := range r.byPID {
		out = append(out, s)
	}
	if len(out) == 0 {
		r.draining = true
	}
	return out
}

// Undrain clears what Drain set, for a restart that did not happen.
func (r *Registry) Undrain() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.draining = false
}

// Draining reports whether Drain has marked the registry draining.
func (r *Registry) Draining() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.draining
}

// Save writes every live session's record and every dormant one, atomically, 0600 in a directory
// it makes 0700: a record of a named session holds that session's signing key, so nothing but
// this user may read the file or list the directory. After Close it writes nothing.
//
// Live and dormant are snapshotted under ONE registry lock. Read in two, a retire landing
// between them would put the same session in the file twice — live and dormant at once, so a
// restart would re-pin it AND keep a record the sweep later revokes the live session's own
// enrollment from — and a registration landing between them would write neither.
func (r *Registry) Save() error {
	r.saveMu.Lock()
	defer r.saveMu.Unlock()
	if r.closed {
		return nil
	}
	r.mu.Lock()
	recs := make([]Record, 0, len(r.byPID)+len(r.dormant))
	for _, s := range r.byPID {
		// One reading of the identity, so a record cannot pair one generation's runtime id
		// with another's key, nor this moment's live enrollment with another's lapsed one.
		id := s.identity()
		// The boot id goes with the pid and the start ticks: only all three together name one
		// process incarnation after a restart that was also a reboot (Registry.SameBoot).
		rec := Record{PID: s.PID, StartTicks: s.StartTicks, BootID: r.bootID, RuntimeID: id.RuntimeID,
			EnrollmentID: id.EnrollmentID, LapsedEnrollmentID: id.LapsedID, SessionID: id.SessionID}
		if id.SessionID != "" {
			rec.Key = encodeKey(id.Key)
		}
		recs = append(recs, rec)
	}
	for _, rec := range r.dormant {
		recs = append(recs, rec)
	}
	r.mu.Unlock()
	sort.Slice(recs, func(i, j int) bool { return recs[i].RuntimeID < recs[j].RuntimeID })
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(r.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// MkdirAll leaves an EXISTING directory's mode alone, so a records directory that was there
	// already — the one the machine login lives in, made by an older release or by hand — could
	// stay group- or world-readable while holding a session's signing key. Correct it on every
	// save, so no write lands in a directory anyone else can list.
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	// O_EXCL through CreateTemp-like semantics is not needed here (the name is ours and the
	// directory is 0700), but the mode is: WriteFile applies it only when it creates the file,
	// so a .tmp left by an interrupted save with a wider mode is corrected before it is written.
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

// LoadDormant takes the dormant records out of recs and keeps them, so a session that comes back
// after this helper restarted finds its identity where a session that came back after a process
// restart does. Recover calls it with what LoadRecords read.
func (r *Registry) LoadDormant(recs []Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range recs {
		if rec.SessionID != "" && rec.GoneAt != nil {
			r.dormant[rec.SessionID] = rec
		}
	}
}

// Close waits for a Save in progress, then makes every later Save write nothing, so a caller can
// remove the directory the records live in while a registration, a retire or an enrollment is
// still finishing: Serve returning does not wait for those, and each saves the records after the
// registry has changed. A test's cleanup calls it; the helper never does, since a session retired
// while the helper stops must still be recorded.
func (r *Registry) Close() {
	r.saveMu.Lock()
	defer r.saveMu.Unlock()
	r.closed = true
}

// LoadRecords reads what Save wrote; no file means no sessions.
func LoadRecords(path string) ([]Record, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var recs []Record
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return recs, nil
}
