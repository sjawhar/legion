package oauth

import (
	"context"
	"crypto"
	"encoding/json"
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
	// ClaimService is the service a token that names no person is, and is empty for every token
	// that names one. It is written always, for the reason ClaimProjectScope is: a reader of the
	// row never has to take an absent service as either "no service" or "a claim this build of
	// the broker did not write".
	ClaimService = "service"
	// ClaimMachine is the machine a person signed in on acted through: the Google account the
	// trade's actor token named (exchange.Claims.Actor). It is set only beside ClaimPerson, and
	// only for a person's sign-in; it is absent from every other token, which no machine acted
	// for. The RFC 8693 'act' claim the library writes beside it records the same delegation as
	// the actor token's issuer spells it, a Google subject rather than an account, so this is
	// the name an introspection can answer and a reader can act on.
	ClaimMachine = "machine"
	// ClaimProjectScope is what ClaimProject means, one of oauthwire.ProjectScopeNamed or
	// oauthwire.ProjectScopePerson. It is written always, so a reader never has to take an
	// absent project as either "every project" or "none".
	ClaimProjectScope = "project_scope"
	// ClaimProject is the one project key a token of the named scope may write, and is absent
	// from one of the person scope, which names none.
	ClaimProject = "project"
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
	// for expiring too far in the future. It is also what a pod-session registration is kept for
	// past its last write (oauthstore.PodSessionRetention), since a registration must outlive
	// every token the pod it was written for can still present.
	kubernetesTolerance = 24 * time.Hour
	// githubTolerance admits a workflow run's token, which lives minutes, while bounding how
	// stale an iat the exchange accepts.
	githubTolerance = time.Hour
	// googleTolerance admits a machine's Google identity token, which Google mints for an hour
	// and which its metadata server hands out again until it is nearly spent, so the window has
	// to cover a whole lifetime as the GitHub one does rather than a fresh token alone.
	googleTolerance = time.Hour
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
	// Google is the issuer a machine's identity token comes from, Google's own in a deployment
	// and a stand-in for it on a dev stack.
	Google IssuerConfig
	// MachineCredentials creates or re-uses the launcher credential a machine holds, in the
	// transaction the exchange carries (oauthstore.TxFrom), and answers its id. Every secrets
	// route is an enrollment's, and an enrollment is made under a launcher credential and a
	// proof signed by its key, so without this a machine holding a Dispatch access token could
	// still read no secret. It is required of a broker that accepts a Google subject token and
	// unused by one that does not; enroll.Service.IssueTokenLoginTx is what the broker's wiring
	// hands over.
	MachineCredentials MachineCredentials
	// PersonAudience is what a person's own Google identity token's `aud` must carry
	// (BROKER_SIGNIN_DEVICE_AUDIENCE): the device-flow client `agent-secrets login` runs
	// Google's device grant as. It is an audience of its own beside Google.Audience, which is a
	// machine's, so neither token is spendable where the other is: a machine's identity token
	// cannot be presented as the person it acts for, and a person's sign-in token cannot be
	// presented as a machine. Empty is a broker that accepts no person's sign-in at all, whose
	// person subject token type and machine actor token type are then not registered, so a
	// caller presenting either is refused by the library before any handler of the broker's
	// runs. Its issuer and keys are Google's own, since both tokens come from the one issuer.
	PersonAudience string
	// PersonSignIns records the sign-in of the person a trade names on the box key its DPoP
	// proof presented, in the transaction the exchange carries, and refuses with ErrSignInRevoked
	// a trade whose key that person has revoked. It is what makes a revoked sign-in stop working
	// at the broker rather than at the person's own machine, and it is required of a broker that
	// accepts a person's Google identity token and unused by one that does not.
	PersonSignIns PersonSignIns
}

