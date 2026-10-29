package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
)

// fakeLiveKind stands in for the events and agent views, which a live socket starts as it starts
// any kind beside the delivery views. Each subscription it starts sends frames events of its own
// (LiveEventFrame, numbered from 1, each carrying pad bytes), or sends until stopped when frames
// is negative, and then holds until stopped. It hands every subscription it starts to the test.
type fakeLiveKind struct {
	frames  int
	pad     int
	started chan *fakeLiveSubscription
}

func newFakeLiveKind(frames, pad int) *fakeLiveKind {
	return &fakeLiveKind{frames: frames, pad: pad, started: make(chan *fakeLiveSubscription, 256)}
}

// kind is the liveKind a test server holds; resumes is whether it takes an EventsLiveResume.
func (k *fakeLiveKind) kind(resumes bool) liveKind {
	return liveKind{resumes: resumes, start: k.start}
}

func (k *fakeLiveKind) start(ctx context.Context, stream liveStreamer, sub string, view LiveView, resume *EventsLiveResume) (liveSubscription, *LiveError) {
	ctx, cancel := context.WithCancel(ctx)
	running := &fakeLiveSubscription{sub: sub, view: view, resume: resume, human: stream.Human(), cancel: cancel, done: make(chan struct{})}
	event := json.RawMessage(`{"pad":"` + strings.Repeat("x", k.pad) + `"}`)
	go func() {
		defer close(running.done)
		for n := 1; k.frames < 0 || n <= k.frames; n++ {
			if err := stream.SendWait(ctx, LiveEventFrame{Type: "event", Sub: sub, ID: strconv.Itoa(n), Event: event}); err != nil {
				running.err = err
				return
			}
			running.sent.Add(1)
		}
		<-ctx.Done()
	}()
	k.started <- running
	return running, nil
}

// next is the next subscription the kind starts, within within.
func (k *fakeLiveKind) next(t *testing.T, within time.Duration, what string) *fakeLiveSubscription {
	t.Helper()
	select {
	case running := <-k.started:
		return running
	case <-time.After(within):
		t.Fatalf("no subscription started within %s, waiting for %s", within, what)
		return nil
	}
}

// none fails when the kind starts a subscription within within.
func (k *fakeLiveKind) none(t *testing.T, within time.Duration, what string) {
	t.Helper()
	select {
	case running := <-k.started:
		t.Fatalf("%s: started %q", what, running.sub)
	case <-time.After(within):
	}
}

// fakeLiveSubscription is one subscription a fakeLiveKind started.
type fakeLiveSubscription struct {
	sub    string
	view   LiveView
	resume *EventsLiveResume
	human  bool
	cancel context.CancelFunc
	// done closes once the subscription's goroutine returns; err is then what SendWait answered
	// that ended its sending, nil when it sent every frame.
	done  chan struct{}
	err   error
	sent  atomic.Int64
	stops atomic.Int32
}

func (f *fakeLiveSubscription) stop() {
	f.stops.Add(1)
	f.cancel()
	<-f.done
}

// ended waits within within for the subscription's goroutine to return, and answers what SendWait
// answered that ended its sending.
func (f *fakeLiveSubscription) ended(t *testing.T, within time.Duration) error {
	t.Helper()
	select {
	case <-f.done:
		return f.err
	case <-time.After(within):
		t.Fatalf("subscription %q still running after %s", f.sub, within)
		return nil
	}
}

// liveSubscribe is a page's subscribe frame for view, with resume as its raw JSON when not empty.
func liveSubscribe(sub string, view LiveView, resume string) LiveSubscribe {
	frame := LiveSubscribe{Type: "subscribe", Sub: sub, View: view}
	if resume != "" {
		frame.Resume = json.RawMessage(resume)
	}
	return frame
}

func eventsView() LiveView { return LiveView{DeliveryLiveView: DeliveryLiveView{Kind: liveEventsView}} }

func agentView(session string) LiveView {
	return LiveView{DeliveryLiveView: DeliveryLiveView{Kind: liveAgentView}, SessionID: &session}
}

// hubSubs is how many subscriptions the hub holds, of every socket.
func hubSubs(hub *deliveryHub) int {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	held := 0
	for _, subs := range hub.conns {
		held += len(subs)
	}
	return held
}

