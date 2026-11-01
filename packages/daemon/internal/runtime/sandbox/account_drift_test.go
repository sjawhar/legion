package sandbox

import (
	"log/slog"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// otherAccount is the ServiceAccount an operator moves runtime.kubernetes.pod.service_account to
// over pods the configuration before it created: distinct from testOptions()'s own account, which
// names none, so every pod a rig launches runs as the namespace's default.
const otherAccount = "legion-worker"

// onAccount edits a stored pod to run as account, as the API server records the account a pod was
// created under: the one field accountDrift reads.
func onAccount(account string) func(*corev1.Pod) {
	return func(pod *corev1.Pod) { pod.Spec.ServiceAccountName = account }
}

// A pod created under a ServiceAccount other than the configured one runs its agents with that
// account's identity for the pod's whole life: nothing the daemon writes moves it, so a relaunch
// of any of its roles replaces it, as one on another image is replaced (accountDrift beside
// imageDrift), the daemon-launched controller's pod included, and the Sandbox is kept under the
// new pod (an issue pod's disk is the pod's own ephemeral volume and goes with it, as for image
// drift). Unlike image drift, a launch installing a stale-plugin pair is no exemption: the install
// cannot move the account either, so such a pod is replaced and the launch starts plain into a
// fresh pod already on the configured account. A pod already on the configured account is kept,
// the role's new generation started in it — including a pod on `default` under a configuration
// naming no account, which is that same account spelled the other way.
func TestARelaunchReplacesAPodRunningAnotherServiceAccount(t *testing.T) {
	plugins := &runtime.PluginInstall{PiLegion: "9.7.0", PiEnvoy: "9.6.1", Contract: 24}
	for name, tc := range map[string]struct {
		spec     func(*testing.T) runtime.SpawnSpec
		session  string
		account  string            // the account the restarted daemon is configured with
		stored   func(*corev1.Pod) // what the stored pod is edited to run as before the restart
		plugins  *runtime.PluginInstall
		replaced bool
	}{
		"an issue pod under a configuration naming another ServiceAccount":         {workerSpec, resumeSession, otherAccount, nil, nil, true},
		"the controller's pod under a configuration naming another ServiceAccount": {controllerSpec, controllerSession, otherAccount, nil, nil, true},
		"an installing Resume replaces a pod on another ServiceAccount":            {workerSpec, resumeSession, otherAccount, nil, plugins, true},
		"an issue pod already on the configured ServiceAccount":                    {workerSpec, resumeSession, otherAccount, onAccount(otherAccount), nil, false},
		"no configured ServiceAccount and a pod on default":                        {workerSpec, resumeSession, "", onAccount(defaultServiceAccount), nil, false},
		"no configured ServiceAccount and a pod on another one":                    {workerSpec, resumeSession, "", onAccount(otherAccount), nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil)
			spec := tc.spec(t)
			loc := g.spawn(spec)
			sandboxUID := g.sandbox(loc.Sandbox.Name).UID
			if tc.stored != nil {
				g.update(g.pod(loc.Sandbox.Name), tc.stored)
			}
			r2 := secondRuntime(t, g, func(o *Options) { o.Pod.ServiceAccount = tc.account })()
			g.connectRunning(spec.Claim, loc.Sandbox.Generation)
			g.eventually("the role's launcher to report its child to the restarted daemon", func() bool {
				state, connected := r2.launchers.state(spec.Claim, loc.Sandbox.PodUID)
				return connected && state.Child != nil
			})
			spec.Generation, spec.BootToken, spec.ResumeSessionFile, spec.Plugins = 2, "boot-g2", tc.session, tc.plugins
			newLoc, err := r2.Resume(g.ctx, &loc, spec)
			if err != nil {
				t.Fatalf("Resume: %v", err)
			}
			if replaced := newLoc.Sandbox.PodUID != loc.Sandbox.PodUID; replaced != tc.replaced {
				t.Fatalf("Resume replaced the pod: %t, want %t (pod %s, then %s)", replaced, tc.replaced, loc.Sandbox.PodUID, newLoc.Sandbox.PodUID)
			}
			if newLoc.Sandbox.Generation != 2 {
				t.Fatalf("resumed at %+v, want generation 2", newLoc.Sandbox)
			}
			// The replacement is the Sandbox's next pod, never another Sandbox: the issue keeps
			// its Sandbox and its address record across it; its disk, the pod's own ephemeral
			// volume, does not.
			if after := g.sandbox(loc.Sandbox.Name); after == nil || after.UID != sandboxUID {
				t.Fatalf("the Sandbox is %+v, want the one the launch spawned (%s) kept", after, sandboxUID)
			}
			if tc.replaced {
				if account, drifted := accountDrift(g.pod(loc.Sandbox.Name), tc.account); drifted {
					t.Fatalf("the pod the role runs in runs as %s, want the configured %s", account, serviceAccountOr(tc.account))
				}
			}
			if tc.plugins != nil {
				// The inverse of a kept drifted image's installing Resume, which starts through
				// the install wrapper: this pod was replaced, so its fresh copies are already
				// current and the argv starts the shim directly.
				start := lastStart(t, g, spec.Claim)
				i := slices.Index(start.Argv, "--")
				if i < 0 || i+1 >= len(start.Argv) {
					t.Fatalf("the installing Resume's argv %q names no command after --", start.Argv)
				}
				if start.Argv[i+1] == "/bin/sh" {
					t.Fatalf("a pod replaced for its ServiceAccount was started through an install wrapper: argv %q", start.Argv)
				}
			}
		})
	}
}

