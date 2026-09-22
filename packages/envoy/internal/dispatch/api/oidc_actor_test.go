package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	serviceTokenAudience = "dispatch"
	serviceTokenSubject  = "system:serviceaccount:legion:legion-worker"
)

// serviceTokenIssuer is the shared local issuer plus the two conveniences these
// tests want: a token for one audience, and a verifier for the audience
// Dispatch runs with.
type serviceTokenIssuer struct {
	*oidctest.Issuer
	signing *oidctest.Key
}

func newServiceTokenIssuer(t *testing.T) *serviceTokenIssuer {
	t.Helper()
	issuer := oidctest.New(t)
	return &serviceTokenIssuer{Issuer: issuer, signing: issuer.PublishKey(t, "service-token-key")}
}

// mint signs a service-account token for audience, exactly as a projected token
// arrives: issuer, subject, array audience, and an hour of life.
func (ti *serviceTokenIssuer) mint(t *testing.T, audience string) string {
	t.Helper()
	return ti.Mint(t, ti.signing, ti.Claims(serviceTokenSubject, audience))
}

func (ti *serviceTokenIssuer) verifier(t *testing.T) *oidc.Verifier {
	t.Helper()
	verifier, err := oidc.New(context.Background(), ti.URL(), serviceTokenAudience)
	if err != nil {
		t.Fatalf("build verifier for %s: %v", ti.URL(), err)
	}
	return verifier
}

// seedServiceTokenIssue gives a service-token test an issue to write on.
func seedServiceTokenIssue(t *testing.T, handler http.Handler) string {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Service tokens",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	return decodeBody[struct {
		Key string `json:"key"`
	}](t, created).Key
}

type commentAuthor struct {
	Author model.Actor `json:"author"`
}

// A verified service-account token writes as the session it names, and the verified
// Kubernetes subject rides along on every actor it persists — the comment read back
// and the issue event both carry it, and no owner is invented for it.
func TestServiceTokenActorCarriesVerifiedSubject(t *testing.T) {
	issuer := newServiceTokenIssuer(t)
	handler, _ := newTestServer(t, testServerOptions{oidc: issuer.verifier(t)})
	key := seedServiceTokenIssue(t, handler)
	token := issuer.mint(t, serviceTokenAudience)

	created := agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments", map[string]any{
		"body":  "the worker reports in",
		"actor": map[string]any{"kind": "session", "id": "ses_x"},
	}, token)
	if created.Code != http.StatusCreated {
		t.Fatalf("service-token comment: status=%d body=%s", created.Code, created.Body.String())
	}
	comment := decodeBody[struct {
		ID string `json:"id"`
	}](t, created)

	read := agentRequest(t, handler, http.MethodGet, "/api/v1/comments/"+comment.ID, nil, token)
	if read.Code != http.StatusOK {
		t.Fatalf("read comment: status=%d body=%s", read.Code, read.Body.String())
	}
	author := decodeBody[struct {
		Comment commentAuthor `json:"comment"`
	}](t, read).Comment.Author
	if author.Kind != "session" || author.ID != "ses_x" {
		t.Fatalf("persisted author = %#v, want the session the body named", author)
	}
	if author.Owner != nil {
		t.Fatalf("persisted author owner = %q, want none for a service token", *author.Owner)
	}
	if author.Service == nil || *author.Service != serviceTokenSubject {
		t.Fatalf("persisted author service = %v, want %q", author.Service, serviceTokenSubject)
	}

	events := agentRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/events", nil, token)
	if events.Code != http.StatusOK {
		t.Fatalf("read events: status=%d body=%s", events.Code, events.Body.String())
	}
	log := decodeBody[[]model.Event](t, events)
	var found *model.Actor
	for index, event := range log {
		if event.Type == "comment.created" {
			found = &log[index].Actor
		}
	}
	if found == nil {
		t.Fatalf("no comment.created event in %d events", len(log))
	}
	if found.Service == nil || *found.Service != serviceTokenSubject {
		t.Fatalf("comment.created actor service = %v, want %q", found.Service, serviceTokenSubject)
	}
}

