package sandbox

import (
	"errors"
	"strings"
	"testing"
	"time"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// Suspend of the claim's current process asks the agent to end itself over its connection, then
// sets the Sandbox Suspended; the Sandbox, its Secret, and its session stay for a Resume.
func TestSuspendStopsTheRecordedProcessGracefully(t *testing.T) {
	g := newRig(t, nil)
	conn := fake.NewConn()
	g.conns.Register(workerToken, conn)
	loc := g.spawn(workerSpec(t))
	g.clearActions()
	if err := g.r.Suspend(g.ctx, loc); err != nil {
		t.Fatal(err)
	}
	name := SandboxName(workerToken)
	if conn.Shutdowns() != 1 {
		t.Fatalf("%d shutdown frames, want 1", conn.Shutdowns())
	}
	expectSteps(t, steps(t, g.writes(), name), "suspend")
	if g.sandbox(name) == nil || g.secret(secretName(name)) == nil {
		t.Fatal("suspend removed what a resume needs")
	}
}

// A locator that is no longer the claim's current process is already stopped (decision 3d), with
// one exception that keeps a valid token from running on: a pod recorded by no locator.
func TestSuspendWithAStaleLocator(t *testing.T) {
	name := SandboxName(workerToken)
	t.Run("a newer relaunch by the daemon is left alone", func(t *testing.T) {
		g := newRig(t, nil)
		conn := fake.NewConn()
		g.conns.Register(workerToken, conn)
		old := g.spawn(workerSpec(t))
		if err := g.r.Suspend(g.ctx, old); err != nil {
			t.Fatal(err)
		}
		newer := g.spawn(workerSpec(t))
		g.clearActions()
		shutdowns := conn.Shutdowns()
		if err := g.r.Suspend(g.ctx, old); err != nil {
			t.Fatalf("a stale locator is stopped, not an error: %v", err)
		}
		if writes := g.writes(); len(writes) > 0 || conn.Shutdowns() != shutdowns {
			t.Fatalf("a stale Suspend wrote %v and sent %d shutdown frames", writes, conn.Shutdowns()-shutdowns)
		}
		if g.sandbox(name).mode() != modeRunning || string(g.pod(name).UID) != newer.Incarnation {
			t.Fatal("the newer incarnation was touched")
		}
		if obs, err := g.r.Probe(g.ctx, newer); err != nil || obs.Kind != runtime.Alive {
			t.Fatalf("the newer incarnation after the stale Suspend: %s, %v", obs.Kind, err)
		}
	})
	t.Run("a pod the controller recreated unrecorded is suspended", func(t *testing.T) {
		g := newRig(t, nil)
		conn := fake.NewConn()
		g.conns.Register(workerToken, conn)
		loc := g.spawn(workerSpec(t))
		if err := g.kube.Tracker().Delete(podsGVR, testNamespace, name); err != nil {
			t.Fatal(err)
		}
		g.eventually("the controller to recreate the pod", func() bool {
			pod := g.pod(name)
			return pod != nil && string(pod.UID) != loc.Incarnation
		})
		g.eventually("the store to see it", func() bool {
			pod := g.r.storedPod(name)
			return pod != nil && string(pod.UID) != loc.Incarnation
		})
		g.clearActions()
		if err := g.r.Suspend(g.ctx, loc); err != nil {
			t.Fatal(err)
		}
		expectSteps(t, steps(t, g.writes(), name), "suspend")
		if conn.Shutdowns() != 0 {
			t.Fatal("a shutdown frame went to a process the locator does not name")
		}
	})
	t.Run("no pod and no newer relaunch: the sandbox is suspended before one is recreated", func(t *testing.T) {
		g := newRig(t, nil)
		loc := g.spawn(workerSpec(t))
		g.hold.Store(true)
		if err := g.kube.Tracker().Delete(podsGVR, testNamespace, name); err != nil {
			t.Fatal(err)
		}
		g.eventually("the store to lose the pod", func() bool { return g.r.storedPod(name) == nil })
		g.clearActions()
		if err := g.r.Suspend(g.ctx, loc); err != nil {
			t.Fatal(err)
		}
		expectSteps(t, steps(t, g.writes(), name), "suspend")
	})
}

// Release ends the claim whatever its locator says: the Sandbox is deleted by name. The shutdown
// frame goes to the process the locator records, and only while it is still the claim's running
// pod (decision 3c/3d): a stale or absent locator's process is not acted on, and deleting the
// Sandbox ends whatever pod it holds. A Sandbox already gone is released.
func TestReleaseDeletesTheClaimsSandbox(t *testing.T) {
	name := SandboxName(workerToken)
	for label, tc := range map[string]struct {
		locate    func(runtime.Locator) *runtime.Locator
		shutdowns int
	}{
		"current": {func(loc runtime.Locator) *runtime.Locator { return &loc }, 1},
		"stale": {func(loc runtime.Locator) *runtime.Locator {
			stale := sandboxLocator(workerToken, "uid-pod-long-gone")
			return &stale
		}, 0},
		"no locator": {func(runtime.Locator) *runtime.Locator { return nil }, 0},
	} {
		t.Run(label, func(t *testing.T) {
			g := newRig(t, nil)
			conn := fake.NewConn()
			g.conns.Register(workerToken, conn)
			loc := g.spawn(workerSpec(t))
			g.clearActions()
			if err := g.r.Release(g.ctx, runtime.Known{Claim: workerToken, Locator: tc.locate(loc)}); err != nil {
				t.Fatal(err)
			}
			if g.sandbox(name) != nil {
				t.Fatal("the sandbox survived its release")
			}
			if conn.Shutdowns() != tc.shutdowns {
				t.Fatalf("%d shutdown frames, want %d", conn.Shutdowns(), tc.shutdowns)
			}
			if err := g.r.Release(g.ctx, runtime.Known{Claim: workerToken}); err != nil {
				t.Fatalf("releasing a released claim: %v", err)
			}
		})
	}
}

// Release takes the claim once. A claim paired with another claim's locator is refused before
// anything is sent or deleted: releasing one claim never ends another's Sandbox or process.
func TestReleaseRefusesALocatorOfAnotherClaim(t *testing.T) {
	g := newRig(t, nil)
	conn := fake.NewConn()
	g.conns.Register(workerToken, conn)
	g.spawn(workerSpec(t))
	other := g.spawn(testSpec(t, otherToken, claim.RoleReviewer, testTree))
	g.clearActions()
	if err := g.r.Release(g.ctx, runtime.Known{Claim: workerToken, Locator: &other}); err == nil || !strings.Contains(err.Error(), string(otherToken)) {
		t.Fatalf("Release = %v, want a refusal naming the locator's claim", err)
	}
	if writes := g.writes(); len(writes) > 0 || conn.Shutdowns() != 0 {
		t.Fatalf("the refused release wrote %v and sent %d shutdown frames", writes, conn.Shutdowns())
	}
	if g.sandbox(SandboxName(workerToken)) == nil || g.sandbox(SandboxName(otherToken)) == nil {
		t.Fatal("a refused release deleted a sandbox")
	}
}

// A Release whose delete failed keeps the claim in the watch, so its Sandbox is still observed,
// and leaves the Sandbox to the orphan sweep once the daemon has retired the claim: the sweep
// protects the daemon's known claims, never the watch, so nothing a failed release leaves is
// left unowned. While the claim is still known the Sandbox stays.
func TestAFailedReleaseLeavesTheSandboxToTheSweep(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(workerSpec(t))
	name := SandboxName(workerToken)
	failing := true
	g.dyn.PrependReactor("delete", "sandboxes", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		if failing {
			return true, nil, errors.New("etcdserver: request timed out")
		}
		return false, nil, nil
	})
	if err := g.r.Release(g.ctx, runtime.Known{Claim: workerToken, Locator: &loc}); err == nil || !strings.Contains(err.Error(), "request timed out") {
		t.Fatalf("Release: %v, want the delete's failure", err)
	}
	if recorded, ok := g.r.recorded(workerToken); !ok || recorded != loc {
		t.Fatalf("the watch holds %+v after the failed release, want %+v", recorded, loc)
	}
	failing = false
	if err := g.r.ReconcileOrphans(g.ctx, []runtime.Known{{Claim: workerToken}}, 0); err != nil {
		t.Fatal(err)
	}
	if g.sandbox(name) == nil {
		t.Fatal("the sweep deleted the sandbox of a claim still known")
	}
	if err := g.r.ReconcileOrphans(g.ctx, nil, 0); err != nil {
		t.Fatal(err)
	}
	if g.sandbox(name) != nil {
		t.Fatal("the retired claim's sandbox outlived the sweep")
	}
}

