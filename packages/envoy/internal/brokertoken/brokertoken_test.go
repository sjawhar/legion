package brokertoken_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/brokertoken/brokertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

const (
	// dispatchOrigin is DISPATCH_SERVER_URL for these tests: the origin a caller signs its
	// proof's 'htu' against, whatever host the request reaches Dispatch under.
	dispatchOrigin = "https://dispatch.test"
	// resourcePath is the route a caller presents its token to.
	resourcePath = "/api/v1/sessions"
	// accountToken is what the stub broker expects Dispatch to introspect as, standing in for
	// the pod's projected ServiceAccount token.
	accountToken = "dispatchs-own-projected-service-account-token"
	// callerAddress is the source address the rate-limiter buckets are keyed on.
	callerAddress = "203.0.113.7:51789"
)

// clock is a test's own reading of now for the cache and the address buckets, so a test proves
// what happens after the cache lapses without sleeping through it. A proof's own acceptance
// window is the library's, on the real clock, so this never moves far enough to matter there.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *clock { return &clock{at: time.Now()} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// signer produces the requests a caller holding one key sends, through the shared stub caller so
// that every proof in this suite is the one the real producer makes rather than a second signer
// written here.
type signer struct {
	t      *testing.T
	caller *brokertokentest.Caller
	jkt    string
}

// newSigner is a caller with a key of its own.
func newSigner(t *testing.T) *signer {
	t.Helper()
	caller := brokertokentest.NewCaller(t)
	return &signer{t: t, caller: caller, jkt: caller.JKT()}
}

// request is one signed request presenting token at url, as Dispatch's handler would see it.
func (s *signer) request(url, token string) *http.Request {
	s.t.Helper()
	signed := s.caller.Signed(s.t, http.MethodGet, url, token)
	signed.RemoteAddr = callerAddress
	return signed
}

// at is a signed GET of path under Dispatch's own origin.
func (s *signer) at(path, token string) *http.Request {
	return s.request(dispatchOrigin+path, token)
}

// rig is one verifier, the broker it asks and the clock it reads.
type rig struct {
	broker   *brokertokentest.StubBroker
	clock    *clock
	verifier *brokertoken.Verifier
}

// newRig builds a verifier on a database of its own, with options applied before it is built.
func newRig(t *testing.T, options ...func(*brokertoken.Options)) *rig {
	t.Helper()
	database := storetest.Open(t)
	broker := brokertokentest.NewStubBroker(t)
	tick := newClock()
	opts := brokertoken.Options{
		BrokerURL: broker.Server.URL,
		Account:   brokertoken.NewAccountToken(accountTokenFile(t, accountToken)),
		Origin:    dispatchOrigin,
		Replay:    store.NewDPoPProofs(database.Pool),
		Address:   sourceAddress,
		Now:       tick.now,
	}
	for _, option := range options {
		option(&opts)
	}
	verifier, err := brokertoken.New(opts)
	if err != nil {
		t.Fatalf("brokertoken.New: %v", err)
	}
	return &rig{broker: broker, clock: tick, verifier: verifier}
}

// sourceAddress is the deployment's rule for which address a bucket is keyed on, as Options
// takes it: every request of this suite comes from callerAddress, so one bucket serves a test.
// Dispatch passes auth.ForwardedAddress here, which this package no longer reads for itself.
func sourceAddress(r *http.Request) string { return r.RemoteAddr }

// accountTokenFile writes token to a file of the test's own directory, as a pod's projected
// ServiceAccount token is mounted.
func accountTokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write the account token file: %v", err)
	}
	return path
}

// liveToken is the broker's answer for a token it issued to jkt for acting as a session,
// expiring in life. Its client is the exchange every such token comes from, which the check
// holds an answer to: a token issued to any other client is no session's credential.
func liveToken(jkt string, life time.Duration) brokertoken.Introspection {
	return brokertoken.Introspection{
		Active:       true,
		Session:      "ses_a1b2c3",
		Person:       "sjawhar",
		ProjectScope: brokertoken.ProjectScopeNamed,
		Project:      "INFRA",
		Scope:        "dispatch.write",
		ExpiresAt:    time.Now().Add(life).Unix(),
		ClientID:     brokertoken.ExchangeClientID,
		Thumbprint:   jkt,
	}
}

