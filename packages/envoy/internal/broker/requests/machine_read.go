// packages/envoy/internal/broker/requests/machine_read.go
// machine_read.go holds requests.Machine's read-model methods: queries the API's GET handlers
// and the value-release path use, as opposed to machine_state.go's write/decision/transition
// methods.
package requests

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/rules"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// Values releases the inject-mode values of a live grant to its own enrollment. Every call
// re-checks the enrollment and the grant, re-verifies the grant's whole approval chain
// (VerifyChain), and — when the rules have changed since the grant's request was decided — that
// the current rules still allow this requester every granted name (stillAllowed). A name is
// released only when both the delivery frozen at grant time and the current delivery are inject: a
// grant approved for proxy delivery never widens into a raw value. It holds no pooled connection
// across a Secrets Manager read: the grant's names are read into memory before the first value is
// fetched.
func (m *Machine) Values(ctx context.Context, grantID, enrollmentID string) (map[string]string, []string, time.Time, error) {
	var owner, requestID, rulesVersion string
	var expires time.Time
	var live bool
	var enr enrollmentRow
	err := m.Store.Pool.QueryRow(ctx, `select g.enrollment_id, g.expires_at, g.revoked_at is null and g.expires_at > now() and e.revoked_at is null and e.lease_expires_at > now(),
		g.request_id, r.rules_version, e.kind, e.operator, e.subject
		from grants g join enrollments e on e.id=g.enrollment_id join requests r on r.id=g.request_id where g.id=$1`, grantID).
		Scan(&owner, &expires, &live, &requestID, &rulesVersion, &enr.Kind, &enr.Operator, &enr.Subject)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, time.Time{}, ErrGrantNotLive
	}
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	if owner != enrollmentID {
		return nil, nil, time.Time{}, ErrNotYours
	}
	if !live {
		return nil, nil, time.Time{}, ErrGrantNotLive
	}
	if err := m.VerifyChain(ctx, grantID); err != nil {
		return nil, nil, time.Time{}, err
	}
	granted, err := m.grantedSecrets(ctx, requestID)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	set := m.Rules.Get()
	if rulesVersion != set.Version {
		for _, g := range granted {
			if err := stillAllowed(set, g.name, g.decision, enr.requester()); err != nil {
				return nil, nil, time.Time{}, err
			}
		}
	}
	values := map[string]string{}
	var proxyOnly []string
	released := []string{}
	for _, g := range granted {
		current, ok := set.Secrets[g.name]
		if !ok {
			return nil, nil, time.Time{}, fmt.Errorf("%w: %s is no longer in the rules", ErrGrantNotLive, g.name)
		}
		if g.delivery == "proxy" || current.Delivery == "proxy" {
			proxyOnly = append(proxyOnly, g.name)
			continue
		}
		value, err := m.Secrets.Read(ctx, g.source)
		if errors.Is(err, secrets.ErrNotFound) {
			return nil, nil, time.Time{}, fmt.Errorf("%w: %s", ErrSecretNotInStore, g.name)
		}
		if err != nil {
			return nil, nil, time.Time{}, fmt.Errorf("read %s: %w", g.name, err)
		}
		values[g.name] = value
		released = append(released, g.name)
	}
	if _, err := m.Store.Pool.Exec(ctx, `insert into audit (kind, enrollment_id, request_id, grant_id, actor, detail) values ('grant.used',$1,$2,$3,$4,$5)`,
		enrollmentID, requestID, grantID, "session:"+enrollmentID, auditDetail{"names": released}); err != nil {
		return nil, nil, time.Time{}, err
	}
	return values, proxyOnly, expires, nil
}

// grantedSecret is one name a request did not deny, as frozen when the request was decided.
type grantedSecret struct{ name, source, decision, delivery string }

