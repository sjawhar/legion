// A deregistration releases the role claims its session holds, and it used to find them by
// listing every key of the role bucket and reading each claim: one pass over the whole bucket and
// a round trip per key in it, per deregistration. At the fleet's role count that ran out the
// route's own budget (sendRequestBudget, 3.5 s), so DELETE /v1/interests/<session_id> answered the
// bus-unavailable 503 writeStoreError gives store work that did not answer, naming the read of
// some other session's claim. The sweep now reads the roles the session holds from the role claim
// view, which makes no bucket call.
//
// internal/store's role_claim_sweep_test.go proves that on the store's own surface. These two
// cases prove it where the 503 was measured: the route. They drive both real callers of
// store.Registry.Remove with no topics - DELETE /v1/interests/<session_id> (adminInterestsHandler)
// and POST /v1/interests/unsubscribe with an empty topic list, which
// packages/claude-envoy/scripts/smoke-clear-rebind.sh uses - over a role bucket holding about
// 5,000 claims held by other sessions, through the listener's own mux.
//
// Each case's session holds two of its own claims, one per way a deregistration can find a claim:
// the role its interest record names, which POST /v1/roles/set leaves there and the pass over the
// record releases (releaseRoleClaims), and a role the record does not name, which only the claim
// view can name (releaseAllRoleClaims) - the path this change rewrote. A case written with the
// record's role alone would pass with no view sweep at all.
//
// A third case drives the DELETE over a role bucket of about 20,000 claims with the role claim
// view held where it cannot be served, which is the state a listener warming up, or one whose
// copy has fallen past its stale bound, serves deregistrations in. The sweep then reads no view
// at all: it falls back to a scan of the whole role bucket
// (store.Registry.releaseAllRoleClaimsByScan), which this change made release each claim from the
// record that scan carried rather than read each key back. That is the path the issue's
// acceptance criterion A3 is about, and it is driven here through the same mux, over the bucket
// size the per-key read was measured answering 503 at.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/store"
)

const (
	// sweepRouteOtherClaims is how many claims held by sessions other than the ones under test
	// the role bucket holds while these routes are driven. It is the bucket size a deregistration
	// must not pay for, and it is the order of magnitude the issue was filed at.
	sweepRouteOtherClaims = 5000
	// sweepRouteDegradedOtherClaims is sweepRouteOtherClaims for the case that holds the role
	// claim view where it cannot be served. It is 20,000 because that is the count the per-key
	// read was measured at: over a bucket of that size every deregistration ran the fallback's
	// read per key, and DELETE /v1/interests/<session_id> answered 503 after 3.5 s, the route's
	// own budget. A smaller bucket would answer inside that budget with the read per key still
	// in place, so it would not stand against the measurement this case is here to beat.
	sweepRouteDegradedOtherClaims = 20000
	// sweepRouteClaimsHeld is how many claims each case's own session holds: the role its
	// interest record names and the role only the claim view names.
	sweepRouteClaimsHeld = 2
	// sweepRouteCallDelay is what sweepRouteRoleKV adds to every role bucket Get and Delete. It
	// stands in for the round trip a deployed bucket's call pays and an in-process server does
	// not: a sweep that calls the bucket once per key cannot answer 5,004 keys inside the route's
	// 3.5 s budget, and one that calls it twice per role held answers in four of these delays.
	// The degraded case below reads the same delay the other way round: its scan sends no call
	// per key, so it pays two of these delays over 20,004 claims, where a read per key would pay
	// 20,004 of them and answer nothing before the budget ends.
	sweepRouteCallDelay = 10 * time.Millisecond
)

// sweepRouteRoleKV is the role bucket's legacy handle with every Get and Delete counted, per key
// and in total, and each delayed by sweepRouteCallDelay. It is how these cases read what a
// request cost the bucket: the delay is what turns the old sweep's cost into the route's 503, and
// the counts say exactly which keys the request touched, so a sweep that read another session's
// claim is a failure here rather than a slow route.
//
// The counters are the only state a call writes and they are guarded, because the calls run on
// the goroutines bus.Bounded makes while the test reads the counts on its own, which -race would
// otherwise report.
type sweepRouteRoleKV struct {
	natsgo.KeyValue

	mu          sync.Mutex
	gets        map[string]int
	deletes     map[string]int
	getTotal    int
	deleteTotal int
}