// machineLoginToken is the answer a machine login's own access token gets: the credential the
// agent-secrets helper holds to ask the broker for everything else. It is live and DPoP-bound
// like any other, and no exchange decided a session, a person or a project scope for it, so
// the broker names none.
func machineLoginToken(jkt string, life time.Duration) brokertoken.Introspection {
	return brokertoken.Introspection{
		Active:     true,
		Scope:      "machine",
		ExpiresAt:  time.Now().Add(life).Unix(),
		ClientID:   "agent-secrets-helper",
		Thumbprint: jkt,
	}
}

// TestATokenThatIsNoSessionsCredentialIsRefused: a live, DPoP-bound token the broker issued for
// something other than acting as a session names no session to record writes as, so it is
// refused with an outcome of its own rather than handed back as a Caller for whoever mounts
// this check to fill in. Each leg of the rule is its own token: a machine login's own, one the
// exchange issued that names no person, and one that names a session and a person but comes
// from another client. The proof-checked Verify and the answer-only Live refuse all three, so
// an open connection cannot keep one alive either.
func TestATokenThatIsNoSessionsCredentialIsRefused(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	noPerson := liveToken(caller.jkt, time.Hour)
	noPerson.Person = ""
	anotherClient := liveToken(caller.jkt, time.Hour)
	anotherClient.ClientID = "agent-secrets-helper"
	for name, answer := range map[string]brokertoken.Introspection{
		"a machine login's own token": machineLoginToken(caller.jkt, time.Hour),
		"a token naming no person":    noPerson,
		"a token of another client":   anotherClient,
	} {
		t.Run(name, func(t *testing.T) {
			token := "a-token-that-is-" + strings.ReplaceAll(name, " ", "-")
			rig.broker.Says(token, answer)

			decided, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token))
			if !errors.Is(err, brokertoken.ErrNotASession) {
				t.Fatalf("Verify answered %v, want ErrNotASession", err)
			}
			if decided != (brokertoken.Caller{}) {
				t.Fatalf("Verify = %+v, want no caller at all", decided)
			}
			live, err := rig.verifier.Live(t.Context(), caller.at(resourcePath, token))
			if !errors.Is(err, brokertoken.ErrNotASession) {
				t.Fatalf("Live answered %v, want ErrNotASession", err)
			}
			if live != (brokertoken.Caller{}) {
				t.Fatalf("Live = %+v, want no caller at all", live)
			}
		})
	}

	// The negative control: the same shape with every claim a session's token carries is the
	// caller it has always been.
	const sound = "a-token-the-exchange-issued"
	rig.broker.Says(sound, liveToken(caller.jkt, time.Hour))
	if _, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, sound)); err != nil {
		t.Fatalf("a session's own token was refused: %v", err)
	}
}

// TestALiveTokenWithASoundProofIsAccepted: the two checks together admit the caller, and what
// the broker said the token is reaches the caller's decision.
func TestALiveTokenWithASoundProofIsAccepted(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	const token = "a-token-the-broker-issued"
	rig.broker.Says(token, liveToken(caller.jkt, time.Hour))

	decided, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token))
	if err != nil {
		t.Fatalf("Verify refused a live token with a sound proof: %v", err)
	}
	if decided != (brokertoken.Caller{Session: "ses_a1b2c3", Person: "sjawhar", Project: "INFRA"}) {
		t.Fatalf("Verify = %+v, want the session, the person and the project and nothing else of the answer", decided)
	}
	if rig.broker.Count() != 1 {
		t.Fatalf("the broker was asked %d times, want once", rig.broker.Count())
	}
	if want := "Bearer " + accountToken; rig.broker.Bearers()[0] != want {
		t.Fatalf("the broker was presented %q, want Dispatch's own %q", rig.broker.Bearers()[0], want)
	}
}