func (m *Machine) grantedSecrets(ctx context.Context, requestID string) ([]grantedSecret, error) {
	rows, err := m.Store.Pool.Query(ctx, `select name, source, decision, delivery from request_secrets where request_id=$1 and decision <> 'deny' order by name`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var granted []grantedSecret
	for rows.Next() {
		var g grantedSecret
		if err := rows.Scan(&g.name, &g.source, &g.decision, &g.delivery); err != nil {
			return nil, err
		}
		granted = append(granted, g)
	}
	return granted, rows.Err()
}

// stillAllowed re-checks one granted name against rules newer than the ones its request was
// decided under. The name must still exist and the current rules must still let this requester
// have it: a deny, or no entry naming the requester at all, refuses; so does a name granted
// automatically that the current rules want approved, since no human ever approved it. A name a
// human approved stays allowed while the rules still want an approval, whoever the approver now
// is.
func stillAllowed(set *rules.Set, name, frozenDecision string, requester rules.Requester) error {
	d, err := set.Evaluate(name, requester)
	if errors.Is(err, rules.ErrUnknownSecret) {
		return fmt.Errorf("%w: %s is no longer in the rules", ErrGrantNotLive, name)
	}
	if err != nil {
		return err
	}
	switch {
	case d.Outcome == "deny":
		return fmt.Errorf("%w: the current rules no longer allow %s", ErrGrantNotLive, name)
	case frozenDecision == "automatic" && d.Outcome == "approval":
		return fmt.Errorf("%w: the current rules require approval for %s", ErrGrantNotLive, name)
	}
	return nil
}

func (m *Machine) Get(ctx context.Context, id string) (Request, error) {
	var r Request
	var enrollmentID string
	err := m.Store.Pool.QueryRow(ctx, `select r.id, r.state, r.enrollment_id, r.decided_at, r.decided_by, r.decision_detail, r.record_id,
		(select g.id::text from grants g where g.request_id=r.id and g.revoked_at is null limit 1)
		from requests r where r.id=$1`, id).Scan(&r.ID, &r.State, &enrollmentID, &r.DecidedAt, &r.DecidedBy, &r.Detail, &r.RecordID, &r.GrantID)
	if err != nil {
		return Request{}, err
	}
	rows, err := m.Store.Pool.Query(ctx, `select name, decision, delivery from request_secrets where request_id=$1 order by name`, id)
	if err != nil {
		return Request{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var d SecretDecision
		if err := rows.Scan(&d.Name, &d.Decision, &d.Delivery); err != nil {
			return Request{}, err
		}
		r.Secrets = append(r.Secrets, d)
	}
	return r, rows.Err()
}

func (m *Machine) OwnerOf(ctx context.Context, requestID string) (string, error) {
	var owner string
	err := m.Store.Pool.QueryRow(ctx, `select enrollment_id from requests where id=$1`, requestID).Scan(&owner)
	return owner, err
}

// SessionID answers a request row's own optional session_id — an override supplied in the
// request body at Create time, distinct from the enrollment's own session_id — for
// wake.Envoy's notification target. main.go's waker tries this first and discards its error, so
// a null session_id or a request id that no longer exists both answer ("", nil); only a genuine
// Postgres failure is a non-nil error.
func (m *Machine) SessionID(ctx context.Context, id string) (string, error) {
	var sessionID *string
	err := m.Store.Pool.QueryRow(ctx, `select session_id from requests where id=$1`, id).Scan(&sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if sessionID == nil {
		return "", nil
	}
	return *sessionID, nil
}

// Grant is a live grant's public summary, for GET /v1/enrollments/self. It follows the same query
// pattern as OwnerOf and enrollment above.
type Grant struct {
	ID        string    `json:"grant_id"`
	RequestID string    `json:"request_id"`
	Approver  *string   `json:"approver"`
	ExpiresAt time.Time `json:"expires_at"`
}

// LiveGrants lists this enrollment's own live (not revoked, not expired) grants.
func (m *Machine) LiveGrants(ctx context.Context, enrollmentID string) ([]Grant, error) {
	rows, err := m.Store.Pool.Query(ctx, `select id, request_id, approver, expires_at from grants
		where enrollment_id=$1 and revoked_at is null and expires_at > now() order by expires_at`, enrollmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var grants []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.ID, &g.RequestID, &g.Approver, &g.ExpiresAt); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	return grants, rows.Err()
}

// RecordKind reads a credential-request record's own kind ("agent_secret" or
// "launcher_credential") — the one column api.decideRecord needs before it knows which service's
// ApplyDecision a record id belongs to, and api.readRecord needs to decide what an already-read
// detail means. pgx.ErrNoRows means no such record.
func (m *Machine) RecordKind(ctx context.Context, recordID string) (string, error) {
	var kind string
	err := m.Store.Pool.QueryRow(ctx, `select kind from credential_requests where id=$1`, recordID).Scan(&kind)
	return kind, err
}

// PendingSummary is one still-undecided credential-request record for GET /v1/pending: its id,
// kind, the plain identifiers its request object names (secret names for agent_secret, the single
// host for launcher_credential), and when it was requested.
type PendingSummary struct {
	RecordID    string
	Kind        string
	Identifiers []string
	RequestedAt time.Time
}

// PendingForApprover lists every still-pending credential-request record — of either kind — that
// names approver, newest first: GET /v1/pending's exact contract. An agent_secret record is
// pending while its request is: every writer of a terminal event moves the request out of
// 'pending' in the same transaction, and a request an ended enrollment cancelled before
// endEnrollment wrote that event carries none, so the request row is the truth. A machine login has
// no request row and is pending while it carries no terminal decision event, matching
// credential_request_decision's own partial index.
func (m *Machine) PendingForApprover(ctx context.Context, approver string) ([]PendingSummary, error) {
	rows, err := m.Store.Pool.Query(ctx, `select cr.id, cr.kind, cr.body, cr.created_at from credential_requests cr
		where cr.approver=$1 and (
			(cr.kind='agent_secret' and cr.id in (select r.record_id from requests r where r.state='pending'))
			or (cr.kind='launcher_credential' and not exists (
				select 1 from credential_request_events ev where ev.record_id=cr.id and ev.event = any($2)))
		) order by cr.created_at desc`, record.CanonicalLogin(approver), record.TerminalEventNames())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingSummary
	for rows.Next() {
		var id, kind, canonical string
		var createdAt time.Time
		if err := rows.Scan(&id, &kind, &canonical, &createdAt); err != nil {
			return nil, err
		}
		body, err := record.ParseBody(canonical)
		if err != nil {
			return nil, err
		}
		obj, err := record.VerifyRequestObject(body.Request, m.Audience, m.Skew, createdAt)
		if err != nil {
			return nil, err
		}
		identifiers := make([]string, len(obj.Details))
		for i, d := range obj.Details {
			identifiers[i] = d.Identifier
		}
		out = append(out, PendingSummary{RecordID: id, Kind: kind, Identifiers: identifiers, RequestedAt: createdAt})
	}
	return out, rows.Err()
}

// RecordDecision is a decided credential-request record's own terminal event, for
// GET /v1/credential-requests/{id}'s "decided" field.
type RecordDecision struct {
	Event        string
	At           time.Time
	CredentialID string
}

// RecordDetail is a full credential-request record for GET /v1/credential-requests/{id}: its
// decoded request object plus its current state, folding in a later grant revocation the record's
// own decision events never overwrite (State becomes "revoked" while Decided keeps the original
// approval). Enrollment is nil for a machine login (Kind == "launcher_credential"), which carries
// no requesting enrollment at all.
type RecordDetail struct {
	RecordID        string
	Kind            string
	State           string
	Approver        string
	Enrollment      *record.Enrollment
	Identifiers     []string
	Service         string
	Reason          string
	LifetimeSeconds int
	RulesVersion    string
	ExpiresAt       time.Time
	RequestedAt     time.Time
	Decided         *RecordDecision
}

// ReadRecord reads a credential-request record's full detail by id, for GET
// /v1/credential-requests/{id} and, reused verbatim, POST /v1/machine-logins/lookup.
// pgx.ErrNoRows means no such record.
func (m *Machine) ReadRecord(ctx context.Context, recordID string) (RecordDetail, error) {
	var canonical, approver, kind string
	var createdAt, expiresAt time.Time
	if err := m.Store.Pool.QueryRow(ctx, `select body, approver, kind, created_at, expires_at from credential_requests where id=$1`, recordID).
		Scan(&canonical, &approver, &kind, &createdAt, &expiresAt); err != nil {
		return RecordDetail{}, err
	}
	body, err := record.ParseBody(canonical)
	if err != nil {
		return RecordDetail{}, err
	}
	obj, err := record.VerifyRequestObject(body.Request, m.Audience, m.Skew, createdAt)
	if err != nil {
		return RecordDetail{}, err
	}
	var enr *record.Enrollment
	if kind != "launcher_credential" {
		e := body.Enrollment
		enr = &e
	}
	identifiers := make([]string, len(obj.Details))
	service := ""
	for i, d := range obj.Details {
		identifiers[i] = d.Identifier
		if d.Service != "" {
			service = d.Service
		}
	}
	detail := RecordDetail{
		RecordID: recordID, Kind: kind, State: "pending", Approver: approver, Enrollment: enr,
		Identifiers: identifiers, Service: service, Reason: obj.Reason, LifetimeSeconds: body.LifetimeSeconds,
		RulesVersion: body.RulesVersion, ExpiresAt: expiresAt, RequestedAt: createdAt,
	}
	var event, credID string
	var decidedAt time.Time
	err = m.Store.Pool.QueryRow(ctx, `select event, at, coalesce(credential_id,'') from credential_request_events
		where record_id=$1 and event = any($2)`, recordID, record.TerminalEventNames()).Scan(&event, &decidedAt, &credID)
	if errors.Is(err, pgx.ErrNoRows) && kind == "agent_secret" {
		// A request an ended enrollment cancelled before endEnrollment wrote the cancelled event
		// left its record with no terminal event; its request row says it was cancelled, and when.
		event = "cancelled"
		err = m.Store.Pool.QueryRow(ctx, `select decided_at from requests where record_id=$1 and state='cancelled'`, recordID).Scan(&decidedAt)
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return detail, nil
	case err != nil:
		return RecordDetail{}, err
	}
	detail.State = event
	detail.Decided = &RecordDecision{Event: event, At: decidedAt, CredentialID: credID}
	var revoked int
	err = m.Store.Pool.QueryRow(ctx, `select 1 from credential_request_events where record_id=$1 and event='revoked' limit 1`, recordID).Scan(&revoked)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return RecordDetail{}, err
	default:
		detail.State = "revoked"
	}
	return detail, nil
}

// ApproverGrant is one live, approval-granted grant an approver (or its enrollment's operator) may
// revoke, for GET /v1/grants?approver=<login>. Approver is the login that approved it, which for an
// operator's own list can be another login.
type ApproverGrant struct {
	GrantID    string
	RecordID   *string
	Enrollment record.Enrollment
	Names      []string
	Approver   string
	ExpiresAt  time.Time
	CreatedAt  time.Time
}

// GrantsForApprover lists every live grant approver (or its enrollment's operator) may revoke:
// only grants an approval actually decided — an automatic grant carries no approver and never
// appears here — newest first.
func (m *Machine) GrantsForApprover(ctx context.Context, approver string) ([]ApproverGrant, error) {
	login := record.CanonicalLogin(approver)
	rows, err := m.Store.Pool.Query(ctx, `select g.id, r.record_id, e.kind, e.runtime_id, coalesce(e.operator,''), e.slot, g.approver, g.expires_at, g.created_at,
		coalesce((select array_agg(rs.name order by rs.name) from request_secrets rs where rs.request_id=r.id and rs.decision<>'deny'), '{}')
		from grants g
		join requests r on r.id=g.request_id
		join enrollments e on e.id=g.enrollment_id
		where g.revoked_at is null and g.expires_at > now() and g.approver is not null and g.approver<>''
		and (g.approver=$1 or e.operator=$1)
		order by g.created_at desc`, login)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApproverGrant
	for rows.Next() {
		var g ApproverGrant
		if err := rows.Scan(&g.GrantID, &g.RecordID, &g.Enrollment.Kind, &g.Enrollment.RuntimeID, &g.Enrollment.Operator, &g.Enrollment.Slot, &g.Approver, &g.ExpiresAt, &g.CreatedAt, &g.Names); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
