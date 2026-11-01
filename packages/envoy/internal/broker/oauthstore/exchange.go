package oauthstore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"authelia.com/provider/oauth2"
	"authelia.com/provider/oauth2/handler/rfc8693"
	"github.com/jackc/pgx/v5"
)

// What the exchange's handlers ask of the session they run on: the token exchange's own
// interface, the provider's, and the DPoP binding the broker enforces, which the embedded
// *oauth2.DefaultSession answers so a session that carries a binding keeps it through an
// exchange.
var (
	_ oauth2.Session          = (*ExchangeSession)(nil)
	_ rfc8693.Session         = (*ExchangeSession)(nil)
	_ oauth2.DPoPBoundSession = (*ExchangeSession)(nil)
)

// ErrSessionHeld is a session name another principal holds. It is the one refusal BindSession
// has, and the text names the session so an operator reading it knows which name is taken without
// having to find the request again.
var ErrSessionHeld = errors.New("oauthstore: another identity holds this session name")

// SessionRetention is how long a session name stays its principal's after the last token minted
// under it expires: 90 days. Until then the name is refused to every other principal; after it,
// the sweep deletes the row and the name is anyone's to claim.
//
// It bounds how many names the table keeps, and it is safe to state because a name moves only
// once its principal has stopped using it for longer than any credential the exchange accepts can
// sit unused: the longest, a machine login, lapses at most 30 days after its last refresh
// (BROKER_LAUNCHER_CREDENTIAL_SECONDS' ceiling), a pod's projected token at most a day after it
// was minted, a workflow run's within the hour. A principal coming back after 90 days comes back
// through a new login approved by a person, a new pod or a new run, and nothing it held before
// is still spendable. It is far past how long a Legion role sits idle between assignments, which
// is hours or days.
//
// It is a constant rather than a setting because the bound follows from those ceilings, not from
// anything a deployment chooses: shorter would hand a name over while its principal could still
// return, and longer only keeps rows. An operator who must free a name sooner deletes its row
// from oauth_session_bindings.
const SessionRetention = 90 * 24 * time.Hour

// PodSessionRetention is how long a pod's registration to write under a session name
// (RegisterPodSession) is kept past the last write of it: 24 hours.
//
// It is the longest a subject token the exchange accepts from a pod can live
// (oauth.kubernetesTolerance, which admits a projected service-account token's whole life,
// Kubernetes bounding one at 24 hours), so a registration outlives every token the pod it was
// written for can still present. Shorter would refuse a pod still holding a token the exchange
// accepts; longer would let a pod write under the name after every token naming it had been
// refused for age anyway.
//
// It is a constant rather than a setting because the bound follows from that tolerance and from
// nothing a deployment chooses. What it means for an operator: the authority is the registrar's,
// so a daemon that stops registering costs its pods their writes within this window, and a
// registration is renewed by writing it again.
const PodSessionRetention = 24 * time.Hour

// heldByConfiguration is the expires_at of a session name an entry fixes, which ReserveSessions
// and a fixed entry's own BindSession write and nothing lets lapse: the name is its entry's for as
// long as the configuration says so, and Sweep's expires_at <= now() never selects it.
const heldByConfiguration = "'infinity'::timestamptz"

// claimActor is the RFC 8693 Section 4.1 'act' claim, which names the party acting for the
// subject in a delegation. The library spells it in a package its users cannot import.
const claimActor = "act"

