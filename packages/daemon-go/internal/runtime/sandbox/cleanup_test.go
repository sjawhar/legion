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

	"github.com/jackc/pgx/v5/pgxpool"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/store"
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
	st                         *store.Store
	root, child, tree          string
	rootPVC                    string
	pvcDeletesObserved         []store.IssueResources
	sandboxDeletes, pvcDeletes map[string]metav1.DeleteOptions
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
	objects := []k8sruntime.Object{
		rootSandbox,
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: c.rootPVC, Namespace: testNamespace, UID: "uid-pvc-root", ResourceVersion: "pvc-rv-root"}},
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
	if _, err := st.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, linger_until, last_dispatch_seq)
		values ($1, $1, $2, 'root', 'done', 1, 'done', 'A', now() + interval '1 hour', 1)`, testTree, testProject); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureIssueResources(ctx, testProject, testTree, testTree, c.root); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureIssueResources(ctx, testProject, childIssue, testTree, c.child); err != nil {
		t.Fatal(err)
	}
	// What the root's record says at the moment its PVC delete is requested.
	c.kube.PrependReactor("delete", "persistentvolumeclaims", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		root, _, _ := st.IssueResources(context.Background(), testProject, testTree)
		c.pvcDeletesObserved = append(c.pvcDeletesObserved, root)
		return false, nil, nil
	})
	c.dyn.PrependReactor("delete", "sandboxes", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		delete := action.(k8stesting.DeleteAction)
		c.sandboxDeletes[delete.GetName()] = delete.GetDeleteOptions()
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
	child, began, err := c.st.BeginIssueCleanup(ctx, testProject, childIssue, testTree, 1)
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
	err := c.r.CleanupIssue(c.ctx, testProject, testTree, testTree, 1)
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
	err := c.r.CleanupIssue(c.ctx, testProject, testTree, testTree, 1)
	if err == nil || !strings.Contains(err.Error(), c.child) {
		t.Fatalf("root cleanup = %v, want a refusal naming the child Sandbox %s", err, c.child)
	}
	c.rootIntact(t)
	if err := c.st.EnsureIssueResources(c.ctx, testProject, "LEGION-210", testTree, "legion-legion-legion-210"); !errors.Is(err, store.ErrIssueCleanupInProgress) {
		t.Fatalf("a new child admitted during the root's cleanup: %v", err)
	}
}

// Children first, root last: the child's Sandbox goes, then the root Sandbox that owns the tree
// PVC, then the PVC, which is requested only once the root Sandbox is gone and before the root's
// cleanup is confirmed; confirmation follows both.
func TestARootIsCleanedAfterItsChildrenSandboxThenPVC(t *testing.T) {
	c := newCleanupRig(t, true)
	if err := c.r.CleanupIssue(c.ctx, testProject, childIssue, testTree, 1); err != nil {
		t.Fatalf("child cleanup: %v", err)
	}
	if err := c.r.CleanupIssue(c.ctx, testProject, testTree, testTree, 1); err != nil {
		t.Fatalf("root cleanup: %v", err)
	}
	want := []string{"sandboxes " + c.child, "sandboxes " + c.root, "persistentvolumeclaims " + c.rootPVC}
	if got := c.deletes(); strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Fatalf("deletes = %v, want %v", got, want)
	}
	if len(c.pvcDeletesObserved) != 1 || !c.pvcDeletesObserved[0].CleanupStarted || !c.pvcDeletesObserved[0].CleanupConfirmedAt.IsZero() {
		t.Fatalf("the root's record at its PVC delete = %+v, want begun and unconfirmed", c.pvcDeletesObserved)
	}
	if c.sandbox(c.root) != nil {
		t.Fatal("the root Sandbox survived its cleanup")
	}
	if root := c.rootRecord(t); root.CleanupConfirmedAt.IsZero() {
		t.Fatalf("root record after cleanup = %+v, want confirmed", root)
	}
}

// Deletion can race an operator replacement of a same-named object. Every deletion carries both
// object identity fields Kubernetes checks: a stale cleaner then gets a conflict instead of
// deleting a replacement Sandbox or root PVC.
func TestCleanupDeletesCarryUIDAndResourceVersionPreconditions(t *testing.T) {
	c := newCleanupRig(t, true)
	if err := c.r.CleanupIssue(c.ctx, testProject, childIssue, testTree, 1); err != nil {
		t.Fatalf("child cleanup: %v", err)
	}
	if err := c.r.CleanupIssue(c.ctx, testProject, testTree, testTree, 1); err != nil {
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
	got, found := c.pvcDeletes[c.rootPVC]
	if !found || got.Preconditions == nil || got.Preconditions.UID == nil || got.Preconditions.ResourceVersion == nil ||
		string(*got.Preconditions.UID) != "uid-pvc-root" || *got.Preconditions.ResourceVersion != "pvc-rv-root" {
		t.Fatalf("PVC %s delete options = %+v, want UID uid-pvc-root and resourceVersion pvc-rv-root", c.rootPVC, got)
	}
}
