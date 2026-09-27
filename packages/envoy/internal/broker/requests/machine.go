// packages/envoy/internal/broker/requests/machine.go
// Package requests is the broker's state machine for agent_secret credential requests. A request
// row freezes, at creation, the enrollment, the resource set with each name's decision and
// delivery, the allowed approver, the rules version and the lifetime. Its only transitions are
// pending -> granted | denied | cancelled | expired; each is an UPDATE guarded by state='pending'
// inside one transaction that also writes the audit row, so a duplicate or late decision changes
// nothing. A request that needs approval also freezes an append-only credential-request record
// (internal/broker/record): the requester's signed request object plus the broker's decision
// fields. The record is decided on a WebAuthn assertion (internal/broker/approvers) over its
// domain-separated challenge, never a Dispatch ask, and every release of a grant it produced
// re-verifies the whole chain — record hash, requester signature, approver assertion against the
// pinned keys — so a row written by anyone but the broker releases nothing (AGENTC-393 v9).
package requests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/approvers"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/rules"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
)

var (
	ErrNotYours     = errors.New("this request or grant belongs to another session")
	ErrNotApprover  = errors.New("only the grant's approver or its enrollment's operator may revoke it")
	ErrTerminal     = errors.New("request is already decided")
	ErrGrantNotLive = errors.New("grant is expired, revoked, or its session ended")
	// ErrGrantChainInvalid: re-verifying a grant's whole approval chain (record hash, requester
	// signature, approver assertion against the pinned keys) failed — a row written by anyone but
	// the broker, or an approver key revoked since, releases nothing.
	ErrGrantChainInvalid = errors.New("this grant's approval chain no longer verifies")
	// ErrSecretNotInStore: the rules name a secret whose source the secrets store does not hold.
	ErrSecretNotInStore = errors.New("secret is not in the secrets store")
	ErrMixedApprovers   = errors.New("the requested secrets need different approvers; request them separately")
)

// jtiRetentionMargin is how long past a request object's expiry its jti is remembered, mirroring
// proof.Verifier's own retention margin.
const jtiRetentionMargin = time.Minute

type SecretDecision struct {
	Name     string `json:"name"`
	Decision string `json:"decision"`
	Delivery string `json:"delivery"`
	Source   string `json:"-"`
}

type Request struct {
	ID        string           `json:"request_id"`
	State     string           `json:"state"`
	GrantID   *string          `json:"grant_id"`
	RecordID  *string          `json:"record_id"`
	Secrets   []SecretDecision `json:"secrets"`
	DecidedAt *time.Time       `json:"decided_at"`
	DecidedBy *string          `json:"decided_by"`
	Detail    *string          `json:"detail"`
	Coalesced bool             `json:"coalesced,omitempty"`
}

// Decision is ApplyDecision's result: the request's new state and, when granted, the new grant's
// id (empty when the record was denied, or approved but the requesting enrollment had died).
type Decision struct {
	RequestID string
	State     string
	GrantID   string
}

type Machine struct {
	Store      *store.Store
	Rules      *rules.Current
	Secrets    secrets.Reader
	Approvers  *approvers.Service
	MaxGrant   time.Duration
	PendingTTL time.Duration
	// Audience is BROKER_PUBLIC_URL: the aud every request object's signature is checked against.
	Audience string
	Skew     time.Duration
	// Replay records a request object's jti (the proof_jtis table), answering false when it was
	// already seen. Wired to enroll.Service.Replay in production.
	Replay func(ctx context.Context, jti string, expires time.Time) (fresh bool, err error)
}

type enrollmentRow struct {
	ID, Kind, Thumbprint string
	Operator             *string
	RuntimeID            string
	Subject              *string
}

// requester is the enrollment as the rules see it.
func (e enrollmentRow) requester() rules.Requester {
	return rules.Requester{Kind: e.Kind, Operator: deref(e.Operator), Subject: deref(e.Subject)}
}