func (kv *sweepRouteRoleKV) Get(key string) (natsgo.KeyValueEntry, error) {
	kv.mu.Lock()
	kv.gets[key]++
	kv.getTotal++
	kv.mu.Unlock()
	time.Sleep(sweepRouteCallDelay)
	return kv.KeyValue.Get(key)
}

func (kv *sweepRouteRoleKV) Delete(key string, opts ...natsgo.DeleteOpt) error {
	kv.mu.Lock()
	kv.deletes[key]++
	kv.deleteTotal++
	kv.mu.Unlock()
	time.Sleep(sweepRouteCallDelay)
	return kv.KeyValue.Delete(key, opts...)
}

// totals is how many Gets and Deletes the role bucket has taken through this handle.
func (kv *sweepRouteRoleKV) totals() (gets, deletes int) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return kv.getTotal, kv.deleteTotal
}

// callsOn is how many Gets and Deletes key has taken through this handle.
func (kv *sweepRouteRoleKV) callsOn(key string) (gets, deletes int) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return kv.gets[key], kv.deletes[key]
}

// delayRoleBucketCalls puts a counting, delaying handle over registry's role bucket for the length
// of one case and puts the bucket's own handle back when that case ends, so the case that follows
// counts only its own request's calls. The swap is role_kv_reach_test.go's, the way this package
// already reaches the registry's role handle; the seeding below runs through the handle taken
// here, before the counting one is installed, so no fixture write is counted as a request's.
func delayRoleBucketCalls(t *testing.T, registry *store.Registry) *sweepRouteRoleKV {
	t.Helper()
	base := registryRoleKV(t, registry)
	handle := &sweepRouteRoleKV{KeyValue: base, gets: map[string]int{}, deletes: map[string]int{}}
	setRegistryRoleKV(t, registry, handle)
	t.Cleanup(func() { setRegistryRoleKV(t, registry, base) })
	return handle
}

// putSweepRouteRoleClaim stores role's claim naming holder, as a claim POST /v1/roles/set wrote
// reads from the bucket. It returns its error rather than failing the test, because the pool below
// calls it from goroutines of its own.
func putSweepRouteRoleClaim(kv natsgo.KeyValue, role, holder string) error {
	value, err := json.Marshal(store.RoleClaim{HolderSessionID: holder, ClaimedAt: time.Now().UnixMilli()})
	if err != nil {
		return err
	}
	_, err = kv.Put(role, value)
	return err
}

// seedOtherSessionsRoleClaims stores count claims straight into the role bucket, each role held by
// a session of its own and none of them a session under test: the claims a deregistration must not
// read. The puts run from a bounded pool of goroutines for the clock alone - 5,002 sequential puts
// measured 3.61 s in this pod - and the first failure any of them met fails the test on the test's
// own goroutine.
func seedOtherSessionsRoleClaims(t *testing.T, kv natsgo.KeyValue, count int) {
	t.Helper()
	const workers = 32
	pending := make(chan string)
	var (
		pool         sync.WaitGroup
		failed       sync.Once
		firstFailure error
	)
	for range workers {
		pool.Add(1)
		go func() {
			defer pool.Done()
			for role := range pending {
				if err := putSweepRouteRoleClaim(kv, role, "ses_holder_of_"+role); err != nil {
					failed.Do(func() { firstFailure = err })
				}
			}
		}()
	}
	for i := range count {
		pending <- fmt.Sprintf("sweep-route-other-%05d", i)
	}
	close(pending)
	pool.Wait()
	if firstFailure != nil {
		t.Fatalf("seed the role bucket with %d claims held by other sessions: %v", count, firstFailure)
	}
}