// validate refuses a configuration the exchange could not serve.
func (c ExchangeConfig) validate() error {
	if err := c.Kubernetes.validate("Kubernetes"); err != nil {
		return err
	}
	if err := c.GitHub.validate("GitHub"); err != nil {
		return err
	}
	if err := c.Google.validate("Google"); err != nil {
		return err
	}
	if c.Google.configured() && c.MachineCredentials == nil {
		return errors.New("oauth: a broker that accepts a machine's Google identity token needs MachineCredentials: the exchange mints the machine's launcher credential, and without it no machine it answers could enroll or read a secret")
	}
	if c.PersonAudience != "" && !c.Google.configured() {
		return errors.New("oauth: a broker that accepts a person's Google identity token needs the Google issuer too: the machine the person signs in on presents its own identity token as the actor, and the entries that list that machine are the Google ones")
	}
	if c.PersonAudience != "" && c.PersonSignIns == nil {
		return errors.New("oauth: a broker that accepts a person's Google identity token needs PersonSignIns: the first trade records the sign-in the box key belongs to, and every later one is held to it")
	}
	return nil
}

// MachineCredential is the launcher credential one machine's exchange asks for. The broker's
// wiring turns it into a row; nothing here is the provider's to decide.
type MachineCredential struct {
	// Subject keys the credential, so a machine's next exchange moves the one credential it
	// already holds rather than adding another (exchange.MachineSubject).
	Subject string
	// Service is the fleet the machine's account is listed for (exchange.Claims.Fleet). A
	// credential keyed by a subject carries one, which is what says the login is a service's
	// and no person approved it.
	Service string
	// Operator is the person the machine's fleet writes for, and is what makes its enrollments
	// that person's sessions, exactly as a dev box's machine login does. It is empty for a fleet
	// whose entry names no person, whose token names its service instead.
	Operator string
	// Host is the machine as an operator reads it on their machine-login page:
	// <project>/<zone>/<instance name> for a machine minted on a Compute Engine instance, and
	// the account for one minted anywhere else.
	Host string
	// Thumbprint and JWK are the key the request's DPoP proof presented, which is the key the
	// machine's later launcher proofs are signed with.
	Thumbprint string
	JWK        json.RawMessage
	// Expires is when the credential ends: the subject token's own expiry, so a machine that
	// stops being able to mint an identity token stops enrolling within the hour Google bounds
	// one at.
	Expires time.Time
}

// MachineCredentials creates or re-uses the credential cred describes, in the transaction ctx
// carries, and answers its id.
type MachineCredentials func(ctx context.Context, cred MachineCredential) (string, error)

// PersonSignIn is one person's sign-in on one machine as the exchange decided it. The broker's
// wiring turns it into a row; nothing here is the provider's to decide.
type PersonSignIn struct {
	// Person is the login the person's own Google identity token names, the token's person.
	Person string
	// Thumbprint is the RFC 7638 thumbprint of the box key the request's DPoP proof presented:
	// the key `agent-secrets login` made at the sign-in and keeps with Google's refresh token,
	// which is what tells this sign-in from the person's others.
	Thumbprint string
	// Machine is the Google account of the machine the person signed in on, as the actor token
	// named it and BROKER_VM_SERVICES lists it.
	Machine string
}

