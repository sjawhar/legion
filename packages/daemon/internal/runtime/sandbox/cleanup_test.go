package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

const childIssue = "LEGION-209"

// resourceStore is a migrated store on a database of its own.
func resourceStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LEGION_TEST_PG_DSN is unset, so there is no Postgres to hold the issue resources the cleanup is fenced by")
	}
	ctx := context.Background()
	base, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse LEGION_TEST_PG_DSN: %v", err)
	}
	adminURL := *base
	adminURL.Path = "/postgres"
	admin, err := pgxpool.New(ctx, adminURL.String())
	if err != nil {
		t.Fatalf("connect to the admin database: %v", err)
	}
	t.Cleanup(admin.Close)
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	name := "legion_sandbox_test_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(ctx, "create database "+name); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop database "+name+" with (force)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	testURL := *base
	testURL.Path = "/" + name
	st, err := store.Open(ctx, testURL.String())
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

// cleanupRig is a tree of two issues, LEGION-208 and its child LEGION-209, recorded in a real
// store: the root's Sandbox and tree PVC exist; the child's Sandbox exists when childLive.
type cleanupRig struct {
	*rig
	st                *store.Store
	root, child, tree string
	rootPVC           string
	sandboxDeletes    map[string]metav1.DeleteOptions
	pvcDeletes        map[string]metav1.DeleteOptions
}

