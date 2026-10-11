package sandbox

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// Suspend of a role's current process stops that child through its launcher and nothing else: the
// issue Sandbox stays Running, so every sibling role in the pod keeps running, and the role's
// Secret and session stay for a Resume.
func TestSuspendStopsOnlyTheRecordedRoleProcess(t *testing.T) {
	g := newRig(t, nil)
	root := g.spawn(rootSpec(t))
	loc := g.spawn(workerSpec(t))
	g.clearActions()
	if err := g.r.Suspend(g.ctx, loc); err != nil {
		t.Fatal(err)
	}
	name := SandboxName(workerToken)
	stops := 0
	for _, frame := range g.sent(workerToken) {
		if stop, ok := frame.(shimwire.LauncherStop); ok {
			stops++
			if stop.Generation != loc.Sandbox.Generation {
				t.Fatalf("stop of generation %d, want the recorded %d", stop.Generation, loc.Sandbox.Generation)
			}
		}
	}
	if stops != 1 {
		t.Fatalf("%d stops sent to the tester's launcher, want 1", stops)
	}
	for _, frame := range g.sent(rootToken) {
		if _, ok := frame.(shimwire.LauncherStop); ok {
			t.Fatal("the architect's launcher was sent a stop for the tester's suspension")
		}
	}
	expectSteps(t, steps(t, g.writes(), name))
	if g.sandbox(name).mode() != modeRunning || g.secret(roleSecretName(name, claim.RoleTester)) == nil {
		t.Fatal("suspend changed the issue pod or removed what a resume needs")
	}
	if obs, err := g.r.Probe(g.ctx, root); err != nil || obs.Kind != runtime.Alive {
		t.Fatalf("the architect after the tester's suspension: %s, %v", obs.Kind, err)
	}
}

// A locator that is no longer the role's current process is already stopped (decision 3d): a
// newer generation of the same role in the same pod, a pod the controller replaced, or no pod.
// Nothing is written and no stop is sent.
func TestSuspendWithAStaleLocator(t *testing.T) {
	name := SandboxName(workerToken)
	stopsOf := func(g *rig) int {
		n := 0
		for _, frame := range g.sent(workerToken) {
			if _, ok := frame.(shimwire.LauncherStop); ok {
				n++
			}
		}
		return n
	}
	t.Run("a newer generation of the role is left alone", func(t *testing.T) {
		g := newRig(t, nil)
		old := g.spawn(workerSpec(t))
		if err := g.r.Suspend(g.ctx, old); err != nil {
			t.Fatal(err)
		}
		spec := workerSpec(t)
		spec.Generation = old.Sandbox.Generation + 1
		newer := g.spawn(spec)
		g.clearActions()
		stops := stopsOf(g)
		if err := g.r.Suspend(g.ctx, old); err != nil {
			t.Fatalf("a stale locator is stopped, not an error: %v", err)
		}
		if writes := g.writes(); len(writes) > 0 || stopsOf(g) != stops {
			t.Fatalf("a stale Suspend wrote %v and sent %d stops", writes, stopsOf(g)-stops)
		}
		if newer.Sandbox.PodUID != old.Sandbox.PodUID {
			t.Fatalf("the newer generation ran in pod %s, want the same issue pod %s", newer.Sandbox.PodUID, old.Sandbox.PodUID)
		}
		if obs, err := g.r.Probe(g.ctx, newer); err != nil || obs.Kind != runtime.Alive {
			t.Fatalf("the newer generation after the stale Suspend: %s, %v", obs.Kind, err)
		}
	})
	t.Run("a pod the controller recreated is not the recorded process", func(t *testing.T) {
		g := newRig(t, nil)
		loc := g.spawn(workerSpec(t))
		if err := g.kube.Tracker().Delete(podsGVR, testNamespace, name); err != nil {
			t.Fatal(err)
		}
		g.eventually("the store to see the recreated pod", func() bool {
			pod := g.r.storedPod(name)
			return pod != nil && string(pod.UID) != loc.Sandbox.PodUID
		})
		g.clearActions()
		if err := g.r.Suspend(g.ctx, loc); err != nil {
			t.Fatal(err)
		}
		if writes := g.writes(); len(writes) > 0 || stopsOf(g) != 0 {
			t.Fatalf("Suspend of the replaced pod's process wrote %v and sent %d stops", writes, stopsOf(g))
		}
	})
	t.Run("no pod", func(t *testing.T) {
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
		if writes := g.writes(); len(writes) > 0 || stopsOf(g) != 0 {
			t.Fatalf("Suspend with no pod wrote %v and sent %d stops", writes, stopsOf(g))
		}
	})
}

