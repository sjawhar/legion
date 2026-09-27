package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// noticeExceptionSubjects is where the Envoy listener reports a role-lane forward it could not
// complete: notifications.envoy.exceptions.<the original role topic>
// (packages/envoy/cmd/listener/delivery.go, publishDeliveryException). A role topic is outside the
// notification stream, so the report goes over core NATS and reaches only a subscriber connected
// when it is sent; the TypeScript daemon subscribes to the same subjects
// (packages/daemon/src/daemon/events.ts).
const noticeExceptionSubjects = "notifications.envoy.exceptions." + roleTopicPrefix + "*"

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
// waits the nth delay after the report of the one before it. The first catches a quick relaunch,
// the second one heartbeat later, and the last is sent after any registration a stopped session
// left behind must have lapsed, past both windows with a minute to spare. That copy then either
// reaches the relaunched session or is refused (notify.ErrNoHolder) and held by the executor until
// a session holds the role again, so nothing is dropped while the architect's claim lives. There
// are three, as the TypeScript daemon re-sends one at most three times (processes.ts,
// resendToRootArchitect): a holder that keeps its registration alive but never confirms a delivery
// would otherwise be sent copies without end, and the report of its last copy is logged and queues
// nothing.
var noticeReholdDelays = [...]time.Duration{30 * time.Second, pluginHeartbeat, max(listenerSessionTTL, listenerClaimStale) + time.Minute}

// outboxDedupeKey is the dedupe key the outbox publishes every row under (notice), which names
// the row: a report carrying another key is not about a notice of this daemon.
var outboxDedupeKey = regexp.MustCompile(`^legion-outbox:[0-9]+$`)

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
// architect's session failed after the publish was accepted: the forward failed
// (`delivery_failed`), the holder lapsed between the two (`no_holder`), or the session the
// listener forwarded to did not confirm it in time (`receipt_timeout`) and is not the claim's own
// live session. A session that stopped running but is still registered produces that last one:
// the listener forwards to it and waits for a receipt that never comes. So does the relaunch
// window, where the claim's new session has registered with the daemon but its plugin has not yet
// taken the Envoy role back from the stopped one. A late receipt from the claim's own live session
// (registered, ready, working or idle) is a slow holder that has the notice, as the TypeScript
// daemon reads it (processes.ts, handleException), so it is logged and nothing is queued.
//
// The notice goes back through the executor as a new row of its issue, due after the next of
// noticeReholdDelays and counted in its Resends, so it is routed, fenced and held as any notice
// is. It is queued again once per delay; the report of its last copy is logged and queues
// nothing. A report about another daemon's publish, another project, a role that is not an
// architect, or anything but a notice changes nothing. The exception lane has no redelivery, so a
// report that cannot be read is logged here and dropped. One published copy of a row is queued
// again once, keyed by the row's dedupe key: a publish whose 200 was lost is retried under the
// same key, and the listener reports each failed forward under a fresh event id.
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
	issue, notice, ok := r.exceptionNotice(exception)
	if !ok {
		return nil
	}
	switch exception.Reason {
	case "receipt_timeout":
		architect := claim.Token(strings.TrimPrefix(exception.OriginalTopic, roleTopicPrefix))
		if machine, ok := r.supervisor.Machine(architect); ok {
			if c := machine.Claim(); exception.RecipientSession != "" && exception.RecipientSession == c.Session && sessionLive(c.State) {
				r.log.Info("outbox notice receipt was late; its architect's session is live, so nothing is re-sent",
					"issue", issue, "kind", notice.Kind, "topic", exception.OriginalTopic, "key", exception.DedupeKey, "session", c.Session)
				return nil
			}
		}
	case "delivery_failed", "no_holder":
	default:
		return fmt.Errorf("%w: exception %s names reason %q", errNoticeException, envelope.EventID, exception.Reason)
	}
	if notice.Resends >= len(noticeReholdDelays) {
		r.log.Warn("outbox notice not re-held again: it was queued again the most times a notice is",
			"issue", issue, "kind", notice.Kind, "topic", exception.OriginalTopic, "key", exception.DedupeKey, "reason", exception.Reason, "resends", notice.Resends)
		return nil
	}
	delay := noticeReholdDelays[notice.Resends]
	notice.Resends++
	row, err := record.NewOutboxRow(issue, notice, r.now().Add(delay))
	if err != nil {
		return fmt.Errorf("%w: exception %s carries a notice the outbox refuses: %w", errNoticeException, envelope.EventID, err)
	}
	queued := false
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		fresh, err := r.records.MarkProcessed(ctx, tx, "envoy-exception", exception.DedupeKey)
		if err != nil || !fresh {
			return err
		}
		queued = true
		return r.records.Enqueue(ctx, tx, row)
	}); err != nil {
		return fmt.Errorf("re-hold the notice of exception %s: %w", envelope.EventID, err)
	}
	if queued {
		r.log.Info("outbox notice re-held: the listener could not forward it to its architect's session",
			"issue", issue, "kind", notice.Kind, "topic", exception.OriginalTopic, "key", exception.DedupeKey, "reason", exception.Reason)
	}
	return nil
}