// waitStalled waits until running has queued nothing for 300 ms with socket's stream queue full:
// the page that reads nothing has stopped the socket's writer, which is no longer writing.
func waitStalled(t *testing.T, socket *liveSocket, running *fakeLiveSubscription) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	last, since := running.sent.Load(), time.Now()
	for time.Since(since) < 300*time.Millisecond {
		if time.Now().After(deadline) {
			t.Fatalf("the writer still took frames of %q after 10s with the page reading nothing", running.sub)
		}
		time.Sleep(20 * time.Millisecond)
		if sent := running.sent.Load(); sent != last || !streamFull(socket) {
			last, since = sent, time.Now()
		}
	}
}

func TestASubscriptionPastTheCapIsRefused(t *testing.T) {
	events := newFakeLiveKind(0, 0)
	rig := newLiveSocketRig(t, testServerOptions{liveKinds: map[string]liveKind{liveEventsView: events.kind(true)}}, nil)
	client := rig.dial(t, asUser("alice"))

	// The cap counts every kind: four delivery views, which the hub holds once it has told each
	// delivery is not configured, and events subscriptions for the rest.
	const deliveryViews = 4
	var delivery []string
	for n := range deliveryViews {
		sub := "d" + strconv.Itoa(n)
		delivery = append(delivery, sub)
		client.send(t, deliverySubscribe(sub, livePipelineView(nil)))
	}
	running := map[string]*fakeLiveSubscription{}
	for n := range contracts.LiveSocketMaxSubscriptions - deliveryViews {
		client.send(t, liveSubscribe("e"+strconv.Itoa(n), eventsView(), ""))
		started := events.next(t, 5*time.Second, "events subscription "+strconv.Itoa(n))
		running[started.sub] = started
	}
	client.refusals(t, 10*time.Second, "DELIVERY_NOT_CONFIGURED", delivery...)

	// One more, of either kind, is refused, and starts nothing.
	client.send(t, deliverySubscribe("over-d", livePipelineView(nil)))
	client.refusal(t, 3*time.Second, "over-d", "TOO_MANY_SUBSCRIPTIONS")
	client.send(t, liveSubscribe("over-e", eventsView(), ""))
	client.refusal(t, 3*time.Second, "over-e", "TOO_MANY_SUBSCRIPTIONS")
	events.none(t, 200*time.Millisecond, "a subscribe past the cap")
	if held := hubSubs(rig.served.deliveryHub); held != deliveryViews {
		t.Fatalf("the hub holds %d subscriptions, want the %d under the cap", held, deliveryViews)
	}

	// A sub the socket holds is replaced at the cap, not refused.
	client.send(t, liveSubscribe("e0", eventsView(), ""))
	events.next(t, 5*time.Second, "e0's replacement")
	if err := running["e0"].ended(t, 5*time.Second); !errors.Is(err, context.Canceled) && err != nil {
		t.Fatalf("the replaced e0 ended with %v", err)
	}

	// An unsubscribe makes room for one more, and only one.
	client.send(t, LiveUnsubscribe{Type: "unsubscribe", Sub: "e1"})
	running["e1"].ended(t, 5*time.Second)
	client.send(t, liveSubscribe("over-e", eventsView(), ""))
	events.next(t, 5*time.Second, "over-e, once e1 left")
	client.send(t, deliverySubscribe("over-d", livePipelineView(nil)))
	client.refusal(t, 3*time.Second, "over-d", "TOO_MANY_SUBSCRIPTIONS")
}

