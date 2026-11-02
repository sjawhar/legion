package cistore

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The store accepts a check only while the settlement it would publish stays within
// maxSettlementBytes, 64 KiB under NATS's 1 MiB default max payload. A commit whose settlement is
// at that bound, of failing checks named in `"` (12 bytes a character there), settles through the
// listener's real publisher and a real NATS server at the default max payload: the stream stores
// it, and the record is settled rather than overflowed.
func TestASettlementAtItsBoundIsStored(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the bus: %v", err)
	}
	defer client.Close()
	if got := client.Conn.MaxPayload(); got != natsDefaultMaxPayload {
		t.Fatalf("the server's max payload is %d, want the %d default", got, natsDefaultMaxPayload)
	}
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)
	url := func(i int) string { return "https://github.com/example-org/example-repo/runs/" + strconv.Itoa(1000+i) }
	name := func(i, runes int) string { return fmt.Sprintf("%s%04d", strings.Repeat(`"`, runes-4), i) }
	// sizeWith is the size of the settlement the record would publish with one more failing check,
	// named name, which the store would accept only within maxSettlementBytes. A few bytes are
	// left for what the record's later writes add (a longer generation).
	const slack = 16
	sizeWith := func(i int, name string) int {
		t.Helper()
		state := getState(t, store, owner, repo, number, sha)
		state.Checks[name] = Check{Name: name, CheckRunID: checkRunID(1000 + i), URL: url(i), Status: "completed", Conclusion: "failure", ObservedAt: "2026-09-27T00:00:00Z"}
		size, err := settlementSize(state)
		if err != nil {
			t.Fatalf("settlement size: %v", err)
		}
		return size + slack
	}
	record := func(i int, name string) {
		t.Helper()
		if err := recordCheck(store, owner, repo, number, sha, name, strconv.Itoa(1000+i), url(i), "completed", "failure", "2026-09-27T00:00:00Z"); err != nil {
			t.Fatalf("record check %d: %v", i, err)
		}
	}

	// Checks named at the envelope text cap while the next one fits, then one whose name is as long
	// as still fits.
	record(0, name(0, 2048))
	checks := 1
	for ; sizeWith(checks, name(checks, 2048)) <= maxSettlementBytes; checks++ {
		record(checks, name(checks, 2048))
	}
	last := sort.Search(2048-4, func(runes int) bool { return sizeWith(checks, name(checks, runes+5)) > maxSettlementBytes }) + 4
	record(checks, name(checks, last))
	checks++
	state := getState(t, store, owner, repo, number, sha)
	size, err := settlementSize(state)
	if err != nil {
		t.Fatalf("settlement size: %v", err)
	}
	if state.Overflowed || size > maxSettlementBytes || size < maxSettlementBytes-64 {
		t.Fatalf("the record of %d checks settles in %d bytes (overflowed %v), want within 64 bytes under its %d-byte bound",
			checks, size, state.Overflowed, maxSettlementBytes)
	}

	waitCacheChecks(t, store, owner, repo, number, sha, checks)
	runSummaryTick(store, client, 0, logging.New("test"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stored, err := client.NextStoredMessage(ctx, "notifications.github.example-org.example-repo.pr.42.checks", 0)
	if err != nil {
		t.Fatalf("read the stored settlement: %v", err)
	}
	if len(stored.Data) < maxSettlementBytes-64 {
		t.Fatalf("the stored settlement is %d bytes, want the %d-byte one the record settles in", len(stored.Data), size)
	}
	if got := getState(t, store, owner, repo, number, sha); !got.SettledEmitted || got.Overflowed {
		t.Fatalf("after its settlement was stored the record is settled %v, overflowed %v; want settled", got.SettledEmitted, got.Overflowed)
	}
	t.Logf("the stored settlement of %d checks is %d bytes", checks, len(stored.Data))
}