// exceptionNotice reads the notice a report is about: a publish of this daemon's outbox (its
// dedupe key names a row) to the role topic of an architect of this project, whose summary is the
// executor's own "<kind> on <issue>" for an issue of this project and whose payload is that
// notice. ok is false for a report about anything else.
func (r *outbox) exceptionNotice(exception roleLaneException) (string, record.Notice, bool) {
	if !outboxDedupeKey.MatchString(exception.DedupeKey) {
		return "", record.Notice{}, false
	}
	token, isRole := strings.CutPrefix(exception.OriginalTopic, roleTopicPrefix)
	if !isRole || !strings.HasPrefix(token, "legion-"+r.project+"-") || !strings.HasSuffix(token, "-"+string(claim.RoleArchitect)) {
		return "", record.Notice{}, false
	}
	var notice record.Notice
	decoder := json.NewDecoder(strings.NewReader(exception.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&notice); err != nil {
		return "", record.Notice{}, false
	}
	kind, issue, found := strings.Cut(exception.PayloadSummary, " on ")
	if !found || record.NoticeKind(kind) != notice.Kind || !claim.IsIssueKey(issue) || !strings.HasPrefix(issue, r.dispatchProject+"-") {
		return "", record.Notice{}, false
	}
	return issue, notice, true
}

// sessionLive is whether a claim in state has a session that is up and registered: it can take a
// delivery, even if it is slow to confirm one.
func sessionLive(state supervise.ClaimState) bool {
	switch state {
	case supervise.StateRegistered, supervise.StateReady, supervise.StateWorking, supervise.StateIdle:
		return true
	default:
		return false
	}
}

// releaseWaiting makes due at once every notice waiting for a later attempt that architect now
// owns, once architect's claim is ready: its agent took its Envoy role, or took it back, and said
// it can be prompted, so a notice sent now reaches it. That covers a copy waiting out its re-send
// delay, whichever session it failed on (a Go relaunch resumes the claim's session file, so the
// relaunched agent registers the very session id the stopped one had), and a notice held on the
// outbox's backoff while nobody held the role. A copy waiting minutes for a stopped session's
// registration to lapse would otherwise hold back every later notice to that architect behind the
// fence, after the architect is back. Released, the notices go in the order they were written,
// ahead of the ones behind them.
func (r *outbox) releaseWaiting(ctx context.Context, architect claim.Token) error {
	now := r.now()
	var waiting []record.OutboxRow
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		waiting, err = r.records.WaitingNotices(ctx, tx, r.dispatchProject, now)
		return err
	}); err != nil {
		return fmt.Errorf("release the notices waiting for %s: %w", architect, err)
	}
	runs := func(token claim.Token) bool { return claimRuns(r.claimState(token)) }
	released := []int64{}
	for _, row := range waiting {
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			return fmt.Errorf("decode waiting notice row %d: %w", row.ID, err)
		}
		notice, ok := payload.(record.Notice)
		if !ok {
			continue
		}
		issue, tree, err := r.readNoticeTree(ctx, row)
		if errors.Is(err, errNoticeUnroutable) {
			continue
		}
		if err != nil {
			return err
		}
		if owner, err := owningArchitect(tree.project, tree.issues, issue, notice.Kind, runs); err != nil || owner != architect {
			continue
		}
		released = append(released, row.ID)
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
		return fmt.Errorf("release the notices waiting for %s: %w", architect, err)
	}
	r.log.Info("outbox notices released: their architect is ready", "architect", architect, "rows", released)
	return nil
}