// Release is claim-scoped. It stops at most the role process and MUST NOT delete, suspend or
// otherwise change its issue Sandbox, even when this runtime sees no sibling role: the tree's
// reserved cleanup (store.CleanupReservedTree, then CleanupTree) alone deletes issue Sandboxes.
func TestReleaseNeverDeletesTheIssueSandbox(t *testing.T) {
	name := SandboxName(workerToken)
	for label, locate := range map[string]func(runtime.Locator) *runtime.Locator{
		"current": func(loc runtime.Locator) *runtime.Locator { return &loc },
		"stale": func(runtime.Locator) *runtime.Locator {
			stale := sandboxLocator(workerToken, "uid-pod-long-gone")
			return &stale
		},
		"no locator": func(runtime.Locator) *runtime.Locator { return nil },
	} {
		t.Run(label, func(t *testing.T) {
			g := newRig(t, nil)
			g.issueLaunchers(workerToken)
			loc := g.spawn(workerSpec(t))
			if label != "current" {
				if err := g.r.Suspend(g.ctx, loc); err != nil {
					t.Fatal(err)
				}
			}
			g.clearActions()
			if err := g.r.Release(g.ctx, runtime.Known{Claim: workerToken, Locator: locate(loc)}); err != nil {
				t.Fatal(err)
			}
			if g.sandbox(name) == nil || g.pod(name) == nil || g.sandbox(name).mode() != modeRunning {
				t.Fatal("claim release changed the issue pod; only the durable issue-resource cleanup may do that")
			}
			for _, write := range g.writes() {
				if write.resource == "sandboxes" {
					t.Fatalf("claim release wrote the issue Sandbox: %+v", write)
				}
			}
			if err := g.r.Release(g.ctx, runtime.Known{Claim: workerToken}); err != nil {
				t.Fatalf("releasing an already released claim: %v", err)
			}
		})
	}
}

// Release takes the claim once. A claim paired with another claim's locator is refused before
// anything is sent or deleted: releasing one claim never ends another's Sandbox or process.
func TestReleaseRefusesALocatorOfAnotherClaim(t *testing.T) {
	g := newRig(t, nil)
	g.spawn(workerSpec(t))
	other := g.spawn(testSpec(t, otherToken, claim.RoleReviewer, testTree))
	g.clearActions()
	if err := g.r.Release(g.ctx, runtime.Known{Claim: workerToken, Locator: &other}); err == nil || !strings.Contains(err.Error(), string(otherToken)) {
		t.Fatalf("Release = %v, want a refusal naming the locator's claim", err)
	}
	for _, token := range []claim.Token{workerToken, otherToken} {
		for _, frame := range g.sent(token) {
			if _, ok := frame.(shimwire.LauncherStop); ok {
				t.Fatalf("the refused release sent %s's launcher a stop", token)
			}
		}
	}
	if writes := g.writes(); len(writes) > 0 {
		t.Fatalf("the refused release wrote %v", writes)
	}
	if g.sandbox(SandboxName(workerToken)) == nil {
		t.Fatal("a refused release deleted the issue sandbox")
	}
}