// Create verifies the request object (record.VerifyRequestObject, jti replay through the Replay
// seam), requires iss to be this enrollment's own key and no login_hint (session requests never
// name their own approver — that's the rules' job), evaluates the rules, and for a request that
// needs approval writes the request row and its credential-request record in one transaction, the
// same advisory-lock coalescing createPending has always used to serialize identical requests
// from one enrollment.
func (m *Machine) Create(ctx context.Context, enrollmentID, compactRequest, sessionID string) (Request, error) {
	enr, err := m.enrollment(ctx, enrollmentID)
	if err != nil {
		return Request{}, err
	}
	obj, err := record.VerifyRequestObject(compactRequest, m.Audience, m.Skew, time.Now())
	if err != nil {
		return Request{}, err
	}
	if obj.LoginHint != "" {
		return Request{}, fmt.Errorf("%w: login_hint is only for machine login requests", record.ErrRequestInvalid)
	}
	if obj.Thumbprint != enr.Thumbprint {
		return Request{}, fmt.Errorf("%w: iss is not this enrollment's own key", record.ErrRequestInvalid)
	}
	if len(obj.Details) == 0 || obj.Details[0].Type != "agent_secret" {
		return Request{}, fmt.Errorf("%w: this endpoint accepts only agent_secret authorization_details", record.ErrRequestInvalid)
	}
	fresh, err := m.Replay(ctx, obj.JTI, obj.Expires.Add(m.Skew+jtiRetentionMargin))
	if err != nil {
		return Request{}, err
	}
	if !fresh {
		return Request{}, fmt.Errorf("%w: replayed jti", record.ErrRequestInvalid)
	}
	names := make([]string, len(obj.Details))
	for i, d := range obj.Details {
		names[i] = d.Identifier
	}

	set := m.Rules.Get()
	requester := enr.requester()
	if existing, ok, err := m.reuseLiveGrant(ctx, enrollmentID, names, set, requester); err != nil {
		return Request{}, err
	} else if ok {
		return existing, nil
	}
	e, err := m.evaluate(set, names, requester)
	if err != nil {
		return Request{}, err
	}
	r := newRequest{
		id: uuid.NewString(), enrollmentID: enrollmentID, reason: obj.Reason, state: e.state,
		approver: e.approver, rulesVersion: set.Version, sessionID: sessionID, lifetime: e.lifetime, decisions: e.decisions,
	}
	if e.state == "pending" {
		r.pendingExpiresAt = time.Now().Add(m.PendingTTL)
		return m.createPending(ctx, enr, r, obj)
	}
	req := Request{ID: r.id, State: e.state, Secrets: e.decisions}
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return Request{}, err
	}
	defer tx.Rollback(ctx)
	if err := m.insertRequest(ctx, tx, r); err != nil {
		return Request{}, err
	}
	if e.state == "granted" {
		grantID, err := insertGrant(ctx, tx, r.id, enrollmentID, "", e.lifetime)
		if err != nil {
			return Request{}, err
		}
		req.GrantID = &grantID
	}
	return req, tx.Commit(ctx)
}

// evaluation is one pass of the rules over a request's names.
type evaluation struct {
	decisions []SecretDecision
	state     string // granted, denied or pending
	approver  string
	lifetime  time.Duration
}

func (m *Machine) evaluate(set *rules.Set, names []string, requester rules.Requester) (evaluation, error) {
	e := evaluation{decisions: make([]SecretDecision, 0, len(names)), state: "granted", lifetime: m.MaxGrant}
	needsApproval, denied := false, false
	for _, name := range names {
		d, err := set.Evaluate(name, requester)
		if err != nil {
			return evaluation{}, err
		}
		switch {
		case d.Outcome == "deny":
			denied = true
		case d.Outcome == "approval":
			if e.approver != "" && e.approver != d.Approver {
				return evaluation{}, ErrMixedApprovers
			}
			e.approver = d.Approver
			needsApproval = true
		}
		if d.MaxLifetime > 0 && d.MaxLifetime < e.lifetime {
			e.lifetime = d.MaxLifetime
		}
		e.decisions = append(e.decisions, SecretDecision{Name: name, Decision: d.Outcome, Delivery: d.Delivery, Source: d.Source})
	}
	switch {
	case denied:
		e.state = "denied"
	case needsApproval:
		e.state = "pending"
	}
	return e, nil
}

