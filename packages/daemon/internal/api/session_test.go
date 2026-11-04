package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/agentsecrets"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The session pods a harness's namespace holds: sessionPod is the operator's own, legionPod one of
// this project's claims, which no session route serves.
const (
	sessionPod    = "k8s-session-ada"
	sessionPodUID = "uid-k8s-session-ada"
	legionPod     = "legion-legion-legion-208"
)

// fakeSessionPods is api.SessionPods over a fixed namespace: sessionPod is a session pod,
// legionPod is Legion's own, every other name is nothing. It records each bundle written and
// each enrollment recorded for revocation.
type fakeSessionPods struct {
	// readErr, when set, is what every read answers: a cluster the daemon cannot reach.
	readErr error
	// writeErr, when set, is what every write answers.
	writeErr    error
	writes      []ghconfig.Bundle
	enrollments []string
}

func (f *fakeSessionPods) SessionPod(_ context.Context, name string) (string, error) {
	switch {
	case f.readErr != nil:
		return "", f.readErr
	case name == sessionPod:
		return sessionPodUID, nil
	case name == legionPod:
		return "", fmt.Errorf("pod %s %w", name, runtime.ErrPodNotSession)
	}
	return "", fmt.Errorf("pod %s: %w", name, runtime.ErrNoSuchPod)
}

func (f *fakeSessionPods) WriteSessionGitHubCredential(_ context.Context, name, uid string, bundle ghconfig.Bundle) (string, error) {
	if f.writeErr != nil {
		return "", f.writeErr
	}
	f.writes = append(f.writes, bundle)
	return name + "-gh", nil
}

func (f *fakeSessionPods) RecordSessionEnrollment(name, uid, enrollment string) {
	f.enrollments = append(f.enrollments, name+" "+uid+" "+enrollment)
}

// fakeEnroller is api.SessionEnroller: it records the enrollment asked for and answers err, or an
// enrollment of its own when err is nil.
type fakeEnroller struct {
	err  error
	made []agentsecrets.PodEnrollment
}

func (f *fakeEnroller) Enroll(_ context.Context, e agentsecrets.PodEnrollment) (agentsecrets.Enrollment, error) {
	f.made = append(f.made, e)
	if f.err != nil {
		return agentsecrets.Enrollment{}, f.err
	}
	return agentsecrets.Enrollment{ID: "enr-session"}, nil
}

// sessionHarness serves the two session routes and nothing else. They read no store, so it needs
// none: every test here runs wherever `go test` does.
type sessionHarness struct {
	t       *testing.T
	handler http.Handler
}

// newSessionHarness serves both session routes over pods and enroller, for a project whose
// repositories are under two owners, sjawhar and acme.
func newSessionHarness(t *testing.T, enroller SessionEnroller, pods SessionPods, tokens appauth.Tokens) *sessionHarness {
	t.Helper()
	return &sessionHarness{t: t, handler: NewServer("127.0.0.1", 8437, Options{
		Project: testProject, OperatorToken: testOperatorToken,
		Log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		SessionEnroller: enroller, SessionPods: pods,
		Tokens: tokens, GitHubOwners: func() []string { return []string{"sjawhar", "acme"} },
	}).Handler}
}

// sessionRequest is one session route call with bearer as the operator bearer; "" sends none.
func (h *sessionHarness) sessionRequest(bearer, route string, body any) *httptest.ResponseRecorder {
	h.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		h.t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/legion/v1/sessions/"+route, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, request)
	return recorder
}

func (h *sessionHarness) sessionEnroll(bearer string, req SessionEnrollRequest) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.sessionRequest(bearer, "agent-secrets", req)
}

func (h *sessionHarness) sessionCredential(bearer string, req SessionCredentialRequest) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.sessionRequest(bearer, "github-credential", req)
}

// enrollRequest is a well-formed enrollment of the harness's session pod.
func enrollRequest() SessionEnrollRequest {
	return SessionEnrollRequest{Pod: sessionPod, Thumbprint: "tp-session", PodToken: "a.b.c", Session: "ses_session"}
}

