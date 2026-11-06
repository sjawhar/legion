package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// GrantLogSession is the session an audit row was written under, read from the row's own
// enrollment: what a quarterly access review needs to say which agent session did the thing,
// rather than only which enrollment row it hung off. Every field is empty for a row that names
// no enrollment.
type GrantLogSession struct {
	// Kind is the enrollment's kind, session or pod.
	Kind string `json:"kind,omitzero"`
	// SessionID is the Envoy session the enrollment recorded, empty for an enrollment that names
	// none (a pod enrolled before its session had a name, say).
	SessionID string `json:"session_id,omitzero"`
	// RuntimeID is what the enrollment proved it runs on: the box's id for a session, the pod's
	// UID for a pod.
	RuntimeID string `json:"runtime_id,omitzero"`
}

// GrantLogSecret is one secret of a request and how that request's policy evaluation decided it
// (automatic, approval or deny). The review reads which names a session asked for and which of
// them a person had to answer for.
type GrantLogSecret struct {
	// Name is the secret's name as the session asked for it.
	Name string `json:"name"`
	// Decision is the request_secrets row's decision: automatic, approval or deny.
	Decision string `json:"decision"`
}

// GrantLogEvent is one row of the broker's audit table, joined to everything a quarterly access
// review has to read beside it: who the session was, who the broker verified it to be, what it
// asked for and who answered. The grant-log route's response is this shape — each event is this
// struct as its JSON tags name it, so the route adds no second vocabulary — and
// LogGrantEvent writes the same fields as one INFO line at the moment the broker decides.
//
// A field the row does not have is empty or zero: a row with no enrollment carries no person and
// no session, a row with no request carries no state, approver or secrets. Raw carries the row's
// own detail object whatever its kind, so an audit kind this type says nothing about is still
// readable by whoever reads the review.
//
// No field here holds a secret value or a token: the audit table holds none, and every string
// below is a name, a login, an id or a decision.
type GrantLogEvent struct {
	// ID is the audit row's own id, which is also the paging cursor: GrantLogQuery.After.
	ID int64 `json:"id,omitzero"`
	// At is when the row was written.
	At time.Time `json:"at,omitzero"`
	// Kind is the audit row's kind, request.created and grant.revoked among them.
	Kind string `json:"kind"`
	// Actor is who the broker recorded as having done it: session:<enrollment id>,
	// human:<login>, launcher:<credential id>, or the broker itself.
	Actor string `json:"actor"`
	// EnrollmentID is the enrollment the row names, empty for a row that names none (every
	// launcher_credential.* kind).
	EnrollmentID string `json:"enrollment_id,omitzero"`
	// RequestID is the request the row names, empty for a row that names none (a grant.moved
	// row, and every enrollment and launcher-credential kind).
	RequestID string `json:"request_id,omitzero"`
	// GrantID is the grant the row names, empty for a row that names none.
	GrantID string `json:"grant_id,omitzero"`
	// Session is the enrollment's own identity: its kind, its session id and its runtime id.
	Session GrantLogSession `json:"session,omitzero"`
	// Person is the operator the enrollment acts for (enrollments.operator), empty for a pod and
	// for a machine's own session.
	Person string `json:"person,omitzero"`
	// Service is the service the broker VERIFIED for the request, read from the request's own
	// request.created audit row: the service account the enrollment proved still named this
	// service when the request was decided. It is what the policy decided under.
	Service string `json:"service,omitzero"`
	// ClaimedService is the service the enrollment's launcher credential claims
	// (launcher_credentials.service), which the policy does NOT trust: a credential may name a
	// service whose account no longer proves it, and then Service is empty while this is not.
	// The review reads the pair to see a claim the broker refused.
	ClaimedService string `json:"claimed_service,omitzero"`
	// Secrets are the request's secrets and each one's decision, in name order; empty for a row
	// that names no request.
	Secrets []GrantLogSecret `json:"secrets,omitzero"`
	// State is the request's state now: pending, granted, denied, cancelled or expired.
	State string `json:"state,omitzero"`
	// Approver is the login the request's record allows to decide it (requests.allowed_approver),
	// or record.AnyoneApprover for a shared secret; empty when the policy decided the request
	// itself.
	Approver string `json:"approver,omitzero"`
	// DecidedBy is who the request records as having decided it: the approving login, the broker
	// for an expiry, requests.PolicyDeniedActor for a denial the policy made under an approval.
	DecidedBy string `json:"decided_by,omitzero"`
	// LifetimeSeconds is how long the request's grant lasts; zero when the request records none,
	// which is a grant that lasts until its session ends.
	LifetimeSeconds int `json:"lifetime_seconds,omitzero"`
	// Reason is the reason the session signed into its request object, the sentence a person
	// reads on the approval page.
	Reason string `json:"reason,omitzero"`
	// Detail is the request's decision_detail: why it was denied, or why an approval granted
	// nothing.
	Detail string `json:"detail,omitzero"`
	// Raw is the audit row's own detail object, verbatim, so a kind this type says nothing about
	// is still readable.
	Raw json.RawMessage `json:"raw,omitzero"`
}

