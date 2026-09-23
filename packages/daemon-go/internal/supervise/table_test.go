package supervise

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// sealed reads the package's own source for what the table has to cover: every type that
// implements the sealed Event interface, every ClaimState, and every TimerKind. It reads the
// source rather than a list kept beside the table, so a new event type, state, or timer that
// nobody gave rows to fails this test instead of passing it.
func sealed(t *testing.T) (events []string, states []ClaimState, timers []TimerKind) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(files, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Name.Name == "isEvent" && decl.Recv != nil {
					if ident, ok := decl.Recv.List[0].Type.(*ast.Ident); ok {
						events = append(events, ident.Name)
					}
				}
			case *ast.GenDecl:
				if decl.Tok != token.CONST {
					continue
				}
				for _, spec := range decl.Specs {
					value := spec.(*ast.ValueSpec)
					typ, ok := value.Type.(*ast.Ident)
					if !ok || len(value.Values) != 1 {
						continue
					}
					literal, ok := value.Values[0].(*ast.BasicLit)
					if !ok {
						continue
					}
					text, err := strconv.Unquote(literal.Value)
					if err != nil {
						t.Fatal(err)
					}
					switch typ.Name {
					case "ClaimState":
						states = append(states, ClaimState(text))
					case "TimerKind":
						timers = append(timers, TimerKind(text))
					}
				}
			}
		}
	}
	if len(events) == 0 || len(states) == 0 || len(timers) == 0 {
		t.Fatalf("read no sealed set from the source: events %v, states %v, timers %v", events, states, timers)
	}
	return events, states, timers
}

// samples is one event of every kind the table distinguishes, by Go type. A Timer is one kind per
// TimerKind.
func samples(t *testing.T) map[string][]Event {
	t.Helper()
	events, _, timers := sealed(t)
	byType := map[string][]Event{
		"RuntimeObservation": {RuntimeObservation{}},
		"StreamHello":        {StreamHello{}},
		"StreamTurnStart":    {StreamTurnStart{}},
		"StreamTurnEnd":      {StreamTurnEnd{}},
		"StreamClosed":       {StreamClosed{}},
		"StreamLateRefusal":  {StreamLateRefusal{}},
		"PromptAcked":        {PromptAcked{}},
		"PromptRefused":      {PromptRefused{}},
		"RequestSpawn":       {RequestSpawn{}},
		"RequestRegister":    {RequestRegister{}},
		"RequestReady":       {RequestReady{}},
		"RequestSuspend":     {RequestSuspend{}},
		"RequestResume":      {RequestResume{}},
		"RequestStop":        {RequestStop{}},
		"RequestRetry":       {RequestRetry{}},
		"RequestDeliver":     {RequestDeliver{}},
		"RequestExit":        {RequestExit{}},
	}
	for _, kind := range timers {
		byType["Timer"] = append(byType["Timer"], Timer{Kind: kind})
	}
	for _, name := range events {
		if _, ok := byType[name]; !ok {
			t.Errorf("event type %s has no sample here: add one, and give it a row or a named ignore in every state", name)
		}
	}
	return byType
}

func TestTheTableHasARowOrANamedIgnoreForEveryEventInEveryState(t *testing.T) {
	_, states, _ := sealed(t)
	covered := map[key]bool{}
	for name, events := range samples(t) {
		for _, ev := range events {
			for _, state := range states {
				k := key{state, kindOf(ev)}
				covered[k] = true
				r, ok := table[k]
				if !ok {
					t.Errorf("%s in %s (kind %q): no row and no named ignore", name, state, k.kind)
					continue
				}
				switch {
				case r.act != nil && r.ignore != "":
					t.Errorf("%s in %s: both a row (%s) and an ignore (%s)", name, state, r.name, r.ignore)
				case r.act == nil && r.ignore == "":
					t.Errorf("%s in %s: neither a row nor a named ignore", name, state)
				case r.act != nil && r.name == "":
					t.Errorf("%s in %s: a row with no name", name, state)
				case r.act == nil && len(r.to) > 0:
					t.Errorf("%s in %s: an ignore that names destination states %v", name, state, r.to)
				}
			}
		}
	}
	for k := range table {
		if !covered[k] {
			t.Errorf("row %s/%s is keyed on a state or event kind the sealed set does not have", k.state, k.kind)
		}
	}
}