// GET /whoami is the surface an agent reads to learn its own identity: a verified
// service token is an agent with no owner and the subject it authenticated as.
func TestServiceTokenWhoamiNamesTheSubject(t *testing.T) {
	issuer := newServiceTokenIssuer(t)
	handler, _ := newTestServer(t, testServerOptions{oidc: issuer.verifier(t)})

	response := agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, issuer.mint(t, serviceTokenAudience))
	if response.Code != http.StatusOK {
		t.Fatalf("service-token whoami: status=%d body=%s", response.Code, response.Body.String())
	}
	got := decodeBody[struct {
		Kind    string  `json:"kind"`
		Owner   *string `json:"owner"`
		Service *string `json:"service"`
	}](t, response)
	if got.Kind != "agent" || got.Owner != nil {
		t.Fatalf("service-token whoami = %#v, want an agent with a null owner", got)
	}
	if got.Service == nil || *got.Service != serviceTokenSubject {
		t.Fatalf("service-token whoami service = %v, want %q", got.Service, serviceTokenSubject)
	}
}

// A token for another audience is refused as a token, never demoted to an unknown
// bearer: OIDC_TOKEN_INVALID is how an operator knows the JWT was verified and
// rejected rather than looked up as a personal token, whose 401 carries UNAUTHORIZED.
func TestServiceTokenForAnotherAudienceIsRejectedAsInvalidToken(t *testing.T) {
	issuer := newServiceTokenIssuer(t)
	handler, _ := newTestServer(t, testServerOptions{oidc: issuer.verifier(t)})

	response := agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, issuer.mint(t, "envoy"))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-audience token: status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"code":"OIDC_TOKEN_INVALID"`) {
		t.Fatalf("wrong-audience token body = %s, want code OIDC_TOKEN_INVALID", response.Body.String())
	}
}

// The request body may carry `service` once model.Actor has the field; it is the
// verified token that decides it. A shared-token caller naming a privileged subject
// gets an actor with none.
func TestSuppliedServiceIsIgnored(t *testing.T) {
	issuer := newServiceTokenIssuer(t)
	handler, _ := newTestServer(t, testServerOptions{oidc: issuer.verifier(t)})
	key := seedServiceTokenIssue(t, handler)

	created := agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments", map[string]any{
		"body": "trust me",
		"actor": map[string]any{
			"kind":    "session",
			"id":      "ses_x",
			"service": "system:serviceaccount:kube-system:admin",
		},
	}, "agent-token")
	if created.Code != http.StatusCreated {
		t.Fatalf("shared-token comment: status=%d body=%s", created.Code, created.Body.String())
	}
	comment := decodeBody[struct {
		ID string `json:"id"`
	}](t, created)

	read := agentRequest(t, handler, http.MethodGet, "/api/v1/comments/"+comment.ID, nil, "agent-token")
	if read.Code != http.StatusOK {
		t.Fatalf("read comment: status=%d body=%s", read.Code, read.Body.String())
	}
	author := decodeBody[struct {
		Comment commentAuthor `json:"comment"`
	}](t, read).Comment.Author
	if author.Service != nil {
		t.Fatalf("supplied service was persisted as %q; the body must not decide it", *author.Service)
	}
}

// TestRejectionLogOmitsTheTokensUnverifiedClaims: optionalActor runs on every
// authAny route before any authorization, so the `iss` and `aud` in a rejected
// token are strings an unauthenticated caller chose. The class is what an
// operator acts on; writing the caller's own strings into the operator's log on
// every request is a log-injection surface bounded only by the header limit.
func TestRejectionLogOmitsTheTokensUnverifiedClaims(t *testing.T) {
	issuer := newServiceTokenIssuer(t)
	handler, _ := newTestServer(t, testServerOptions{oidc: issuer.verifier(t)})

	const chosen = "https://attacker-chosen-issuer.invalid/CANARY"
	claims := issuer.Claims(serviceTokenSubject, serviceTokenAudience)
	claims["iss"] = chosen

	previous := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	response := agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil,
		issuer.Mint(t, issuer.signing, claims))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("foreign-issuer token: status=%d body=%s", response.Code, response.Body.String())
	}
	logged := logs.String()
	if !strings.Contains(logged, "reason=issuer") {
		t.Fatalf("rejection log does not name the class: %s", logged)
	}
	if strings.Contains(logged, chosen) {
		t.Fatalf("rejection log repeats the caller's own unverified claim: %s", logged)
	}
}

// With no issuer configured the JWT branch does not exist: a JWT-shaped bearer is
// an unknown personal token, exactly as before this change.
func TestJWTBearerWithoutConfiguredIssuerTakesThePersonalTokenPath(t *testing.T) {
	issuer := newServiceTokenIssuer(t)
	handler, _ := newTestServer(t, testServerOptions{})

	response := agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, issuer.mint(t, serviceTokenAudience))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unconfigured JWT bearer: status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"code":"UNAUTHORIZED"`) {
		t.Fatalf("unconfigured JWT bearer body = %s, want code UNAUTHORIZED", response.Body.String())
	}
}
