package sandbox

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
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

// keptBrokerURL is the secrets broker a deployment enrols with after its pods were made, and
// movedBrokerURL where an operator moves that broker afterwards.
const (
	keptBrokerURL  = "https://secrets.internal.example"
	movedBrokerURL = "https://secrets-next.internal.example"
)

// enrolledWith enrols the runtime's pods with the secrets broker at url
// (runtime.kubernetes.agent_secrets): the deployment change no pod created before it carries, so
// a launch into such a pod keeps it and starts its role without the broker.
func enrolledWith(url string) func(*Options) {
	return func(o *Options) {
		o.AgentSecrets = &AgentSecrets{URL: url, Audience: "agent-secrets", TokenExpiry: time.Hour}
	}
}

// The record of a generation says what that generation was handed, never what the configuration
// reads: the launch writes it on the issue Sandbox before the start (recordAddresses), and it is
// the only account of the running process a later daemon has (movedEnvironment). A role started
// in a kept pod that carries no enrollment is handed no broker at all — no agent-secrets flag, no
// AGENT_SECRETS_URL — so its record must name that variable unset. A record claiming a broker for
// it would describe a process that does not exist, and the variable left out of the record
// entirely reads as moved ("(unrecorded)") the first time anything is compared.
func TestAKeptPodWithoutTheEnrollmentRecordsNoBrokerForItsGeneration(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(workerSpec(t))
	r2 := restartedOver(t, g, loc, enrolledWith(keptBrokerURL))
	kept := resumeTester(t, g, r2, loc)
	if kept.Sandbox.PodUID != loc.Sandbox.PodUID {
		t.Fatalf("Resume replaced the drifted pod; this test is about the pod a launch keeps")
	}
	s := g.sandbox(loc.Sandbox.Name)
	if s == nil {
		t.Fatalf("the issue sandbox %s is absent", loc.Sandbox.Name)
	}
	var record addressRecord
	if err := json.Unmarshal([]byte(s.Annotations[addressesAnnotation(claim.RoleTester)]), &record); err != nil {
		t.Fatalf("read the tester's address record %q: %v", s.Annotations[addressesAnnotation(claim.RoleTester)], err)
	}
	if record.Generation != kept.Sandbox.Generation {
		t.Fatalf("the record is of generation %d, want the generation %d this launch started", record.Generation, kept.Sandbox.Generation)
	}
	i := slices.IndexFunc(record.Addresses, func(a recordedAddress) bool { return a.Variable == "AGENT_SECRETS_URL" })
	if i < 0 {
		t.Fatalf("the record %+v names no AGENT_SECRETS_URL at all, which reads as (unrecorded) and so as moved", record.Addresses)
	}
	if held := record.Addresses[i]; held.Name != "(unset)" || held.SHA256 != addressDigest("") {
		t.Errorf("the record holds AGENT_SECRETS_URL %+v, want it unset: the generation it records was started with no broker and its pod mounts no %s",
			held, roleVolume(agentSecretsKeyVolume, claim.RoleTester))
	}
}

// Moving a broker a generation never held moves nothing of that generation. An observation
// compares the record with what a generation started now in that same pod would be handed
// (movedEnvironment), and that pod carries no enrollment, so both sides hold no broker URL and
// the role reads Alive. Reading it StaleAddress instead would have the supervisor repoint the
// role mid-turn, which is the thing the update round exists to prevent: the pod is replaced once
// every worker in it has pushed its work and answered, and never under a running turn.
func TestAKeptPodsGenerationIsNotStaleWhenTheBrokerItNeverHeldMoves(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(workerSpec(t))
	r2 := restartedOver(t, g, loc, enrolledWith(keptBrokerURL))
	kept := resumeTester(t, g, r2, loc)
	if kept.Sandbox.PodUID != loc.Sandbox.PodUID {
		t.Fatalf("Resume replaced the drifted pod; this test is about the pod a launch keeps")
	}
	r3 := restartedOver(t, g, kept, enrolledWith(movedBrokerURL))
	if obs := readopt(t, g.ctx, r3, kept)[0]; obs.Kind != runtime.Alive {
		t.Errorf("%s (%s), want Alive: the operator moved the broker from %s to %s, and the tester's generation was handed neither",
			obs.Kind, obs.Detail, keptBrokerURL, movedBrokerURL)
	}
}
