package sandbox

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A launch that keeps a drifted pod leaves out the shim's agent-secrets flags when the pod carries
// neither the broker's token volume nor the role's key directory (launcherCommand, launch.enrolls),
// so the role starts and runs without the broker until the update round replaces its pod. Its
// environment must say the same thing. AGENT_SECRETS_URL and AGENT_SECRETS_KEY_DIR are how an
// agent decides whether it holds a broker at all — `skill://legion-worker` has it read exactly
// those two before it reports a resource unreachable, and then run `agent-secrets <SECRET> -- …`
// — and AGENT_SECRETS_KEY_DIR names /var/run/legion/agent-secrets, a directory the kept pod does
// not mount.
//
// A role told it has a broker it has not, in a pod it cannot see is pending replacement, reads its
// own failure as a missing grant and reports a secrets problem. The two variables belong with the
// flags: a launch that enrols its role sets them, and one that does not, does not.
func TestAKeptPodWithoutTheEnrollmentTellsItsRoleOfNoBroker(t *testing.T) {
	g := newRig(t, nil)
	spec := workerSpec(t)
	loc := g.spawn(spec)
	broker := AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
	r2 := secondRuntime(t, g, func(o *Options) { o.AgentSecrets = &broker })()
	g.connectRunning(workerToken, loc.Sandbox.Generation)
	g.eventually("the tester's launcher to report its child to the restarted daemon", func() bool {
		state, connected := r2.launchers.state(workerToken, loc.Sandbox.PodUID)
		return connected && state.Child != nil
	})
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
	kept, err := r2.Resume(g.ctx, &loc, spec)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if kept.Sandbox.PodUID != loc.Sandbox.PodUID {
		t.Fatalf("Resume replaced the drifted pod; this test is about the pod a launch keeps")
	}
	start := lastStart(t, g, workerToken)
	if slices.Contains(start.Argv, "--agent-secrets-key-dir") {
		t.Fatalf("the start carries the agent-secrets flags, so this pod does carry the enrollment and the case proves nothing")
	}
	env := startEnv(start)
	for _, name := range []string{"AGENT_SECRETS_URL", "AGENT_SECRETS_KEY_DIR"} {
		if value, named := env[name]; named && value != "" {
			t.Errorf("the role's generation is told %s=%q while its start enrols it with nothing and its pod mounts no %s; an agent reads those two to decide it holds a broker",
				name, value, roleVolume(agentSecretsKeyVolume, claim.RoleTester))
		}
	}
}