func TestARepeatedSubReplacesAnotherKind(t *testing.T) {
	events := newFakeLiveKind(-1, 512)
	rig := newLiveSocketRig(t, testServerOptions{liveKinds: map[string]liveKind{liveEventsView: events.kind(true)}},
		func(listener net.Listener) net.Listener { return smallSendBuffers{listener} })
	client := rig.open(t, smallReadDialer(), asUser("alice"))

	// The page reads nothing while x's events subscription fills the stream queue, until the writer
	// is held back.
	client.send(t, liveSubscribe("x", eventsView(), ""))
	first := events.next(t, 5*time.Second, "x's events subscription")
	socket := rig.sockets(t, 1)[0]
	waitStalled(t, socket, first)

	// A delivery view in its place stops it, and the hub's answer waits behind the stalled writer
	// with x's last frames still on the stream queue. None of them follows the answer.
	client.send(t, deliverySubscribe("x", livePipelineView(nil)))
	if err := first.ended(t, 5*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("the replaced events subscription's SendWait answered %v, want its context's end", err)
	}
	pollUntil(t, 10*time.Second, "the hub answers x", func() bool { return len(socket.queue) == 1 })
	if len(socket.stream) == 0 {
		t.Fatal("no frame of x's was left on the stream queue for the answer to overtake")
	}
	client.read()
	deadline := time.Now().Add(10 * time.Second)
	for {
		frame := decodeSocketFrame(t, client.takeBy(t, deadline, 10*time.Second, "the hub's answer for x"))
		if refusal, ok := frame.(LiveError); ok {
			if refusal.Sub != "x" || refusal.Code != "DELIVERY_NOT_CONFIGURED" {
				t.Fatalf("refusal = %+v, want DELIVERY_NOT_CONFIGURED for x", refusal)
			}
			break
		}
	}
	client.quiet(t, 500*time.Millisecond, "after the delivery view took x's place")
	if held := hubSubs(rig.served.deliveryHub); held != 1 {
		t.Fatalf("the hub holds %d subscriptions, want x's", held)
	}

	// An events subscription in a delivery view's place leaves the hub.
	client.send(t, liveSubscribe("x", eventsView(), ""))
	second := events.next(t, 5*time.Second, "x's second events subscription")
	client.event(t, 5*time.Second, "x")
	if held := hubSubs(rig.served.deliveryHub); held != 0 {
		t.Fatalf("the hub holds %d subscriptions once an events subscription took x's place, want none", held)
	}

	// And an events subscription in another's place stops it.
	client.send(t, liveSubscribe("x", eventsView(), ""))
	events.next(t, 5*time.Second, "x's third events subscription")
	if err := second.ended(t, 5*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("the replaced events subscription's SendWait answered %v, want its context's end", err)
	}
	if stops := second.stops.Load(); stops != 1 {
		t.Fatalf("the replaced events subscription was stopped %d times, want once", stops)
	}
}

func TestAResumeOfTheWrongShapeIsAnInvalidFrame(t *testing.T) {
	events := newFakeLiveKind(0, 0)
	agent := newFakeLiveKind(0, 0)
	rig := newLiveSocketRig(t, testServerOptions{liveKinds: map[string]liveKind{
		liveEventsView: events.kind(true),
		liveAgentView:  agent.kind(false),
	}}, nil)
	client := rig.dial(t, asUser("alice"))
	const session = "6f1c5b1e-8a3c-4b7e-9a51-0d2f3c4b5a69"
	deliveryResume := `{"seq":"0","revs":{"items":{},"maps":{},"counts":{},"fields":{}}}`
	eventsResume := `{"since":"7"}`
	pipeline := LiveView{DeliveryLiveView: livePipelineView(nil)}

	for _, test := range []struct {
		name  string
		frame LiveSubscribe
	}{
		{"an events resume on a delivery view", liveSubscribe("a", pipeline, eventsResume)},
		{"a delivery resume on an events view", liveSubscribe("b", eventsView(), deliveryResume)},
		{"an events resume on an agent view", liveSubscribe("c", agentView(session), eventsResume)},
		{"a delivery resume on an agent view", liveSubscribe("d", agentView(session), deliveryResume)},
		{"a resume that is not an object", liveSubscribe("e", eventsView(), `"7"`)},
	} {
		client.send(t, test.frame)
		client.refusal(t, 3*time.Second, test.frame.Sub, "INVALID_FRAME")
	}
	events.none(t, 200*time.Millisecond, "an events view with a resume of another shape")
	agent.none(t, 0, "an agent view with a resume")
	if held := hubSubs(rig.served.deliveryHub); held != 0 {
		t.Fatalf("the hub holds %d subscriptions after a delivery view with an events resume", held)
	}

	// Each kind takes its own: an events view its since, an agent view none (null is none), and a
	// delivery view its seq and revisions.
	client.send(t, liveSubscribe("f", eventsView(), eventsResume))
	if started := events.next(t, 5*time.Second, "an events view with its resume"); started.resume == nil || started.resume.Since != "7" {
		t.Fatalf("the events subscription started with resume %+v, want since 7", started.resume)
	}
	client.send(t, liveSubscribe("g", agentView(session), ""))
	if started := agent.next(t, 5*time.Second, "an agent view"); started.resume != nil || started.view.SessionID == nil || *started.view.SessionID != session {
		t.Fatalf("the agent subscription started with resume %+v and view %+v", started.resume, started.view)
	}
	client.sendText(t, []byte(`{"type":"subscribe","sub":"h","view":{"kind":"agent","session_id":"`+session+`"},"resume":null}`))
	agent.next(t, 5*time.Second, "an agent view with a null resume")
	client.send(t, liveSubscribe("i", pipeline, deliveryResume))
	client.refusal(t, 10*time.Second, "i", "DELIVERY_NOT_CONFIGURED")
}

