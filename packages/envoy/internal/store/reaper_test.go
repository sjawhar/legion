package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// logLine returns the first line of logs carrying msg, for a test that holds what one line says.
func logLine(t *testing.T, logs *lockedBuffer, msg string) string {
	t.Helper()
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `msg="`+msg+`"`) {
			return line
		}
	}
	t.Fatalf("no line says %q; the log is:\n%s", msg, logs.String())
	return ""
}

// logLines counts the lines of logs carrying msg, for a test that holds how many a pass wrote
// rather than what one of them says.
func logLines(logs *lockedBuffer, msg string) int {
	count := 0
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `msg="`+msg+`"`) {
			count++
		}
	}
	return count
}

// The role claim reaper is the backstop for a claim a deregistration did not release, the one its
// role claim view had not been delivered when the session left (releaseAllRoleClaims), so a pass
// that ends at the first role it stumbles on is no backstop at the role count that made the
// deregistration miss the claim: every role after it waits another interval, and a failure that
// lasts leaves them for good. A pass reads every claim from one scan of the bucket and makes no
// read of its own per key, so the one per-claim read failure left to isolate is a stored record
// this build cannot decode (ErrMalformedRoleClaim): here one such record sits between the claims
// of a holder that is gone. The pass reaps every other claim, counts that one skipped, and names
// it in a WARN, so the role left behind is readable from the log rather than inferred from a
// short count.
//
// Nothing about that record changes from pass to pass - the bus answers it, the decode refuses it
// again - so it is counted and logged on every pass for as long as it is stored, which is what
// RoleClaimSweep says Skipped means. The other class of claim a pass cannot read is the key NATS
// refuses before anything is sent (bus.ErrRefused), which no pass can read or write either and
// which legacy_keys_test.go's "the role claim reaper skips a claim it cannot read or delete"
// covers: that one is deleted rather than skipped, and the two must not be confused.
func TestTheRoleClaimReaperGoesPastAClaimItCouldNotRead(t *testing.T) {
	logs := captureRegistryLogs(t)
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)
	const unreadable = "legion-controller"
	roles := []string{"legion-architect", unreadable, "legion-merger", "legion-reviewer"}
	raw := reg.roleKV.Legacy()
	for _, role := range roles {
		if role == unreadable {
			continue
		}
		putRoleClaim(t, raw, role, "ses_gone")
	}
	// Stored by a direct put, because a record this build cannot decode is not one putRoleClaim
	// can write: valid JSON that is not a claim, which the bus hands over and DecodeRoleClaim
	// refuses.
	if _, err := raw.Put(unreadable, []byte(`["not a role claim"]`)); err != nil {
		t.Fatalf("store a record this build cannot decode under %s: %v", unreadable, err)
	}

	sweep, err := reg.ReapRoleClaims(context.Background(), func(string) bool { return false }, 0)
	if err != nil {
		t.Fatalf("a pass over a record it cannot decode: %v, want the pass to go on", err)
	}
	if want := (RoleClaimSweep{Reaped: len(roles) - 1, Skipped: 1}); sweep != want {
		t.Fatalf("the pass = %+v, want %+v: every claim but %s reaped, and that one counted skipped", sweep, want, unreadable)
	}
	for _, role := range roles {
		if role == unreadable {
			continue
		}
		assertRoleHolder(t, reg, role, "")
	}
	if _, err := raw.Get(unreadable); err != nil {
		t.Fatalf("%s's claim after the pass: %v, want it kept for the next one", unreadable, err)
	}
	line := logLine(t, logs, "role reaper skipped a claim it could not read")
	if !strings.Contains(line, "role="+unreadable) || !strings.Contains(line, ErrMalformedRoleClaim.Error()) {
		t.Fatalf("the line for the claim the pass left = %s; want it to name %s and the decode failure", line, unreadable)
	}
}