// The boot reconciliation names, in the log, each Sandbox whose pod runs as a ServiceAccount other
// than the configured one — a pod created under the account the configuration named before, whose
// agents hold that account's identity until a relaunch replaces the pod — with the account it runs
// as and the configured one, one warning per Sandbox, the daemon-launched controller's included,
// and nothing of a pod already on the configured account. A cache Sandbox is passed over before
// the warning, as it is for the image: its own sweep makes its pod again under the new account,
// with no relaunch to wait for.
func TestTheBootReconciliationWarnsOfEachPodRunningAnotherServiceAccount(t *testing.T) {
	g := newRig(t, nil)
	spec := workerSpec(t)
	issue, controller := g.spawn(spec), g.spawn(controllerSpec(t))
	child := g.spawn(childSpec(t))
	// The child's pod already runs as the account the restarted daemon is configured with.
	g.update(g.pod(child.Sandbox.Name), onAccount(otherAccount))
	logs := &lockedLog{}
	r2 := secondRuntime(t, g, func(o *Options) {
		o.Pod.ServiceAccount = otherAccount
		o.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	})()
	warnings := func(sandbox string) int {
		return strings.Count(logs.String(), "sandbox runtime: "+sandbox+" runs as ServiceAccount ")
	}
	known := []runtime.Known{{Claim: workerToken, Locator: &issue}, {Claim: controllerToken, Locator: &controller}, {Claim: childToken, Locator: &child}}

	if err := r2.ReconcileOrphans(g.ctx, known, 0); err != nil {
		t.Fatal(err)
	}
	for _, loc := range []runtime.Locator{issue, controller} {
		want := "sandbox runtime: " + loc.Sandbox.Name + " runs as ServiceAccount " + defaultServiceAccount +
			", the configured ServiceAccount is " + otherAccount + "; its agents hold that identity until the pod is relaunched"
		if !strings.Contains(logs.String(), want) || warnings(loc.Sandbox.Name) != 1 {
			t.Errorf("the boot reconciliation logged %q, want one warning %q", logs.String(), want)
		}
	}
	if warnings(child.Sandbox.Name) != 0 {
		t.Errorf("the boot reconciliation warned of %s, whose pod runs as the configured account: %q", child.Sandbox.Name, logs.String())
	}
	if cache := CacheSandboxName(testProject, spec.Repository); warnings(cache) != 0 {
		t.Errorf("the boot reconciliation warned of the cache %s, which its own sweep makes again: %q", cache, logs.String())
	}
}
