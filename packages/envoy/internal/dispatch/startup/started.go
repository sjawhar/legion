// Package startup announces on the bus the build the whole Dispatch server fleet serves
// (LEGION-781, INFRA-555): one server.started envelope each time the commit every live server
// process serves changes, which the Legion daemon compares against the merge commits of its open
// production checks to learn that a Dispatch change reached production. "Every live process" is
// the registry's (internal/dispatch/processes): a rolling update that has replaced some pods but
// not all is not yet serving the new build, and a pod that restarts on the build the fleet already
// serves announces nothing.
//
// The announcement is a row of server_build_announcements before it is an envelope: whichever
// process finds the fleet agreeing on a commit the latest row does not name inserts the row, and
// whichever process has a connected bus publishes every row not yet published. So a process
// whose NATS is down announces into the table and leaves the publish to the first process whose
// bus answers. It goes directly through the bus client rather than the outbox: it is no issue's,
// document's or project's event, and the outbox publishes only those.
package startup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Subject is the one subject Dispatch publishes its fleet's build on, mirrored by
// DISPATCH_SERVER_STARTED_SUBJECT in packages/contracts/src/dispatch-api.ts and matched whole by the
// Legion daemon's intake. A subject of the notification stream (bus.StreamSubjects'
// notifications.dispatch.>), so the publish is a JetStream publish with an acknowledgement, and the
// stream's duplicate window recognises a second publish of one announcement.
const Subject = "notifications.dispatch.server.started"

// Source is the envelope source of every Dispatch publish, this one included.
const Source = "dispatch"

// Payload is a server.started envelope's payload, mirrored by DispatchServerStartedPayload in
// packages/contracts/src/dispatch-api.ts: the commit every live server process serves, and when
// the fleet was found serving it (the announcement's announced_at, on the database's clock), in
// UTC to the second. The time keeps the name started_at from the per-process start this replaced,
// since the daemon and the TypeScript schema read that name.
type Payload struct {
	Commit    string    `json:"commit"`
	StartedAt time.Time `json:"started_at"`
}

// Interval is how often a process looks for a fleet-wide build to announce and for an
// announcement to publish. A variable rather than a constant only so this package's tests can
// shorten it; nothing else sets it.
var Interval = 5 * time.Second

// statementTimeout bounds one pass's announcing transaction, well inside Interval, so a database
// that has stopped answering costs a pass and not the loop: the next tick tries again.
const statementTimeout = 3 * time.Second

// publishTimeout bounds one publish. The bus client bounds its own publish too, but a client that
// is still dialling or reconnecting must not hold the publishing transaction, and with it every
// other process's publish, for longer than this: the next pass tries again.
const publishTimeout = 10 * time.Second

// publishPassTimeout bounds a pass's publishing transaction as a whole, its statements and its
// publishes together, so a database link that stalls mid-pass ends the pass rather than the loop.
const publishPassTimeout = 30 * time.Second

// announceLock is the transaction advisory lock a pass takes, with pg_try_advisory_xact_lock,
// before it announces and again before it publishes, so one process at a time does either: two
// processes finding the fleet agreeing at once insert one row, and two with a connected bus
// publish it once. A pass that finds the lock held skips that step, since the holder is doing
// it. It is a single 64-bit key ("DANNOUNC"), apart from every other single-key advisory lock of
// the server for the reason processes' registryLock gives, and apart from that one.
const announceLock int64 = 0x44414E4E4F554E43

// Publisher is what an Announcer publishes through: the bus client.
type Publisher interface {
	// Connected reports whether the client has a live connection. A pass whose publisher has none
	// publishes nothing and takes no lock for it, so a process whose NATS is down never holds the
	// publish back from a process whose NATS answers.
	Connected() bool
	// PublishReportingDuplicate publishes the envelope and returns once the stream has
	// acknowledged it, reporting whether the stream already held it.
	PublishReportingDuplicate(contracts.Envelope) (bool, error)
}

// Deps is what an Announcer reads and publishes through.
type Deps struct {
	// Pool is the shared pool the announcements and the registry are read and written through.
	Pool *store.Pool
	// Publisher publishes announcements; nil where NATS is disabled, and such a process announces
	// into the table and publishes nothing.
	Publisher Publisher
}

