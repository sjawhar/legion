package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// noticeExceptionSubjects is where the Envoy listener reports a role-lane forward it could not
// complete: notifications.envoy.exceptions.<the original role topic>
// (packages/envoy/cmd/listener/delivery.go, publishDeliveryException). A role topic is outside the
// notification stream, so the report goes over core NATS and reaches only a subscriber connected
// when it is sent; the TypeScript daemon subscribes to the same subjects
// (packages/daemon/src/daemon/events.ts).
const noticeExceptionSubjects = "notifications.envoy.exceptions." + notify.RoleTopicPrefix + "*"

// The listener's liveness windows for a role holder (packages/envoy/internal/session): a session's
// registration lives for listenerSessionTTL after its last heartbeat (registry.go), delivery drops
// a holder whose claim went unrefreshed for listenerClaimStale (claim.go, ClaimStaleAfter), and the
// plugin heartbeats every pluginHeartbeat. A session that stops running can therefore stay a
// registered holder for up to the longer of the two windows, and until then the listener accepts a
// publish for it, forwards it, and reports it undelivered.
const (
	listenerSessionTTL = 5 * time.Minute
	listenerClaimStale = 5 * time.Minute
	pluginHeartbeat    = 2 * time.Minute
)

// noticeReholdDelays is how long each copy of a notice whose forward failed waits: the nth copy
// waits the nth delay after the report of the one before it, except that a copy goes at once when
// copyDueAtOnce says so. The first catches a quick relaunch, the second one heartbeat later, and
// the last is sent after any registration a stopped session left behind must have lapsed, past both
// windows with a minute to spare. That copy then either reaches the relaunched session or is
// refused (notify.ErrNoHolder) and held by the executor until a session holds the role again, so
// nothing is dropped while the architect's claim lives. There are three, as the TypeScript daemon
// re-sends one at most three times (processes.ts, resendToRootArchitect): a holder that keeps its
// registration alive but never confirms a delivery would otherwise be sent copies without end, and
// the report of its last copy is logged and queues nothing.
var noticeReholdDelays = [...]time.Duration{30 * time.Second, pluginHeartbeat, max(listenerSessionTTL, listenerClaimStale) + time.Minute}

// errNoticeException is a report the listener sent that cannot be read as the failed forward of
// a notice.
var errNoticeException = errors.New("malformed role-lane exception")

// roleLaneException is the listener's report of one failed role-lane forward, the payload of its
// exception envelope: the original envelope's topic, summary, payload and dedupe key, the session
// the forward went to, and why it failed.
type roleLaneException struct {
	OriginalTopic    string `json:"original_topic"`
	EventID          string `json:"event_id"`
	Reason           string `json:"reason"`
	RecipientSession string `json:"recipient_session"`
	PayloadSummary   string `json:"payload_summary"`
	Payload          string `json:"payload"`
	DedupeKey        string `json:"dedupe_key"`
}

// subscribeNoticeExceptions subscribes rehold to every role-lane exception on conn.
func subscribeNoticeExceptions(conn *nats.Conn, rehold func(data []byte)) (*nats.Subscription, error) {
	sub, err := conn.Subscribe(noticeExceptionSubjects, func(msg *nats.Msg) { rehold(msg.Data) })
	if err != nil {
		return nil, fmt.Errorf("subscribe to role-lane exceptions: %w", err)
	}
	return sub, nil
}

