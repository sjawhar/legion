package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
)

// The delivery lease's timing. Every pod of one machine runs a lease over one record; the pod
// holding it is the machine's one deliverer and every other pod is a standby. The holder renews
// every leaseRenewEvery and counts itself the holder until the start of its last successful
// renewal + leaseTTL - leaseMargin, on its monotonic clock. A standby reads the record every
// leaseStandbyPoll and takes it once the same revision has stayed unchanged for a full leaseTTL
// on its own monotonic clock, measured from when it first saw that revision. No wall clocks are
// compared: the holder's validity ends at its renewal's start + 4 s, and a standby takes no
// earlier than that write + 5 s, which is safe while the two pods' clock rates differ by less
// than 20 %.
const (
	leaseRenewEvery  = time.Second
	leaseTTL         = 5 * time.Second
	leaseMargin      = time.Second
	leaseStandbyPoll = 500 * time.Millisecond
)

// leaseConfig carries the lease's timing, so a test of this package shortens it;
// productionLeaseConfig is the only value the listener runs with.
type leaseConfig struct {
	renewEvery  time.Duration
	ttl         time.Duration
	margin      time.Duration
	standbyPoll time.Duration
}

func productionLeaseConfig() leaseConfig {
	return leaseConfig{renewEvery: leaseRenewEvery, ttl: leaseTTL, margin: leaseMargin, standbyPoll: leaseStandbyPoll}
}

// The listener state bucket (bus.ListenerStateBucket) keeps one value per key and drops a key
// nobody has written for listenerStateMaxAge, so a lease record nobody renews leaves the bucket
// after 10 minutes. listenerStateRequestWait is the JetStream wait of every request on the
// bucket's handle: a renewal that hangs must give up well inside the holder's validity
// (leaseTTL - leaseMargin) so its retry still lands in it. A bucket create the wait cuts short
// is a connection error the caller retries, and the retry finds the bucket the server made.
const (
	listenerStateMaxAge      = 10 * time.Minute
	listenerStateRequestWait = 2 * time.Second
)

// errListenerStateRefused marks a listener whose NATS user may not use the listener state
// bucket: no retry opens it until the grant changes. It wraps the server's own violation.
var errListenerStateRefused = errors.New("NATS refused KV bucket " + bus.ListenerStateBucket +
	": the listener's NATS user lacks the grant for KV bucket " + bus.ListenerStateBucket +
	" (infra/core ListenerPermissions)")

// openListenerState opens the listener state bucket, creating it at replicas with file storage,
// listenerStateMaxAge and one value per key when it does not exist, with wait as the JetStream
// wait of every request on the handle it returns (listenerStateRequestWait in production).
//
// A refusal under the connection's grant is errListenerStateRefused, which errors.Is tells apart
// from a connection error. nats-server drops a publish the grant refuses and answers it only with
// `-ERR 'Permissions Violation for Publish to "<subject>"'`, which nats.go records as the
// connection's last error and reports to no request, so the refused request itself runs out at
// wait. So a failed open is a refusal when its own error is such a violation, or when the
// connection's last error changed across the open to a violation of a subject of this bucket
// (its stream KV_envoy_listener, or its keys $KV.envoy_listener.>). nats.go keeps one last error
// per connection, so a violation another goroutine causes in between hides this one, and the
// open reads as a connection error its caller retries.
func openListenerState(conn *nats.Conn, replicas int, wait time.Duration) (bus.KeyValue, error) {
	before := conn.LastError()
	js, err := conn.JetStream(nats.MaxWait(wait))
	if err != nil {
		return bus.KeyValue{}, fmt.Errorf("open %s KV bucket: %w", bus.ListenerStateBucket, err)
	}
	kv, err := bus.EnsureKeyValue(js, &nats.KeyValueConfig{
		Bucket:   bus.ListenerStateBucket,
		History:  1,
		TTL:      listenerStateMaxAge,
		Storage:  nats.FileStorage,
		Replicas: replicas,
	})
	if err == nil {
		return kv, nil
	}
	if violation := listenerStateRefusal(err, before, conn.LastError()); violation != nil {
		return bus.KeyValue{}, fmt.Errorf("%w: %w", errListenerStateRefused, violation)
	}
	return bus.KeyValue{}, fmt.Errorf("open %s KV bucket: %w", bus.ListenerStateBucket, err)
}

// publishViolation captures the subject of the server's refusal of a publish.
var publishViolation = regexp.MustCompile(`Publish to "([^"]*)"`)