// Announcer announces the fleet's build (Run) and remembers the latest announcement's commit
// (FleetCommit) for /healthz.
type Announcer struct {
	deps  Deps
	fleet atomic.Pointer[string]
}

// New returns an Announcer over deps; nothing runs until Run.
func New(deps Deps) *Announcer {
	return &Announcer{deps: deps}
}

// FleetCommit is the commit of the latest announcement as of this process's last pass, nil when
// there is none yet, or before the first pass has read one. It is nil on a nil Announcer.
func (a *Announcer) FleetCommit() *string {
	if a == nil {
		return nil
	}
	return a.fleet.Load()
}

// Run passes at once and then every Interval until ctx ends. Each pass announces the fleet's build
// when the live processes agree on one the latest announcement does not name (announce), then
// publishes every announcement not yet published (publish). A pass that fails is logged and the
// next one tries again.
func (a *Announcer) Run(ctx context.Context) {
	ticker := time.NewTicker(Interval)
	defer ticker.Stop()
	for {
		a.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// pass is one pass of Run.
func (a *Announcer) pass(ctx context.Context) {
	if err := a.announce(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("dispatch: announce the server fleet's build", "error", err)
	}
	if err := a.publish(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("dispatch: publish the server fleet's build; the next pass retries", "error", err)
	}
}

// announce reads the latest announcement and, holding announceLock, inserts a new one when every
// live process serves one non-empty commit that the latest does not name: a fleet that serves two
// builds is mid-roll, a fleet any of whose processes carries no stamp has no build to name, and a
// fleet still on the announced build has nothing new to say. A rollback to a build announced
// before is a change too, and is announced again under a new seq. Without the lock it only reads,
// since the holder decides. FleetCommit is updated once the transaction has committed.
func (a *Announcer) announce(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, statementTimeout)
	defer cancel()
	tx, err := a.deps.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	// A no-op once the transaction has committed.
	defer func() { _ = tx.Rollback(context.Background()) }()
	var locked bool
	if err := tx.QueryRow(ctx, `select pg_try_advisory_xact_lock($1)`, announceLock).Scan(&locked); err != nil {
		return err
	}
	latest, err := latestCommit(ctx, tx)
	if err != nil {
		return err
	}
	if locked {
		live, err := processes.Live(ctx, tx)
		if err != nil {
			return err
		}
		if commit, agreed := servedByEvery(live); agreed && (latest == nil || *latest != commit) {
			var seq int64
			if err := tx.QueryRow(ctx, `insert into server_build_announcements (commit) values ($1) returning seq`, commit).Scan(&seq); err != nil {
				return fmt.Errorf("insert the announcement of %s: %w", commit, err)
			}
			slog.Info("dispatch: every live server process serves one build; announced it", "commit", commit, "seq", seq, "processes", len(live))
			latest = &commit
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	a.fleet.Store(latest)
	return nil
}

// latestCommit is the commit of the announcement with the highest seq, nil when there is none.
func latestCommit(ctx context.Context, tx pgx.Tx) (*string, error) {
	var commit string
	err := tx.QueryRow(ctx, `select commit from server_build_announcements order by seq desc limit 1`).Scan(&commit)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the latest announcement: %w", err)
	}
	return &commit, nil
}

// servedByEvery is the commit every live process serves, and whether there is one: at least one
// process is live, and each serves the same non-empty commit.
func servedByEvery(live []processes.Process) (string, bool) {
	if len(live) == 0 || live[0].Commit == "" {
		return "", false
	}
	for _, p := range live[1:] {
		if p.Commit != live[0].Commit {
			return "", false
		}
	}
	return live[0].Commit, true
}

// pending is an announcement not yet published.
type pending struct {
	seq         int64
	commit      string
	announcedAt time.Time
}

// publish publishes every announcement whose published_at is null, oldest first, and sets
// published_at on each one the stream acknowledges, holding announceLock so one process publishes
// at a time. It does nothing while the publisher is absent or disconnected, and nothing when
// another process holds the lock. The first publish that fails ends the pass, so announcements
// reach the stream in their order; the acknowledged ones before it are kept. A publish that
// landed but whose acknowledgement or whose published_at was lost is published again by a later
// pass, and the stream, which keys a Dispatch envelope by its dedupe key, stores it once.
func (a *Announcer) publish(ctx context.Context) error {
	if a.deps.Publisher == nil || !a.deps.Publisher.Connected() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, publishPassTimeout)
	defer cancel()
	tx, err := a.deps.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	// A no-op once the transaction has committed.
	defer func() { _ = tx.Rollback(context.Background()) }()
	var locked bool
	if err := tx.QueryRow(ctx, `select pg_try_advisory_xact_lock($1)`, announceLock).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return nil
	}
	rows, err := tx.Query(ctx, `
		select seq, commit, announced_at from server_build_announcements
		where published_at is null
		order by seq`)
	if err != nil {
		return fmt.Errorf("read the unpublished announcements: %w", err)
	}
	unpublished, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (pending, error) {
		var p pending
		err := row.Scan(&p.seq, &p.commit, &p.announcedAt)
		return p, err
	})
	if err != nil {
		return fmt.Errorf("read the unpublished announcements: %w", err)
	}
	var failed error
	for _, p := range unpublished {
		item, err := Envelope(p.commit, p.seq, p.announcedAt)
		if err != nil {
			failed = err
			break
		}
		duplicate, err := publishWithin(ctx, a.deps.Publisher, item)
		if err != nil {
			failed = fmt.Errorf("publish %s: %w", item.EventID, err)
			break
		}
		if _, err := tx.Exec(ctx, `update server_build_announcements set published_at = now() where seq = $1`, p.seq); err != nil {
			failed = fmt.Errorf("record %s published: %w", item.EventID, err)
			break
		}
		slog.Info("dispatch: published the server fleet's build", "commit", p.commit, "seq", p.seq, "event_id", item.EventID, "duplicate", duplicate)
	}
	if err := tx.Commit(ctx); err != nil {
		return errors.Join(failed, err)
	}
	return failed
}

