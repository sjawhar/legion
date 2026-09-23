package api

import (
	"bytes"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

func secretHash(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// The registration answers who the agent is and a secret for its later calls, and the secret's
// hash is on the claim row before the answer is written: a daemon that restarts one instruction
// later still recognises the agent.
func TestRegisterIssuesASecretWhoseHashIsStoredBeforeTheResponse(t *testing.T) {
	h := newHarness(t)
	token, boot := h.launch("LEGION-208", claim.RoleArchitect)

	got := h.registered(boot, "ses_architect")

	want := claim.RegisterResponse{
		ClaimToken: token, Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleArchitect, Generation: 1,
		Secret: got.Secret,
	}
	if got != want || got.Secret == "" {
		t.Fatalf("register answered %+v, want %+v with a secret", got, want)
	}
	stored := h.stored(token)
	if !bytes.Equal(stored.CapabilityHash, secretHash(got.Secret)) {
		t.Errorf("stored capability hash %x, want the hash of the secret the response carried", stored.CapabilityHash)
	}
	if stored.State != supervise.StateRegistered || stored.Session != "ses_architect" ||
		stored.SessionFile != "/sessions/ses_architect.jsonl" {
		t.Errorf("stored %+v, want the registration's session and transcript recorded", stored)
	}
	if bytes.Contains([]byte(stored.Session+stored.SessionFile), []byte(got.Secret)) {
		t.Error("the secret itself reached the claim row")
	}
}

func TestRegisterRefusesABootTokenNoLaunchMinted(t *testing.T) {
	h := newHarness(t)
	h.launch("LEGION-208", claim.RoleArchitect)

	wantRefusal(t, h.register("a token nothing minted", "ses_architect"), http.StatusForbidden, "Invalid boot token")
}

// A pane the daemon has already replaced registering with its own launch's token answers for a
// generation the claim has left: the agent of that pane is not the one the claim runs now.
func TestRegisterRefusesTheBootTokenOfALaunchTheClaimHasReplaced(t *testing.T) {
	h := newHarness(t)
	token, first := h.launch("LEGION-208", claim.RoleImplementer)
	h.relaunch(token)

	wantRefusal(t, h.register(first, "ses_implementer"), http.StatusConflict, "Stale generation")
	if stored := h.stored(token); stored.Session != "" || stored.CapabilityHash != nil {
		t.Errorf("the refused registration was recorded: %+v", stored)
	}
}

// A claim resumes the agent it recorded or none: another session arriving on its launch — on a
// relaunch, or with the token a first registration already spent — is refused with the shipped
// sentence the plugin exits on.
func TestRegisterRefusesAnotherSessionOnAClaimThatRecordedOne(t *testing.T) {
	h := newHarness(t)
	token, first := h.launch("LEGION-208", claim.RoleImplementer)
	h.registered(first, "ses_implementer")

	t.Run("the same launch's token", func(t *testing.T) {
		wantRefusal(t, h.register(first, "ses_intruder"), http.StatusConflict, claim.SameAgentRefusal.Message)
	})
	t.Run("a resumed launch", func(t *testing.T) {
		resumed := h.relaunch(token)
		if calls := h.runtime.CallsOf("Resume"); len(calls) != 1 {
			t.Fatalf("the relaunch made %d resumes, want the recorded session resumed", len(calls))
		}
		wantRefusal(t, h.register(resumed, "ses_fresh_agent"), http.StatusConflict, claim.SameAgentRefusal.Message)
	})
	if stored := h.stored(token); stored.Session != "ses_implementer" {
		t.Errorf("stored session %q, want the recorded one kept", stored.Session)
	}
}

// The recorded session registering again — its plugin restarted inside the same pane, or the
// resumed agent — is issued a new secret, and the old one stops working.
func TestTheRecordedSessionRegisteringAgainIsReissuedASecret(t *testing.T) {
	h := newHarness(t)
	token, boot := h.launch("LEGION-208", claim.RoleTester)
	first := h.registered(boot, "ses_tester")

	second := h.registered(boot, "ses_tester")

	if second.Secret == "" || second.Secret == first.Secret {
		t.Fatalf("re-registration issued %q after %q, want a new secret", second.Secret, first.Secret)
	}
	if !bytes.Equal(h.stored(token).CapabilityHash, secretHash(second.Secret)) {
		t.Error("the stored hash is not the reissued secret's")
	}
	stale := claim.ReadyRequest{ClaimToken: token, SessionID: "ses_tester", Secret: first.Secret, Generation: 1}
	wantRefusal(t, h.request(http.MethodPost, "/legion/v1/claims/ready", stale, nil), http.StatusForbidden, "Invalid session secret")
	current := stale
	current.Secret = second.Secret
	if recorder := h.request(http.MethodPost, "/legion/v1/claims/ready", current, nil); recorder.Code != http.StatusNoContent {
		t.Fatalf("ready with the reissued secret = %d, want 204; body %s", recorder.Code, recorder.Body)
	}
}

// The hash is persisted before the response or there is no response: a registration the store
// would not take is a 500 that carries no secret, so no agent holds a secret the daemon forgot.
func TestRegisterAnswers500AndNoSecretWhenTheStoreRefusesTheWrite(t *testing.T) {
	h := newHarness(t)
	token, boot := h.launch("LEGION-208", claim.RoleArchitect)
	err := h.store.Tx(h.ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(h.ctx, `create function refuse_capability() returns trigger language plpgsql as
			$$ begin raise exception 'the capability write is refused'; end $$;
			create trigger refuse_capability before update on claims for each row
			when (new.capability_hash is distinct from old.capability_hash) execute function refuse_capability();`)
		return err
	})
	if err != nil {
		t.Fatalf("install the refusing trigger: %v", err)
	}

	recorder := h.register(boot, "ses_architect")

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", recorder.Code, recorder.Body)
	}
	if strings.Contains(recorder.Body.String(), "secret") {
		t.Fatalf("the failed registration answered %s, which names a secret", recorder.Body)
	}
	if stored := h.stored(token); stored.CapabilityHash != nil || stored.State != supervise.StateLaunching {
		t.Errorf("stored %+v, want the refused registration absent from the row", stored)
	}
}

