// A deregistration releases the role claims its session holds from two sources: the role topics
// of the session's own interest record (releaseRoleClaims), and the role claim view
// (releaseAllRoleClaims), which reads the roles the session holds from the watcher's copy of the
// bucket and makes no bucket call to find them. The scan of the role bucket that the view
// replaced is still there as the fallback a view that cannot be served runs
// (releaseAllRoleClaimsByScan), and it is no longer what answered the route 503 this change was
// filed on: it is one streamed scan carrying every record's value and revision, and past that
// scan the fallback sends nothing but the deletes it owes, where it once paid a round trip per
// key of the bucket.
//
// These cases are the ones the issue's Verification document asks for before the merge. What
// each turns on is a count or a holder, never a duration, except the one deadline that says what
// the fallback now costs: over the same bucket of 5,002 claims it finishes a deregistration
// inside the same second the view sweep answers in, from one scan and no read per key. The
// control against a later change quietly putting that read back is a count and not a clock - the
// scan's Gets are zero, and a Get per key would be read straight off the handle that counts them
// - because a scan this fast would beat the deadline even carrying a few reads it does not need.

package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

const (
	// otherRoleClaims is how many claims held by sessions other than the one under test the large
	// fixture's bucket holds. It is the bucket size no sweep may pay a call per key for: at
	// claimCallDelay a Get per key could not answer inside sweepDeadline, which is why the cases
	// below read what a sweep cost from the counts rather than from the clock.
	otherRoleClaims = 5000
	// smallRoleClaims is the bucket the cases about one failed call use. They turn on which call
	// failed, not on how many there were, so the bucket is small enough that every one of them
	// finishes well inside any deadline.
	smallRoleClaims = 50
	// claimCallDelay is what the counting handle adds to each role bucket Get and Delete, so a
	// sweep that makes a call per key of the bucket cannot finish inside a second and one that
	// makes two calls per role held finishes in a tenth of it. It stands in for the round trip a
	// deployed bucket's call pays, which an in-process server does not.
	claimCallDelay = 10 * time.Millisecond
	// sweepDeadline is a hang guard, not a budget: it bounds every deregistration below so a
	// sweep that never answers fails its case instead of hanging it. Nothing here reads it as a
	// cost. What each case proves is its counts -- no Get per key, one scan, one Delete per claim
	// the session holds -- which a sweep that read the bucket per key fails whatever the clock
	// says.
	//
	// It was one second, and the comment here called that the deregistration route's own budget,
	// which it is not: the routes that deregister give their store work sendRequestBudget,
	// 3.5 s (cmd/listener/api.go), and never one second. So the second was this file's own
	// number, and it made the fallback case a timing assertion on work it does not control: that
	// case's one scan carries all 5,002 claims' payloads, which measured 1.000 s and 1.001 s on
	// a loaded two-CPU pod, so the case failed in CI (envoy-go-race job 128260301379) and in 50
	// of 50 executions here, at main's head and on the branch alike, taking the reaper case
	// after it down with it, since a fallback that did not finish leaves its two claims in the
	// bucket the cases share.
	//
	// Thirty seconds is an order above that scan and still well below the fifty a Get per key
	// would cost (otherRoleClaims x claimCallDelay), which is the one thing this bound
	// discriminates. What the production budget covers is a separate question these cases do not
	// answer: at the per-claim cost measured here, about 0.2 ms, 3.5 s covers a role bucket of
	// roughly seventeen thousand claims, and the scan runs at all only when the view cannot be
	// served, every other deregistration costing the two reads and two deletes the first case
	// counts.
	sweepDeadline = 30 * time.Second
)

// sweepRoleKeyValue is the role bucket's legacy handle with every Get and Delete counted, per key
// and in total, and each delayed by delay. It is how these cases read what a deregistration cost:
// the count, not the clock, is what says the sweep read the roles the session holds rather than
// the bucket. Counting and delaying are on one handle because a count is only evidence while the
// calls counted are the calls the deregistration made, and a delay that applied to some other
// handle's calls would prove nothing about these.
//
// failDelete fails the named key's delete with an error of the test's own, as a bus that refuses
// one call of many does, and beforeFirstDelete runs before the first delete reaches the bucket,
// so another session's claim can land under it (racingUpdateKeyValue's shape, for the claim
// path). Both, and delay, are set before the handle is installed and never written again, so the
// goroutines bus.Bounded runs these calls on only read them; the counters are the only state a
// call writes, and they are guarded, because a sweep's calls and the test's reads of the counts
// are on different goroutines and -race would otherwise report them.
//
// scanTotal counts the watches the sweeps open over the bucket, one per pass over every key, and
// is written by the jetstream handle beside this one (sweepRoleWatchHandle) under this same
// mutex and read by scans. A sweep now costs one scan and the deletes it owes, so that pair of
// counts - one scan, no Get - is the whole of what these cases read its cost by.
type sweepRoleKeyValue struct {
	natsgo.KeyValue

	delay             time.Duration
	failDelete        map[string]error
	beforeFirstDelete func(key string)

	mu          sync.Mutex
	gets        map[string]int
	deletes     map[string]int
	getTotal    int
	deleteTotal int
	scanTotal   int
}

func (kv *sweepRoleKeyValue) Get(key string) (natsgo.KeyValueEntry, error) {
	kv.mu.Lock()
	kv.gets[key]++
	kv.getTotal++
	kv.mu.Unlock()
	time.Sleep(kv.delay)
	return kv.KeyValue.Get(key)
}