// The daemon reads its claims before it sweeps, and a retry after boot sweeps at a grace of 0
// while it launches claims, so a claim launched after that read is missing from known. Its tree's
// lifecycle is open, as its launch's own lifecycle check required: the sweep keeps its Sandbox,
// whether the launch has finished or is still waiting for its pod.
func TestTheOrphanSweepKeepsTheSandboxesOfALiveTreeUnknownClaimsLaunched(t *testing.T) {
	g := newRig(t, nil)
	var known []runtime.Known // read before either launch
	g.spawn(workerSpec(t))
	g.hold.Store(true)
	g.launcher(childToken)
	launching := make(chan error, 1)
	go func() {
		_, err := g.r.Spawn(g.ctx, childSpec(t))
		launching <- err
	}()
	g.eventually("the launching child issue's sandbox in the runtime's store", func() bool {
		_, ok, _ := g.r.sandboxes.GetStore().GetByKey(testNamespace + "/" + SandboxName(childToken))
		return ok
	})
	if err := g.r.ReconcileOrphans(g.ctx, known, 0); err != nil {
		t.Fatal(err)
	}
	if g.sandbox(SandboxName(workerToken)) == nil {
		t.Fatal("a sandbox launched after the claims were read was swept as an orphan")
	}
	if g.sandbox(SandboxName(childToken)) == nil {
		t.Fatal("a sandbox still launching was swept as an orphan")
	}
	g.hold.Store(false)
	if err := <-launching; err != nil {
		t.Fatalf("the child issue's launch: %v", err)
	}
}

// A launch of a claim that begins while the sweep deletes a leftover Sandbox under the claim's
// name waits for that delete, then launches into a Sandbox of its own: it never takes up the
// leftover the delete then takes from under it, which would cost the claim a launch.
func TestALaunchBegunDuringAnOrphansDeleteWaitsForIt(t *testing.T) {
	leftover := SandboxName(workerToken)
	var armed atomic.Bool
	got := make(chan struct{}, 1)
	launching := make(chan error, 1)
	var g *rig
	hooks := &sandboxHooks{
		beforeDelete: func(name string) {
			if name != leftover || !armed.CompareAndSwap(true, false) {
				return
			}
			go func() {
				_, err := g.r.Spawn(g.ctx, workerSpec(t))
				launching <- err
			}()
			// The launch reads the claim's Sandbox as soon as nothing holds it back; give it the
			// chance before this delete goes ahead.
			select {
			case <-got:
			case <-time.After(time.Second):
			}
		},
		afterGet: func(name string) {
			if name == leftover {
				select {
				case got <- struct{}{}:
				default:
				}
			}
		},
	}
	g = newRig(t, []k8sruntime.Object{
		sandboxObject(t, leftover, "uid-sandbox-leftover", modeSuspended, claimLabels(claim.RoleTester)),
	}, withSandboxHooks(hooks))
	g.launcher(workerToken)
	// The leftover's tree had its cleanup confirmed; the launch is its re-admission's.
	g.store.close(testTree)
	armed.Store(true)
	if err := g.r.ReconcileOrphans(g.ctx, nil, 0); err != nil {
		t.Fatal(err)
	}
	if armed.Load() {
		t.Fatal("the sweep never deleted the leftover")
	}
	if err := <-launching; err != nil {
		t.Fatalf("the launch begun during the leftover's delete: %v", err)
	}
	if s := g.sandbox(leftover); s == nil || s.UID == "uid-sandbox-leftover" {
		t.Fatalf("the claim's sandbox after its launch is %+v, want one of its own", s)
	}
}

