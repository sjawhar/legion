// packages/envoy/internal/broker/requests/machine.go
// Package requests is the broker's state machine. A request row freezes, at creation, the
// enrollment, the resource set with each name's decision and delivery, the allowed approver,
// the rules version and the lifetime. Its only transitions are pending -> granted | denied |
// cancelled | expired; each is an UPDATE guarded by state='pending' inside one transaction that
// also writes the audit row, so a duplicate or late answer changes nothing.
package requests

import (
	"context"
	"errors"
	"fmt"
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
	ErrNotYours       = errors.New("this request or grant belongs to another session")
	ErrTerminal       = errors.New("request is already decided")
	ErrGrantNotLive   = errors.New("grant is expired, revoked, or its session ended")
	ErrMixedApprovers = errors.New("the requested secrets need different approvers; request them separately")
	ErrReasonTooLong  = errors.New("reason must be at most 400 characters")
	ErrIssueMismatch  = errors.New("this session's approvals belong to its own issue; the requested issue differs")
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
}

func (m *Machine) Create(ctx context.Context, enrollmentID string, names []string, reason, issue, sessionID string) (Request, error) {
	if len(reason) > 400 {
		return Request{}, ErrReasonTooLong
	}
	enr, err := m.enrollment(ctx, enrollmentID)
	if err != nil {
		return Request{}, err
	}
	set := m.Rules.Get()
	requester := rules.Requester{Kind: enr.Kind}
	if enr.Operator != nil {
		requester.Operator = *enr.Operator
	}
	issueKey := issue
	if enr.ApproverKind == "issue_assignee" && enr.ApproverIssue != nil {
		if issue != "" && issue != *enr.ApproverIssue {
			return Request{}, ErrIssueMismatch
		}
		issueKey = *enr.ApproverIssue
	}
	if issueKey == "" {
		issueKey, err = m.StandingIssue(ctx, requester.Operator)
		if err != nil {
			return Request{}, err
		}
	}
	if enr.ApproverKind == "issue_assignee" {
		if requester.IssueAssignee, err = m.IssueAssignee(ctx, issueKey); err != nil {
			return Request{}, err
		}
	}
	decisions := make([]SecretDecision, 0, len(names))
	approver := ""
	lifetime := m.MaxGrant
	needsApproval := false
	for _, name := range names {
		d, err := set.Evaluate(name, requester)
		if errors.Is(err, rules.ErrUnknownSecret) {
			d = rules.Decision{Outcome: "deny", Delivery: "inject", Source: ""}
		} else if err != nil {
			return Request{}, err
		}
		if d.Outcome == "approval" {
			if approver != "" && approver != d.Approver {
				return Request{}, ErrMixedApprovers
			}
			approver = d.Approver
			needsApproval = true
		}
		if d.MaxLifetime > 0 && d.MaxLifetime < lifetime {
			lifetime = d.MaxLifetime
		}
		decisions = append(decisions, SecretDecision{Name: name, Decision: d.Outcome, Delivery: d.Delivery, Source: d.Source})
	}
	state := "granted"
	for _, d := range decisions {
		if d.Decision == "deny" {
			state = "denied"
		}
	}
	if state != "denied" && needsApproval {
		state = "pending"
	}
	if existing, ok, err := m.coalesce(ctx, enrollmentID, names); err != nil {
		return Request{}, err
	} else if ok && state == "pending" {
		existing.Coalesced = true
		return existing, nil
	}
	req := Request{ID: uuid.NewString(), State: state, IssueKey: issueKey, Secrets: decisions}
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return Request{}, err
	}
	defer tx.Rollback(ctx)
	var pendingExpires *time.Time
	if state == "pending" {
		t := time.Now().Add(m.PendingTTL)
		pendingExpires = &t
	}
	_, err = tx.Exec(ctx, `insert into requests (id, enrollment_id, issue_key, reason, state, allowed_approver, rules_version, lifetime_seconds, pending_expires_at, session_id, decided_at, decision_detail)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, case when $5 in ('granted','denied') then now() end, case when $5='denied' then 'policy denies at least one requested secret' end)`,
		req.ID, enrollmentID, issueKey, reason, state, nullable(approver), set.Version, int(lifetime.Seconds()), pendingExpires, nullable(sessionID))
	if err != nil {
		return Request{}, err
	}
	for _, d := range decisions {
		if _, err := tx.Exec(ctx, `insert into request_secrets (request_id, name, decision, delivery, source) values ($1,$2,$3,$4,$5)`, req.ID, d.Name, d.Decision, d.Delivery, d.Source); err != nil {
			return Request{}, err
		}
	}
	if state == "granted" {
		grantID, err := insertGrant(ctx, tx, req.ID, enrollmentID, "", lifetime)
		if err != nil {
			return Request{}, err
		}
		req.GrantID = &grantID
	}
	if state == "pending" {
		question := question(enr, decisions, reason, lifetime)
		ask, err := m.Dispatch.CreateAsk(ctx, issueKey, question, []dispatch.Option{
			{Label: "Approve", Description: "Release these secrets to this one session until its end or the lifetime shown."},
			{Label: "Deny", Description: "Refuse; the agent is told the request was denied."},
		}, "med")
		if err != nil {
			return Request{}, fmt.Errorf("open Dispatch ask: %w", err)
		}
		if _, err := tx.Exec(ctx, `update requests set ask_id=$2, ask_edited_at=$3 where id=$1`, req.ID, ask.ID, ask.EditedAt); err != nil {
			return Request{}, err
		}
		ref := "dispatch://" + issueKey + "/ask/" + ask.ID
		req.AskRef = &ref
	}
	if err := audit(ctx, tx, "request.created", enrollmentID, req.ID, req.GrantID, "session:"+enrollmentID,
		fmt.Sprintf(`{"state":%q,"secrets":%q}`, state, strings.Join(names, ","))); err != nil {
		return Request{}, err
	}
	return req, tx.Commit(ctx)
}