func newCleanupRig(t *testing.T, childLive bool) *cleanupRig {
	t.Helper()
	st := resourceStore(t)
	c := &cleanupRig{
		st: st, root: SandboxName(rootToken), child: SandboxName(childToken), tree: testTree, rootPVC: TreeClaimName(rootToken),
		sandboxDeletes: map[string]metav1.DeleteOptions{}, pvcDeletes: map[string]metav1.DeleteOptions{},
	}
	rootSandbox := sandboxObject(t, c.root, "uid-sandbox-root", modeRunning, claimLabels(rootSpec(t).Role))
	rootSandbox.SetResourceVersion("sandbox-rv-root")
	rootOwner := metav1.OwnerReference{
		APIVersion: sandboxGVR.GroupVersion().String(), Kind: "Sandbox", Name: c.root, UID: "uid-sandbox-root",
		Controller: new(true), BlockOwnerDeletion: new(true),
	}
	objects := []k8sruntime.Object{
		rootSandbox,
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: c.rootPVC, Namespace: testNamespace, UID: "uid-pvc-root", ResourceVersion: "pvc-rv-root", OwnerReferences: []metav1.OwnerReference{rootOwner},
		}},
	}
	if childLive {
		childSandbox := sandboxObject(t, c.child, "uid-sandbox-child", modeRunning,
			map[string]string{labelProject: testProject, labelTree: testTree, labelIssue: childIssue})
		childSandbox.SetResourceVersion("sandbox-rv-child")
		objects = append(objects, childSandbox)
	}
	c.rig = newRig(t, objects, withoutController())
	c.r.SetIssueResourceStore(st)
	ctx := context.Background()
	if _, err := st.OpenTreeLifecycle(ctx, testProject, testTree, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, linger_until, last_dispatch_seq)
		values ($1, $1, upper($2), 'root', 'done', 1, 'done', 'A', now() + interval '1 hour', 1)`, testTree, testProject); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureIssueResources(ctx, testProject, testTree, testTree, c.root, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureIssueResources(ctx, testProject, childIssue, testTree, c.child, 1); err != nil {
		t.Fatal(err)
	}
	c.dyn.PrependReactor("delete", "sandboxes", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		delete := action.(k8stesting.DeleteAction)
		options := delete.GetDeleteOptions()
		c.sandboxDeletes[delete.GetName()] = options
		if delete.GetName() == c.root && options.PropagationPolicy != nil && *options.PropagationPolicy == metav1.DeletePropagationForeground {
			object, err := c.kube.Tracker().Get(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), testNamespace, c.rootPVC)
			if err != nil {
				return true, nil, err
			}
			pvc := object.(*corev1.PersistentVolumeClaim)
			if len(pvc.OwnerReferences) != 1 || pvc.OwnerReferences[0].UID != "uid-sandbox-root" ||
				pvc.OwnerReferences[0].Controller == nil || !*pvc.OwnerReferences[0].Controller ||
				pvc.OwnerReferences[0].BlockOwnerDeletion == nil || !*pvc.OwnerReferences[0].BlockOwnerDeletion {
				return true, nil, errors.New("root PVC is not a blocking Sandbox owner-dependent")
			}
			if err := c.kube.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), testNamespace, c.rootPVC); err != nil {
				return true, nil, err
			}
		}
		return false, nil, nil
	})
	c.kube.PrependReactor("delete", "persistentvolumeclaims", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		delete := action.(k8stesting.DeleteAction)
		c.pvcDeletes[delete.GetName()] = delete.GetDeleteOptions()
		return false, nil, nil
	})
	c.clearActions()
	return c
}

// confirmChildRecord cleans the child's record without touching the cluster.
func (c *cleanupRig) confirmChildRecord(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	epoch, reserved, err := c.r.ReserveWorkflowTreeCleanup(ctx, testProject, testTree, 1)
	if err != nil || !reserved {
		t.Fatalf("reserve the child cleanup: epoch %d, reserved %t, err %v", epoch, reserved, err)
	}
	child, began, err := c.st.BeginIssueCleanup(ctx, testProject, childIssue, testTree, epoch)
	if err != nil || !began {
		t.Fatalf("begin the child's cleanup: began %t, err %v", began, err)
	}
	if err := c.st.ConfirmIssueCleanup(ctx, testProject, childIssue, child.CleanupGeneration); err != nil {
		t.Fatal(err)
	}
}

func (c *cleanupRig) deletes() []string {
	var out []string
	for _, a := range c.writes() {
		if a.verb == "delete" {
			out = append(out, a.resource+" "+a.name)
		}
	}
	return out
}

func (c *cleanupRig) rootRecord(t *testing.T) store.IssueResources {
	t.Helper()
	root, found, err := c.st.IssueResources(context.Background(), testProject, testTree)
	if err != nil || !found {
		t.Fatalf("root record: found %t, err %v", found, err)
	}
	return root
}

func (c *cleanupRig) cleanupWorkflow(ctx context.Context, issue string) error {
	epoch, reserved, err := c.r.ReserveWorkflowTreeCleanup(ctx, testProject, testTree, 1)
	if err != nil {
		return err
	}
	if !reserved {
		return nil
	}
	return c.r.CleanupIssue(ctx, testProject, issue, testTree, epoch)
}

func (c *cleanupRig) rootIntact(t *testing.T) {
	t.Helper()
	if deletes := c.deletes(); len(deletes) != 0 {
		t.Fatalf("the root cleanup requested %v while a child of its tree remained", deletes)
	}
	if c.sandbox(c.root) == nil {
		t.Fatal("the root Sandbox is gone")
	}
	if _, err := c.kube.CoreV1().PersistentVolumeClaims(testNamespace).Get(context.Background(), c.rootPVC, metav1.GetOptions{}); err != nil {
		t.Fatalf("the tree PVC: %v", err)
	}
	if !c.rootRecord(t).CleanupConfirmedAt.IsZero() {
		t.Fatal("the root's cleanup was confirmed")
	}
}

// A child whose record is unconfirmed holds the root: no root Sandbox or PVC delete is requested,
// and the root's record is not taken, so a re-admitted root still launches.
func TestARootCleanupDeletesNothingWhileAChildRecordIsUnconfirmed(t *testing.T) {
	c := newCleanupRig(t, false)
	err := c.cleanupWorkflow(c.ctx, testTree)
	if !errors.Is(err, store.ErrTreeChildrenPending) {
		t.Fatalf("root cleanup = %v, want ErrTreeChildrenPending", err)
	}
	c.rootIntact(t)
	if c.rootRecord(t).CleanupStarted {
		t.Fatal("a refused root cleanup took the root's record")
	}
}

// A child Sandbox the API still lists holds the root even when every child record is confirmed:
// no delete is requested, and the root's begun cleanup keeps refusing new children until it ends.
func TestARootCleanupDeletesNothingWhileTheAPIListsAChildSandbox(t *testing.T) {
	c := newCleanupRig(t, true)
	c.confirmChildRecord(t)
	err := c.cleanupWorkflow(c.ctx, testTree)
	if err == nil || !strings.Contains(err.Error(), c.child) {
		t.Fatalf("root cleanup = %v, want a refusal naming the child Sandbox %s", err, c.child)
	}
	c.rootIntact(t)
	if err := c.st.EnsureIssueResources(c.ctx, testProject, "LEGION-210", testTree, "legion-legion-legion-210", 1); !errors.Is(err, store.ErrIssueCleanupInProgress) {
		t.Fatalf("a new child admitted during the root's cleanup: %v", err)
	}
}

// Children first, root last: the child's Sandbox goes before the root Sandbox. The root request
// uses foreground propagation, so its blocking PVC dependent is removed by garbage collection
// before the root is absent and confirmation admits a later run; the restricted runtime never
// makes a PVC API call itself.
func TestARootIsCleanedAfterItsChildrenByForegroundGarbageCollection(t *testing.T) {
	c := newCleanupRig(t, true)
	if err := c.cleanupWorkflow(c.ctx, childIssue); err != nil {
		t.Fatalf("child cleanup: %v", err)
	}
	if err := c.cleanupWorkflow(c.ctx, testTree); err != nil {
		t.Fatalf("root cleanup: %v", err)
	}
	want := []string{"sandboxes " + c.child, "sandboxes " + c.root}
	if got := c.deletes(); strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Fatalf("deletes = %v, want %v", got, want)
	}
	if len(c.pvcDeletes) != 0 {
		t.Fatalf("the restricted runtime sent PVC delete requests: %+v", c.pvcDeletes)
	}
	if c.sandbox(c.root) != nil {
		t.Fatal("the root Sandbox survived its cleanup")
	}
	if _, err := c.kube.CoreV1().PersistentVolumeClaims(testNamespace).Get(context.Background(), c.rootPVC, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the root PVC after foreground cleanup: %v, want NotFound", err)
	}
	if root := c.rootRecord(t); root.CleanupConfirmedAt.IsZero() {
		t.Fatalf("root record after cleanup = %+v, want confirmed", root)
	}
}

// A stale cleanup must not delete a replacement Sandbox. Both child and root deletes carry UID and
// resourceVersion preconditions, while root deletion explicitly waits for the blocking PVC owner
// dependency through foreground garbage collection rather than using a PVC grant.
func TestCleanupSandboxDeletesCarryIdentityAndRootIsForeground(t *testing.T) {
	c := newCleanupRig(t, true)
	if err := c.cleanupWorkflow(c.ctx, childIssue); err != nil {
		t.Fatalf("child cleanup: %v", err)
	}
	if err := c.cleanupWorkflow(c.ctx, testTree); err != nil {
		t.Fatalf("root cleanup: %v", err)
	}
	for name, want := range map[string]struct{ uid, version string }{
		c.child: {"uid-sandbox-child", "sandbox-rv-child"},
		c.root:  {"uid-sandbox-root", "sandbox-rv-root"},
	} {
		got, found := c.sandboxDeletes[name]
		if !found || got.Preconditions == nil || got.Preconditions.UID == nil || got.Preconditions.ResourceVersion == nil ||
			string(*got.Preconditions.UID) != want.uid || *got.Preconditions.ResourceVersion != want.version {
			t.Fatalf("Sandbox %s delete options = %+v, want UID %s and resourceVersion %s", name, got, want.uid, want.version)
		}
	}
	root := c.sandboxDeletes[c.root]
	if root.PropagationPolicy == nil || *root.PropagationPolicy != metav1.DeletePropagationForeground {
		t.Fatalf("root Sandbox delete propagation = %+v, want foreground", root.PropagationPolicy)
	}
	if len(c.pvcDeletes) != 0 {
		t.Fatalf("the restricted runtime sent PVC delete requests: %+v", c.pvcDeletes)
	}
}

// Operator-created trees have no workflow `issues` row. Their explicit close authority must still
// run the same durable child-first/root-last cleanup rather than manufacturing a workflow record.
func TestDirectTreeCleanupRequiresNoWorkflowRecord(t *testing.T) {
	c := newCleanupRig(t, true)
	if _, err := c.st.Pool().Exec(c.ctx, `delete from issues where key = $1`, testTree); err != nil {
		t.Fatal(err)
	}
	if _, err := c.st.Pool().Exec(c.ctx, `update tree_lifecycles set authority = 'operator' where project = $1 and tree = $2`, testProject, testTree); err != nil {
		t.Fatal(err)
	}
	epoch, reserved, err := c.r.ReserveOperatorTreeCleanup(c.ctx, testProject, testTree)
	if err != nil || !reserved {
		t.Fatalf("reserve operator cleanup: reserved %t, err %v", reserved, err)
	}
	if err := c.r.CleanupTree(c.ctx, testProject, testTree, epoch); err != nil {
		t.Fatalf("direct tree cleanup: %v", err)
	}
	if got, want := c.deletes(), []string{"sandboxes " + c.child, "sandboxes " + c.root}; strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Fatalf("direct cleanup deletes = %v, want children before root %v", got, want)
	}
	if root := c.rootRecord(t); root.CleanupConfirmedAt.IsZero() {
		t.Fatalf("direct root cleanup record = %+v, want confirmed", root)
	}
}

// A transient foreground root delete holds the durable marker and the next admission. Retrying the
// same close resumes that marker, then foreground garbage collection removes the blocking PVC and
// confirmation admits the next resource epoch; no failure clears the fence.
func TestForegroundRootDeleteFailureKeepsTheAdmissionFenceUntilRetryConfirms(t *testing.T) {
	c := newCleanupRig(t, true)
	if err := c.cleanupWorkflow(c.ctx, childIssue); err != nil {
		t.Fatalf("child cleanup: %v", err)
	}
	failed := true
	c.dyn.PrependReactor("delete", "sandboxes", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		delete := action.(k8stesting.DeleteAction)
		if delete.GetName() == c.root && failed {
			failed = false
			return true, nil, errors.New("transient Sandbox API failure")
		}
		return false, nil, nil
	})
	if err := c.cleanupWorkflow(c.ctx, testTree); err == nil || !strings.Contains(err.Error(), "transient Sandbox API failure") {
		t.Fatalf("first root cleanup = %v, want its transient delete failure", err)
	}
	if root := c.rootRecord(t); !root.CleanupStarted || !root.CleanupConfirmedAt.IsZero() {
		t.Fatalf("root after transient delete = %+v, want begun and unconfirmed", root)
	}
	if err := c.st.EnsureIssueResources(c.ctx, testProject, testTree, testTree, c.root, 1); !errors.Is(err, store.ErrIssueCleanupInProgress) {
		t.Fatalf("new root admission during foreground cleanup retry = %v, want ErrIssueCleanupInProgress", err)
	}
	if err := c.cleanupWorkflow(c.ctx, testTree); err != nil {
		t.Fatalf("root cleanup retry: %v", err)
	}
	if root := c.rootRecord(t); root.CleanupConfirmedAt.IsZero() {
		t.Fatalf("root after retry = %+v, want confirmed", root)
	}
	if _, err := c.st.OpenTreeLifecycle(c.ctx, testProject, testTree, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatal(err)
	}
	if err := c.st.EnsureIssueResources(c.ctx, testProject, testTree, testTree, c.root, 2); err != nil {
		t.Fatalf("new root admission after confirmation: %v", err)
	}
}

// A child claim that won its admission before the reservation but has not reached resource
// admission has no resource row, so a resource-row census misses it. The tree cleanup censuses the
// stored claims after the reservation: it deletes nothing while that child is unretired, and runs
// child-first, root-last once it retired.
func TestATreeCleanupWaitsForAPreResourceChildClaim(t *testing.T) {
	c := newCleanupRig(t, true)
	pre := supervise.Claim{
		Token: claim.Token("legion-legion-legion-210-tester"), Project: testProject, Tree: testTree, Issue: "LEGION-210",
		Role: claim.RoleTester, State: supervise.StateLaunching,
	}
	pre, err := c.st.AdmitClaim(c.ctx, pre)
	if err != nil {
		t.Fatalf("admit the child before the reservation: %v", err)
	}
	epoch, reserved, err := c.r.ReserveWorkflowTreeCleanup(c.ctx, testProject, testTree, 1)
	if err != nil || !reserved {
		t.Fatalf("reserve: epoch %d, reserved %t, err %v", epoch, reserved, err)
	}
	if err := c.r.CleanupTree(c.ctx, testProject, testTree, epoch); !errors.Is(err, store.ErrIssueCleanupInProgress) || !strings.Contains(err.Error(), string(pre.Token)) {
		t.Fatalf("cleanup with a pre-resource child = %v, want the named wait naming %s", err, pre.Token)
	}
	if deletes := c.deletes(); len(deletes) != 0 {
		t.Fatalf("the cleanup deleted %v while a child claim of the tree was unretired", deletes)
	}
	pre.State = supervise.StateRetired
	if err := c.st.PutClaim(c.ctx, pre); err != nil {
		t.Fatal(err)
	}
	if err := c.r.CleanupTree(c.ctx, testProject, testTree, epoch); err != nil {
		t.Fatalf("cleanup once the child retired: %v", err)
	}
	want := []string{"sandboxes " + c.child, "sandboxes " + c.root}
	if got := c.deletes(); strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Fatalf("deletes = %v, want %v", got, want)
	}
}

// A close that reserved before the tree's first resource record deletes nothing and confirms the
// reservation, so the next explicit admission opens a new epoch instead of waiting forever.
func TestATreeCleanupBeforeAnyResourceConfirmsItsReservation(t *testing.T) {
	st := resourceStore(t)
	g := newRig(t, nil, withoutController())
	g.r.SetIssueResourceStore(st)
	ctx := context.Background()
	if _, err := st.OpenTreeLifecycle(ctx, testProject, testTree, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, linger_until, last_dispatch_seq)
		values ($1, $1, upper($2), 'root', 'done', 1, 'done', 'A', now() + interval '1 hour', 1)`, testTree, testProject); err != nil {
		t.Fatal(err)
	}
	epoch, reserved, err := g.r.ReserveWorkflowTreeCleanup(ctx, testProject, testTree, 1)
	if err != nil || !reserved {
		t.Fatalf("reserve: epoch %d, reserved %t, err %v", epoch, reserved, err)
	}
	g.clearActions()
	if err := g.r.CleanupTree(ctx, testProject, testTree, epoch); err != nil {
		t.Fatalf("cleanup of a tree with no resources: %v", err)
	}
	if writes := g.writes(); len(writes) != 0 {
		t.Fatalf("a tree with no resources wrote %+v", writes)
	}
	opened, err := st.OpenTreeLifecycle(ctx, testProject, testTree, treelifecycle.AuthorityWorkflow)
	if err != nil || opened.Epoch != epoch+1 {
		t.Fatalf("next admission = %+v, err %v; want epoch %d", opened, err, epoch+1)
	}
}

// A launch whose tree's cleanup reserved its epoch is refused at the resource recheck at once,
// before any pod or Secret is touched, with the reservation's sentinel: waiting out the boot
// deadline would hold the claim's machine while the close needs it to stop the claim.
func TestALaunchIntoAReservedTreeIsRefusedAtOnceAndTouchesNothing(t *testing.T) {
	c := newCleanupRig(t, false)
	if _, reserved, err := c.r.ReserveWorkflowTreeCleanup(c.ctx, testProject, testTree, 1); err != nil || !reserved {
		t.Fatalf("reserve: reserved %t, err %v", reserved, err)
	}
	spec := rootSpec(t)
	spec.TreeEpoch = 1
	started := time.Now()
	_, err := c.r.Spawn(c.ctx, spec)
	if !errors.Is(err, treelifecycle.ErrCleanupReserved) {
		t.Fatalf("launch into a reserved tree = %v, want the reservation's refusal", err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("the refusal took %s; it must not wait toward the %s boot deadline", elapsed, 2*time.Second)
	}
	if writes := c.writes(); len(writes) != 0 {
		t.Fatalf("a refused launch wrote %+v", writes)
	}
}