// The enrollment route enrolls the pod's key under the daemon's own broker login, proving the pod
// with the pod's own projected token, and answers the id the pod renews under. The uid it enrolls
// is the one the daemon resolved, never one the request carried, and the enrollment is recorded
// for the runtime to revoke once the pod is gone.
func TestSessionEnrollmentEnrollsTheResolvedPod(t *testing.T) {
	enroller := &fakeEnroller{}
	pods := &fakeSessionPods{}
	h := newSessionHarness(t, enroller, pods, tokenSource{})

	recorder := h.sessionEnroll(testOperatorToken, enrollRequest())
	if recorder.Code != http.StatusOK {
		t.Fatalf("session enrollment = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	var answer SessionEnrollResponse
	decodeInto(t, recorder, &answer)
	if answer.EnrollmentID != "enr-session" || answer.PodUID != sessionPodUID || answer.LeaseExpiresAt == "" {
		t.Fatalf("answer = %+v, want enr-session on %s with an expiry", answer, sessionPodUID)
	}
	want := agentsecrets.PodEnrollment{PodUID: sessionPodUID, Thumbprint: "tp-session", PodToken: "a.b.c", Session: "ses_session"}
	if len(enroller.made) != 1 || enroller.made[0] != want {
		t.Fatalf("enrollments = %+v, want exactly %+v", enroller.made, want)
	}
	if recorded := []string{sessionPod + " " + sessionPodUID + " enr-session"}; !slices.Equal(pods.enrollments, recorded) {
		t.Fatalf("recorded %v, want %v for the sweep to revoke once the pod is gone", pods.enrollments, recorded)
	}
}

// Neither session route answers without the operator bearer, which is the same authority the
// controller secret route requires: a wrong one and none at all are refused alike, nothing is
// enrolled, and nothing is written.
func TestSessionRoutesRefuseWithoutTheOperatorBearer(t *testing.T) {
	enroller := &fakeEnroller{}
	pods := &fakeSessionPods{}
	h := newSessionHarness(t, enroller, pods, tokenSource{})

	for _, bearer := range []string{"", "not-the-operator-token"} {
		wantFailure(t, h.sessionEnroll(bearer, enrollRequest()), http.StatusForbidden, "INVALID_OPERATOR_TOKEN")
		wantFailure(t, h.sessionCredential(bearer, SessionCredentialRequest{Pod: sessionPod, Owner: "sjawhar"}),
			http.StatusForbidden, "INVALID_OPERATOR_TOKEN")
	}
	if len(enroller.made) != 0 || len(pods.writes) != 0 {
		t.Fatalf("enrollments %+v and writes %+v after six refusals, want none", enroller.made, pods.writes)
	}
}

// A pod the namespace does not hold is refused before the broker or GitHub is reached, and so is
// one of Legion's own: neither route can be aimed at a claim's pod.
func TestSessionRoutesRefuseAPodThatIsNotASessionPod(t *testing.T) {
	enroller := &fakeEnroller{}
	pods := &fakeSessionPods{}
	h := newSessionHarness(t, enroller, pods, tokenSource{})

	for _, tc := range []struct {
		pod  string
		code string
		at   int
	}{
		{"k8s-session-gone", "POD_NOT_FOUND", http.StatusNotFound},
		{legionPod, "POD_NOT_SESSION", http.StatusConflict},
	} {
		t.Run(tc.pod, func(t *testing.T) {
			enroll := enrollRequest()
			enroll.Pod = tc.pod
			wantFailure(t, h.sessionEnroll(testOperatorToken, enroll), tc.at, tc.code)
			wantFailure(t, h.sessionCredential(testOperatorToken, SessionCredentialRequest{Pod: tc.pod, Owner: "sjawhar"}), tc.at, tc.code)
		})
	}
	if len(enroller.made) != 0 || len(pods.writes) != 0 {
		t.Fatalf("enrollments %+v and writes %+v for pods that are no session's, want none", enroller.made, pods.writes)
	}
}

// A caller that read the pod may say which pod it meant: a uid that is not the one the daemon
// resolved is a pod of that name replaced since, and is refused rather than served.
func TestSessionRoutesRefuseAPodUidThatIsNotTheResolvedOne(t *testing.T) {
	enroller := &fakeEnroller{}
	pods := &fakeSessionPods{}
	h := newSessionHarness(t, enroller, pods, tokenSource{})

	enroll := enrollRequest()
	enroll.PodUID = "uid-the-pod-before-it-was-replaced"
	wantFailure(t, h.sessionEnroll(testOperatorToken, enroll), http.StatusConflict, "POD_UID_MISMATCH")
	wantFailure(t, h.sessionCredential(testOperatorToken, SessionCredentialRequest{
		Pod: sessionPod, PodUID: "uid-the-pod-before-it-was-replaced", Owner: "sjawhar",
	}), http.StatusConflict, "POD_UID_MISMATCH")
	if len(enroller.made) != 0 || len(pods.writes) != 0 {
		t.Fatalf("enrollments %+v and writes %+v after a uid mismatch, want none", enroller.made, pods.writes)
	}
}

// A deployment that enrolls no pod with the broker (`runtime.kubernetes.agent_secrets` unset) has
// no enrollment to make, and a runtime that runs no pods has no session pod at all: each says so
// with a code of its own rather than failing somewhere deeper.
func TestSessionRoutesRefuseWhatTheDeploymentDoesNotHave(t *testing.T) {
	noBroker := newSessionHarness(t, nil, &fakeSessionPods{}, tokenSource{})
	wantFailure(t, noBroker.sessionEnroll(testOperatorToken, enrollRequest()), http.StatusConflict, "AGENT_SECRETS_UNCONFIGURED")

	noPods := newSessionHarness(t, &fakeEnroller{}, nil, tokenSource{})
	wantFailure(t, noPods.sessionEnroll(testOperatorToken, enrollRequest()), http.StatusConflict, "SESSION_PODS_UNAVAILABLE")
	wantFailure(t, noPods.sessionCredential(testOperatorToken, SessionCredentialRequest{Pod: sessionPod, Owner: "sjawhar"}),
		http.StatusConflict, "SESSION_PODS_UNAVAILABLE")
}

// Each route names the field it is missing, in the coded shape every credential route uses.
func TestSessionRoutesRefuseAMissingField(t *testing.T) {
	h := newSessionHarness(t, &fakeEnroller{}, &fakeSessionPods{}, tokenSource{})
	for _, req := range []SessionEnrollRequest{
		{Thumbprint: "tp", PodToken: "a.b.c"},
		{Pod: sessionPod, PodToken: "a.b.c"},
		{Pod: sessionPod, Thumbprint: "tp"},
	} {
		wantFailure(t, h.sessionEnroll(testOperatorToken, req), http.StatusBadRequest, "MISSING_FIELD")
	}
	for _, req := range []SessionCredentialRequest{{Owner: "sjawhar"}, {Pod: sessionPod}} {
		wantFailure(t, h.sessionCredential(testOperatorToken, req), http.StatusBadRequest, "MISSING_FIELD")
	}
}

// A broker refusal a retry cannot change is told apart from the broker being out of reach, so the
// caller knows whether asking again can answer differently.
func TestSessionEnrollmentTellsARefusalFromAnOutage(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		at   int
		code string
	}{
		{"a refusal", &agentsecrets.APIError{Status: http.StatusBadRequest, Code: "POD_TOKEN_INVALID", Message: "the token names another pod"},
			http.StatusForbidden, "ENROLLMENT_REFUSED"},
		{"no login yet", &agentsecrets.APIError{Code: "NO_MACHINE_CREDENTIAL", Message: "no launcher credential"},
			http.StatusServiceUnavailable, "ENROLLMENT_UNAVAILABLE"},
		{"the broker out of reach", errors.New("dial the broker: connection refused"),
			http.StatusServiceUnavailable, "ENROLLMENT_UNAVAILABLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSessionHarness(t, &fakeEnroller{err: tc.err}, &fakeSessionPods{}, tokenSource{})
			wantFailure(t, h.sessionEnroll(testOperatorToken, enrollRequest()), tc.at, tc.code)
		})
	}
}

