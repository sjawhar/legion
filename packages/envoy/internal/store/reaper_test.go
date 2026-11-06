package store

import (
	"errors"
	"strings"
	"testing"
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

	sweep, err := reg.ReapRoleClaims(func(string) bool { return false }, 0)
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