// listenerStateRefusal returns the permissions violation that refused a failed open of the
// listener state bucket, or nil when the open failed for another reason: err when it is itself
// a violation of a subject of the bucket, else after, the connection's last error once the open
// failed, when it differs from before, the last error before the open, and is such a violation.
func listenerStateRefusal(err, before, after error) error {
	if refusesListenerState(err) {
		return err
	}
	if !sameLastError(before, after) && refusesListenerState(after) {
		return after
	}
	return nil
}

// refusesListenerState reports whether err is the server's refusal of a publish to a subject of
// the listener state bucket: a JetStream API call on its stream KV_<bucket>, or a write to one of
// its keys, $KV.<bucket>.<key>.
func refusesListenerState(err error) bool {
	if err == nil || !errors.Is(err, nats.ErrPermissionViolation) {
		return false
	}
	match := publishViolation.FindStringSubmatch(err.Error())
	if match == nil {
		return false
	}
	subject, stream := match[1], ".KV_"+bus.ListenerStateBucket
	return strings.HasSuffix(subject, stream) || strings.Contains(subject, stream+".") ||
		strings.HasPrefix(subject, "$KV."+bus.ListenerStateBucket+".")
}

// sameLastError reports whether a and b are the same error value. nats.go records each
// violation as a new error, so a new refusal never equals the one it replaced. A value that
// cannot be compared counts as different rather than panicking.
func sameLastError(a, b error) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}

// leaseFields are the lease record's fields this build knows (leaseRecordKeys names their JSON
// keys). holder is the holding pod's instance (ENVOY_INSTANCE_ID or its hostname), so a standby
// names the active pod; boot is random per process start, so two starts of one instance differ;
// deliver_subject is the durable's deliver subject the holder receives on; service_since is when
// this machine's pods started serving without a gap, the server's timestamp of the write that
// began it; acquired_at is the server's timestamp of the holder's acquiring write (informational);
// released marks a record its holder gave up on a graceful stop.
type leaseFields struct {
	Holder         string    `json:"holder"`
	Boot           string    `json:"boot"`
	DeliverSubject string    `json:"deliver_subject"`
	ServiceSince   time.Time `json:"service_since,omitzero"`
	AcquiredAt     time.Time `json:"acquired_at,omitzero"`
	Released       bool      `json:"released"`
}

// leaseRecordKeys are leaseFields' JSON keys; any other key a record carries is a newer build's,
// kept across every rewrite.
var leaseRecordKeys = []string{"holder", "boot", "deliver_subject", "service_since", "acquired_at", "released"}

// leaseRecord is the lease's value: the fields this build knows and the JSON of every key it does
// not, which encode writes back unchanged. Its unknown map is never modified once decoded, so
// copies of a record may share it.
type leaseRecord struct {
	leaseFields
	unknown map[string]json.RawMessage
}

func decodeLeaseRecord(data []byte) (leaseRecord, error) {
	var record leaseRecord
	if err := json.Unmarshal(data, &record.leaseFields); err != nil {
		return leaseRecord{}, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return leaseRecord{}, err
	}
	for _, key := range leaseRecordKeys {
		delete(all, key)
	}
	if len(all) > 0 {
		record.unknown = all
	}
	return record, nil
}

func (r leaseRecord) encode() ([]byte, error) {
	known, err := json.Marshal(r.leaseFields)
	if err != nil || len(r.unknown) == 0 {
		return known, err
	}
	all := make(map[string]json.RawMessage, len(leaseRecordKeys)+len(r.unknown))
	maps.Copy(all, r.unknown)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(known, &fields); err != nil {
		return nil, err
	}
	maps.Copy(all, fields)
	return json.Marshal(all)
}

// leaseAcquisition is how a pod took the lease: what the record was when it wrote over it. The
// receive loop acts on it: over an expired record that was not released (the holder was killed
// or lost its node, or this pod's own renewals lapsed and then resumed) it waits one AckWait
// before it subscribes, so the messages the old holder had in hand are redelivered before newer
// ones, and it counts a durable still push-bound as the dead holder's binding; from an absent or
// released record it subscribes at once, and counts a push-bound durable as a pre-lease build's.
type leaseAcquisition int

const (
	leaseTakenAbsent leaseAcquisition = iota + 1
	leaseTakenReleased
	leaseTakenExpired
)

func (a leaseAcquisition) String() string {
	switch a {
	case leaseTakenAbsent:
		return "absent"
	case leaseTakenReleased:
		return "released"
	case leaseTakenExpired:
		return "expired"
	default:
		return "none"
	}
}

