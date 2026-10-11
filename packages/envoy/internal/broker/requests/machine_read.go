// packages/envoy/internal/broker/requests/machine_read.go
// machine_read.go holds requests.Machine's read-model methods: queries the API's GET handlers
// and the value-release path use, as opposed to machine_state.go's write/decision/transition
// methods.
package requests

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// Values releases the values of a live grant to its own enrollment, and when the grant expires:
// nil for a grant that lasts until its session ends. Every call re-checks the
// enrollment and the grant, re-verifies the grant's whole approval chain (VerifyChain), and — when
// the policy has changed since the grant's request was decided, or any granted name is a service's
// (anyServiceOwned: the policy's version does not cover BROKER_SERVICES' accounts) — that the
// current policy still allows the session, with the names its operator withheld from it, every
// granted name under the login that decided it (stillAllowed). Each value is read from the secret
// the request froze.
// It holds no pooled connection across a Secrets Manager read: the grant's names are read into
// memory before the first value is fetched.
func (m *Machine) Values(ctx context.Context, grantID, enrollmentID string) (map[string]string, *time.Time, error) {
	var owner, requestID, policyVersion, decidedBy string
	var expires *time.Time
	var live bool
	enr := enrollmentRow{ID: enrollmentID}
	err := m.Store.Pool.QueryRow(ctx, `select g.enrollment_id, g.expires_at, g.revoked_at is null and `+grantUnexpired("g")+` and e.revoked_at is null and e.lease_expires_at > now(),
		g.request_id, r.rules_version, coalesce(r.decided_by, ''), e.kind, e.operator, e.subject, c.service
		from grants g join enrollments e on e.id=g.enrollment_id join requests r on r.id=g.request_id
		left join launcher_credentials c on c.id = e.launcher_credential_id where g.id=$1`, grantID).
		Scan(&owner, &expires, &live, &requestID, &policyVersion, &decidedBy, &enr.Kind, &enr.Operator, &enr.Subject, &enr.Service)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrGrantNotLive
	}
	if err != nil {
		return nil, nil, err
	}
	if owner != enrollmentID {
		return nil, nil, ErrNotYours
	}
	if !live {
		return nil, nil, ErrGrantNotLive
	}
	if err := m.VerifyChain(ctx, grantID); err != nil {
		return nil, nil, err
	}
	granted, err := requestedSecrets(ctx, m.Store.Pool, requestID)
	if err != nil {
		return nil, nil, err
	}
	if set := m.Policy.Get(); policyVersion != set.Version || anyServiceOwned(set, granted) {
		requester, err := enr.requester(ctx, m.Store.Pool, m.ServiceAccounts)
		if err != nil {
			return nil, nil, err
		}
		for _, g := range granted {
			if err := stillAllowed(set, g.name, g.decision, decidedBy, requester); err != nil {
				return nil, nil, err
			}
		}
	}
	values := map[string]string{}
	released := []string{}
	for _, g := range granted {
		value, err := m.Secrets.Read(ctx, g.source)
		if errors.Is(err, secrets.ErrNotFound) {
			return nil, nil, fmt.Errorf("%w: %s", ErrSecretNotInStore, g.name)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", g.name, err)
		}
		values[g.name] = value
		released = append(released, g.name)
	}
	if _, err := m.Store.Pool.Exec(ctx, `insert into audit (kind, enrollment_id, request_id, grant_id, actor, detail) values ('grant.used',$1,$2,$3,$4,$5)`,
		enrollmentID, requestID, grantID, "session:"+enrollmentID, auditDetail{"names": released}); err != nil {
		return nil, nil, err
	}
	return values, expires, nil
}

// requestedSecret is one name a request asked for that the policy did not deny when the request
// was made: the secret it froze (source) and the decision the policy gave it then.
type requestedSecret struct{ name, source, decision string }