// A request the claim's state does not allow is refused with the machine's reason: a retired
// claim registers nothing.
func TestRegisterOnARetiredClaimIsRefusedWithTheMachinesReason(t *testing.T) {
	h := newHarness(t)
	token, boot := h.launch("LEGION-208", claim.RoleArchitect)
	m, _ := h.supervisor.Machine(token)
	if err := m.Handle(h.ctx, supervise.RequestStop{Claim: token}); err != nil {
		t.Fatalf("stop: %v", err)
	}

	wantRefusal(t, h.register(boot, "ses_architect"), http.StatusConflict,
		"register refused: the claim is retired (the claim is retired)")
}

func TestClaimRoutesRefuseABodyTheyDoNotRead(t *testing.T) {
	h := newHarness(t)
	for _, testCase := range []struct {
		name, route, body, wantFragment string
	}{
		{"not JSON", "register", `{"bootToken":`, "invalid request body"},
		{"an unknown member", "register", `{"bootToken":"b","sessionId":"s","ompSessionFile":"f","agentId":"a","pluginContract":1,"workerSecret":"x"}`, "workerSecret"},
		{"no session", "register", `{"bootToken":"b","ompSessionFile":"f","agentId":"a","pluginContract":1}`, "sessionId is required"},
		{"no transcript", "register", `{"bootToken":"b","sessionId":"s","agentId":"a","pluginContract":1}`, "ompSessionFile is required"},
		{"no secret", "ready", `{"claimToken":"c","sessionId":"s","generation":1}`, "secret is required"},
		{"no reason", "exit", `{"claimToken":"c","sessionId":"s","secret":"x","generation":1}`, "reason is required"},
		{"two bodies", "ready", `{"claimToken":"c","sessionId":"s","secret":"x","generation":1}{}`, "invalid request body"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := h.request(http.MethodPost, "/legion/v1/claims/"+testCase.route, testCase.body, nil)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", recorder.Code, recorder.Body)
			}
			if !strings.Contains(recorder.Body.String(), testCase.wantFragment) {
				t.Errorf("body %s does not name %q", recorder.Body, testCase.wantFragment)
			}
		})
	}
}

