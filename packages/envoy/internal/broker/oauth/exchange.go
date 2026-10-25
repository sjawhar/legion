package oauth

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"authelia.com/provider/oauth2"
	"authelia.com/provider/oauth2/handler/rfc8693"
	"authelia.com/provider/oauth2/token/jose"
	"authelia.com/provider/oauth2/token/jwt"

	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

// tokenTypeAccessToken is RFC 8693's own name for an access token: the only kind this provider's
// exchange mints, the kind it asks for by default, and the kind a dev box session presents its
// machine login's token as. It is the name the client registration lists, so both read it from
// the one place the broker and its callers share.
const tokenTypeAccessToken = oauthwire.SubjectTokenTypeAccessToken

// The form parameters of an exchange request the broker reads itself. The library reads the rest
// (RFC 8693 Section 2.1); FormSession is the broker's own, because RFC 8693 has no parameter for
// the name a caller writes under.
const (
	// FormSubjectTokenType is the request's subject_token_type, which says which kind of
	// identity the caller is presenting and so which decision the exchange makes.
	FormSubjectTokenType = "subject_token_type"
	// FormSession is the session name the caller asks to write under. An entry that fixes a
	// session refuses a request naming another; an entry that fixes none requires it.
	FormSession = "session"
)

// The extra claims an exchanged access token carries, which are what an introspection answers
// from: this provider's access tokens are opaque, so the claims live on the stored session rather
// than in the token's own bytes.
const (
	// ClaimSession is the session the token's writes are recorded as.
	ClaimSession = "session"
	// ClaimPerson is the person the session acts for.
	ClaimPerson = "person"
	// ClaimProjectScope is what ClaimProject means, one of ProjectScopeNamed or
	// ProjectScopePerson. It is written always, so a reader never has to take an absent project
	// as either "every project" or "none".
	ClaimProjectScope = "project_scope"
	// ClaimProject is the one project key a ProjectScopeNamed token may write, and is absent
	// from a ProjectScopePerson token, which names none.
	ClaimProject = "project"
)

// The wire names of exchange.ProjectScope, which ClaimProjectScope carries and an introspection
// answer repeats. They are short and fixed so a reader of either reads one word rather than the
// prose exchange.ProjectScope.String prints for an operator.
const (
	// ProjectScopeNamed is a token that may write the one project it names.
	ProjectScopeNamed = "named"
	// ProjectScopePerson is a token that may write every project its person may write, and names
	// no project.
	ProjectScopePerson = "person"
)

// subjectTokenAlgorithms are the signature algorithms a JWT subject token may be signed with: the
// asymmetric ones, which are the only ones an issuer's published key set can hold. The library
// parses a subject token allowing every algorithm including 'none' and leaves the choice to the
// type's ValidateFunc, so refusing the rest here is what keeps an unsigned token, or one signed
// with a secret the issuer shares with somebody, from ever reaching a key.
var subjectTokenAlgorithms = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.EdDSA,
}

// The lifetime tolerance each JWT subject token type allows, which the library applies to both
// ends: a token whose iat is older than the window, and one whose exp is further ahead than it,
// are refused.
const (
	// kubernetesTolerance admits a projected service-account token's whole life. Kubernetes
	// bounds one at 24 hours, so a shorter window would refuse a freshly minted token outright
	// for expiring too far in the future.
	kubernetesTolerance = 24 * time.Hour
	// githubTolerance admits a workflow run's token, which lives minutes, while bounding how
	// stale an iat the exchange accepts.
	githubTolerance = time.Hour
)

// KeySource answers the public key a token's header names, which is what the library's
// jwt.Keyfunc has to return: *oidc.Keys is one, reading the issuer's published set.
type KeySource interface {
	// Key is the public key a token naming kid and signed with alg is checked against.
	Key(ctx context.Context, kid, alg string) (crypto.PublicKey, error)
}

// IssuerConfig is one issuer whose tokens the exchange accepts as a subject token: a Kubernetes
// cluster, or GitHub. Every field is required of a configured issuer and the zero value is an
// issuer the broker was not told about, whose token type is then not registered at all — so a
// caller presenting it is refused by the library before any handler of the broker's runs.
type IssuerConfig struct {
	// Issuer is the `iss` the subject token must carry, which the library compares exactly.
	Issuer string
	// Audience is what the subject token's `aud` must carry. The library checks a subject
	// token's issuer and its expiry and never its audience, so the exchange checks this itself:
	// a token minted for another service is not spendable here.
	Audience string
	// Keys is the issuer's published signing keys, which the token's `kid` and `alg` are
	// resolved against.
	Keys KeySource
}