// The other half of the same contract, and the half the case above cannot reach: the release of
// an expired claim fails, rather than its read. A read that fails leaves a claim whose holder
// nothing knows, while a release that fails leaves one this pass has already found expired, so a
// pass that ended here would leave every role listed after an expired holder's unswept - the
// claims the backstop exists for. The pass reaps the rest, counts this one skipped, and names it
// and its holder in a WARN of its own, since an operator reading a short count has no other way
// to tell which claim stayed.
func TestTheRoleClaimReaperGoesPastAClaimItCouldNotRelease(t *testing.T) {
	logs := captureRegistryLogs(t)
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)
	const undeletable = "legion-architect"
	const holder = "ses_gone"
	roles := []string{"legion-controller", undeletable, "legion-reviewer", "legion-merger"}
	raw := reg.roleKV.Legacy()
	for _, role := range roles {
		putRoleClaim(t, raw, role, holder)
	}
	// The delete fails the way a loaded bucket fails one key's, so nothing says the next pass
	// cannot make it either.
	refusal := errors.New("nats: the bus refused the delete")
	countRoleBucketCalls(t, reg, &sweepRoleKeyValue{failDelete: map[string]error{undeletable: refusal}})

	sweep, err := reg.ReapRoleClaims(context.Background(), func(string) bool { return false }, 0)
	if err != nil {
		t.Fatalf("a pass over a claim whose release fails: %v, want the pass to go on", err)
	}
	if want := (RoleClaimSweep{Reaped: len(roles) - 1, Skipped: 1}); sweep != want {
		t.Fatalf("the pass = %+v, want %+v: every claim but %s reaped, and that one counted skipped", sweep, want, undeletable)
	}
	for _, role := range roles {
		if role == undeletable {
			continue
		}
		assertRoleHolder(t, reg, role, "")
	}
	if _, err := raw.Get(undeletable); err != nil {
		t.Fatalf("%s's claim after the pass: %v, want it kept for the next one", undeletable, err)
	}
	line := logLine(t, logs, "role reaper skipped a claim it could not release")
	if !strings.Contains(line, "role="+undeletable) || !strings.Contains(line, "holder_session_id="+holder) {
		t.Fatalf("the line for the claim the pass left = %s; want it to name %s and its holder %s", line, undeletable, holder)
	}
}

// endedContextRoleKeyValue is the role bucket's legacy handle with every release's delete
// failing, the first of them ending the context the pass was given. The delete is the only call
// a pass makes per role now that every claim comes from the one scan (roleClaimsUntil), so it is
// where the two outages that reach a pass after that scan answered land: the listener's
// shutdown, which ends the reaper loop's context under the pass, and a bus that stops answering
// mid-pass under a request-scoped deadline. Both leave a pass holding every claim the bucket
// holds and no call it can still make.
type endedContextRoleKeyValue struct {
	natsgo.KeyValue

	end func()

	mu      sync.Mutex
	deletes int
}

func (kv *endedContextRoleKeyValue) Delete(key string, opts ...natsgo.DeleteOpt) error {
	kv.mu.Lock()
	kv.deletes++
	kv.mu.Unlock()
	kv.end()
	return errors.New("nats: no responders available for request")
}

// count is how many deletes reached the bucket. It is guarded because the pass makes them on
// bus.Bounded's goroutine and the test reads the count on its own, which -race would report.
func (kv *endedContextRoleKeyValue) count() int {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return kv.deletes
}

// failRoleClaimDeletesAndEndTheContext installs such a handle over reg's role bucket handle for
// the rest of the test (swapRoleBucketHandle), so the pass still scans the bucket and only the
// deletes it owes fail.
func failRoleClaimDeletesAndEndTheContext(t *testing.T, reg *Registry, end func()) *endedContextRoleKeyValue {
	t.Helper()
	return swapRoleBucketHandle(t, reg, func(legacy natsgo.KeyValue) *endedContextRoleKeyValue {
		return &endedContextRoleKeyValue{KeyValue: legacy, end: end}
	}, nil)
}

