package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/kvwatch/kvwatchtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// heldRoles is RolesHeldBy that fails the test when the view cannot answer.
func heldRoles(t *testing.T, reg *Registry, sessionID string) []string {
	t.Helper()
	roles, err := reg.RolesHeldBy(sessionID)
	if err != nil {
		t.Fatalf("RolesHeldBy(%s): %v", sessionID, err)
	}
	return roles
}

// The registry's view of the role claims follows the role bucket, whoever writes it: a claim
// stored before Open and one written after give their holders the role, a claim that supersedes
// one moves the role, a deleted claim leaves the role to nobody, and a claim this build cannot
// decode gives it to nobody either and is logged at WARN once. A claim this registry writes itself
// is in the view as soon as its write answers. Interest topics play no part.
func TestRolesHeldByFollowsTheRoleClaims(t *testing.T) {
	logs := captureRegistryLogs(t)
	conn, cleanup := connectNATS(t)
	defer cleanup()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	rawRoles, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.RoleClaimBucket, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("create the role bucket: %v", err)
	}
	putRoleClaim(t, rawRoles, "restored", "ses_a")

	reg, err := Open(conn, WithReplicas(1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := reg.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("WaitForCacheReady: %v", err)
	}
	// A role topic on a record says nothing about who holds the role.
	if _, err := reg.Upsert(Interest{SessionID: "ses_b", MachineID: "m1"}, []string{"notifications.role.restored"}); err != nil {
		t.Fatalf("upsert ses_b: %v", err)
	}
	if got := heldRoles(t, reg, "ses_a"); !slices.Equal(got, []string{"restored"}) {
		t.Fatalf("the holder of a claim stored before Open holds %v, want [restored]", got)
	}
	if got := heldRoles(t, reg, "ses_b"); len(got) != 0 {
		t.Fatalf("a session whose record carries a role topic but whose claim names another holds %v, want none", got)
	}

	held := func(sessionID string, want ...string) func() bool {
		return func() bool {
			got, err := reg.RolesHeldBy(sessionID)
			return err == nil && slices.Equal(got, append([]string{}, want...))
		}
	}
	putRoleClaim(t, rawRoles, "reviewer", "ses_a")
	waitFor(t, 5*time.Second, held("ses_a", "restored", "reviewer"))

	putRoleClaim(t, rawRoles, "reviewer", "ses_b")
	waitFor(t, 5*time.Second, held("ses_b", "reviewer"))
	waitFor(t, 5*time.Second, held("ses_a", "restored"))

	if err := rawRoles.Delete("reviewer"); err != nil {
		t.Fatalf("delete reviewer's claim: %v", err)
	}
	waitFor(t, 5*time.Second, held("ses_b"))

	if _, err := rawRoles.Put("restored", []byte(`{"holder_session_id":""}`)); err != nil {
		t.Fatalf("store a claim naming no holder: %v", err)
	}
	waitFor(t, 5*time.Second, held("ses_a"))
	waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(logs.String(), "role claim watcher found a claim it cannot read")
	})
	if n := strings.Count(logs.String(), "role claim watcher found a claim it cannot read"); n != 1 {
		t.Fatalf("one undecodable claim logged %d times, want once:\n%s", n, logs.String())
	}

	if _, err := reg.SetRole("ses_c", "m1", "planner", false); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	if got := heldRoles(t, reg, "ses_c"); !slices.Equal(got, []string{"planner"}) {
		t.Fatalf("a claim this registry wrote is not in its view as the write answers: %v", got)
	}
	if err := reg.Remove(context.Background(), "ses_c", []string{"notifications.role.planner"}); err != nil {
		t.Fatalf("release planner: %v", err)
	}
	if got := heldRoles(t, reg, "ses_c"); len(got) != 0 {
		t.Fatalf("a claim this registry released is still in its view: %v", got)
	}
}