// configured reports whether the broker was told about this issuer at all.
func (c IssuerConfig) configured() bool {
	return c.Issuer != "" || c.Audience != "" || c.Keys != nil
}

// validate refuses an issuer that is named but not complete: half a configuration would register
// a token type whose tokens could not be checked, which is worse than not accepting that kind of
// caller at all. where names the issuer in the refusal.
func (c IssuerConfig) validate(where string) error {
	if !c.configured() {
		return nil
	}
	if c.Issuer == "" || c.Audience == "" || c.Keys == nil {
		return fmt.Errorf("oauth: the %s subject token issuer is configured in part (issuer %q, audience %q, keys %t); it needs all three or none", where, c.Issuer, c.Audience, c.Keys != nil)
	}
	return nil
}

// ExchangeConfig is what the RFC 8693 token exchange decides and verifies with. None of it is the
// provider's to decide: the broker's wiring reads it from the deployment's configuration and
// hands it over whole.
type ExchangeConfig struct {
	// Policy is the listed identities and what each may write. A nil Policy is a deployment
	// that has listed none, which New reads as the empty set: the exchange is composed whether
	// or not any identity or issuer is, because the token-exchange grant is the exchange
	// client's only grant and a caller it refuses must be refused by name.
	Policy *exchange.Set
	// Kubernetes is the cluster issuer a pod's projected service-account token comes from.
	Kubernetes IssuerConfig
	// GitHub is the issuer a continuous-integration workflow run's token comes from.
	GitHub IssuerConfig
}

// validate refuses a configuration the exchange could not serve.
func (c ExchangeConfig) validate() error {
	if err := c.Kubernetes.validate("Kubernetes"); err != nil {
		return err
	}
	return c.GitHub.validate("GitHub")
}

// tokenTypes are the token types the library registers for the exchange, keyed by the exact
// string a request names them with.
//
// The access token is always registered: it is the only kind this provider issues, so it is both
// the default requested type and the type a dev box session presents its machine access token
// under. Each JWT type is registered only where its issuer is configured, so a caller presenting
// an issuer the broker was never told about is refused by the library rather than by a half-built
// handler.
func (c ExchangeConfig) tokenTypes() map[string]oauth2.RFC8693TokenType {
	types := map[string]oauth2.RFC8693TokenType{
		tokenTypeAccessToken: &rfc8693.DefaultTokenType{Name: tokenTypeAccessToken},
	}
	if c.Kubernetes.configured() {
		types[oauthwire.SubjectTokenTypeKubernetes] = &rfc8693.JWTType{
			Name:   oauthwire.SubjectTokenTypeKubernetes,
			Issuer: c.Kubernetes.Issuer,
			JWTValidationConfig: rfc8693.JWTValidationConfig{
				// A pod's projected token is a standing credential its kubelet refreshes, and a
				// long-running role exchanges many times with the one it holds, so single use
				// would refuse every renewal. What keeps a token from being spendable outside a
				// pod is the pod binding the decision requires.
				ValidateJTI:                false,
				JWTLifetimeToleranceWindow: kubernetesTolerance,
				ValidateFunc:               keyFunc(c.Kubernetes.Keys),
			},
		}
	}
	if c.GitHub.configured() {
		types[oauthwire.SubjectTokenTypeGitHub] = &rfc8693.JWTType{
			Name:   oauthwire.SubjectTokenTypeGitHub,
			Issuer: c.GitHub.Issuer,
			JWTValidationConfig: rfc8693.JWTValidationConfig{
				// A workflow run mints a fresh token per request, so one presentation is all a
				// token needs and a leaked one cannot be replayed.
				ValidateJTI:                true,
				JWTLifetimeToleranceWindow: githubTolerance,
				ValidateFunc:               keyFunc(c.GitHub.Keys),
			},
		}
	}
	return types
}

// issuerOf is the configured issuer a subject token type's tokens come from, and false for a type
// that is not one of the two JWT kinds.
func (c ExchangeConfig) issuerOf(subjectTokenType string) (IssuerConfig, bool) {
	switch subjectTokenType {
	case oauthwire.SubjectTokenTypeKubernetes:
		return c.Kubernetes, true
	case oauthwire.SubjectTokenTypeGitHub:
		return c.GitHub, true
	}
	return IssuerConfig{}, false
}