// rehold queues a notice again when the listener reports that its forward to the owning
// architect's session did not complete after the publish was accepted: the forward failed
// (`delivery_failed`), the holder lapsed between the two (`no_holder`), or the session the
// listener forwarded to did not confirm it in time (`receipt_timeout`). A session that stopped
// running but is still registered produces that last one: the listener forwards to it and waits
// for a receipt that never comes. So does the relaunch window, until the claim's agent has taken
// the Envoy role back. Every such report is queued, a late receipt from the claim's own session
// too: a Go relaunch resumes the claim's session id, so a report naming it cannot tell the stopped
// process from the relaunched one, and a copy the session already has costs one frame its plugin
// drops, where a skipped copy the relaunched process never saw is a notice lost.
//
// The notice goes back through the executor as a new row of its issue, counted in its Resends,
// so it is routed, fenced and held as any notice is, and it is published under the dedupe key of
// the row it copies (Notice.Published), so a session that did get the forward recognises the copy.
// A copy is due at once when copyDueAtOnce says so, and otherwise after the next of
// noticeReholdDelays. It is queued again once per delay; the report of its last copy is logged
// and queues nothing. A report about another daemon's publish, another project, a role that is not
// an architect, or anything but a notice changes nothing. The exception lane has no redelivery, so
// a report that cannot be read is logged here and dropped. Each published copy of a row is queued
// again once, keyed by the row's dedupe key and the copy's count: a publish whose 200 was lost is
// retried under the same key, and the listener reports each failed forward under a fresh event id.
//
// The row finishes when the listener accepts the publish, and the listener reports a failed
// forward only after its receipt window (two seconds, longer while forwards queue behind others on
// its role lane). A notice to the same architect written before the report arrives is not held
// behind the failed one, so it can arrive before the copy. A catch-up a newer one superseded
// (catchUpSuperseded) is not queued again: its copy would arrive after the fresh catch-up. Nor is
// a notice whose architect stopped with its finished tree (stoppedWithTree): nothing of the close
// is sent to it. Those checks read the claim from memory, outside ApplyFact's lock, which the
// ready handler holds, so a copy can still lose the race to a ready; the notice executor checks
// again when it runs the row.
func (r *outbox) rehold(ctx context.Context, data []byte) error {
	if r.supervisor == nil {
		return errors.New("notice re-hold has no claim supervisor")
	}
	var envelope struct {
		EventID string `json:"event_id"`
		Source  string `json:"source"`
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("%w: decode the envelope: %w", errNoticeException, err)
	}
	if envelope.Source != "envoy" || strings.TrimSpace(envelope.EventID) == "" {
		return fmt.Errorf("%w: envelope %q from source %q", errNoticeException, envelope.EventID, envelope.Source)
	}
	var exception roleLaneException
	if err := json.Unmarshal([]byte(envelope.Payload), &exception); err != nil {
		return fmt.Errorf("%w: decode exception %s: %w", errNoticeException, envelope.EventID, err)
	}
	reported, ok := r.exceptionNotice(exception)
	if !ok {
		return nil
	}
	issue, notice := reported.issue, reported.notice
	switch exception.Reason {
	case "delivery_failed", "no_holder", "receipt_timeout":
	default:
		return fmt.Errorf("%w: exception %s names reason %q", errNoticeException, envelope.EventID, exception.Reason)
	}
	held := r.supervisedClaim(reported.architect)
	if !claimRuns(held.State) {
		lingers, err := r.issueTreeLingers(ctx, issue)
		if err != nil {
			return fmt.Errorf("re-hold the notice of exception %s: %w", envelope.EventID, err)
		}
		if stoppedWithTree(lingers, held.State) {
			r.log.Info("outbox notice not re-held: its architect stopped with its finished tree",
				"issue", issue, "kind", notice.Kind, "topic", exception.OriginalTopic, "key", exception.DedupeKey, "reason", exception.Reason, "state", held.State)
			return nil
		}
	}
	if notice.Resends >= len(noticeReholdDelays) {
		r.log.Warn("outbox notice not re-held again: it was queued again the most times a notice is",
			"issue", issue, "kind", notice.Kind, "topic", exception.OriginalTopic, "key", exception.DedupeKey, "reason", exception.Reason, "resends", notice.Resends)
		return nil
	}
	due := r.now().Add(noticeReholdDelays[notice.Resends])
	if copyDueAtOnce(held, notice.Resends, exception.RecipientSession) {
		due = r.now()
	}
	mark := fmt.Sprintf("%s#%d", exception.DedupeKey, notice.Resends)
	notice.Resends++
	notice.ResendOf = reported.row
	row, err := record.NewOutboxRow(issue, notice, due)
	if err != nil {
		return fmt.Errorf("%w: exception %s carries a notice the outbox refuses: %w", errNoticeException, envelope.EventID, err)
	}
	queued, superseded := false, false
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		if notice.CatchUp != nil {
			root, err := r.records.Issue(ctx, tx, issue)
			if err != nil {
				return err
			}
			if superseded = catchUpSuperseded(*notice.CatchUp, held, root); superseded {
				return nil
			}
		}
		fresh, err := r.records.MarkProcessed(ctx, tx, "envoy-exception", mark)
		if err != nil || !fresh {
			return err
		}
		queued = true
		return r.records.Enqueue(ctx, tx, row)
	}); err != nil {
		return fmt.Errorf("re-hold the notice of exception %s: %w", envelope.EventID, err)
	}
	if superseded {
		r.log.Info("outbox catch-up not re-held: a newer catch-up supersedes it", "issue", issue, "key", exception.DedupeKey,
			"generation", notice.CatchUp.Generation, "launch", notice.CatchUp.Launch, "reason", exception.Reason)
	}
	if queued {
		r.log.Info("outbox notice re-held: the listener could not forward it to its architect's session",
			"issue", issue, "kind", notice.Kind, "topic", exception.OriginalTopic, "key", exception.DedupeKey, "reason", exception.Reason)
	}
	return nil
}