// PersonSignIns records the sign-in in, in the transaction ctx carries, and answers
// ErrSignInRevoked when that person has revoked that box key.
type PersonSignIns func(ctx context.Context, in PersonSignIn) error

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
	if c.Google.configured() {
		types[oauthwire.SubjectTokenTypeGoogle] = &rfc8693.JWTType{
			Name:   oauthwire.SubjectTokenTypeGoogle,
			Issuer: c.Google.Issuer,
			JWTValidationConfig: rfc8693.JWTValidationConfig{
				// A machine reads one identity token from its metadata server and is handed the
				// same token until it is nearly spent, so a machine renewing its access token
				// presents the one it holds again and single use would refuse every renewal, as
				// it would a pod's. What keeps such a token from being spendable by anyone who
				// copied it is the DPoP key the minted token is bound to.
				ValidateJTI:                false,
				JWTLifetimeToleranceWindow: googleTolerance,
				ValidateFunc:               keyFunc(c.Google.Keys),
			},
		}
	}
	if c.PersonAudience != "" {
		// A person's own token and the machine's actor token come from the one issuer and are
		// checked against the one key set; what tells them apart is the audience each must
		// carry, which the broker checks itself (audience). Both are registered together: a
		// person's trade carries both, and neither is presentable alone — the person type as a
		// subject with no actor is refused by the decision, and the machine type is an actor
		// type the exchange client may present and no subject type at all.
		for _, name := range []string{oauthwire.SubjectTokenTypePerson, oauthwire.ActorTokenTypeMachine} {
			types[name] = &rfc8693.JWTType{
				Name:   name,
				Issuer: c.Google.Issuer,
				JWTValidationConfig: rfc8693.JWTValidationConfig{
					// A person's sign-in holds Google's refresh token and mints an identity
					// token from it whenever one is wanted, and the machine presents the same
					// metadata token its own trades do, so neither is single use, for the
					// reason a machine's subject token is not. What keeps a copied pair from
					// being spendable is the box key the sign-in is recorded against.
					ValidateJTI:                false,
					JWTLifetimeToleranceWindow: googleTolerance,
					ValidateFunc:               keyFunc(c.Google.Keys),
				},
			}
		}
	}
	return types
}

