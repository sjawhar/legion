package requests

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/dispatch"
)

// fakeAskReader is the poller's askReader: it answers GetAsk from a map the test mutates between
// RunOnce calls, and fails loudly on an id it wasn't given so an unexpected read shows up in the
// test rather than silently returning a zero Ask.
type fakeAskReader struct {
	asks map[string]dispatch.Ask
}

func (f *fakeAskReader) GetAsk(_ context.Context, id string) (dispatch.Ask, error) {
	ask, ok := f.asks[id]
	if !ok {
		return dispatch.Ask{}, fmt.Errorf("fakeAskReader: no ask registered for %q", id)
	}
	return ask, nil
}

// wakeCall records one Poller.Wake invocation for a test to assert against.
type wakeCall struct{ enrollmentID, requestID, state string }

func recordingWake(calls *[]wakeCall) func(context.Context, string, string, string) {
	return func(_ context.Context, enrollmentID, requestID, state string) {
		*calls = append(*calls, wakeCall{enrollmentID, requestID, state})
	}
}

// TestPollerRunOnceGrantsAndDeniesAcrossPolls pins the poller's core loop: it reads every pending
// row from Postgres (never from memory), asks Dispatch for each ask's authoritative state, applies
// the answer, and only then wakes Envoy. Two requests are pending; the first RunOnce grants the one
// Dispatch already answered and leaves the other's still-open ask untouched, waking exactly once
// with "granted". A second RunOnce, after that ask resolves without an approval, denies it and
// wakes exactly once with "denied" -- the already-decided first request is never touched again.
func TestPollerRunOnceGrantsAndDeniesAcrossPolls(t *testing.T) {
	m, _, _, enrA, enrB := newFixture(t)
	ctx := context.Background()

	reqA, err := m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create(reqA): %v", err)
	}
	reqB, err := m.Create(ctx, enrB.ID.String(), []string{"DEEL_API_KEY"}, "need it too", "", "")
	if err != nil {
		t.Fatalf("Create(reqB): %v", err)
	}

	reader := &fakeAskReader{asks: map[string]dispatch.Ask{
		"ask-1": {ID: "ask-1", State: "answered", Answer: &dispatch.Answer{User: "sjawhar", Selected: []string{"Approve"}, At: time.Now()}},
		"ask-2": {ID: "ask-2", State: "open"},
	}}
	var wakes []wakeCall
	poller := &Poller{Machine: m, Dispatch: reader, Interval: time.Minute, Wake: recordingWake(&wakes)}

	if err := poller.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce (first): %v", err)
	}
	if len(wakes) != 1 || wakes[0] != (wakeCall{enrA.ID.String(), reqA.ID, "granted"}) {
		t.Fatalf("wakes = %+v, want one granted wake for reqA", wakes)
	}
	gotA, err := m.Get(ctx, reqA.ID)
	if err != nil {
		t.Fatalf("Get(reqA): %v", err)
	}
	if gotA.State != "granted" {
		t.Fatalf("reqA state = %q, want granted", gotA.State)
	}
	gotB, err := m.Get(ctx, reqB.ID)
	if err != nil {
		t.Fatalf("Get(reqB): %v", err)
	}
	if gotB.State != "pending" {
		t.Fatalf("reqB state = %q, want pending", gotB.State)
	}

	reader.asks["ask-2"] = dispatch.Ask{ID: "ask-2", State: "resolved"}
	wakes = nil
	if err := poller.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce (second): %v", err)
	}
	if len(wakes) != 1 || wakes[0] != (wakeCall{enrB.ID.String(), reqB.ID, "denied"}) {
		t.Fatalf("wakes = %+v, want one denied wake for reqB", wakes)
	}
	gotB, err = m.Get(ctx, reqB.ID)
	if err != nil {
		t.Fatalf("Get(reqB) after second poll: %v", err)
	}
	if gotB.State != "denied" {
		t.Fatalf("reqB state = %q, want denied", gotB.State)
	}
}

// TestPollerRunOnceExpiresPending pins that RunOnce calls ExpirePending before reading pending
// rows: a request whose pending_expires_at is backdated is expired in the same pass, never reaches
// Dispatch (the fake has no registered ask, so a stray GetAsk call fails the test), and never wakes.
func TestPollerRunOnceExpiresPending(t *testing.T) {
	m, _, _, enrA, _ := newFixture(t)
	ctx := context.Background()

	req, err := m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.Store.Pool.Exec(ctx, `update requests set pending_expires_at = now() - interval '1 hour' where id=$1`, req.ID); err != nil {
		t.Fatalf("backdate pending_expires_at: %v", err)
	}

	poller := &Poller{
		Machine:  m,
		Dispatch: &fakeAskReader{asks: map[string]dispatch.Ask{}},
		Interval: time.Minute,
		Wake: func(context.Context, string, string, string) {
			t.Fatal("Wake should not be called for an expired request")
		},
	}
	if err := poller.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got, err := m.Get(ctx, req.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != "expired" {
		t.Fatalf("state = %q, want expired", got.State)
	}
}

// TestPollerRunOnceOnFreshMachineReadsPendingFromStore pins restart safety: a Poller built around a
// brand new *Machine value -- sharing only the Store, none of the Rules/Secrets/etc a running
// broker would normally carry -- still finds and correctly grants a pending row that was inserted
// before it existed, because RunOnce reads every pending row from Postgres rather than any
// in-memory state.
func TestPollerRunOnceOnFreshMachineReadsPendingFromStore(t *testing.T) {
	m, _, _, enrA, _ := newFixture(t)
	ctx := context.Background()

	req, err := m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	fresh := &Machine{Store: m.Store}
	reader := &fakeAskReader{asks: map[string]dispatch.Ask{
		"ask-1": {ID: "ask-1", State: "answered", Answer: &dispatch.Answer{User: "sjawhar", Selected: []string{"Approve"}, At: time.Now()}},
	}}
	var wakes []wakeCall
	poller := &Poller{Machine: fresh, Dispatch: reader, Interval: time.Minute, Wake: recordingWake(&wakes)}

	if err := poller.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(wakes) != 1 || wakes[0] != (wakeCall{enrA.ID.String(), req.ID, "granted"}) {
		t.Fatalf("wakes = %+v, want one granted wake for req", wakes)
	}
	got, err := m.Get(ctx, req.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != "granted" {
		t.Fatalf("state = %q, want granted", got.State)
	}
}