// The sweep deletes the project's Sandboxes of a tree whose cleanup confirmed (or that has no
// lifecycle), only past the grace, and in the foreground, as the tree cleanup does: the issue's
// next Sandbox names the same PVC, which a background delete would leave Terminating for a
// re-admission to ask for. A Sandbox of a live tree survives whatever claims are known, with or
// without a locator: a suspended role holds its session on the issue's volume the Sandbox owns
// (N3), and a claim launched after the daemon read its claims is missing from known. The image
// probe's Sandbox is no claim's and is the probe's own to delete (#1266), and one whose labels name
// no issue and tree is kept and reported: what cannot be told apart from a live tree's is never
// deleted.
func TestTheOrphanSweepDeletesOnlySandboxesOfClosedTreesPastTheGrace(t *testing.T) {
	orphan := claim.Token("legion-legion-legion-9-planner")
	probe := "legion-probe-legion-1d10089a0000"
	unlabelled := "legion-legion-legion-77"
	g := newRig(t, []k8sruntime.Object{
		sandboxObject(t, SandboxName(orphan), "uid-sandbox-orphan", modeSuspended,
			map[string]string{labelProject: testProject, labelTree: "LEGION-9", labelIssue: "LEGION-9"}),
		sandboxObject(t, SandboxName(rootToken), "uid-sandbox-root", modeSuspended, claimLabels(claim.RoleArchitect)),
		sandboxObject(t, probe, "uid-sandbox-probe", modeRunning, map[string]string{labelProject: testProject, labelProbe: "image"}),
		sandboxObject(t, unlabelled, "uid-sandbox-unlabelled", modeSuspended, map[string]string{labelProject: testProject}),
	})
	g.store.close("LEGION-9")
	deletes := map[string]metav1.DeleteOptions{}
	g.dyn.PrependReactor("delete", "sandboxes", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		delete := action.(k8stesting.DeleteAction)
		deletes[delete.GetName()] = delete.GetDeleteOptions()
		return false, nil, nil
	})
	created := g.sandbox(SandboxName(orphan)).CreationTimestamp.Time
	g.now.Store(new(created.Add(time.Minute)))
	if err := g.r.ReconcileOrphans(g.ctx, nil, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	if g.sandbox(SandboxName(orphan)) == nil {
		t.Fatal("an orphan inside the grace was deleted")
	}
	g.now.Store(new(created.Add(3 * time.Minute)))
	if err := g.r.ReconcileOrphans(g.ctx, nil, 2*time.Minute); err == nil || !strings.Contains(err.Error(), unlabelled) {
		t.Fatalf("sweep past the grace = %v, want the unlabelled Sandbox %s reported", err, unlabelled)
	}
	if g.sandbox(SandboxName(orphan)) != nil {
		t.Fatal("an orphan of a closed tree past the grace survived")
	}
	if policy := deletes[SandboxName(orphan)].PropagationPolicy; policy == nil || *policy != metav1.DeletePropagationForeground {
		t.Fatalf("the orphan's delete propagation = %v, want foreground, so its PVC is gone before its Sandbox is", policy)
	}
	if g.sandbox(SandboxName(rootToken)) == nil {
		t.Fatal("the live tree's root Sandbox, and with it the issue's volume, was deleted though no claim of it was known")
	}
	for _, kept := range []string{probe, unlabelled} {
		if g.sandbox(kept) == nil {
			t.Fatalf("Sandbox %s was swept as an orphan", kept)
		}
	}
}

