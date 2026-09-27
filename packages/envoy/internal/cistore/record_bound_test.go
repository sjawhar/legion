package cistore

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
)

// GitHub allows 50,000 check runs in one check suite, so a head's record, which holds the latest run
// of every check name GitHub reports on it, has no bound of GitHub's own below NATS's max payload.
// The store bounds it (maxRecordBytes): a check that would take it past the bound is refused with
// bus.ErrTooLarge, which the webhook answers as a refusal, and the record says it overflowed, so the
// listener never settles a head it could not record whole.
func TestAHeadRecordRefusesACheckPastItsBoundAndNeverSettles(t *testing.T) {
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

	// A job name at the envelope text cap, of the character JSON writes widest.
	name := func(i int) string { return fmt.Sprintf("%s%04d", strings.Repeat("<", 2044), i) }
	recorded := 0
	var refused error
	for i := range 200 {
		err := recordCheck(store, owner, repo, number, sha, name(i), strconv.Itoa(1000+i),
			"https://github.com/example-org/example-repo/runs/"+strconv.Itoa(1000+i), "completed", "success", "2026-09-27T00:00:00Z")
		if err != nil {
			refused = err
			break
		}
		recorded++
	}
	if !errors.Is(refused, bus.ErrTooLarge) {
		t.Fatalf("after %d checks the store answered %v, want a check past the record's bound refused with bus.ErrTooLarge", recorded, refused)
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

	waitCacheChecks(t, store, owner, repo, number, sha, recorded)
	runSummaryTick(store, pub, 0, logging.New("test"))
	if pub.count() != 0 {
		t.Fatalf("published %d settlements for a head whose record overflowed, want none", pub.count())
	}
}

// The settlement names each check up to three times (its run, its group, and a failure's entry with
// its URL) inside the envelope's payload string, where a character costs up to seven bytes against
// the six it costs in the record, which names it twice. A record at its bound must still settle in
// one publish: every check failed, each named at the envelope text cap in the widest character.
func TestASettlementFromARecordAtItsBoundFitsNATS(t *testing.T) {
	const natsDefaultMaxPayload = 1 << 20
	state := State{Owner: "example-org", Repo: "example-repo", Number: "42", SHA: strings.Repeat("a", 40), Checks: map[string]Check{}}
	for i := 0; ; i++ {
		name := fmt.Sprintf("%s%04d", strings.Repeat("<", 2044), i)
		state.Checks[name] = Check{
			Name: name, CheckRunID: checkRunID(1_000_000_000_000 + i), Status: "completed", Conclusion: "failure",
			URL: "https://github.com/example-org/example-repo/runs/" + strconv.Itoa(1_000_000_000_000+i), ObservedAt: "2026-09-27T00:00:00Z",
		}
		data, err := json.Marshal(state)
		if err != nil {
			t.Fatalf("encode record: %v", err)
		}
		if len(data) > maxRecordBytes {
			delete(state.Checks, name)
			break
		}
	}
	env, err := settlementEnvelope(state, 1_790_000_000_000)
	if err != nil {
		t.Fatalf("settlement envelope: %v", err)
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("encode settlement: %v", err)
	}
	if len(data)+len(env.Topic) > natsDefaultMaxPayload {
		t.Fatalf("a settlement of %d checks is %d bytes, past NATS's default max payload (%d)", len(state.Checks), len(data), natsDefaultMaxPayload)
	}
}