// ExchangeSession is the session an exchanged token's request runs on: the *oauth2.DefaultSession
// every other grant of this provider uses, with the subject and actor token claims the RFC 8693
// handlers deposit on it beside it. It is what rfc8693.Session asks of a session, and embedding
// the default one means a row written from either kind reads back into either kind, so the
// machine login's stored sessions keep working unchanged.
//
// Both claim maps are stored, not dropped as the library's own session drops them: an
// introspection of an exchanged token answers from the row, and the claims the exchange decided
// on are part of what the row has to say.
type ExchangeSession struct {
	*oauth2.DefaultSession

	// SubjectToken is the verified claims of the subject token the exchange was made against: a
	// pod's service-account token, a workflow run's GitHub token, a machine's Google identity
	// token, or a machine access token.
	SubjectToken map[string]any `json:"subject_token,omitempty"`

	// ActorToken is the verified claims of the actor token of a delegation. The broker's exchange
	// client permits no actor token, so it is empty for every token this provider mints today,
	// and is kept because the library sets it and an introspection must read back what was set.
	ActorToken map[string]any `json:"actor_token,omitempty"`

	// Principal is who the session name the exchange decided on belongs to
	// (exchange.Claims.Principal). The exchange's decision sets it while the library validates
	// the request, and the provider claims the name for it once the library has set the issued
	// token's expiry. It is not a claim of the token and is not stored: the row the claim is
	// recorded in is oauth_session_bindings.
	Principal string `json:"-"`

	// Instance is the pod uid the exchange decided this token's writes come from
	// (exchange.Claims.Instance), empty for a caller no registration separates from the others of
	// its principal: a fixed-session entry, a workflow run or a dev box. The provider reads it
	// against oauth_pod_sessions in the exchange's own transaction, so a pod writes under a
	// session name only while a registration says it may. Like Principal it is no claim of the
	// token and is not stored.
	Instance string `json:"-"`
}

// NewExchangeSession is an empty exchange session with its extra claims ready to be written, the
// shape the provider's session factory hands every request.
func NewExchangeSession() *ExchangeSession {
	return &ExchangeSession{DefaultSession: &oauth2.DefaultSession{Extra: map[string]any{}}}
}

// SetSubjectToken records the subject token's verified claims.
func (s *ExchangeSession) SetSubjectToken(token map[string]any) { s.SubjectToken = token }

// GetSubjectToken is the subject token's verified claims, nil when none were set.
func (s *ExchangeSession) GetSubjectToken() map[string]any { return s.SubjectToken }

// SetActorToken records the actor token's verified claims.
func (s *ExchangeSession) SetActorToken(token map[string]any) { s.ActorToken = token }

// GetActorToken is the actor token's verified claims, nil when none were set.
func (s *ExchangeSession) GetActorToken() map[string]any { return s.ActorToken }

// SetClaimActor records the 'act' claim of a delegation among the session's extra claims, which
// is where this provider's opaque access tokens carry everything an introspection answers.
func (s *ExchangeSession) SetClaimActor(act map[string]any) {
	if s.DefaultSession == nil {
		s.DefaultSession = &oauth2.DefaultSession{}
	}
	if s.Extra == nil {
		s.Extra = map[string]any{}
	}
	s.Extra[claimActor] = act
}

// AccessTokenClaimsMap is what an exchanged access token says about itself: its subject, its
// username and every extra claim the exchange wrote, which is what an introspection answer is
// built from.
func (s *ExchangeSession) AccessTokenClaimsMap() map[string]any {
	claims := map[string]any{"sub": s.GetSubject(), "username": s.GetUsername()}
	if s.DefaultSession != nil {
		maps.Copy(claims, s.Extra)
	}
	return claims
}

// Clone is a copy of the session that shares nothing the caller can write through, which the
// library relies on when it carries one request's session onto another. The two claim maps are
// copied one level deep: they are a verified token's claims, which nothing changes once the
// handler that read them has set them.
func (s *ExchangeSession) Clone() oauth2.Session {
	if s == nil {
		return nil
	}
	cloned := &ExchangeSession{
		SubjectToken: maps.Clone(s.SubjectToken),
		ActorToken:   maps.Clone(s.ActorToken),
		Principal:    s.Principal,
		Instance:     s.Instance,
	}
	if s.DefaultSession != nil {
		cloned.DefaultSession, _ = s.DefaultSession.Clone().(*oauth2.DefaultSession)
	}
	return cloned
}