// fixture is a claim restored in state with everything an event needs to pass its fences: the
// generation, a recorded session, a locator for the states that have a process, and a pending
// delivery (confirmed while working, as a delivered turn leaves it).
func fixture(state ClaimState) Claim {
	c := queuedClaim()
	c.State = state
	c.Generation = 3
	c.Pending = &Delivery{ID: "delivery-1", Task: "the task", QueuedAt: epoch}
	switch state {
	case StateQueued:
		c.Generation = 0
	case StateLaunching, StateShimConnected, StateRegistered, StateReady, StateWorking, StateIdle:
		c.Locator = &runtime.Locator{
			Runtime:     runtime.RuntimeTmux,
			Claim:       testToken,
			Incarnation: "4242:77",
			Tmux:        &runtime.TmuxLocator{Window: "@1", Pane: "%1"},
		}
	}
	switch state {
	case StateRegistered, StateReady, StateWorking, StateIdle, StateSuspended, StateFailed, StateRetired:
		c.Session, c.SessionFile = session, sessionFile
	}
	if state == StateWorking {
		c.Pending.DeliveredAt, c.Pending.ConfirmedAt = epoch, epoch
	}
	return c
}

// fenced fills a sample in with the fixture's own generation, incarnation, session, and delivery,
// so that it reaches the table rather than stopping at a fence.
func fenced(ev Event, c Claim) Event {
	switch ev := ev.(type) {
	case RuntimeObservation:
		// A claim with no process can only be told about an incarnation it does not hold.
		ev.Observation.Locator = runtime.Locator{Claim: c.Token, Incarnation: "1:1"}
		if c.Locator != nil {
			ev.Observation.Locator = *c.Locator
		}
		ev.Observation.Kind = runtime.Alive
		return ev
	case StreamHello:
		return StreamHello{Claim: c.Token, Generation: c.Generation}
	case StreamTurnStart:
		return StreamTurnStart{Claim: c.Token}
	case StreamTurnEnd:
		return StreamTurnEnd{Claim: c.Token}
	case StreamClosed:
		return StreamClosed{Claim: c.Token}
	case StreamLateRefusal:
		return StreamLateRefusal{Claim: c.Token, DeliveryID: c.Pending.ID, Error: "busy"}
	case PromptAcked:
		return PromptAcked{Claim: c.Token, Generation: c.Generation, DeliveryID: c.Pending.ID}
	case PromptRefused:
		return PromptRefused{Claim: c.Token, Generation: c.Generation, DeliveryID: c.Pending.ID, Err: errBoom}
	case Timer:
		return Timer{Claim: c.Token, Kind: ev.Kind, Generation: c.Generation, Seq: 7777, DeliveryID: c.Pending.ID}
	case RequestSpawn:
		return RequestSpawn{Claim: c.Token}
	case RequestRegister:
		return RequestRegister{Claim: c.Token, Generation: c.Generation, Session: session, SessionFile: sessionFile}
	case RequestReady:
		return RequestReady{Claim: c.Token, Generation: c.Generation, Session: session}
	case RequestSuspend:
		return RequestSuspend{Claim: c.Token}
	case RequestResume:
		return RequestResume{Claim: c.Token}
	case RequestStop:
		return RequestStop{Claim: c.Token}
	case RequestRetry:
		return RequestRetry{Claim: c.Token}
	case RequestDeliver:
		return RequestDeliver{Claim: c.Token, Task: "another task"}
	case RequestExit:
		return RequestExit{Claim: c.Token, Generation: c.Generation, Session: session, Reason: "done"}
	}
	panic("unhandled sample")
}