func (kv *sweepRoleKeyValue) Delete(key string, opts ...natsgo.DeleteOpt) error {
	kv.mu.Lock()
	kv.deletes[key]++
	kv.deleteTotal++
	first := kv.deleteTotal == 1
	kv.mu.Unlock()
	// The hook runs outside the lock: it writes through the bucket's own handle, and a test that
	// reads the counts from it would otherwise deadlock on this one.
	if first && kv.beforeFirstDelete != nil {
		kv.beforeFirstDelete(key)
	}
	time.Sleep(kv.delay)
	if err := kv.failDelete[key]; err != nil {
		return err
	}
	return kv.KeyValue.Delete(key, opts...)
}

// totals is how many Gets and Deletes the role bucket has taken through this handle.
func (kv *sweepRoleKeyValue) totals() (gets, deletes int) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return kv.getTotal, kv.deleteTotal
}

// callsOn is how many Gets and Deletes key has taken through this handle.
func (kv *sweepRoleKeyValue) callsOn(key string) (gets, deletes int) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return kv.gets[key], kv.deletes[key]
}

// scans is how many scans of the role bucket have been opened through the jetstream handle
// beside this one: one watch of every key is one pass over the bucket.
func (kv *sweepRoleKeyValue) scans() int {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return kv.scanTotal
}

// sweepRoleWatchHandle is the role bucket's jetstream handle with every watch counted on the
// sweepRoleKeyValue it belongs to, and every other call forwarded to the handle it wraps. A scan
// of the bucket is one watch of all its keys (kvwatch.ScanExistingKeys), so the count of watches
// is the count of passes over the bucket, which is what a sweep that reads from one scan costs.
//
// It counts the sweeps' scans and not the role watcher's, which follows the bucket for the life
// of the registry: the watcher captured the bus.KeyValue it watches through when it was built
// (newRoleWatcher, at Open), so it goes on watching through the handle it holds and never sees
// the one countRoleBucketCalls installs afterwards. Every watch counted here was opened by a
// call the case under test made.
type sweepRoleWatchHandle struct {
	jetstream.KeyValue

	counts *sweepRoleKeyValue
}

func (kv sweepRoleWatchHandle) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	kv.counts.mu.Lock()
	kv.counts.scanTotal++
	kv.counts.mu.Unlock()
	return kv.KeyValue.Watch(ctx, keys, opts...)
}

// swapRoleBucketHandle installs the handle build returns over reg's role bucket handle for the
// rest of the test, and puts the previous handle back when the test ends, so a case that shares
// a registry with another leaves it as it found it. build is handed the bucket's own legacy
// handle to wrap; the watch handle beside it is kept, and wrapWatch, when it is not nil, wraps
// that one too. Keeping it is what leaves the case under test able to read the bucket at all: a
// scan of it is a watch of all its keys (kvwatch.ScanExistingKeys), so a handle built without
// that half would give a scan that silently delivers nothing, which is the very thing these
// cases measure.
//
// The swap is made under the mutex that guards that handle (Registry.kvMu), which every reader
// of it takes (Registry.roles), so a call in flight - a watcher's, or another case's release -
// cannot read the field while this writes it, which -race would otherwise report.
func swapRoleBucketHandle[H natsgo.KeyValue](t *testing.T, reg *Registry, build func(legacy natsgo.KeyValue) H, wrapWatch func(watch jetstream.KeyValue, handle H) jetstream.KeyValue) H {
	t.Helper()
	reg.kvMu.Lock()
	previous := reg.roleKV
	handle := build(previous.Legacy())
	watch := previous.WatchHandle()
	if wrapWatch != nil {
		watch = wrapWatch(watch, handle)
	}
	reg.roleKV = bus.NewKeyValue(handle, watch, previous.Streams())
	reg.kvMu.Unlock()
	t.Cleanup(func() {
		reg.kvMu.Lock()
		defer reg.kvMu.Unlock()
		reg.roleKV = previous
	})
	return handle
}

// countRoleBucketCalls installs handle over reg's role bucket handle for the rest of the test
// (swapRoleBucketHandle). Both halves of that handle are wrapped: handle itself counts the Gets
// and Deletes the legacy half takes, and a sweepRoleWatchHandle over the jetstream half counts
// the scans, so a sweep that reads the bucket by watching it is counted rather than passing
// straight through.
func countRoleBucketCalls(t *testing.T, reg *Registry, handle *sweepRoleKeyValue) *sweepRoleKeyValue {
	t.Helper()
	handle.gets = map[string]int{}
	handle.deletes = map[string]int{}
	return swapRoleBucketHandle(t, reg,
		func(legacy natsgo.KeyValue) *sweepRoleKeyValue {
			handle.KeyValue = legacy
			return handle
		},
		func(watch jetstream.KeyValue, counts *sweepRoleKeyValue) jetstream.KeyValue {
			return sweepRoleWatchHandle{KeyValue: watch, counts: counts}
		})
}

// putRoleClaimsInParallel stores one claim per role, each held by holder(role), from a bounded
// pool of goroutines. The pool is here for the clock alone: 5,002 sequential puts through one
// handle measured 3.61 s in this pod, which is too slow to pay once per case, so the large
// fixture is built once and shared by the cases that can share it.
func putRoleClaimsInParallel(t *testing.T, kv natsgo.KeyValue, roles []string, holder func(role string) string) {
	t.Helper()
	const workers = 32
	pending := make(chan string)
	var pool sync.WaitGroup
	for range workers {
		pool.Add(1)
		go func() {
			defer pool.Done()
			for role := range pending {
				putRoleClaim(t, kv, role, holder(role))
			}
		}()
	}
	for _, role := range roles {
		pending <- role
	}
	close(pending)
	pool.Wait()
}

// otherSessionsClaims is count role names, each held by a session of its own, none of them the
// session a case deregisters: the claims a sweep must not read.
func otherSessionsClaims(prefix string, count int) ([]string, func(role string) string) {
	roles := make([]string, count)
	for i := range roles {
		roles[i] = fmt.Sprintf("%s-%05d", prefix, i)
	}
	return roles, func(role string) string { return "ses_holder_of_" + role }
}