// issueTreeLingers says whether the tree of issue, as recorded, lingers or has closed. An issue
// that is not recorded has no tree to finish.
func (r *outbox) issueTreeLingers(ctx context.Context, issue string) (bool, error) {
	lingers := false
	err := pgx.BeginTxFunc(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		recorded, err := r.records.Issue(ctx, tx, issue)
		if err != nil || recorded == nil {
			return err
		}
		lingers, err = record.TreeLingers(ctx, r.records, tx, recorded.Tree)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("read the tree of %s: %w", issue, err)
	}
	return lingers, nil
}

// catchUpSuperseded says whether a newer catch-up than catchUp exists for its architect: held, the
// claim as this daemon supervises it, has taken its role at a later launch than the one catchUp was
// written for, and that launch's ready wrote one (workflow's claimReady), queued or delivered; or
// root, as recorded now, is at a later generation, and its re-admission's start writes one,
// whether it finds the claim running or relaunches it. Neither holds for a catch-up that may be
// the architect's only one: a claim relaunching or not yet ready has had no later ready, and one
// its later ready will write drops the queued copy.
func catchUpSuperseded(catchUp record.CatchUp, held supervise.Claim, root *record.Issue) bool {
	if claimTookRole(held.State) && held.Generation > catchUp.Launch {
		return true
	}
	return root != nil && root.Generation > catchUp.Generation
}

// copyDueAtOnce says whether the next copy of a notice is due at once, given held, the architect's
// claim as this daemon supervises it (the zero Claim when it does not), resends, the copies of the
// notice already sent, and recipient, the session the report names. A copy is due at once when
// the architect's agent has taken its role and said it is ready (claimTookRole), and it is the
// first copy or its report names a session other than the one the claim is ready on: that ready
// has passed, so no release (releaseWaiting) would make the copy due, and every later notice to
// the architect would wait behind it. The first covers a relaunch that took its role inside the
// receipt window; a later copy that failed on another session went where the role no longer is. A
// later copy whose report names no session (the listener names none for no_holder, or when it
// could not read the holder) says nothing about where the role went. It waits its delay, as does a
// later copy that failed on the claim's own ready session and every copy while the claim has not
// taken its role, so a claim that reads live while its session hears nothing (a reconnecting
// connection, a death the supervisor has not seen) keeps the notice for the whole schedule instead
// of spending its copies in seconds.
func copyDueAtOnce(held supervise.Claim, resends int, recipient string) bool {
	if !claimTookRole(held.State) {
		return false
	}
	return resends == 0 || recipient != "" && recipient != held.Session
}

