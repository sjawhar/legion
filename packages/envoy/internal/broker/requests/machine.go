// packages/envoy/internal/broker/requests/machine.go
// Package requests is the broker's state machine. A request row freezes, at creation, the
// enrollment, the resource set with each name's decision and delivery, the allowed approver,
// the rules version and the lifetime. Its only transitions are pending -> granted | denied |
// cancelled | expired; each is an UPDATE guarded by state='pending' inside one transaction that
// also writes the audit row, so a duplicate or late answer changes nothing.
package requests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/dispatch"
	"github.com/sjawhar/envoy/internal/broker/rules"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
)

var (
	ErrNotYours     = errors.New("this request or grant belongs to another session")
	ErrNotApprover  = errors.New("only the grant's approver or its enrollment's operator may revoke it")
	ErrTerminal     = errors.New("request is already decided")
	ErrGrantNotLive = errors.New("grant is expired, revoked, or its session ended")
	// ErrSecretNotInStore: the rules name a secret whose source the secrets store does not hold.
	ErrSecretNotInStore = errors.New("secret is not in the secrets store")
	ErrMixedApprovers   = errors.New("the requested secrets need different approvers; request them separately")
	ErrReasonTooLong    = errors.New("reason must be at most 400 characters")
	ErrIssueMismatch    = errors.New("this session's approvals belong to its own issue; the requested issue differs")
)

type askOpener interface {
	CreateAsk(ctx context.Context, issue, question string, options []dispatch.Option, urgency string) (dispatch.Ask, error)
}

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
	AskRef    *string          `json:"ask"`
	IssueKey  string           `json:"-"`
	Secrets   []SecretDecision `json:"secrets"`
	DecidedAt *time.Time       `json:"decided_at"`
	DecidedBy *string          `json:"decided_by"`
	Detail    *string          `json:"detail"`
	Coalesced bool             `json:"coalesced,omitempty"`
}

type Machine struct {
	Store         *store.Store
	Rules         *rules.Current
	Dispatch      askOpener
	Secrets       secrets.Reader
	MaxGrant      time.Duration
	PendingTTL    time.Duration
	StandingIssue func(ctx context.Context, operator string) (string, error)
	IssueAssignee func(ctx context.Context, issue string) (string, error)
}

type enrollmentRow struct {
	ID, Kind      string
	Operator      *string
	ApproverKind  string
	ApproverIssue *string
	RuntimeID     string
	Subject       *string
}

// requester is the enrollment as the rules see it, before any Dispatch lookup.
func (e enrollmentRow) requester() rules.Requester {
	return rules.Requester{Kind: e.Kind, Operator: deref(e.Operator), Subject: deref(e.Subject)}
}