// The gh bundle a session gets is the one an implementer pod of the same owner already gets: the
// implement App's write token for the owner it names, which is also its default, and that App's
// read-only token for every other owner of the project. The answer names what was written and
// carries no token.
func TestSessionCredentialWritesTheImplementBundleForTheOwner(t *testing.T) {
	pods := &fakeSessionPods{}
	h := newSessionHarness(t, &fakeEnroller{}, pods, tokenSource{})

	recorder := h.sessionCredential(testOperatorToken, SessionCredentialRequest{Pod: sessionPod, PodUID: sessionPodUID, Owner: "SJAWHAR"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("session github credential = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	var answer SessionCredentialResponse
	decodeInto(t, recorder, &answer)
	if answer.SecretName != sessionPod+"-gh" || answer.PodUID != sessionPodUID || answer.Default != "sjawhar" ||
		answer.App != string(appauth.Implement) || !slices.Equal(answer.Owners, []string{"acme", "sjawhar"}) || answer.ExpiresAt == "" {
		t.Fatalf("answer = %+v, want the pod's Secret, the implement App, sjawhar default and both owners", answer)
	}
	if body := recorder.Body.String(); strings.Contains(body, "installation-token") {
		t.Fatalf("the answer carries token text: %s", body)
	}
	if len(pods.writes) != 1 {
		t.Fatalf("writes = %+v, want exactly one bundle", pods.writes)
	}
	written := pods.writes[0]
	if written.Default != "sjawhar" || !strings.Contains(written.Owners["sjawhar"].Hosts, "installation-token\n") {
		t.Fatalf("the written bundle's default owner holds %q, want the write token", written.Owners["sjawhar"].Hosts)
	}
	if !strings.Contains(written.Owners["acme"].Hosts, "installation-token-read") {
		t.Fatalf("the written bundle's other owner holds %q, want the read-only token", written.Owners["acme"].Hosts)
	}
}

// An owner the project does not serve buys no lease: the refusal is appauth's own sentinel,
// checked before anything is minted, so naming an owner is no authority to read it. An owner that
// is no GitHub login at all is refused before that.
func TestSessionCredentialRefusesAnOwnerOutsideTheProject(t *testing.T) {
	pods := &fakeSessionPods{}
	h := newSessionHarness(t, &fakeEnroller{}, pods, tokenSource{})

	recorder := h.sessionCredential(testOperatorToken, SessionCredentialRequest{Pod: sessionPod, Owner: "someone-else"})
	sentence := recorder.Body.String()
	wantFailure(t, recorder, http.StatusBadRequest, "OWNER_OUTSIDE_PROJECT")
	if !strings.Contains(sentence, appauth.ErrOwnerOutsideProject.Error()) {
		t.Fatalf("refusal = %s, want it to name %q", sentence, appauth.ErrOwnerOutsideProject)
	}
	wantFailure(t, h.sessionCredential(testOperatorToken, SessionCredentialRequest{Pod: sessionPod, Owner: "not a login"}),
		http.StatusBadRequest, "INVALID_OWNER")
	if len(pods.writes) != 0 {
		t.Fatalf("writes = %+v, want none for an owner outside the project", pods.writes)
	}
}

// A daemon with no GitHub App for this project, one GitHub refuses to mint for, and a Secret the
// cluster will not take are three different answers, each with a code of its own.
func TestSessionCredentialNamesWhyItCouldNotServe(t *testing.T) {
	noApp := newSessionHarness(t, &fakeEnroller{}, &fakeSessionPods{}, nil)
	wantFailure(t, noApp.sessionCredential(testOperatorToken, SessionCredentialRequest{Pod: sessionPod, Owner: "sjawhar"}),
		http.StatusInternalServerError, "GITHUB_TOKEN_SOURCE_UNAVAILABLE")

	down := newSessionHarness(t, &fakeEnroller{}, &fakeSessionPods{}, failingTokens{})
	wantFailure(t, down.sessionCredential(testOperatorToken, SessionCredentialRequest{Pod: sessionPod, Owner: "sjawhar"}),
		http.StatusBadGateway, "GITHUB_TOKEN_FAILED")

	unwritable := newSessionHarness(t, &fakeEnroller{}, &fakeSessionPods{writeErr: errors.New("the API server said no")}, tokenSource{})
	wantFailure(t, unwritable.sessionCredential(testOperatorToken, SessionCredentialRequest{Pod: sessionPod, Owner: "sjawhar"}),
		http.StatusBadGateway, "SESSION_SECRET_WRITE_FAILED")

	unreadable := newSessionHarness(t, &fakeEnroller{}, &fakeSessionPods{readErr: errors.New("the API server is unreachable")}, tokenSource{})
	wantFailure(t, unreadable.sessionCredential(testOperatorToken, SessionCredentialRequest{Pod: sessionPod, Owner: "sjawhar"}),
		http.StatusBadGateway, "POD_READ_FAILED")
}