// ApplyAnswer moves a pending request on a Dispatch ask read. It is idempotent: a request that is
// no longer pending is left alone and changed=false is returned.
func (m *Machine) ApplyAnswer(ctx context.Context, id string, ask dispatch.Ask) (bool, error) {
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var enrollmentID, state string
	var approver, storedAskID, recordedEdited *string
	var lifetime int
	err = tx.QueryRow(ctx, `select enrollment_id, state, allowed_approver, ask_id, ask_edited_at, lifetime_seconds from requests where id=$1 for update`, id).
		Scan(&enrollmentID, &state, &approver, &storedAskID, &recordedEdited, &lifetime)
	if err != nil {
		return false, err
	}
	if state != "pending" {
		return false, nil
	}
	if ask.State == "open" {
		return false, nil
	}
	next, detail := "denied", ""
	switch {
	case storedAskID == nil || *storedAskID != ask.ID:
		detail = "ask id does not match the request's own ask"
	case ask.State != "answered" || ask.Answer == nil:
		detail = "ask was " + ask.State + " without an approval"
	case !sameEdit(recordedEdited, ask.EditedAt):
		detail = "ask was edited after it was opened"
	case approver == nil || ask.Answer.User != *approver:
		detail = "answered by " + ask.Answer.User + ", not the allowed approver"
	case len(ask.Answer.Selected) != 1 || ask.Answer.Selected[0] != "Approve":
		detail = "approver chose " + strings.Join(ask.Answer.Selected, ",")
	default:
		next = "granted"
	}
	by := ""
	if ask.Answer != nil {
		by = ask.Answer.User
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
	if err := audit(ctx, tx, "request."+next, enrollmentID, id, grantID, "human:"+by, fmt.Sprintf(`{"detail":%q,"ask_id":%q}`, detail, ask.ID)); err != nil {
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
	if err := audit(ctx, tx, "request.cancelled", enrollmentID, id, nil, "session:"+enrollmentID, "{}"); err != nil {
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

// Values releases the inject-mode values of a live grant to its own enrollment. It re-checks the
// enrollment, the grant, and that every name is still an inject-mode secret in the current rules.
func (m *Machine) Values(ctx context.Context, grantID, enrollmentID string) (map[string]string, []string, time.Time, error) {
	var owner string
	var expires time.Time
	var live bool
	var requestID string
	err := m.Store.Pool.QueryRow(ctx, `select g.enrollment_id, g.expires_at, g.revoked_at is null and g.expires_at > now() and e.revoked_at is null and e.lease_expires_at > now(), g.request_id
		from grants g join enrollments e on e.id=g.enrollment_id where g.id=$1`, grantID).Scan(&owner, &expires, &live, &requestID)
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
	rows, err := m.Store.Pool.Query(ctx, `select name, source from request_secrets where request_id=$1 and decision <> 'deny'`, requestID)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	defer rows.Close()
	set := m.Rules.Get()
	values := map[string]string{}
	var proxyOnly, released []string
	for rows.Next() {
		var name, source string
		if err := rows.Scan(&name, &source); err != nil {
			return nil, nil, time.Time{}, err
		}
		current, ok := set.Secrets[name]
		if !ok {
			return nil, nil, time.Time{}, fmt.Errorf("%w: %s is no longer in the rules", ErrGrantNotLive, name)
		}
		if current.Delivery == "proxy" {
			proxyOnly = append(proxyOnly, name)
			continue
		}
		value, err := m.Secrets.Read(ctx, source)
		if err != nil {
			return nil, nil, time.Time{}, fmt.Errorf("read %s: %w", name, err)
		}
		values[name] = value
		released = append(released, name)
	}
	if _, err := m.Store.Pool.Exec(ctx, `insert into audit (kind, enrollment_id, request_id, grant_id, actor, detail) values ('grant.used',$1,$2,$3,$4, jsonb_build_object('names',$5::text))`,
		enrollmentID, requestID, grantID, "session:"+enrollmentID, strings.Join(released, ",")); err != nil {
		return nil, nil, time.Time{}, err
	}
	return values, proxyOnly, expires, nil
}

// RevokeGrant ends a grant. enrollmentID non-nil means the session itself is revoking and must own
// the grant; nil means a human (already authenticated by the caller) is revoking.
func (m *Machine) RevokeGrant(ctx context.Context, grantID, by string, enrollmentID *string) error {
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var owner, requestID string
	if err := tx.QueryRow(ctx, `select enrollment_id, request_id from grants where id=$1 for update`, grantID).Scan(&owner, &requestID); err != nil {
		return err
	}
	if enrollmentID != nil && *enrollmentID != owner {
		return ErrNotYours
	}
	if _, err := tx.Exec(ctx, `update grants set revoked_at=now(), revoked_by=$2 where id=$1 and revoked_at is null`, grantID, by); err != nil {
		return err
	}
	if err := audit(ctx, tx, "grant.revoked", owner, requestID, &grantID, by, "{}"); err != nil {
		return err
	}
	return tx.Commit(ctx)
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

func (m *Machine) enrollment(ctx context.Context, id string) (enrollmentRow, error) {
	var e enrollmentRow
	err := m.Store.Pool.QueryRow(ctx, `select id, kind, operator, approver_kind, approver_issue, runtime_id from enrollments where id=$1 and revoked_at is null`, id).
		Scan(&e.ID, &e.Kind, &e.Operator, &e.ApproverKind, &e.ApproverIssue, &e.RuntimeID)
	return e, err
}

func (m *Machine) coalesce(ctx context.Context, enrollmentID string, names []string) (Request, bool, error) {
	sorted := append([]string(nil), names...)
	sortStrings(sorted)
	rows, err := m.Store.Pool.Query(ctx, `select r.id, string_agg(s.name, ',' order by s.name) from requests r join request_secrets s on s.request_id=r.id
		where r.enrollment_id=$1 and r.state='pending' group by r.id`, enrollmentID)
	if err != nil {
		return Request{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, joined string
		if err := rows.Scan(&id, &joined); err != nil {
			return Request{}, false, err
		}
		if joined == strings.Join(sorted, ",") {
			rows.Close()
			r, err := m.Get(ctx, id)
			return r, err == nil, err
		}
	}
	return Request{}, false, rows.Err()
}

func insertGrant(ctx context.Context, tx pgx.Tx, requestID, enrollmentID, approver string, lifetime time.Duration) (string, error) {
	id := uuid.NewString()
	_, err := tx.Exec(ctx, `insert into grants (id, request_id, enrollment_id, approver, expires_at) values ($1,$2,$3,$4, now() + $5::interval)`,
		id, requestID, enrollmentID, nullable(approver), fmt.Sprintf("%d seconds", int(lifetime.Seconds())))
	return id, err
}

func audit(ctx context.Context, tx pgx.Tx, kind, enrollmentID, requestID string, grantID *string, actor, detail string) error {
	_, err := tx.Exec(ctx, `insert into audit (kind, enrollment_id, request_id, grant_id, actor, detail) values ($1,$2,$3,$4,$5,$6::jsonb)`, kind, enrollmentID, requestID, grantID, actor, detail)
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

func sameEdit(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func sortStrings(s []string) { sort.Strings(s) }