// issuerOf is the configured issuer a token type's tokens come from, and false for a type that
// is not one of the JWT kinds. The machine's actor token is Google's own, the same token its
// subject type names; a person's carries the device-flow client's audience over Google's issuer
// and keys, so it is answered as an issuer of its own.
func (c ExchangeConfig) issuerOf(subjectTokenType string) (IssuerConfig, bool) {
	switch subjectTokenType {
	case oauthwire.SubjectTokenTypeKubernetes:
		return c.Kubernetes, true
	case oauthwire.SubjectTokenTypeGitHub:
		return c.GitHub, true
	case oauthwire.SubjectTokenTypeGoogle, oauthwire.ActorTokenTypeMachine:
		return c.Google, true
	case oauthwire.SubjectTokenTypePerson:
		return IssuerConfig{Issuer: c.Google.Issuer, Audience: c.PersonAudience, Keys: c.Google.Keys}, true
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
// credential's row (liveCredential) until the token commits; the three JWT types took their
// decision during validation, through Store.ExchangeSubject. The Google subject type mints or
// moves the machine's own launcher credential here (machineCredential), for the same reason and
// in the same transaction: the answer names it, and a machine whose token committed without it
// could enroll nothing. The person subject type records the sign-in its box key belongs to here
// (personSignIn), for the same reason again: a revoke that commits first refuses the trade, and
// one that commits after it ends the token the trade minted. Beside those, and for every
// type, the registration a pod's session name needs is read before the library builds anything
// (registeredPodSession). The claim on the session name is made
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
	var credential MachineCredential
	var credentialID string
	if request.GetRequestForm().Get(FormSubjectTokenType) == oauthwire.SubjectTokenTypeGoogle {
		credentialID, credential, err = p.machineCredential(txCtx, r, session)
		if err != nil {
			p.writeError(ctx, w, request, err)
			return
		}
	}
	if request.GetRequestForm().Get(FormSubjectTokenType) == oauthwire.SubjectTokenTypePerson {
		if err := p.personSignIn(txCtx, session); err != nil {
			p.writeError(ctx, w, request, err)
			return
		}
	}
	if err := p.registeredPodSession(txCtx, session); err != nil {
		p.writeError(ctx, w, request, err)
		return
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
	if credentialID != "" {
		// Named in the answer as the device login's own token answer names it, so one client
		// reads a credential the same way whichever grant gave it one.
		resp.SetExtra(oauthwire.ExtraCredentialID, credentialID)
		resp.SetExtra(oauthwire.ExtraCredentialExpiresAt, credential.Expires.UTC().Format(time.RFC3339))
	}
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
// — the four JWT subject types through Store.ExchangeSubject while the library validates the
// request, the machine access-token type from exchangeGrant once it has — so one function decides
// for all five.
//
// In order: the subject token's audience, which the library never checks and which a token minted
// for another service would fail; the policy's decision, which names the session, the person or
// the service, the project and the principal; and then the claims themselves, onto the session,
// where the row the token is stored in carries them and an introspection answers from them, with
// the principal beside them for claimSession.
//
// The subject the library asks for is the principal, which every decision names: a token that
// names a service and no person has no person to be the subject of, and a subject that is
// sometimes empty would leave the library's own 'sub' saying nothing. The principal says which
// listed identity asked, which is what a reader of the stored row wants of a subject.
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
	case oauthwire.SubjectTokenTypeGoogle:
		if err = p.audience(subjectTokenType, subjectToken); err == nil {
			claims, err = p.exchange.Policy.Google(subjectToken, name)
		}
	case oauthwire.SubjectTokenTypePerson:
		// Two tokens, each held to the audience its own type is configured with: the person's
		// to the device-flow client's, the machine's to the one a machine trades for itself
		// with, so neither is spendable in the other's place. The actor token's claims are on
		// the session because the library validated it before it asked for the subject
		// (rfc8693.CustomJWTTypeHandler), and a request that carried none leaves it nil, which
		// the policy refuses by name.
		actor := session.GetActorToken()
		if err = p.audience(subjectTokenType, subjectToken); err == nil && actor != nil {
			err = p.audience(oauthwire.ActorTokenTypeMachine, actor)
		}
		if err == nil {
			claims, err = p.exchange.Policy.Person(subjectToken, actor, name)
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
	session.Subject, session.Username = claims.Principal, claims.Principal
	session.Principal, session.Instance, session.Fleet = claims.Principal, claims.Instance, claims.Fleet
	if session.Extra == nil {
		// A session read back out of a row that carried no extra claims has none to write on.
		session.Extra = map[string]any{}
	}
	session.Extra[ClaimSession] = claims.Session
	session.Extra[ClaimPerson] = claims.Person
	session.Extra[ClaimService] = claims.Service
	if claims.Actor == "" {
		// Absent rather than empty, unlike the service: every token carries a person or a
		// service, so an absent one of those would be ambiguous, where a machine is the mark of
		// the one kind of trade that has an actor at all.
		delete(session.Extra, ClaimMachine)
	} else {
		session.Extra[ClaimMachine] = claims.Actor
	}
	switch claims.Scope {
	case exchange.ProjectScopeNamed:
		session.Extra[ClaimProjectScope] = oauthwire.ProjectScopeNamed
		session.Extra[ClaimProject] = claims.Project
	default:
		session.Extra[ClaimProjectScope] = oauthwire.ProjectScopePerson
		delete(session.Extra, ClaimProject)
	}
	return claims.Principal, nil
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

// machineCredential creates or re-uses the launcher credential the machine presenting a Google
// identity token holds (ExchangeConfig.MachineCredentials), in the exchange's own transaction,
// and writes its id onto the session so the stored token names it. It answers the id and what it
// asked for, which is what the token answer's extras repeat.
//
// It is the bridge between the two halves of what a machine does: the exchange answers a
// Dispatch access token, which writes Dispatch and reads no secret, and every secrets route is
// an enrollment's, made under a launcher credential and a proof signed by its key. One trade
// gives the machine both, and both end together, because the credential is written in the
// transaction the token is written in and expires when the subject token does.
//
// The credential is keyed by the machine (exchange.MachineSubject) and bound to the key the
// request's DPoP proof presented, so a machine's next exchange moves the one credential it holds
// and no machine can move another's. Its operator is the person the fleet's entry names, which
// is what makes the machine's enrollments that person's sessions, as a dev box's machine login
// does; a fleet listed with no person has none, and such a machine holds a credential that
// enrolls nothing until its entry names one.
//
// A key another live credential holds is invalid_grant rather than a server error: the broker's
// state refuses it and the machine fixes it by making a key of its own, which is what every
// machine does.
func (p *Provider) machineCredential(ctx context.Context, r *http.Request, session *oauthstore.ExchangeSession) (string, MachineCredential, error) {
	subjectToken := session.GetSubjectToken()
	identity := exchange.ReadGoogleIdentity(subjectToken)
	thumbprint := session.GetDPoPJWKThumbprint()
	key, err := proofKey(r, thumbprint)
	if err != nil {
		return "", MachineCredential{}, oauth2.ErrServerError.WithWrap(err).WithDebugError(err)
	}
	expires, ok := expiryOf(subjectToken)
	if !ok {
		return "", MachineCredential{}, oauth2.ErrInvalidRequest.WithHint("The subject token names no expiry, so there is nothing to expire the machine's credential with.")
	}
	person, _ := session.Extra[ClaimPerson].(string)
	cred := MachineCredential{
		Subject:    exchange.MachineSubject(identity, thumbprint),
		Service:    session.Fleet,
		Operator:   person,
		Host:       machineHost(identity),
		Thumbprint: thumbprint,
		JWK:        key,
		Expires:    expires,
	}
	id, err := p.exchange.MachineCredentials(ctx, cred)
	if err != nil {
		if errors.Is(err, ErrMachineKeyTaken) {
			return "", MachineCredential{}, oauth2.ErrInvalidGrant.WithHint("Another live machine login holds the key this proof was signed with. A machine makes a key of its own.").WithWrap(err).WithDebugError(err)
		}
		return "", MachineCredential{}, oauth2.ErrServerError.WithWrap(err).WithDebugError(err)
	}
	if session.Extra == nil {
		session.Extra = map[string]any{}
	}
	// On the session, not only in the answer, because the row the token is stored in is what a
	// second exchange of that token reads its credential back out of (decide, the access-token
	// subject type).
	session.Extra[oauthwire.ExtraCredentialID] = id
	return id, cred, nil
}

// personSignIn records the sign-in the person's trade belongs to
// (ExchangeConfig.PersonSignIns), in the exchange's own transaction, and refuses a trade whose
// box key that person has revoked.
//
// The sign-in is the person, the box key and the machine: the person the decision named, the
// RFC 7638 thumbprint of the key the request's DPoP proof presented — which is the key
// `agent-secrets login` made at the sign-in and keeps with Google's refresh token — and the
// Google account the actor token named. The first trade of a person and a box key writes the
// row; every later trade of that pair is the same sign-in, used again.
//
// It runs in the transaction the token is written in, for the reason the machine's credential
// does: a revoke that commits first refuses this trade, and one that commits after it ends the
// token this trade minted, so no token outlives the sign-in it was minted under.
//
// A revoked key is invalid_grant rather than a server error, and names the sign-in command: the
// subject token is sound and it is the broker's own record that refuses, which the person fixes
// by signing in again, with a key of its own.
func (p *Provider) personSignIn(ctx context.Context, session *oauthstore.ExchangeSession) error {
	person, _ := session.Extra[ClaimPerson].(string)
	machine, _ := session.Extra[ClaimMachine].(string)
	thumbprint := session.GetDPoPJWKThumbprint()
	if person == "" || machine == "" || thumbprint == "" {
		return oauth2.ErrServerError.WithDebugf("the exchange reached a person's sign-in with person (%t), machine (%t) and bound key (%t)",
			person != "", machine != "", thumbprint != "")
	}
	err := p.exchange.PersonSignIns(ctx, PersonSignIn{Person: person, Thumbprint: thumbprint, Machine: machine})
	if errors.Is(err, ErrSignInRevoked) {
		return oauth2.ErrInvalidGrant.WithHintf("%s revoked this machine's sign-in, so the key this proof was signed with trades nothing. Sign in again with `agent-secrets login`.", person).WithWrap(err).WithDebugError(err)
	}
	if err != nil {
		return oauth2.ErrServerError.WithWrap(err).WithDebugError(err)
	}
	return nil
}

// machineHost is the machine as its credential's row records it, which is what an operator reads
// on their machine-login page: the instance a Compute Engine token was minted on, and the
// account alone for a token minted anywhere else, which names no instance to show.
func machineHost(identity exchange.GoogleIdentity) string {
	if identity.Compute.Whole() {
		return identity.Compute.Project + "/" + identity.Compute.Zone + "/" + identity.Compute.Instance
	}
	return identity.Account
}

// proofKey is the public key of the DPoP proof on r, as JSON, which the machine's launcher
// credential records beside its thumbprint. The library has already verified this proof and
// bound the session to its key, and it publishes the parsed key only inside its own request
// handling, so the header is read again here and held to the thumbprint the library bound: a
// key whose thumbprint is not that one is not the key this request proved.
func proofKey(r *http.Request, thumbprint string) (json.RawMessage, error) {
	header := r.Header.Get("DPoP")
	if header == "" || thumbprint == "" {
		return nil, fmt.Errorf("oauth: the exchange reached the machine's credential with no DPoP proof (%t) or no bound key (%t)", header != "", thumbprint != "")
	}
	proof, err := jose.ParseSigned(header, subjectTokenAlgorithms)
	if err != nil {
		return nil, fmt.Errorf("oauth: re-read the request's DPoP proof: %w", err)
	}
	if len(proof.Signatures) != 1 || proof.Signatures[0].Header.JSONWebKey == nil {
		return nil, fmt.Errorf("oauth: the request's DPoP proof carries %d signatures and no embedded key", len(proof.Signatures))
	}
	key := proof.Signatures[0].Header.JSONWebKey.Public()
	jkt, err := jwt.ThumbprintJWK(&key)
	if err != nil {
		return nil, fmt.Errorf("oauth: the thumbprint of the DPoP proof's key: %w", err)
	}
	if jkt != thumbprint {
		return nil, fmt.Errorf("oauth: the DPoP proof's key is %s and the request is bound to %s", jkt, thumbprint)
	}
	raw, err := json.Marshal(&key)
	if err != nil {
		return nil, fmt.Errorf("oauth: the DPoP proof's key as JSON: %w", err)
	}
	return raw, nil
}

// expiryOf is a verified token's `exp` as a time. A claim map decoded from JSON holds a number
// as float64, and the library's own parse holds one as json.Number, so both are read; ok is
// false for a token that names no expiry at all.
func expiryOf(token map[string]any) (time.Time, bool) {
	switch value := token["exp"].(type) {
	case float64:
		return time.Unix(int64(value), 0), true
	case int64:
		return time.Unix(value, 0), true
	case json.Number:
		seconds, err := value.Int64()
		return time.Unix(seconds, 0), err == nil
	}
	return time.Time{}, false
}

// registeredPodSession refuses an exchange whose claims name an instance (exchange.Claims.Instance,
// a pod uid) that no registration names for the session the token would write under, reading
// oauthstore.Store.PodSessionRegistered in the exchange's own transaction. An exchange whose
// claims carry no instance is not gated: a fixed-session entry's pod, a workflow run and a dev box
// each write under a name their configuration or their person decides, and nothing of theirs is
// registered.
//
// It is the second, independent rule beside the claim on the session name: the claim refuses
// another principal the name, and this refuses another pod of the same principal, which the claim
// cannot tell apart because the name belongs to the account. It runs here rather than inside
// decide because a refusal raised through the library's store hook arrives as invalid_request,
// and this one is invalid_grant: the subject token is sound and it is the broker's state that
// refuses it, exactly as a session name another principal holds is.
func (p *Provider) registeredPodSession(ctx context.Context, session *oauthstore.ExchangeSession) error {
	if session.Instance == "" {
		return nil
	}
	name, _ := session.Extra[ClaimSession].(string)
	registered, err := p.store.PodSessionRegistered(ctx, session.Instance, name)
	if err != nil {
		return oauth2.ErrServerError.WithWrap(err).WithDebugError(err)
	}
	if !registered {
		return oauth2.ErrInvalidGrant.WithHintf("No registration says this pod may write under the session name '%s'. A pod writes under a session name its deployment registered for it.", name)
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