// reportedNotice is the notice a report is about, as exceptionNotice reads it: its issue, the
// notice as it was published, the outbox row the report's dedupe key names, and the architect whose
// role topic the notice went to.
type reportedNotice struct {
	issue     string
	notice    record.Notice
	row       int64
	architect claim.Token
}

// exceptionNotice reads the notice a report is about: a publish of this daemon's outbox (its
// dedupe key names a row) to the role topic of an architect of this project, whose summary is the
// executor's own "<kind> on <issue>" for an issue of this project and whose payload is that
// notice. ok is false for a report about anything else.
func (r *outbox) exceptionNotice(exception roleLaneException) (reportedNotice, bool) {
	row, ok := record.ParseOutboxKey(exception.DedupeKey)
	if !ok {
		return reportedNotice{}, false
	}
	token, isRole := strings.CutPrefix(exception.OriginalTopic, notify.RoleTopicPrefix)
	if !isRole || !strings.HasPrefix(token, "legion-"+r.project+"-") || !strings.HasSuffix(token, "-"+string(claim.RoleArchitect)) {
		return reportedNotice{}, false
	}
	var notice record.Notice
	decoder := json.NewDecoder(strings.NewReader(exception.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&notice); err != nil {
		return reportedNotice{}, false
	}
	kind, issue, found := parseNoticeSummary(exception.PayloadSummary)
	if !found || kind != notice.Kind || !claim.IsIssueKey(issue) || !strings.HasPrefix(issue, r.dispatchProject+"-") {
		return reportedNotice{}, false
	}
	return reportedNotice{issue: issue, notice: notice, row: row, architect: claim.Token(token)}, true
}

// releaseWaiting makes due at once every notice waiting for a later attempt that one of architects
// now owns, once their claims are ready: each agent took its Envoy role, or took it back, and said
// it can be prompted, so a notice sent now reaches it. That covers a copy waiting out its re-send
// delay, whichever session it failed on (a Go relaunch resumes the claim's session file, so the
// relaunched agent registers the very session id the stopped one had), and a notice held on the
// outbox's backoff while nobody held the role. A copy waiting minutes for a stopped session's
// registration to lapse would otherwise hold back every later notice to that architect behind the
// fence, after the architect is back. Released, the notices go in the order they were written,
// ahead of the ones behind them. The waiting notices and their trees are one repeatable read, each
// tree read once; the fence is not read, since a release only makes a notice due and its send
// still passes the fence.
func (r *outbox) releaseWaiting(ctx context.Context, architects ...claim.Token) error {
	now := r.now()
	runs := func(token claim.Token) bool { return claimRuns(r.claimState(token)) }
	released := []int64{}
	if err := pgx.BeginTxFunc(ctx, r.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		waiting, err := r.records.WaitingNotices(ctx, tx, r.dispatchProject, now)
		if err != nil {
			return err
		}
		routes := map[string]noticeRoute{}
		for _, row := range waiting {
			payload, err := record.DecodeOutboxPayload(row)
			if err != nil {
				return fmt.Errorf("decode waiting notice row %d: %w", row.ID, err)
			}
			notice, ok := payload.(record.Notice)
			if !ok {
				continue
			}
			issue, route, err := r.readNoticeRoute(ctx, tx, row.Issue, routes)
			if errors.Is(err, errNoticeUnroutable) {
				continue
			}
			if err != nil {
				return err
			}
			if owner, err := owningArchitect(route.project, route.issues, issue, notice.Kind, runs); err == nil && slices.Contains(architects, owner) {
				released = append(released, row.ID)
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("read the notices waiting for %v: %w", architects, err)
	}
	if len(released) == 0 {
		return nil
	}
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		for _, id := range released {
			if err := r.records.ExpediteOutbox(ctx, tx, id, now); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("release the notices waiting for %v: %w", architects, err)
	}
	r.log.Info("outbox notices released: their architect is ready", "architects", architects, "rows", released)
	return nil
}