// The sweep decides on a Sandbox as the informer held it, and takes the pod's launch turn only once
// its tree looks closed. An issue of that closed tree can be re-admitted in between — a child
// re-admitted as a root of its own, whose launch keeps the suspended Sandbox, relabelled for the
// new tree with its UID unchanged (TestAnOrphanRootKeepsTheSandboxAndVolumeItsOldTreeLeft), and
// runs a pod in it before the sweep's turn comes. The delete is fenced to what the decision read,
// as the controller branch's is (TestAControllerSandboxWrittenSinceItsDeleteWasDecidedStays): the
// re-admitted issue keeps its running Sandbox, and with it the volume holding its clone, workspace
// and sessions, on this sweep and on the next, which reads the Sandbox under its live tree.
func TestAnIssueSandboxReadmittedSinceItsDeleteWasDecidedStays(t *testing.T) {
	orphan := claim.Token("legion-legion-legion-209-architect")
	name := SandboxName(orphan)
	oldTree := map[string]string{labelProject: testProject, labelTree: labelValue(testTree), labelIssue: labelValue("LEGION-209")}
	spec := testSpec(t, orphan, claim.RoleArchitect, "LEGION-209")
	spec.Tree = "LEGION-209"
	var g *rig
	var loc runtime.Locator
	store := &readmittingStore{fakeStore: newFakeStore(), tree: testTree}
	store.readmit = func() { loc = g.spawn(spec) }
	g = newRig(t, []k8sruntime.Object{sandboxObject(t, name, "uid-sandbox-old-tree", modeSuspended, oldTree)},
		withOptions(func(o *Options) { o.Store = store }))
	g.dyn.PrependReactor("patch", "sandboxes", k8stesting.ObjectReaction(&versionedTracker{ObjectTracker: generationTracker{g.dyn.Tracker()}}))
	g.dyn.PrependReactor("delete", "sandboxes", fenceSandboxDeletes(g))
	store.close(testTree)
	if err := g.r.ReconcileOrphans(g.ctx, nil, 0); err != nil {
		t.Fatal(err)
	}
	if !store.readmitted.Load() {
		t.Fatal("the sweep never read the old tree's lifecycle, so the re-admission never ran between its decision and its delete")
	}
	kept := func(when string) {
		t.Helper()
		s := g.sandbox(name)
		if s == nil || s.UID != "uid-sandbox-old-tree" || s.Labels[labelTree] != labelValue("LEGION-209") || s.mode() != modeRunning {
			t.Fatalf("the re-admitted issue's Sandbox %s = %+v; want the relabelled Sandbox uid-sandbox-old-tree kept Running under tree LEGION-209, its volume and sessions with it", when, s)
		}
		if pod := g.pod(name); pod == nil || string(pod.UID) != loc.Sandbox.PodUID {
			t.Fatalf("the re-admitted issue's pod %s = %+v; want the launch's pod %s still running", when, pod, loc.Sandbox.PodUID)
		}
	}
	kept("after the sweep that decided on the old tree's Sandbox")
	g.eventually("the runtime's store to hold the Sandbox relabelled", func() bool {
		s, _ := g.r.storedSandbox(name)
		return s != nil && s.Labels[labelTree] == labelValue("LEGION-209")
	})
	if err := g.r.ReconcileOrphans(g.ctx, nil, 0); err != nil {
		t.Fatal(err)
	}
	kept("after the next sweep")
}

// readmittingStore is the runtime's store whose first TreeLive read of tree runs readmit before it
// answers: a re-admission of one of the tree's issues that lands between the sweep's decision and
// the pod turn it takes to delete.
type readmittingStore struct {
	*fakeStore
	tree       string
	once       sync.Once
	readmit    func()
	readmitted atomic.Bool
}

func (s *readmittingStore) TreeLive(ctx context.Context, project, tree string) (bool, error) {
	if tree == s.tree {
		s.once.Do(func() {
			s.readmit()
			s.readmitted.Store(true)
		})
	}
	return s.fakeStore.TreeLive(ctx, project, tree)
}

// versionedTracker stores a Sandbox patch under a new resourceVersion, as the API server does for
// every write; the fake's tracker leaves the version alone.
type versionedTracker struct {
	k8stesting.ObjectTracker
	writes atomic.Int64
}

func (t *versionedTracker) Patch(gvr schema.GroupVersionResource, obj k8sruntime.Object, ns string, opts ...metav1.PatchOptions) error {
	if object, err := metaOf(obj); err == nil && gvr == sandboxGVR {
		object.SetResourceVersion(fmt.Sprintf("rv-%d", t.writes.Add(1)))
	}
	return t.ObjectTracker.Patch(gvr, obj, ns, opts...)
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