// requestedSecrets reads request requestID's names that the policy did not deny, in name order:
// what a granted request's grant releases (Values, reuseLiveGrant), and what a pending request
// waits to have approved (currentPolicyAdmits), since one denied name denies the whole request.
func requestedSecrets(ctx context.Context, q querier, requestID string) ([]requestedSecret, error) {
	rows, err := q.Query(ctx, `select name, source, decision from request_secrets where request_id=$1 and decision <> 'deny' order by name`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var requested []requestedSecret
	for rows.Next() {
		var g requestedSecret
		if err := rows.Scan(&g.name, &g.source, &g.decision); err != nil {
			return nil, err
		}
		requested = append(requested, g)
	}
	return requested, rows.Err()
}

// stillAllowed re-checks one granted name against a policy newer than the one its request was
// decided under. The policy must still serve the name and still let this requester have it: a
// deny refuses; so does a name granted automatically that the current policy wants approved,
// since no human ever approved it; and so does a name the current policy wants approved by
// someone decidedBy, the login that decided the request, is not (record.MayDecide). So an
// approval keeps its grant while the secret stays the approver's, or becomes shared, whose
// approver is anyone, or once the policy gives this requester the secret without asking; and a
// secret handed to a person stops every grant of it that person must approve and did not. A name
// withheld from the session that its request got automatically is judged as an approval by
// decidedBy too: the withhold ended every grant that got the name without asking (withhold), so a
// live one was approved after it, by someone the withheld name let approve it
// (currentPolicyAdmits), and an automatic grant, which no one decided, never passes.
func stillAllowed(set *policy.Set, name, frozenDecision, decidedBy string, requester policy.Requester) error {
	d, err := set.Evaluate(name, requester)
	if errors.Is(err, policy.ErrUnknownSecret) {
		return fmt.Errorf("%w: %s is no longer an agent secret", ErrGrantNotLive, name)
	}
	if err != nil {
		return err
	}
	switch {
	case d.Outcome == policy.Deny:
		return fmt.Errorf("%w: the current policy no longer allows %s", ErrGrantNotLive, name)
	case d.Outcome != policy.Approval:
		return nil
	case frozenDecision == policy.Automatic && !slices.Contains(requester.Withheld, name):
		return fmt.Errorf("%w: the current policy requires approval for %s", ErrGrantNotLive, name)
	case !record.MayDecide(record.KindAgentSecret, d.Approver, "", decidedBy):
		return fmt.Errorf("%w: %s now needs its owner's approval, which this grant does not have", ErrGrantNotLive, name)
	}
	return nil
}

// Get reads one request by id. Its Approver is the approver column of its credential-request
// record, the column PendingForApprover lists the record by, so the request names exactly the
// person whose list shows it; a request with no record has none.
func (m *Machine) Get(ctx context.Context, id string) (Request, error) {
	var r Request
	var enrollmentID string
	err := m.Store.Pool.QueryRow(ctx, `select r.id, r.state, r.enrollment_id, r.lifetime_seconds, r.decided_at, r.decided_by, r.decision_detail, r.record_id, cr.approver,
		(select g.id::text from grants g where g.request_id=r.id and g.revoked_at is null limit 1)
		from requests r left join credential_requests cr on cr.id = r.record_id where r.id=$1`, id).
		Scan(&r.ID, &r.State, &enrollmentID, &r.LifetimeSeconds, &r.DecidedAt, &r.DecidedBy, &r.Detail, &r.RecordID, &r.Approver, &r.GrantID)
	if err != nil {
		return Request{}, err
	}
	rows, err := m.Store.Pool.Query(ctx, `select name, decision from request_secrets where request_id=$1 order by name`, id)
	if err != nil {
		return Request{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var d SecretDecision
		if err := rows.Scan(&d.Name, &d.Decision); err != nil {
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
	// The grant's id, which the values and revoke routes take.
	ID string `json:"grant_id"`
	// The request it answered.
	RequestID string `json:"request_id"`
	// The login that approved it; null for a grant the policy gave automatically.
	Approver *string `json:"approver"`
	// When it expires; null for a grant that lasts until its session ends.
	ExpiresAt *time.Time `json:"expires_at"`
}

// LiveGrants lists this enrollment's own live (not revoked, not expired) grants, soonest to
// expire first and those that last until the session ends last.
func (m *Machine) LiveGrants(ctx context.Context, enrollmentID string) ([]Grant, error) {
	rows, err := m.Store.Pool.Query(ctx, `select id, request_id, approver, expires_at from grants g
		where enrollment_id=$1 and revoked_at is null and `+grantUnexpired("g")+` order by expires_at nulls last, created_at`, enrollmentID)
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

// grantUnexpired is the SQL predicate that grant alias's expiry has not passed: true for a grant
// with no expiry, which lasts until its session ends. Every read and state transition that asks
// whether a grant is live uses it beside revoked_at is null.
func grantUnexpired(alias string) string {
	return "(" + alias + ".expires_at is null or " + alias + ".expires_at > now())"
}

// insertGrant writes a grant that lasts lifetimeSeconds from now, or, when nil, until its session
// ends: a null expires_at, which endEnrollment's revoke alone ends.
func insertGrant(ctx context.Context, tx pgx.Tx, requestID, enrollmentID, approver string, lifetimeSeconds *int) (string, error) {
	id := uuid.NewString()
	_, err := tx.Exec(ctx, `insert into grants (id, request_id, enrollment_id, approver, expires_at)
		values ($1,$2,$3,$4, now() + make_interval(secs => $5))`,
		id, requestID, enrollmentID, nullable(approver), lifetimeSeconds)
	return id, err
}

type pendingRequestRow struct {
	ID       string
	Lifetime *int
	Names    []string
	// Legacy is a request written before migration 0013 (writtenBeforeMigration13), which asked
	// for no lifetime and recorded the release's fixed one.
	Legacy bool
}

// pendingRequest returns the oldest pending request for any name sorted names shares. Create holds
// every matching name's advisory lock before it asks, so a concurrent request cannot pass the
// lookup and open another record for that name.
func pendingRequest(ctx context.Context, q querier, enrollmentID string, sorted []string) (pendingRequestRow, error) {
	rows, err := q.Query(ctx, `select r.id, r.lifetime_seconds, array_agg(s.name order by s.name), `+writtenBeforeMigration13("r")+`
		from requests r join request_secrets s on s.request_id=r.id
		where r.enrollment_id=$1 and r.state='pending' and exists (
			select 1 from request_secrets matching where matching.request_id=r.id and matching.name = any($2)
		) group by r.id, r.lifetime_seconds, r.created_at order by min(r.created_at)`, enrollmentID, sorted)
	if err != nil {
		return pendingRequestRow{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return pendingRequestRow{}, rows.Err()
	}
	var pending pendingRequestRow
	if err := rows.Scan(&pending.ID, &pending.Lifetime, &pending.Names, &pending.Legacy); err != nil {
		return pendingRequestRow{}, err
	}
	return pending, rows.Err()
}

// joins answers whether a request asking for lifetime joins this pending request: the same
// lifetime, or, for a request asking for none, one written before migration 0013, which asked for
// none and recorded the release's fixed lifetime.
func (p pendingRequestRow) joins(lifetime *int) bool {
	return sameLifetime(p.Lifetime, lifetime) || (lifetime == nil && p.Legacy)
}

func sameLifetime(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// querier is what a read takes to run inside a transaction or straight on the pool.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// matchingRequest runs query (whose rows are a request id and that request's secret names, with
// $1 bound to enrollmentID and $2 to lifetime, the lifetime the request must have asked for) and
// answers the first request whose names are exactly sorted, or "" when none is. Names are compared
// as sets of whole strings, never as one joined string, so a name containing a separator cannot
// pass for two names.
func matchingRequest(ctx context.Context, q querier, query, enrollmentID string, lifetime *int, sorted []string) (string, error) {
	rows, err := q.Query(ctx, query, enrollmentID, lifetime)
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

// writtenBeforeMigration13 is the SQL predicate that request alias was written before migration
// 0013 (grants until the session ends) was applied, recording that release's fixed 43200-second
// lifetime although it asked for none. A request without a lifetime takes such a request's live
// grant, keeping its expiry, and joins it while it is pending, so a session running across the
// deploy neither asks its approver again nor is refused. The request's own time decides, since it
// is the row that recorded the old default: a request pending at the deploy and approved after it
// is covered too. No request written after the deploy matches, so a later --ttl 12h request is
// never taken for one, and the bridge ends on its own, with nothing to remove: such a request
// waits at most 12 hours and its grant lasts 12 more, so none matches a day after the deploy.
func writtenBeforeMigration13(alias string) string {
	return "(" + alias + ".lifetime_seconds = 43200 and " + alias +
		".created_at < (select applied_at from broker_schema_migrations where version = 13))"
}

// reuseLiveGrant answers Create's own "request (or reuse the live grant)" contract: an exact
// name-set match (never a subset or superset — the same matching rule coalescing uses for pending
// requests), asking for the same lifetime (or, asking for none, a request written before
// migration 0013: writtenBeforeMigration13), against a still-live grant (not revoked, not expired)
// under this enrollment is returned as-is, with no new request row and no new record, as long as
// the policy it was decided under is still current and no granted name is a service's
// (anyServiceOwned: the policy's version does not cover BROKER_SERVICES' accounts), or the current
// policy still allows it to the session (stillAllowed, the check Values makes), and its whole
// approval chain still verifies (VerifyChain, the same check Values makes). A caller that already
// holds a live grant for these exact names never re-asks a human who already approved it, a policy
// tightened since then is never bypassed by reuse, and neither is a chain that no longer verifies.
// No live grant holds a withheld name nobody approved: the withhold ended each one (withhold).
func (m *Machine) reuseLiveGrant(ctx context.Context, enr enrollmentRow, names []string, lifetime *int, set *policy.Set) (Request, bool, error) {
	id, err := matchingRequest(ctx, m.Store.Pool, `select r.id, array_agg(s.name) from requests r
		join request_secrets s on s.request_id=r.id
		join grants g on g.request_id=r.id
		where r.enrollment_id=$1 and (
			r.lifetime_seconds is not distinct from $2 or ($2::int is null and `+writtenBeforeMigration13("r")+`)
		) and r.state='granted' and g.revoked_at is null and `+grantUnexpired("g")+`
		group by r.id`, enr.ID, lifetime, sortedCopy(names))
	if err != nil || id == "" {
		return Request{}, false, err
	}
	var rulesVersion, decidedBy string
	if err := m.Store.Pool.QueryRow(ctx, `select rules_version, coalesce(decided_by, '') from requests where id=$1`, id).Scan(&rulesVersion, &decidedBy); err != nil {
		return Request{}, false, err
	}
	granted, err := requestedSecrets(ctx, m.Store.Pool, id)
	if err != nil {
		return Request{}, false, err
	}
	if rulesVersion != set.Version || anyServiceOwned(set, granted) {
		requester, err := enr.requester(ctx, m.Store.Pool, m.ServiceAccounts)
		if err != nil {
			return Request{}, false, err
		}
		for _, g := range granted {
			if err := stillAllowed(set, g.name, g.decision, decidedBy, requester); errors.Is(err, ErrGrantNotLive) {
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

// RecordKind reads a credential-request record's own kind ("agent_secret" or
// "launcher_credential") — the one column api.decideRecord needs before it knows which service's
// ApplyDecision a record id belongs to, and api.readRecord needs to decide what an already-read
// detail means. pgx.ErrNoRows means no such record.
func (m *Machine) RecordKind(ctx context.Context, recordID string) (string, error) {
	var kind string
	err := m.Store.Pool.QueryRow(ctx, `select kind from credential_requests where id=$1`, recordID).Scan(&kind)
	return kind, err
}

// Session is the two possibly-differing session ids a credential-request record can name, read
// independently (LEGION-587): Enrollment is the id the requesting session's own enrollment stated
// when it enrolled (enrollments.session_id — agentbox's --session-id, or the Legion daemon's
// worker claim session), Request is the id the request itself stated as an unsigned override in
// its body at Create time (requests.session_id). Either is empty when that id was never set; a
// machine login (kind launcher_credential, which has no request row and no requesting enrollment)
// always answers both empty. An id outlives its enrollment's revocation: enrollments.session_id is
// never cleared, only revoked_at is set, so the join below still finds it.
type Session struct {
	Request    string
	Enrollment string
}

// PendingSummary is one still-undecided credential-request record for GET /v1/pending: its id,
// kind, the plain identifiers its request object names (secret names for agent_secret, the single
// host for launcher_credential), when it was requested, and the session it names.
type PendingSummary struct {
	RecordID    string
	Kind        string
	Identifiers []string
	RequestedAt time.Time
	Session     Session
}

// pendingForApproverQuery is PendingForApprover's whole SQL text, pulled out so
// TestPendingForApproverAvoidsSequentialScans (same package) can PREPARE and EXPLAIN the exact
// statement the pool runs rather than a copy that could drift from it.
//
// The launcher_credential branch's NOT EXISTS spells its terminal-event predicate literally
// (event in ('approved','denied','expired','cancelled')) rather than binding
// record.TerminalEventNames() as a parameter, for the same reason machine.ExpirePending's ON
// CONFLICT target already does for the same index (credential_request_decision,
// store/migrations/0005_credential_requests.up.sql): Postgres can only recognize a partial index
// from a predicate it can prove implies the index's at plan time, which a bound parameter never
// does once the plan goes generic. The literal makes the index provably reachable in every plan
// shape; whether an unforced planner actually prefers it over hashing the far smaller matching set
// in one pass is then an ordinary cost comparison, which is why
// TestPendingForApproverAvoidsSequentialScans also sets enable_seqscan = off — that is what
// guarantees the index path, not the literal alone. A later change to the terminal event list must
// change this literal too, which TestPendingForApproverTerminalEventsLiteralMatchesTheList pins
// against record.TerminalEventNames().
//
// The two left joins read each row's session (LEGION-587): req on requests.record_id, the same
// column requests_record (0008_lookup_indexes.up.sql) already indexes for ReadRecord, and enr on
// credential_requests' own enrollment_id, its primary-key lookup into enrollments.
// TestPendingForApproverAvoidsSequentialScans checks only credential_requests and
// credential_request_events for a sequential scan; it pins nothing about how Postgres joins in
// req and enr (a hash join over requests and enrollments, measured locally at 5,000 enrollments —
// free to change with the planner's own cost estimates, and not this test's concern).
const pendingForApproverQuery = `select cr.id, cr.kind, cr.body, cr.created_at, coalesce(req.session_id, ''), coalesce(enr.session_id, '')
	from credential_requests cr
	left join requests req on req.record_id = cr.id
	left join enrollments enr on enr.id = cr.enrollment_id
	where (
		cr.approver in ($1, $2) and cr.kind='agent_secret' and cr.id in (select r.record_id from requests r where r.state='pending')
		or cr.approver in ($1, $2) and cr.kind='launcher_credential' and not exists (
			select 1 from credential_request_events ev where ev.record_id=cr.id and ev.event in ('approved','denied','expired','cancelled'))
	) order by cr.created_at desc`

// PendingForApprover lists every still-pending credential-request record — of either kind — that
// approver may decide, newest first: GET /v1/pending's exact contract. That is every record naming
// approver, and every record whose approver is record.AnyoneApprover: a request for a shared
// human-tier secret, and a service's machine login. A service's login a broker from before that
// rule opened names a person instead and is listed for that person alone, though anyone who types
// its code may decide it (record.MayDecide). The rule for
// pending, which ReadRecord applies too: an agent_secret record is pending while its request is.
// Every writer moves the request out of 'pending' together with the record's terminal event
// (store.EndPendingRequests, ApplyDecision), but a request an ended enrollment cancelled before
// endEnrollment wrote that event carries none, so the request row is the truth. A machine login
// has no request row and is pending while it carries no terminal decision event, matching
// credential_request_decision's own partial index — see pendingForApproverQuery's own comment for
// why that match requires a literal predicate, not record.TerminalEventNames() bound as a
// parameter. The approver scan itself is credential_requests_approver
// (store/migrations/0011_credential_requests_approver.up.sql): with 300,000 historical rows and
// no index, this was one sequential scan of the whole table on every 15 s poll tick of every open
// Dispatch tab (336 ms; LEGION-575's review).
func (m *Machine) PendingForApprover(ctx context.Context, approver string) ([]PendingSummary, error) {
	rows, err := m.Store.Pool.Query(ctx, pendingForApproverQuery, record.CanonicalLogin(approver), record.AnyoneApprover)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingSummary
	for rows.Next() {
		var id, kind, canonical, requestSession, enrollmentSession string
		var createdAt time.Time
		if err := rows.Scan(&id, &kind, &canonical, &createdAt, &requestSession, &enrollmentSession); err != nil {
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
		out = append(out, PendingSummary{
			RecordID: id, Kind: kind, Identifiers: identifiers, RequestedAt: createdAt,
			Session: Session{Request: requestSession, Enrollment: enrollmentSession},
		})
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
	LifetimeSeconds *int
	RulesVersion    string
	ExpiresAt       time.Time
	RequestedAt     time.Time
	Decided         *RecordDecision
	Session         Session
}

// ReadRecord reads a credential-request record's full detail by id, for GET
// /v1/credential-requests/{id} and, reused verbatim, POST /v1/machine-logins/lookup.
// pgx.ErrNoRows means no such record.
func (m *Machine) ReadRecord(ctx context.Context, recordID string) (RecordDetail, error) {
	var canonical, approver, kind, requestSession, enrollmentSession string
	var createdAt, expiresAt time.Time
	if err := m.Store.Pool.QueryRow(ctx, `select cr.body, cr.approver, cr.kind, cr.created_at, cr.expires_at,
			coalesce(req.session_id, ''), coalesce(enr.session_id, '')
		from credential_requests cr
		left join requests req on req.record_id = cr.id
		left join enrollments enr on enr.id = cr.enrollment_id
		where cr.id=$1`, recordID).
		Scan(&canonical, &approver, &kind, &createdAt, &expiresAt, &requestSession, &enrollmentSession); err != nil {
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
	agentSecret := kind == record.KindAgentSecret
	var enr *record.Enrollment
	if agentSecret {
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
		Session: Session{Request: requestSession, Enrollment: enrollmentSession},
	}
	// Whether the record is still pending follows PendingForApprover's rule: an agent_secret record
	// by its request's state, a machine login by its terminal event. A decided record's terminal
	// event names the decision; a request an ended enrollment cancelled before endEnrollment wrote
	// that event has none, and its request row stands in for it.
	var event, credID string
	var decidedAt time.Time
	if agentSecret {
		err = m.Store.Pool.QueryRow(ctx, `select coalesce(ev.event, r.state), coalesce(ev.at, r.decided_at), coalesce(ev.credential_id, '')
			from requests r left join credential_request_events ev on ev.record_id=r.record_id and ev.event = any($2)
			where r.record_id=$1 and r.state <> 'pending'`, recordID, record.TerminalEventNames()).Scan(&event, &decidedAt, &credID)
	} else {
		err = m.Store.Pool.QueryRow(ctx, `select event, at, coalesce(credential_id,'') from credential_request_events
			where record_id=$1 and event = any($2)`, recordID, record.TerminalEventNames()).Scan(&event, &decidedAt, &credID)
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

// ApproverGrant is one live grant a person may revoke, for GET /v1/grants?approver=<login>.
// Granted is policy.Automatic for a grant the policy gave the session without asking, which has no
// RecordID and an empty Approver, and policy.Approval for one a person approved, whose RecordID is
// the record the approval rests on and whose Approver is that person's login, which for an
// operator's own list can be another login. ExpiresAt is nil for a grant that lasts until its
// session ends.
type ApproverGrant struct {
	GrantID    string
	Granted    string
	RecordID   *string
	Enrollment record.Enrollment
	Names      []string
	Approver   string
	ExpiresAt  *time.Time
	CreatedAt  time.Time
}

// GrantsForApprover lists every live grant approver may revoke, newest first: each grant of a
// session approver operates, approved or automatic, and each grant approver approved on anyone's
// session.
func (m *Machine) GrantsForApprover(ctx context.Context, approver string) ([]ApproverGrant, error) {
	login := record.CanonicalLogin(approver)
	rows, err := m.Store.Pool.Query(ctx, `select g.id, r.record_id, e.kind, e.runtime_id, coalesce(e.operator,''), e.slot, coalesce(g.approver,''), g.expires_at, g.created_at,
		coalesce((select array_agg(rs.name order by rs.name) from request_secrets rs where rs.request_id=r.id and rs.decision<>'deny'), '{}')
		from grants g
		join requests r on r.id=g.request_id
		join enrollments e on e.id=g.enrollment_id
		where g.revoked_at is null and `+grantUnexpired("g")+`
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
		g.Granted = policy.Approval
		if g.RecordID == nil {
			g.Granted = policy.Automatic
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
