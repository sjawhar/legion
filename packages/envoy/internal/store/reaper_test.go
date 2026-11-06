package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
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
// lasts leaves them for good. Here one claim's read fails the way a loaded bucket fails one - an
// ordinary error, not the bus.ErrRefused that legacy_keys_test.go's "the role claim reaper skips a
// claim it cannot read or delete" covers - between the claims of a holder that is gone. The pass
// reaps every other claim, counts that one skipped, and names it in a WARN, so the role left
// behind is readable from the log rather than inferred from a short count.
func TestTheRoleClaimReaperGoesPastAClaimItCouldNotRead(t *testing.T) {
	logs := captureRegistryLogs(t)
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)
	const unreadable = "legion-architect"
	roles := []string{"legion-controller", unreadable, "legion-reviewer", "legion-merger"}
	raw := reg.roleKV.Legacy()
	for _, role := range roles {
		putRoleClaim(t, raw, role, "ses_gone")
	}
	// The read fails the way a loaded bucket fails one key's: an ordinary error, not a refusal,
	// so nothing says the next pass cannot read it.
	countRoleBucketCalls(t, reg, &sweepRoleKeyValue{failGet: map[string]error{unreadable: errors.New("read timed out")}})

	sweep, err := reg.ReapRoleClaims(context.Background(), func(string) bool { return false }, 0)
	if err != nil {
		t.Fatalf("a pass over a claim whose read fails: %v, want the pass to go on", err)
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
	if !strings.Contains(line, "role="+unreadable) || !strings.Contains(line, "read timed out") {
		t.Fatalf("the line for the claim the pass left = %s; want it to name %s and the read's error", line, unreadable)
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

// endedContextRoleKeyValue is the role bucket's legacy handle with every claim read failing, the
// first of them ending the context the pass was given. It is the shape of the two outages that
// reach a pass after its listing answered: the listener's shutdown, which ends the reaper loop's
// context and then drains NATS, and a bus that stops answering mid-pass under a request-scoped
// deadline. Both leave a pass holding a list of every role and no way to read one.
type endedContextRoleKeyValue struct {
	natsgo.KeyValue

	end func()

	mu    sync.Mutex
	reads int
}

func (kv *endedContextRoleKeyValue) Get(key string) (natsgo.KeyValueEntry, error) {
	kv.mu.Lock()
	kv.reads++
	kv.mu.Unlock()
	kv.end()
	return nil, errors.New("nats: no responders available for request")
}

// count is how many claim reads reached the bucket. It is guarded because the pass makes them on
// bus.Bounded's goroutine and the test reads the count on its own, which -race would report.
func (kv *endedContextRoleKeyValue) count() int {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return kv.reads
}

// failRoleClaimReadsAndEndTheContext installs such a handle over reg's role bucket handle for the
// rest of the test, and puts the previous one back after it, as countRoleBucketCalls does for the
// counting handle: the swap is made under the mutex every reader of that field takes
// (Registry.roles), and the previous handle's watch handle is kept, so the pass still lists the
// bucket and only its reads fail.
func failRoleClaimReadsAndEndTheContext(t *testing.T, reg *Registry, end func()) *endedContextRoleKeyValue {
	t.Helper()
	reg.kvMu.Lock()
	previous := reg.roleKV
	handle := &endedContextRoleKeyValue{KeyValue: previous.Legacy(), end: end}
	reg.roleKV = bus.NewKeyValue(handle, previous.WatchHandle(), previous.Streams())
	reg.kvMu.Unlock()
	t.Cleanup(func() {
		reg.kvMu.Lock()
		defer reg.kvMu.Unlock()
		reg.roleKV = previous
	})
	return handle
}

// A pass goes on past one role's failed read, which is what makes this reaper a backstop; once
// the context it was given has ended, every read it has left fails the same way, and going on
// buys nothing but a WARN per role - about 5,000 of them at the fleet's role count, against the
// one ERROR a pass that ended at the first failure wrote. A shutdown is where that happens: the
// listener ends the kv loops' context and then drains NATS, so a pass in flight holds a full
// list of roles and a bus that answers none of them.
//
// So the pass ends there, with the counts it had reached and the context's error, and the one
// line its loop logs for a failed cycle carries those counts beside the error. Both halves are
// held here: the pass's answer, and the single line StartRoleClaimReaper writes for it.
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
	const readFailure = "role reaper skipped a claim it could not read"

	passCtx, endPass := context.WithCancel(context.Background())
	defer endPass()
	reads := failRoleClaimReadsAndEndTheContext(t, reg, endPass)
	sweep, err := reg.ReapRoleClaims(passCtx, func(string) bool { return false }, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a pass whose context ended under it = %+v, %v; want it to end with the context's error", sweep, err)
	}
	if want := (RoleClaimSweep{}); sweep != want {
		t.Fatalf("the pass = %+v, want %+v: the counts it had reached when the context ended", sweep, want)
	}
	if got := logLines(logs, readFailure); got != 0 {
		t.Fatalf("the pass wrote %d lines for claims it could not read, over %d roles; want none, since the cycle's own line says the pass ended", got, len(roles))
	}
	if got := reads.count(); got != 1 {
		t.Fatalf("the pass sent %d reads to the bucket over %d roles; want the one it was making when the context ended", got, len(roles))
	}

	// And through the loop that runs it, which is where an operator reads a cut-short pass: one
	// line, carrying what the pass had reached beside the error that ended it.
	loopCtx, endLoop := context.WithCancel(context.Background())
	defer endLoop()
	failRoleClaimReadsAndEndTheContext(t, reg, endLoop)
	done := reg.StartRoleClaimReaper(loopCtx, func(string) bool { return false }, 10*time.Millisecond, 0)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the role claim reaper loop is still running after its context ended")
	}
	if got := logLines(logs, readFailure); got != 0 {
		t.Fatalf("the loop's pass wrote %d lines for claims it could not read, over %d roles; want none", got, len(roles))
	}
	if got := logLines(logs, "role claim reaper cycle failed"); got != 1 {
		t.Fatalf("the loop wrote %d cycle failure lines, want the one line a cut-short pass costs; the log is:\n%s", got, logs.String())
	}
	line := logLine(t, logs, "role claim reaper cycle failed")
	for _, want := range []string{"context canceled", "reaped=0", "skipped=0"} {
		if !strings.Contains(line, want) {
			t.Fatalf("the cycle's failure line = %s; want it to carry %q, so a pass cut short is still readable", line, want)
		}
	}
}