// TestAPersonScopedTokenNamesNoProject: the broker's two project scopes are read where the
// Caller is built, so a reader takes Project as it stands. A token scoped to its person writes
// wherever that person may, which is no one project, and carries none even where the answer
// still names one.
func TestAPersonScopedTokenNamesNoProject(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	const token = "a-dev-box-sessions-token"
	answer := liveToken(caller.jkt, time.Hour)
	answer.ProjectScope = brokertoken.ProjectScopePerson
	rig.broker.Says(token, answer)

	decided, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token))
	if err != nil {
		t.Fatalf("Verify refused a person-scoped token: %v", err)
	}
	if decided != (brokertoken.Caller{Session: "ses_a1b2c3", Person: "sjawhar"}) {
		t.Fatalf("Verify = %+v, want the session and the person with no project", decided)
	}
}

// TestOneTokenCostsOneIntrospectionInsideTheCachesLife: the cache is what keeps this check off
// the broker's account-class bucket, so many requests bearing one live token cost one
// introspection, and the answer is dropped at the token's own expiry rather than held for the
// whole window.
func TestOneTokenCostsOneIntrospectionInsideTheCachesLife(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	const token = "a-token-presented-again-and-again"
	// Two seconds, inside AffirmativeCacheTTL, so the answer's own expiry is what ends it.
	rig.broker.Says(token, liveToken(caller.jkt, 2*time.Second))

	for request := range 8 {
		if _, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token)); err != nil {
			t.Fatalf("request %d: %v", request, err)
		}
	}
	if rig.broker.Count() != 1 {
		t.Fatalf("8 requests cost %d introspections, want 1", rig.broker.Count())
	}

	rig.clock.advance(3 * time.Second)
	if _, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token)); err != nil {
		t.Fatalf("the request past the answer's expiry: %v", err)
	}
	if rig.broker.Count() != 2 {
		t.Fatalf("the request past the answer's expiry cost %d introspections in all, want 2", rig.broker.Count())
	}
}

// TestRequestsArrivingAtOnceWithOneTokenCostOneIntrospection: the single flight is what makes a
// burst on a cold cache cost one introspection rather than one a request, and the address bucket
// is spent inside that flight, so the burst costs one request of it rather than one each. The
// bucket here holds exactly one, so charging per request would refuse every request but the
// first — which is what an honest caller making a burst of calls on a token it has just been
// given would meet.
func TestRequestsArrivingAtOnceWithOneTokenCostOneIntrospection(t *testing.T) {
	rig := newRig(t, func(o *brokertoken.Options) {
		o.AddressLimit = ratelimit.Limit{Every: time.Hour, Burst: 1}
	})
	caller := newSigner(t)
	const token = "a-token-many-requests-arrive-with-at-once"
	rig.broker.Says(token, liveToken(caller.jkt, time.Hour))

	// Each request needs its own proof: a replayed one is refused, which is a different check.
	const requests = 8
	signed := make([]*http.Request, requests)
	for i := range signed {
		signed[i] = caller.at(fmt.Sprintf("%s/%d", resourcePath, i), token)
	}
	var wait sync.WaitGroup
	failures := make([]error, requests)
	for i := range signed {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, failures[i] = rig.verifier.Verify(t.Context(), signed[i])
		}()
	}
	wait.Wait()
	for i, err := range failures {
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if rig.broker.Count() != 1 {
		t.Fatalf("%d requests arriving at once cost %d introspections, want 1", requests, rig.broker.Count())
	}
}

// TestATokenTheBrokerCallsNotLiveIsRefusedAndNeverRemembered: a refusal is never served from the
// cache, so a token the broker reinstates is accepted on its next presentation rather than kept
// refused.
func TestATokenTheBrokerCallsNotLiveIsRefusedAndNeverRemembered(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	const token = "a-token-the-broker-does-not-know"

	for request := range 2 {
		_, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token))
		if !errors.Is(err, brokertoken.ErrNotActive) {
			t.Fatalf("request %d answered %v, want ErrNotActive", request, err)
		}
	}
	if rig.broker.Count() != 2 {
		t.Fatalf("two presentations of a refused token cost %d introspections, want 2", rig.broker.Count())
	}

	rig.broker.Says(token, liveToken(caller.jkt, time.Hour))
	if _, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token)); err != nil {
		t.Fatalf("the token the broker now calls live: %v", err)
	}
}