// newRequest is one request row as Create writes it.
type newRequest struct {
	id, enrollmentID, reason, state, approver, rulesVersion, sessionID, recordID string
	lifetime                                                                     time.Duration
	decisions                                                                    []SecretDecision
	// pendingExpiresAt is set only when state == "pending"; it is also the credential-request
	// record's own ExpiresAt, computed once so the two never drift a few microseconds apart.
	pendingExpiresAt time.Time
}

func (r newRequest) names() []string {
	names := make([]string, len(r.decisions))
	for i, d := range r.decisions {
		names[i] = d.Name
	}
	return names
}

// insertRequest writes the request row, its per-secret decisions, and its request.created audit
// row. A pending row's record_id is filled in by createPending in the same transaction.
func (m *Machine) insertRequest(ctx context.Context, tx pgx.Tx, r newRequest) error {
	var pendingExpires *time.Time
	if r.state == "pending" {
		t := r.pendingExpiresAt
		pendingExpires = &t
	}
	detail := ""
	if r.state == "denied" {
		detail = "policy denies at least one requested secret"
	}
	if _, err := tx.Exec(ctx, `insert into requests (id, enrollment_id, reason, state, allowed_approver, rules_version, lifetime_seconds, pending_expires_at, session_id, record_id, decided_at, decision_detail)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, case when $4 in ('granted','denied') then now() end, $11)`,
		r.id, r.enrollmentID, r.reason, r.state, nullable(r.approver), r.rulesVersion, int(r.lifetime.Seconds()), pendingExpires, nullable(r.sessionID), nullable(r.recordID), nullable(detail)); err != nil {
		return err
	}
	for _, d := range r.decisions {
		if _, err := tx.Exec(ctx, `insert into request_secrets (request_id, name, decision, delivery, source) values ($1,$2,$3,$4,$5)`, r.id, d.Name, d.Decision, d.Delivery, d.Source); err != nil {
			return err
		}
	}
	return audit(ctx, tx, "request.created", r.enrollmentID, r.id, nil, "session:"+r.enrollmentID,
		auditDetail{"state": r.state, "secrets": r.names()})
}

