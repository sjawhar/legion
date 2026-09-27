package cistore

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
)

const natsDefaultMaxPayload = 1 << 20

// GitHub allows 50,000 check runs in one check suite, so a head's record, which holds the latest run
// of every check name GitHub reports on it, has no bound of GitHub's own below NATS's max payload,
// and neither has the settlement published from it. The store bounds both: a check that would take
// the record past maxRecordBytes, or its settlement past maxSettlementBytes, is refused with
// bus.ErrTooLarge, which the webhook answers as a refusal, and the record says it overflowed, so the
// listener never settles a head it could not record whole. Which bound a head meets first depends on
// the names: a `<` costs 6 bytes in the record, twice, and 7 in the settlement, three times over for
// a failing check; a `"` costs 2 in the record and 4 in the settlement, since the settlement is a JSON
// string inside the envelope's JSON.
func TestAHeadRecordRefusesACheckPastItsBoundsAndNeverSettles(t *testing.T) {
	for _, tc := range []struct {
		name       string
		char       string
		conclusion string
		bound      string
	}{
		{"passing checks named in `<`, which meet the record's bound", "<", "success", "record"},
		{"failing checks named in `\"`, which meet the settlement's bound", `"`, "failure", "settlement"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, cleanup := connectNATS(t)
			defer cleanup()
			store := openStore(t, conn)
			pub := &recPub{}
			const (
				owner  = "example-org"
				repo   = "example-repo"
				number = "42"
				sha    = "abcdef1234567890abcdef1234567890abcdef12"
			)
			if err := store.RecordHead(owner, repo, number, sha, "2026-09-27T00:00:00Z"); err != nil {
				t.Fatalf("record head: %v", err)
			}
			waitHead(t, store, owner, repo, number, sha)

			// A job name at the envelope text cap.
			name := func(i int) string { return fmt.Sprintf("%s%04d", strings.Repeat(tc.char, 2044), i) }
			recorded := 0
			var refused error
			for i := range 200 {
				err := recordCheck(store, owner, repo, number, sha, name(i), strconv.Itoa(1000+i),
					"https://github.com/example-org/example-repo/runs/"+strconv.Itoa(1000+i), "completed", tc.conclusion, "2026-09-27T00:00:00Z")
				if err != nil {
					refused = err
					break
				}
				recorded++
			}
			if !errors.Is(refused, bus.ErrTooLarge) || !strings.Contains(refused.Error(), "'s "+tc.bound+" would be") {
				t.Fatalf("after %d checks the store answered %v, want a check past the %s's bound refused with bus.ErrTooLarge", recorded, refused, tc.bound)
			}

			entry, err := store.watcher.KV().Get(Key(owner, repo, number, sha))
			if err != nil {
				t.Fatalf("read the record: %v", err)
			}
			if len(entry.Value()) > maxRecordBytes {
				t.Fatalf("the record is %d bytes, past its %d-byte bound", len(entry.Value()), maxRecordBytes)
			}
			var stored State
			if err := json.Unmarshal(entry.Value(), &stored); err != nil {
				t.Fatalf("decode the record: %v", err)
			}
			if !stored.Overflowed || len(stored.Checks) != recorded {
				t.Fatalf("record overflowed=%v with %d checks, want overflowed with the %d it recorded", stored.Overflowed, len(stored.Checks), recorded)
			}
			// Every check the store accepted still settles in one publish.
			accepted := stored
			accepted.Overflowed = false
			settlement, err := settlementSize(accepted)
			if err != nil {
				t.Fatalf("settlement size: %v", err)
			}
			if settlement > natsDefaultMaxPayload {
				t.Fatalf("the %d checks the store accepted settle in %d bytes, past NATS's default max payload (%d)", recorded, settlement, natsDefaultMaxPayload)
			}

			waitCacheChecks(t, store, owner, repo, number, sha, recorded)
			runSummaryTick(store, pub, 0, logging.New("test"))
			if pub.count() != 0 {
				t.Fatalf("published %d settlements for a head whose record overflowed, want none", pub.count())
			}
		})
	}
}

// refusingPub refuses every settlement as NATS refuses one it cannot take whole, and counts them.
type refusingPub struct {
	mu       sync.Mutex
	attempts int
}

func (p *refusingPub) Publish(contracts.Envelope) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attempts++
	return fmt.Errorf("publish: %w", bus.ErrTooLarge)
}

func (p *refusingPub) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attempts
}

// NATS refuses a settlement the same way on every tick, so a refused one is terminal: the record is
// marked overflowed and the head is not published again, where a publish that merely failed is
// retried on the next tick. A server whose max payload is set below the store's bound refuses one
// the store accepted.
func TestASettlementNATSRefusesIsNotPublishedAgain(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &refusingPub{}
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "43"
		sha    = "1234567890abcdef1234567890abcdef12345678"
	)
	if err := store.RecordHead(owner, repo, number, sha, "2026-09-27T00:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}
	waitHead(t, store, owner, repo, number, sha)
	if err := recordCheck(store, owner, repo, number, sha, "build", "900", "https://github.com/example-org/example-repo/runs/900", "completed", "success", "2026-09-27T00:00:00Z"); err != nil {
		t.Fatalf("record check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)

	runSummaryTick(store, pub, 0, logging.New("test"))
	if pub.count() != 1 {
		t.Fatalf("the first tick attempted %d settlements, want one", pub.count())
	}
	if got := getState(t, store, owner, repo, number, sha); !got.Overflowed {
		t.Fatal("the record of a settlement NATS refused is not marked overflowed")
	}
	runSummaryTick(store, pub, 0, logging.New("test"))
	if pub.count() != 1 {
		t.Fatalf("a settlement NATS refused was attempted %d times, want once", pub.count())
	}
}