// Create decides a request for names. It evaluates the rules before it asks Dispatch anything:
// a request every name of which is automatic, or any name of which is denied, is decided without
// Dispatch, so automatic access keeps working while Dispatch is unreachable. Only a request that
// needs an approval looks up the approving issue's assignee (when the enrollment approves through
// one) and the operator's standing issue (when no issue was named), and opens an ask; a Dispatch
// failure on that path is returned as the *dispatch.Error it is.
func (m *Machine) Create(ctx context.Context, enrollmentID string, names []string, reason, issue, sessionID string) (Request, error) {
	if len(reason) > 400 {
		return Request{}, ErrReasonTooLong
	}
	enr, err := m.enrollment(ctx, enrollmentID)
	if err != nil {
		return Request{}, err
	}
	issueKey := issue
	if enr.ApproverKind == "issue_assignee" && enr.ApproverIssue != nil {
		if issue != "" && issue != *enr.ApproverIssue {
			return Request{}, ErrIssueMismatch
		}
		issueKey = *enr.ApproverIssue
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
	if e.awaitsAssignee && enr.ApproverKind == "issue_assignee" {
		if requester.IssueAssignee, err = m.IssueAssignee(ctx, issueKey); err != nil {
			return Request{}, fmt.Errorf("read the approving issue's assignee: %w", err)
		}
		if e, err = m.evaluate(set, names, requester); err != nil {
			return Request{}, err
		}
	}
	if e.state == "pending" && issueKey == "" {
		if requester.Operator == "" {
			return Request{}, errors.New("this enrollment has no approving issue and no operator whose standing issue could hold the ask")
		}
		if issueKey, err = m.StandingIssue(ctx, requester.Operator); err != nil {
			return Request{}, fmt.Errorf("find the standing secrets issue: %w", err)
		}
	}
	r := newRequest{
		id: uuid.NewString(), enrollmentID: enrollmentID, issueKey: issueKey, reason: reason, state: e.state,
		approver: e.approver, rulesVersion: set.Version, sessionID: sessionID, lifetime: e.lifetime, decisions: e.decisions,
		denialDetail: e.denialDetail,
	}
	if e.state == "pending" {
		return m.createPending(ctx, enr, r)
	}
	req := Request{ID: r.id, State: e.state, IssueKey: issueKey, Secrets: e.decisions}
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
	// awaitsAssignee: nothing else denies the request, and some name wants the approving issue's
	// assignee, who was not known to this pass.
	awaitsAssignee bool
	// denialDetail overrides the generic policy-deny decision_detail when the denial has a more
	// specific cause (currently: the matching rule needs the issue's assignee to approve, but no
	// assignee is available).
	denialDetail string
}

func (m *Machine) evaluate(set *rules.Set, names []string, requester rules.Requester) (evaluation, error) {
	e := evaluation{decisions: make([]SecretDecision, 0, len(names)), state: "granted", lifetime: m.MaxGrant}
	needsApproval, denied, awaiting := false, false, false
	for _, name := range names {
		d, err := set.Evaluate(name, requester)
		if errors.Is(err, rules.ErrUnknownSecret) {
			d = rules.Decision{Outcome: "deny", Delivery: "inject", Source: ""}
		} else if err != nil {
			return evaluation{}, err
		}
		switch {
		case d.NeedsAssignee && requester.IssueAssignee == "":
			awaiting = true
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
	case denied || awaiting:
		e.state = "denied"
		e.awaitsAssignee = awaiting && !denied
		if e.awaitsAssignee {
			e.denialDetail = "the matching rule requires approval by the issue's assignee, but none is available to decide this request"
		}
	case needsApproval:
		e.state = "pending"
	}
	return e, nil
}

// newRequest is one request row as Create writes it.
type newRequest struct {
	id, enrollmentID, issueKey, reason, state, approver, rulesVersion, sessionID, denialDetail string
	lifetime                                                                                   time.Duration
	decisions                                                                                  []SecretDecision
}

func (r newRequest) names() []string {
	names := make([]string, len(r.decisions))
	for i, d := range r.decisions {
		names[i] = d.Name
	}
	return names
}

// insertRequest writes the request row, its per-secret decisions, and its request.created audit
// row. A pending row starts with no ask; createPending records it once Dispatch has opened one.
func (m *Machine) insertRequest(ctx context.Context, tx pgx.Tx, r newRequest) error {
	var pendingExpires *time.Time
	if r.state == "pending" {
		t := time.Now().Add(m.PendingTTL)
		pendingExpires = &t
	}
	detail := ""
	if r.state == "denied" {
		detail = r.denialDetail
		if detail == "" {
			detail = "policy denies at least one requested secret"
		}
	}
	if _, err := tx.Exec(ctx, `insert into requests (id, enrollment_id, issue_key, reason, state, allowed_approver, rules_version, lifetime_seconds, pending_expires_at, session_id, decided_at, decision_detail)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, case when $5 in ('granted','denied') then now() end, $11)`,
		r.id, r.enrollmentID, r.issueKey, r.reason, r.state, nullable(r.approver), r.rulesVersion, int(r.lifetime.Seconds()), pendingExpires, nullable(r.sessionID), nullable(detail)); err != nil {
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

// createPending records a pending request and opens its Dispatch ask without ever holding a
// transaction or pooled connection across the Dispatch call. The row is written first, with no
// ask, in one short transaction that serializes identical requests from one enrollment on an
// advisory lock, so a concurrent twin coalesces onto it instead of opening a second ask. The ask
// is then opened with nothing held and its id recorded in a second statement. An open that fails
// cancels the row; a row whose ask was never recorded (the broker stopped in between) is
// cancelled by CancelUnopened.
func (m *Machine) createPending(ctx context.Context, enr enrollmentRow, r newRequest) (Request, error) {
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
	if err := m.insertRequest(ctx, tx, r); err != nil {
		return Request{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Request{}, err
	}

	// The row exists now: finish opening its ask even if the caller goes away, so a disconnect
	// never strands it half-made. The Dispatch client's own timeout bounds the call.
	ctx = context.WithoutCancel(ctx)
	ask, err := m.Dispatch.CreateAsk(ctx, r.issueKey, question(enr, r.decisions, r.reason, r.lifetime), []dispatch.Option{
		{Label: "Approve", Description: "Release these secrets to this one session until its end or the lifetime shown."},
		{Label: "Deny", Description: "Refuse; the agent is told the request was denied."},
	}, "med")
	if err != nil {
		if cancelErr := m.cancelByBroker(ctx, r.id, r.enrollmentID, "the Dispatch ask could not be opened"); cancelErr != nil {
			slog.Error("broker: cancel a request whose ask could not be opened", "request", r.id, "error", cancelErr)
		}
		return Request{}, fmt.Errorf("open Dispatch ask: %w", err)
	}
	// Recorded even if the row has left pending meanwhile (the session cancelled it), so the ask
	// stays tied to its request.
	if _, err := m.Store.Pool.Exec(ctx, `update requests set ask_id=$2, ask_edited_at=$3 where id=$1 and ask_id is null`, r.id, ask.ID, ask.EditedAt); err != nil {
		return Request{}, err
	}
	ref := "dispatch://" + r.issueKey + "/ask/" + ask.ID
	return Request{ID: r.id, State: "pending", IssueKey: r.issueKey, Secrets: r.decisions, AskRef: &ref}, nil
}

// cancelByBroker cancels a still-pending request the broker itself can no longer carry forward,
// writing the transition and its audit row together.
func (m *Machine) cancelByBroker(ctx context.Context, id, enrollmentID, detail string) error {
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `update requests set state='cancelled', decided_at=now(), decided_by='broker', decision_detail=$2 where id=$1 and state='pending'`, id, detail)
	if err != nil || tag.RowsAffected() != 1 {
		return err
	}
	if err := audit(ctx, tx, "request.cancelled", enrollmentID, id, nil, "broker", auditDetail{"reason": detail}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// unopenedAskGrace is how long a pending request may go without a recorded ask before
// CancelUnopened treats it as stranded: comfortably longer than one Dispatch call can take.
const unopenedAskGrace = 2 * time.Minute

// CancelUnopened cancels every pending request whose Dispatch ask was never recorded within
// unopenedAskGrace of its creation — a request whose broker stopped between writing the row and
// opening its ask — each with its own audit row, in one transaction.
func (m *Machine) CancelUnopened(ctx context.Context, now time.Time) (int, error) {
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	const detail = "the Dispatch ask was never opened"
	rows, err := tx.Query(ctx, `update requests set state='cancelled', decided_at=$1, decided_by='broker', decision_detail=$2
		where state='pending' and ask_id is null and created_at < $3 returning id, enrollment_id`, now, detail, now.Add(-unopenedAskGrace))
	if err != nil {
		return 0, err
	}
	type cancelledRow struct{ id, enrollmentID string }
	var cancelled []cancelledRow
	for rows.Next() {
		var c cancelledRow
		if err := rows.Scan(&c.id, &c.enrollmentID); err != nil {
			rows.Close()
			return 0, err
		}
		cancelled = append(cancelled, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, c := range cancelled {
		if err := audit(ctx, tx, "request.cancelled", c.enrollmentID, c.id, nil, "broker", auditDetail{"reason": detail}); err != nil {
			return 0, err
		}
	}
	return len(cancelled), tx.Commit(ctx)
}

// ApplyAnswer moves a pending request on a Dispatch ask read. It is idempotent: a request that is
// no longer pending is left alone and changed=false is returned. An approval grants only while
// the requesting enrollment is still live (not revoked, lease not lapsed); otherwise the request
// is denied as withdrawn. The enrollment row is locked before the request row, the same order
// enroll.Revoke takes them in, so the two never deadlock.
func (m *Machine) ApplyAnswer(ctx context.Context, id string, ask dispatch.Ask) (bool, error) {
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var enrollmentID, state string
	var approver, storedAskID, recordedEdited *string
	var lifetime int
	var live bool
	if err := tx.QueryRow(ctx, `select enrollment_id from requests where id=$1`, id).Scan(&enrollmentID); err != nil {
		return false, err
	}
	if err := tx.QueryRow(ctx, `select revoked_at is null and lease_expires_at > now() from enrollments where id=$1 for share`, enrollmentID).Scan(&live); err != nil {
		return false, err
	}
	err = tx.QueryRow(ctx, `select state, allowed_approver, ask_id, ask_edited_at, lifetime_seconds from requests where id=$1 for update`, id).
		Scan(&state, &approver, &storedAskID, &recordedEdited, &lifetime)
	if err != nil {
		return false, err
	}
	if state != "pending" {
		return false, nil
	}
	decision, detail, err := dispatch.Verdict(ask, deref(storedAskID), recordedEdited, deref(approver))
	if err != nil || decision == dispatch.Undecided {
		return false, err
	}
	next := "denied"
	if decision == dispatch.Approved {
		if live {
			next = "granted"
		} else {
			detail = "enrollment no longer live"
		}
	}
	by, actor := "", "broker"
	if ask.Answer != nil {
		by = dispatch.CanonicalLogin(ask.Answer.User)
		actor = "human:" + by
	}
	tag, err := tx.Exec(ctx, `update requests set state=$2, decided_at=now(), decided_by=$3, decision_detail=$4 where id=$1 and state='pending'`, id, next, nullable(by), nullable(detail))
	if err != nil || tag.RowsAffected() != 1 {
		return false, err
	}
	var grantID *string
	if next == "granted" {
		g, err := insertGrant(ctx, tx, id, enrollmentID, by, time.Duration(lifetime)*time.Second)
		if err != nil {
			return false, err
		}
		grantID = &g
	}
	if err := audit(ctx, tx, "request."+next, enrollmentID, id, grantID, actor, auditDetail{"detail": detail, "ask_id": ask.ID}); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (m *Machine) Cancel(ctx context.Context, id, enrollmentID string) error {
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var owner, state string
	if err := tx.QueryRow(ctx, `select enrollment_id, state from requests where id=$1 for update`, id).Scan(&owner, &state); err != nil {
		return err
	}
	if owner != enrollmentID {
		return ErrNotYours
	}
	if state != "pending" {
		return ErrTerminal
	}
	if _, err := tx.Exec(ctx, `update requests set state='cancelled', decided_at=now(), decided_by=$2, decision_detail='cancelled by the requesting session' where id=$1 and state='pending'`, id, "session:"+enrollmentID); err != nil {
		return err
	}
	if err := audit(ctx, tx, "request.cancelled", enrollmentID, id, nil, "session:"+enrollmentID, auditDetail{}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (m *Machine) ExpirePending(ctx context.Context, now time.Time) (int, error) {
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `update requests set state='expired', decided_at=$1, decision_detail='no answer before the request expired' where state='pending' and pending_expires_at < $1 returning id, enrollment_id`, now)
	if err != nil {
		return 0, err
	}
	type expiredRow struct{ id, enrollmentID string }
	var expired []expiredRow
	for rows.Next() {
		var r expiredRow
		if err := rows.Scan(&r.id, &r.enrollmentID); err != nil {
			rows.Close()
			return 0, err
		}
		expired = append(expired, r)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	rows.Close()
	for _, r := range expired {
		if _, err := tx.Exec(ctx, `insert into audit (kind, enrollment_id, request_id, actor) values ('request.expired',$1,$2,'broker')`, r.enrollmentID, r.id); err != nil {
			return 0, err
		}
	}
	return len(expired), tx.Commit(ctx)
}

// Values releases the inject-mode values of a live grant to its own enrollment. Every call
// re-checks the enrollment and the grant, and — when the rules have changed since the grant's
// request was decided — that the current rules still allow this requester every granted name
// (stillAllowed). A name is released only when both the delivery frozen at grant time and the
// current delivery are inject: a grant approved for proxy delivery never widens into a raw value.
// It holds no pooled connection across a Secrets Manager read: the grant's names are read into
// memory before the first value is fetched.
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
// is. An entry that wants the issue's assignee counts as wanting an approval without the
// assignee being looked up.
func stillAllowed(set *rules.Set, name, frozenDecision string, requester rules.Requester) error {
	d, err := set.Evaluate(name, requester)
	if errors.Is(err, rules.ErrUnknownSecret) {
		return fmt.Errorf("%w: %s is no longer in the rules", ErrGrantNotLive, name)
	}
	if err != nil {
		return err
	}
	outcome := d.Outcome
	if d.NeedsAssignee {
		outcome = "approval"
	}
	switch {
	case outcome == "deny":
		return fmt.Errorf("%w: the current rules no longer allow %s", ErrGrantNotLive, name)
	case frozenDecision == "automatic" && outcome == "approval":
		return fmt.Errorf("%w: the current rules require approval for %s", ErrGrantNotLive, name)
	}
	return nil
}

// Revoker is who is ending a grant: exactly one of a session (EnrollmentID, authenticated by its
// proof) or a human (Login, a Dispatch login the caller authenticated).
type Revoker struct {
	EnrollmentID string
	Login        string
}

// actor is the Revoker in the audit trail's actor convention.
func (r Revoker) actor() string {
	if r.EnrollmentID != "" {
		return "session:" + r.EnrollmentID
	}
	return "human:" + dispatch.CanonicalLogin(r.Login)
}

// RevokeGrant ends a grant. A session may end only its own grants; a human only a grant they
// approved or one whose enrollment they operate, so a signed-in human of one operator can never
// end another operator's grant. Revoking an already-revoked grant succeeds and changes nothing: it
// writes no second grant.revoked audit row, so the trail never names a revoker who ended nothing.
func (m *Machine) RevokeGrant(ctx context.Context, grantID string, by Revoker) error {
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
	if by.EnrollmentID != "" {
		if by.EnrollmentID != owner {
			return ErrNotYours
		}
	} else if !mayRevoke(by.Login, approver, operator) {
		return ErrNotApprover
	}
	tag, err := tx.Exec(ctx, `update grants set revoked_at=now(), revoked_by=$2 where id=$1 and revoked_at is null`, grantID, by.actor())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if err := audit(ctx, tx, "grant.revoked", owner, requestID, &grantID, by.actor(), auditDetail{}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// mayRevoke reports whether the human login is the grant's approver or its enrollment's operator.
func mayRevoke(login string, approver, operator *string) bool {
	login = dispatch.CanonicalLogin(login)
	if login == "" {
		return false
	}
	for _, allowed := range []*string{approver, operator} {
		if allowed != nil && dispatch.CanonicalLogin(*allowed) == login {
			return true
		}
	}
	return false
}

func (m *Machine) Get(ctx context.Context, id string) (Request, error) {
	var r Request
	var enrollmentID string
	err := m.Store.Pool.QueryRow(ctx, `select r.id, r.state, r.issue_key, r.enrollment_id, r.decided_at, r.decided_by, r.decision_detail,
		(select g.id::text from grants g where g.request_id=r.id and g.revoked_at is null limit 1),
		case when r.ask_id is not null then 'dispatch://'||r.issue_key||'/ask/'||r.ask_id end
		from requests r where r.id=$1`, id).Scan(&r.ID, &r.State, &r.IssueKey, &enrollmentID, &r.DecidedAt, &r.DecidedBy, &r.Detail, &r.GrantID, &r.AskRef)
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

// Grant is a live grant's public summary, added in Task 11 for GET /v1/enrollments/self, which
// needs an enrollment's own live grants and has no existing read to reuse. It follows the same
// query pattern as OwnerOf and enrollment above.
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

// enrollment reads a live enrollment (not revoked, lease not lapsed); pgx.ErrNoRows otherwise.
func (m *Machine) enrollment(ctx context.Context, id string) (enrollmentRow, error) {
	var e enrollmentRow
	err := m.Store.Pool.QueryRow(ctx, `select id, kind, operator, approver_kind, approver_issue, runtime_id, subject from enrollments
		where id=$1 and revoked_at is null and lease_expires_at > now()`, id).
		Scan(&e.ID, &e.Kind, &e.Operator, &e.ApproverKind, &e.ApproverIssue, &e.RuntimeID, &e.Subject)
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
// returned as-is, with no new request row and no Dispatch ask, as long as the rules it was decided
// under are still current or the current rules still allow it (stillAllowed, the check Values
// makes). A caller that already holds a live grant for these exact names never re-asks a human
// who already approved it, and a rule tightened since then is never bypassed by reuse.
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
	return r, err == nil, err
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

func question(enr enrollmentRow, decisions []SecretDecision, reason string, lifetime time.Duration) string {
	names := make([]string, 0, len(decisions))
	for _, d := range decisions {
		if d.Decision == "approval" {
			names = append(names, d.Name)
		}
	}
	who := enr.Kind + " " + enr.RuntimeID
	if enr.Operator != nil {
		who = *enr.Operator + "'s " + who
	}
	q := fmt.Sprintf("Release %s to %s for up to %s? The agent's reason: %s", strings.Join(names, ", "), who, lifetime.Round(time.Minute), reason)
	if len(q) > 800 {
		q = q[:797] + "..."
	}
	return q
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

// endedAsk is a request that ended without an answer whose Dispatch ask may still be open.
type endedAsk struct{ id, askID, state string }

func (m *Machine) endedAsks(ctx context.Context) ([]endedAsk, error) {
	rows, err := m.Store.Pool.Query(ctx, `select id, ask_id, state from requests
		where state in ('cancelled','expired') and ask_id is not null and ask_retracted_at is null`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ended []endedAsk
	for rows.Next() {
		var e endedAsk
		if err := rows.Scan(&e.id, &e.askID, &e.state); err != nil {
			return nil, err
		}
		ended = append(ended, e)
	}
	return ended, rows.Err()
}

func (m *Machine) markAskRetracted(ctx context.Context, id string) error {
	_, err := m.Store.Pool.Exec(ctx, `update requests set ask_retracted_at=now() where id=$1`, id)
	return err
}