// keyFunc is a JWT subject token type's ValidateFunc: the library hands it the parsed but
// unverified token and checks the signature with whatever key it returns.
//
// The algorithm comes from a header nobody has verified, and the library's parse allows every
// algorithm including 'none', so the header is refused before any key is returned: an issuer
// publishes public keys, so a token signed with a shared secret or not signed at all is not one
// of its tokens whatever key id it names.
//
// The read runs on its own context rather than the request's, because the library's signature
// gives it none; the key source bounds its own fetch, and a set already cached costs no read.
func keyFunc(keys KeySource) jwt.Keyfunc {
	return func(token *jwt.Token) (any, error) {
		if !slices.Contains(subjectTokenAlgorithms, token.SignatureAlgorithm) {
			return nil, fmt.Errorf("oauth: a subject token signed with %q is refused: its issuer signs with a published key", token.SignatureAlgorithm)
		}
		return keys.Key(context.Background(), token.KeyID, string(token.SignatureAlgorithm))
	}
}

// exchangeGrant is the token endpoint's RFC 8693 branch: a caller's platform identity for a
// Dispatch access token, bound to the key its DPoP proof names.
//
// The whole request runs in one transaction (oauthstore.TxFrom carries it), because the broker's
// own step writes: the session name it claims and the token it is claimed for must land together
// or not at all. The machine access-token subject type takes its decision here, between the
// library validating the request and the library building the answer, and then holds its launcher
// credential's row (liveCredential) until the token commits; the two JWT types took their
// decision during validation, through Store.ExchangeSubject. The claim on the session name is made
// for every type once the library has built the answer, because that is when the issued token's
// expiry is set, and the claim must not end before the token it is made for.
func (p *Provider) exchangeGrant(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := p.reserveSessions(ctx); err != nil {
		p.writeError(ctx, w, nil, oauth2.ErrServerError.WithWrap(err).WithDebugError(err))
		return
	}
	txCtx, err := p.store.BeginTX(ctx)
	if err != nil {
		p.writeError(ctx, w, nil, oauth2.ErrServerError.WithWrap(err).WithDebugError(err))
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = p.store.Rollback(txCtx)
		}
	}()
	request, err := p.oauth.NewAccessRequest(txCtx, r, oauthstore.NewExchangeSession())
	if err != nil {
		p.writeError(ctx, w, request, err)
		return
	}
	session, ok := request.GetSession().(*oauthstore.ExchangeSession)
	if !ok {
		p.writeError(ctx, w, request, oauth2.ErrServerError.WithDebugf("the exchange ran on a %T session", request.GetSession()))
		return
	}
	if request.GetRequestForm().Get(FormSubjectTokenType) == tokenTypeAccessToken {
		if _, err := p.decide(txCtx, request, session.GetSubjectToken()); err != nil {
			p.writeError(ctx, w, request, err)
			return
		}
		if err := p.liveCredential(txCtx, session.GetSubjectToken()); err != nil {
			p.writeError(ctx, w, request, err)
			return
		}
	}
	resp, err := p.oauth.NewAccessResponse(txCtx, request)
	if err != nil {
		p.writeError(ctx, w, request, err)
		return
	}
	taken, err := p.claimSession(txCtx, session)
	if err != nil {
		p.writeError(ctx, w, request, err)
		return
	}
	if err := p.store.Commit(txCtx); err != nil {
		p.writeError(ctx, w, request, oauth2.ErrServerError.WithWrap(err).WithDebugError(err))
		return
	}
	committed = true
	if taken != "" {
		// Logged once the claim has committed, because it is the one way a principal loses a name
		// it used within the retention: the configuration now fixes it for an entry.
		slog.Warn("broker: the token exchange took a session name the configuration fixes from its holder",
			"session", session.Extra[ClaimSession], "from", taken, "to", session.Principal)
	}
	p.oauth.WriteAccessResponse(ctx, w, request, resp)
}

