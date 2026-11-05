// packages/envoy/internal/broker/helper/session_identity_gate_test.go
//go:build linux

// The three defects a fresh-reviewer round found in the first cut of a session identity that
// outlives its process (dispatch://INFRA-1167). Each is a way the durable record — which now
// holds a private key and an enrollment that carries a person's grants — could be handed to the
// wrong process or lost outright:
//
//  1. Recover matched a record to a live process by pid and start ticks alone. Both are counted
//     from boot and both repeat after one, so after a reboot an UNRELATED process could match a
//     record exactly and be re-pinned with its key and its enrollment, without ever knowing the
//     session id the whole model rests on.
//  2. Name ownership was decided in one lock and taken in another, so two roots could both find
//     a name free and both take it; and retire left the live set before entering the dormant
//     one, so a process coming back in between found neither and stranded the old identity.
//  3. The dormant record was consumed before the registration that wanted it committed, so a
//     register that then failed — draining, a peer whose pid changed, a lost ancestry race —
//     destroyed the identity, and the enrollment id was dropped even when it succeeded.
package helper

import (
	"context"
	"crypto/ecdsa"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
)

// otherBootID is a boot id no running kernel has: any well-formed UUID that is not this boot's.
const otherBootID = "00000000-0000-4000-8000-000000000000"

// rootingParent makes every pid its own root for the registry's ancestry walks: each walk takes
// one hop to pid 1 and stops, so two pids are never in each other's trees. It lets a test race
// two independent roots without spawning processes.
func rootingParent(int) (int, error) { return 1, nil }

// namedSession is a live session named sessionID around key, as a register or a Recover would
// build it.
func namedSession(t *testing.T, pid int, sessionID string, key *ecdsa.PrivateKey) *Session {
	t.Helper()
	sess, err := newSession(pid, 1, SessionRuntimeID(sessionID), sessionID, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

// dormantRecordFor is the record a retired named session leaves behind, with key and enrollment.
func dormantRecordFor(t *testing.T, pid int, sessionID, enrollmentID string, key *ecdsa.PrivateKey) Record {
	t.Helper()
	gone := time.Now()
	return Record{PID: pid, StartTicks: 1, BootID: otherBootID, RuntimeID: SessionRuntimeID(sessionID),
		EnrollmentID: enrollmentID, SessionID: sessionID, Key: encodeKey(key), GoneAt: &gone}
}

// TestOnlyARecordOfThisBootRePinsItsProcess is the regression for defect 1. The three cases
// differ in exactly one field: a record of THIS boot re-pins the live process it names, and a
// record whose pid and start ticks match that very process just as exactly — but which names
// another boot, or no boot at all — must not, however alive that process is.
func TestOnlyARecordOfThisBootRePinsItsProcess(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bootID   func(this string) string
		rePinned bool
	}{
		{"this boot", func(this string) string { return this }, true},
		{"another boot", func(string) string { return otherBootID }, false},
		{"no boot recorded", func(string) string { return "" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "sessions.json")
			// A process that IS alive, recorded with the pid and start ticks it really has:
			// after a reboot, matching those two is all an unrelated process can do.
			live := sleeper(t)
			pid := live.Process.Pid
			ticks, err := StartTicks(pid)
			if err != nil {
				t.Fatal(err)
			}
			key := recordNamed(t, state, pid, ticks, testSessionID, "enr-before")
			recs, err := LoadRecords(state)
			if err != nil {
				t.Fatal(err)
			}
			if len(recs) != 1 {
				t.Fatalf("one record expected, got %+v", recs)
			}
			if recs[0].BootID == "" {
				t.Fatal("a saved record must name the boot it was written on")
			}
			recs[0].BootID = tc.bootID(recs[0].BootID)
			writeRecords(t, state, recs)

			r := startRig(t, state) // startRig runs Recover
			seedEnrollment(r.fake, "enr-before", thumbprintOf(t, key), SessionRuntimeID(testSessionID))
			sess := r.srv.Registry.Get(pid)
			if tc.rePinned {
				if sess == nil {
					t.Fatal("a record of this boot must re-pin the process it names")
				}
				return
			}
			if sess != nil {
				t.Fatal("a record from another boot must never re-pin a process that happens to match its pid and start ticks: that process would be handed the recorded key and enrollment without knowing the session id")
			}
			if _, ok := r.srv.Registry.Dormant(testSessionID); !ok {
				t.Fatal("the identity must be parked for the session itself to reclaim, not re-pinned and not revoked")
			}
		})
	}
}