// assertRoleBucketCalls fails the test unless the role bucket took exactly one Get and one Delete
// for each of held and nothing at all for any other key. It is the whole of check v1: a sweep
// that reads the roles its session holds from the view calls the bucket twice per role held, and
// the scan it replaced calls it once per key of the bucket, so the two differ by the count long
// before they differ by the clock.
func assertRoleBucketCalls(t *testing.T, handle *sweepRoleKeyValue, held []string) {
	t.Helper()
	gets, deletes := handle.totals()
	t.Logf("the deregistration made %d Gets and %d Deletes on a role bucket of %d claims", gets, deletes, otherRoleClaims+len(held))
	if gets != len(held) || deletes != len(held) {
		t.Fatalf("the deregistration made %d Gets and %d Deletes on a role bucket of %d claims; want %d of each, one per role the session holds",
			gets, deletes, otherRoleClaims+len(held), len(held))
	}
	for _, role := range held {
		roleGets, roleDeletes := handle.callsOn(role)
		if roleGets != 1 || roleDeletes != 1 {
			t.Fatalf("role %s, which the session holds, took %d Gets and %d Deletes; want one of each", role, roleGets, roleDeletes)
		}
	}
}

// scannedRoleNames is the roles a scan of the bucket delivered, in the order it delivered them,
// for a check that is about which keys the bucket still holds rather than about their claims.
func scannedRoleNames(scanned []scannedRoleClaim) []string {
	names := make([]string, 0, len(scanned))
	for _, claim := range scanned {
		names = append(names, claim.Role)
	}
	return names
}

// assertBucketKeptOnlyTheOtherClaims fails the test unless the role bucket holds every claim the
// fixture seeded for other sessions and neither of released. A sweep that released a claim of
// another session's would be the bug check v3 is about, and one that left its own session's claim
// behind is the leak the deregistration exists to prevent.
func assertBucketKeptOnlyTheOtherClaims(t *testing.T, reg *Registry, released []string) {
	t.Helper()
	scanned, err := reg.roleClaimsUntil(context.Background())
	if err != nil {
		t.Fatalf("list the role bucket's keys after the deregistration: %v", err)
	}
	keys := scannedRoleNames(scanned)
	if len(keys) != otherRoleClaims {
		t.Fatalf("the role bucket holds %d claims after the deregistration, want the %d held by other sessions", len(keys), otherRoleClaims)
	}
	for _, role := range released {
		if slices.Contains(keys, role) {
			t.Fatalf("the deregistered session's claim on %s is still in the bucket", role)
		}
	}
}

