// What a deregistration's fallback (releaseAllRoleClaimsByScan) writes about the claims it could
// not read. Two classes reach it and neither is released: a claim stored under a key no call of
// this build can read (bus.ErrRefused), which the role reaper deletes, and a record no build can
// decode (ErrMalformedRoleClaim), which the role reaper does no more than count skipped. The
// sweep counts each class inside its loop and writes one line per class after it, because it
// runs only while the role claim view cannot be served - the outage itself - and once per
// deregistration, so a line per claim over a bucket of thousands, under a fleet deregistering,
// would amplify what it reports.
//
// The case here is about those two lines: how many of each are written, which class's count each
// carries, and what it names the first claim of that class by - the key's size for a key no call
// can read, the role for a record no build can decode. The fixtures it needs are here with it,
// because nothing else in the package stores a claim it cannot read on purpose: a registry whose
// view cannot be served over a bucket of this session's claims alone, the records this build
// cannot decode, and the claims under keys it cannot read.

package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	// degradedUndecodableClaims is how many records this build cannot decode the all-undecodable
	// case stores: tens, so one line for the sweep is told from one line per claim by an order
	// of magnitude, and few enough that the fixture is seeded and read in a moment. The sweep
	// this is about runs only while the bus is degraded, so the bucket it meets is the deployed
	// one, and what the count has to show is which of the two the line count follows.
	degradedUndecodableClaims = 30
	// degradedRefusedKeys is how many claims the mixed case stores under keys no call of this
	// build can read, and degradedMixedUndecodable how many records it cannot decode. The two
	// differ from each other and from the count above, so a line carrying the wrong class's
	// count cannot read as the right one.
	degradedRefusedKeys      = 5
	degradedMixedUndecodable = 3
)

// unreadableRoleBucketRegistry is smallRoleBucketRegistry's sibling for the cases about what a
// degraded sweep writes about the claims it could not read: a registry whose role claim view
// cannot be served over a bucket holding nothing but the claims session holds and its interest
// record, for the case to fill with the records it cannot read. It seeds no other session's
// readable claim, because what these cases count is the claims the sweep left behind and a
// readable stranger's claim is in neither count. It returns the registry and the role bucket's
// own handle, taken before any counting handle is installed over it, as its sibling does.
func unreadableRoleBucketRegistry(t *testing.T, session string, held []string) (*Registry, natsgo.KeyValue) {
	t.Helper()
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	reg, rawInterests := coldRegistry(t, conn)
	rawRoles := reg.roles().Legacy()
	for _, role := range held {
		putRoleClaim(t, rawRoles, role, session)
	}
	// The record's only topic is not a role's, so the pass over it releases nothing and the
	// claims below are the fallback's alone to release.
	putInterest(t, rawInterests, Interest{SessionID: session, MachineID: "m1", Topics: []string{"notifications.chat"}})
	if err := reg.RoleViewErr(); err == nil {
		t.Fatal("the registry's role claim view can be served, so these cases would never reach the fallback")
	}
	return reg, rawRoles
}

// storeUndecodableRoleClaims stores count records under prefix-00, prefix-01 and so on, each
// valid JSON that is not a role claim, straight through the bucket's own handle: no write path of
// this build stores one, and DecodeRoleClaim refuses every one of them with ErrMalformedRoleClaim.
// It returns the roles in the order a scan of the bucket delivers them, which is sorted
// (roleClaimsUntil), so the first of them is the one the sweep's line names.
func storeUndecodableRoleClaims(t *testing.T, kv natsgo.KeyValue, prefix string, count int) []string {
	t.Helper()
	roles := make([]string, 0, count)
	for i := range count {
		role := fmt.Sprintf("%s-%02d", prefix, i)
		if _, err := kv.Put(role, []byte(`["not a role claim"]`)); err != nil {
			t.Fatalf("store a record this build cannot decode under %s: %v", role, err)
		}
		roles = append(roles, role)
	}
	return roles
}

