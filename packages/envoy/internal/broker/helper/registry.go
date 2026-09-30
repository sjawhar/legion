// packages/envoy/internal/broker/helper/registry.go
//go:build linux

package helper

import (
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
)

// Session is one registered host process and the broker identity the helper holds for it. Key
// never leaves this process; Record is what survives a restart.
type Session struct {
	PID          int
	StartTicks   uint64
	RuntimeID    string
	Key          *ecdsa.PrivateKey
	Thumbprint   string
	RegisteredAt time.Time

	mu           sync.Mutex
	enrollmentID string
	lapsedID     string // a refused renew's enrollment, until its revoke succeeds (markLapsed)
	lastError    string
	ready        chan struct{} // closed on the first successful enrollment
	stop         chan struct{} // closed when the session is removed
	peer         *Peer
}

func newSession(pid int, ticks uint64, runtimeID string, peer *Peer) (*Session, error) {
	key, err := proof.NewKey()
	if err != nil {
		return nil, err
	}
	tp, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	return &Session{
		PID: pid, StartTicks: ticks, RuntimeID: runtimeID, Key: key, Thumbprint: tp,
		RegisteredAt: time.Now(), ready: make(chan struct{}), stop: make(chan struct{}), peer: peer,
	}, nil
}

func (s *Session) EnrollmentID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enrollmentID
}

func (s *Session) State() string {
	if s.EnrollmentID() == "" {
		return "enrolling"
	}
	return "enrolled"
}

func (s *Session) LastError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastError
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

func (s *Session) setError(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastError = msg
}

// A session's lapsed id is an enrollment of its runtime that must be revoked before it enrolls
// again: one whose renew the broker refused (markLapsed), or, after a helper restart, the one its
// record named (setLapsed, from Recover). enrollLoop revokes it first (clearLapsed once done), and
// until then it stays on the session's record, so a restart in between still revokes it.

// markLapsed is what a refused renew does: the broker no longer honours this enrollment (its lease
// lapsed, or it was revoked), so the session stops counting as enrolled at once — sign and
// sign-request answer as for any session still enrolling — and the enrollment becomes the lapsed
// id. Returns it.
func (s *Session) markLapsed() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lapsedID, s.enrollmentID = s.enrollmentID, ""
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
	return SessionInfo{PID: s.PID, RuntimeID: s.RuntimeID, EnrollmentID: s.EnrollmentID(), State: s.State(), RegisteredAt: s.RegisteredAt.UTC().Format(time.RFC3339)}
}

// Record is a Session without its key: enough to re-pin the process after a helper restart and
// to revoke the enrollment it had.
type Record struct {
	PID          int    `json:"pid"`
	StartTicks   uint64 `json:"start_ticks"`
	RuntimeID    string `json:"runtime_id"`
	EnrollmentID string `json:"enrollment_id"`
}

// Registry is the live sessions plus the path their records are saved to.
type Registry struct {
	mu     sync.Mutex
	saveMu sync.Mutex // serializes Save so the last rename is always the newest snapshot
	path   string
	byPID  map[int]*Session
}

func NewRegistry(path string) *Registry {
	return &Registry{path: path, byPID: map[int]*Session{}}
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
// ancestors. nil when pid is in no session.
func (r *Registry) Root(pid int) *Session {
	for _, s := range r.List() {
		if DescendsFrom(pid, s.PID) {
			return s
		}
	}
	return nil
}

// addRootIfAbsent inserts sess as pid's session only if pid still has none, atomically with the
// check — the race register() alone cannot close: two simultaneous registrations for one
// unregistered pid must not both win. Returns the winning session and whether sess itself won.
func (r *Registry) addRootIfAbsent(pid int, sess *Session) (*Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.byPID {
		if DescendsFrom(pid, s.PID) {
			return s, false
		}
	}
	r.byPID[sess.PID] = sess
	return sess, true
}

// Save writes every session's record, atomically, 0600.
func (r *Registry) Save() error {
	r.saveMu.Lock()
	defer r.saveMu.Unlock()
	recs := make([]Record, 0)
	for _, s := range r.List() {
		recs = append(recs, Record{PID: s.PID, StartTicks: s.StartTicks, RuntimeID: s.RuntimeID, EnrollmentID: s.recordedEnrollmentID()})
	}
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
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