// leaseStatus is a lease at one instant.
type leaseStatus struct {
	// Holding is whether this process holds the lease now: it holds the record and is inside its
	// validity on the monotonic clock.
	Holding bool
	// Term counts this process's acquisitions, a resume after its renewals lapsed included, so a
	// term the receive loop subscribed for differs from a later one even when no change between
	// them reached it.
	Term uint64
	// How is how Term's acquisition was taken; zero before the first.
	How leaseAcquisition
	// ActiveInstance is the holder the record names as this process last wrote or read it, empty
	// when there is no record or it is released: a standby's /healthz names it.
	ActiveInstance string
	// ServiceSince is the record's service_since as last written or read, zero when no record
	// carried one.
	ServiceSince time.Time
}

// lease is one pod's view of its machine's delivery lease, the record lease.<durable> in the
// listener state bucket. Start runs it; Holding and Status read it at any time; Changes signals
// each acquisition and each loss; Release gives it up on a graceful stop.
type lease struct {
	kv       bus.KeyValue
	key      string
	instance string
	boot     string
	cfg      leaseConfig
	now      func() time.Time
	logger   *logging.Logger
	changes  chan struct{}

	// The loop's own state, read and written only by step (and by Release once the loop ended):
	// the revision a standby is timing and when it first saw it, whether the holder's lapse has
	// been signalled, and whether a streak of failed bucket calls has been logged.
	seenRev        uint64
	seenAt         time.Time
	lapseSignalled bool
	failing        bool

	mu             sync.Mutex
	held           bool
	rev            uint64
	validUntil     time.Time
	term           uint64
	how            leaseAcquisition
	record         leaseRecord
	deliverSubject string
	stop           context.CancelFunc
	done           chan struct{}
}

// newLease makes machineID's lease over kv for this process, instance, with timing cfg and now
// as its clock. now must carry a monotonic reading (time.Now in production): the lease only
// subtracts its readings and never compares them with another pod's.
func newLease(kv bus.KeyValue, machineID, instance string, cfg leaseConfig, now func() time.Time, logger *logging.Logger) *lease {
	var boot [8]byte
	_, _ = rand.Read(boot[:]) // crypto/rand.Read never returns an error
	return &lease{
		kv:       kv,
		key:      "lease." + bus.ListenerDurable(machineID),
		instance: instance,
		boot:     hex.EncodeToString(boot[:]),
		cfg:      cfg,
		now:      now,
		logger:   logger,
		changes:  make(chan struct{}, 1),
	}
}

// Start runs the lease until ctx ends or Release stops it: a standby polls and takes the record,
// a holder renews it. Start is called once.
func (l *lease) Start(ctx context.Context) {
	ctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	l.mu.Lock()
	l.stop, l.done = stop, done
	l.mu.Unlock()
	go func() {
		defer close(done)
		for {
			if waitUnlessDone(ctx, l.step()) != nil {
				return
			}
		}
	}()
}

// Changes signals that the lease was acquired or lost. It holds at most one signal, so changes
// that come faster than the reader collapse into one: the reader reads Status after each signal
// and acts on the state there (a Term that differs from the one it acted on is a new
// acquisition). A loss is signalled when the holder's validity runs out without a renewal (as
// soon as the loop next wakes, which is no later than the validity's end unless a bucket call is
// in flight then), when a renewal finds another pod's record, and on Release. Holding is exact at
// every instant and is what gates each message.
func (l *lease) Changes() <-chan struct{} { return l.changes }

// Holding reports whether this process holds the lease now.
func (l *lease) Holding() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holdingLocked()
}

func (l *lease) holdingLocked() bool {
	return l.held && l.now().Before(l.validUntil)
}

// Status reads the lease now.
func (l *lease) Status() leaseStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	status := leaseStatus{
		Holding:      l.holdingLocked(),
		Term:         l.term,
		How:          l.how,
		ServiceSince: l.record.ServiceSince,
	}
	if !l.record.Released {
		status.ActiveInstance = l.record.Holder
	}
	return status
}

// SetDeliverSubject records the durable's deliver subject this process receives on, written
// into the record from the next renewal or acquisition on.
func (l *lease) SetDeliverSubject(subject string) {
	l.mu.Lock()
	l.deliverSubject = subject
	l.mu.Unlock()
}