// GrantLogQuery is one page of the grant log: the closed period to read, where to resume and how
// many rows to answer.
type GrantLogQuery struct {
	// Since and Until bound the period, half-open: at >= Since and at < Until. A review reads a
	// closed quarter, so the period's end is never now.
	Since, Until time.Time
	// After resumes after the audit row of that id; zero starts at the period's first row.
	After int64
	// Limit bounds the page and is required: it must be positive, and ReadGrantLog refuses
	// anything else (ErrGrantLogLimit) rather than reading a whole quarter into memory for a
	// caller that forgot to say how much of it it wanted.
	Limit int
}

// ErrGrantLogLimit is ReadGrantLog's refusal of a GrantLogQuery whose Limit is zero or less: the
// page's size is required, so that no caller reads a whole period's audit rows by leaving it
// out.
var ErrGrantLogLimit = errors.New("store: the grant log is read a page at a time: Limit must be positive")

// readGrantLog is the one SQL read behind the grant log. It is ordered by id and filtered on at
// because the audit table's only index besides its primary key is audit_request (request_id):
// there is no index on at, and this change may not add one. Ordered by id with `id > after`
// paging, Postgres reads the primary-key index forward from the cursor and stops at the page's
// limit, so reading a closed period costs one forward pass over the table in total rather than
// one per page: only the first page walks the rows older than Since, and only the last page
// walks past Until to the end. Ordering by at instead would sort the whole period for every
// page, and paging on at would still have to find the period's start each time. Audit ids are
// handed out by a bigserial, so id order is write order and the review reads the events in the
// order they happened.
//
// Every join is a left join of the row's own keys: the row's enrollment for the person and the
// session, that enrollment's launcher credential for the claimed service, the row's request for
// its state and decision, that request's secrets, and that request's own request.created audit
// row for the service the broker verified when it decided. A row that names no enrollment (every
// launcher_credential.* kind) keeps its own detail in raw and nothing else.
const readGrantLog = `select a.id, a.at, a.kind, a.actor,
		coalesce(a.enrollment_id::text, ''), coalesce(a.request_id::text, ''), coalesce(a.grant_id::text, ''),
		coalesce(e.kind, ''), coalesce(e.session_id, ''), coalesce(e.runtime_id, ''),
		coalesce(e.operator, ''), coalesce(c.service, ''), coalesce(created.detail->>'service', ''),
		coalesce(r.state, ''), coalesce(r.allowed_approver, ''), coalesce(r.decided_by, ''),
		coalesce(r.lifetime_seconds, 0), coalesce(r.reason, ''), coalesce(r.decision_detail, ''),
		s.secrets, a.detail
	from audit a
	left join enrollments e on e.id = a.enrollment_id
	left join launcher_credentials c on c.id = e.launcher_credential_id
	left join requests r on r.id = a.request_id
	left join lateral (
		select jsonb_agg(jsonb_build_object('name', rs.name, 'decision', rs.decision) order by rs.name) as secrets
		from request_secrets rs where rs.request_id = a.request_id
	) s on true
	left join lateral (
		select ca.detail from audit ca where ca.request_id = a.request_id and ca.kind = 'request.created' order by ca.id limit 1
	) created on true
	where a.at >= $1 and a.at < $2 and a.id > $3
	order by a.id
	limit $4`

