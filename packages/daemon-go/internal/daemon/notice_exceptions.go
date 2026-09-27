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

// noticeReholdDelay spaces the copies of a notice whose forward failed. The listener takes a
// registered holder as live for its session's TTL and accepts a publish for it, so while a session
// that stopped running stays registered, each copy is accepted and then reported undelivered in
// turn. Once the registration lapses, the listener refuses the publish itself (notify.ErrNoHolder),
// and the executor holds the row until a session holds the role again.
const noticeReholdDelay = 30 * time.Second

// outboxDedupeKey is the dedupe key the outbox publishes every row under (notice), which names
// the row: a report carrying another key is not about a notice of this daemon.
var outboxDedupeKey = regexp.MustCompile(`^legion-outbox:[0-9]+$`)

// errNoticeException is a report the listener sent that cannot be read as the failed forward of
// a notice.
var errNoticeException = errors.New("malformed role-lane exception")

// roleLaneException is the listener's report of one failed role-lane forward, the payload of its
// exception envelope: the original envelope's topic, summary, payload and dedupe key, and why the
// forward failed.
type roleLaneException struct {
	OriginalTopic  string `json:"original_topic"`
	EventID        string `json:"event_id"`
	Reason         string `json:"reason"`
	PayloadSummary string `json:"payload_summary"`
	Payload        string `json:"payload"`
	DedupeKey      string `json:"dedupe_key"`
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
// (`delivery_failed`), the holder lapsed between the two (`no_holder`), or the holder did not
// confirm it in time (`receipt_timeout`) while the architect's claim has no live session. That
// last is what a session that stopped running but is still registered produces: the listener
// forwards to it and waits for a receipt that never comes. A late receipt while the claim's
// session is live (registered, ready, working or idle) is a slow holder that has the notice, as
// the TypeScript daemon reads it (processes.ts, handleException), so it is logged and nothing is
// queued. The notice goes back through the executor as a new row of its issue, due after
// noticeReholdDelay, so it is routed, fenced and held as any notice is. A report about another
// daemon's publish, another project, a role that is not an architect, or anything but a notice
// changes nothing. The exception lane has no redelivery, so a report that cannot be read is
// logged here and dropped. Each report is queued at most once, keyed by its envelope's event id.
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
		if sessionLive(r.claimState(architect)) {
			r.log.Info("outbox notice receipt was late; its architect's session is live, so nothing is re-sent",
				"issue", issue, "kind", notice.Kind, "topic", exception.OriginalTopic, "key", exception.DedupeKey)
			return nil
		}
	case "delivery_failed", "no_holder":
	default:
		return fmt.Errorf("%w: exception %s names reason %q", errNoticeException, envelope.EventID, exception.Reason)
	}
	row, err := record.NewOutboxRow(issue, notice, r.now().Add(noticeReholdDelay))
	if err != nil {
		return fmt.Errorf("%w: exception %s carries a notice the outbox refuses: %w", errNoticeException, envelope.EventID, err)
	}
	queued := false
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		fresh, err := r.records.MarkProcessed(ctx, tx, "envoy-exception", envelope.EventID)
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
// executor's own "<kind> on <issue>" and whose payload is that notice. ok is false for a report
// about anything else.
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
	if !found || record.NoticeKind(kind) != notice.Kind || !claim.IsIssueKey(issue) {
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