// TestAProofSignedByAnotherKeyIsRefused: holding the token is not enough, which is what the
// binding is for.
func TestAProofSignedByAnotherKeyIsRefused(t *testing.T) {
	rig := newRig(t)
	owner, thief := newSigner(t), newSigner(t)
	const token = "a-token-bound-to-the-owners-key"
	rig.broker.Says(token, liveToken(owner.jkt, time.Hour))

	_, err := rig.verifier.Verify(t.Context(), thief.at(resourcePath, token))
	if !errors.Is(err, brokertoken.ErrKeyMismatch) {
		t.Fatalf("a proof signed by another key answered %v, want ErrKeyMismatch", err)
	}
}

// TestAReplayedProofIsRefused: the same proof presented twice is refused, which is Dispatch's
// replay store doing its work — the second presentation asks the broker nothing, so only the
// store can have refused it.
func TestAReplayedProofIsRefused(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	const token = "a-token-whose-proof-is-captured"
	rig.broker.Says(token, liveToken(caller.jkt, time.Hour))
	signed := caller.at(resourcePath, token)

	if _, err := rig.verifier.Verify(t.Context(), signed); err != nil {
		t.Fatalf("the first presentation: %v", err)
	}
	_, err := rig.verifier.Verify(t.Context(), signed)
	if !errors.Is(err, brokertoken.ErrProofInvalid) {
		t.Fatalf("the replayed proof answered %v, want ErrProofInvalid", err)
	}
	if rig.broker.Count() != 1 {
		t.Fatalf("the replay cost %d introspections in all, want 1: it was refused by the store", rig.broker.Count())
	}
}

// TestTheProofIsCheckedAgainstDispatchsOwnOriginNotAForwardedHeader: the library rebuilds the
// expected 'htu' from X-Forwarded-Proto and the Host, both of which a caller behind a proxy can
// choose, so Dispatch checks against DISPATCH_SERVER_URL plus the request's path instead. A
// proof naming that origin is accepted however the request is addressed, and one naming an
// origin the caller picked is refused.
func TestTheProofIsCheckedAgainstDispatchsOwnOriginNotAForwardedHeader(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	const token = "a-token-presented-through-a-proxy"
	rig.broker.Says(token, liveToken(caller.jkt, time.Hour))

	sound := caller.at(resourcePath, token)
	sound.Host = "dispatch.internal:8080"
	sound.Header.Set("X-Forwarded-Proto", "http")
	sound.Header.Set("X-Forwarded-Host", "whatever.the.caller.says")
	if _, err := rig.verifier.Verify(t.Context(), sound); err != nil {
		t.Fatalf("a proof naming Dispatch's own origin was refused behind a proxy: %v", err)
	}

	chosen := caller.request("http://whatever.the.caller.says"+resourcePath, token)
	chosen.Host = "whatever.the.caller.says"
	chosen.Header.Set("X-Forwarded-Proto", "http")
	_, err := rig.verifier.Verify(t.Context(), chosen)
	if !errors.Is(err, brokertoken.ErrProofInvalid) {
		t.Fatalf("a proof naming the caller's own origin answered %v, want ErrProofInvalid", err)
	}
}

