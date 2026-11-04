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
// while a pid-keyed session's `<host>:<pid>:<start ticks>` can never be (both halves are
// boot-unique). Paired with the recorded key, it is what lets the broker hand the SAME enrollment
// back — and with it the requests Sami has not yet answered and the grants he has given, all of
// which are bound to the enrollment id alone and are cancelled when it ends (dispatch://INFRA-1167).
type Session struct {
	PID          int
	StartTicks   uint64
	RuntimeID    string
	SessionID    string
	Key          *ecdsa.PrivateKey
	Thumbprint   string
	RegisteredAt time.Time

	mu           sync.Mutex
	enrollmentID string
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
	peer  *Peer
}

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
		PID: pid, StartTicks: ticks, RuntimeID: runtimeID, SessionID: sessionID, Key: key, Thumbprint: tp,
		RegisteredAt: time.Now(), ready: make(chan struct{}), stop: make(chan struct{}), peer: peer,
	}, nil
}

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
func (s *Session) snapshot() enrollmentState {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := "enrolled"
	if s.enrollmentID == "" {
		state = "enrolling"
	}
	return enrollmentState{EnrollmentID: s.enrollmentID, State: state, LastError: s.lastError}
}

func (s *Session) setEnrolled(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enrollmentID, s.lastError = id, ""
	select {
	case <-s.ready:
	default:
		close(s.ready)
	}
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
	s.lapsedID, s.enrollmentID, s.lastError = s.enrollmentID, "", reason
	select {
	case <-s.ready:
		s.ready = make(chan struct{})
	default:
	}
	return s.lapsedID
}

// setLapsed gives a re-pinned session its recorded enrollment as the lapsed id.
func (s *Session) setLapsed(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lapsedID = id
}

// lapsed is the session's lapsed id, "" when it has none.
func (s *Session) lapsed() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lapsedID
}

// clearLapsed forgets the lapsed id once its revoke is done.
func (s *Session) clearLapsed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lapsedID = ""
}

// recordedEnrollmentID is the enrollment a restart must revoke: the live one, else the lapsed id
// not revoked yet.
func (s *Session) recordedEnrollmentID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enrollmentID != "" {
		return s.enrollmentID
	}
	return s.lapsedID
}

func (s *Session) Info() SessionInfo {
	st := s.snapshot()
	return SessionInfo{PID: s.PID, RuntimeID: s.RuntimeID, EnrollmentID: st.EnrollmentID, State: st.State, RegisteredAt: s.RegisteredAt.UTC().Format(time.RFC3339)}
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
type Record struct {
	PID          int        `json:"pid"`
	StartTicks   uint64     `json:"start_ticks"`
	RuntimeID    string     `json:"runtime_id"`
	EnrollmentID string     `json:"enrollment_id"`
	SessionID    string     `json:"session_id,omitempty"`
	Key          string     `json:"key,omitempty"`
	GoneAt       *time.Time `json:"gone_at,omitempty"`
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
	// draining is set by Drain on a registry with no sessions and cleared only by Undrain: the
	// helper is about to be restarted, and a session registered now would lose its grants to the
	// restart.
	draining bool
}

func NewRegistry(path string) *Registry {
	return &Registry{path: path, byPID: map[int]*Session{}, dormant: map[string]Record{}, parent: procParent}
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

// TakeDormant removes and returns the record kept for sessionID.
func (r *Registry) TakeDormant(sessionID string) (Record, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.dormant[sessionID]
	delete(r.dormant, sessionID)
	return rec, ok
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

// LiveSessionID reports whether a live session already holds sessionID: a second process must
// not be handed an identity another one is signing with.
func (r *Registry) LiveSessionID(sessionID string) *Session {
	if sessionID == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.byPID {
		if s.SessionID == sessionID {
			return s
		}
	}
	return nil
}

// Rename gives a live session its omp session id and the runtime id that goes with it, and the
// key an earlier incarnation of that session used when there is one. The enrollment it holds
// becomes its lapsed one, so enrollLoop revokes that before enrolling the new runtime id. It
// does nothing, and answers false, for a session that already has an id or has left the registry.
func (r *Registry) Rename(s *Session, sessionID, runtimeID string, key *ecdsa.PrivateKey) bool {
	r.mu.Lock()
	if cur, ok := r.byPID[s.PID]; !ok || cur != s || s.SessionID != "" {
		r.mu.Unlock()
		return false
	}
	s.SessionID, s.RuntimeID = sessionID, runtimeID
	if key != nil {
		if tp, err := proof.Thumbprint(&key.PublicKey); err == nil {
			s.Key, s.Thumbprint = key, tp
		}
	}
	r.mu.Unlock()
	s.markLapsed("this session has been named " + sessionID + "; enrolling it under that name")
	return true
}

func (r *Registry) Add(s *Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byPID[s.PID] = s
}

// Remove forgets a session and closes its stop channel, which ends its enroll and exit-watch
// goroutines. It removes only that very session (a later session may hold the same pid once
// the pidfd is released) and reports whether it did, so retiring twice — an unregister
// followed by the exit watch firing — does nothing the second time.
func (r *Registry) Remove(s *Session) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.byPID[s.PID]; !ok || cur != s {
		return false
	}
	close(s.stop)
	delete(r.byPID, s.PID)
	return true
}

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

// addRootIfAbsent inserts sess as pid's session only if pid still has none, atomically with the
// check — the race register() alone cannot close: two simultaneous registrations for one
// unregistered pid must not both win. Returns the winning session and whether sess itself won;
// on a draining registry it inserts nothing and returns draining true.
func (r *Registry) addRootIfAbsent(pid int, sess *Session) (winner *Session, won, draining bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.draining {
		return nil, false, true
	}
	for _, s := range r.byPID {
		if in, _ := descendsFrom(pid, s.PID, r.parent); in {
			return s, false, false
		}
	}
	r.byPID[sess.PID] = sess
	return sess, true, false
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
func (r *Registry) Save() error {
	r.saveMu.Lock()
	defer r.saveMu.Unlock()
	if r.closed {
		return nil
	}
	recs := make([]Record, 0)
	for _, s := range r.List() {
		rec := Record{PID: s.PID, StartTicks: s.StartTicks, RuntimeID: s.RuntimeID, EnrollmentID: s.recordedEnrollmentID(), SessionID: s.SessionID}
		if s.SessionID != "" {
			rec.Key = encodeKey(s.Key)
		}
		recs = append(recs, rec)
	}
	r.mu.Lock()
	for _, rec := range r.dormant {
		recs = append(recs, rec)
	}
	r.mu.Unlock()
	sort.Slice(recs, func(i, j int) bool { return recs[i].RuntimeID < recs[j].RuntimeID })
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
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
