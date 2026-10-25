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

// ErrSessionHeld is a session name another identity holds and has not let lapse. It is the one
// refusal BindSession has, and the text names the session so an operator reading it knows which
// name is taken without having to find the request again.
var ErrSessionHeld = errors.New("oauthstore: another identity holds this session name")

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
	// pod's service-account token, a workflow run's GitHub token, or a machine access token.
	SubjectToken map[string]any `json:"subject_token,omitempty"`

	// ActorToken is the verified claims of the actor token of a delegation. The broker's exchange
	// client permits no actor token, so it is empty for every token this provider mints today,
	// and is kept because the library sets it and an introspection must read back what was set.
	ActorToken map[string]any `json:"actor_token,omitempty"`
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
	}
	if s.DefaultSession != nil {
		cloned.DefaultSession, _ = s.DefaultSession.Clone().(*oauth2.DefaultSession)
	}
	return cloned
}

// BindSession claims the session name for identity until expires, the moment the token the claim
// is made for would have expired, and reports ErrSessionHeld when another identity holds it and
// its claim has not lapsed.
//
// The name is the identity's for as long as its tokens live: the same identity re-claiming it
// extends the claim and never shortens it, a lapsed claim is taken over by whoever asks next, and
// an unheld name is claimed outright. Without this, any caller that may exchange could name any
// session string, and a token naming another pod's session would be believed by everything
// downstream that believes the token.
//
// It is one statement, so two exchanges racing for one name cannot both win: the loser's insert
// conflicts with the winner's row and its update is refused by the row's own identity and expiry.
func (s *Store) BindSession(ctx context.Context, session, identity string, expires time.Time) error {
	var bound bool
	err := s.db(ctx).QueryRow(ctx, `insert into oauth_session_bindings (session, identity, expires_at) values ($1, $2, $3)
		on conflict (session) do update set identity = excluded.identity,
			expires_at = greatest(oauth_session_bindings.expires_at, excluded.expires_at)
		where oauth_session_bindings.identity = excluded.identity or oauth_session_bindings.expires_at <= now()
		returning true`, session, identity, expires).Scan(&bound)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrSessionHeld, session)
	}
	if err != nil {
		return fmt.Errorf("bind session name: %w", err)
	}
	return nil
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