// TestAFloodOfDistinctUnknownTokensFromOneAddressIsBoundedBeforeTheBroker: the address bucket is
// what stops a stranger spending the budget a person's approval needs (INFRA-228), so K requests
// bearing K tokens the broker has never issued cost fewer than K introspections. With the bound
// lifted and nothing else changed they cost K, which is what says the bucket did it.
func TestAFloodOfDistinctUnknownTokensFromOneAddressIsBoundedBeforeTheBroker(t *testing.T) {
	const flood = 40
	spend := func(t *testing.T, rig *rig) int64 {
		caller := newSigner(t)
		for request := range flood {
			token := fmt.Sprintf("a-token-the-broker-never-issued-%d", request)
			if _, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token)); err == nil {
				t.Fatalf("request %d was accepted; want a refusal", request)
			}
		}
		return rig.broker.Count()
	}

	bounded := spend(t, newRig(t))
	if bounded >= flood || bounded < int64(brokertoken.DefaultAddressLimit.Burst) {
		t.Fatalf("%d requests from one address cost %d introspections, want the bucket's burst of %d and fewer than %d",
			flood, bounded, brokertoken.DefaultAddressLimit.Burst, flood)
	}
	// The negative control: the same flood with the bucket wide enough to admit all of it.
	unbounded := spend(t, newRig(t, func(o *brokertoken.Options) {
		o.AddressLimit = ratelimit.Limit{Every: time.Nanosecond, Burst: flood}
	}))
	if unbounded != flood {
		t.Fatalf("with the bound lifted, %d requests cost %d introspections, want %d", flood, unbounded, flood)
	}
}

// TestARefusedAddressIsNeverTheBrokersVerdict: the refusal a spent bucket gives is its own
// outcome, so a caller is told to come back rather than told its token is bad.
func TestARefusedAddressIsNeverTheBrokersVerdict(t *testing.T) {
	rig := newRig(t, func(o *brokertoken.Options) {
		o.AddressLimit = ratelimit.Limit{Every: time.Minute, Burst: 1}
	})
	caller := newSigner(t)
	rig.broker.Says("the-first-token", liveToken(caller.jkt, time.Hour))
	if _, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, "the-first-token")); err != nil {
		t.Fatalf("the request that spends the bucket: %v", err)
	}

	_, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, "a-second-token"))
	if !errors.Is(err, brokertoken.ErrRateLimited) {
		t.Fatalf("the request with no token left in the bucket answered %v, want ErrRateLimited", err)
	}
	if rig.broker.Count() != 1 {
		t.Fatalf("the refused request reached the broker: %d introspections, want 1", rig.broker.Count())
	}
	if rig.verifier.RetryAfter() != time.Minute {
		t.Fatalf("RetryAfter = %v, want the bucket's minute", rig.verifier.RetryAfter())
	}
}

// TestABrokerThatCannotBeReachedIsTheUnreachableOutcome: a caller whose broker is away must be
// asked to come back, not told its token is refused, or it would discard a token that is live.
func TestABrokerThatCannotBeReachedIsTheUnreachableOutcome(t *testing.T) {
	away := httptest.NewServer(http.NotFoundHandler())
	url := away.URL
	away.Close()
	rig := newRig(t, func(o *brokertoken.Options) { o.BrokerURL = url })
	caller := newSigner(t)

	_, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, "a-token-nobody-can-be-asked-about"))
	if !errors.Is(err, brokertoken.ErrBrokerUnreachable) {
		t.Fatalf("a broker that cannot be reached answered %v, want ErrBrokerUnreachable", err)
	}
	if errors.Is(err, brokertoken.ErrNotActive) {
		t.Fatalf("a broker that cannot be reached refused the token: %v", err)
	}
}

// TestAnAccountTokenDispatchCannotReadAsksTheBrokerNothing: Dispatch's own missing credential is
// its deployment's failure, so it is the unreachable outcome and the error names the file.
func TestAnAccountTokenDispatchCannotReadAsksTheBrokerNothing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nothing-here")
	rig := newRig(t, func(o *brokertoken.Options) { o.Account = brokertoken.NewAccountToken(missing) })
	caller := newSigner(t)

	_, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, "a-token-never-asked-about"))
	if !errors.Is(err, brokertoken.ErrBrokerUnreachable) || !strings.Contains(err.Error(), missing) {
		t.Fatalf("an unreadable account token answered %v, want ErrBrokerUnreachable naming %s", err, missing)
	}
	var unreadable *brokertoken.AccountTokenError
	if !errors.As(err, &unreadable) {
		t.Fatalf("err = %v, want an *AccountTokenError in the chain", err)
	}
	if rig.broker.Count() != 0 {
		t.Fatalf("the broker was asked %d times, want not at all", rig.broker.Count())
	}
}

