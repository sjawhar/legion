// project_scope_test.go drives the real server with an access token the broker scoped to one
// project, through the same stub broker and the same proof producer broker_token_test.go uses,
// and walks the route table for a mutating row the check cannot decide.
package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEveryMutatingRouteIsClassed: every mutating row of the table declares what its write
// belongs to, and every class names a row that is served. The sub-tests are the negative
// control: the same walk over a fixture holding an unclassed mutating row, and over one classed
// as a project's write whose address names no object, fails on each.
func TestEveryMutatingRouteIsClassed(t *testing.T) {
	served := &server{deps: Deps{TestHooksEnabled: true}}
	routes := served.routes()
	if undecidable := served.undecidableWrites(routes); len(undecidable) > 0 {
		t.Fatalf("%d mutating row(s) the project check cannot decide:\n%s", len(undecidable), strings.Join(undecidable, "\n"))
	}
	mounted := make(map[string]bool, len(routes))
	for _, route := range routes {
		mounted[route.Method+" "+route.Pattern] = true
	}
	for name := range writeClasses {
		if !mounted[name] {
			t.Errorf("writeClasses names %q, which the table does not serve", name)
		}
	}

	t.Run("a mutating row with no class fails this walk", func(t *testing.T) {
		undecidable := served.undecidableWrites([]apiRoute{
			{Method: http.MethodGet, Pattern: "/api/v1/widgets", Auth: authAny},
			{Method: http.MethodPost, Pattern: "/api/v1/widgets", Auth: authAny},
		})
		if len(undecidable) != 1 || !strings.HasPrefix(undecidable[0], "POST /api/v1/widgets:") {
			t.Fatalf("the walk answered %q, want the unclassed POST named and the GET left alone", undecidable)
		}
	})

	t.Run("a project row whose address names no object fails it too", func(t *testing.T) {
		undecidable := served.undecidableWrites([]apiRoute{
			{Method: http.MethodPost, Pattern: "/api/v1/widgets/{id}/paint", Auth: authAny, Scope: scopeProject},
		})
		if len(undecidable) != 1 || !strings.Contains(undecidable[0], "names no object") {
			t.Fatalf("the walk answered %q, want the unresolvable project row named", undecidable)
		}
	})
}

// projectFixture is one project with an object of every addressing kind the check resolves.
type projectFixture struct {
	key      string
	issue    string
	document string
	ask      string
	comment  string
	message  string
}

// asPerson serves a request signed in as the test's person, the credential that carries no
// project.
func (r *brokerTokenRig) asPerson(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return r.serve(withPerson(brokerRequest(t, method, path, body, "", ""), brokerTestPerson))
}

// seedProject builds a project and, in it, an issue, a project document, an ask, a comment and
// an issue message: one object of each kind a resolver reads. A person writes them all, since a
// person's cookie carries no project and this check is a no-op for it.
func seedProject(t *testing.T, rig *brokerTokenRig, key, name string) projectFixture {
	t.Helper()
	created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": key, "name": name})
	if created.Code != http.StatusCreated {
		t.Fatalf("create project %s: status=%d body=%s", key, created.Code, created.Body.String())
	}
	issue := rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": key, "title": name + " work", "spec": "# Spec\n\nA passage.",
	})
	if issue.Code != http.StatusCreated {
		t.Fatalf("create an issue in %s: status=%d body=%s", key, issue.Code, issue.Body.String())
	}
	fixture := projectFixture{key: key, issue: decodeBody[struct {
		Key string `json:"key"`
	}](t, issue).Key}

	document := rig.asPerson(t, http.MethodPost, "/api/v1/projects/"+key+"/artifacts", map[string]any{
		"name": "notes.md", "content": "# Notes\n\nA project document.",
	})
	if document.Code != http.StatusCreated {
		t.Fatalf("upload a project document to %s: status=%d body=%s", key, document.Code, document.Body.String())
	}
	fixture.document = decodeBody[struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}](t, document).Artifact.ID
	if fixture.document == "" {
		t.Fatalf("the uploaded project document has no id: %s", document.Body.String())
	}

	fixture.ask = seededID(t, rig, "ask", "/api/v1/issues/"+fixture.issue+"/asks", map[string]any{
		"question": "Which way for " + key + "?",
	})
	fixture.comment = seededID(t, rig, "comment", "/api/v1/issues/"+fixture.issue+"/comments", map[string]any{
		"body": "A remark on " + key + ".",
	})
	fixture.message = seededID(t, rig, "message", "/api/v1/issues/"+fixture.issue+"/messages", map[string]any{
		"body": "A note on " + key + ".",
	})
	return fixture
}

