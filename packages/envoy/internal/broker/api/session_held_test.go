// session_held_test.go pins what POST /v1/enrollments answers a launcher reaching for a session
// name another principal already holds (dispatch://INFRA-1167): 403 SESSION_HELD, the session
// named so the operator reading it knows which name is taken, and nothing of whoever holds it.
package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// TestEnrollingUnderANameAnotherPrincipalHoldsIs403SessionHeld: one person's box enrols a session
// name, a second person's box enrols that very name, and the broker refuses the second — a grant
// follows a name under one principal, so the name belongs to one principal
// (enroll.ErrSessionHeld). The body names the session, so an operator reading it knows which name
// is taken, and names no holder, since whose the name is, is not the refused caller's to learn.
// What the refusal says beyond the session is nobody's to depend on, so none of it is pinned here.
func TestEnrollingUnderANameAnotherPrincipalHoldsIs403SessionHeld(t *testing.T) {
	ts := newTestServer(t)
	session := "role-" + t.Name()
	hers, herKey := ts.mintLauncherCredential(t, testApprover, "example-host-devbox")
	status, body := ts.launcher(t, herKey, hers, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "session", "runtime_id": "box-" + t.Name(), "thumbprint": thumbprintOf(t, newSigningKey(t)),
		"session_id": session,
	})
	if status != http.StatusCreated {
		t.Fatalf("the first principal's POST /v1/enrollments = %d, want 201: %s", status, body)
	}

	theirs, theirKey := ts.mintLauncherCredential(t, "mallory@example.com", "example-host-devbox-m")
	status, body = ts.launcher(t, theirKey, theirs, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "session", "runtime_id": "box-m-" + t.Name(), "thumbprint": thumbprintOf(t, newSigningKey(t)),
		"session_id": session,
	})
	if status != http.StatusForbidden {
		t.Fatalf("the second principal's POST /v1/enrollments = %d, want 403: %s", status, body)
	}
	refusal := decode[wireError](t, body)
	if refusal.Code != "SESSION_HELD" {
		t.Fatalf("the refusal's code = %q, want SESSION_HELD: %s", refusal.Code, body)
	}
	if !strings.Contains(refusal.Error, session) {
		t.Fatalf("the refusal reads %q, want it to name the session %s so the operator knows which name is taken", refusal.Error, session)
	}
	for _, leaked := range []string{testApprover, "person:", "mallory@example.com"} {
		if strings.Contains(refusal.Error, leaked) {
			t.Fatalf("the refusal reads %q, want nothing of who holds the name in it (%q)", refusal.Error, leaked)
		}
	}
}
