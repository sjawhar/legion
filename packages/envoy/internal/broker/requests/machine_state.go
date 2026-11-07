// packages/envoy/internal/broker/requests/machine_state.go
// Package requests is the broker's state machine for agent_secret credential requests. A request
// row freezes, at creation, the enrollment, the resource set with each name's decision and the
// secret it reads, the allowed approver, the policy version and the lifetime its request object
// signed, else the 12-hour default (defaultLifetime): a grant lasts that long, or until its session
// ends when the request names none and every name is tagged default-grant=session.
// Its only transitions are pending -> granted | denied | cancelled | expired; each is an UPDATE
// guarded by state='pending' inside one transaction that also writes the audit row, so a duplicate
// or late decision changes nothing. A request that needs approval also freezes an append-only
// credential-request record (internal/broker/record): the requester's signed request object plus
// the broker's decision fields. The record is decided by a person signed in to the broker's own
// approval page (internal/broker/approval), whose session names the login that decides it: its
// approver's, or anyone's for a record whose approver is record.AnyoneApprover. Every release of a
// grant it produced re-verifies the whole chain — record hash, requester signature, one approval
// by a login the record allows — so a row written by anyone but the broker releases nothing.
//
// One column of a request row is not frozen: its enrollment_id, and the same holds for a grant's
// and a withheld name's. A session that enrols on a box it has not run on before takes what the
// enrollment it is moving off holds — its grants, its requests and the names its operator withheld
// from it — because an approval a person gives hours later, and the grant that comes of it, belong
// to the session rather than to the box that asked (enroll.Create's adoption,
// dispatch://INFRA-1167). So every read and every transition here is keyed by the enrollment the
// row itself names now, never by the one the append-only credential_requests record froze: that is
// what gets a moved session its own grant back, lets it cancel its own request, and still refuses
// it another session's. moved_session_test.go drives each of those surfaces across a move.
package requests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/ratelimit"
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
	ErrSecretNotInStore  = errors.New("secret is not in the secrets store")
	ErrMixedApprovers    = errors.New("the requested secrets need different approvers; request them separately")
	// ErrGrantRequiresLifetime: a request names a secret whose max-grant tag requires a signed
	// lifetime no longer than that tag. Its returned error names the secret and the limit.
	ErrGrantRequiresLifetime = errors.New("a requested secret requires a shorter grant lifetime")
	// ErrMintFailed: a kind secret's minter could not make a token from the stored value, such as
	// a Google login that needs a new sign-in or a GitHub App GitHub refuses. Its returned error
	// carries the minter's own message, which never holds the stored value or a token.
	ErrMintFailed = errors.New("the broker could not mint a token from this secret")
	// ErrNarrowingNotApplicable: a release asked to narrow a GitHub App token, and the grant holds
	// no github-app secret, so nothing it releases could be narrowed; it releases nothing rather
	// than every value whole.
	ErrNarrowingNotApplicable = errors.New("this grant holds no github-app secret to narrow")
	// ErrNarrowingOwnerNotListed: a release asked to narrow a GitHub App token to an account that a
	// github-app secret of the grant does not list in its github-owners tag; it releases nothing,
	// and no minter runs, since the caller's input, not the App, is at fault.
	ErrNarrowingOwnerNotListed = errors.New("the narrowing names an account a github-app secret of this grant does not serve")
	// ErrRequestPending means this session already has a person deciding one of the requested
	// names. Its returned error names that pending request.
	ErrRequestPending = errors.New("a request for one or more of these secrets is already pending")
	// ErrSessionIDInvalid: the request's own session_id override (requests.session_id — an
	// unsigned claim any enrolled process may send, never verified against a signature) is too
	// long or carries a whitespace or control character, refused before it is ever stored or
	// forwarded to Envoy's wake (LEGION-587).
	ErrSessionIDInvalid = errors.New("session id must be at most 128 characters with no whitespace or control character")
)

// maxSessionIDLength bounds the request body's session_id override; see ErrSessionIDInvalid.
const maxSessionIDLength = 128