// The sweep deletes the project's Sandboxes that belong to no known claim, only past the grace;
// a known claim's Sandbox survives with or without a locator — a suspended claim holds its
// session there, and a root the tree volume (N3).
func TestTheOrphanSweepDeletesOnlyUnknownSandboxesPastTheGrace(t *testing.T) {
	orphan := claim.Token("legion-legion-legion-9-planner")
	g := newRig(t, []k8sruntime.Object{
		sandboxObject(t, SandboxName(orphan), "uid-sandbox-orphan", modeSuspended, claimLabels(claim.RoleTester)),
		sandboxObject(t, SandboxName(rootToken), "uid-sandbox-root", modeSuspended, claimLabels(claim.RoleArchitect)),
	})
	known := []runtime.Known{{Claim: rootToken}}
	created := g.sandbox(SandboxName(orphan)).CreationTimestamp.Time
	g.now.Store(new(created.Add(time.Minute)))
	if err := g.r.ReconcileOrphans(g.ctx, known, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	if g.sandbox(SandboxName(orphan)) == nil {
		t.Fatal("an orphan inside the grace was deleted")
	}
	g.now.Store(new(created.Add(3 * time.Minute)))
	if err := g.r.ReconcileOrphans(g.ctx, known, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	if g.sandbox(SandboxName(orphan)) != nil {
		t.Fatal("an orphan past the grace survived")
	}
	if g.sandbox(SandboxName(rootToken)) == nil {
		t.Fatal("the suspended root's sandbox, and with it the tree volume, was deleted")
	}
}

// Adoption goes over the claim's connection, to the recorded process only.
func TestAdoptWorkingCopyAsksTheRecordedProcess(t *testing.T) {
	g := newRig(t, nil)
	conn := fake.NewConn()
	g.conns.Register(workerToken, conn)
	loc := g.spawn(workerSpec(t))
	id := runtime.GitIdentity{Name: "legion-implement[bot]", Email: "1+legion-implement[bot]@users.noreply.github.com"}
	if err := g.r.AdoptWorkingCopy(g.ctx, loc, id); err != nil {
		t.Fatal(err)
	}
	if adoptions := conn.Adoptions(); len(adoptions) != 1 || adoptions[0].Identity != id {
		t.Fatalf("adoptions %+v", adoptions)
	}
	if err := g.r.AdoptWorkingCopy(g.ctx, sandboxLocator(workerToken, "uid-pod-stale"), id); err == nil {
		t.Fatal("a stale locator's adoption was asked")
	}
}
