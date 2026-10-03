// packages/envoy/internal/broker/requests/machine_state.go
// Package requests is the broker's state machine for agent_secret credential requests. A request
// row freezes, at creation, the enrollment, the resource set with each name's decision and
// delivery, the allowed approver, the rules version and the lifetime. Its only transitions are
// pending -> granted | denied | cancelled | expired; each is an UPDATE guarded by state='pending'
// inside one transaction that also writes the audit row, so a duplicate or late decision changes
// nothing. A request that needs approval also freezes an append-only credential-request record
// (internal/broker/record): the requester's signed request object plus the broker's decision
// fields. The record is decided by its approver's Dispatch login, which Dispatch's server sends on
// the UI routes and the UI bearer vouches for, and every release of a grant it produced
// re-verifies the whole chain — record hash, requester signature, one approval by the record's
// approver — so a row written by anyone but the broker releases nothing (AGENTC-393).
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

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/rules"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
)

var (
	ErrNotYours    = errors.New("this request or grant belongs to another session")
	ErrNotApprover = errors.New("only the grant's approver or its enrollment's operator may revoke it")
	ErrTerminal    = errors.New("request is already decided")
	// ErrExpired is ApplyDecision's refusal for a record that expired undecided: the sweeper
	// expired its request, or its pending_expires_at has passed before the sweeper got to it.
	ErrExpired      = errors.New("request expired before its approver acted on it")
	ErrGrantNotLive = errors.New("grant is expired, revoked, or its session ended")
	// ErrGrantChainInvalid: re-verifying a grant's whole approval chain (record hash, requester
	// signature, one approval by the record's approver) failed — a row written by anyone but the
	// broker releases nothing.
	ErrGrantChainInvalid = errors.New("this grant's approval chain no longer verifies")
	// ErrSecretNotInStore: the rules name a secret whose source the secrets store does not hold.
	ErrSecretNotInStore = errors.New("secret is not in the secrets store")
	ErrMixedApprovers   = errors.New("the requested secrets need different approvers; request them separately")
)

// jtiRetentionMargin is how long past a request object's expiry its jti is remembered, mirroring
// proof.Verifier's own retention margin.
const jtiRetentionMargin = time.Minute

// SecretDecision is how the rules decided one name of a request.
type SecretDecision struct {
	// The secret's name.
	Name string `json:"name"`
	// "automatic", "approval" or "deny".
	Decision string `json:"decision"`
	// "inject" or "proxy".
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
	MaxGrant   time.Duration
	PendingTTL time.Duration
	// Audience is BROKER_PUBLIC_URL: the aud every request object's signature is checked against.
	Audience string
	Skew     time.Duration
	// Replay records a request object's jti (the proof_jtis table), answering false when it was
	// already seen. Wired to enroll.Service.Replay in production.
	Replay func(ctx context.Context, jti string, expires time.Time) (fresh bool, err error)
	// Chain re-verifies a grant's whole approval chain on every release (VerifyChain): the same
	// record.ChainVerifier machinery enroll.Service.AuthenticateLauncher uses for launcher
	// credentials, built by NewChainVerifier against agent_secret records instead.
	Chain *record.ChainVerifier
}

// NewChainVerifier builds the record.ChainVerifier VerifyChain uses, scoped to agent_secret
// records.
func NewChainVerifier(st *store.Store, audience string, skew time.Duration) *record.ChainVerifier {
	return st.ChainVerifier("agent_secret", audience, skew)
}

type enrollmentRow struct {
	ID, Kind, Thumbprint string
	Operator             *string
	RuntimeID, Slot      string
	Subject              *string
}

// requester is the enrollment as the rules see it. A pod's slot is not part of it: every slot of
// a pod is matched by its verified service-account subject alone.
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
		Enrollment:      record.Enrollment{Kind: enr.Kind, RuntimeID: enr.RuntimeID, Operator: deref(enr.Operator), Slot: enr.Slot},
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