// TestARequestPresentingNoDPoPTokenIsNotThisCredential: a request carrying another credential —
// a bearer token, or a browser's session cookie and no Authorization header at all — belongs to
// another check, and this one says so rather than refusing it.
//
// It says so without reading the request's body, which whoever mounts this check still has to
// hand to a handler. RFC 6750 puts an access token in a form parameter as well as a header, so a
// reading that looks there parses the form, and the body of a multipart upload — which is how
// Dispatch's document writes arrive, under a cookie — is gone before its handler sees it.
func TestARequestPresentingNoDPoPTokenIsNotThisCredential(t *testing.T) {
	rig := newRig(t)
	var body bytes.Buffer
	upload := multipart.NewWriter(&body)
	part, err := upload.CreateFormFile("file", "note.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("# a document the handler still needs\n")); err != nil {
		t.Fatal(err)
	}
	if err := upload.Close(); err != nil {
		t.Fatal(err)
	}
	sent := body.String()

	for name, authorization := range map[string]string{
		"a bearer token":          "Bearer a-token-of-another-kind",
		"a browser's own cookie":  "",
		"a scheme nothing mounts": "Negotiate something-else-entirely",
	} {
		t.Run(name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, dispatchOrigin+resourcePath, strings.NewReader(sent))
			if err != nil {
				t.Fatal(err)
			}
			if authorization != "" {
				request.Header.Set("Authorization", authorization)
			}
			request.Header.Set("Content-Type", upload.FormDataContentType())
			if _, ok := brokertoken.Presented(request); ok {
				t.Fatal("Presented read another credential as this one")
			}
			if _, err := rig.verifier.Verify(context.Background(), request); !errors.Is(err, brokertoken.ErrNotPresented) {
				t.Fatalf("the request answered %v, want ErrNotPresented", err)
			}
			read, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatalf("read the body back: %v", err)
			}
			if string(read) != sent {
				t.Fatalf("the body reads back as %d of its %d bytes, want all of them: deciding the credential consumed it", len(read), len(sent))
			}
		})
	}
}

// TestNewRefusesAnOriginItCannotCheckAProofAgainst: an origin that is not an absolute http(s)
// origin is a deployment error, and it fails the build of the verifier rather than every
// request.
func TestNewRefusesAnOriginItCannotCheckAProofAgainst(t *testing.T) {
	database := storetest.Open(t)
	for name, origin := range map[string]string{
		"empty":       "",
		"no scheme":   "dispatch.test",
		"not http":    "ftp://dispatch.test",
		"with a path": "https://dispatch.test/dispatch",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := brokertoken.New(brokertoken.Options{
				BrokerURL: "https://broker.test",
				Account:   brokertoken.NewAccountToken(accountTokenFile(t, accountToken)),
				Origin:    origin,
				Replay:    store.NewDPoPProofs(database.Pool),
				Address:   sourceAddress,
			})
			if err == nil {
				t.Fatalf("New accepted the origin %q", origin)
			}
		})
	}
}

// TestNewRefusesAVerifierWithNoSourceAddress: which address an introspection bucket is keyed on
// is the deployment's rule and this package has none of its own, so a Verifier built without
// one is refused at the build rather than left bounding the broker by some default nobody
// chose. The negative control is the same options with the function supplied.
func TestNewRefusesAVerifierWithNoSourceAddress(t *testing.T) {
	database := storetest.Open(t)
	options := func() brokertoken.Options {
		return brokertoken.Options{
			BrokerURL: "https://broker.test",
			Account:   brokertoken.NewAccountToken(accountTokenFile(t, accountToken)),
			Origin:    dispatchOrigin,
			Replay:    store.NewDPoPProofs(database.Pool),
		}
	}
	if _, err := brokertoken.New(options()); err == nil {
		t.Fatal("New built a verifier with no source address function")
	}
	supplied := options()
	supplied.Address = sourceAddress
	if _, err := brokertoken.New(supplied); err != nil {
		t.Fatalf("New refused the same options with the address function supplied: %v", err)
	}
}