func TestASubscriptionKnowsWhetherItsCallerIsAPerson(t *testing.T) {
	events := newFakeLiveKind(0, 0)
	rig := newLiveSocketRig(t, testServerOptions{liveKinds: map[string]liveKind{liveEventsView: events.kind(true)}}, nil)
	for name, test := range map[string]struct {
		header http.Header
		human  bool
	}{
		"a person signed in": {asUser("alice"), true},
		"an agent's bearer":  {withBearer("agent-token"), false},
	} {
		client := rig.dial(t, test.header)
		client.send(t, liveSubscribe("e", eventsView(), ""))
		if started := events.next(t, 5*time.Second, name+"'s events subscription"); started.human != test.human {
			t.Errorf("%s: the subscription's stream says Human() = %t, want %t", name, started.human, test.human)
		}
	}
}

// A page that reads nothing holds back an events subscription, which waits for room in the stream
// queue rather than dropping frames or closing the socket; once the page reads again it is sent
// every frame, in order.
func TestASlowStreamWaitsAndNeverClosesTheSocket(t *testing.T) {
	const frames = 400
	events := newFakeLiveKind(frames, 512)
	rig := newLiveSocketRig(t, testServerOptions{liveKinds: map[string]liveKind{liveEventsView: events.kind(true)}},
		func(listener net.Listener) net.Listener { return smallSendBuffers{listener} })
	client := rig.open(t, smallReadDialer(), asUser("alice"))
	client.send(t, liveSubscribe("slow", eventsView(), ""))
	running := events.next(t, 5*time.Second, "the slow events subscription")
	socket := rig.sockets(t, 1)[0]
	pollUntil(t, 5*time.Second, "the stream queue fills while the page reads nothing", func() bool { return streamFull(socket) })

	time.Sleep(300 * time.Millisecond)
	if sent := running.sent.Load(); sent >= frames {
		t.Fatalf("the subscription queued all %d frames while the page read nothing: it never waited", sent)
	}
	if socket.overflowed() {
		t.Fatal("a full stream queue overflowed the socket")
	}

	client.read()
	for n := 1; n <= frames; n++ {
		if event := client.event(t, 10*time.Second, "slow"); event.ID != strconv.Itoa(n) {
			t.Fatalf("event %s arrived where %d was due", event.ID, n)
		}
	}
	if !client.open() {
		t.Fatalf("the socket ended: %v", client.err)
	}
	if sent := running.sent.Load(); sent != frames {
		t.Fatalf("the subscription sent %d frames, want %d", sent, frames)
	}
}

func TestSendWaitReturnsWhenItsSubscriptionStops(t *testing.T) {
	t.Run("the page unsubscribes", func(t *testing.T) {
		events := newFakeLiveKind(-1, 512)
		rig := newLiveSocketRig(t, testServerOptions{liveKinds: map[string]liveKind{liveEventsView: events.kind(true)}},
			func(listener net.Listener) net.Listener { return smallSendBuffers{listener} })
		client := rig.open(t, smallReadDialer(), asUser("alice"))
		client.send(t, liveSubscribe("x", eventsView(), ""))
		running := events.next(t, 5*time.Second, "x's events subscription")
		socket := rig.sockets(t, 1)[0]
		waitStalled(t, socket, running)

		// The queue never drains, and the page's unsubscribe still ends the wait.
		client.send(t, LiveUnsubscribe{Type: "unsubscribe", Sub: "x"})
		if err := running.ended(t, 3*time.Second); !errors.Is(err, context.Canceled) {
			t.Fatalf("SendWait answered %v once x was unsubscribed, want its context's end", err)
		}
	})

	t.Run("its context ends", func(t *testing.T) {
		stream := &liveSubscriptionStream{socket: &liveSocket{stream: make(chan liveStreamFrame, 1), closed: make(chan struct{})}}
		requireSendWaitWaits(t, stream, context.Background())
		ctx, cancel := context.WithCancel(context.Background())
		returned := sendWaitInBackground(stream, ctx)
		requireNoReturn(t, returned)
		cancel()
		if err := awaitReturn(t, returned); !errors.Is(err, context.Canceled) {
			t.Fatalf("SendWait answered %v once its context ended, want context.Canceled", err)
		}
	})

	t.Run("the socket ends", func(t *testing.T) {
		// The socket's end ends the subscription's context too, which derives from the socket's, and
		// SendWait answers the socket's end whichever of the two it sees first.
		socket := newLiveSocket(context.Background(), nil, false)
		socket.stream = make(chan liveStreamFrame, 1)
		stream := &liveSubscriptionStream{socket: socket}
		ctx, cancel := context.WithCancel(socket.ctx)
		defer cancel()
		requireSendWaitWaits(t, stream, ctx)
		returned := sendWaitInBackground(stream, ctx)
		requireNoReturn(t, returned)
		socket.ended()
		if err := awaitReturn(t, returned); !errors.Is(err, errLiveSocketClosed) {
			t.Fatalf("SendWait answered %v once the socket ended, want errLiveSocketClosed", err)
		}
		if ctx.Err() == nil {
			t.Fatal("the socket ended and the subscription's context did not")
		}
		if err := stream.SendWait(ctx, "after the end"); !errors.Is(err, errLiveSocketClosed) {
			t.Fatalf("SendWait after the socket ended answered %v, want errLiveSocketClosed", err)
		}
	})
}

