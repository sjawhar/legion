package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type mentionPrivacyCase struct {
	name, path string
	private    bool
}

func mentionPrivacyCases(issue string) []mentionPrivacyCase {
	cases := []mentionPrivacyCase{{"person nodes", "/api/v1/people/alice/nodes", true}}
	for _, direction := range []string{"to", "from"} {
		for _, identity := range []struct{ kind, ref string }{{"person", "dispatch://people/alice"}, {"session", "dispatch://sessions/reader"}, {"role", "dispatch://roles/reviewer"}} {
			cases = append(cases, mentionPrivacyCase{direction + " " + identity.kind, "/api/v1/references?" + url.Values{direction: {identity.ref}}.Encode(), true})
		}
		cases = append(cases, mentionPrivacyCase{direction + " ordinary issue", "/api/v1/references?" + url.Values{direction: {"dispatch://" + issue}}.Encode(), false})
	}
	return cases
}

func TestIdentityReferencePrivacyCredentialClasses(t *testing.T) {
	issuer := newServiceTokenIssuer(t)
	handler, database, _ := newTestServer(t, testServerOptions{oidc: issuer.verifier(t)})
	issue := createInteractionIssue(t, handler, "PRIV", "Identity privacy", "A spec")
	cases := mentionPrivacyCases(issue.Key)
	for _, credential := range []struct {
		name, token                string
		browser, anonymous, person bool
	}{
		{name: "browser", browser: true, person: true},
		{name: "shared bearer", token: "agent-token"},
		{name: "service account bearer", token: issuer.mint(t, serviceTokenAudience)},
		{name: "personal bearer without scopes", token: personalTokenHolding(t, database)},
		{name: "personal bearer person only", token: personalTokenHolding(t, database, model.ScopePerson), person: true},
		{name: "personal bearer both scopes", token: personalTokenHolding(t, database, model.ScopePerson, model.ScopeApprove), person: true},
		{name: "anonymous", anonymous: true},
	} {
		t.Run(credential.name, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					var response *httptest.ResponseRecorder
					if credential.browser {
						response = dispatchRequest(t, handler, http.MethodGet, tc.path, nil, "alice")
					} else if credential.anonymous {
						response = httptest.NewRecorder()
						handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil))
					} else {
						response = agentRequest(t, handler, http.MethodGet, tc.path, nil, credential.token)
					}
					want := http.StatusOK
					if tc.private && !credential.person {
						want = http.StatusForbidden
					}
					if credential.anonymous {
						want = http.StatusUnauthorized
					}
					if response.Code != want {
						t.Fatalf("status=%d body=%s want=%d", response.Code, response.Body.String(), want)
					}
				})
			}
		})
	}
}

func TestIdentityReferencePrivacyBrokerCredentialClasses(t *testing.T) {
	rig := newBrokerTokenRig(t)
	issue := createInteractionIssue(t, rig.handler, "BPRIV", "Broker identity privacy", "A spec")
	caller := newBrokerCaller(t)
	for _, class := range []struct {
		name, scope string
		person      bool
	}{{"project scoped broker", brokertoken.ProjectScopeNamed, false}, {"person scoped broker", brokertoken.ProjectScopePerson, true}} {
		t.Run(class.name, func(t *testing.T) {
			token := "identity-reference-" + class.scope
			says := liveNamedToken(caller.jkt)
			says.ProjectScope = class.scope
			if class.person {
				says.Project = ""
			} else {
				says.Project = "BPRIV"
			}
			rig.broker.Says(token, says)
			for _, tc := range mentionPrivacyCases(issue.Key) {
				t.Run(tc.name, func(t *testing.T) {
					response := rig.serve(caller.presenting(t, http.MethodGet, tc.path, token, nil))
					want := http.StatusOK
					if tc.private && !class.person {
						want = http.StatusForbidden
					}
					if response.Code != want {
						t.Fatalf("status=%d body=%s want=%d", response.Code, response.Body.String(), want)
					}
				})
			}
		})
	}
}