// createPending records a pending request and its credential-request record together, in one
// transaction that serializes identical requests from one enrollment on an advisory lock, so a
// concurrent twin coalesces onto it instead of writing a second record.
func (m *Machine) createPending(ctx context.Context, enr enrollmentRow, r newRequest, obj record.RequestObject) (Request, error) {
	sorted := sortedCopy(r.names())
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return Request{}, err
	}
	defer tx.Rollback(ctx)
	lockKey, err := json.Marshal([]any{r.enrollmentID, sorted})
	if err != nil {
		return Request{}, err
	}
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1, 0))`, string(lockKey)); err != nil {
		return Request{}, fmt.Errorf("lock identical pending requests: %w", err)
	}
	existingID, err := matchingRequest(ctx, tx, `select r.id, array_agg(s.name) from requests r join request_secrets s on s.request_id=r.id
		where r.enrollment_id=$1 and r.state='pending' group by r.id`, r.enrollmentID, sorted)
	if err != nil {
		return Request{}, err
	}
	if existingID != "" {
		if err := tx.Commit(ctx); err != nil {
			return Request{}, err
		}
		existing, err := m.Get(ctx, existingID)
		existing.Coalesced = true
		return existing, err
	}

	body := record.Body{
		Request:         obj.Compact,
		Approver:        r.approver,
		Enrollment:      record.Enrollment{Kind: enr.Kind, RuntimeID: enr.RuntimeID, Operator: deref(enr.Operator)},
		LifetimeSeconds: int(r.lifetime.Seconds()),
		RulesVersion:    r.rulesVersion,
		ExpiresAt:       r.pendingExpiresAt,
	}
	recordID := body.ID()
	if _, err := tx.Exec(ctx, `insert into credential_requests (id, body, kind, approver, enrollment_id, expires_at) values ($1,$2,'agent_secret',$3,$4,$5)`,
		recordID, body.Canonical(), body.Approver, r.enrollmentID, body.ExpiresAt); err != nil {
		return Request{}, err
	}
	r.recordID = recordID
	if err := m.insertRequest(ctx, tx, r); err != nil {
		return Request{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Request{}, err
	}
	rid := recordID
	return Request{ID: r.id, State: "pending", Secrets: r.decisions, RecordID: &rid}, nil
}

// Cancel ends a still-pending request the requesting session no longer wants, writing the
// transition, its audit row, and — when the request has a credential-request record — the
// record's cancelled event, all in one transaction.
func (m *Machine) Cancel(ctx context.Context, id, enrollmentID string) error {
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var owner, state string
	var recordID *string
	if err := tx.QueryRow(ctx, `select enrollment_id, state, record_id from requests where id=$1 for update`, id).Scan(&owner, &state, &recordID); err != nil {
		return err
	}
	if owner != enrollmentID {
		return ErrNotYours
	}
	if state != "pending" {
		return ErrTerminal
	}
	const detail = "cancelled by the requesting session"
	if _, err := tx.Exec(ctx, `update requests set state='cancelled', decided_at=now(), decided_by=$2, decision_detail=$3 where id=$1 and state='pending'`, id, "session:"+enrollmentID, detail); err != nil {
		return err
	}
	if err := audit(ctx, tx, "request.cancelled", enrollmentID, id, nil, "session:"+enrollmentID, auditDetail{}); err != nil {
		return err
	}
	if recordID != nil {
		if _, err := tx.Exec(ctx, `insert into credential_request_events (record_id, event, actor, detail) values ($1,'cancelled',$2,$3)`,
			*recordID, "session:"+enrollmentID, detail); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// expiredRequest is one row a sweep's own transaction moved from pending to expired: enough to
// wake its owner (Sweeper's own job) without re-reading the request afterward.
type expiredRequest struct {
	id, enrollmentID, recordID string
}

// ExpirePending expires every pending request past its deadline, writing the state transition,
// its audit row, and its record's expired event together.
func (m *Machine) ExpirePending(ctx context.Context, now time.Time) (int, error) {
	expired, err := m.expirePending(ctx, now)
	return len(expired), err
}

// expirePending is ExpirePending's shared implementation: it returns the rows it moved to
// 'expired' so Sweeper (this package's own Tick) can wake each one's owner once the transaction
// has actually committed.
func (m *Machine) expirePending(ctx context.Context, now time.Time) ([]expiredRequest, error) {
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	const detail = "no answer before the request expired"
	rows, err := tx.Query(ctx, `update requests set state='expired', decided_at=$1, decision_detail=$2
		where state='pending' and pending_expires_at < $1 returning id, enrollment_id, record_id`, now, detail)
	if err != nil {
		return nil, err
	}
	var expired []expiredRequest
	for rows.Next() {
		var r expiredRequest
		if err := rows.Scan(&r.id, &r.enrollmentID, &r.recordID); err != nil {
			rows.Close()
			return nil, err
		}
		expired = append(expired, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for _, r := range expired {
		if _, err := tx.Exec(ctx, `insert into audit (kind, enrollment_id, request_id, actor) values ('request.expired',$1,$2,'broker')`, r.enrollmentID, r.id); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `insert into credential_request_events (record_id, event, actor, detail) values ($1,'expired','broker',$2)`, r.recordID, detail); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return expired, nil
}

// ApplyDecision decides a pending agent_secret record on a verified assertion. approve=true mints
// the grant while the requesting enrollment is still live; otherwise (or on a deny) the request is
// denied. It re-reads the record body, recomputes its id, re-verifies the embedded request object,
// then verifies the assertion inside the same transaction (approvers.Service.VerifyAssertion bumps
// the sign_count counter), and writes the event, the request transition and the audit row in that
// same transaction. A non-pending record is ErrTerminal: a duplicate or late decision changes
// nothing. The enrollment row is locked before the request row — the same order every other
// enrollment-then-request writer in this package takes them in, so none of them deadlock.
func (m *Machine) ApplyDecision(ctx context.Context, recordID string, approve bool, assertion json.RawMessage) (Decision, error) {
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return Decision{}, err
	}
	defer tx.Rollback(ctx)

	var enrollmentID, approver, body string
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `select enrollment_id, approver, body, created_at from credential_requests where id=$1`, recordID).
		Scan(&enrollmentID, &approver, &body, &createdAt); err != nil {
		return Decision{}, err
	}
	var live bool
	if err := tx.QueryRow(ctx, `select revoked_at is null and lease_expires_at > now() from enrollments where id=$1 for share`, enrollmentID).Scan(&live); err != nil {
		return Decision{}, err
	}
	var requestID, state string
	var lifetime int
	if err := tx.QueryRow(ctx, `select id, state, lifetime_seconds from requests where record_id=$1 for update`, recordID).
		Scan(&requestID, &state, &lifetime); err != nil {
		return Decision{}, err
	}
	if state != "pending" {
		return Decision{}, ErrTerminal
	}

	parsed, err := verifyRecordBody(recordID, body)
	if err != nil {
		return Decision{}, err
	}
	// now is reset to the record's own creation time: the request object's own iat/exp bound only
	// how fresh it had to be when the broker first accepted it (up to 10 minutes), never how long
	// the resulting decision window may stay open (PendingTTL, hours) — re-verifying it with the
	// real current time would fail every decision made more than a few minutes after creation.
	if _, err := record.VerifyRequestObject(parsed.Request, m.Audience, m.Skew, createdAt); err != nil {
		return Decision{}, fmt.Errorf("%w: request object no longer verifies: %s", ErrGrantChainInvalid, err)
	}

	challenge := record.ApproveChallenge(recordID)
	if !approve {
		challenge = record.DenyChallenge(recordID)
	}
	asserted, err := m.Approvers.VerifyAssertion(ctx, tx, approver, challenge, assertion)
	if err != nil {
		return Decision{}, err
	}
	event, by := "denied", "human:"+asserted.Login
	next, detail := "denied", ""
	if approve {
		event = "approved"
		if live {
			next = "granted"
		} else {
			detail = "the requesting enrollment is no longer live"
		}
	}
	if _, err := tx.Exec(ctx, `update requests set state=$2, decided_at=now(), decided_by=$3, decision_detail=$4 where id=$1 and state='pending'`,
		requestID, next, asserted.Login, nullable(detail)); err != nil {
		return Decision{}, err
	}
	if _, err := tx.Exec(ctx, `insert into credential_request_events (record_id, event, assertion, credential_id, actor, detail) values ($1,$2,$3,$4,$5,$6)`,
		recordID, event, assertion, asserted.CredentialID, by, nullable(detail)); err != nil {
		return Decision{}, err
	}
	var grantID string
	if next == "granted" {
		if grantID, err = insertGrant(ctx, tx, requestID, enrollmentID, asserted.Login, time.Duration(lifetime)*time.Second); err != nil {
			return Decision{}, err
		}
	}
	if err := audit(ctx, tx, "request."+next, enrollmentID, requestID, nullable(grantID), by, auditDetail{"detail": detail, "record_id": recordID}); err != nil {
		return Decision{}, err
	}
	return Decision{RequestID: requestID, State: next, GrantID: grantID}, tx.Commit(ctx)
}

// verifyRecordBody re-parses a credential_requests row's stored body and confirms it still hashes
// to its own id. The append-only trigger should make a mismatch unreachable outside a direct
// database tamper; every reader checks it rather than trusting the row blindly.
func verifyRecordBody(recordID, body string) (record.Body, error) {
	parsed, err := record.ParseBody(body)
	if err != nil || parsed.ID() != recordID {
		return record.Body{}, fmt.Errorf("%w: record %s: stored body does not reproduce its id", ErrGrantChainInvalid, recordID)
	}
	return parsed, nil
}

// VerifyChain re-verifies a grant's whole approval chain against whatever is true right now, not
// just what was true when the grant was minted: the record's stored body still hashes to its own
// id, its embedded request object still verifies, and its single approval event's stored assertion
// still verifies against the pinned key material for its approver — a tampered row, a forged grant
// with no approval event, or a since-revoked approving key all fail here. An automatic grant (no
// record_id) needs none of this and always passes. Called by Values and reuseLiveGrant.
func (m *Machine) VerifyChain(ctx context.Context, grantID string) error {
	var recordID *string
	if err := m.Store.Pool.QueryRow(ctx, `select r.record_id from grants g join requests r on r.id=g.request_id where g.id=$1`, grantID).Scan(&recordID); err != nil {
		return err
	}
	if recordID == nil {
		return nil
	}
	var body, approver string
	var createdAt time.Time
	if err := m.Store.Pool.QueryRow(ctx, `select body, approver, created_at from credential_requests where id=$1`, *recordID).Scan(&body, &approver, &createdAt); err != nil {
		return err
	}
	parsed, err := verifyRecordBody(*recordID, body)
	if err != nil {
		return err
	}
	if _, err := record.VerifyRequestObject(parsed.Request, m.Audience, m.Skew, createdAt); err != nil {
		return fmt.Errorf("%w: request object no longer verifies: %s", ErrGrantChainInvalid, err)
	}
	var assertion json.RawMessage
	err = m.Store.Pool.QueryRow(ctx, `select assertion from credential_request_events where record_id=$1 and event='approved'`, *recordID).Scan(&assertion)
	if errors.Is(err, pgx.ErrNoRows) || len(assertion) == 0 {
		return fmt.Errorf("%w: no approval event on this record", ErrGrantChainInvalid)
	}
	if err != nil {
		return err
	}
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // read-only re-verification; never commits a sign_count bump
	if _, err := m.Approvers.VerifyAssertion(ctx, tx, approver, record.ApproveChallenge(*recordID), assertion); err != nil && !errors.Is(err, approvers.ErrCounterReplay) {
		// ErrCounterReplay is expected and harmless here: we are re-checking the exact historical
		// assertion the deciding transaction already consumed, not authorizing a new action.
		return fmt.Errorf("%w: approver assertion no longer verifies: %s", ErrGrantChainInvalid, err)
	}
	return nil
}

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

// RevokeGrant ends a grant a session holds — a session may end only its own grant. Revoking an
// already-revoked grant succeeds and changes nothing: it writes no second grant.revoked audit row,
// so the trail never names a revoker who ended nothing. Human revocation is RevokeByApprover.
func (m *Machine) RevokeGrant(ctx context.Context, grantID, enrollmentID string) error {
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var owner, requestID string
	if err := tx.QueryRow(ctx, `select enrollment_id, request_id from grants where id=$1 for update`, grantID).Scan(&owner, &requestID); err != nil {
		return err
	}
	if owner != enrollmentID {
		return ErrNotYours
	}
	actor := "session:" + enrollmentID
	tag, err := tx.Exec(ctx, `update grants set revoked_at=now(), revoked_by=$2 where id=$1 and revoked_at is null`, grantID, actor)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if err := audit(ctx, tx, "grant.revoked", owner, requestID, &grantID, actor, auditDetail{}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RevokeByApprover ends a grant on a human's WebAuthn assertion over its revoke challenge. The
// asserting key must belong to the grant's approver or its enrollment's operator (mayRevoke),
// matching the same trust boundary the pre-record human revocation path enforced, now
// authenticated by an assertion instead of a Dispatch login. Revoking an already-revoked grant
// succeeds and changes nothing.
func (m *Machine) RevokeByApprover(ctx context.Context, grantID string, assertion json.RawMessage) error {
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var owner, requestID string
	var approver, operator *string
	if err := tx.QueryRow(ctx, `select g.enrollment_id, g.request_id, g.approver, e.operator
		from grants g join enrollments e on e.id=g.enrollment_id where g.id=$1 for update of g`, grantID).Scan(&owner, &requestID, &approver, &operator); err != nil {
		return err
	}
	asserted, err := m.Approvers.VerifyAssertion(ctx, tx, "", record.RevokeChallenge(grantID), assertion)
	if err != nil {
		return err
	}
	if !mayRevoke(asserted.Login, approver, operator) {
		return ErrNotApprover
	}
	actor := "human:" + asserted.Login
	tag, err := tx.Exec(ctx, `update grants set revoked_at=now(), revoked_by=$2 where id=$1 and revoked_at is null`, grantID, actor)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if err := audit(ctx, tx, "grant.revoked", owner, requestID, &grantID, actor, auditDetail{}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// mayRevoke reports whether the (already-canonical) login is the grant's approver or its
// enrollment's operator.
func mayRevoke(login string, approver, operator *string) bool {
	login = record.CanonicalLogin(login)
	if login == "" {
		return false
	}
	for _, allowed := range []*string{approver, operator} {
		if allowed != nil && record.CanonicalLogin(*allowed) == login {
			return true
		}
	}
	return false
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
// names approver, newest first: GET /v1/pending's exact contract. A record counts as pending when
// it carries no terminal decision event yet, matching credential_request_decision's own partial
// index.
func (m *Machine) PendingForApprover(ctx context.Context, approver string) ([]PendingSummary, error) {
	rows, err := m.Store.Pool.Query(ctx, `select cr.id, cr.kind, cr.body, cr.created_at from credential_requests cr
		where cr.approver=$1 and not exists (
			select 1 from credential_request_events ev where ev.record_id=cr.id and ev.event in ('approved','denied','expired','cancelled')
		) order by cr.created_at desc`, record.CanonicalLogin(approver))
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
// /v1/credential-requests/{id} and, reused verbatim, POST /v1/machine-logins/lookup (which adds
// its own challenges on top). pgx.ErrNoRows means no such record.
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
		where record_id=$1 and event in ('approved','denied','expired','cancelled')`, recordID).Scan(&event, &decidedAt, &credID)
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
// revoke, for GET /v1/grants?approver=<login>.
type ApproverGrant struct {
	GrantID    string
	RecordID   *string
	Enrollment record.Enrollment
	Names      []string
	ExpiresAt  time.Time
	CreatedAt  time.Time
}