// requireSendWaitWaits fills stream's one slot, which SendWait takes at once.
func requireSendWaitWaits(t *testing.T, stream *liveSubscriptionStream, ctx context.Context) {
	t.Helper()
	if err := stream.SendWait(ctx, "fills the queue"); err != nil {
		t.Fatalf("SendWait with room answered %v", err)
	}
}

func sendWaitInBackground(stream *liveSubscriptionStream, ctx context.Context) <-chan error {
	returned := make(chan error, 1)
	go func() { returned <- stream.SendWait(ctx, "waits for room") }()
	return returned
}

func requireNoReturn(t *testing.T, returned <-chan error) {
	t.Helper()
	select {
	case err := <-returned:
		t.Fatalf("SendWait returned %v with no room in the queue", err)
	case <-time.After(100 * time.Millisecond):
	}
}

func awaitReturn(t *testing.T, returned <-chan error) error {
	t.Helper()
	select {
	case err := <-returned:
		return err
	case <-time.After(time.Second):
		t.Fatal("SendWait never returned")
		return nil
	}
}

// contextBoundSubscription relies on the socket's context alone, as a test of the socket's end: its
// goroutine ends only when the context start gave it ends, and its stop waits for that goroutine
// and cancels nothing of its own. No real view's stop is written so; each cancels its own context.
type contextBoundSubscription struct{ done chan struct{} }

func (s *contextBoundSubscription) stop() { <-s.done }

// Every way a socket ends, the page's close and the server's 4001 among them, ends the context its
// subscriptions run on before the socket waits for them, so a subscription that ends on that
// context alone ends, and the socket's handler returns.
func TestASubscriptionsContextEndsWhenItsSocketEnds(t *testing.T) {
	for name, end := range map[string]func(t *testing.T, rig *liveSocketRig, client *socketClient){
		"the page closes it": func(t *testing.T, _ *liveSocketRig, client *socketClient) {
			closing := gws.FormatCloseMessage(gws.CloseGoingAway, "")
			if err := client.conn.WriteControl(gws.CloseMessage, closing, time.Now().Add(time.Second)); err != nil {
				t.Fatalf("the page's close: %v", err)
			}
		},
		"the rotate hook closes it": func(t *testing.T, rig *liveSocketRig, _ *socketClient) {
			if response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/live/_test/rotate", nil, "alice"); response.Code != http.StatusOK {
				t.Fatalf("POST /api/v1/live/_test/rotate: status=%d body=%s", response.Code, response.Body.String())
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			started := make(chan *contextBoundSubscription, 1)
			// release ends the goroutine only once the test is over, so a socket that never ends its
			// subscriptions' context fails the test rather than hanging the cleanups, which wait for
			// every socket's handler.
			release := make(chan struct{})
			bound := liveKind{resumes: true, start: func(ctx context.Context, _ liveStreamer, _ string, _ LiveView, _ *EventsLiveResume) (liveSubscription, *LiveError) {
				running := &contextBoundSubscription{done: make(chan struct{})}
				go func() {
					defer close(running.done)
					select {
					case <-ctx.Done():
					case <-release:
					}
				}()
				started <- running
				return running, nil
			}}
			rig := newLiveSocketRig(t, testServerOptions{testHooks: true, liveKinds: map[string]liveKind{liveEventsView: bound}}, nil)
			t.Cleanup(func() { close(release) })
			client := rig.dial(t, asUser("alice"))
			client.send(t, liveSubscribe("e", eventsView(), ""))
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("no subscription started within 5s")
			}

			end(t, rig, client)
			returned := make(chan struct{})
			go func() {
				rig.served.liveSockets.wait()
				close(returned)
			}()
			select {
			case <-returned:
			case <-time.After(5 * time.Second):
				t.Fatal("the socket's handler had not returned 5s after the socket ended: its subscription's context had not ended, so its stop never returned")
			}
		})
	}
}