// storeRoleClaimsUnderRefusedKeys stores one claim per letter, each under a key of its own of the
// shape an earlier listener could write and no call of this build can read
// (testnats.LegacyKeys' unreadable key, which bus.KeyValue.RefusesRead answers for). The put goes
// through the bucket's own handle because the checked handle refuses to write such a key at all.
// It returns the keys in the order a scan delivers them, sorted, so the first is the one the
// sweep's line sizes.
//
// No two of them are the same length, and that is what makes the line's key_bytes readable as a
// claim about which key it named. The sweep names the first claim of this class it met by its
// key's size alone, since the key itself is thousands of bytes of one letter, so keys all of one
// length would hold that assertion up whichever of them the line had named - the first, the last
// or any between - and a sweep that kept the last claim rather than the first would pass it
// unchanged. Each key after the first is therefore shortened by one byte more than the one
// before, which leaves every one of them in the same class: testnats.LegacyKeys' unreadable key
// sits eleven bytes past the bound a direct get of the role bucket is refused at, so a handful of
// bytes off it is still a key no call of this build can read, and still far inside the longer
// bound a delete is held to, so the role reaper can remove it. A key's letter differs at its
// first byte, so the shortening moves none of them in the order a scan delivers them.
func storeRoleClaimsUnderRefusedKeys(t *testing.T, kv natsgo.KeyValue, holder string, letters []string) []string {
	t.Helper()
	keys := make([]string, 0, len(letters))
	for i, letter := range letters {
		_, key := testnats.LegacyKeys(bus.RoleClaimBucket, letter)
		key = key[:len(key)-i]
		if _, err := kv.Put(key, []byte(`{"holder_session_id":"`+holder+`"}`)); err != nil {
			t.Fatalf("store the %d-byte key an earlier listener left: %v", len(key), err)
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// Acceptance criterion A5. The degraded sweep once wrote a WARN for every claim it could not
// read, and it runs only while the bus is degraded, once per deregistration: a fleet
// deregistering under that outage, over a bucket holding thousands of such claims, wrote
// thousands of lines per deregistration and amplified the outage it was reporting. It now counts
// each class inside the loop and writes one line per class after it, carrying the count and the
// first claim of that class it met.
//
// The case beside this one, "a claim of another session that cannot be read is logged and the
// sweep goes on", has exactly one unreadable claim in its bucket, so one line per claim and one
// line per sweep are the same number there and it can tell them apart no more than a count of
// one can. These two have tens of them, and each asserts the count of lines rather than their
// contents alone.
func TestTheDeregistrationsFallbackSummarizesTheClaimsItCannotReadInOneLinePerClass(t *testing.T) {
	const (
		undecodableLine = "role registry could not read stored claims while releasing a session's claims; they stay in the bucket, unreleased by this deregistration, and role and error are the first claim's"
		refusedLine     = "role registry left stored claims it cannot read to the role reaper while releasing a session's claims; key_bytes and error are the first claim's"
	)

	t.Run("every claim of the bucket but the session's own is a record it cannot decode and one line carries them all", func(t *testing.T) {
		logs := captureRegistryLogs(t)
		const session = "ses_degraded_undecodable"
		held := []string{"degraded-held-a", "degraded-held-b"}
		reg, rawRoles := unreadableRoleBucketRegistry(t, session, held)
		// Named so they sort after the two the session holds, which makes the role the line
		// carries the first of its own class rather than the first key of the bucket.
		undecodable := storeUndecodableRoleClaims(t, rawRoles, "unreadable", degradedUndecodableClaims)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := reg.Remove(ctx, session, nil, releasedByItsProcess); err != nil {
			t.Fatalf("Remove(%s) over a bucket whose every other claim is a record this build cannot decode: %v, want both of its own claims released and no error", session, err)
		}
		if wrote := logLines(logs, undecodableLine); wrote != 1 {
			t.Fatalf("the sweep wrote %d lines over %d claims it could not read, want the one summary line a degraded deregistration pays for. the log is:\n%s",
				wrote, len(undecodable), logs.String())
		}
		// Every field is read off that one line, because the fallback's own INFO line carries
		// the session too and a check over the whole log would take it for this one.
		line := logLine(t, logs, undecodableLine)
		for _, want := range []string{
			"session_id=" + session,
			fmt.Sprintf("claims=%d", len(undecodable)),
			"role=" + undecodable[0],
			ErrMalformedRoleClaim.Error(),
		} {
			if !strings.Contains(line, want) {
				t.Fatalf("the sweep's one line for the %d claims it could not read = %s; want it to carry %q, which is how many it left, the first it met and why",
					len(undecodable), line, want)
			}
		}
		// The other class's line is not written at all for a deregistration that met none of it,
		// which is why the two classes have a line each rather than one line with both counts.
		if wrote := logLines(logs, refusedLine); wrote != 0 {
			t.Fatalf("the sweep wrote %d lines about claims under keys the bus refuses, over a bucket holding none; want none", wrote)
		}
		for _, role := range held {
			assertRoleHolder(t, reg, role, "")
		}
		assertInterestMissing(t, reg, session)
		// What the line says of them: they stay in the bucket, every one of them, released by
		// this deregistration no more than it read them.
		scanned, err := reg.roleClaimsUntil(context.Background())
		if err != nil {
			t.Fatalf("list the role bucket's keys after the deregistration: %v", err)
		}
		if kept := scannedRoleNames(scanned); !slices.Equal(kept, undecodable) {
			t.Fatalf("the bucket holds %v after the deregistration, want the %d records it could not read with both of %s's claims released",
				kept, len(undecodable), session)
		}
	})

	t.Run("claims under keys the bus refuses and records it cannot decode are counted apart, one line each", func(t *testing.T) {
		logs := captureRegistryLogs(t)
		const session = "ses_degraded_both_classes"
		held := []string{"both-held-a", "both-held-b"}
		reg, rawRoles := unreadableRoleBucketRegistry(t, session, held)
		// One bucket holding both classes: the counts are of different sizes and the line each
		// carries must hold its own class's, which is what shows the two are counted apart
		// rather than summed into whichever line is written first.
		refused := storeRoleClaimsUnderRefusedKeys(t, rawRoles, "ses_legacy_holder", []string{"a", "b", "c", "d", "e"})
		undecodable := storeUndecodableRoleClaims(t, rawRoles, "unreadable", degradedMixedUndecodable)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := reg.Remove(ctx, session, nil, releasedByItsProcess); err != nil {
			t.Fatalf("Remove(%s) over a bucket holding claims of both classes it cannot read: %v, want both of its own claims released and no error", session, err)
		}

		if wrote := logLines(logs, refusedLine); wrote != 1 {
			t.Fatalf("the sweep wrote %d lines over %d claims stored under keys it cannot read, want one. the log is:\n%s", wrote, len(refused), logs.String())
		}
		// The key is named by its size and not its text, which is what tells a key too long from
		// one outside the bucket's alphabet without putting 4 KiB of key in the log.
		line := logLine(t, logs, refusedLine)
		for _, want := range []string{
			"session_id=" + session,
			fmt.Sprintf("claims=%d", len(refused)),
			fmt.Sprintf("key_bytes=%d", len(refused[0])),
			fmt.Sprintf("a key of %d bytes", len(refused[0])),
		} {
			if !strings.Contains(line, want) {
				t.Fatalf("the sweep's one line for the %d claims under keys it cannot read = %s; want it to carry %q",
					len(refused), line, want)
			}
		}

		if wrote := logLines(logs, undecodableLine); wrote != 1 {
			t.Fatalf("the sweep wrote %d lines over %d records it could not decode, want one. the log is:\n%s", wrote, len(undecodable), logs.String())
		}
		line = logLine(t, logs, undecodableLine)
		for _, want := range []string{
			"session_id=" + session,
			fmt.Sprintf("claims=%d", len(undecodable)),
			"role=" + undecodable[0],
			ErrMalformedRoleClaim.Error(),
		} {
			if !strings.Contains(line, want) {
				t.Fatalf("the sweep's one line for the %d records it could not decode = %s; want it to carry %q",
					len(undecodable), line, want)
			}
		}

		for _, role := range held {
			assertRoleHolder(t, reg, role, "")
		}
		assertInterestMissing(t, reg, session)
		scanned, err := reg.roleClaimsUntil(context.Background())
		if err != nil {
			t.Fatalf("list the role bucket's keys after the deregistration: %v", err)
		}
		kept := scannedRoleNames(scanned)
		if len(kept) != len(refused)+len(undecodable) {
			t.Fatalf("the bucket holds %d claims after the deregistration, want the %d it could not read with both of %s's claims released",
				len(kept), len(refused)+len(undecodable), session)
		}
		for _, role := range held {
			if slices.Contains(kept, role) {
				t.Fatalf("%s's claim on %s is still in the bucket, want it released by the deregistration", session, role)
			}
		}
	})
}