func seededID(t *testing.T, rig *brokerTokenRig, kind, path string, body any) string {
	t.Helper()
	response := rig.asPerson(t, http.MethodPost, path, body)
	if response.Code != http.StatusCreated {
		t.Fatalf("seed a %s at %s: status=%d body=%s", kind, path, response.Code, response.Body.String())
	}
	id := decodeBody[struct {
		ID string `json:"id"`
	}](t, response).ID
	if id == "" {
		t.Fatalf("the seeded %s has no id: %s", kind, response.Body.String())
	}
	return id
}

// addressedWrite is one write of one addressing kind, built against whichever project's objects
// it is pointed at.
type addressedWrite struct {
	kind   string
	method string
	path   func(projectFixture) string
	body   any
	// handlerAnswer is the status the handler itself answers for the caller's own project, where
	// this check lets the request through. 0 means any answer that is not this check's refusal.
	handlerAnswer int
}

func addressedWrites() []addressedWrite {
	return []addressedWrite{
		{
			kind: "issue key", method: http.MethodPatch,
			path:          func(p projectFixture) string { return "/api/v1/issues/" + p.issue },
			body:          map[string]any{"title": "Retitled by a scoped token"},
			handlerAnswer: http.StatusOK,
		},
		{
			kind: "project key", method: http.MethodPost,
			path:          func(p projectFixture) string { return "/api/v1/projects/" + p.key + "/artifacts" },
			body:          map[string]any{"name": "scoped.md", "content": "# Written by a scoped token"},
			handlerAnswer: http.StatusCreated,
		},
		{
			kind: "artifact id", method: http.MethodPost,
			path:          func(p projectFixture) string { return "/api/v1/artifacts/" + p.document + "/asks" },
			body:          map[string]any{"question": "Is this document right?"},
			handlerAnswer: http.StatusCreated,
		},
		{
			kind: "ask id", method: http.MethodPost,
			path:          func(p projectFixture) string { return "/api/v1/asks/" + p.ask + "/resolve" },
			body:          map[string]any{"kind": "resolved", "reason": "Answered elsewhere."},
			handlerAnswer: http.StatusOK,
		},
		{
			kind: "comment id", method: http.MethodPost,
			path: func(p projectFixture) string { return "/api/v1/comments/" + p.comment + "/resolve" },
			body: map[string]any{},
		},
		{
			// A message on an issue that targets nobody: the handler's own 404 proves the request
			// reached it, where another project's message never does.
			kind: "message id", method: http.MethodPost,
			path:          func(p projectFixture) string { return "/api/v1/messages/" + p.message + "/deliveries" },
			body:          map[string]any{"delivery": "btw"},
			handlerAnswer: http.StatusNotFound,
		},
	}
}