// Check v1. A deregistration costs the roles its session holds, not the size of the role bucket.
// The fixture is 5,000 claims held by other sessions and two held by the session deregistered,
// and every role bucket Get and Delete takes claimCallDelay: a sweep that reads the roles from
// the view answers in four of those delays, and the two sweeps that read the bucket itself - the
// fallback a view that cannot be served runs, and the role claim reaper's pass - pay one scan
// over the 5,002 claims and nothing per key but the deletes they owe, which the last two cases
// here measure over the very same fixture rather than assert of the code from memory.
//
// The four cases share one bucket of that size, which is why they are subtests of one function:
// seeding it costs seconds, and each leaves it holding exactly the 5,000 claims the other
// sessions hold, whatever order they run in.
//
// The first case's interest record carries no role topic, which is the point of it: Remove
// releases from the record's role topics first (releaseRoleClaims) and from the view second
// (releaseAllRoleClaims), so a session whose record names its roles has them released by the
// first pass, and a case written that way would pass with no view sweep at all. With no role
// topic on the record, only the view sweep can release these two claims. The second case is the
// production shape, where the record does name them: every release the record pass makes leaves
// the view (forgetRoleHolder), so the sweep that follows finds nothing left to do and the counts
// are the same two Gets and two Deletes.
func TestTheDeregistrationSweepReadsOnlyTheClaimsItsSessionHoldsFromALargeRoleBucket(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.InterestBucket, Replicas: 1, Storage: natsgo.FileStorage}); err != nil {
		t.Fatalf("create the interest bucket: %v", err)
	}
	rawRoles, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.RoleClaimBucket, Replicas: 1, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("create the role bucket: %v", err)
	}
	seeded := time.Now()
	others, holderOfOther := otherSessionsClaims("other-role", otherRoleClaims)
	putRoleClaimsInParallel(t, rawRoles, others, holderOfOther)
	t.Logf("seeded %d claims held by other sessions in %s", otherRoleClaims, time.Since(seeded).Round(time.Millisecond))

	reg, err := Open(conn, WithReplicas(1))
	if err != nil {
		t.Fatalf("open the registry over the seeded role bucket: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	ready, cancelReady := context.WithTimeout(context.Background(), time.Minute)
	defer cancelReady()
	if err := reg.WaitForCacheReady(ready); err != nil {
		t.Fatalf("wait for the caches over %d stored claims: %v", otherRoleClaims, err)
	}

	// deregister seeds session's claims and its interest record, waits until the view names it
	// the holder of both roles, and removes it under sweepDeadline with every role bucket call
	// delayed. It returns the deregistration's own error and the handle that counted its calls.
	deregister := func(t *testing.T, session string, roles []string, topics []string) (error, *sweepRoleKeyValue) {
		t.Helper()
		for _, role := range roles {
			putRoleClaim(t, rawRoles, role, session)
		}
		if _, err := reg.Upsert(Interest{SessionID: session, MachineID: "m1"}, topics); err != nil {
			t.Fatalf("store %s's interest record: %v", session, err)
		}
		waitForHeld(t, reg, session, roles...)

		counting := countRoleBucketCalls(t, reg, &sweepRoleKeyValue{delay: claimCallDelay})
		ctx, cancel := context.WithTimeout(context.Background(), sweepDeadline)
		defer cancel()
		started := time.Now()
		err := reg.Remove(ctx, session, nil)
		t.Logf("Remove(%s) over %d stored claims answered in %s", session, otherRoleClaims+len(roles), time.Since(started).Round(time.Millisecond))
		return err, counting
	}

	t.Run("a session whose record carries no role topic has both its claims released by the view sweep alone", func(t *testing.T) {
		const session = "ses_sweep_view_only"
		roles := []string{"sweep-view-only-a", "sweep-view-only-b"}
		// The record's only topic is not a role's, so the pass over it releases nothing and
		// every call the bucket takes below is the view sweep's own.
		err, counting := deregister(t, session, roles, []string{"notifications.chat"})
		if err != nil {
			t.Fatalf("Remove(%s) under %s over %d stored claims: %v", session, sweepDeadline, otherRoleClaims+len(roles), err)
		}
		assertRoleBucketCalls(t, counting, roles)
		assertBucketKeptOnlyTheOtherClaims(t, reg, roles)
		assertInterestMissing(t, reg, session)
	})

	t.Run("a session whose record carries its role topics pays the same two reads and two deletes", func(t *testing.T) {
		const session = "ses_sweep_with_topics"
		roles := []string{"sweep-with-topics-a", "sweep-with-topics-b"}
		topics := []string{contracts.RoleTopicPrefix + roles[0], contracts.RoleTopicPrefix + roles[1]}
		err, counting := deregister(t, session, roles, topics)
		if err != nil {
			t.Fatalf("Remove(%s) under %s over %d stored claims: %v", session, sweepDeadline, otherRoleClaims+len(roles), err)
		}
		// The record pass released both, so each role's single Get and Delete is that pass's;
		// the view sweep behind it found the view no longer naming the session any role, which
		// is how the counts stay at two rather than doubling.
		assertRoleBucketCalls(t, counting, roles)
		assertBucketKeptOnlyTheOtherClaims(t, reg, roles)
		assertInterestMissing(t, reg, session)
	})

	// Acceptance criterion A1 for the degraded path, and the stronger control the case it
	// replaced tried to be. A registry whose role claim view cannot be served (no watcher
	// follows the role claims) takes the fallback over this very same bucket, and the scan it
	// now runs answers inside the route's own budget: one pass over the 5,002 claims, no read
	// per key, and one delete per claim the session holds. The zero Gets are what a later
	// change cannot slip a per-key read past - the count shows it long before any clock would,
	// since a scan this fast would beat the deadline carrying a few reads it does not need.
	t.Run("the same fixture on a view that cannot be served answers inside the deadline from one scan and no per-key read", func(t *testing.T) {
		const session = "ses_sweep_fallback"
		roles := []string{"sweep-fallback-a", "sweep-fallback-b"}
		cold, rawInterests := coldRegistry(t, conn)
		viewErr := cold.RoleViewErr()
		if viewErr == nil {
			t.Fatal("the control registry's role claim view can be served, so it would take the view sweep and control nothing")
		}
		if !errors.Is(viewErr, ErrRoleViewNotReady) {
			t.Fatalf("the control registry's view refuses with %v, want ErrRoleViewNotReady", viewErr)
		}
		for _, role := range roles {
			putRoleClaim(t, rawRoles, role, session)
		}
		// The record's only topic is not a role's, so the pass over it releases nothing and
		// every call the bucket takes below is the fallback's own.
		putInterest(t, rawInterests, Interest{SessionID: session, MachineID: "m1", Topics: []string{"notifications.chat"}})
		counting := countRoleBucketCalls(t, cold, &sweepRoleKeyValue{delay: claimCallDelay})

		ctx, cancel := context.WithTimeout(context.Background(), sweepDeadline)
		defer cancel()
		started := time.Now()
		err := cold.Remove(ctx, session, nil)
		elapsed := time.Since(started)
		gets, deletes := counting.totals()
		scans := counting.scans()
		// The scan's own duration is what the route's budget pays for now: the listing carries
		// every claim's payload and not its metadata alone, so this is the number a bucket that
		// grows is read against.
		t.Logf("the fallback's scan released %s's claims over %d stored claims in %s, against the %s deadline",
			session, otherRoleClaims+len(roles), elapsed.Round(time.Millisecond), sweepDeadline)
		t.Logf("the fallback made %d scans, %d Gets and %d Deletes over %d stored claims",
			scans, gets, deletes, otherRoleClaims+len(roles))
		// The control is read before the answer, because a per-key read put back is what this
		// case is here to catch and it shows in the count whether the deregistration then came
		// in under the deadline or ran past it.
		if gets != 0 {
			t.Fatalf("the fallback made %d Gets over %d stored claims, want none: every claim comes from the scan",
				gets, otherRoleClaims+len(roles))
		}
		if err != nil {
			t.Fatalf("Remove(%s) under %s on a view that cannot be served, over %d stored claims: %v",
				session, sweepDeadline, otherRoleClaims+len(roles), err)
		}
		if scans != 1 {
			t.Fatalf("the fallback made %d scans of the role bucket, want the one it reads every claim from", scans)
		}
		if deletes != len(roles) {
			t.Fatalf("the fallback made %d Deletes, want %d, one per claim %s holds", deletes, len(roles), session)
		}
		for _, role := range roles {
			roleGets, roleDeletes := counting.callsOn(role)
			if roleGets != 0 || roleDeletes != 1 {
				t.Fatalf("role %s, which the session holds, took %d Gets and %d Deletes; want no Get and the one delete",
					role, roleGets, roleDeletes)
			}
		}
		assertBucketKeptOnlyTheOtherClaims(t, cold, roles)
		assertInterestMissing(t, cold, session)
	})

	// Acceptance criterion A1 for the role claim reaper, over the same fixture: a pass is one
	// scan of the bucket and nothing per role but the deletes of the claims whose holders are
	// gone. Three of those among 5,000 whose holders are alive, so a pass that read a claim per
	// key would show 5,003 Gets where this one shows none.
	t.Run("one reaper pass over the same bucket reaps the gone holders' claims from one scan and no per-key read", func(t *testing.T) {
		gone := []string{"sweep-reaped-a", "sweep-reaped-b", "sweep-reaped-c"}
		goneHolders := map[string]bool{}
		for _, role := range gone {
			holder := "ses_gone_holder_of_" + role
			goneHolders[holder] = true
			putRoleClaim(t, rawRoles, role, holder)
		}
		counting := countRoleBucketCalls(t, reg, &sweepRoleKeyValue{delay: claimCallDelay})

		started := time.Now()
		sweep, err := reg.ReapRoleClaims(context.Background(), func(holder string) bool { return !goneHolders[holder] }, 0)
		elapsed := time.Since(started)
		if err != nil {
			t.Fatalf("a reaper pass over %d stored claims: %v", otherRoleClaims+len(gone), err)
		}
		t.Logf("the reaper pass over %d stored claims answered in %s", otherRoleClaims+len(gone), elapsed.Round(time.Millisecond))
		if want := (RoleClaimSweep{Reaped: len(gone)}); sweep != want {
			t.Fatalf("the pass = %+v, want %+v: the claims of the three holders that are gone reaped, and nothing skipped", sweep, want)
		}
		gets, deletes := counting.totals()
		t.Logf("the reaper pass made %d scans, %d Gets and %d Deletes over %d stored claims",
			counting.scans(), gets, deletes, otherRoleClaims+len(gone))
		if scans := counting.scans(); scans != 1 {
			t.Fatalf("the pass made %d scans of the role bucket, want the one it reads every claim from", scans)
		}
		if gets != 0 {
			t.Fatalf("the pass made %d Gets over %d stored claims, want none: every claim comes from the scan",
				gets, otherRoleClaims+len(gone))
		}
		if deletes != len(gone) {
			t.Fatalf("the pass made %d Deletes, want %d, one per claim whose holder is gone", deletes, len(gone))
		}
		// The fixture is left exactly as every other case here finds it: the 5,000 claims of
		// the sessions that are alive, not one of them released by a pass that swept three
		// others out from between them.
		scanned, err := reg.roleClaimsUntil(context.Background())
		if err != nil {
			t.Fatalf("list the role bucket after the reaper pass: %v", err)
		}
		kept := make([]string, 0, len(scanned))
		for _, claim := range scanned {
			if claim.Claim.HolderSessionID != holderOfOther(claim.Role) {
				t.Fatalf("role %s is held by %q after the pass, want the %q the fixture seeded",
					claim.Role, claim.Claim.HolderSessionID, holderOfOther(claim.Role))
			}
			kept = append(kept, claim.Role)
		}
		if !slices.Equal(kept, others) {
			t.Fatalf("the role bucket holds %d claims after the pass, want the %d held by the sessions that are alive",
				len(kept), otherRoleClaims)
		}
	})
}

// smallRoleBucketRegistry is a registry whose view cannot be served, over a bucket of
// smallRoleClaims claims: held are the roles session holds, and the rest are held by sessions of
// their own. It returns the registry, the role bucket's own handle - taken before any counting
// handle is installed over it, so a case's own writes are not counted as the sweep's - and the
// roles the other sessions hold.
func smallRoleBucketRegistry(t *testing.T, session string, held []string) (*Registry, natsgo.KeyValue, []string) {
	t.Helper()
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	reg, rawInterests := coldRegistry(t, conn)
	rawRoles := reg.roles().Legacy()
	others, holderOfOther := otherSessionsClaims("small-other", smallRoleClaims-len(held))
	putRoleClaimsInParallel(t, rawRoles, others, holderOfOther)
	for _, role := range held {
		putRoleClaim(t, rawRoles, role, session)
	}
	putInterest(t, rawInterests, Interest{SessionID: session, MachineID: "m1", Topics: []string{"notifications.chat"}})
	if err := reg.RoleViewErr(); err == nil {
		t.Fatal("the registry's role claim view can be served, so these cases would never reach the fallback")
	}
	return reg, rawRoles, others
}

// Check v2. One stored record the sweep cannot use must not fail a deregistration that did not
// need it. The fallback reads a bucket of claims that are almost all other sessions', so a
// record among them that this build cannot decode says nothing about the session being removed:
// it is logged, naming the role, and the sweep goes on to the claims that are the session's.
// That record is the one per-claim read failure left now that every claim comes from one scan -
// there is no Get per key to fail - and it reads the same on every pass, so the claim is left
// standing for the role reaper to meet again. A delete that fails is the opposite case and must
// fail the call, because the claim it did not delete is one the session holds and nothing else
// in this deregistration will release it.
//
// These two are a pair on purpose. A sweep that returned every error alike would turn one
// stranger's unreadable claim into a failed deregistration, and one that swallowed every error
// alike would report a session deregistered while it still held a role, which is the state that
// stops the next holder from claiming it.
func TestTheDeregistrationSweepsFallbackGoesPastAClaimItCannotReadAndFailsOnOneItCannotDelete(t *testing.T) {
	t.Run("a claim of another session that cannot be read is logged and the sweep goes on", func(t *testing.T) {
		logs := captureRegistryLogs(t)
		const session = "ses_small_unreadable"
		held := []string{"small-held-read-a", "small-held-read-b"}
		reg, rawRoles, others := smallRoleBucketRegistry(t, session, held)
		unreadable := others[len(others)/2]
		// A stored record this build cannot decode, written straight to the bucket: with every
		// claim read from one scan there is no per-key read left to fail, so this is the one way
		// a single claim of the bucket is unreadable while the rest are read.
		if _, err := rawRoles.Put(unreadable, []byte(`["not a role claim"]`)); err != nil {
			t.Fatalf("store a record this build cannot decode under %s: %v", unreadable, err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := reg.Remove(ctx, session, nil); err != nil {
			t.Fatalf("Remove(%s) over a bucket with one unreadable claim of another session's: %v, want it released and no error", session, err)
		}
		for _, role := range held {
			assertRoleHolder(t, reg, role, "")
		}
		assertInterestMissing(t, reg, session)
		// The line is the only record that a claim was left unread, so it must name which: an
		// operator reading it has no other way to tell the role apart from the thousands the
		// scan did read. It is one summary line for the whole sweep rather than one per claim,
		// because the sweep runs only while the bus is degraded, so it carries the count beside
		// the first claim it met - which is this one claim and this one role here. Every field is
		// read off that one line, because the fallback's own INFO line carries the session too
		// and a check over the whole log would take it for this one.
		const left = "role registry could not read stored claims while releasing a session's claims; they stay in the bucket, unreleased by this deregistration, and role and error are the first claim's"
		line := logLine(t, logs, left)
		if !strings.Contains(line, "role="+unreadable) || !strings.Contains(line, "session_id="+session) || !strings.Contains(line, "claims=1") {
			t.Fatalf("the sweep's line for the unread claim = %s; want it to name the claim %s, the session %s and the one claim it left", line, unreadable, session)
		}
		if wrote := logLines(logs, left); wrote != 1 {
			t.Fatalf("the sweep wrote %d lines for the claims it left, want the one summary line a degraded deregistration pays for", wrote)
		}
		// And the claim it could not read is still in the bucket, which is what the line says:
		// this deregistration released it no more than it read it.
		scanned, err := reg.roleClaimsUntil(context.Background())
		if err != nil {
			t.Fatalf("list the role bucket's keys: %v", err)
		}
		keys := scannedRoleNames(scanned)
		if !slices.Contains(keys, unreadable) {
			t.Fatalf("the claim the sweep could not read is gone from the bucket; the line says it stays there")
		}
		if len(keys) != smallRoleClaims-len(held) {
			t.Fatalf("the bucket holds %d claims, want the %d other sessions' with both of %s's released", len(keys), smallRoleClaims-len(held), session)
		}
	})

	t.Run("a claim the session holds that cannot be deleted fails the deregistration", func(t *testing.T) {
		const session = "ses_small_undeletable"
		// Both sort before every other session's claim, so the scan reaches the undeletable one
		// first and what it does next is not a matter of which key the bucket happens to list
		// first.
		held := []string{"small-held-delete-a", "small-held-delete-b"}
		reg, _, _ := smallRoleBucketRegistry(t, session, held)
		refusal := errors.New("nats: the bus refused the delete")
		countRoleBucketCalls(t, reg, &sweepRoleKeyValue{failDelete: map[string]error{held[0]: refusal}})

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := reg.Remove(ctx, session, nil)
		if !errors.Is(err, refusal) {
			t.Fatalf("Remove(%s) answered %v, want the refused delete of the claim it still holds", session, err)
		}
		// The sweep stopped where it failed: neither claim is released, and the interest record
		// is still there, so the caller's retry finds the same session to deregister.
		for _, role := range held {
			assertRoleHolder(t, reg, role, session)
		}
		if _, err := reg.Get(context.Background(), session); err != nil {
			t.Fatalf("read %s's interest record after the failed deregistration: %v, want the record left for the retry", session, err)
		}
	})
}

// Check v3. The roles the sweep releases come from a copy of the bucket, which is at best a
// moment behind it, so the sweep must never take the copy's word for who holds a role: every
// release it makes is conditional on the very revision it has just read, and a claim that is no
// longer the session's is left alone. Three cases, one per way the copy and the bucket can
// disagree, each with the watcher's delivery of the role gated (gatedRoleClaims) so that what the
// view holds is exactly what this registry's own writes put there and nothing the watcher
// delivered later can mask the disagreement.
//
// Each session's record carries a topic that is not the role's, so the pass over the record
// releases nothing and the claim's fate is the view sweep's alone (removeInterestTopics takes the
// role topic SetRole added back off the record, releasing nothing).
func TestTheDeregistrationSweepLeavesEveryClaimAnotherSessionHolds(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.InterestBucket, Replicas: 1, Storage: natsgo.FileStorage}); err != nil {
		t.Fatalf("create the interest bucket: %v", err)
	}
	rawRoles, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.RoleClaimBucket, Replicas: 1, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("create the role bucket: %v", err)
	}
	reg, err := Open(conn, WithReplicas(1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	ready, cancelReady := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelReady()
	if err := reg.WaitForCacheReady(ready); err != nil {
		t.Fatalf("WaitForCacheReady: %v", err)
	}

	const (
		ownWrite     = "sweep-own-write"
		takenBefore  = "sweep-taken-before"
		takenDuring  = "sweep-taken-during"
		otherHolder  = "ses_sweep_other_holder"
		recordsTopic = "notifications.chat"
	)
	// The watcher holds its delivery of these three roles for good, so no delivery of them ever
	// reaches the view: none of them is in the bucket yet, so the read that makes the view
	// servable does not meet the gate. From here the view's copy of each is whatever this
	// registry's own claim writes put there (setOwnRoleHolder), which is the state a listener is
	// in between a claim's write and the watcher's echo of it.
	gated := map[string]bool{ownWrite: true, takenBefore: true, takenDuring: true}
	gate := make(chan struct{})
	var openOnce sync.Once
	open := func() { openOnce.Do(func() { close(gate) }) }
	t.Cleanup(open)
	reg.roleWatcher.Stop()
	reg.roleWatcher = kvwatch.New("role claims", reg.roles(), gatedRoleClaims{r: reg, hold: func(key string) {
		if gated[key] {
			<-gate
		}
	}})
	reg.roleWatcher.Start()
	waitFor(t, 30*time.Second, func() bool { return servableView(reg) })

	// claimAndDropTheTopic has session claim role through the registry, so the claim reaches the
	// view through the write rather than the watcher, and then takes the role's topic back off
	// session's record, leaving a record that carries one topic that is not a role's. What
	// releases the claim after that can only be the view sweep.
	claimAndDropTheTopic := func(t *testing.T, session, role string) {
		t.Helper()
		if _, err := reg.Upsert(Interest{SessionID: session, MachineID: "m1"}, []string{recordsTopic}); err != nil {
			t.Fatalf("store %s's interest record: %v", session, err)
		}
		if _, err := reg.SetRole(session, "m1", role, false); err != nil {
			t.Fatalf("claim %s for %s: %v", role, session, err)
		}
		if holder := holderOf(reg, role); holder != session {
			t.Fatalf("the view names %q the holder of %s after %s's own claim write, want %s", holder, role, session, session)
		}
		if err := reg.removeInterestTopics(context.Background(), session, []string{contracts.RoleTopicPrefix + role}); err != nil {
			t.Fatalf("take %s's topic off %s's record: %v", role, session, err)
		}
	}

	t.Run("a claim the registry wrote after the view's last read of the bucket is released", func(t *testing.T) {
		const session = "ses_sweep_own_write"
		claimAndDropTheTopic(t, session, ownWrite)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := reg.Remove(ctx, session, nil); err != nil {
			t.Fatalf("Remove(%s): %v", session, err)
		}
		// The watcher has delivered nothing of this role, so a sweep reading the roles from the
		// bucket's last delivered state would have found none to release: the claim is in the
		// view because the write put it there, and that is what the sweep released.
		assertRoleHolder(t, reg, ownWrite, "")
		if holder := holderOf(reg, ownWrite); holder != "" {
			t.Fatalf("the view still names %q the holder of %s after its release", holder, ownWrite)
		}
		assertInterestMissing(t, reg, session)
	})

	t.Run("a claim the view still names the session's that another session has taken is kept", func(t *testing.T) {
		const session = "ses_sweep_taken_before"
		claimAndDropTheTopic(t, session, takenBefore)
		// Another session takes the role by writing its claim to the bucket, as another listener
		// does. The watcher's delivery of it is gated, so the view goes on naming the session
		// below the holder: the disagreement the sweep must not act on.
		revision := putRoleClaim(t, rawRoles, takenBefore, otherHolder)
		if holder := holderOf(reg, takenBefore); holder != session {
			t.Fatalf("the view names %q the holder of %s, want the stale %s: the gate did not hold the delivery and the case tests nothing", holder, takenBefore, session)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := reg.Remove(ctx, session, nil); err != nil {
			t.Fatalf("Remove(%s): %v", session, err)
		}
		// The read the sweep makes before each release names the holder, and it is not this
		// session, so the claim is not this session's to release: the new holder keeps the role
		// at the very revision it wrote.
		assertRoleHolder(t, reg, takenBefore, otherHolder)
		entry, err := rawRoles.Get(takenBefore)
		if err != nil {
			t.Fatalf("read %s's claim after the deregistration: %v", takenBefore, err)
		}
		if entry.Revision() != revision {
			t.Fatalf("%s's claim is at revision %d after the deregistration, want the %d %s wrote", takenBefore, entry.Revision(), revision, otherHolder)
		}
		assertInterestMissing(t, reg, session)
	})

	t.Run("a claim another session takes between the sweep's read and its delete is kept", func(t *testing.T) {
		const session = "ses_sweep_taken_during"
		claimAndDropTheTopic(t, session, takenDuring)
		// The race the conditional delete exists for: the sweep has read the claim and found it
		// the session's, and the new holder's claim lands before the delete reaches the bucket.
		// The delete names the revision the read returned, so it loses, and losing it is not a
		// failure - the claim it did not delete is the new holder's.
		counting := countRoleBucketCalls(t, reg, &sweepRoleKeyValue{beforeFirstDelete: func(key string) {
			if key != takenDuring {
				return
			}
			if _, err := rawRoles.Put(key, []byte(`{"holder_session_id":"`+otherHolder+`"}`)); err != nil {
				t.Errorf("land %s's claim on %s under the sweep's delete: %v", otherHolder, key, err)
			}
		}})

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := reg.Remove(ctx, session, nil); err != nil {
			t.Fatalf("Remove(%s) lost its conditional delete to a claim that landed under it: %v, want the loss read as the race it is", session, err)
		}
		if _, deletes := counting.callsOn(takenDuring); deletes != 1 {
			t.Fatalf("the sweep made %d Deletes on %s, want the one it lost", deletes, takenDuring)
		}
		assertRoleHolder(t, reg, takenDuring, otherHolder)
		assertInterestMissing(t, reg, session)
	})
}

// Acceptance criterion A2, on both of the sweeps that now read every claim from one scan. The
// scan carries the revision each record was read at, and the release deletes at that revision
// and no other (nats.LastRevision), so a claim another session takes between the scan and the
// delete is kept: the delete loses its compare-and-swap, which is the answer, not a failure.
// Each case pairs the taken-over claim with one that did not change, which must be released, so
// a sweep that stopped deleting altogether could not pass either of them.
//
// What each turns on is the holder and the revision the bucket ends with, because those are
// what an unconditional delete would destroy: it would take the new holder's claim away and
// leave the role to nobody. Both run over the small fixture, which is all the race needs - the
// claim lands from beforeFirstDelete, under the sweep's own first delete, rather than from any
// timing of the bucket's size.
func TestTheSweepsThatReadFromOneScanKeepAClaimTakenUnderTheirDelete(t *testing.T) {
	const otherHolder = "ses_race_other_holder"

	// landClaimUnderTheFirstDelete returns the hook that writes otherHolder's claim on role when
	// the sweep's first delete reaches the bucket, and the function that reads back the revision
	// it wrote. The revision is guarded because the hook runs on the goroutine bus.Bounded put
	// the delete on and the case reads it from its own.
	landClaimUnderTheFirstDelete := func(t *testing.T, raw natsgo.KeyValue, role string) (func(key string), func() uint64) {
		t.Helper()
		var (
			mu       sync.Mutex
			revision uint64
		)
		hook := func(key string) {
			if key != role {
				t.Errorf("the sweep's first delete fell on %s, want %s, the role the claim lands on", key, role)
				return
			}
			// Written through the bucket's own handle, so the claim the case lands is not
			// counted among the sweep's calls.
			landed, err := raw.Put(key, []byte(`{"holder_session_id":"`+otherHolder+`"}`))
			if err != nil {
				t.Errorf("land %s's claim on %s under the sweep's delete: %v", otherHolder, key, err)
				return
			}
			mu.Lock()
			revision = landed
			mu.Unlock()
		}
		return hook, func() uint64 {
			mu.Lock()
			defer mu.Unlock()
			return revision
		}
	}

	// assertTheNewHoldersClaimStands fails the test unless the bucket holds role for otherHolder
	// at the very revision that holder wrote. Reading both is the point: a delete that named no
	// revision would have deleted the claim, and one that deleted and let the holder write again
	// would leave another revision.
	assertTheNewHoldersClaimStands := func(t *testing.T, raw natsgo.KeyValue, role string, want uint64) {
		t.Helper()
		entry, err := raw.Get(role)
		if err != nil {
			t.Fatalf("read %s's claim after the sweep: %v, want the claim %s took under the delete", role, err, otherHolder)
		}
		claim, err := DecodeRoleClaim(entry.Value())
		if err != nil {
			t.Fatalf("decode %s's claim after the sweep: %v", role, err)
		}
		if claim.HolderSessionID != otherHolder {
			t.Fatalf("%s is held by %q after the sweep, want the %s that took it under the delete", role, claim.HolderSessionID, otherHolder)
		}
		if entry.Revision() != want {
			t.Fatalf("%s's claim is at revision %d after the sweep, want the %d %s wrote", role, entry.Revision(), want, otherHolder)
		}
	}

	t.Run("the role reaper keeps an expired claim another session took under its delete and reaps the one that did not change", func(t *testing.T) {
		const expired = "ses_reaper_race_expired"
		// The taken-over role sorts first, so the sweep's first delete - the one the hook runs
		// before - is the one the new claim lands under.
		taken, unchanged := "reap-race-a-taken-over", "reap-race-b-unchanged"
		reg, rawRoles, _ := smallRoleBucketRegistry(t, expired, []string{taken, unchanged})
		hook, takenRevision := landClaimUnderTheFirstDelete(t, rawRoles, taken)
		counting := countRoleBucketCalls(t, reg, &sweepRoleKeyValue{beforeFirstDelete: hook})

		sweep, err := reg.ReapRoleClaims(context.Background(), func(holder string) bool { return holder != expired }, 0)
		if err != nil {
			t.Fatalf("a reaper pass that lost one conditional delete to a claim landing under it: %v, want the loss read as the race it is", err)
		}
		// One reaped, and the claim the pass did not delete is not among them: the release
		// reported it superseded, which is neither a reap nor a failure to skip.
		if want := (RoleClaimSweep{Reaped: 1}); sweep != want {
			t.Fatalf("the pass = %+v, want %+v: %s reaped and %s left to the session that took it", sweep, want, unchanged, taken)
		}
		if _, deletes := counting.callsOn(taken); deletes != 1 {
			t.Fatalf("the pass made %d Deletes on %s, want the one it lost", deletes, taken)
		}
		assertTheNewHoldersClaimStands(t, rawRoles, taken, takenRevision())
		assertRoleHolder(t, reg, unchanged, "")
	})

	t.Run("the deregistration's fallback keeps a claim another session took under its delete and releases the one that did not change", func(t *testing.T) {
		const session = "ses_sweep_race_held"
		// Both sort before the other sessions' claims, and the taken-over role before the
		// other, so the sweep's first delete is the one the new claim lands under.
		taken, unchanged := "fallback-race-a-taken-over", "fallback-race-b-unchanged"
		reg, rawRoles, _ := smallRoleBucketRegistry(t, session, []string{taken, unchanged})
		hook, takenRevision := landClaimUnderTheFirstDelete(t, rawRoles, taken)
		countRoleBucketCalls(t, reg, &sweepRoleKeyValue{beforeFirstDelete: hook})

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := reg.Remove(ctx, session, nil); err != nil {
			t.Fatalf("Remove(%s) lost its conditional delete to a claim that landed under it: %v, want the loss read as the race it is", session, err)
		}
		assertTheNewHoldersClaimStands(t, rawRoles, taken, takenRevision())
		assertRoleHolder(t, reg, unchanged, "")
		assertInterestMissing(t, reg, session)
	})
}