func TestReadyMakesTheRegisteredClaimReady(t *testing.T) {
	h := newHarness(t)
	token, boot := h.launch("LEGION-208", claim.RoleArchitect)
	registered := h.registered(boot, "ses_architect")

	recorder := h.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: "ses_architect", Secret: registered.Secret, Generation: registered.Generation,
	}, nil)

	if recorder.Code != http.StatusNoContent || recorder.Body.Len() != 0 {
		t.Fatalf("ready = %d %q, want 204 and no body", recorder.Code, recorder.Body)
	}
	if stored := h.stored(token); stored.State != supervise.StateReady {
		t.Fatalf("stored state %s, want ready", stored.State)
	}
}

// ready and exit are the agent's own requests: each carries the secret its registration issued,
// the generation it runs at, and the session it is — and each refusal is the one that failed.
func TestReadyAndExitRefuseWhatTheyCannotAuthenticateOrFence(t *testing.T) {
	for _, route := range []string{"ready", "exit"} {
		t.Run(route, func(t *testing.T) {
			h := newHarness(t)
			token, boot := h.launch("LEGION-208", claim.RoleImplementer)
			registered := h.registered(boot, "ses_implementer")
			send := func(token claim.Token, session, secret string, generation uint64) *httptest.ResponseRecorder {
				body := map[string]any{"claimToken": token, "sessionId": session, "secret": secret, "generation": generation}
				if route == "exit" {
					body["reason"] = "phase complete"
				}
				return h.request(http.MethodPost, "/legion/v1/claims/"+route, body, nil)
			}

			wantRefusal(t, send(token, "ses_implementer", "not the secret", 1), http.StatusForbidden, "Invalid session secret")
			wantRefusal(t, send("legion-legion-legion-999-tester", "ses_implementer", registered.Secret, 1),
				http.StatusForbidden, "Invalid session secret")
			wantRefusal(t, send(token, "ses_other", registered.Secret, 1), http.StatusConflict, claim.SameAgentRefusal.Message)

			// The relaunch has not registered yet, so the claim still holds the first launch's
			// secret: what is refused is the generation, not the secret.
			h.relaunch(token)
			wantRefusal(t, send(token, "ses_implementer", registered.Secret, 1), http.StatusConflict, "Stale generation")
			if stored := h.stored(token); stored.State != supervise.StateLaunching || stored.Generation != 2 {
				t.Errorf("stored %s at generation %d, want the relaunch untouched by the refused %s", stored.State, stored.Generation, route)
			}
		})
	}
}

func TestExitRetiresTheClaim(t *testing.T) {
	h := newHarness(t)
	token, boot := h.launch("LEGION-208", claim.RoleReviewer)
	registered := h.registered(boot, "ses_reviewer")

	recorder := h.request(http.MethodPost, "/legion/v1/claims/exit", claim.ExitRequest{
		ClaimToken: token, SessionID: "ses_reviewer", Secret: registered.Secret, Generation: 1, Reason: "phase complete",
	}, nil)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("exit = %d, want 204; body %s", recorder.Code, recorder.Body)
	}
	if stored := h.stored(token); stored.State != supervise.StateRetired || stored.Locator != nil {
		t.Fatalf("stored %+v, want the claim retired with no process", stored)
	}
	if stops := h.runtime.CallsOf("Stop"); len(stops) != 0 {
		t.Errorf("the agent's own exit stopped its process %d times; it is already ending itself", len(stops))
	}
}

// Nothing in the register flow depends on the shim's hello: an agent whose registration
// overtakes its shim's hello still registers.
func TestRegisterBeforeTheHelloRegisters(t *testing.T) {
	h := newHarness(t)
	token, boot := h.launch("LEGION-208", claim.RoleArchitect)
	if state := h.stored(token).State; state != supervise.StateLaunching {
		t.Fatalf("precondition: stored state %s, want launching", state)
	}
	h.registered(boot, "ses_architect")
	if loc := h.stored(token).Locator; loc == nil || loc.Runtime != runtime.RuntimeTmux {
		t.Fatalf("stored locator %+v, want the launch's", loc)
	}
}