// A pass goes on past one role's failed release, which is what makes this reaper a backstop;
// once the context it was given has ended, every call it has left is refused before it is sent
// (bus.Bounded), and going on buys nothing but a WARN per role - about 5,000 of them at the
// fleet's role count. A shutdown is where that happens: the listener ends the kv loops' context
// under a pass in flight, which holds every claim the bucket holds from a scan that answered
// and a context that refuses every delete it owes.
//
// So the pass ends there, with the counts it had reached and the context's error, and the one
// line its loop logs for it carries those counts. That line is INFO rather than ERROR: the
// context that ended is the loop's own, which the listener ends to shut down, and a SIGTERM is
// an ordered shutdown rather than a failure - the rule cmd/listener/shutdown_test.go holds over
// every other line of that path, and the one it cannot hold over this one, since nothing there
// puts a pass in flight at the moment of the signal. Both halves are held here: the pass's
// answer, and the single line StartRoleClaimReaper writes for it.
func TestTheRoleClaimReaperEndsThePassWhenItsContextEnds(t *testing.T) {
	logs := captureRegistryLogs(t)
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)
	roles := []string{"legion-architect", "legion-controller", "legion-merger", "legion-reviewer", "legion-tester"}
	raw := reg.roleKV.Legacy()
	for _, role := range roles {
		putRoleClaim(t, raw, role, "ses_gone")
	}
	const releaseFailure = "role reaper skipped a claim it could not release"

	passCtx, endPass := context.WithCancel(context.Background())
	defer endPass()
	deletes := failRoleClaimDeletesAndEndTheContext(t, reg, endPass)
	sweep, err := reg.ReapRoleClaims(passCtx, func(string) bool { return false }, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a pass whose context ended under it = %+v, %v; want it to end with the context's error", sweep, err)
	}
	if want := (RoleClaimSweep{}); sweep != want {
		t.Fatalf("the pass = %+v, want %+v: the counts it had reached when the context ended", sweep, want)
	}
	if got := logLines(logs, releaseFailure); got != 0 {
		t.Fatalf("the pass wrote %d lines for claims it could not release, over %d roles; want none, since the cycle's own line says the pass ended", got, len(roles))
	}
	if got := deletes.count(); got != 1 {
		t.Fatalf("the pass sent %d deletes to the bucket over %d roles; want the one it was making when the context ended", got, len(roles))
	}

	// And through the loop that runs it, which is where an operator reads a cut-short pass, and
	// where its level is decided: one line, carrying what the pass had reached beside the error
	// that ended it, at INFO, because the context that ended was the loop's own.
	loopCtx, endLoop := context.WithCancel(context.Background())
	defer endLoop()
	failRoleClaimDeletesAndEndTheContext(t, reg, endLoop)
	done := reg.StartRoleClaimReaper(loopCtx, func(string) bool { return false }, 10*time.Millisecond, 0)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the role claim reaper loop is still running after its context ended")
	}
	if got := logLines(logs, releaseFailure); got != 0 {
		t.Fatalf("the loop's pass wrote %d lines for claims it could not release, over %d roles; want none", got, len(roles))
	}
	if got := logLines(logs, "role claim reaper cycle failed"); got != 0 {
		t.Fatalf("the loop wrote %d cycle failure lines for a pass its own context ended; want none, since that pass ended with the listener rather than failing. the log is:\n%s", got, logs.String())
	}
	const endedCycle = "role claim reaper cycle ended with its loop"
	if got := logLines(logs, endedCycle); got != 1 {
		t.Fatalf("the loop wrote %d lines for the pass its own context ended, want the one line a cut-short pass costs; the log is:\n%s", got, logs.String())
	}
	line := logLine(t, logs, endedCycle)
	for _, want := range []string{"level=INFO", "context canceled", "reaped=0", "skipped=0"} {
		if !strings.Contains(line, want) {
			t.Fatalf("the line the loop wrote for the pass its own context ended = %s; want it to carry %q, so a clean shutdown reads as one and a pass cut short is still readable", line, want)
		}
	}
}

