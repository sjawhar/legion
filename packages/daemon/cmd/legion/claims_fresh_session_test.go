package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// `legion claims retry --fresh-session` sends the one body the retry route reads,
// `{"freshSession":true}`, and the claim starts a new session rather than resuming the one it
// recorded: what a claim whose provider refused its session needs, since resuming it would be
// refused again. A retry without the flag sends no body at all
// (TestClaimsRetryPostsToTheClaimsRetryRoute).
func TestClaimsRetryFreshSessionSendsTheFreshSessionBody(t *testing.T) {
	d := newOperatorDaemon(t)
	refused := fake.SpawnResult{Err: errors.New("the runtime refused the launch")}
	d.runtime.ScriptSpawn(refused, refused, refused)
	if code, _, _ := d.claims(d.spawnArgs()...); code != 1 {
		t.Fatalf("spawn whose every launch the runtime refused exited %d, want 1", code)
	}

	out := d.succeed(append(append([]string{"retry"}, d.reach()...), "--claim", string(architectClaim), "--fresh-session")...)

	seen := d.lastOperatorRequest()
	wantOperatorRequest(t, seen, http.MethodPost, "/legion/v1/operator/claims/"+string(architectClaim)+"/retry")
	if want := `{"freshSession":true}`; string(seen.body) != want {
		t.Fatalf("retry --fresh-session sent %q, want %q", seen.body, want)
	}
	if !strings.Contains(out, " launching ") {
		t.Fatalf("retry --fresh-session stdout = %q, want the claim launching again", out)
	}
	started := d.runtime.CallsOf("Spawn")
	if len(started) != 4 {
		t.Fatalf("the runtime saw %d spawns, want the retry's fresh start after the three refused", len(started))
	}
	if file := started[len(started)-1].Spec.ResumeSessionFile; file != "" {
		t.Fatalf("the fresh start resumes %q, want a session of its own", file)
	}
}

// A fresh session whose phase restart the workflow refused answers 200 with the claim and that
// refusal (api.FreshSessionResponse.PhaseNotRestarted, served on the operator's retry as
// api.OperatorClaim.PhaseNotRestarted), where the route once answered 409. The claim's own line
// says the claim is back, so the refusal is a line of its own under it: without it an operator
// reads a success and no word of the work nothing handed the claim. Every other answer carries
// no refusal and prints the claim's line alone.
func TestARetryWhosePhaseRestartWasRefusedPrintsTheRefusalUnderTheClaim(t *testing.T) {
	retried := api.OperatorClaim{Token: architectClaim, Tree: "LEGION-208", Issue: "LEGION-208",
		Role: "architect", Generation: 3, State: "launching", PluginContract: api.DaemonAPIVersion}
	line := string(architectClaim) + " architect LEGION-208 launching 3 " + thisBinarysContract + " - -\n"
	refusal := "LEGION-208 is still held after the retry of its phase implementing, so nothing relaunched " +
		string(architectClaim) + ": its tree lingers, and a lingering tree's members stay where they stood"
	for _, tc := range []struct {
		name  string
		claim api.OperatorClaim
		want  string
	}{
		{name: "the phase was restarted", claim: retried, want: line},
		{
			name: "the workflow refused the restart",
			claim: func() api.OperatorClaim {
				c := retried
				c.PhaseNotRestarted = "the claim is back in a new session owing no task, and nothing restarted its phase: " + refusal
				return c
			}(),
			want: line + "the claim is back in a new session owing no task, and nothing restarted its phase: " + refusal + "\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer, err := json.Marshal(tc.claim)
			if err != nil {
				t.Fatalf("marshal the claim the route answers: %v", err)
			}
			var out bytes.Buffer
			if err := printClaim(&out, answer); err != nil {
				t.Fatalf("print the claim: %v", err)
			}
			if out.String() != tc.want {
				t.Fatalf("printed %q, want %q", out.String(), tc.want)
			}
		})
	}
}