// Every named ignore does what it says: the claim does not move, nothing is asked of the runtime
// or the agent, nothing is written — and a request the state does not allow is refused, naming
// the state, rather than answered as if it had happened.
func TestEveryNamedIgnoreChangesNothingAndRefusesARequest(t *testing.T) {
	_, states, _ := sealed(t)
	for _, events := range samples(t) {
		for _, sample := range events {
			for _, state := range states {
				r := table[key{state, kindOf(sample)}]
				if r.ignore == "" {
					continue
				}
				t.Run(string(state)+"/"+string(kindOf(sample)), func(t *testing.T) {
					h := newBareHarness(t)
					c := fixture(state)
					if err := h.store.PutClaim(h.ctx, c); err != nil {
						t.Fatal(err)
					}
					if err := h.store.PutDelivery(h.ctx, c.Token, *c.Pending); err != nil {
						t.Fatal(err)
					}
					h.conns.Register(testToken, h.conn)
					h.start(c)
					ev := fenced(sample, c)
					if timer, ok := ev.(Timer); ok {
						h.m.timers[timer.Kind] = armed{seq: timer.Seq, deliveryID: timer.DeliveryID}
					}
					before := h.claim()
					puts := len(h.store.history())

					err := h.handle(ev)

					if wantsRefusal(ev) {
						var refused *RefusedError
						if !errors.As(err, &refused) || refused.State != state {
							t.Errorf("refusal = %v, want a RefusedError naming %s", err, state)
						}
					} else if err != nil {
						t.Errorf("ignored event returned %v", err)
					}
					after := h.claim()
					if after.State != before.State || after.Budgets != before.Budgets || after.Generation != before.Generation {
						t.Errorf("claim moved: %+v -> %+v", before, after)
					}
					if calls := h.rt.Methods(); len(calls) > 0 {
						t.Errorf("asked the runtime %v", calls)
					}
					if prompts := h.conn.Prompts(); len(prompts) > 0 {
						t.Errorf("prompted the agent %+v", prompts)
					}
					if got := len(h.store.history()); got != puts {
						t.Errorf("wrote the claim %d times", got-puts)
					}
				})
			}
		}
	}
}

// The machine holds an action to its row: a row that names where it may leave the claim cannot
// leave it anywhere else.
func TestARowCannotMoveTheClaimOutsideTheStatesItNames(t *testing.T) {
	h := newHarness(t)
	k := key{StateQueued, kindOf(RequestSpawn{})}
	saved := table[k]
	t.Cleanup(func() { table[k] = saved })
	table[k] = rule{name: "a rogue row", to: []ClaimState{StateLaunching}, act: func(m *Machine, _ context.Context, _ Event) error {
		m.claim.State = StateWorking
		return nil
	}}

	err := h.handle(RequestSpawn{Claim: testToken})
	if err == nil || !strings.Contains(err.Error(), "a rogue row") {
		t.Fatalf("a row that moved the claim to working, outside [launching], returned %v", err)
	}
}

func TestAnEventForAnotherClaimIsAnError(t *testing.T) {
	h := newHarness(t)
	other := claim.Token("legion-LEGION-209-tester")
	for _, ev := range []Event{
		RequestSpawn{Claim: other},
		StreamHello{Claim: other, Generation: 1},
		RuntimeObservation{Observation: runtime.Observation{Locator: runtime.Locator{Claim: other}}},
	} {
		if err := h.handle(ev); err == nil || !strings.Contains(err.Error(), string(other)) {
			t.Errorf("%T for %s handled by %s's machine: %v", ev, other, testToken, err)
		}
	}
	if calls := h.rt.Methods(); len(calls) > 0 {
		t.Errorf("asked the runtime %v", calls)
	}
}

// wantsRefusal is whether an ignored event is a request, which its caller is told was refused.
func wantsRefusal(ev Event) bool {
	switch ev.(type) {
	case RequestSpawn, RequestRegister, RequestReady, RequestSuspend, RequestResume, RequestStop,
		RequestRetry, RequestDeliver, RequestExit:
		return true
	}
	return false
}