// The same end at the pass's other bucket call: the delete of a claim stored under a key this
// build cannot read (bus.KeyValue.RefusesRead, a key an earlier listener stored past the read
// bound). It is bounded by the pass's context like every release, so a context the listener
// ended under the pass fails it there, and the pass ends with that error rather than writing a
// line for this role and going on to the thousands behind it - the one bucket call of a pass
// that an ended context could not reach before. None of the three delete arms writes its line
// here, because the context check comes before them, as it does before the release's line.
func TestTheRoleClaimReaperEndsThePassWhenItsContextEndsOnAKeyItCannotRead(t *testing.T) {
	logs := captureRegistryLogs(t)
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)
	raw := reg.roleKV.Legacy()
	stale := time.Now().Add(-time.Hour).UnixMilli()
	claim, err := json.Marshal(RoleClaim{HolderSessionID: "ses_gone", ClaimedAt: stale})
	if err != nil {
		t.Fatalf("encode the claim the earlier listener stored: %v", err)
	}
	// Five keys past the read bound and within the delete's, as an earlier listener left them:
	// every one of them reaches the refused-key arm, so which the scan hands over first decides
	// nothing.
	letters := []string{"a", "b", "c", "d", "e"}
	for _, letter := range letters {
		_, role := testnats.LegacyKeys(bus.RoleClaimBucket, letter)
		if _, err := raw.Put(role, claim); err != nil {
			t.Fatalf("store the %d-byte role: %v", len(role), err)
		}
	}

	passCtx, endPass := context.WithCancel(context.Background())
	defer endPass()
	deletes := failRoleClaimDeletesAndEndTheContext(t, reg, endPass)
	sweep, err := reg.ReapRoleClaims(passCtx, func(string) bool { return false }, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a pass whose context ended under its delete of an unreadable key = %+v, %v; want it to end with the context's error", sweep, err)
	}
	if want := (RoleClaimSweep{}); sweep != want {
		t.Fatalf("the pass = %+v, want %+v: the counts it had reached when the context ended", sweep, want)
	}
	for _, msg := range []string{
		"role reaper deleted a claim it cannot read",
		"role reaper skipped a claim it cannot delete",
		"role reaper skipped a claim it cannot read whose delete failed",
	} {
		if got := logLines(logs, msg); got != 0 {
			t.Fatalf("the pass wrote %d %q lines over %d unreadable keys; want none, since the cycle's own line says the pass ended", got, msg, len(letters))
		}
	}
	if got := deletes.count(); got != 1 {
		t.Fatalf("the pass sent %d deletes to the bucket over %d unreadable keys; want the one it was making when the context ended", got, len(letters))
	}
}

// blockedDeleteRoleKeyValue is the role bucket's legacy handle with every delete parked until
// release is closed, and sent closed once the first of them has arrived. It is the half of the
// refused key's delete the handle above cannot reach: that one answers an error of its own, so a
// pass that read the context rather than the delete's answer still ends, and only the ctx.Err()
// check is held by it. A delete that answers nothing at all leaves nothing but the bound around
// it to end the pass.
type blockedDeleteRoleKeyValue struct {
	natsgo.KeyValue

	release <-chan struct{}
	sent    chan struct{}

	first   sync.Once
	mu      sync.Mutex
	deletes int
}

func (kv *blockedDeleteRoleKeyValue) Delete(key string, opts ...natsgo.DeleteOpt) error {
	kv.mu.Lock()
	kv.deletes++
	kv.mu.Unlock()
	kv.first.Do(func() { close(kv.sent) })
	<-kv.release
	return errors.New("nats: no responders available for request")
}

// count is how many deletes reached the bucket, guarded for the reason the handle above's is.
func (kv *blockedDeleteRoleKeyValue) count() int {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return kv.deletes
}

// blockRoleClaimDeletes installs such a handle over reg's role bucket handle for the rest of the
// test (swapRoleBucketHandle), so the pass still scans the bucket and only the deletes it owes
// are parked.
func blockRoleClaimDeletes(t *testing.T, reg *Registry, release <-chan struct{}) *blockedDeleteRoleKeyValue {
	t.Helper()
	return swapRoleBucketHandle(t, reg, func(legacy natsgo.KeyValue) *blockedDeleteRoleKeyValue {
		return &blockedDeleteRoleKeyValue{KeyValue: legacy, release: release, sent: make(chan struct{})}
	}, nil)
}