// ReadGrantLog answers the audit rows of q's period, oldest first, as the access review reads
// them: every grant and every refusal the broker recorded, each naming its session, that
// session's person, the service the broker verified for it, the secrets it asked for, the
// decision and who decided. It is the one read behind the grant-log route; readGrantLog says why
// it is ordered by id and filtered on at.
//
// A page holds at most q.Limit rows; the caller reads the next page with After set to the last
// row's ID, and has read the period whole when a page comes back short. A limit of zero or less
// is refused (ErrGrantLogLimit): a quarter's audit rows are more than one answer should hold, so
// the size of the page is the caller's to state and not this read's to guess.
func ReadGrantLog(ctx context.Context, pool *pgxpool.Pool, q GrantLogQuery) ([]GrantLogEvent, error) {
	if q.Limit <= 0 {
		return nil, fmt.Errorf("%w: %d", ErrGrantLogLimit, q.Limit)
	}
	rows, err := pool.Query(ctx, readGrantLog, q.Since, q.Until, q.After, q.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// The page is as big as the limit allows, so a full page is read into one allocation.
	events := make([]GrantLogEvent, 0, q.Limit)
	for rows.Next() {
		var ev GrantLogEvent
		var secrets, raw []byte
		if err := rows.Scan(&ev.ID, &ev.At, &ev.Kind, &ev.Actor,
			&ev.EnrollmentID, &ev.RequestID, &ev.GrantID,
			&ev.Session.Kind, &ev.Session.SessionID, &ev.Session.RuntimeID,
			&ev.Person, &ev.ClaimedService, &ev.Service,
			&ev.State, &ev.Approver, &ev.DecidedBy, &ev.LifetimeSeconds, &ev.Reason, &ev.Detail,
			&secrets, &raw); err != nil {
			return nil, err
		}
		if len(secrets) > 0 {
			if err := json.Unmarshal(secrets, &ev.Secrets); err != nil {
				return nil, err
			}
		}
		ev.Raw = json.RawMessage(raw)
		events = append(events, ev)
	}
	return events, rows.Err()
}

// GrantLogMessage opens the one line the broker logs, at INFO, for each request decision and each
// grant end (LogGrantEvent). The deployment's log pipeline collects it and keeps it, so that the
// decisions of a period can be read without the database, and so it does not change — as
// policy.RefusedMessage and requests.MintFailedMessage do not, for the alarms that filter on
// them.
const GrantLogMessage = "broker: grant log"

// LogGrantEvent writes ev as the broker's one grant-log line: the event's own fields, each as its
// own attribute of its own, and a field the event does not hold left out, so the line names what
// the writer had in hand and claims nothing else. It is the only writer of that line, as
// ReadGrantLog is the only reader of the rows behind it, which is what lets one test pin the two
// constructions against each other.
//
// The line holds no secret value and no token: a GrantLogEvent holds none, because every field it
// carries is a name, a login, an id, a state or a decision, and Raw is an audit row's detail,
// which the broker writes with no value in it.
//
// A line is lost if the process dies between the transaction's commit and this call. That is the
// right way round: the audit row is the record the review reads, and the line is the copy the log
// pipeline keeps.
func LogGrantEvent(ev GrantLogEvent) {
	attrs := make([]any, 0, 19)
	add := func(key string, value any) { attrs = append(attrs, slog.Any(key, value)) }
	text := func(key, value string) {
		if value != "" {
			attrs = append(attrs, slog.String(key, value))
		}
	}
	if ev.ID != 0 {
		add("id", ev.ID)
	}
	if !ev.At.IsZero() {
		add("at", ev.At)
	}
	text("kind", ev.Kind)
	text("actor", ev.Actor)
	text("enrollment_id", ev.EnrollmentID)
	text("request_id", ev.RequestID)
	text("grant_id", ev.GrantID)
	if ev.Session != (GrantLogSession{}) {
		add("session", ev.Session)
	}
	text("person", ev.Person)
	text("service", ev.Service)
	text("claimed_service", ev.ClaimedService)
	if len(ev.Secrets) > 0 {
		add("secrets", ev.Secrets)
	}
	text("state", ev.State)
	text("approver", ev.Approver)
	text("decided_by", ev.DecidedBy)
	if ev.LifetimeSeconds != 0 {
		add("lifetime_seconds", ev.LifetimeSeconds)
	}
	text("reason", ev.Reason)
	text("detail", ev.Detail)
	text("raw", string(ev.Raw))
	slog.Info(GrantLogMessage, attrs...)
}