// reserveSessions holds every session name an entry fixes for that entry's principal
// (oauthstore.Store.ReserveSessions), once for the life of the provider and before its first
// exchange, so the first caller of another principal to name one is refused by the same claim
// that refuses any held name. Each replica of the broker reserves for the configuration it was
// started with. A failure answers that exchange server_error, and the next one tries again.
//
// It takes no name another principal holds: replicas whose configurations disagree, as during a
// rollout that re-points a fixed name, would otherwise take it back and forth in the order they
// first exchanged. claimSession gives the name to its entry when that entry first asks for it,
// and a name left with its holder until then is logged, naming both.
func (p *Provider) reserveSessions(ctx context.Context) error {
	if p.reserved.Load() {
		return nil
	}
	p.reserving.Lock()
	defer p.reserving.Unlock()
	if p.reserved.Load() {
		return nil
	}
	left, err := p.store.ReserveSessions(ctx, p.reservations)
	if err != nil {
		return err
	}
	for session, holder := range left {
		slog.Warn("broker: the token exchange left a session name the configuration fixes with its holder until the entry claims it",
			"session", session, "holder", holder, "entry", p.reservations[session])
	}
	p.reserved.Store(true)
	return nil
}

// decide is the exchange's own decision, and the only one: what the minted token says, and who
// the session name it says it is writing under belongs to. Both paths into the exchange reach it
// — the two JWT subject types through Store.ExchangeSubject while the library validates the
// request, the machine access-token type from exchangeGrant once it has — so one function decides
// for all three.
//
// In order: the subject token's audience, which the library never checks and which a token minted
// for another service would fail; the policy's decision, which names the session, the person, the
// project and the principal; and then the claims themselves, onto the session, where the row the
// token is stored in carries them and an introspection answers from them, with the principal
// beside them for claimSession.
//
// Every refusal is an OAuth error the caller can act on and none repeats the subject token:
// RFC 8693 Section 2.2.2 makes a subject token that is unacceptable on policy invalid_request.
func (p *Provider) decide(_ context.Context, request oauth2.Requester, subjectToken map[string]any) (string, error) {
	session, ok := request.GetSession().(*oauthstore.ExchangeSession)
	if !ok {
		return "", oauth2.ErrServerError.WithDebugf("the exchange ran on a %T session", request.GetSession())
	}
	form := request.GetRequestForm()
	subjectTokenType := form.Get(FormSubjectTokenType)
	name := form.Get(FormSession)
	var claims exchange.Claims
	var err error
	switch subjectTokenType {
	case oauthwire.SubjectTokenTypeKubernetes:
		if err = p.audience(subjectTokenType, subjectToken); err == nil {
			claims, err = p.exchange.Policy.Pod(subjectToken, name)
		}
	case oauthwire.SubjectTokenTypeGitHub:
		if err = p.audience(subjectTokenType, subjectToken); err == nil {
			claims, err = p.exchange.Policy.Workflow(subjectToken, name)
		}
	case tokenTypeAccessToken:
		// A machine access token is the broker's own, so there is no issuer to check it against:
		// the login that was issued it recorded the approver it acts for and the launcher
		// credential it holds, and those are the identity.
		credential, _ := subjectToken[oauthwire.ExtraCredentialID].(string)
		approver, _ := subjectToken["sub"].(string)
		claims, err = p.exchange.Policy.Machine(credential, approver, name)
	default:
		return "", oauth2.ErrInvalidRequest.WithHintf("The '%s' token type is not one this broker exchanges.", subjectTokenType)
	}
	if err != nil {
		// The refusal's own text is the hint: every sentence the exchange policy refuses with
		// names what to fix, and repeats nothing of the token but the public identity it claimed.
		return "", oauth2.ErrInvalidRequest.WithHintf("The subject token was refused: %s.", err).WithWrap(err).WithDebugError(err)
	}
	session.Subject, session.Username = claims.Person, claims.Person
	session.Principal = claims.Principal
	if session.Extra == nil {
		// A session read back out of a row that carried no extra claims has none to write on.
		session.Extra = map[string]any{}
	}
	session.Extra[ClaimSession] = claims.Session
	session.Extra[ClaimPerson] = claims.Person
	switch claims.Scope {
	case exchange.ProjectScopeNamed:
		session.Extra[ClaimProjectScope] = ProjectScopeNamed
		session.Extra[ClaimProject] = claims.Project
	default:
		session.Extra[ClaimProjectScope] = ProjectScopePerson
		delete(session.Extra, ClaimProject)
	}
	return claims.Person, nil
}