// TestAProjectScopedTokenWritesOnlyItsOwnProject: one real token, two projects. Every addressing
// kind the check resolves is refused across projects, naming both, and the same call against the
// token's own project is answered by the handler.
func TestAProjectScopedTokenWritesOnlyItsOwnProject(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-scoped-to-one-project"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	own := seedProject(t, rig, brokerTestProject, "Broker")
	other := seedProject(t, rig, "OTH", "Other")

	for _, write := range addressedWrites() {
		t.Run(write.kind, func(t *testing.T) {
			refused := rig.serve(caller.presenting(t, write.method, write.path(other), token, write.body))
			if refused.Code != http.StatusForbidden {
				t.Fatalf("a write to %s: status=%d body=%s, want 403", other.key, refused.Code, refused.Body.String())
			}
			refusal := decodeBody[refusalAnswer](t, refused)
			if refusal.Code != codeProjectScope {
				t.Fatalf("refusal = %+v, want %s", refusal, codeProjectScope)
			}
			if !strings.Contains(refusal.Error, own.key) || !strings.Contains(refusal.Error, other.key) {
				t.Fatalf("refusal %q names neither the project the credential covers (%s) nor the project the write belongs to (%s)",
					refusal.Error, own.key, other.key)
			}
			if strings.Contains(refusal.Error, token) {
				t.Fatalf("the refusal repeats the token: %q", refusal.Error)
			}

			answered := rig.serve(caller.presenting(t, write.method, write.path(own), token, write.body))
			if write.handlerAnswer != 0 && answered.Code != write.handlerAnswer {
				t.Fatalf("the same write to %s: status=%d body=%s, want %d from the handler",
					own.key, answered.Code, answered.Body.String(), write.handlerAnswer)
			}
			if answered.Code == http.StatusForbidden {
				if blocked := decodeBody[refusalAnswer](t, answered); blocked.Code == codeProjectScope {
					t.Fatalf("the token's own project was refused: %+v", blocked)
				}
			}
		})
	}
}

// TestAnUnknownTargetKeepsTheHandlersAnswer: a target that does not exist resolves to no
// project, so the caller gets the 404 it has always got rather than a refusal that would say
// whether some other project holds that id.
func TestAnUnknownTargetKeepsTheHandlersAnswer(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-addressing-nothing"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	seedProject(t, rig, brokerTestProject, "Broker")

	for name, request := range map[string]struct {
		method string
		path   string
		body   any
	}{
		"an issue nobody has":   {http.MethodPatch, "/api/v1/issues/NONE-1", map[string]any{"title": "x"}},
		"an ask nobody has":     {http.MethodPost, "/api/v1/asks/5d4a3b2c-1e0f-4a9b-8c7d-6e5f4a3b2c1d/resolve", map[string]any{"kind": "resolved", "reason": "x"}},
		"an id that is no uuid": {http.MethodPost, "/api/v1/comments/not-a-uuid/resolve", map[string]any{}},
		"a project nobody has":  {http.MethodPost, "/api/v1/projects/NONE/artifacts", map[string]any{"name": "n.md", "content": "# N"}},
		"a document nobody has": {http.MethodPost, "/api/v1/artifacts/5d4a3b2c-1e0f-4a9b-8c7d-6e5f4a3b2c1d/asks", map[string]any{"question": "?"}},
		"a message nobody has":  {http.MethodPost, "/api/v1/messages/5d4a3b2c-1e0f-4a9b-8c7d-6e5f4a3b2c1d/deliveries", map[string]any{"delivery": "btw"}},
	} {
		t.Run(name, func(t *testing.T) {
			response := rig.serve(caller.presenting(t, request.method, request.path, token, request.body))
			if response.Code == http.StatusForbidden {
				if refusal := decodeBody[refusalAnswer](t, response); refusal.Code == codeProjectScope {
					t.Fatalf("an unknown target was refused by project scope: %+v", refusal)
				}
			}
			if response.Code < http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want the handler's own refusal", response.Code, response.Body.String())
			}
		})
	}
}

// TestADeploymentWriteIsRefusedToAProjectScopedToken: a credential that may write one project
// may not create another, and the refusal names the route rather than leaving the caller to
// guess. The same write by a person and by a personal token, neither of which names a project,
// is untouched.
func TestADeploymentWriteIsRefusedToAProjectScopedToken(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-that-may-not-create-a-project"
	rig.broker.Says(token, liveNamedToken(caller.jkt))

	refused := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/projects", token, map[string]any{
		"key": "NEW", "name": "New",
	}))
	if refused.Code != http.StatusForbidden {
		t.Fatalf("a project create under a project-scoped token: status=%d body=%s, want 403", refused.Code, refused.Body.String())
	}
	refusal := decodeBody[refusalAnswer](t, refused)
	if refusal.Code != codeProjectScopeDeployment {
		t.Fatalf("refusal = %+v, want %s", refusal, codeProjectScopeDeployment)
	}
	if !strings.Contains(refusal.Error, brokerTestProject) || !strings.Contains(refusal.Error, "POST /api/v1/projects") {
		t.Fatalf("refusal %q names neither the project the credential covers nor the route", refusal.Error)
	}

	person := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": "PRS", "name": "Person"})
	if person.Code != http.StatusCreated {
		t.Fatalf("a person creating a project: status=%d body=%s", person.Code, person.Body.String())
	}
}

