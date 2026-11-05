package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"

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
