package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	legionclaim "github.com/sjawhar/legion/daemon/internal/claim"
)

// registerAndReady brings the claim's agent up as the plugin does and answers what it learned:
// the secret its later calls, the turn outcomes among them, authenticate with.
func (d *operatorDaemon) registerAndReady(token legionclaim.Token) legionclaim.RegisterResponse {
	d.t.Helper()
	bootToken := ""
	for _, call := range d.runtime.CallsOf("Spawn") {
		if call.Spec.Claim == token {
			bootToken = call.Spec.BootToken
		}
	}
	if bootToken == "" {
		d.t.Fatalf("no launch of %s was made", token)
	}
	session := "ses_" + string(token)
	var registered legionclaim.RegisterResponse
	d.post("/legion/v1/claims/register", legionclaim.RegisterRequest{
		BootToken: bootToken, SessionID: session, OmpSessionFile: "/sessions/" + string(token) + ".jsonl",
		AgentID: "agent", PluginContract: api.DaemonAPIVersion,
	}, &registered)
	d.post("/legion/v1/claims/ready", legionclaim.ReadyRequest{
		ClaimToken: token, SessionID: session, Secret: registered.Secret, Generation: registered.Generation,
	}, nil)
	return registered
}

// A claim whose provider refuses launch_failure_limit turns in a row fails with the provider's
// refusal as its cause, and `legion claims retry --fresh-session` starts it in a new session
// rather than resuming the one every turn of which was refused: the runtime is asked to spawn,
// with no transcript to resume.
func TestClaimsRetryFreshSessionStartsTheRefusedClaimInANewSession(t *testing.T) {
	d := newOperatorDaemon(t)
	d.succeed(d.spawnArgs()...)
	registered := d.registerAndReady(architectClaim)

	var answer legionclaim.TurnOutcomeResponse
	for turn := uint64(1); turn <= 3; turn++ {
		answer = legionclaim.TurnOutcomeResponse{}
		d.post("/legion/v1/claims/turn-outcome", legionclaim.TurnOutcomeRequest{
			ClaimToken: architectClaim, SessionID: "ses_" + string(architectClaim), Secret: registered.Secret,
			Generation: 1, Sequence: turn, Outcome: legionclaim.TurnRefused,
			Message: "the provider refused the request",
		}, &answer)
	}
	if answer != (legionclaim.TurnOutcomeResponse{RefusedRun: 3, Failed: true}) {
		t.Fatalf("the third refusal answered %+v, want a run of 3 that failed the claim", answer)
	}
	if out := d.succeed(append([]string{"list"}, d.reach()...)...); !strings.Contains(out, " failed ") {
		t.Fatalf("the claim list reads %q, want the claim failed by its provider's refusals", out)
	}
	spawns, resumes := len(d.runtime.CallsOf("Spawn")), len(d.runtime.CallsOf("Resume"))

	out := d.succeed(append(append([]string{"retry"}, d.reach()...), "--claim", string(architectClaim), "--fresh-session")...)

	seen := d.lastOperatorRequest()
	wantOperatorRequest(t, seen, http.MethodPost, "/legion/v1/operator/claims/"+string(architectClaim)+"/retry")
	if want := `{"freshSession":true}`; string(seen.body) != want {
		t.Fatalf("retry --fresh-session sent %q, want %q", seen.body, want)
	}
	if !strings.Contains(out, " launching ") {
		t.Fatalf("retry --fresh-session stdout = %q, want the claim launching again", out)
	}
	if got := len(d.runtime.CallsOf("Resume")); got != resumes {
		t.Fatalf("the runtime saw %d resumes, want the refused session left alone", got-resumes)
	}
	started := d.runtime.CallsOf("Spawn")
	if len(started) != spawns+1 {
		t.Fatalf("the runtime saw %d spawns, want the retry's fresh start", len(started)-spawns)
	}
	if file := started[len(started)-1].Spec.ResumeSessionFile; file != "" {
		t.Fatalf("the fresh start resumes %q, want a session of its own", file)
	}
}