// validSessionID reports whether sessionID is acceptable to store: empty (unset) or at most
// maxSessionIDLength runes with no whitespace or control character.
func validSessionID(sessionID string) bool {
	if utf8.RuneCountInString(sessionID) > maxSessionIDLength {
		return false
	}
	for _, r := range sessionID {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// jtiRetentionMargin is how long past a request object's expiry its jti is remembered, mirroring
// proof.Verifier's own retention margin.
const jtiRetentionMargin = time.Minute

// DefaultMissRereads bounds the policy rereads one enrollment's unknown-name requests trigger:
// a session inventing names costs at most Burst DescribeSecret calls, refilled one per Every.
// The budget is per process, so two broker pods make two of it for the same enrollment. That is
// left as it is because this bounds load rather than authority - a reread only moves a pod
// toward the secret store's own truth, and grants nothing - so the cost of sharing it across
// pods, a store read's worth of state in Postgres on every miss, buys nothing an operator needs.
var DefaultMissRereads = ratelimit.Limit{Every: 10 * time.Second, Burst: 10}

// SecretDecision is how the policy decided one name of a request.
type SecretDecision struct {
	// The secret's name.
	Name string `json:"name"`
	// "automatic", "approval" or "deny".
	Decision string `json:"decision"`
	Source   string `json:"-"`
}

// Request is one agent_secret request as Create and Get answer it. Approver is the approver its
// credential-request record names (a person's canonical login, or record.AnyoneApprover), read
// from the record rather than the current policy, since the record is what the approver's list
// shows; it is nil when the request has no record, because the policy decided it at once.
// LifetimeSeconds is the lifetime its grant lasts: the one its signed request object named, else
// the 12-hour default; nil when it named none and every name is tagged default-grant=session.
type Request struct {
	ID              string           `json:"request_id"`
	State           string           `json:"state"`
	GrantID         *string          `json:"grant_id"`
	RecordID        *string          `json:"record_id"`
	Approver        *string          `json:"approver"`
	Secrets         []SecretDecision `json:"secrets"`
	LifetimeSeconds *int             `json:"lifetime_seconds"`
	DecidedAt       *time.Time       `json:"decided_at"`
	DecidedBy       *string          `json:"decided_by"`
	Detail          *string          `json:"detail"`
	Coalesced       bool             `json:"coalesced,omitempty"`
}

// Decision is ApplyDecision's result: the request's new state and, when granted, the new grant's
// id (empty when the record was denied, or approved but the requesting enrollment had died).
type Decision struct {
	RequestID string
	State     string
	GrantID   string
	// Detail is what the decision recorded beside the request's new state, empty when it
	// recorded none. An approval ends denied with one of two: the requesting enrollment was no
	// longer live, or the secret's requesters tag no longer lists the session
	// ("<NAME>: requester-not-listed"). The approval page answers the approver with it, so the
	// person who pressed approve reads why the broker denied it instead.
	Detail string
}

// Minter makes one release of a kind secret from its stored value and the policy's facts about it
// (secrets.GitHubApp.Mint, secrets.GoogleLogin.Mint). Its error never holds the stored value or a
// token.
type Minter func(ctx context.Context, req secrets.MintRequest) (string, error)

type Machine struct {
	Store      *store.Store
	Policy     *policy.Current
	Secrets    secrets.Reader
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
	// MissRereads bounds, per enrollment, the single-name policy rereads Create makes for names the
	// live policy does not serve (rereadMissing); nil means DefaultMissRereads.
	MissRereads *ratelimit.Limit
	// ServicesByAccount are the registered services' accounts (BROKER_SERVICES): for each account,
	// the service a pod whose projected token proved it runs as that account proves, when its
	// launcher's service is that one (requester).
	ServicesByAccount map[string]string
	// VMServicesByAccount is BROKER_VM_SERVICES, keyed by a Google service account's email: the
	// service a machine whose Google identity token the exchange took proves, when its launcher
	// credential's service is that one (requester). It is read on every call, so an account
	// dropped from the setting stops its machines' service secrets at their next one.
	VMServicesByAccount map[string]string
	// Minters maps each policy kind (policy.Kinds) to its Minter. A release never returns the
	// stored value of a kind secret.
	Minters map[string]Minter
	// missRereads is MissRereads' per-enrollment buckets, built on first use (missRereadLimiter).
	missRereads     *ratelimit.Keyed
	missRereadsOnce sync.Once
	// testAdoptionHook, when set, runs in applyDecisionAttempt between the read that locks the
	// requesting enrollment and the read that locks the request. It exists only so a test can
	// deterministically move the request to another enrollment in that window, which is where an
	// adoption's commit lands; no production caller sets it.
	testAdoptionHook func()
}

// missRereadLimiter is the per-enrollment buckets MissRereads names, built on the first miss.
func (m *Machine) missRereadLimiter() *ratelimit.Keyed {
	m.missRereadsOnce.Do(func() {
		limit := DefaultMissRereads
		if m.MissRereads != nil {
			limit = *m.MissRereads
		}
		m.missRereads = ratelimit.NewKeyed(limit)
	})
	return m.missRereads
}

// NewChainVerifier builds the record.ChainVerifier VerifyChain uses, scoped to agent_secret
// records.
func NewChainVerifier(st *store.Store, audience string, skew time.Duration) *record.ChainVerifier {
	return st.ChainVerifier(record.KindAgentSecret, audience, skew)
}

type enrollmentRow struct {
	ID, Kind, Thumbprint string
	// Operator is the person the session acts for, nil for a pod and for a machine's own
	// session; Service is the service of the launcher credential that enrolled it, nil for a
	// person's machine; Subject is a pod's verified service account or the credential subject a
	// machine's session recorded, nil for every other session; Host is that credential's host.
	Operator, Service, Subject, Host *string
	RuntimeID, Slot                  string
	// SessionID is the Envoy session this enrollment recorded, empty for one that names none.
	// It is read with the rest of the row, rather than looked up when a decision is logged, so
	// the grant-log line a committed transaction writes names the session it decided for
	// without a second query (grantLogEvent).
	SessionID string
}

// requester is enrollment e as the policy sees it, read through q: the person it acts for (none
// for a pod, and none for a machine's own session), its service, and the names that person
// withheld from it (RevokeByApprover's withhold). Its service is its launcher credential's only
// when the account the enrollment recorded still proves it: `BROKER_SERVICES` for a pod's
// verified service account, `BROKER_VM_SERVICES` for the Google account a machine's credential
// subject names (exchange.ReadSubject). A machine login's service name is the machine's own
// claim, which anyone signed in to Dispatch may approve, so the account is what proves the
// service. Any other session has none.
// It is read on every request, decision and release, so an account dropped from the
// configuration stops an enrolled pod's or machine's service secrets at its next call, not at
// its lease's end. A caller that decides
// for the session reads it after locking the session's row, which RevokeByApprover holds while it
// withholds, so a withhold either committed before the read or waits for the caller.
// The withheld names are read by the enrollment's own id, and an adoption carries a session's
// withheld_secrets rows to the enrollment it moves to, so these are the names withheld from the
// session rather than from a box it has left.
func (m *Machine) requester(ctx context.Context, q querier, e enrollmentRow) (policy.Requester, error) {
	rows, err := q.Query(ctx, `select name from withheld_secrets where enrollment_id=$1`, e.ID)
	if err != nil {
		return policy.Requester{}, err
	}
	withheld, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return policy.Requester{}, err
	}
	var service string
	if e.Subject != nil && e.Service != nil {
		switch e.Kind {
		case "pod":
			if bound, ok := m.ServicesByAccount[*e.Subject]; ok && bound == *e.Service {
				service = *e.Service
			}
		case "session":
			if read := exchange.ReadSubject(*e.Subject); read.Kind == exchange.SubjectMachine {
				if bound, ok := m.VMServicesByAccount[read.Account]; ok && bound == *e.Service {
					service = *e.Service
				}
			}
		}
	}
	return policy.Requester{Operator: deref(e.Operator), Service: service, Withheld: withheld}, nil
}

// Create verifies the request object (record.VerifyRequestObject, jti replay through the Replay
// seam), requires iss to be this enrollment's own key and no login_hint (session requests never
// name their own approver — that's the policy's job), rereads each name the live policy does not
// serve (rereadMissing), and hands back a live grant for exactly these names when one may be
// reused (reuseLiveGrant). Otherwise it decides and writes the request in one transaction: it
// takes the advisory locks that serialize pending requests for each requested name in one
// enrollment (lockPendingNames), then locks the enrollment live (lockLiveEnrollment), so no
// request or grant lands on an enrollment that ended after Create first read it (such an
// enrollment is pgx.ErrNoRows, as one that had ended before), and no withhold
// (RevokeByApprover, which takes the same row) lands between the read of what is withheld from
// the session and the write. It then evaluates the policy for the session and writes a granted or
// denied request, with a granted one's grant, or a pending request and its credential-request
// record. A second pending request that overlaps one of its names is refused, so an untrusted
// session cannot flood an approver's Inbox by varying --ttl.
func (m *Machine) Create(ctx context.Context, enrollmentID, compactRequest, sessionID string) (Request, error) {
	if !validSessionID(sessionID) {
		return Request{}, ErrSessionIDInvalid
	}
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
	if len(obj.Details) == 0 || obj.Details[0].Type != record.KindAgentSecret {
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

	set, err := m.rereadMissing(ctx, enrollmentID, names, m.Policy.Get())
	if err != nil {
		return Request{}, err
	}
	lifetime := defaultLifetime(set, names, obj.LifetimeSeconds)
	for _, name := range names {
		if limit, over := overMaxGrant(set, name, lifetime); over {
			return Request{}, fmt.Errorf("%w: %s allows at most %s; ask with agent-secrets --ttl %s", ErrGrantRequiresLifetime, name, limit, limit)
		}
	}
	if existing, ok, err := m.reuseLiveGrant(ctx, enr, names, lifetime, set); err != nil {
		return Request{}, err
	} else if ok {
		return existing, nil
	}
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return Request{}, err
	}
	defer tx.Rollback(ctx)
	sorted := sortedCopy(names)
	if err := lockPendingNames(ctx, tx, enrollmentID, sorted); err != nil {
		return Request{}, err
	}
	if err := lockLiveEnrollment(ctx, tx, enrollmentID); err != nil {
		return Request{}, err
	}
	requester, err := m.requester(ctx, tx, enr)
	if err != nil {
		return Request{}, err
	}
	e, err := evaluate(set, names, requester)
	if err != nil {
		return Request{}, err
	}
	if e.state == "pending" {
		pending, err := pendingRequest(ctx, tx, enrollmentID, sorted)
		if err != nil {
			return Request{}, err
		}
		if pending.ID != "" {
			if slices.Equal(pending.Names, sorted) && (pending.joins(lifetime) || (obj.LifetimeSeconds == nil && pending.Lifetime == nil)) {
				if err := tx.Commit(ctx); err != nil {
					return Request{}, err
				}
				existing, err := m.Get(ctx, pending.ID)
				existing.Coalesced = true
				return existing, err
			}
			return Request{}, fmt.Errorf("%w: request %s is still pending", ErrRequestPending, pending.ID)
		}
	}
	r := newRequest{
		id: uuid.NewString(), enrollmentID: enrollmentID, reason: obj.Reason, state: e.state,
		approver: e.approver, rulesVersion: set.Version, sessionID: sessionID, lifetime: lifetime, decisions: e.decisions,
		requester: requester, denials: e.denials,
	}
	// Both branches end the same way — commit, then the one grant-log line for the request.created
	// audit row insertRequest wrote — so only how req is produced differs between them.
	var req Request
	if e.state != "pending" {
		req = Request{ID: r.id, State: r.state, Secrets: r.decisions, LifetimeSeconds: r.lifetime, Detail: nullable(r.decisionDetail())}
		if err := m.insertRequest(ctx, tx, r); err != nil {
			return Request{}, err
		}
		if r.state == "granted" {
			grantID, err := insertGrant(ctx, tx, r.id, enrollmentID, "", r.lifetime)
			if err != nil {
				return Request{}, err
			}
			req.GrantID = &grantID
		}
	} else {
		r.pendingExpiresAt = time.Now().Add(m.PendingTTL)
		pending, err := m.createPending(ctx, tx, enr, r, obj)
		if err != nil {
			return Request{}, err
		}
		req = pending
	}
	if err := tx.Commit(ctx); err != nil {
		return Request{}, err
	}
	store.LogGrantEvent(r.createdRequestLogEvent(enr))
	return req, nil
}

// rereadMissing rereads each requested name the live set does not serve (the spec's miss path: a
// secret created seconds ago is not in the listing yet), bounded per enrollment by MissRereads,
// and answers the set to evaluate against. A name no secret can carry (policy.Current.CheckName:
// a free-text identifier, or one past Secrets Manager's name limit) stays unknown before it costs
// a token or a line. A failed reread logs LoadFailedMessage and the name stays unknown — the
// caller sees UNKNOWN_SECRET exactly as before the miss path existed. A reread that fails once ctx
// has ended (the caller went away, or its deadline passed) is the request's failure, not Secrets
// Manager's: it logs nothing and answers ctx's error, so the alarm on LoadFailedMessage never
// counts a client that left. It runs before Create's transaction, so no Secrets Manager call
// holds a row lock.
func (m *Machine) rereadMissing(ctx context.Context, enrollmentID string, names []string, set *policy.Set) (*policy.Set, error) {
	for _, name := range names {
		if _, ok := set.Secrets[name]; ok {
			continue
		}
		if m.Policy.CheckName(name) != nil {
			continue
		}
		if !m.missRereadLimiter().Allow(enrollmentID) {
			return set, nil
		}
		if _, err := m.Policy.RefreshOne(ctx, name); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			slog.Error(policy.LoadFailedMessage, "name", name, "error", err)
			continue
		}
		set = m.Policy.Get()
	}
	return set, nil
}

