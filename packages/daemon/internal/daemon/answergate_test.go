package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

const gateToken = claim.Token("legion-legion-legion-9-tester")

// launchChecks is the store under the gate: it records each start's check and refuses as told.
type launchChecks struct {
	supervise.Store
	mu      sync.Mutex
	checked []uint64
	refuse  error
}

func (s *launchChecks) CheckLaunch(_ context.Context, c supervise.Claim) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checked = append(s.checked, c.Generation)
	return s.refuse
}

// claimViews is the machine's View as the gate reads it, set by the test.
type claimViews struct {
	mu   sync.Mutex
	view supervise.ClaimView
}

func (v *claimViews) get(token claim.Token) (supervise.ClaimView, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.view, token == gateToken
}

func (v *claimViews) set(state supervise.ClaimState, generation uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.view.State, v.view.Generation = state, generation
}

func newTestGate(t *testing.T) (*answerGate, *launchChecks, *claimViews) {
	t.Helper()
	inner := &launchChecks{}
	views := &claimViews{view: supervise.ClaimView{Claim: supervise.Claim{Token: gateToken, Session: "ses_tester", State: supervise.StateSuspended, Generation: 3}}}
	gate := newAnswerGate(inner, views.get, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return gate, inner, views
}

// A claim started while an answer is being added to its conversation waits for the append to end
// before its check passes; from then on the gate refuses the loop the conversation, so the next
// answer appends nothing (it is forwarded), until the process that start began has ended.
func TestAStartDuringAnAppendWaitsForItAndThenHoldsTheConversation(t *testing.T) {
	gate, inner, views := newTestGate(t)
	appending, release := make(chan struct{}), make(chan struct{})
	appended := make(chan struct{})
	go func() {
		ran, err := gate.Append(gateToken, "ses_tester", func() error {
			close(appending)
			<-release
			return nil
		})
		if !ran || err != nil {
			t.Errorf("the first append = %t, %v; want it written", ran, err)
		}
		close(appended)
	}()
	<-appending
	started := make(chan error, 1)
	go func() { started <- gate.CheckLaunch(context.Background(), views.view.Claim) }()
	select {
	case err := <-started:
		t.Fatalf("the start passed its check (%v) while the answer was being appended", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-appended
	if err := <-started; err != nil {
		t.Fatalf("the start's check after the append = %v", err)
	}
	if len(inner.checked) != 1 {
		t.Fatalf("store checks = %v; want the start checked once", inner.checked)
	}

	wrote := false
	ran, err := gate.Append(gateToken, "ses_tester", func() error { wrote = true; return nil })
	if ran || err != nil || wrote || !gate.Running(gateToken, "ses_tester") {
		t.Fatalf("an append after the start = %t, %v, wrote %t; want nothing appended and the claim running", ran, err, wrote)
	}

	views.set(supervise.StateLaunching, 4)
	if !gate.Running(gateToken, "ses_tester") {
		t.Fatal("the launched claim does not run its conversation")
	}
	views.set(supervise.StateSuspended, 4)
	if gate.Running(gateToken, "ses_tester") {
		t.Fatal("the claim still runs its conversation after the process the start began ended")
	}
	if ran, err := gate.Append(gateToken, "ses_tester", func() error { wrote = true; return nil }); !ran || err != nil || !wrote {
		t.Fatalf("an append once the claim stopped = %t, %v, wrote %t; want it written", ran, err, wrote)
	}
}

// A start waits for an append at most the gate's bound, then starts anyway.
func TestAStartWaitsForAnAppendOnlyAsLongAsTheBound(t *testing.T) {
	gate, _, views := newTestGate(t)
	gate.wait = 50 * time.Millisecond
	appending, release := make(chan struct{}), make(chan struct{})
	go func() {
		_, _ = gate.Append(gateToken, "ses_tester", func() error { close(appending); <-release; return nil })
	}()
	<-appending
	defer close(release)
	begun := time.Now()
	if err := gate.CheckLaunch(context.Background(), views.view.Claim); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(begun); waited < gate.wait {
		t.Fatalf("the start waited %s; want the bound %s", waited, gate.wait)
	}
	if !gate.Running(gateToken, "ses_tester") {
		t.Fatal("the start past the bound is not marked started")
	}
}

// A start the store underneath refuses starts nothing, so the conversation stays the loop's; a
// live claim runs its current conversation and not an older one a newer session replaced.
func TestARefusedStartAndAnOlderConversationLeaveTheLoopItsAppend(t *testing.T) {
	gate, inner, views := newTestGate(t)
	inner.refuse = errors.New("the tree's cleanup is reserved")
	if err := gate.CheckLaunch(context.Background(), views.view.Claim); err == nil {
		t.Fatal("the refused start passed its check")
	}
	if gate.Running(gateToken, "ses_tester") {
		t.Fatal("a refused start marked the claim started")
	}
	views.set(supervise.StateWorking, 4)
	if !gate.Running(gateToken, "ses_tester") || gate.Running(gateToken, "ses_older") {
		t.Fatal("a working claim must run its current conversation and no older one")
	}
	if ran, err := gate.Append(gateToken, "ses_older", func() error { return nil }); !ran || err != nil {
		t.Fatalf("an append to an older conversation = %t, %v; want it written", ran, err)
	}
}
