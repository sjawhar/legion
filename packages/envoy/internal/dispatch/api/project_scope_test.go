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
			{routeRow: routeRow{Method: http.MethodGet, Pattern: "/api/v1/widgets", Auth: authAny}},
			{routeRow: routeRow{Method: http.MethodPost, Pattern: "/api/v1/widgets", Auth: authAny}},
		})
		if len(undecidable) != 1 || !strings.HasPrefix(undecidable[0], "POST /api/v1/widgets:") {
			t.Fatalf("the walk answered %q, want the unclassed POST named and the GET left alone", undecidable)
		}
	})

	t.Run("a project row whose address names no object fails it too", func(t *testing.T) {
		undecidable := served.undecidableWrites([]apiRoute{
			{routeRow: routeRow{Method: http.MethodPost, Pattern: "/api/v1/widgets/{id}/paint", Auth: authAny}, Scope: scopeProject},
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
// it is pointed at, each id written in whichever spelling the matrix is running.
type addressedWrite struct {
	kind   string
	method string
	path   func(p projectFixture, spell func(string) string) string
	body   any
	// handlerAnswer is the status the handler itself answers for the caller's own project, where
	// this check lets the request through. 0 means any answer that is not this check's refusal.
	handlerAnswer int
	// uuidAddressed is whether the path names its object by uuid, which is what gives the
	// matrix a second spelling to run (uuidSpellings).
	uuidAddressed bool
}

func addressedWrites() []addressedWrite {
	return []addressedWrite{
		{
			kind: "issue key", method: http.MethodPatch,
			path:          func(p projectFixture, _ func(string) string) string { return "/api/v1/issues/" + p.issue },
			body:          map[string]any{"title": "Retitled by a scoped token"},
			handlerAnswer: http.StatusOK,
		},
		{
			kind: "project key", method: http.MethodPost,
			path: func(p projectFixture, _ func(string) string) string {
				return "/api/v1/projects/" + p.key + "/artifacts"
			},
			body:          map[string]any{"name": "scoped.md", "content": "# Written by a scoped token"},
			handlerAnswer: http.StatusCreated,
		},
		{
			kind: "artifact id", method: http.MethodPost,
			path: func(p projectFixture, spell func(string) string) string {
				return "/api/v1/artifacts/" + spell(p.document) + "/asks"
			},
			body:          map[string]any{"question": "Is this document right?"},
			handlerAnswer: http.StatusCreated,
			uuidAddressed: true,
		},
		{
			kind: "ask id", method: http.MethodPost,
			path: func(p projectFixture, spell func(string) string) string {
				return "/api/v1/asks/" + spell(p.ask) + "/resolve"
			},
			body:          map[string]any{"kind": "resolved", "reason": "Answered elsewhere."},
			handlerAnswer: http.StatusOK,
			uuidAddressed: true,
		},
		{
			kind: "comment id", method: http.MethodPost,
			path: func(p projectFixture, spell func(string) string) string {
				return "/api/v1/comments/" + spell(p.comment) + "/resolve"
			},
			body:          map[string]any{},
			uuidAddressed: true,
		},
		{
			// A message on an issue that targets nobody: the handler's own 404 proves the request
			// reached it, where another project's message never does.
			kind: "message id", method: http.MethodPost,
			path: func(p projectFixture, spell func(string) string) string {
				return "/api/v1/messages/" + spell(p.message) + "/deliveries"
			},
			body:          map[string]any{"delivery": "btw"},
			handlerAnswer: http.StatusNotFound,
			uuidAddressed: true,
		},
	}
}

// uuidSpelling is one way of writing a uuid in a path, and what the handler behind the check
// answers for it where the write is the caller's own to make.
type uuidSpelling struct {
	name string
	// spell writes the id as this spelling.
	spell func(string) string
	// handlerAnswered is whether the handler's own answer is the one the write declares. Only
	// the canonical spelling is every handler's: a route that parses its id in Go before it
	// reads the row (requireUUIDPath) refuses an alternate spelling 400, which is its answer to
	// make and not this check's.
	handlerAnswered bool
}

// uuidSpellings are the spellings of one uuid a path may carry, which this check and the
// handler behind it must read the same way. Postgres's uuid parser takes a hyphen after any
// group of four digits, so `a061-e3f2-…` is the same id as `a061e3f2-…` to every query a
// handler makes, while Go's uuid.Parse refuses it: a check that read ids with the second
// admitted a write to an object it had resolved to nothing (INFRA-264).
func uuidSpellings(write addressedWrite) []uuidSpelling {
	canonical := uuidSpelling{name: "canonical", spell: func(id string) string { return id }, handlerAnswered: true}
	if !write.uuidAddressed {
		return []uuidSpelling{canonical}
	}
	return []uuidSpelling{canonical, {name: "hyphens every four digits", spell: hyphenatedEveryFour}}
}

// hyphenatedEveryFour writes a uuid with a hyphen after each group of four digits, one of the
// spellings Postgres reads as that uuid and Go's parser does not.
func hyphenatedEveryFour(id string) string {
	digits := strings.ReplaceAll(id, "-", "")
	var spelled strings.Builder
	for group := 0; group*4 < len(digits); group++ {
		if group > 0 {
			spelled.WriteByte('-')
		}
		spelled.WriteString(digits[group*4 : min(group*4+4, len(digits))])
	}
	return spelled.String()
}

// TestAProjectScopedTokenWritesOnlyItsOwnProject: one real token, two projects. Every addressing
// kind the check resolves is refused across projects, naming both, and the same call against the
// token's own project is answered by the handler. Each id-addressed kind runs under both
// spellings of its uuid, since the check and the handler have to read a path the same way.
func TestAProjectScopedTokenWritesOnlyItsOwnProject(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-scoped-to-one-project"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	own := seedProject(t, rig, brokerTestProject, "Broker")
	other := seedProject(t, rig, "OTH", "Other")

	for _, write := range addressedWrites() {
		for _, spelling := range uuidSpellings(write) {
			t.Run(write.kind+", "+spelling.name, func(t *testing.T) {
				refused := rig.serve(caller.presenting(t, write.method, write.path(other, spelling.spell), token, write.body))
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

				answered := rig.serve(caller.presenting(t, write.method, write.path(own, spelling.spell), token, write.body))
				if spelling.handlerAnswered && write.handlerAnswer != 0 && answered.Code != write.handlerAnswer {
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
}

// TestATargetThatResolvesToNoProjectIsRefusedToAProjectScopedToken: the check fails closed, so
// a target it cannot resolve to the caller's own project is refused whether another project
// holds it, no project does, or nothing does — an id nobody has, and an id that is no uuid in
// any spelling. A refusal names the project the credential covers and no other, so it says
// nothing about what exists outside it, and that is what closes the existence oracle the
// earlier behaviour had: a scoped caller read 403 for another project's object and the
// handler's 404 for an id nobody holds, which told it which ids exist.
//
// This replaces the assertion that such a target kept its handler's answer for every caller.
// It still does for a caller that names no project, which is every credential but a
// project-scoped broker token, and the second half holds that.
func TestATargetThatResolvesToNoProjectIsRefusedToAProjectScopedToken(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-addressing-nothing"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	seedProject(t, rig, brokerTestProject, "Broker")

	const unknownUUID = "5d4a3b2c-1e0f-4a9b-8c7d-6e5f4a3b2c1d"
	for name, request := range map[string]struct {
		method string
		path   string
		body   any
	}{
		"an issue nobody has":   {http.MethodPatch, "/api/v1/issues/NONE-1", map[string]any{"title": "x"}},
		"an ask nobody has":     {http.MethodPost, "/api/v1/asks/" + unknownUUID + "/resolve", map[string]any{"kind": "resolved", "reason": "x"}},
		"an id that is no uuid": {http.MethodPost, "/api/v1/comments/not-a-uuid/resolve", map[string]any{}},
		"a project nobody has":  {http.MethodPost, "/api/v1/projects/NONE/artifacts", map[string]any{"name": "n.md", "content": "# N"}},
		"a document nobody has": {http.MethodPost, "/api/v1/artifacts/" + unknownUUID + "/asks", map[string]any{"question": "?"}},
		"a message nobody has":  {http.MethodPost, "/api/v1/messages/" + unknownUUID + "/deliveries", map[string]any{"delivery": "btw"}},
		"a comment nobody has, spelled with hyphens every four digits": {
			http.MethodPost, "/api/v1/comments/" + hyphenatedEveryFour(unknownUUID) + "/resolve", map[string]any{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			refused := rig.serve(caller.presenting(t, request.method, request.path, token, request.body))
			if refused.Code != http.StatusForbidden {
				t.Fatalf("status=%d body=%s, want 403 from the project check", refused.Code, refused.Body.String())
			}
			refusal := decodeBody[refusalAnswer](t, refused)
			if refusal.Code != codeProjectScope {
				t.Fatalf("refusal = %+v, want %s", refusal, codeProjectScope)
			}
			if !strings.Contains(refusal.Error, brokerTestProject) {
				t.Fatalf("refusal %q does not name the project the credential covers (%s)", refusal.Error, brokerTestProject)
			}
			if strings.Contains(refusal.Error, token) {
				t.Fatalf("the refusal repeats the token: %q", refusal.Error)
			}

			// A caller that names no project is untouched: the handler answers for itself, as it
			// always has.
			answered := rig.asPerson(t, request.method, request.path, request.body)
			if answered.Code < http.StatusBadRequest {
				t.Fatalf("a person's same call: status=%d body=%s, want the handler's own refusal",
					answered.Code, answered.Body.String())
			}
			if answered.Code == http.StatusForbidden {
				if blocked := decodeBody[refusalAnswer](t, answered); blocked.Code == codeProjectScope {
					t.Fatalf("a caller that names no project was refused by project scope: %+v", blocked)
				}
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