// TestCredentialsThatNameNoProjectAreUnaffected: the check is a no-op for every credential that
// exists today. A person and a personal token write both projects, and the deployment-wide
// create too.
func TestCredentialsThatNameNoProjectAreUnaffected(t *testing.T) {
	rig := newBrokerTokenRig(t)
	own := seedProject(t, rig, brokerTestProject, "Broker")
	other := seedProject(t, rig, "OTH", "Other")

	for _, project := range []projectFixture{own, other} {
		comment := rig.asPerson(t, http.MethodPost, "/api/v1/issues/"+project.issue+"/comments", map[string]any{
			"body": "A person writes here.",
		})
		if comment.Code != http.StatusCreated {
			t.Fatalf("a person commenting on %s: status=%d body=%s", project.key, comment.Code, comment.Body.String())
		}
	}

	minted := rig.asPerson(t, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": "laptop"})
	if minted.Code != http.StatusCreated {
		t.Fatalf("mint a personal token: status=%d body=%s", minted.Code, minted.Body.String())
	}
	personal := decodeBody[struct {
		Token string `json:"token"`
	}](t, minted).Token
	asToken := func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		body["actor"] = map[string]any{"kind": "session", "id": "ses_personal_token"}
		return rig.serve(brokerRequest(t, method, path, body, "Bearer "+personal, ""))
	}

	for _, project := range []projectFixture{own, other} {
		comment := asToken(t, http.MethodPost, "/api/v1/issues/"+project.issue+"/comments", map[string]any{
			"body": fmt.Sprintf("A personal token writes on %s.", project.key),
		})
		if comment.Code != http.StatusCreated {
			t.Fatalf("a personal token commenting on %s: status=%d body=%s", project.key, comment.Code, comment.Body.String())
		}
	}
	created := asToken(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": "TOK", "name": "Token"})
	if created.Code != http.StatusCreated {
		t.Fatalf("a personal token creating a project: status=%d body=%s", created.Code, created.Body.String())
	}
}

// TestAProjectScopedTokenCreatesIssuesOnlyInItsOwnProject: the one write whose project its
// address does not name. Its handler makes the same check, with the same refusal, before the
// create does any work.
func TestAProjectScopedTokenCreatesIssuesOnlyInItsOwnProject(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-creating-issues"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	seedProject(t, rig, brokerTestProject, "Broker")
	seedProject(t, rig, "OTH", "Other")

	refused := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues", token, map[string]any{
		"project": "OTH", "title": "Filed into another project", "spec": "# Spec",
	}))
	if refused.Code != http.StatusForbidden {
		t.Fatalf("creating an issue in another project: status=%d body=%s, want 403", refused.Code, refused.Body.String())
	}
	refusal := decodeBody[refusalAnswer](t, refused)
	if refusal.Code != codeProjectScope || !strings.Contains(refusal.Error, brokerTestProject) || !strings.Contains(refusal.Error, "OTH") {
		t.Fatalf("refusal = %+v, want %s naming %s and OTH", refusal, codeProjectScope, brokerTestProject)
	}

	created := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues", token, map[string]any{
		"project": brokerTestProject, "title": "Filed into its own project", "spec": "# Spec",
	}))
	if created.Code != http.StatusCreated {
		t.Fatalf("creating an issue in its own project: status=%d body=%s", created.Code, created.Body.String())
	}
}