// The bound itself, which the case above cannot hold. There the delete answers an error, so the
// pass ends whether it was waiting for the delete or for its context, and what that case pins is
// the ctx.Err() check behind the answer: take bus.Bounded off the delete and it still passes.
// The delete of a key this build cannot read is bounded like every release, and a bound is only
// visible against a call that does not answer.
//
// So the delete here is parked, and the pass's context ends while it is parked: a pass that
// waits for the bucket answers nothing until the test releases the delete, and the pass answers
// before that, which only the bound around the delete can do. The parked call is released before
// the test ends, so no goroutine is left on it - what bus.Bounded abandons runs on to its own
// end, which here is the release (bus.Bounded, and ReapRoleClaims on why a delete that lands
// late is safe).
func TestTheRoleClaimReaperAnswersAtItsContextsEndWhileItsDeleteOfAKeyItCannotReadIsParked(t *testing.T) {
	logs := captureRegistryLogs(t)
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)
	raw := reg.roleKV.Legacy()
	stale := time.Now().Add(-time.Hour).UnixMilli()
	claim, err := json.Marshal(RoleClaim{HolderSessionID: "ses_gone", ClaimedAt: stale})
	if err != nil {
		t.Fatalf("encode the claim the earlier listener stored: %v", err)
	}
	letters := []string{"a", "b", "c", "d", "e"}
	for _, letter := range letters {
		_, role := testnats.LegacyKeys(bus.RoleClaimBucket, letter)
		if _, err := raw.Put(role, claim); err != nil {
			t.Fatalf("store the %d-byte role: %v", len(role), err)
		}
	}

	// Closed however the case ends, before the handle is put back, so the delete bus.Bounded
	// abandoned is never left parked on a test that has finished.
	release := make(chan struct{})
	defer close(release)
	deletes := blockRoleClaimDeletes(t, reg, release)

	passCtx, endPass := context.WithCancel(context.Background())
	defer endPass()
	type passAnswer struct {
		sweep RoleClaimSweep
		err   error
	}
	answers := make(chan passAnswer, 1)
	go func() {
		sweep, err := reg.ReapRoleClaims(passCtx, func(string) bool { return false }, 0)
		answers <- passAnswer{sweep: sweep, err: err}
	}()
	select {
	case <-deletes.sent:
	case <-time.After(30 * time.Second):
		t.Fatal("the pass has not reached its delete of the first key it cannot read")
	}
	endPass()

	// Far longer than a pass that answers at its context's end needs, and finite because the
	// delete it is waiting on answers only when this case releases it: a pass that waits for the
	// bucket waits here for good.
	const answerBound = 30 * time.Second
	var answered passAnswer
	select {
	case answered = <-answers:
	case <-time.After(answerBound):
		t.Fatalf("the pass is still inside the delete of a key it cannot read %s after its context ended; want it to answer at the context's end, which is what bus.Bounded around that delete is for", answerBound)
	}
	if !errors.Is(answered.err, context.Canceled) {
		t.Fatalf("a pass whose context ended under its parked delete of an unreadable key = %+v, %v; want it to end with the context's error", answered.sweep, answered.err)
	}
	if want := (RoleClaimSweep{}); answered.sweep != want {
		t.Fatalf("the pass = %+v, want %+v: the counts it had reached when the context ended", answered.sweep, want)
	}
	for _, msg := range []string{
		"role reaper deleted a claim it cannot read",
		"role reaper skipped a claim it cannot delete",
		"role reaper skipped a claim it cannot read whose delete failed",
	} {
		if got := logLines(logs, msg); got != 0 {
			t.Fatalf("the pass wrote %d %q lines over %d unreadable keys; want none, since it answered its context rather than the delete", got, msg, len(letters))
		}
	}
	if got := deletes.count(); got != 1 {
		t.Fatalf("the pass sent %d deletes to the bucket over %d unreadable keys; want the one it was parked on when the context ended", got, len(letters))
	}
}