// publishWithin is one publish, given up after publishTimeout or when ctx ends. A publish given up
// on may still land; its announcement stays unpublished and a later pass publishes it again, which
// the stream recognises as a duplicate.
func publishWithin(ctx context.Context, publisher Publisher, item contracts.Envelope) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()
	type outcome struct {
		duplicate bool
		err       error
	}
	done := make(chan outcome, 1)
	go func() {
		duplicate, err := publisher.PublishReportingDuplicate(item)
		done <- outcome{duplicate, err}
	}()
	select {
	case o := <-done:
		return o.duplicate, o.err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// Envelope is the server.started envelope of announcement seq, which found every live process
// serving commit at announcedAt: its event id, source event id and dedupe key
// dispatch-server-started-<commit>-<seq>, source dispatch, topic Subject, issued at announcedAt,
// the summary `dispatch serves <sha7> on every server process`, the event id as its trace id (as
// the outbox's envelopes carry theirs), and Payload as JSON text. Every field is a function of the
// announcement's row, so every process builds the same envelope for it, and a second publish is
// the same message the stream already holds. Every field the daemon's envelope check requires is
// set; Validate holds it to the contract.
func Envelope(commit string, seq int64, announcedAt time.Time) (contracts.Envelope, error) {
	announcedAt = announcedAt.UTC().Truncate(time.Second)
	payload, err := json.Marshal(Payload{Commit: commit, StartedAt: announcedAt})
	if err != nil {
		return contracts.Envelope{}, fmt.Errorf("encode server.started payload: %w", err)
	}
	eventID := fmt.Sprintf("dispatch-server-started-%s-%d", commit, seq)
	item := contracts.Envelope{
		EventID:        eventID,
		Source:         Source,
		SourceEventID:  eventID,
		Topic:          Subject,
		DedupeKey:      eventID,
		IssuedAt:       announcedAt.UnixMilli(),
		PayloadSummary: "dispatch serves " + shortCommit(commit) + " on every server process",
		Payload:        string(payload),
		TraceID:        eventID,
	}
	if err := item.Validate(); err != nil {
		return contracts.Envelope{}, err
	}
	return item, nil
}

// shortCommit is the first seven characters of a commit sha, or the whole of a shorter string.
func shortCommit(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}