// DefaultGrantLifetime is how long a grant lasts when its request names no lifetime, unless every
// name it grants carries default-grant=session (policy.TagDefaultGrant), or one of them is human
// tier (DefaultHumanGrantLifetime).
const DefaultGrantLifetime = 12 * time.Hour

// DefaultHumanGrantLifetime is how long a grant lasts when its request names no lifetime and any
// name it grants is human tier. A person approves such a request by the action its reason names,
// and the broker enforces only the duration, so a long default hands every later action of that
// session the same approval. Fifteen minutes is the window in which the broker releases and reuses
// that credential, not a count of actions: a release inside it is still unasked, and a value
// already in a process is beyond the broker. A session that needs longer says so with --ttl, which
// the approver reads on the card.
const DefaultHumanGrantLifetime = 15 * time.Minute

// defaultLifetime is the lifetime a request's grant lasts: the one it signed, else nil (until its
// session ends) when every name opts out with default-grant=session, else
// DefaultHumanGrantLifetime when any name is human tier, else DefaultGrantLifetime. A name the set
// does not serve (a malformed tag among them) keeps the 12-hour default, so it never yields a
// session-long grant; its tier is unknown, so it never shortens one either.
func defaultLifetime(set *policy.Set, names []string, signed *int) *int {
	if signed != nil {
		return signed
	}
	fallback, optedOut := DefaultGrantLifetime, len(names) > 0
	for _, name := range names {
		secret, ok := set.Secrets[name]
		if !ok || !secret.SessionDefault {
			optedOut = false
		}
		if ok && secret.Tier == policy.TierHuman {
			fallback = DefaultHumanGrantLifetime
		}
	}
	if optedOut {
		return nil
	}
	seconds := int(fallback / time.Second)
	return &seconds
}

// overMaxGrant reports whether a request signing lifetime (nil: until its session ends) asks for
// longer than name's max-grant tag allows, and that bound as a --ttl the CLI accepts. A name with
// no max-grant, or one the set does not serve (left to evaluate, which refuses it as unknown), is
// never over.
func overMaxGrant(set *policy.Set, name string, lifetime *int) (string, bool) {
	maxGrant := set.Secrets[name].MaxGrant
	if maxGrant == 0 || (lifetime != nil && time.Duration(*lifetime)*time.Second <= maxGrant) {
		return "", false
	}
	return formatGrantDuration(maxGrant), true
}

// formatGrantDuration is a max-grant as a --ttl the CLI accepts: whole seconds only
// (ttlSeconds), so a tag such as 1.5s is suggested as 1s, the longest whole lifetime it allows.
func formatGrantDuration(duration time.Duration) string {
	duration = duration.Truncate(time.Second)
	switch {
	case duration%time.Hour == 0:
		return strconv.FormatInt(int64(duration/time.Hour), 10) + "h"
	case duration%time.Minute == 0:
		return strconv.FormatInt(int64(duration/time.Minute), 10) + "m"
	default:
		return duration.String()
	}
}

// evaluation is one pass of the policy over a request's names. approver is set when a name needs
// approval, and then names the one approver every such name has. denials are the denied names
// the policy named a reason for, in the request's own name order: the request row records them
// as its decision_detail (newRequest.decisionDetail), so a refused session is told which secret
// refused it and why instead of only that something did.
type evaluation struct {
	decisions []SecretDecision
	denials   []denial
	state     string // granted, denied or pending
	approver  string
}

// denial is one denied name of a request and the rule behind the deny (policy.Decision.Reason).
// A deny the policy cannot name a reason for makes no denial at all, since there is nothing to
// tell the session beyond the refusal itself.
type denial struct{ name, reason string }

// evaluate is the policy's answer for each of names to requester.
func evaluate(set *policy.Set, names []string, requester policy.Requester) (evaluation, error) {
	e := evaluation{decisions: make([]SecretDecision, 0, len(names)), state: "granted"}
	denied := false
	for _, name := range names {
		d, err := set.Evaluate(name, requester)
		if err != nil {
			return evaluation{}, err
		}
		switch {
		case d.Outcome == policy.Deny:
			denied = true
			if d.Reason != "" {
				e.denials = append(e.denials, denial{name: name, reason: d.Reason})
			}
		case d.Outcome == policy.Approval:
			if e.approver != "" && e.approver != d.Approver {
				return evaluation{}, ErrMixedApprovers
			}
			e.approver = d.Approver
		}
		e.decisions = append(e.decisions, SecretDecision{Name: name, Decision: d.Outcome, Source: d.Source})
	}
	switch {
	case denied:
		e.state = "denied"
	case e.approver != "":
		e.state = "pending"
	}
	return e, nil
}