// The other half of that level, and the one a change that quieted the whole branch would walk
// past: a pass that fails while its loop runs on is the reaper's own failure and keeps its
// ERROR, because nothing else tells an operator the backstop has stopped working. The listing is
// what fails here, since a failed read under a live loop is logged per role and never ends the
// pass: earlyEndKV hands the scan a watch that closes before it has delivered the bucket, which
// drainScan reports as a scan short of it.
//
// The loop is left running while this reads its line, so the count is a floor rather than one:
// the pass repeats every interval, and the line this holds is the first. Whether the loop's own
// end is INFO is the case above's; this one holds only that a live loop's failure is not.
func TestTheRoleClaimReaperLogsAPassThatFailedUnderALiveLoopAtError(t *testing.T) {
	logs := captureRegistryLogs(t)
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)
	putRoleClaim(t, reg.roleKV.Legacy(), "legion-architect", "ses_gone")
	watchRoleBucketThrough(t, reg, earlyEndKV{KeyValue: reg.roles().WatchHandle(), ended: true})

	loopCtx, endLoop := context.WithCancel(context.Background())
	defer endLoop()
	const cycleFailed = "role claim reaper cycle failed"
	done := reg.StartRoleClaimReaper(loopCtx, func(string) bool { return false }, 10*time.Millisecond, 0)
	waitFor(t, 30*time.Second, func() bool { return logLines(logs, cycleFailed) > 0 })
	line := logLine(t, logs, cycleFailed)
	endLoop()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the role claim reaper loop is still running after its context ended")
	}

	for _, want := range []string{"level=ERROR", "reaped=0", "skipped=0"} {
		if !strings.Contains(line, want) {
			t.Fatalf("the line the loop wrote for a pass that failed under it = %s; want it to carry %q, so a backstop that has stopped working is read as one", line, want)
		}
	}
}

const (
	// outageRoleClaims is how many claims the bucket holds in the outage case below. It is the
	// 200 the issue's probe measured one WARN per role over - 200 of 200 - which is enough to
	// tell one line per pass from one line per role by two orders of magnitude, and it is kept
	// well clear of the package's one 5,000-claim fixture, which that case neither shares nor
	// duplicates.
	outageRoleClaims = 200
)

// refusedGetRoleKeyValue is the role bucket's legacy handle with every Get failing as a bus with
// no responder does, and every Delete reaching the bucket. Both are counted.
//
// It is the outage that strikes a pass after its scan has answered: the bucket's records are in
// the pass's hand, and the bus behind any read it might still make is gone. The watch handle is
// left alone by the installer below, because a scan that failed is the other outage and the
// other case.
//
// The fake is here rather than a hook on the sweep cases' counting handle (sweepRoleKeyValue)
// because this is its only user: no sweep reads a claim by key any more, so nothing else in the
// package has a per-key read to fail.
type refusedGetRoleKeyValue struct {
	natsgo.KeyValue

	mu      sync.Mutex
	gets    int
	deletes int
}

func (kv *refusedGetRoleKeyValue) Get(key string) (natsgo.KeyValueEntry, error) {
	kv.mu.Lock()
	kv.gets++
	kv.mu.Unlock()
	return nil, errors.New("nats: no responders available for request")
}

func (kv *refusedGetRoleKeyValue) Delete(key string, opts ...natsgo.DeleteOpt) error {
	kv.mu.Lock()
	kv.deletes++
	kv.mu.Unlock()
	return kv.KeyValue.Delete(key, opts...)
}

// counts is how many reads the outage refused and how many deletes reached the bucket. They are
// guarded because a pass can make its calls on bus.Bounded's goroutine while the test reads the
// counts on its own, which -race would otherwise report.
func (kv *refusedGetRoleKeyValue) counts() (gets, deletes int) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return kv.gets, kv.deletes
}

// failRoleClaimGets installs such a handle over reg's role bucket handle for the rest of the
// test (swapRoleBucketHandle), so the pass's scan reads the whole bucket and the outage reaches
// only the reads and deletes a pass makes per key.
func failRoleClaimGets(t *testing.T, reg *Registry) *refusedGetRoleKeyValue {
	t.Helper()
	return swapRoleBucketHandle(t, reg, func(legacy natsgo.KeyValue) *refusedGetRoleKeyValue {
		return &refusedGetRoleKeyValue{KeyValue: legacy}
	}, nil)
}