// A view that cannot answer says so rather than answering that a session holds nothing: the role
// claim watcher has ended (here its bucket went away) and nothing has replaced it, or no watcher
// follows the claims at all.
func TestRolesHeldByRefusesWhenItsViewCannotAnswer(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	reg, err := Open(conn, WithReplicas(1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := reg.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("WaitForCacheReady: %v", err)
	}
	if _, err := reg.RolesHeldBy("ses_a"); err != nil {
		t.Fatalf("a ready view refused: %v", err)
	}
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue(bus.RoleClaimBucket); err != nil {
		t.Fatalf("delete the role bucket: %v", err)
	}
	waitFor(t, 30*time.Second, func() bool {
		_, err := reg.RolesHeldBy("ses_a")
		return errors.Is(err, ErrRoleViewNotReady)
	})
	if reg.WatchErr() == nil {
		t.Fatal("the registry reports no watcher error while its role claim view cannot answer")
	}

	if _, err := (&Registry{}).RolesHeldBy("ses_a"); !errors.Is(err, ErrRoleViewNotReady) {
		t.Fatalf("a registry no watcher feeds answered RolesHeldBy with %v, want ErrRoleViewNotReady", err)
	}
}

// heldOnceComplete asks RolesHeldBy(sessionID) until it answers want, and fails the test the first
// time it answers anything else: a view that is not complete refuses (ErrRoleViewNotReady), and
// never reports the session without its roles. It returns how many refusals came first.
func heldOnceComplete(t *testing.T, reg *Registry, sessionID string, want []string) int {
	t.Helper()
	refusals := 0
	deadline := time.Now().Add(30 * time.Second)
	for {
		roles, err := reg.RolesHeldBy(sessionID)
		switch {
		case err == nil && slices.Equal(roles, want):
			return refusals
		case err == nil:
			t.Fatalf("RolesHeldBy(%s) answered %v, want %v or ErrRoleViewNotReady: a holder reported without its role", sessionID, roles, want)
		case !errors.Is(err, ErrRoleViewNotReady):
			t.Fatalf("RolesHeldBy(%s) refused with %v, want ErrRoleViewNotReady", sessionID, err)
		case time.Now().After(deadline):
			t.Fatalf("RolesHeldBy(%s) still refuses after 30 s: %v", sessionID, err)
		}
		refusals++
		time.Sleep(5 * time.Millisecond)
	}
}

// A role claim view whose first scan nats.go's idle timer ended holds only the claims that scan
// delivered, so it is no answer: RolesHeldBy refuses it as not ready rather than report the holder
// of a claim the scan did not deliver without its roles. The watcher scans the bucket again
// (newRoleWatcher's kvwatch.WithRescan), and the view answers once that scan completes.
func TestARoleViewWhoseFirstScanTimedOutAnswersOnlyOnceItsRescanCompletes(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	reg, err := Open(conn, WithReplicas(1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	rawRoles, err := js.KeyValue(bus.RoleClaimBucket)
	if err != nil {
		t.Fatalf("open the role bucket: %v", err)
	}
	putRoleClaim(t, rawRoles, "architect", "ses_other")
	putRoleClaim(t, rawRoles, "reviewer", "ses_holder")
	putRoleClaim(t, rawRoles, "planner", "ses_holder")
	// The registry's own view reaches the claims first, so the view the test then starts again
	// is the only one short of them.
	waitFor(t, 5*time.Second, func() bool {
		roles, err := reg.RolesHeldBy("ses_holder")
		return err == nil && slices.Equal(roles, []string{"planner", "reviewer"})
	})

	// The view starts again as a listener start's does, from nothing, and its first scan delivers
	// architect's claim and then stalls until nats.go's idle timer ends it.
	reg.roleWatcher.Stop()
	reg.resetRoleHolders()
	reg.roleWatcher = reg.newRoleWatcher(bus.KeyValue{KeyValue: kvwatchtest.TimingOutFirst(rawRoles, 1)})
	reg.roleWatcher.Start()
	waitFor(t, 5*time.Second, reg.roleWatcher.Ready)
	if roles, err := reg.RolesHeldBy("ses_holder"); !errors.Is(err, ErrRoleViewNotReady) {
		t.Fatalf("after a first scan the idle timer ended, RolesHeldBy(ses_holder) = %v, %v; want ErrRoleViewNotReady", roles, err)
	}
	if refusals := heldOnceComplete(t, reg, "ses_holder", []string{"planner", "reviewer"}); refusals == 0 {
		t.Fatal("the view answered at once; the test needs it refused until the rescan completes")
	}
	if got := heldRoles(t, reg, "ses_other"); !slices.Equal(got, []string{"architect"}) {
		t.Fatalf("after the rescan ses_other holds %v, want [architect]", got)
	}
}

// A recreated role bucket's reset empties the view, and the refill after it holds only the claims
// it has delivered so far, so it is no answer until its scan completes: RolesHeldBy refuses it as
// not ready rather than report a holder whose claim the refill has not reached without its role.
// The refill is held at the holder's claim, after another claim, so the test asks while it is
// partial.
func TestARecreatedRoleBucketsViewAnswersOnlyOnceItsRefillCompletes(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	reg, err := Open(conn, WithReplicas(1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	if _, err := reg.SetRole("ses_holder", "m1", "reviewer", false); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		roles, err := reg.RolesHeldBy("ses_holder")
		return err == nil && slices.Equal(roles, []string{"reviewer"})
	})

	var refilling atomic.Bool
	held := make(chan struct{})
	var holdOnce, releaseOnce sync.Once
	released := make(chan struct{})
	release := func() { releaseOnce.Do(func() { close(released) }) }
	t.Cleanup(release)
	apply := func(entry natsgo.KeyValueEntry) {
		if refilling.Load() && entry.Key() == "reviewer" {
			holdOnce.Do(func() { close(held) })
			<-released
		}
		reg.applyRoleClaim(entry)
	}
	reset := func() {
		reg.resetRoleHolders()
		refilling.Store(true)
	}
	reg.roleWatcher.Stop()
	reg.roleWatcher = kvwatch.New("role claims", reg.roles(), apply, reset)
	reg.roleWatcher.Start()
	waitFor(t, 5*time.Second, reg.roleWatcher.Ready)
	heldOnceComplete(t, reg, "ses_holder", []string{"reviewer"})

	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue(bus.RoleClaimBucket); err != nil {
		t.Fatalf("delete the role bucket: %v", err)
	}
	recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: bus.RoleClaimBucket, Replicas: 1, Storage: natsgo.FileStorage})
	putRoleClaim(t, recreated, "architect", "ses_other")
	putRoleClaim(t, recreated, "reviewer", "ses_holder")
	if err := reg.Rewatch(conn); err != nil {
		t.Fatalf("Rewatch: %v", err)
	}
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("the refill never reached the holder's claim")
	}
	if roles, err := reg.RolesHeldBy("ses_holder"); !errors.Is(err, ErrRoleViewNotReady) {
		t.Fatalf("with the refill short of the holder's claim, RolesHeldBy(ses_holder) = %v, %v; want ErrRoleViewNotReady", roles, err)
	}
	release()
	heldOnceComplete(t, reg, "ses_holder", []string{"reviewer"})
	if got := heldRoles(t, reg, "ses_other"); !slices.Equal(got, []string{"architect"}) {
		t.Fatalf("after the refill ses_other holds %v, want [architect]", got)
	}
}