// Both deregistration routes answer inside the route's own budget over a role bucket of about
// 5,000 claims held by other sessions, and release the two claims their session holds.
//
// What this case turns on is the deadline: every role bucket Get and Delete takes
// sweepRouteCallDelay, so the pass over every key of the bucket that the sweep used to make cannot
// answer inside sendRequestBudget and the route answers the bus-unavailable 503 rather than its
// 204 or 200 - which is what the negative control, this file's own test run against the commit
// before the fix, reports. The call counts are asserted beside the deadline, and say the same
// thing more sharply: two Gets and two Deletes, one of each on each role the session holds and
// nothing at all on any other key, so no other session's claim was so much as read.
//
// The fixture is built once and the two cases share it, because 5,004 claims cost too much to seed
// twice. They run in order, not in parallel, since each installs a counting handle over the one
// role bucket both use. The claims are seeded after store.Open through the registry's own role
// bucket handle, so the case waits until the claim view names every one of them (RolesBySession
// counts 5,004 across all holders, each case's session holding its two) before it drives a route:
// a sweep reading a view that had not seen them would prove nothing about a bucket that holds
// them.
func TestBothDeregistrationRoutesAnswerInsideTheRouteBudgetOverARoleBucketOfFiveThousandOtherSessionsClaims(t *testing.T) {
	const topic = "notifications.team.sweep-route"
	cases := []struct {
		name         string
		sessionID    string
		onRecordRole string
		viewOnlyRole string
		method       string
		path         string
		body         string
		wantStatus   int
		// wantBody is the answer's body with its trailing newline trimmed, empty where the answer
		// carries none.
		wantBody string
	}{
		{
			// The route the 503 was measured on: adminInterestsHandler's Remove(ctx, id, nil).
			name:         "DELETE of a session's interests",
			sessionID:    "ses_sweep_route_delete",
			onRecordRole: "sweep-route-delete-on-record",
			viewOnlyRole: "sweep-route-delete-view-only",
			method:       http.MethodDelete,
			path:         "/v1/interests/ses_sweep_route_delete",
			wantStatus:   http.StatusNoContent,
			wantBody:     "",
		},
		{
			// The other caller of Remove with no topics, which smoke-clear-rebind.sh drives: an
			// unsubscribe whose request names no topic clears the session's interests and its role
			// claims exactly as the DELETE does, and answers the topics it removed - none.
			name:         "POST /v1/interests/unsubscribe with an empty topic list",
			sessionID:    "ses_sweep_route_unsubscribe",
			onRecordRole: "sweep-route-unsubscribe-on-record",
			viewOnlyRole: "sweep-route-unsubscribe-view-only",
			method:       http.MethodPost,
			path:         "/v1/interests/unsubscribe",
			body:         `{"session_id":"ses_sweep_route_unsubscribe"}`,
			wantStatus:   http.StatusOK,
			wantBody:     `{"removed":[]}`,
		},
	}

	interests := map[string][]string{}
	live := make([]string, 0, len(cases))
	for _, testCase := range cases {
		interests[testCase.sessionID] = []string{topic}
		live = append(live, testCase.sessionID)
	}
	registry, sessions := setupSessionsTest(t, interests, live)
	mux := http.NewServeMux()
	// The routes are registered as main registers them, so each request goes through the mux and
	// the handler the listener serves rather than a handler built for the test. consumers is nil,
	// as it is in every test that builds the deps without session delivery: the unsubscribe
	// route's refollow answers nothing for a session with no consumer.
	registerV1Routes(mux, &listenerDeps{registry: registry, sessions: sessions}, "test-machine", logging.New("test"))

	roleKV := registryRoleKV(t, registry)
	for _, testCase := range cases {
		claimRoles(t, registry, testCase.sessionID, testCase.onRecordRole)
		// The view-only claim is written straight to the bucket, so the session's interest record
		// never names this role and the pass over that record cannot release it: the claim view is
		// the only place the deregistration can learn the session holds it.
		if err := putSweepRouteRoleClaim(roleKV, testCase.viewOnlyRole, testCase.sessionID); err != nil {
			t.Fatalf("seed %s's claim on %s: %v", testCase.sessionID, testCase.viewOnlyRole, err)
		}
	}
	seedingStarted := time.Now()
	seedOtherSessionsRoleClaims(t, roleKV, sweepRouteOtherClaims)
	t.Logf("seeded %d claims held by other sessions in %s", sweepRouteOtherClaims, time.Since(seedingStarted))

	wantClaims := sweepRouteOtherClaims + sweepRouteClaimsHeld*len(cases)
	viewStarted := time.Now()
	waitFor(t, time.Minute, "the role claim view to name every claim in the bucket", func() bool {
		held, err := registry.RolesBySession()
		if err != nil {
			return false
		}
		named := 0
		for _, roles := range held {
			named += len(roles)
		}
		if named != wantClaims {
			return false
		}
		for _, testCase := range cases {
			if len(held[testCase.sessionID]) != sweepRouteClaimsHeld {
				return false
			}
		}
		return true
	})
	t.Logf("the role claim view names all %d claims, %d of them held by a session under test, %s after the seeding finished",
		wantClaims, sweepRouteClaimsHeld*len(cases), time.Since(viewStarted))

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			handle := delayRoleBucketCalls(t, registry)

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(testCase.method, testCase.path, strings.NewReader(testCase.body))
			started := time.Now()
			mux.ServeHTTP(recorder, request)
			elapsed := time.Since(started)
			// The counts are read before anything else this case does touches the bucket, so they
			// are the request's own calls and no later read's.
			gets, deletes := handle.totals()
			body := strings.TrimSpace(recorder.Body.String())
			t.Logf("%s %s answered %d in %s after %d Gets and %d Deletes on a role bucket of %d claims; body %q",
				testCase.method, testCase.path, recorder.Code, elapsed, gets, deletes, wantClaims, body)

			if elapsed >= sendRequestBudget {
				t.Fatalf("%s %s answered %d after %s, outside the route's own budget of %s: a deregistration must cost the roles its session holds, not the %d claims in the bucket",
					testCase.method, testCase.path, recorder.Code, elapsed, sendRequestBudget, wantClaims)
			}
			if recorder.Code != testCase.wantStatus {
				t.Fatalf("%s %s: status %d, body %s; want %d", testCase.method, testCase.path, recorder.Code, recorder.Body.String(), testCase.wantStatus)
			}
			if body != testCase.wantBody {
				t.Fatalf("%s %s answered body %q, want %q", testCase.method, testCase.path, body, testCase.wantBody)
			}
			if gets != sweepRouteClaimsHeld || deletes != sweepRouteClaimsHeld {
				t.Fatalf("%s %s made %d Gets and %d Deletes on a role bucket of %d claims; want %d of each, one per role the session holds",
					testCase.method, testCase.path, gets, deletes, wantClaims, sweepRouteClaimsHeld)
			}
			for _, role := range []string{testCase.onRecordRole, testCase.viewOnlyRole} {
				if roleGets, roleDeletes := handle.callsOn(role); roleGets != 1 || roleDeletes != 1 {
					t.Fatalf("role %s, which %s holds, took %d Gets and %d Deletes; want one of each",
						role, testCase.sessionID, roleGets, roleDeletes)
				}
				// The claim itself, read from the bucket: the role is nobody's now, so the next
				// claimant can take it. A sweep that answered 204 and left the claim behind is the
				// leak the deregistration exists to prevent.
				holder, err := registry.RoleHolder(role)
				if err != nil {
					t.Fatalf("read %s's holder after the deregistration: %v", role, err)
				}
				if holder != "" {
					t.Fatalf("%s is still held by %q after %s deregistered; want it unclaimed", role, holder, testCase.sessionID)
				}
			}
			// The interests are gone with the claims: a request naming no topic removes the whole
			// record, which the route reports as the 404 a session with no record earns.
			readBack := httptest.NewRecorder()
			mux.ServeHTTP(readBack, httptest.NewRequest(http.MethodGet, "/v1/interests/"+testCase.sessionID, nil))
			if readBack.Code != http.StatusNotFound {
				t.Fatalf("GET /v1/interests/%s after the deregistration: status %d, body %s; want 404, the record removed",
					testCase.sessionID, readBack.Code, readBack.Body.String())
			}
		})
	}
}

