package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PendingScope selects the pending requests EndPendingRequests ends, by comparing one column with
// its argument.
type PendingScope int

const (
	// RequestByID is the one request whose id is the argument.
	RequestByID PendingScope = iota
	// RequestsOfEnrollment is every request of the enrollment whose id is the argument.
	RequestsOfEnrollment
	// RequestsPastDeadline is every request whose pending_expires_at is before the argument, a
	// time.Time.
	RequestsPastDeadline
)

var pendingScopeWhere = [...]string{
	RequestByID:          "id = $1",
	RequestsOfEnrollment: "enrollment_id = $1",
	RequestsPastDeadline: "pending_expires_at < $1",
}

// RequestEnd is how EndPendingRequests ends each request it selects.
type RequestEnd struct {
	// State is "cancelled" or "expired": the request's new state, its record's event, and its
	// audit row's kind, request.<State>.
	State string
	// Actor names who ended the request, on its audit row and its record's event.
	Actor string
	// DecidedBy is the request's decided_by; nil leaves it null.
	DecidedBy *string
	// DecidedAt is the request's decided_at; nil is Postgres's now().
	DecidedAt *time.Time
	// Detail is the request's decision_detail and its record event's detail.
	Detail string
	// AuditDetail is the audit row's detail; nil leaves the column's default, {}.
	AuditDetail map[string]any
}

// EndedRequest is one request EndPendingRequests ended.
type EndedRequest struct {
	ID           string
	EnrollmentID string
	RecordID     *string
}

// endPendingRequests is one statement: the request rows, their audit rows, and their records'
// events are written together or not at all.
const endPendingRequests = `with ended as (
	update requests set state=$2::text, decided_at=coalesce($3::timestamptz, now()), decided_by=$4::text, decision_detail=$5::text
	where state='pending' and %s
	returning id, enrollment_id, record_id
), audited as (
	insert into audit (kind, enrollment_id, request_id, actor, detail)
	select 'request.' || $2::text, enrollment_id, id, $6::text, coalesce($7::jsonb, '{}'::jsonb) from ended
), recorded as (
	insert into credential_request_events (record_id, event, actor, detail)
	select record_id, $2::text, $6::text, $5::text from ended where record_id is not null
)
select id, enrollment_id, record_id from ended`

// EndPendingRequests moves the requests scope selects that are still pending to end.State inside
// tx, writing in the same statement each one's audit row and, for a request with a
// credential-request record, the record's terminal event. A request no longer pending is left as
// it is. It is how every writer but a human's decision (requests.Machine.ApplyDecision, which
// writes its own event) ends a pending request: the requesting session's cancel, the sweeper's
// expiry, and an enrollment's end. Each writes the record's event with the request's transition,
// but no reader relies on that event being there, since a database can hold requests an older
// broker cancelled without one: requests.Machine.PendingForApprover and ReadRecord take an
// agent_secret record's state from its request row, and record.ChainVerifier releases only on
// the approved event ApplyDecision writes.
func EndPendingRequests(ctx context.Context, tx pgx.Tx, scope PendingScope, arg any, end RequestEnd) ([]EndedRequest, error) {
	var auditDetail any
	if end.AuditDetail != nil {
		auditDetail = end.AuditDetail
	}
	rows, err := tx.Query(ctx, fmt.Sprintf(endPendingRequests, pendingScopeWhere[scope]),
		arg, end.State, end.DecidedAt, end.DecidedBy, end.Detail, end.Actor, auditDetail)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[EndedRequest])
}