// newRequest is one request row as Create writes it. lifetime is the seconds its grant lasts, nil
// for a grant that lasts until its session ends.
type newRequest struct {
	id, enrollmentID, reason, state, approver, rulesVersion, sessionID, recordID string
	lifetime                                                                     *int
	decisions                                                                    []SecretDecision
	// denials are the denied names the policy named a reason for (evaluation.denials), which
	// decisionDetail turns into the row's decision_detail.
	denials []denial
	// requester is the session as the policy saw it when this very request was decided
	// (Machine.requester), so the request.created audit row names the person and the service the
	// decision was made under rather than whoever a second read would answer later.
	requester policy.Requester
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

// decisionDetail is the row's decision_detail, which Create also answers as the request's own
// Detail: for a denied request, each denied name the policy named a reason for as
// "<NAME>: <reason>", several joined with "; " in the request's own name order, and the fixed
// text of every deny the policy cannot name when it named none, so such a refusal reads as it
// always has. Every other state records none: only a denial has something to explain.
func (r newRequest) decisionDetail() string {
	if r.state != "denied" {
		return ""
	}
	if len(r.denials) == 0 {
		return "policy denies at least one requested secret"
	}
	parts := make([]string, len(r.denials))
	for i, d := range r.denials {
		parts[i] = d.name + ": " + d.reason
	}
	return strings.Join(parts, "; ")
}

// auditDetail is the request.created audit row's detail: the request's state, the names it asked
// for, and who asked — person for the operator whose session it is, service for the one its pod
// or machine proved (newRequest.requester, the very requester the request was decided under). A
// key whose value is empty is left out rather than written as "", since the grant-log route reads
// service off this row and a request no service made should answer nothing there rather than an
// empty service name.
func (r newRequest) auditDetail() auditDetail {
	detail := auditDetail{"state": r.state, "secrets": r.names()}
	if r.requester.Operator != "" {
		detail["person"] = r.requester.Operator
	}
	if r.requester.Service != "" {
		detail["service"] = r.requester.Service
	}
	return detail
}

// createdRequestLogEvent is the request.created audit row insertRequest wrote for this request,
// as the grant-log line Create logs once its transaction has committed. Every field it
// carries is one the decision itself was made with: the requester the policy saw, the request's
// names and their decisions in name order — the order store.ReadGrantLog answers them in, so the
// line and the route's event read the same way round — and the audit row's own detail. It is
// named for the row it carries, as endedRequestLogEvent is, so a reader at the call site tells
// it from the package-level grantLogEvent it builds on.
func (r newRequest) createdRequestLogEvent(enr enrollmentRow) store.GrantLogEvent {
	ev := grantLogEvent("request.created", "session:"+r.enrollmentID, enr)
	ev.RequestID = r.id
	ev.Service = r.requester.Service
	ev.Secrets = make([]store.GrantLogSecret, len(r.decisions))
	for i, d := range r.decisions {
		ev.Secrets[i] = store.GrantLogSecret{Name: d.Name, Decision: d.Decision}
	}
	slices.SortFunc(ev.Secrets, func(a, b store.GrantLogSecret) int { return strings.Compare(a.Name, b.Name) })
	ev.State = r.state
	ev.Approver = r.approver
	if r.lifetime != nil {
		ev.LifetimeSeconds = *r.lifetime
	}
	ev.Reason = r.reason
	ev.Detail = r.decisionDetail()
	ev.Raw = rawJSON(r.auditDetail())
	return ev
}

// insertRequest writes the request row, its per-secret decisions, and its request.created audit
// row. A pending row's record_id is filled in by createPending in the same transaction.
func (m *Machine) insertRequest(ctx context.Context, tx pgx.Tx, r newRequest) error {
	var pendingExpires *time.Time
	if r.state == "pending" {
		t := r.pendingExpiresAt
		pendingExpires = &t
	}
	detail := r.decisionDetail()
	if _, err := tx.Exec(ctx, `insert into requests (id, enrollment_id, reason, state, allowed_approver, rules_version, lifetime_seconds, pending_expires_at, session_id, record_id, decided_at, decision_detail)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, case when $4 in ('granted','denied') then now() end, $11)`,
		r.id, r.enrollmentID, r.reason, r.state, nullable(r.approver), r.rulesVersion, r.lifetime, pendingExpires, nullable(r.sessionID), nullable(r.recordID), nullable(detail)); err != nil {
		return err
	}
	for _, d := range r.decisions {
		if _, err := tx.Exec(ctx, `insert into request_secrets (request_id, name, decision, source) values ($1,$2,$3,$4)`, r.id, d.Name, d.Decision, d.Source); err != nil {
			return err
		}
	}
	return audit(ctx, tx, "request.created", r.enrollmentID, r.id, nil, "session:"+r.enrollmentID, r.auditDetail())
}

// createPending writes, in Create's transaction, a pending request and its credential-request
// record.
func (m *Machine) createPending(ctx context.Context, tx pgx.Tx, enr enrollmentRow, r newRequest, obj record.RequestObject) (Request, error) {
	body := record.Body{
		Request:         obj.Compact,
		Approver:        r.approver,
		Enrollment:      record.Enrollment{Kind: enr.Kind, RuntimeID: enr.RuntimeID, Operator: deref(enr.Operator), Slot: enr.Slot},
		LifetimeSeconds: r.lifetime,
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
	return Request{ID: r.id, State: "pending", Secrets: r.decisions, RecordID: &recordID, Approver: &body.Approver, LifetimeSeconds: r.lifetime}, nil
}

// lockPendingNames takes one advisory lock for each requested secret in this enrollment, in sorted
// order, until tx ends. It lets an exact repeat coalesce but refuses any overlapping request that
// would create a second card for the same approver and secret.
func lockPendingNames(ctx context.Context, tx pgx.Tx, enrollmentID string, sorted []string) error {
	for _, name := range sorted {
		key, err := json.Marshal([]string{enrollmentID, name})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1, 0))`, string(key)); err != nil {
			return fmt.Errorf("lock pending requests: %w", err)
		}
	}
	return nil
}

// Cancel ends a still-pending request the requesting session no longer wants, writing the
// transition, its audit row, and — when the request has a credential-request record — the
// record's cancelled event, all in one transaction (store.EndPendingRequests).
// The owner check compares the caller against the requests row's own enrollment_id, which an
// adoption rewrites, so the moved session cancels the request that moved with it and the box it
// moved off is ErrNotYours.
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
	actor := "session:" + enrollmentID
	end := store.RequestEnd{
		State: "cancelled", Actor: actor, DecidedBy: &actor, Detail: "cancelled by the requesting session",
	}
	if _, err := store.EndPendingRequests(ctx, tx, store.RequestByID, id, end); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	store.LogGrantEvent(endedRequestLogEvent(end, enrollmentID, id))
	return nil
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
func (m *Machine) expirePending(ctx context.Context, now time.Time) ([]store.EndedRequest, error) {
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	end := store.RequestEnd{
		State: "expired", Actor: "broker", DecidedAt: &now, Detail: "no answer before the request expired",
	}
	expired, err := store.EndPendingRequests(ctx, tx, store.RequestsPastDeadline, now, end)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	// One audit row was written per request this expired, so one line goes out per request.
	for _, r := range expired {
		store.LogGrantEvent(endedRequestLogEvent(end, r.EnrollmentID, r.ID))
	}
	return expired, nil
}

// ApplyDecision decides a pending agent_secret record by login, the login of the person deciding
// it on the broker's own approval page (approval.Page.decide, its one caller). approve=true mints
// the grant while the requesting enrollment may still take one — live, dormant and holding what
// the session's next box will adopt, or lapsed with no end written against it yet, where the end
// the sweep is about to write would leave it dormant (mayGrant below) — and otherwise the
// request is denied.
// It re-reads the record body, recomputes its id, refuses any login but the record's own approver
// (record.ErrNotApprover) whatever the record's state, and an approval by any login the current
// policy would not have approve one of the request's names (currentPolicyAdmits). A denial
// releases nothing, so the record's approver may still deny a request an owner change left them
// unable to approve. An approval of a name the current policy denies because the secret's
// requesters tag no longer lists the session is neither refused nor granted: nobody could
// approve it back, so the request is recorded denied by the broker itself (PolicyDeniedActor)
// with that reason as its detail, and the event still names the person who pressed approve.
// It re-verifies the embedded request object, and writes the event (naming
// that login), the request transition and the audit row in one transaction. For its approver, a
// record that expired undecided — expired by the sweeper, or past its pending_expires_at before
// the sweeper got to it — is ErrExpired, and any other non-pending record ErrTerminal: a
// duplicate or late decision changes nothing.
//
// The enrollment the decision grants to is the one the `requests` row names, never the one
// `credential_requests` recorded. The record is append-only (credential_requests_refuse_update,
// migration 0005), so nothing can re-point it when a session moves to another box, while the
// request row moves with the grant: enroll.Create hands what a predecessor held to the next
// enrollment of that session name under the same principal. Reading the record's own column
// would answer an approval given after a move with "the requesting enrollment is no longer live"
// and mint nothing, which is the half of the incident dispatch://INFRA-1167 reports.
//
// The work is one attempt (applyDecisionAttempt), made again when the request moves out from
// under it. A session moves once per enrolment, so the attempt after a move reads one enrollment
// under both of its locks; the bound on how many are made is the one enroll.Service.Create puts
// on its own retries.
func (m *Machine) ApplyDecision(ctx context.Context, recordID string, approve bool, login string) (Decision, error) {
	const maxAttempts = 3
	for range maxAttempts {
		decision, moved, err := m.applyDecisionAttempt(ctx, recordID, approve, login)
		if moved {
			continue
		}
		return decision, err
	}
	return Decision{}, fmt.Errorf("decide record %s: exhausted retries while the request kept moving to another enrollment", recordID)
}

// enrollmentHoldsForItsSession is "enrollment e still holds something for its session": a live
// grant, or a request nobody has answered yet. It is enroll's own holdsForItsSession word for
// word, and must stay word for word that. Three statements read the one rule — endEnrollment's
// dormancy probe, which decides whether an end of the box leaves the row dormant instead of
// revoking what it holds, adopt's predecessor query, and mayGrant's third case below — and
// mayGrant's job is to grant on a lapsed row exactly when the end the sweep is about to write
// would leave that row dormant. Were this copy the wider of the two, an approval would be
// granted onto a row the sweep then revokes; were it the narrower, an approval would be denied
// on a row the sweep then carries. Either way the answer a person gets would turn on which of
// the two ran first.
//
// It is a copy rather than one shared constant because enroll's is unexported, and a SQL
// fragment that means anything only when it is spliced against an alias named e is a poor thing
// for a package to export. What holds the copies together instead is
// TestTheLapsedRowGrantRuleAnswersWhatTheEndThatFollowsDoes, which runs this rule and
// enroll.Service.EndLapsed over every shape of row and compares their two answers, the way
// enroll's own serving_test.go pins its copy of a SQL rule against the rule itself.
const enrollmentHoldsForItsSession = `exists (select 1 from grants g where g.enrollment_id=e.id and g.revoked_at is null and (g.expires_at is null or g.expires_at > now()))
				or exists (select 1 from requests r where r.enrollment_id=e.id and r.state='pending')`

// enrollmentWouldGoDormant is "an end of this box, written now, would leave enrollment e dormant
// rather than revoke what it holds": the row names a session, records the principal that session
// belongs to, and still holds a live grant or a request nobody has answered. It is
// enroll.endEnrollment's own dormancy probe — the same two tests, in the same order, around the
// same holdsForItsSession text — because it has to answer what that probe will answer about the
// same row a moment later.
const enrollmentWouldGoDormant = `coalesce(e.session_id,'') <> '' and e.carry_principal is not null and (
				` + enrollmentHoldsForItsSession + `)`

// applyDecisionAttempt makes one of ApplyDecision's attempts, writing everything that decision
// writes or nothing at all. moved is true when the request moved to another enrollment between
// the read that locks the enrollment and the read that locks the request — an adoption
// committing in that window — in which case the attempt has written nothing and ApplyDecision
// makes another, which reads the enrollment the request names now.
//
// The enrollment row is locked before the request row — the same order every other
// enrollment-then-request writer in this package takes them in, so none of them deadlock — and
// the join below is what keeps that order while reading the enrollment's id off the request: it
// locks the enrollment alone (for share of e) and takes no lock on the request. Taking no lock on
// the request is what lets the request move between the two reads. An adoption holds the
// predecessor's enrollment row for update while it moves the requests off it (enroll.adopt), and
// when it commits Postgres rechecks the row this statement locked, never the request row the join
// read beside it, so the join answers with the predecessor — live, or ended and dormant — though
// the request hangs off it no longer. So the second read, under a snapshot of its own, reads the
// request's own enrollment_id, and the attempt is abandoned when the two disagree. Nothing is
// written against a box the session has moved off, and nothing is denied for a move that was
// already done when the decision arrived.
func (m *Machine) applyDecisionAttempt(ctx context.Context, recordID string, approve bool, login string) (_ Decision, moved bool, _ error) {
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return Decision{}, false, err
	}
	defer tx.Rollback(ctx)

	var body string
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `select body, created_at from credential_requests where id=$1`, recordID).
		Scan(&body, &createdAt); err != nil {
		return Decision{}, false, err
	}
	// mayGrant is which of the enrollment's states a grant may be hung off, and there are three.
	// Live — not revoked and its lease not lapsed — is the ordinary one. Dormant — revoked_at set
	// and dormant_since not null: ended by an end of its box while it still held a grant or this
	// pending request, and kept for the session's next box (enroll.endEnrollment) — is the one
	// this carry is for, and a grant may land on it: an approval given while the box is gone is
	// the order dispatch://INFRA-1167 reports, the next enrollment of that session name adopts
	// the grant with the request (enroll.adopt), the carry sweep revokes it if nobody comes back
	// (enroll.Service.EndCarried), and a person's revoke of the machine login or the sign-in
	// reaches it as it reaches any grant (endCredential). The third is a row whose lease has
	// lapsed and against which no end is written yet, the window between the lapse and the sweep
	// that ends it, which is up to BROKER_SWEEP_SECONDS long: a grant lands on it when the end
	// the sweep is about to write would leave the row dormant (enrollmentWouldGoDormant, which
	// is enroll.endEnrollment's own probe). Denying there while granting a second later, on the
	// same row holding the same rows, would make the sweep's timing the person's answer.
	//
	// Ended for good is what is left: a row revoked with nothing to carry, and a lapsed row no
	// end of its box could leave dormant — one that names no session, or names one and records
	// no principal to enrol under it again, so nothing could ever adopt what it holds. An
	// approval of a request on such a row is recorded and then denied below, as it always was.
	var mayGrant bool
	var enr enrollmentRow
	if err := tx.QueryRow(ctx, `select e.id,
			e.dormant_since is not null or (e.revoked_at is null
				and (enrollment_lease_live(e.lease_expires_at) or (`+enrollmentWouldGoDormant+`))),
			e.kind, e.operator, e.subject, c.service, c.host, e.runtime_id, coalesce(e.session_id,'')
		from requests req join enrollments e on e.id = req.enrollment_id
		left join launcher_credentials c on c.id = e.launcher_credential_id where req.record_id=$1 for share of e`, recordID).
		Scan(&enr.ID, &mayGrant, &enr.Kind, &enr.Operator, &enr.Subject, &enr.Service, &enr.Host, &enr.RuntimeID, &enr.SessionID); err != nil {
		return Decision{}, false, err
	}
	if m.testAdoptionHook != nil {
		m.testAdoptionHook()
	}
	var requestID, holder, state string
	var expired bool
	if err := tx.QueryRow(ctx, `select id, enrollment_id, state, pending_expires_at <= now() from requests where record_id=$1 for update`, recordID).
		Scan(&requestID, &holder, &state, &expired); err != nil {
		return Decision{}, false, err
	}
	if holder != enr.ID {
		return Decision{}, true, nil
	}
	parsed, err := verifyRecordBody(recordID, body)
	if err != nil {
		return Decision{}, false, err
	}
	login, err = parsed.ApproverLogin(record.KindAgentSecret, "", login)
	if err != nil {
		return Decision{}, false, err
	}
	// A record that names no lifetime gets the default the current policy gives its names: a
	// request pending from before the 12-hour default, or one whose names were all
	// default-grant=session when it was made, is held to the policy as it stands at approval.
	lifetime := parsed.LifetimeSeconds
	if approve && lifetime == nil {
		requested, err := requestedSecrets(ctx, tx, requestID)
		if err != nil {
			return Decision{}, false, err
		}
		names := make([]string, len(requested))
		for i, g := range requested {
			names[i] = g.name
		}
		lifetime = defaultLifetime(m.Policy.Get(), names, nil)
	}
	// denied is set when the current policy denies one of the names because the secret's
	// requesters tag no longer lists this session: the approval is recorded as the request's own
	// denial below rather than refused, since no login could approve such a name.
	var denied *policyDenial
	if approve {
		requester, err := m.requester(ctx, tx, enr)
		if err != nil {
			return Decision{}, false, err
		}
		if err := m.currentPolicyAdmits(ctx, tx, requestID, requester, login, lifetime); err != nil && !errors.As(err, &denied) {
			return Decision{}, false, err
		}
	}
	switch {
	case state == "expired" || state == "pending" && expired:
		return Decision{}, false, ErrExpired
	case state != "pending":
		return Decision{}, false, ErrTerminal
	}
	// now is reset to the record's own creation time: the request object's own iat/exp bound only
	// how fresh it had to be when the broker first accepted it (up to 10 minutes), never how long
	// the resulting decision window may stay open (PendingTTL, hours) — re-verifying it with the
	// real current time would fail every decision made more than a few minutes after creation.
	obj, err := record.VerifyRequestObject(parsed.Request, m.Audience, m.Skew, createdAt)
	if err != nil {
		return Decision{}, false, fmt.Errorf("%w: request object no longer verifies: %s", ErrGrantChainInvalid, err)
	}

	event, by := "denied", record.HumanActor(login)
	next, detail := "denied", ""
	// decidedBy is what the requests row records as having decided it: the deciding login,
	// except where the policy rather than that person made the answer (PolicyDeniedActor).
	decidedBy := login
	if approve {
		event = "approved"
		if mayGrant {
			next = "granted"
		} else {
			detail = "the requesting enrollment is no longer live"
		}
	}
	if denied != nil {
		event, by, next = "denied", PolicyDeniedActor, "denied"
		detail, decidedBy = denied.Error(), PolicyDeniedActor
	}
	if _, err := tx.Exec(ctx, `update requests set state=$2, decided_at=now(), decided_by=$3, decision_detail=$4,
		lifetime_seconds = case when $2 = 'granted' then $5::int else lifetime_seconds end where id=$1 and state='pending'`,
		requestID, next, decidedBy, nullable(detail), lifetime); err != nil {
		return Decision{}, false, err
	}
	if _, err := tx.Exec(ctx, `insert into credential_request_events (record_id, event, login, actor, detail) values ($1,$2,$3,$4,$5)`,
		recordID, event, login, by, nullable(detail)); err != nil {
		return Decision{}, false, err
	}
	var grantID string
	if next == "granted" {
		// The grant lasts the lifetime the approved record names, or the default above.
		if grantID, err = insertGrant(ctx, tx, requestID, enr.ID, login, lifetime); err != nil {
			return Decision{}, false, err
		}
	}
	rowDetail := auditDetail{"detail": detail, "record_id": recordID}
	if err := audit(ctx, tx, "request."+next, enr.ID, requestID, nullable(grantID), by, rowDetail); err != nil {
		return Decision{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Decision{}, false, err
	}
	// The service the policy verified for this request is on its own request.created row, and
	// its secrets are in request_secrets; neither is in hand here, so the line leaves both to
	// the route's read (grantLogEvent).
	ev := grantLogEvent("request."+next, by, enr)
	ev.RequestID, ev.GrantID, ev.State = requestID, grantID, next
	ev.Approver, ev.DecidedBy = parsed.Approver, decidedBy
	// lifetime_seconds is what the update above left on the row: the lifetime the grant was
	// given, and for every other answer the one the record froze, which is what the request row
	// has carried since it was written.
	rowLifetime := parsed.LifetimeSeconds
	if next == "granted" {
		rowLifetime = lifetime
	}
	if rowLifetime != nil {
		ev.LifetimeSeconds = *rowLifetime
	}
	ev.Reason, ev.Detail, ev.Raw = obj.Reason, detail, rawJSON(rowDetail)
	store.LogGrantEvent(ev)
	return Decision{RequestID: requestID, State: next, GrantID: grantID, Detail: detail}, false, nil
}

// policyDenial is currentPolicyAdmits' answer for a name the current policy denies because the
// secret's requesters tag no longer lists this session's person or its pod's service. It is
// deliberately not a record.ErrNotApprover: no login could approve such a name, so leaving the
// card pending would wait on a person who can do nothing with it. applyDecisionAttempt records
// the approval as the request's denial instead, and its message is that denial's detail.
type policyDenial struct{ name string }

func (e *policyDenial) Error() string {
	return e.name + ": " + policy.ReasonRequesterNotListed
}

// PolicyDeniedActor is what a request the policy denied under an approval records as having
// decided it: requests.decided_by, the denied event's actor and the request.denied audit row's
// actor alike, since the policy and not the person deciding made that answer. The event keeps
// the approver's own login beside it, so the trail still names who pressed approve.
const PolicyDeniedActor = "broker:policy"

// currentPolicyAdmits refuses login an approval of request requestID when the current policy wants
// one of its names approved by someone else (record.MayDecide), since a change of owner since the
// request was made moves who may approve it: a request waiting on anyone for a secret that is now
// a person's is that person's alone to approve, and one waiting on a person for a secret another
// person must now approve is no one's to approve (its record's approver may still deny it;
// otherwise it expires, or its session cancels it and asks again). requester carries the names the
// session's operator withheld from it, each human tier to the session, so a withheld name that the
// request got automatically before the withhold is its owner's to approve (anyone's, for a shared
// secret), whoever the record waits on. A denial releases nothing, so ApplyDecision does
// not ask. A name the current policy grants at once, denies or no longer serves names no
// approver, so it leaves the approval to the record's approver; Values refuses a grant of the last
// two at its first read. The one deny it answers for is a name the requesters tag no longer lists
// this session for (*policyDenial): no owner change could put it back in the session's reach, so
// ApplyDecision records the request denied rather than leaving a card no one can answer. That
// deny is read in a pass of its own over every name, ahead of every refusal below, because a
// request holding such a name has to be answered however its other names read: in one pass the
// first name that cannot be admitted wins, and both of the refusals below can come first — a
// name that is withheld as well as dropped from the list, and, in a request of several names, an
// earlier name the deciding login may not approve — each leaving on the approver's page a card
// that no person can ever act on, until it expires.
// A withheld name the policy denies for any other reason, or no longer serves, is refused
// instead: only its owner may approve it for the session, and no owner can be read while it is
// out of the policy, nor approves it while a service owns it (the one way left, once that pass
// has answered the list's own deny, for the policy to deny a withheld name).
// Admitted, such an approval would release the name once it returns with the tags it had, since
// that brings back the policy version the request was made under and Values re-checks the grant
// (stillAllowed) only when the version has moved. A secret whose max-grant the current policy sets
// below the request's lifetime, or that now has one the request names none for, is no one's to
// approve now, as with a withheld name: the session asks again within it.
func (m *Machine) currentPolicyAdmits(ctx context.Context, tx pgx.Tx, requestID string, requester policy.Requester, login string, lifetime *int) error {
	names, err := requestedSecrets(ctx, tx, requestID)
	if err != nil {
		return err
	}
	set := m.Policy.Get()
	for _, g := range names {
		d, err := set.Evaluate(g.name, requester)
		if errors.Is(err, policy.ErrUnknownSecret) {
			continue
		}
		if err != nil {
			return err
		}
		if d.Outcome == policy.Deny && d.Reason == policy.ReasonRequesterNotListed {
			return &policyDenial{name: g.name}
		}
	}
	for _, g := range names {
		d, err := set.Evaluate(g.name, requester)
		if errors.Is(err, policy.ErrUnknownSecret) {
			if slices.Contains(requester.Withheld, g.name) {
				return fmt.Errorf("%w: %s is withheld from this session and the current policy does not serve it, so no one may approve it now", record.ErrNotApprover, g.name)
			}
			continue
		}
		if err != nil {
			return err
		}
		if d.Outcome == policy.Deny && slices.Contains(requester.Withheld, g.name) {
			return fmt.Errorf("%w: %s is withheld from this session and the current policy denies it, so no one may approve it now", record.ErrNotApprover, g.name)
		}
		if d.Outcome == policy.Approval && !record.MayDecide(record.KindAgentSecret, d.Approver, "", login) {
			return fmt.Errorf("%w: the current policy has %s approve %s", record.ErrNotApprover, d.Approver, g.name)
		}
	}
	for _, g := range names {
		if limit, over := overMaxGrant(set, g.name, lifetime); over {
			return fmt.Errorf("%w: %w: %s now allows grants of at most %s, shorter than this request asks, so no one may approve it now; its session asks again within that bound", record.ErrNotApprover, ErrGrantRequiresLifetime, g.name, limit)
		}
	}
	return nil
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
// The owner check reads the grants row's own enrollment_id, which an adoption rewrites, so the
// moved session ends the grant that moved with it and every other enrollment is ErrNotYours.
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
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	logGrantRow("grant.revoked", actor, enrollmentRow{ID: owner}, requestID, grantID, rawJSON(auditDetail{}))
	return nil
}

// RevokeByApprover ends a grant on a person's login. The login must be the grant's approver or its
// enrollment's operator (mayRevoke). actor is what every row it writes records as having done it,
// which the caller proved: "human:<login>" for a revoke Dispatch relays from its signed-in person,
// "launcher:<credential id>" for one the operator's own machine login makes. When the operator
// revokes, every name the grant's request got automatically is withheld from the grant's session
// from then on, and every other live grant of the session that got one of those names
// automatically ends with it (withhold), so the session asks before it gets the name again
// (Create). The withhold belongs to the session rather than the grant, so the operator's revoke of
// a grant already revoked (by its session, or by an earlier withhold, after the operator's Live
// grants list was loaded) withholds the same names, recorded as a grant.withheld audit row rather
// than a second grant.revoked. Another person's revoke (an approver's of the grant) ends the grant
// alone and withholds nothing: the session is not theirs. It locks the session's row in the
// statement that reads the grant, before it writes the grant: the order every writer that locks
// both takes them in, and the one lock Create and ApplyDecision read the withheld names under, so a
// request deciding on the session as this runs either is written first, and then ended here if it
// got a withheld name, or decides with the name withheld. A revoke that ends nothing and withholds
// nothing new succeeds and changes nothing.
// The enrollment it reads the operator off, and the one withhold writes against, is the enrollment
// the grants row names now, so an approver's revoke reaches a grant that moved and an operator's
// withholds from the box the session runs on.
func (m *Machine) RevokeByApprover(ctx context.Context, grantID, login, actor string) error {
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var session, requestID string
	var approver, operator *string
	if err := tx.QueryRow(ctx, `select g.enrollment_id, g.request_id, g.approver, e.operator
		from grants g join enrollments e on e.id=g.enrollment_id where g.id=$1 for no key update of e`, grantID).Scan(&session, &requestID, &approver, &operator); err != nil {
		return err
	}
	allowed, asOperator := mayRevoke(login, approver, operator)
	if !allowed {
		return ErrNotApprover
	}
	tag, err := tx.Exec(ctx, `update grants set revoked_at=now(), revoked_by=$2 where id=$1 and revoked_at is null`, grantID, actor)
	if err != nil {
		return err
	}
	ended := tag.RowsAffected() > 0
	detail := auditDetail{}
	var endedByWithhold []endedGrant
	if asOperator {
		withheld, alsoEnded, err := withhold(ctx, tx, session, requestID, actor)
		if err != nil {
			return err
		}
		endedByWithhold = alsoEnded
		if len(withheld) > 0 {
			detail["withheld"] = withheld
		}
	}
	kind := "grant.revoked"
	if !ended {
		if len(detail) == 0 {
			return nil
		}
		kind = "grant.withheld"
	}
	if err := audit(ctx, tx, kind, session, requestID, &grantID, actor, detail); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	// One line per audit row this transaction wrote, not one per transaction: withhold can end
	// several other grants of the session, each with a grant.revoked row of its own. Those rows
	// were written before the revoke's own, so every withheld line goes out before it, the order
	// the review reads them in as well (store.ReadGrantLog orders by audit id). Among themselves
	// they are in no order the table holds: withhold's aggregate answers them by grant_id, a
	// UUID order, and the statement's audited CTE inserts them with no ordering at all. Each
	// names the request whose grant it ended, which is not the revoked grant's.
	enr := enrollmentRow{ID: session, Operator: operator}
	for _, g := range endedByWithhold {
		logGrantRow("grant.revoked", actor, enr, g.RequestID, g.GrantID, g.Detail)
	}
	logGrantRow(kind, actor, enr, requestID, grantID, rawJSON(detail))
	return nil
}

// withhold withholds from enrollmentID every name request requestID, whose grant its operator just
// revoked, got automatically, and in the same statement ends every other live grant of the session
// whose request got one of the names it newly withholds automatically, each with a grant.revoked
// audit row by actor naming those of its names under "withheld". So no grant the session holds
// goes on releasing a withheld name without someone having approved it there. A name already
// withheld ends nothing: its first withhold ended every grant that got it without asking, and a
// grant whose request got it automatically since then was approved by someone the withheld name
// let approve it (currentPolicyAdmits), which stands. It answers the names it had not withheld
// before, and the grant.revoked audit row it wrote for each grant it ended, so RevokeByApprover
// logs one grant-log line per row once the transaction has committed.
// Everything it writes and everything it ends is keyed by enrollmentID, the enrollment the grant
// hangs off now, which after a move is the box the session runs on and not the one that asked.
func withhold(ctx context.Context, tx pgx.Tx, enrollmentID, requestID, actor string) ([]string, []endedGrant, error) {
	var names []string
	var ended []endedGrant
	err := tx.QueryRow(ctx, `with names as (
			select name from request_secrets where request_id=$2 and decision=$4
		), withheld as (
			insert into withheld_secrets (enrollment_id, name) select $1, name from names
			on conflict do nothing
			returning name
		), ended as (
			update grants g set revoked_at=now(), revoked_by=$3
			where g.enrollment_id=$1 and g.revoked_at is null and `+grantUnexpired("g")+`
			and exists (select 1 from request_secrets a where a.request_id=g.request_id and a.decision=$4 and a.name in (select name from withheld))
			returning g.id, g.request_id
		), audited as (
			insert into audit (kind, enrollment_id, request_id, grant_id, actor, detail)
			select 'grant.revoked', $1, e.request_id, e.id, $3, jsonb_build_object('withheld', (select jsonb_agg(a.name order by a.name)
				from request_secrets a where a.request_id=e.request_id and a.decision=$4 and a.name in (select name from withheld)))
			from ended e
			returning grant_id, request_id, detail
		)
		select coalesce((select jsonb_agg(name order by name) from withheld), '[]'::jsonb),
			coalesce((select jsonb_agg(jsonb_build_object('grant', grant_id, 'request', request_id, 'detail', detail) order by grant_id) from audited), '[]'::jsonb)`,
		enrollmentID, requestID, actor, policy.Automatic).Scan(&names, &ended)
	return names, ended, err
}

// mayRevoke answers whether login may revoke a grant, and whether as its enrollment's operator,
// whose revoke withholds (RevokeByApprover), rather than as its approver. The operator is checked
// first, so an operator who also approved the grant revokes it as the operator.
func mayRevoke(login string, approver, operator *string) (allowed, asOperator bool) {
	login = record.CanonicalLogin(login)
	switch {
	case login == "":
		return false, false
	case operator != nil && record.CanonicalLogin(*operator) == login:
		return true, true
	case approver != nil && record.CanonicalLogin(*approver) == login:
		return true, false
	}
	return false, false
}

// enrollment reads a live enrollment (not revoked, lease not lapsed); pgx.ErrNoRows otherwise.
func (m *Machine) enrollment(ctx context.Context, id string) (enrollmentRow, error) {
	var e enrollmentRow
	err := m.Store.Pool.QueryRow(ctx, `select e.id, e.kind, e.operator, c.service, c.host, e.subject, e.thumbprint, e.runtime_id, e.slot, coalesce(e.session_id,'') from enrollments e
		left join launcher_credentials c on c.id = e.launcher_credential_id
		where e.id=$1 and e.revoked_at is null and enrollment_lease_live(e.lease_expires_at)`, id).
		Scan(&e.ID, &e.Kind, &e.Operator, &e.Service, &e.Host, &e.Subject, &e.Thumbprint, &e.RuntimeID, &e.Slot, &e.SessionID)
	return e, err
}

// lockLiveEnrollment takes enrollment id's row lock for share inside tx while the enrollment is
// live (not revoked, lease not lapsed); pgx.ErrNoRows when it is not. Taken before tx writes a
// request or grant row, the order ApplyDecision takes them in, it keeps endEnrollment off the row
// until tx commits: EndLapsed skips the locked row and Revoke waits for it, and either then
// cancels what tx wrote. An enrollment ended while tx waited for the lock is no longer live once
// Postgres rechecks the row, so tx writes nothing on it.
func lockLiveEnrollment(ctx context.Context, tx pgx.Tx, id string) error {
	var one int
	return tx.QueryRow(ctx, `select 1 from enrollments where id=$1 and revoked_at is null and enrollment_lease_live(lease_expires_at) for share`, id).Scan(&one)
}

// anyServiceOwned reports whether set serves any of granted as a service's secret, whose grant is
// re-checked on every use: who is a service's own session rests on BROKER_SERVICES' accounts,
// which the policy's version does not cover (policy.Set.ServiceOwned).
func anyServiceOwned(set *policy.Set, granted []requestedSecret) bool {
	return slices.ContainsFunc(granted, func(g requestedSecret) bool { return set.ServiceOwned(g.name) })
}

// anyListed reports whether set serves any of granted as a secret whose requesters tag says who
// may ask for it, whose grant is re-checked on every use: an entry naming a service names the
// pods whose account BROKER_SERVICES maps to it (Machine.requester), which the policy's version
// does not cover, so a pod can fall off a list the tag never changed. A list naming people alone
// is re-checked as well: it costs one policy evaluation per name and answers what the version
// already decided, and one predicate keeps the gate's two callers from telling the sorts apart.
func anyListed(set *policy.Set, granted []requestedSecret) bool {
	return slices.ContainsFunc(granted, func(g requestedSecret) bool { return set.Secrets[g.name].Requesters != "" })
}

// auditDetail is an audit row's non-secret detail object; audit writes it as JSON.
type auditDetail map[string]any

func audit(ctx context.Context, tx pgx.Tx, kind, enrollmentID, requestID string, grantID *string, actor string, detail auditDetail) error {
	_, err := tx.Exec(ctx, `insert into audit (kind, enrollment_id, request_id, grant_id, actor, detail) values ($1,$2,$3,$4,$5,$6)`, kind, enrollmentID, requestID, grantID, actor, detail)
	return err
}

// grantLogEvent is one audit row this package has just committed, as the grant-log line
// store.LogGrantEvent writes for it. The caller fills in what its own transaction already had in
// hand and nothing else: the enrollment's identity and person come from enr, and the caller adds
// the request, grant, state, decision and the row's own detail (Raw) beside them. A field no
// value is at hand for is left empty rather than read back with a second query — the service the
// broker verified for a request, which lives on that request's request.created row, and the
// secrets of a request a later transaction decides, among them. The audit table is the full
// record, and the grant-log route's read (store.ReadGrantLog) is what joins every one of those
// in for the quarterly review; this line is the copy the log pipeline keeps.
//
// It is called after the transaction has committed, never inside it, and once per audit row of a
// logged kind rather than once per transaction: a writer that ends several grants in one
// statement (withhold) writes one audit row per grant and so logs one line per grant. Only a
// request decision and a grant's end are logged — never a grant.used row, of which one session
// writes one per release, and never an enrollment row, which internal/broker/enroll owns — so
// the volume is one line per decision and per grant end.
//
// A crash between the commit and the line loses the line and not the row.
func grantLogEvent(kind, actor string, enr enrollmentRow) store.GrantLogEvent {
	return store.GrantLogEvent{
		Kind:           kind,
		Actor:          actor,
		EnrollmentID:   enr.ID,
		Session:        store.GrantLogSession{Kind: enr.Kind, SessionID: enr.SessionID, RuntimeID: enr.RuntimeID},
		Person:         deref(enr.Operator),
		ClaimedService: deref(enr.Service),
	}
}

// logGrantRow logs the grant-log line of one grant row this package has just committed: a
// grant.revoked or grant.withheld audit row, whose line names the grant, the request it was
// granted for and the row's own detail beside what grantLogEvent reads off enr, and nothing
// else. Every writer of such a row goes through it — a session's own revoke (RevokeGrant), a
// person's and each grant the withhold beside it ended (RevokeByApprover), and the end a
// release writes when what was granted cannot come back (endGrant) — so the four build the
// same line. A request's own line sets fields past these three and builds its event itself.
func logGrantRow(kind, actor string, enr enrollmentRow, requestID, grantID string, raw json.RawMessage) {
	ev := grantLogEvent(kind, actor, enr)
	ev.RequestID, ev.GrantID, ev.Raw = requestID, grantID, raw
	store.LogGrantEvent(ev)
}

// endedRequestLogEvent is one request store.EndPendingRequests has just ended, as its grant-log
// line: the transition that statement wrote, who it recorded as deciding it (nobody, for an
// expiry the broker made) and the detail it gave the request, which is also what the route reads
// back off the request row. It takes the very store.RequestEnd its caller handed that statement,
// so the line and the row it stands for cannot drift. The audit row the statement wrote carries
// the column's default detail, {}. Cancel and expirePending both end requests through it.
func endedRequestLogEvent(end store.RequestEnd, enrollmentID, requestID string) store.GrantLogEvent {
	ev := grantLogEvent("request."+end.State, end.Actor, enrollmentRow{ID: enrollmentID})
	ev.RequestID, ev.State, ev.DecidedBy, ev.Detail = requestID, end.State, deref(end.DecidedBy), end.Detail
	ev.Raw = rawJSON(auditDetail{})
	return ev
}

// endedGrant is one grant withhold ended beside the revoke that triggered it, read back from the
// grant.revoked audit row withhold wrote for it, so RevokeByApprover logs that row without
// reading the table again.
type endedGrant struct {
	GrantID   string          `json:"grant"`
	RequestID string          `json:"request"`
	Detail    json.RawMessage `json:"detail"`
}

// rawJSON is an audit row's detail as the row holds it, for a grant-log line's Raw. A detail this
// package builds is strings and string slices, which always marshal; an object that somehow does
// not is left out of the line rather than failing the call that already committed.
func rawJSON(detail auditDetail) json.RawMessage {
	raw, err := json.Marshal(detail)
	if err != nil {
		return nil
	}
	return raw
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