// Acceptance criterion A3. A deregistration whose role claim view cannot be served answers inside
// the route's own budget over a role bucket of about 20,000 claims, and releases the two claims
// its session holds.
//
// This is the degraded path, the one the issue's measurement was taken on. While that view cannot
// be served - a listener warming up, or a copy past its stale bound - every deregistration takes
// store.Registry.releaseAllRoleClaimsByScan, and over a bucket of this size the read that
// fallback made per key answered DELETE /v1/interests/<session_id> 503 after 3.5 s, the route's
// own budget. The fallback now reads every claim's value and revision from the one scan it
// already makes, so past that scan it sends nothing but the deletes it owes, and the same
// request answers its 204 inside the same budget.
//
// What holds the view unready is refuseRoleClaimView: it stops the watcher that feeds the view and
// puts in its place one that was never started, which has completed no read of its bucket and so
// cannot be served (kvwatch.Watcher.Servable). Nothing here starts it, so
// store.Registry.RoleViewErr refuses from before the request until the case ends, and the case
// asserts that refusal before it drives the route, as internal/store's own fallback cases do: a
// view that answered would take the view sweep instead and this case would prove nothing about
// the scan. The view is left to name every seeded claim first and only then refused, so the
// fallback is taken for that refusal alone, over a view that had seen the whole bucket rather
// than one still filling.
//
// The call counts are the sharp evidence beside the clock, as they are above: one Get and one
// Delete on the role the session's interest record names, which the pass over that record
// releases before the sweep (releaseRoleClaims); one Delete and no Get on the role the record
// does not name, which only a read of the bucket can find and whose claim and revision the scan
// carries; and no call of either kind on the 20,000 claims other sessions hold, which is also
// what says those claims are still theirs. Two deletes in all is the whole of what this
// deregistration sent the bucket past its scan. A read per key put back sends a Get of its own
// for every claim the scan delivered, each delayed by sweepRouteCallDelay, so it is measured
// here as that 503 at the deadline, naming some other session's claim, rather than by a count
// alone.
//
// The fixture is seeded once for the whole function, as the one above is: seeding 20,000 claims
// measured about 1.2 s in this pod, and the acceptance check runs this with -count=20, which
// seeds them twenty times over, about 24 s of the run. The seeding runs through the registry's
// own role bucket handle before the counting one is installed, so no fixture write is counted as
// the request's.
func TestADeregistrationOnARoleClaimViewThatCannotBeServedAnswersInsideTheRouteBudgetOverTwentyThousandClaims(t *testing.T) {
	const (
		sessionID    = "ses_sweep_route_degraded"
		topic        = "notifications.team.sweep-route-degraded"
		onRecordRole = "sweep-route-degraded-on-record"
		scanOnlyRole = "sweep-route-degraded-scan-only"
	)
	registry, sessions := setupSessionsTest(t, map[string][]string{sessionID: {topic}}, []string{sessionID})
	mux := http.NewServeMux()
	// The route is registered as main registers it, so the request goes through the mux and the
	// handler the listener serves rather than one built for the test.
	registerV1Routes(mux, &listenerDeps{registry: registry, sessions: sessions}, "test-machine", logging.New("test"))

	roleKV := registryRoleKV(t, registry)
	claimRoles(t, registry, sessionID, onRecordRole)
	// The second claim is written straight to the bucket, so the session's interest record never
	// names this role and the pass over that record cannot release it: the scan is the only place
	// this deregistration can learn the session holds it.
	if err := putSweepRouteRoleClaim(roleKV, scanOnlyRole, sessionID); err != nil {
		t.Fatalf("seed %s's claim on %s: %v", sessionID, scanOnlyRole, err)
	}
	seedingStarted := time.Now()
	seedOtherSessionsRoleClaims(t, roleKV, sweepRouteDegradedOtherClaims)
	t.Logf("seeded %d claims held by other sessions in %s", sweepRouteDegradedOtherClaims, time.Since(seedingStarted))

	wantClaims := sweepRouteDegradedOtherClaims + sweepRouteClaimsHeld
	viewStarted := time.Now()
	waitFor(t, time.Minute, "the role claim view to name every claim in the bucket", func() bool {
		held, err := registry.RolesBySession()
		if err != nil {
			return false
		}
		named := 0
		for _, roles := range held {
			named += len(roles)
		}
		return named == wantClaims && len(held[sessionID]) == sweepRouteClaimsHeld
	})
	t.Logf("the role claim view names all %d claims, %d of them held by the session under test, %s after the seeding finished",
		wantClaims, sweepRouteClaimsHeld, time.Since(viewStarted))

	// From here the view cannot be served, and nothing in this case makes it servable again.
	refuseRoleClaimView(t, registry)
	viewErr := registry.RoleViewErr()
	if viewErr == nil {
		t.Fatal("the role claim view can still be served, so the deregistration would take the view sweep and this case would prove nothing about the fallback")
	}
	if !errors.Is(viewErr, store.ErrRoleViewNotReady) {
		t.Fatalf("the role claim view refuses with %v, want ErrRoleViewNotReady: the fallback runs on that refusal and no other", viewErr)
	}
	if _, err := registry.RolesHeldBy(sessionID); !errors.Is(err, store.ErrRoleViewNotReady) {
		t.Fatalf("the view answered %s's roles with %v, want ErrRoleViewNotReady: the sweep reads the roles from here", sessionID, err)
	}

	handle := delayRoleBucketCalls(t, registry)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodDelete, "/v1/interests/"+sessionID, nil)
	started := time.Now()
	mux.ServeHTTP(recorder, request)
	elapsed := time.Since(started)
	// The counts are read before anything else this case does touches the bucket, so they are the
	// request's own calls and no later read's.
	gets, deletes := handle.totals()
	t.Logf("DELETE /v1/interests/%s on a role claim view that cannot be served answered %d in %s after %d Gets and %d Deletes on a role bucket of %d claims; body %q",
		sessionID, recorder.Code, elapsed, gets, deletes, wantClaims, strings.TrimSpace(recorder.Body.String()))

	if elapsed >= sendRequestBudget {
		t.Fatalf("DELETE /v1/interests/%s answered %d after %s, outside the route's own budget of %s: the fallback must cost one scan of the bucket, not a read of each of its %d claims",
			sessionID, recorder.Code, elapsed, sendRequestBudget, wantClaims)
	}
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("DELETE /v1/interests/%s: status %d, body %s; want 204", sessionID, recorder.Code, recorder.Body.String())
	}
	if gets != 1 || deletes != sweepRouteClaimsHeld {
		t.Fatalf("DELETE /v1/interests/%s made %d Gets and %d Deletes on a role bucket of %d claims; want the one Get the record's own role costs and %d Deletes, one per claim the session holds",
			sessionID, gets, deletes, wantClaims, sweepRouteClaimsHeld)
	}
	for _, call := range []struct {
		role              string
		wantGets, wantDel int
		how               string
	}{
		{onRecordRole, 1, 1, "the pass over the session's interest record reads the claim and deletes it"},
		{scanOnlyRole, 0, 1, "the scan carries the claim, so the fallback sends the delete alone"},
	} {
		if roleGets, roleDeletes := handle.callsOn(call.role); roleGets != call.wantGets || roleDeletes != call.wantDel {
			t.Fatalf("role %s, which %s holds, took %d Gets and %d Deletes; want %d and %d: %s",
				call.role, sessionID, roleGets, roleDeletes, call.wantGets, call.wantDel, call.how)
		}
		// The claim itself, read from the bucket: the role is nobody's now, so the next claimant
		// can take it. A fallback that answered 204 and left the claim behind is the leak the
		// deregistration exists to prevent.
		holder, err := registry.RoleHolder(call.role)
		if err != nil {
			t.Fatalf("read %s's holder after the deregistration: %v", call.role, err)
		}
		if holder != "" {
			t.Fatalf("%s is still held by %q after %s deregistered; want it unclaimed", call.role, holder, sessionID)
		}
	}
	// The other sessions' claims are still theirs. The two deletes above already say the bucket
	// took no other delete, and these read three of the seeded claims back - the first, one in the
	// middle and the last - through the handle the seeding used, so a scan that released what it
	// walked over would be caught at the record rather than at the count.
	for _, i := range []int{0, sweepRouteDegradedOtherClaims / 2, sweepRouteDegradedOtherClaims - 1} {
		role := fmt.Sprintf("sweep-route-other-%05d", i)
		entry, err := roleKV.Get(role)
		if err != nil {
			t.Fatalf("read %s, held by another session, after the deregistration: %v", role, err)
		}
		claim, err := store.DecodeRoleClaim(entry.Value())
		if err != nil {
			t.Fatalf("decode %s's claim after the deregistration: %v", role, err)
		}
		if claim.HolderSessionID != "ses_holder_of_"+role {
			t.Fatalf("%s is held by %q after %s deregistered; want its own holder %q",
				role, claim.HolderSessionID, sessionID, "ses_holder_of_"+role)
		}
	}
	// The interests are gone with the claims: a request naming no topic removes the whole record,
	// which the route reports as the 404 a session with no record earns.
	readBack := httptest.NewRecorder()
	mux.ServeHTTP(readBack, httptest.NewRequest(http.MethodGet, "/v1/interests/"+sessionID, nil))
	if readBack.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/interests/%s after the deregistration: status %d, body %s; want 404, the record removed",
			sessionID, readBack.Code, readBack.Body.String())
	}
}