// BindSession claims the session name for principal until SessionRetention past expires, the
// moment the token the claim is made for expires, and reports ErrSessionHeld when another
// principal holds it. fixed says the configuration of the process making the claim fixes the
// name for principal; the claim then holds the name with no expiry and takes it from whoever
// holds it, and BindSession answers the principal it took the name from, one whose claim had not
// run out; it answers "" for a name that was unheld, run out or already principal's.
//
// The name is the principal's from its first claim: the same principal re-claiming it extends
// the claim and never shortens it, any other principal is refused for as long as the claim
// stands, and an unheld name, or one whose claim has run out, is claimed outright. A name an
// entry fixes is held by ReserveSessions before its entry's first claim, so a stranger is refused
// it by this same rule, and never runs out. Without this, any caller that may exchange could name
// any session string, and a token naming another principal's session would be believed by
// everything downstream that believes the token.
//
// The configuration decides here, at the claim, rather than at ReserveSessions: two replicas
// started with configurations that disagree, as during a rollout that re-points a fixed name,
// each give the name to the principal their own configuration fixes it for when that principal
// asks, so the entry the configuration fixes a name for is never refused its own name, and a
// re-point takes effect at the new entry's first exchange.
//
// It is one statement, and the only one that refuses a claim, so two exchanges racing for one
// name cannot both win: the loser's insert conflicts with the winner's row and its update is
// refused by the row's own principal and expiry. The holder it answers is read from the
// statement's snapshot, so a claim that committed while this one waited on its row is not named.
func (s *Store) BindSession(ctx context.Context, session, principal string, fixed bool, expires time.Time) (string, error) {
	var taken string
	err := s.db(ctx).QueryRow(ctx, `with prior as (
			select principal from oauth_session_bindings where session = $1 and expires_at > now()
		), bound as (
			insert into oauth_session_bindings (session, principal, expires_at)
			values ($1, $2, case when $4 then `+heldByConfiguration+` else $3 end)
			on conflict (session) do update set principal = excluded.principal,
				expires_at = greatest(oauth_session_bindings.expires_at, excluded.expires_at)
			where $4 or oauth_session_bindings.principal = excluded.principal or oauth_session_bindings.expires_at <= now()
			returning true
		)
		select coalesce((select principal from prior where principal <> $2), '') from bound`,
		session, principal, expires.Add(SessionRetention), fixed).Scan(&taken)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", ErrSessionHeld, session)
	}
	if err != nil {
		return "", fmt.Errorf("bind session name: %w", err)
	}
	return taken, nil
}

// ReserveSessions holds every session name reserved maps to a principal (the names the entries of
// the configuration fix, exchange.Set.Reservations) for that principal, with no expiry, unless
// another principal holds it, and answers each name it left with another principal mapped to that
// holder. A name an earlier configuration fixed and this one does not is handed back to the
// ordinary rule, its principal holding it for SessionRetention from now.
//
// It takes nothing: it is what refuses a stranger a fixed name before the entry's own caller has
// ever exchanged, and a name another principal holds already refuses strangers. Taking is
// BindSession's, at the fixed entry's own claim, so a replica started with an older configuration
// that reserves after a newer one cannot take back a name the newer one has re-pointed.
//
// It is one statement, so the names are reserved together or not at all, and a BindSession racing
// it waits on the row it is writing.
func (s *Store) ReserveSessions(ctx context.Context, reserved map[string]string) (map[string]string, error) {
	sessions, principals := make([]string, 0, len(reserved)), make([]string, 0, len(reserved))
	for session, principal := range reserved {
		sessions, principals = append(sessions, session), append(principals, principal)
	}
	rows, err := s.db(ctx).Query(ctx, `with released as (
			update oauth_session_bindings set expires_at = $3
			where expires_at = `+heldByConfiguration+` and session <> all($1)
		), held as (
			insert into oauth_session_bindings (session, principal, expires_at)
			select session, principal, `+heldByConfiguration+` from unnest($1::text[], $2::text[]) as r (session, principal)
			on conflict (session) do update set principal = excluded.principal, expires_at = excluded.expires_at
			where oauth_session_bindings.principal = excluded.principal or oauth_session_bindings.expires_at <= now()
			returning session
		)
		select session, principal from oauth_session_bindings
		where session = any($1) and session not in (select session from held)`,
		sessions, principals, time.Now().Add(SessionRetention))
	if err != nil {
		return nil, fmt.Errorf("reserve the session names the configuration fixes: %w", err)
	}
	defer rows.Close()
	left := map[string]string{}
	for rows.Next() {
		var session, holder string
		if err := rows.Scan(&session, &holder); err != nil {
			return nil, fmt.Errorf("reserve the session names the configuration fixes: %w", err)
		}
		left[session] = holder
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reserve the session names the configuration fixes: %w", err)
	}
	return left, nil
}