// TestTwoRootsRacingForOneNameLeaveOneWinner is the regression for the first half of defect 2.
// Two unrelated roots ask for one omp session id at the same moment; exactly one may be
// admitted, and the identity kept for that name may be consumed exactly once. Decided in two
// locks — LiveSessionID, then an insert — both would pass the check and both would sign as the
// session.
func TestTwoRootsRacingForOneNameLeaveOneWinner(t *testing.T) {
	for range 100 {
		reg := NewRegistry(filepath.Join(t.TempDir(), "sessions.json"))
		reg.parent = rootingParent
		key, err := proof.NewKey()
		if err != nil {
			t.Fatal(err)
		}
		reg.Park(dormantRecordFor(t, 1000, testSessionID, "enr-before", key))

		var wins, took atomic.Int64
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range 2 {
			pid := 2000 + i
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				adm := reg.AdmitRoot(pid, testSessionID, func(rec Record, kept bool) (*Session, error) {
					var recovered *ecdsa.PrivateKey
					if kept {
						recovered, _ = rec.PrivateKey()
					}
					return newSession(pid, 1, SessionRuntimeID(testSessionID), testSessionID, recovered, nil)
				}, nil)
				if adm.Won {
					wins.Add(1)
				}
				if adm.Took {
					took.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()

		if wins.Load() != 1 || took.Load() != 1 {
			t.Fatalf("two roots racing for one name: %d admitted and %d took the record, want 1 and 1", wins.Load(), took.Load())
		}
		if live := reg.List(); len(live) != 1 {
			t.Fatalf("%d live sessions hold one name", len(live))
		}
		if _, ok := reg.Dormant(testSessionID); ok {
			t.Fatal("the record must be consumed by the one winner, not left for a second taker")
		}
	}
}

// TestAProcessComingBackWhileItsSessionRetiresFindsTheIdentity is the regression for the second
// half of defect 2. A session's process ends and the same session registers again at a new pid
// at that very moment. Whatever the interleaving, the returning process must find EITHER the
// live session still holding the name or the record kept for it — never neither, which is what
// a retire that removed the session before parking it allowed, and which would leave the
// returning process a fresh identity and the old one parked beside it where nothing could
// reclaim it and only the sweep could end it.
func TestAProcessComingBackWhileItsSessionRetiresFindsTheIdentity(t *testing.T) {
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	want := thumbprintOf(t, key)
	for range 200 {
		reg := NewRegistry(filepath.Join(t.TempDir(), "sessions.json"))
		reg.parent = rootingParent
		old := namedSession(t, 1000, testSessionID, key)
		old.setEnrolled("enr-before")
		reg.Add(old)

		var adm admission
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			reg.Retire(old, func(cur identity) (Record, bool) {
				gone := time.Now()
				return Record{PID: old.PID, StartTicks: old.StartTicks, RuntimeID: cur.RuntimeID,
					EnrollmentID: cur.EnrollmentID, SessionID: cur.SessionID, Key: encodeKey(cur.Key), GoneAt: &gone}, true
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			adm = reg.AdmitRoot(2000, testSessionID, func(rec Record, kept bool) (*Session, error) {
				var recovered *ecdsa.PrivateKey
				if kept {
					recovered, _ = rec.PrivateKey()
				}
				return newSession(2000, 1, SessionRuntimeID(testSessionID), testSessionID, recovered, nil)
			}, nil)
		}()
		close(start)
		wg.Wait()

		switch {
		case adm.Held:
			// The live session still held the name; the caller is told to retry, and the
			// retire that follows parks the identity for it.
		case adm.Won:
			if !adm.Took {
				t.Fatal("a process coming back after its session retired was admitted with NO record: the identity it left behind is stranded")
			}
			if got := adm.Session.identity().Thumbprint; got != want {
				t.Fatalf("the returning process signs with %s, want the key its own earlier incarnation used (%s)", got, want)
			}
			if adm.Record.EnrollmentID != "enr-before" {
				t.Fatalf("the reclaimed record carries enrollment %q, want enr-before", adm.Record.EnrollmentID)
			}
		default:
			t.Fatalf("a register that neither won nor found a holder: %+v", adm)
		}
	}
}

// TestAReclaimedIdentityKeepsItsEnrollmentID is the second half of defect 3. Taking the record
// back without its enrollment id, and then saving an empty one over it, would cost the session
// the very thing the record exists for: a helper restart between the register and the first
// enroll would find a record with no enrollment to keep or revoke.
//
// The rig is NOT logged in, so no enrollment can succeed and nothing can repair the record
// behind the assertions: what the session holds here is the inherited id and nothing else.
// That also pins the other half of the rule — an inherited id is not an enrolled one. The
// broker may have revoked it while the session was away, and only an enroll shows otherwise,
// so the reply reads NO_CREDENTIAL rather than claiming a grant the session may not have.
func TestAReclaimedIdentityKeepsItsEnrollmentID(t *testing.T) {
	state := filepath.Join(t.TempDir(), "sessions.json")
	key := parkedRecord(t, state)
	r := newRig(t, state)
	seedEnrollment(r.fake, "enr-before", thumbprintOf(t, key), SessionRuntimeID(testSessionID))
	if _, ok := r.srv.Registry.Dormant(testSessionID); !ok {
		t.Fatal("the record must be parked first")
	}
	id := testSessionID
	resp := r.call(t, Request{Op: "register", SessionID: &id})
	if resp.Code != CodeNoCredential {
		t.Fatalf("register on a helper that was never logged in got %+v, want NO_CREDENTIAL", resp)
	}
	if resp.State == "enrolled" {
		t.Fatalf("an inherited enrollment nothing has confirmed must not read enrolled: %+v", resp)
	}
	sess := r.srv.Registry.Get(os.Getpid())
	if sess == nil {
		t.Fatal("the resumed session must be registered")
	}
	if got := sess.EnrollmentID(); got != "enr-before" {
		t.Fatalf("the reclaimed session holds enrollment %q, want enr-before", got)
	}
	if got := sess.identity().Thumbprint; got != thumbprintOf(t, key) {
		t.Fatal("the reclaimed session must sign with the recorded key")
	}
	// What a restart in this window would read: the live record, which must carry the
	// enrollment the session took back rather than an empty one.
	if err := r.srv.Registry.Save(); err != nil {
		t.Fatal(err)
	}
	recs, err := LoadRecords(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].EnrollmentID != "enr-before" {
		t.Fatalf("the saved record is %+v, want one naming enr-before", recs)
	}
}

// TestANamedRecordWithNoEnrollmentIsStillParked: a named session saved before its first enroll
// landed — the helper holding no launcher credential, a resume interrupted — has a key the
// broker may already know and no enrollment id. Recover must park it like any other named
// record; dropping it for want of an id would leave an identity nothing could ever reclaim,
// which is the gap retire's own fix left behind in the recovery path.
func TestANamedRecordWithNoEnrollmentIsStillParked(t *testing.T) {
	state := filepath.Join(t.TempDir(), "sessions.json")
	gone := sleeper(t)
	gonePID := gone.Process.Pid
	_ = gone.Process.Kill()
	_, _ = gone.Process.Wait()
	key := recordNamed(t, state, gonePID, 1, testSessionID, "")
	recs, err := LoadRecords(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].EnrollmentID != "" || recs[0].Key == "" {
		t.Fatalf("the record must carry a key and no enrollment: %+v", recs)
	}

	r := startRig(t, state)
	rec, ok := r.srv.Registry.Dormant(testSessionID)
	if !ok {
		t.Fatal("a named record with no enrollment yet must be parked, not dropped")
	}
	kept, err := rec.PrivateKey()
	if err != nil || kept == nil || !kept.Equal(key) {
		t.Fatalf("the parked record must still carry the session's key (%v)", err)
	}
	// And the session coming back takes it: the broker's idempotent enroll answers that key
	// under that runtime id with whatever row it already has.
	id := testSessionID
	if resp := r.call(t, Request{Op: "register", WaitSeconds: 10, SessionID: &id}); !resp.OK {
		t.Fatalf("register: %+v", resp)
	}
	sess := r.srv.Registry.Get(os.Getpid())
	if sess == nil || sess.identity().Thumbprint != thumbprintOf(t, key) {
		t.Fatal("the session coming back must sign with the key its record kept")
	}
}

// TestARegisterThatFailsLeavesTheIdentityWhereItWas is the regression for the first half of
// defect 3: the record must be consumed where the session is inserted, and nowhere else. Each
// of these three registers fails AFTER the point the old code had already deleted the record,
// and each must leave the identity reclaimable by the session it belongs to.
func TestARegisterThatFailsLeavesTheIdentityWhereItWas(t *testing.T) {
	t.Run("a draining helper", func(t *testing.T) {
		state := filepath.Join(t.TempDir(), "sessions.json")
		key := parkedRecord(t, state)
		r := startRig(t, state)
		seedEnrollment(r.fake, "enr-before", thumbprintOf(t, key), SessionRuntimeID(testSessionID))
		if d := r.call(t, Request{Op: "drain"}); !d.OK || !r.srv.Registry.Draining() {
			t.Fatalf("drain: %+v", d)
		}
		id := testSessionID
		resp := r.call(t, Request{Op: "register", WaitSeconds: 1, SessionID: &id})
		if resp.OK || resp.Code != CodeDraining {
			t.Fatalf("a register on a draining helper got %+v, want DRAINING", resp)
		}
		requireReclaimable(t, r, key)
	})

	t.Run("a peer whose pid changed", func(t *testing.T) {
		state := filepath.Join(t.TempDir(), "sessions.json")
		key := parkedRecord(t, state)
		r := startRig(t, state)
		seedEnrollment(r.fake, "enr-before", thumbprintOf(t, key), SessionRuntimeID(testSessionID))
		// register resolves and admits the victim pid, then re-reads the peer and finds it
		// names another process: the admission is undone, and the record must go back with it.
		victim := sleeper(t)
		impostor := sleeper(t)
		peer, err := PinPID(impostor.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		resp := r.srv.register(context.Background(), peer, victim.Process.Pid, testSessionID, 0)
		if resp.OK || resp.Code != CodeUnidentified {
			t.Fatalf("a peer no longer naming the admitted pid got %+v, want UNIDENTIFIED", resp)
		}
		if r.srv.Registry.Get(victim.Process.Pid) != nil {
			t.Fatal("a refused mismatch must not leave a ghost session in the registry")
		}
		requireReclaimable(t, r, key)
	})

	t.Run("a lost ancestry race", func(t *testing.T) {
		reg := NewRegistry(filepath.Join(t.TempDir(), "sessions.json"))
		key, err := proof.NewKey()
		if err != nil {
			t.Fatal(err)
		}
		reg.Park(dormantRecordFor(t, 1000, testSessionID, "enr-before", key))
		// A root that already owns the pid's tree: the registration loses the race and gets
		// that session's reply instead, having built nothing and taken nothing.
		root := namedSession(t, 3000, "other-session", nil)
		reg.Add(root)
		reg.parent = func(pid int) (int, error) {
			if pid == 3001 {
				return 3000, nil
			}
			return 1, nil
		}
		built := false
		adm := reg.AdmitRoot(3001, testSessionID, func(Record, bool) (*Session, error) {
			built = true
			return namedSession(t, 3001, testSessionID, nil), nil
		}, nil)
		if adm.Won || adm.Session != root {
			t.Fatalf("a pid inside a live session must get that session, not an admission: %+v", adm)
		}
		if built {
			t.Fatal("a registration that loses the ancestry race must not build a session at all")
		}
		rec, ok := reg.Dormant(testSessionID)
		if !ok || rec.EnrollmentID != "enr-before" {
			t.Fatalf("the identity must stay reclaimable: %+v, %v", rec, ok)
		}
	})
}

// parkedRecord leaves state holding one dormant named record enrolled as enr-before, by
// recording a process that is already gone; a rig started on state parks it at Recover. It
// returns the key the record carries.
func parkedRecord(t *testing.T, state string) *ecdsa.PrivateKey {
	t.Helper()
	gone := sleeper(t)
	pid := gone.Process.Pid
	_ = gone.Process.Kill()
	_, _ = gone.Process.Wait()
	return recordNamed(t, state, pid, 1, testSessionID, "enr-before")
}

// requireReclaimable fails unless the identity parked for testSessionID is still whole: the
// record, its key and its enrollment, all where the session itself can take them back.
func requireReclaimable(t *testing.T, r *rig, key *ecdsa.PrivateKey) {
	t.Helper()
	rec, ok := r.srv.Registry.Dormant(testSessionID)
	if !ok {
		t.Fatal("a register that failed consumed the identity: nothing can ever reclaim it now")
	}
	if rec.EnrollmentID != "enr-before" {
		t.Fatalf("the kept record names enrollment %q, want enr-before", rec.EnrollmentID)
	}
	kept, err := rec.PrivateKey()
	if err != nil || kept == nil || !kept.Equal(key) {
		t.Fatalf("the kept record must still carry the session's own key (%v)", err)
	}
	if got := r.revokes(t); len(got) != 0 {
		t.Fatalf("a register that failed revoked %v", got)
	}
}

// A second review round of the fixes above found four more ways the identity could reach the
// wrong process, or be revoked out from under the session holding it. These pin them.

// TestTheAncestryWinnerIsRefusedToAPeerWhosePIDChanged: a registration whose pid turns out to be
// inside a session another root won still has to prove its peer is who it said. Without that
// check the registration's --session-id — and the dormant key and enrollment kept for it — would
// be applied to whatever session the kernel's reused pid now belongs to, a stranger that never
// knew the session id. The admission's own ancestry walk is as much the kernel's to invalidate
// as the walk before an insert.
func TestTheAncestryWinnerIsRefusedToAPeerWhosePIDChanged(t *testing.T) {
	state := filepath.Join(t.TempDir(), "sessions.json")
	key := parkedRecord(t, state)
	r := startRig(t, state)
	seedEnrollment(r.fake, "enr-before", thumbprintOf(t, key), SessionRuntimeID(testSessionID))
	if root := r.call(t, Request{Op: "register", WaitSeconds: 5}); !root.OK { // this test process
		t.Fatalf("register root: %+v", root)
	}
	victim := sleeper(t) // a child of this process, so inside that root's tree
	impostor := sleeper(t)
	peer, err := PinPID(impostor.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	// Hide the victim's ancestry from register's own Root walk, so the ADMISSION's walk is the
	// one that finds the root: exactly the lost-race shape, with a peer that no longer names
	// the pid either walk resolved.
	hidden := func(pid int) (int, error) {
		if pid == victim.Process.Pid {
			return 1, nil
		}
		return procParent(pid)
	}
	r.parentOf.Store(&hidden)
	resp := r.srv.register(context.Background(), peer, victim.Process.Pid, testSessionID, 0)
	r.parentOf.Store(nil)
	if resp.OK || resp.Code != CodeUnidentified {
		t.Fatalf("a peer no longer naming the pid the admission's walk resolved got %+v, want UNIDENTIFIED", resp)
	}
	if got := r.srv.Registry.Get(os.Getpid()).SessionID(); got != "" {
		t.Fatalf("an unrelated root was named %q by a registration whose peer had changed", got)
	}
	requireReclaimable(t, r, key)
}

// TestARefusedRenewOfAnOldEnrollmentLeavesTheReclaimedOneAlone: a renew is signed against one
// identity and answered later. If a rename reclaims a dormant identity in between, the refusal
// speaks for the enrollment that is gone, not for the one just taken back — and lapsing the new
// one would have enrollLoop revoke it, destroying the very grants the reclaim exists to keep.
func TestARefusedRenewOfAnOldEnrollmentLeavesTheReclaimedOneAlone(t *testing.T) {
	sess, err := newSession(1000, 1, "testhost:1000:1", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sess.setEnrolled("enr-old")
	renewed := sess.identity() // what Broker.Renew signed
	if renewed.EnrollmentID != "enr-old" || !renewed.Confirmed {
		t.Fatalf("the session must hold enr-old, confirmed: %+v", renewed)
	}

	// The rename lands while that renew is in flight, giving the session a dormant identity's
	// key and the enrollment that key holds.
	reclaimedKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if !sess.rename(testSessionID, SessionRuntimeID(testSessionID), reclaimedKey, "enr-kept", "named") {
		t.Fatal("rename")
	}
	if got := sess.EnrollmentID(); got != "enr-kept" {
		t.Fatalf("the renamed session holds %q, want the reclaimed enr-kept", got)
	}

	// Now the old renew's refusal arrives.
	if id, applied := sess.markLapsedAt(renewed, "the broker refused this session's renew"); applied {
		t.Fatalf("a refusal for enr-old lapsed the session's current enrollment as %q", id)
	}
	if got := sess.EnrollmentID(); got != "enr-kept" {
		t.Fatalf("the reclaimed enrollment is now %q; a stale refusal must not touch it", got)
	}
	if got := sess.lapsed(); got != "enr-old" {
		t.Fatalf("the lapsed id is %q, want enr-old, the one the rename retired", got)
	}
	// A refusal that really is for the identity the session holds still applies.
	if id, applied := sess.markLapsedAt(sess.identity(), "refused"); !applied || id != "enr-kept" {
		t.Fatalf("a refusal for the current enrollment must lapse it: %q %v", id, applied)
	}
}

// TestTheBlockingEnrollmentTakesTheLapsedSlot: the session's one lapsed slot gets the
// retry-until-gone revoke, and only one id needs it — the one sitting under the runtime id the
// session is about to enrol, which the broker refuses a second row for outright. An id of a name
// the session no longer has blocks nothing, so it yields the slot and takes the bounded revoke
// instead. Reversed, an outage lasting through three bounded tries leaves the session unable to
// enrol at all until the blocking row's lease ends.
func TestTheBlockingEnrollmentTakesTheLapsedSlot(t *testing.T) {
	sess := namedSession(t, 1000, testSessionID, nil)
	sess.setLapsed("enr-old-pid") // what a rename retired: the old runtime's enrollment
	orphans := takeLapsed(sess, []string{"enr-blocking", "enr-other"}, true)
	if got := sess.lapsed(); got != "enr-blocking" {
		t.Fatalf("the lapsed slot holds %q, want the blocking enr-blocking", got)
	}
	if len(orphans) != 2 || orphans[0] != "enr-old-pid" || orphans[1] != "enr-other" {
		t.Fatalf("orphans %v, want the displaced enr-old-pid and enr-other", orphans)
	}
	// With nothing blocking, an occupied slot is left alone.
	other := namedSession(t, 1001, testSessionID, nil)
	other.setLapsed("enr-old-pid")
	if got := takeLapsed(other, []string{"enr-spare"}, false); len(got) != 1 || got[0] != "enr-spare" {
		t.Fatalf("orphans %v, want enr-spare left for the bounded revoke", got)
	}
	if got := other.lapsed(); got != "enr-old-pid" {
		t.Fatalf("the slot was taken from %q by an id that blocks nothing", got)
	}
}

// TestNamingASessionThatHasLeftIsNotASuccess: a session can be unregistered, or its process can
// exit, while a register is naming it. Nothing live then holds the name, so answering OK would
// let a launcher run on believing its session has an identity, and leave the one kept for that
// name to the sweep.
func TestNamingASessionThatHasLeftIsNotASuccess(t *testing.T) {
	r := startRig(t, "")
	sess, err := newSession(4242, 1, "testhost:4242:1", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Never added to the registry: exactly what a session retired a moment ago looks like.
	resp, refused := r.srv.adoptSessionID(context.Background(), sess, testSessionID)
	if !refused || resp.Code != CodeNotASession {
		t.Fatalf("naming a session that has left the registry got %+v (refused %v), want NOT_A_SESSION", resp, refused)
	}
	if sess.SessionID() != "" {
		t.Fatal("a session that has left the registry must not be renamed")
	}
}