// GrantsForApprover lists every live grant approver (or its enrollment's operator) may revoke:
// only grants an approval actually decided — an automatic grant carries no approver and never
// appears here — newest first.
func (m *Machine) GrantsForApprover(ctx context.Context, approver string) ([]ApproverGrant, error) {
	login := record.CanonicalLogin(approver)
	rows, err := m.Store.Pool.Query(ctx, `select g.id, r.record_id, e.kind, e.runtime_id, coalesce(e.operator,''), g.expires_at, g.created_at,
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
		if err := rows.Scan(&g.GrantID, &g.RecordID, &g.Enrollment.Kind, &g.Enrollment.RuntimeID, &g.Enrollment.Operator, &g.ExpiresAt, &g.CreatedAt, &g.Names); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// enrollment reads a live enrollment (not revoked, lease not lapsed); pgx.ErrNoRows otherwise.
func (m *Machine) enrollment(ctx context.Context, id string) (enrollmentRow, error) {
	var e enrollmentRow
	err := m.Store.Pool.QueryRow(ctx, `select id, kind, operator, thumbprint, runtime_id, subject from enrollments
		where id=$1 and revoked_at is null and lease_expires_at > now()`, id).
		Scan(&e.ID, &e.Kind, &e.Operator, &e.Thumbprint, &e.RuntimeID, &e.Subject)
	return e, err
}

// matchingRequest runs query (whose rows are a request id and that request's secret names, with
// $1 bound to enrollmentID) and answers the first request whose names are exactly sorted, or ""
// when none is. Names are compared as sets of whole strings, never as one joined string, so a
// name containing a separator cannot pass for two names.
func matchingRequest(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, query, enrollmentID string, sorted []string) (string, error) {
	rows, err := q.Query(ctx, query, enrollmentID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var names []string
		if err := rows.Scan(&id, &names); err != nil {
			return "", err
		}
		sort.Strings(names)
		if slices.Equal(names, sorted) {
			return id, nil
		}
	}
	return "", rows.Err()
}

// reuseLiveGrant answers Create's own "request (or reuse the live grant)" contract: an exact
// name-set match (never a subset or superset — the same matching rule coalescing uses for pending
// requests) against a still-live grant (not revoked, not expired) under this enrollment is
// returned as-is, with no new request row and no new record, as long as the rules it was decided
// under are still current or the current rules still allow it (stillAllowed, the check Values
// makes) and its whole approval chain still verifies (VerifyChain, the same check Values makes). A
// caller that already holds a live grant for these exact names never re-asks a human who already
// approved it, a rule tightened since then is never bypassed by reuse, and neither is a chain a
// since-revoked approver key broke.
func (m *Machine) reuseLiveGrant(ctx context.Context, enrollmentID string, names []string, set *rules.Set, requester rules.Requester) (Request, bool, error) {
	id, err := matchingRequest(ctx, m.Store.Pool, `select r.id, array_agg(s.name) from requests r
		join request_secrets s on s.request_id=r.id
		join grants g on g.request_id=r.id
		where r.enrollment_id=$1 and r.state='granted' and g.revoked_at is null and g.expires_at > now()
		group by r.id`, enrollmentID, sortedCopy(names))
	if err != nil || id == "" {
		return Request{}, false, err
	}
	var rulesVersion string
	if err := m.Store.Pool.QueryRow(ctx, `select rules_version from requests where id=$1`, id).Scan(&rulesVersion); err != nil {
		return Request{}, false, err
	}
	if rulesVersion != set.Version {
		granted, err := m.grantedSecrets(ctx, id)
		if err != nil {
			return Request{}, false, err
		}
		for _, g := range granted {
			if err := stillAllowed(set, g.name, g.decision, requester); errors.Is(err, ErrGrantNotLive) {
				return Request{}, false, nil
			} else if err != nil {
				return Request{}, false, err
			}
		}
	}
	r, err := m.Get(ctx, id)
	if err != nil {
		return Request{}, false, err
	}
	if r.GrantID != nil {
		if err := m.VerifyChain(ctx, *r.GrantID); errors.Is(err, ErrGrantChainInvalid) {
			return Request{}, false, nil
		} else if err != nil {
			return Request{}, false, err
		}
	}
	return r, true, nil
}

func insertGrant(ctx context.Context, tx pgx.Tx, requestID, enrollmentID, approver string, lifetime time.Duration) (string, error) {
	id := uuid.NewString()
	_, err := tx.Exec(ctx, `insert into grants (id, request_id, enrollment_id, approver, expires_at) values ($1,$2,$3,$4, now() + $5::interval)`,
		id, requestID, enrollmentID, nullable(approver), fmt.Sprintf("%d seconds", int(lifetime.Seconds())))
	return id, err
}

// auditDetail is an audit row's non-secret detail object; audit writes it as JSON.
type auditDetail map[string]any

func audit(ctx context.Context, tx pgx.Tx, kind, enrollmentID, requestID string, grantID *string, actor string, detail auditDetail) error {
	_, err := tx.Exec(ctx, `insert into audit (kind, enrollment_id, request_id, grant_id, actor, detail) values ($1,$2,$3,$4,$5,$6)`, kind, enrollmentID, requestID, grantID, actor, detail)
	return err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func sortedCopy(s []string) []string {
	sorted := append([]string(nil), s...)
	sort.Strings(sorted)
	return sorted
}