// endingGetKeyValue ends the request (end) when key's interest record is read, and answers that
// read only after the request has seen its end.
type endingGetKeyValue struct {
	natsgo.KeyValue

	key string
	end context.CancelFunc
}

func (kv *endingGetKeyValue) Get(key string) (natsgo.KeyValueEntry, error) {
	if key == kv.key {
		kv.end()
		time.Sleep(100 * time.Millisecond)
	}
	return kv.KeyValue.Get(key)
}

// A claim whose request ends while its old holder's role topic is being removed has landed, so it
// is a success. The cleanup the request's end cut short failed at nothing: it is logged at INFO,
// and nothing is logged at WARN. The topic it leaves gives the old holder no role.
func TestAClaimWhoseRequestEndsDuringItsCleanupSucceedsAndLogsNoFailure(t *testing.T) {
	logs := captureRegistryLogs(t)
	conn, cleanup := connectNATS(t)
	defer cleanup()
	const (
		oldSession = "ses_old"
		newSession = "ses_new"
		role       = "legion-controller"
		roleTopic  = "notifications.role.legion-controller"
	)
	reg, kv := coldRegistry(t, conn)
	putInterest(t, kv, Interest{SessionID: oldSession, MachineID: "example-host", Topics: []string{roleTopic, "notifications.slack.>"}})
	if _, err := reg.roleKV.Put(role, []byte(oldSession)); err != nil {
		t.Fatalf("seed role claim: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg.watcher = kvwatch.New("interest registry", bus.KeyValue{KeyValue: &endingGetKeyValue{KeyValue: kv, key: oldSession, end: cancel}}, reg.applyWatched, reg.resetCache)

	if _, err := reg.SetRoleWithPrevious(ctx, newSession, "example-host", role, "", false); err != nil {
		t.Fatalf("a claim that landed returned %v when its request ended during the cleanup, want success", err)
	}
	assertRoleHolder(t, reg, role, newSession)
	time.Sleep(200 * time.Millisecond)
	out := logs.String()
	if !strings.Contains(out, "level=INFO") || !strings.Contains(out, "the request ended first") {
		t.Fatalf("the cut-short cleanup was not logged at INFO:\n%s", out)
	}
	if strings.Contains(out, "level=WARN") {
		t.Fatalf("a cleanup the request's end cut short was logged at WARN:\n%s", out)
	}
	t.Logf("cut-short cleanup: %s", strings.TrimSpace(out))
}

// A claim write this registry made on the old role bucket, whose answer arrives after a recreated
// bucket's reset, must not land in the new bucket's view: its revision is the old bucket's, and as
// the view's fence it would drop every lower revision the new bucket delivers, leaving the role
// with the old write's holder (or none) until the new bucket's numbering passes it. The control is
// the same write with no reset between: it applies, and fences out an older delivery.
func TestAClaimWriteAnsweredAcrossARecreatedRoleBucketLeavesTheNewBucketsView(t *testing.T) {
	r := &Registry{}
	epoch := r.roleHolderEpoch()
	r.resetRoleHolders()
	r.setOwnRoleHolder("reviewer", "ses_old_bucket", 7, epoch)
	r.setRoleHolder("reviewer", "ses_new_bucket", 1)
	if got := r.holders["reviewer"]; got != "ses_new_bucket" {
		t.Fatalf("after a reset, reviewer's holder = %q, want ses_new_bucket: the old bucket's claim write at revision 7 fenced out the new bucket's revision 1", got)
	}

	control := &Registry{}
	controlEpoch := control.roleHolderEpoch()
	control.setOwnRoleHolder("reviewer", "ses_written", 7, controlEpoch)
	control.setRoleHolder("reviewer", "ses_delayed", 6)
	if got := control.holders["reviewer"]; got != "ses_written" {
		t.Fatalf("with no reset, reviewer's holder = %q, want ses_written: a delayed delivery at revision 6 undid the registry's own write at 7", got)
	}
}