// Acceptance criterion A4. At the parent change's merge a bus outage that struck a pass while
// the reaper loop's context still stood cost one WARN per role - 200 of 200 in the issue's
// probe, about 5,000 lines per pass at the fleet's role count - because the pass read a claim
// per key, and once the bus had stopped answering every one of those reads failed on its own
// account.
//
// That is the outage here, and it strikes where the criterion is about: after the scan has
// answered, while the pass is walking the claims the scan gave it. The bucket's watch handle is
// left alone, so the scan delivers all 200 claims; its legacy handle refuses every Get as a bus
// with no responder does, and takes the deletes; and the pass's context stands throughout, so
// nothing here is the shutdown the two cases above hold.
//
// A pass that read a claim per key would meet that outage once per role: 200 lines saying it
// could not read a claim, and not one claim reaped. A pass that reads every claim from its one
// scan makes no per-key read at all, so the outage costs it nothing - every holder is gone, so
// every claim is reaped, with none of the five lines a pass can write for a role and one delete
// per claim. The count of refused reads is read off the handle beside those lines, because it
// is the same fact from the other side: the outage wrote no line because the pass asked the bus
// for nothing.
//
// The other outage, the one that strikes before the pass has anything in hand, fails the scan,
// and TestTheRoleClaimReaperLogsAPassThatFailedUnderALiveLoopAtError holds what the loop writes
// for it: one line, at ERROR, carrying the counts the pass had reached.
func TestABusOutageUnderAReaperPassCostsNoPerRoleLineAndReapsEveryClaim(t *testing.T) {
	logs := captureRegistryLogs(t)
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)
	roles, holderOf := otherSessionsClaims("outage-role", outageRoleClaims)
	putRoleClaimsInParallel(t, reg.roles().Legacy(), roles, holderOf)
	// Every holder is gone, so every claim of the bucket is the pass's to reap and every role
	// is one the outage could cost a line.
	gone := func(string) bool { return false }
	// Every line the pass writes per role, by name: a change that moved the cost from one of
	// these to another would otherwise pass this case unchanged.
	perRole := []string{
		"role reaper skipped a claim it could not read",
		"role reaper skipped a claim it could not release",
		"role reaper deleted a claim it cannot read",
		"role reaper skipped a claim it cannot delete",
		"role reaper skipped a claim it cannot read whose delete failed",
	}

	outage := failRoleClaimGets(t, reg)
	sweep, err := reg.ReapRoleClaims(context.Background(), gone, 0)
	if err != nil {
		t.Fatalf("a pass over %d claims whose bus refuses every read of a key = %+v, %v; want it to finish, since it makes no such read", outageRoleClaims, sweep, err)
	}
	// Each of these is a fact of its own about what the outage cost, so each is reported rather
	// than the first of them ending the case: a pass that paid for the outage pays in the lines
	// it wrote, the claims it did not reap and the reads it made, and a reader of the failure
	// needs all three.
	if want := (RoleClaimSweep{Reaped: outageRoleClaims}); sweep != want {
		t.Errorf("the pass = %+v, want %+v: every holder is gone, and an outage on a read the pass does not make costs it no claim", sweep, want)
	}
	for _, msg := range perRole {
		if got := logLines(logs, msg); got != 0 {
			t.Errorf("the pass wrote %d %q lines over a bucket of %d claims whose bus refuses every read of a key; want none, since every claim came from the one scan",
				got, msg, outageRoleClaims)
		}
	}
	gets, deletes := outage.counts()
	if gets != 0 {
		t.Errorf("the pass read %d keys of the bucket on top of its one scan, over %d claims; want none: a read per key is what made this outage cost a line per role", gets, outageRoleClaims)
	}
	if deletes != outageRoleClaims {
		t.Errorf("the pass sent %d deletes over %d claims whose holders are all gone; want one per claim", deletes, outageRoleClaims)
	}
}