// RegisterPodSession records that the pod podUID may exchange under the session name session,
// until PodSessionRetention from now, and answers when that registration runs out. A pod uid
// already registered for the name has its registration extended rather than duplicated, so a
// registrar that writes the same row each time a pod starts, or renews it while the pod runs,
// keeps the pod writing and never shortens what another write left.
//
// It is what separates two pods of one listed account, which the exchange reads as one principal:
// the name belongs to the account, so nothing in the claim on the name can tell them apart, and
// without this row either pod could mint a token naming the other's session. A registration says
// only that this deployment started this pod under this name; who the name belongs to stays the
// claim's answer (BindSession).
func (s *Store) RegisterPodSession(ctx context.Context, podUID, session string) (time.Time, error) {
	var expires time.Time
	err := s.db(ctx).QueryRow(ctx, `insert into oauth_pod_sessions (pod_uid, session, expires_at)
		values ($1, $2, $3)
		on conflict (pod_uid, session) do update set expires_at = greatest(oauth_pod_sessions.expires_at, excluded.expires_at)
		returning expires_at`, podUID, session, time.Now().Add(PodSessionRetention)).Scan(&expires)
	if err != nil {
		return time.Time{}, fmt.Errorf("register the pod's session name: %w", err)
	}
	return expires, nil
}

// PodSessionRegistered reports whether a registration still says the pod podUID may exchange
// under session. It reads through s.db(ctx), so the exchange's own transaction asks it, and it
// asks the cluster nothing: a pod that no longer runs goes on being registered until its row
// runs out, exactly as its own token goes on verifying until its expiry.
func (s *Store) PodSessionRegistered(ctx context.Context, podUID, session string) (bool, error) {
	var registered bool
	err := s.db(ctx).QueryRow(ctx, `select exists (select 1 from oauth_pod_sessions
		where pod_uid = $1 and session = $2 and expires_at > now())`, podUID, session).Scan(&registered)
	if err != nil {
		return false, fmt.Errorf("read the pod's session registration: %w", err)
	}
	return registered, nil
}

// SetTokenExchangeCustomJWT remembers a subject token's jti until exp plus jtiRetentionMargin and
// fails when it is already remembered, which is how the library is told a token is being
// presented a second time: it reads any error from here as a replay. The insert, or the takeover
// of a record whose token has expired, is one statement, so two exchanges presenting one token
// cannot both see it unused.
func (s *Store) SetTokenExchangeCustomJWT(ctx context.Context, jti string, exp time.Time) error {
	var recorded bool
	err := s.db(ctx).QueryRow(ctx, `insert into oauth_exchange_jtis (jti, expires_at) values ($1, $2)
		on conflict (jti) do update set expires_at = excluded.expires_at
		where oauth_exchange_jtis.expires_at <= now()
		returning true`, jti, exp.Add(jtiRetentionMargin)).Scan(&recorded)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("oauthstore: this subject token has been presented before")
	}
	if err != nil {
		return fmt.Errorf("record exchanged subject token: %w", err)
	}
	return nil
}

// GetSubjectForTokenExchange answers who the holder of a verified subject token is, for the token
// types whose subject the library cannot read off the token itself. The store decides nothing
// here: it calls ExchangeSubject, which the provider wiring sets to the exchange's own decision,
// and names it in whatever it refuses.
func (s *Store) GetSubjectForTokenExchange(ctx context.Context, request oauth2.Requester, subjectToken map[string]any) (string, error) {
	if s.ExchangeSubject == nil {
		return "", errors.New("oauthstore: the token exchange is composed without a subject decision (Store.ExchangeSubject)")
	}
	subject, err := s.ExchangeSubject(ctx, request, subjectToken)
	if err != nil {
		return "", fmt.Errorf("decide the subject token's subject: %w", err)
	}
	return subject, nil
}