// ApplyDecision decides a pending agent_secret record by login, the Dispatch login of the human
// deciding it. approve=true mints the grant while the requesting enrollment is still live;
// otherwise the request is denied. It re-reads the record body, recomputes its id, refuses any
// login but the record's own approver (record.ErrNotApprover) whatever the record's state,
// re-verifies the embedded request object, and writes the event (naming that login), the request
// transition and the audit row in one transaction. For its approver, a record that expired
// undecided — expired by the sweeper, or past its pending_expires_at before the sweeper got to it
// — is ErrExpired, and any other non-pending record ErrTerminal: a duplicate or late decision
// changes nothing. The enrollment row is locked before the request row — the same order every
// other enrollment-then-request writer in this package takes them in, so none of them deadlock.
func (m *Machine) ApplyDecision(ctx context.Context, recordID string, approve bool, login string) (Decision, error) {
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return Decision{}, err
	}
	defer tx.Rollback(ctx)

	var enrollmentID, body string
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `select enrollment_id, body, created_at from credential_requests where id=$1`, recordID).
		Scan(&enrollmentID, &body, &createdAt); err != nil {
		return Decision{}, err
	}
	var live bool
	if err := tx.QueryRow(ctx, `select revoked_at is null and lease_expires_at > now() from enrollments where id=$1 for share`, enrollmentID).Scan(&live); err != nil {
		return Decision{}, err
	}
	var requestID, state string
	var lifetime int
	var expired bool
	if err := tx.QueryRow(ctx, `select id, state, lifetime_seconds, pending_expires_at <= now() from requests where record_id=$1 for update`, recordID).
		Scan(&requestID, &state, &lifetime, &expired); err != nil {
		return Decision{}, err
	}
	parsed, err := verifyRecordBody(recordID, body)
	if err != nil {
		return Decision{}, err
	}
	login, err = parsed.ApproverLogin(login)
	if err != nil {
		return Decision{}, err
	}
	switch {
	case state == "expired" || state == "pending" && expired:
		return Decision{}, ErrExpired
	case state != "pending":
		return Decision{}, ErrTerminal
	}
	// now is reset to the record's own creation time: the request object's own iat/exp bound only
	// how fresh it had to be when the broker first accepted it (up to 10 minutes), never how long
	// the resulting decision window may stay open (PendingTTL, hours) — re-verifying it with the
	// real current time would fail every decision made more than a few minutes after creation.
	if _, err := record.VerifyRequestObject(parsed.Request, m.Audience, m.Skew, createdAt); err != nil {
		return Decision{}, fmt.Errorf("%w: request object no longer verifies: %s", ErrGrantChainInvalid, err)
	}

	event, by := "denied", "human:"+login
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
		requestID, next, login, nullable(detail)); err != nil {
		return Decision{}, err
	}
	if _, err := tx.Exec(ctx, `insert into credential_request_events (record_id, event, login, actor, detail) values ($1,$2,$3,$4,$5)`,
		recordID, event, login, by, nullable(detail)); err != nil {
		return Decision{}, err
	}
	var grantID string
	if next == "granted" {
		if grantID, err = insertGrant(ctx, tx, requestID, enrollmentID, login, time.Duration(lifetime)*time.Second); err != nil {
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
// database tamper; ApplyDecision checks it before deciding a record rather than trusting the row
// blindly. VerifyChain's own re-verification goes through the shared record.ChainVerifier instead
// (m.Chain), which reproduces this same check as part of its larger chain.
func verifyRecordBody(recordID, body string) (record.Body, error) {
	parsed, err := record.VerifyBodyReproducesID(body, recordID)
	if err != nil {
		return record.Body{}, fmt.Errorf("%w: record %s: stored body does not reproduce its id", ErrGrantChainInvalid, recordID)
	}
	return parsed, nil
}

// VerifyChain re-verifies a grant's whole approval chain against whatever is true right now, not
// just what was true when the grant was minted, through the shared record.ChainVerifier
// machinery (m.Chain, built by NewChainVerifier): the record's stored body still hashes to its
// own id, its embedded request object still verifies, and it carries exactly one terminal
// decision, an approval by the login the record names as its approver — a tampered row, a forged
// grant with no approval event, an approval naming another login, or a second decision all fail
// here. An automatic grant (no record_id) needs none of this and always passes. Called by Values
// and reuseLiveGrant. A genuine dependency failure inside m.Chain.Verify (a Postgres error from
// FetchRecord or FetchDecisions) is returned as-is rather than folded into ErrGrantChainInvalid;
// only record.ErrChainBroken — every reason the chain itself does not verify — is wrapped.
func (m *Machine) VerifyChain(ctx context.Context, grantID string) error {
	var recordID *string
	if err := m.Store.Pool.QueryRow(ctx, `select r.record_id from grants g join requests r on r.id=g.request_id where g.id=$1`, grantID).Scan(&recordID); err != nil {
		return err
	}
	if recordID == nil {
		return nil
	}
	if _, err := m.Chain.Verify(ctx, *recordID); err != nil {
		if errors.Is(err, record.ErrChainBroken) {
			return fmt.Errorf("%w: %s", ErrGrantChainInvalid, err)
		}
		return err
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

// RevokeByApprover ends a grant on a human's Dispatch login. The login must be the grant's
// approver or its enrollment's operator (mayRevoke). Revoking an already-revoked grant succeeds
// and changes nothing.
func (m *Machine) RevokeByApprover(ctx context.Context, grantID, login string) error {
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
	if !mayRevoke(login, approver, operator) {
		return ErrNotApprover
	}
	actor := "human:" + record.CanonicalLogin(login)
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

// enrollment reads a live enrollment (not revoked, lease not lapsed); pgx.ErrNoRows otherwise.
func (m *Machine) enrollment(ctx context.Context, id string) (enrollmentRow, error) {
	var e enrollmentRow
	err := m.Store.Pool.QueryRow(ctx, `select id, kind, operator, thumbprint, runtime_id, slot, subject from enrollments
		where id=$1 and revoked_at is null and lease_expires_at > now()`, id).
		Scan(&e.ID, &e.Kind, &e.Operator, &e.Thumbprint, &e.RuntimeID, &e.Slot, &e.Subject)
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
// approved it, a rule tightened since then is never bypassed by reuse, and neither is a chain that
// no longer verifies.
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