// claimSession claims the session name decide wrote onto session for the principal it decided,
// until oauthstore.SessionRetention past the expiry the library has just set on the issued token,
// in the transaction that stores that token: the claim can never end before the token it was made
// for, and a token whose claim is refused is never stored.
//
// It is where the configuration decides who a fixed name belongs to: when this provider's
// configuration fixes the name for the principal asking (the entry's own caller), the claim holds
// it for that principal with no expiry and takes it from any other holder, answering whom it took
// it from, so an entry is never refused its own name, whatever a replica started with another
// configuration reserved. Every other claim is the ordinary one, which a held name refuses: that
// one statement, oauthstore.Store.BindSession, is the only refusal of a claim.
//
// A name another principal holds is invalid_grant, because the subject token is sound and it is
// the broker's state that refuses.
func (p *Provider) claimSession(ctx context.Context, session *oauthstore.ExchangeSession) (string, error) {
	name, _ := session.Extra[ClaimSession].(string)
	expires := session.GetExpiresAt(oauth2.AccessToken)
	if name == "" || session.Principal == "" || expires.IsZero() {
		return "", oauth2.ErrServerError.WithDebugf("the exchange issued a token without deciding its session name (%t), principal (%t) and expiry (%t)",
			name != "", session.Principal != "", !expires.IsZero())
	}
	taken, err := p.store.BindSession(ctx, name, session.Principal, p.reservations[name] == session.Principal, expires)
	if err != nil {
		if errors.Is(err, oauthstore.ErrSessionHeld) {
			return "", oauth2.ErrInvalidGrant.WithHintf("The session name '%s' belongs to another identity: one the broker's configuration fixes it for, or one that has written under it within the last %d days. Ask under a name of your own.",
				name, int(oauthstore.SessionRetention/(24*time.Hour))).WithWrap(err).WithDebugError(err)
		}
		return "", oauth2.ErrServerError.WithWrap(err).WithDebugError(err)
	}
	return taken, nil
}

// liveCredential holds the launcher credential a machine access token names
// (oauthstore.LockCredential) in the exchange's transaction, until the exchanged token commits,
// and refuses an exchange whose credential is revoked or past its expiry invalid_grant, as the
// device login's own grants refuse a login that has ended.
//
// The library validated the subject token with a plain read of its row, which a revoke can
// deactivate a moment later; the exchanged token names the same credential (its session is read
// out of the subject token's row), so without the lock the token could commit after the revoke
// had ended the credential's tokens and stay live for its whole lifetime. With it, the revoke
// either commits first and the exchange is refused, or waits and then ends the exchanged token
// with the rest.
func (p *Provider) liveCredential(ctx context.Context, subjectToken map[string]any) error {
	credential, _ := subjectToken[oauthwire.ExtraCredentialID].(string)
	err := oauthstore.LockCredential(ctx, credential)
	if errors.Is(err, oauthstore.ErrCredentialEnded) {
		return oauth2.ErrInvalidGrant.WithHint("The machine login the subject token belongs to has ended.").WithWrap(err).WithDebugError(err)
	}
	if err != nil {
		return oauth2.ErrServerError.WithWrap(err).WithDebugError(err)
	}
	return nil
}

// audience refuses a subject token whose `aud` does not carry the audience its issuer is
// configured with. The library checks a subject token's issuer and its expiry and never its
// audience, so without this a token a pod was given for another service would be spendable here.
func (p *Provider) audience(subjectTokenType string, subjectToken map[string]any) error {
	issuer, ok := p.exchange.issuerOf(subjectTokenType)
	if !ok {
		return nil
	}
	if hasAudience(subjectToken["aud"], issuer.Audience) {
		return nil
	}
	return fmt.Errorf("the subject token was not minted for %s, which is the audience this broker accepts from %s", issuer.Audience, issuer.Issuer)
}

// hasAudience reports whether an `aud` claim carries want. RFC 7519 Section 4.1.3 allows either
// one string or an array of them, and a claim map decoded from JSON holds the array as []any.
func hasAudience(claim any, want string) bool {
	switch audience := claim.(type) {
	case string:
		return audience == want
	case []string:
		return slices.Contains(audience, want)
	case []any:
		for _, entry := range audience {
			if text, ok := entry.(string); ok && text == want {
				return true
			}
		}
	}
	return false
}