// Release stops the lease and, when this process holds the record, marks it released at its
// revision, keeping service_since, so a standby takes it at its next poll with no TTL wait. A
// record another pod has already taken is left as it is, and that is no error. It returns
// ctx.Err() when ctx ends before the loop has stopped, and the write's error when the record
// could not be marked; either way this process no longer holds the lease, and a record left
// unreleased is taken as an expired one.
func (l *lease) Release(ctx context.Context) error {
	l.mu.Lock()
	stop, done := l.stop, l.done
	l.mu.Unlock()
	if stop != nil {
		stop()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	l.mu.Lock()
	held, rev, record := l.held, l.rev, l.record
	l.held, l.validUntil = false, time.Time{}
	l.mu.Unlock()
	if !held {
		return nil
	}
	defer l.signal()
	record.Released = true
	value, err := record.encode()
	if err != nil {
		return fmt.Errorf("encode the released lease record: %w", err)
	}
	next, err := l.kv.Update(l.key, value, rev)
	switch {
	case err == nil:
		l.mu.Lock()
		l.rev, l.record = next, record
		l.mu.Unlock()
		l.logger.Info("listener lease released", slog.String("lease", l.key), slog.Uint64("revision", next))
		return nil
	case errors.Is(err, nats.ErrKeyExists):
		return nil
	default:
		return fmt.Errorf("mark lease %s released: %w", l.key, err)
	}
}

// signal records a change for Changes, without waiting for a reader.
func (l *lease) signal() {
	select {
	case l.changes <- struct{}{}:
	default:
	}
}

// step does one round of the lease, renewing it as the holder or reading and maybe taking it as
// a standby, and returns how long to wait before the next.
func (l *lease) step() time.Duration {
	l.mu.Lock()
	held := l.held
	l.mu.Unlock()
	if held {
		return l.renew()
	}
	return l.poll()
}

// renew rewrites the holder's record at the revision it last wrote. A renewal that lands counts
// from its own start; one that started after the validity ran out is a resume, a new term taken
// as over an expired record. A wrong revision means another pod took the record, and this
// process steps down; any other failure is retried every renewEvery, sooner when the validity
// ends first, so its end is signalled.
func (l *lease) renew() time.Duration {
	start := l.now()
	l.mu.Lock()
	lapsed := !start.Before(l.validUntil)
	record, rev := l.record, l.rev
	record.DeliverSubject = l.deliverSubject
	l.mu.Unlock()
	if lapsed && !l.lapseSignalled {
		l.lapseSignalled = true
		l.logger.Warn("listener lease lapsed: its renewals failed for the lease's validity", slog.String("lease", l.key))
		l.signal()
	}
	value, err := record.encode()
	if err != nil {
		l.noteFailure("encode", err)
		return l.cfg.renewEvery
	}
	next, err := l.kv.Update(l.key, value, rev)
	if errors.Is(err, nats.ErrKeyExists) {
		own, mine, err := l.ownRevision()
		if err != nil {
			l.noteFailure("read after a conflict", err)
			return l.cfg.renewEvery
		}
		if mine {
			// An earlier renewal landed though its reply was lost: the record is this process's
			// own write, so renew again at that revision. The validity is not extended until a
			// renewal's reply arrives.
			l.mu.Lock()
			l.rev = own
			l.mu.Unlock()
			return 0
		}
		l.stepDown()
		return 0
	}
	if err != nil {
		l.noteFailure("renew", err)
		l.mu.Lock()
		wait := min(l.cfg.renewEvery, l.validUntil.Sub(l.now()))
		l.mu.Unlock()
		if wait <= 0 {
			wait = l.cfg.renewEvery
		}
		return wait
	}
	l.recovered()
	l.lapseSignalled = false
	l.mu.Lock()
	l.rev, l.record = next, record
	l.validUntil = start.Add(l.cfg.ttl - l.cfg.margin)
	if lapsed {
		l.term++
		l.how = leaseTakenExpired
	}
	term := l.term
	l.mu.Unlock()
	if lapsed {
		l.logger.Info("listener lease resumed: a renewal after its validity found its own record current",
			slog.String("lease", l.key), slog.Uint64("term", term), slog.Uint64("revision", next))
		l.signal()
	}
	return l.cfg.renewEvery
}

// stepDown ends the holder's hold after a renewal found another pod's record.
func (l *lease) stepDown() {
	l.mu.Lock()
	l.held, l.rev, l.validUntil = false, 0, time.Time{}
	l.mu.Unlock()
	l.seenRev, l.lapseSignalled = 0, false
	l.logger.Warn("listener lease lost: another listener holds it", slog.String("lease", l.key))
	l.signal()
}

// ownRevision reads the record after a renewal's conflict and reports its revision and whether it
// is this process's own unreleased write (its boot), which only a renewal whose reply was lost
// leaves behind.
func (l *lease) ownRevision() (uint64, bool, error) {
	entry, err := l.kv.Get(l.key)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	record, err := decodeLeaseRecord(entry.Value())
	if err != nil {
		return 0, false, nil
	}
	return entry.Revision(), record.Boot == l.boot && !record.Released, nil
}

// poll reads the record as a standby and takes it when it is absent (Create), released (Update
// at its revision, at once) or unchanged for a full ttl since this process first saw its
// revision (Update at that revision).
func (l *lease) poll() time.Duration {
	entry, err := l.kv.Get(l.key)
	seen := l.now()
	if errors.Is(err, nats.ErrKeyNotFound) {
		l.recovered()
		l.mu.Lock()
		l.record = leaseRecord{}
		l.mu.Unlock()
		l.seenRev = 0
		return l.acquire(nil, leaseRecord{}, leaseTakenAbsent)
	}
	if err != nil {
		l.noteFailure("read", err)
		return l.cfg.standbyPoll
	}
	l.recovered()
	record, err := decodeLeaseRecord(entry.Value())
	if err != nil {
		// An unreadable record is taken as one that is not released, once it stays unchanged for
		// the ttl, and nothing of it is kept.
		if entry.Revision() != l.seenRev {
			l.logger.Warn("listener lease record unreadable", slog.String("lease", l.key),
				slog.Uint64("revision", entry.Revision()), slog.String("error", err.Error()))
		}
		record = leaseRecord{}
	}
	l.mu.Lock()
	l.record = record
	l.mu.Unlock()
	switch {
	case record.Released:
		return l.acquire(entry, record, leaseTakenReleased)
	case entry.Revision() != l.seenRev:
		l.seenRev, l.seenAt = entry.Revision(), seen
		return l.cfg.standbyPoll
	case seen.Sub(l.seenAt) < l.cfg.ttl:
		return l.cfg.standbyPoll
	default:
		return l.acquire(entry, record, leaseTakenExpired)
	}
}

// acquire writes this process's record over previous (nil: none) and, once it reads back the
// write's server timestamp, holds the lease from the write's start. service_since starts again
// at that timestamp when no record was there, when it carried none, or when its last write is
// more than two ttl older than this one, since then no pod of the machine was serving; otherwise
// it is kept. The record is renewed at once to store service_since and acquired_at. A write
// whose timestamp cannot be read back is not counted as held: the record then expires as any
// unreleased one does, and a standby, this one included, takes it after the ttl.
func (l *lease) acquire(previous nats.KeyValueEntry, record leaseRecord, how leaseAcquisition) time.Duration {
	start := l.now()
	l.mu.Lock()
	record.DeliverSubject = l.deliverSubject
	l.mu.Unlock()
	record.Holder, record.Boot, record.Released, record.AcquiredAt = l.instance, l.boot, false, time.Time{}
	value, err := record.encode()
	if err != nil {
		l.noteFailure("encode", err)
		return l.cfg.standbyPoll
	}
	var rev uint64
	if previous == nil {
		rev, err = l.kv.Create(l.key, value)
	} else {
		rev, err = l.kv.Update(l.key, value, previous.Revision())
	}
	if errors.Is(err, nats.ErrKeyExists) {
		l.seenRev = 0
		return 0
	}
	if err != nil {
		l.noteFailure("take", err)
		return l.cfg.standbyPoll
	}
	written, err := l.kv.GetRevision(l.key, rev)
	if err != nil {
		l.seenRev, l.seenAt = rev, l.now()
		if errors.Is(err, nats.ErrKeyNotFound) {
			return 0
		}
		l.noteFailure("read back", err)
		return l.cfg.standbyPoll
	}
	record.AcquiredAt = written.Created()
	if previous == nil || record.ServiceSince.IsZero() || written.Created().Sub(previous.Created()) > 2*l.cfg.ttl {
		record.ServiceSince = written.Created()
	}
	l.mu.Lock()
	l.held, l.rev, l.record = true, rev, record
	l.validUntil = start.Add(l.cfg.ttl - l.cfg.margin)
	l.term++
	l.how = how
	term := l.term
	l.mu.Unlock()
	l.seenRev, l.lapseSignalled = 0, false
	l.logger.Info("listener lease acquired", slog.String("lease", l.key), slog.String("from", how.String()),
		slog.Uint64("term", term), slog.Uint64("revision", rev), slog.Time("service_since", record.ServiceSince))
	l.signal()
	return l.renew()
}

// noteFailure logs the first failed bucket call of a streak; recovered logs its end.
func (l *lease) noteFailure(action string, err error) {
	if l.failing {
		return
	}
	l.failing = true
	l.logger.Warn("listener lease state unavailable", slog.String("lease", l.key),
		slog.String("action", action), slog.String("error", err.Error()))
}

func (l *lease) recovered() {
	if !l.failing {
		return
	}
	l.failing = false
	l.logger.Info("listener lease state available again", slog.String("lease", l.key))
}
