package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// plannerSessionID is the session whose conversation the agent views below watch: an agent view
// names a session uuid.
const plannerSessionID = "01a0e090-4848-7473-acc5-fc96e6a646d3"

// reviewerSessionID is a second session, for a page watching two at once.
const reviewerSessionID = "6f1c5b1e-8a3c-4b7e-9a51-0d2f3c4b5a69"

// agentFeedWait bounds a wait for an agent view's next frame.
const agentFeedWait = 5 * time.Second

// agentFrame reads the next frame but a heartbeat, which must be sub's agent frame named event, and
// answers its data.
func (c *socketClient) agentFrame(t *testing.T, sub, event string) string {
	t.Helper()
	frame := c.next(t, agentFeedWait, "the "+event+" of "+sub)
	agent, ok := frame.(LiveAgentFrame)
	if !ok || agent.Sub != sub || agent.Event != event {
		t.Fatalf("frame = %+v, want the %s of %s", frame, event, sub)
	}
	return string(agent.Data)
}

// endedAs reads the next frame but a heartbeat, which must end sub, and answers its reason.
func (c *socketClient) endedAs(t *testing.T, sub string) string {
	t.Helper()
	frame := c.next(t, agentFeedWait, "the end of "+sub)
	ended, ok := frame.(LiveEnded)
	if !ok || ended.Sub != sub {
		t.Fatalf("frame = %+v, want the end of %s", frame, sub)
	}
	return ended.Reason
}

// subscribeAgent subscribes sub to session's conversation.
func (c *socketClient) subscribeAgent(t *testing.T, sub, session string) {
	t.Helper()
	c.send(t, liveSubscribe(sub, agentView(session), ""))
}

func TestAgentStreamRelaysTheSessionsOwnReplayThenItsLiveFrames(t *testing.T) {
	source := agentstream.NewMemory()
	source.SetReplay(plannerSessionID, agentstream.Frame(`{"v":1,"kind":"replay-body"}`))
	rig := newLiveSocketRig(t, testServerOptions{agentStream: source}, nil)
	client := rig.dial(t, asUser("alice"))
	client.subscribeAgent(t, "agent", plannerSessionID)

	// Every view opens by saying whether the session is answering at all: an empty transcript
	// means one thing for a session that answers and another for one nobody is listening for.
	if data := client.agentFrame(t, "agent", "responder"); data != `{"v":1,"responding":true}` {
		t.Fatalf("the first frame's data is %s, want the responder verdict", data)
	}
	if data := client.agentFrame(t, "agent", "replay"); data != `{"v":1,"kind":"replay-body"}` {
		t.Fatalf("the second frame's data is %s, want the session's replay", data)
	}
	// The session publishes only while a viewer is attached, so the relay must have told it so
	// before any turn ran — not on its first ten-second re-arm.
	if got := source.Watches(plannerSessionID); got < 1 {
		t.Fatalf("the relay armed the session %d times, want at least 1", got)
	}

	source.Publish(plannerSessionID, agentstream.Frame(`{"v":1,"kind":"message","seq":7}`))
	if data := client.agentFrame(t, "agent", "frame"); data != `{"v":1,"kind":"message","seq":7}` {
		t.Fatalf("the live frame's data is %s, want the published frame", data)
	}
}

// A session nobody answers for is a different statement from a session with an empty history,
// and it is the only thing that tells a plugin that cannot stream from one that has not spoken:
// every release that can stream answers the control subject, and none that cannot does. The
// relay says so on the wire rather than leaving a viewer waiting on a turn that cannot arrive.
func TestAgentStreamReportsASessionNobodyAnswersFor(t *testing.T) {
	source := agentstream.NewMemory()
	source.SetResponder(plannerSessionID, false)
	rig := newLiveSocketRig(t, testServerOptions{agentStream: source}, nil)
	client := rig.dial(t, asUser("alice"))
	client.subscribeAgent(t, "agent", plannerSessionID)

	if data := client.agentFrame(t, "agent", "responder"); data != `{"v":1,"responding":false}` {
		t.Fatalf("the first frame's data is %s, want the no-responder verdict", data)
	}
	// The view itself is healthy: a frame published to it still reaches the viewer, which is
	// what makes the verdict a statement about the session rather than about the relay.
	source.Publish(plannerSessionID, agentstream.Frame(`{"v":1,"kind":"message","seq":1}`))
	if data := client.agentFrame(t, "agent", "frame"); data != `{"v":1,"kind":"message","seq":1}` {
		t.Fatalf("the live frame's data is %s, want the published frame", data)
	}
}

// A session that was not on its control subject when its viewer arrived may be later - a session
// restarted onto a plugin that streams - so every re-arm asks for its replay again, and the first
// answer delivers the history without the page subscribing again. Once it has answered, a re-arm
// only tells the session the viewer is still there.
func TestAgentStreamRearmsTheSessionAndDeliversItsReplayOnItsFirstAnswer(t *testing.T) {
	source := agentstream.NewMemory()
	source.SetResponder(plannerSessionID, false)
	rig := newLiveSocketRig(t, testServerOptions{agentStream: source, agentWatchInterval: 50 * time.Millisecond}, nil)
	client := rig.dial(t, asUser("alice"))
	client.subscribeAgent(t, "agent", plannerSessionID)
	if data := client.agentFrame(t, "agent", "responder"); data != `{"v":1,"responding":false}` {
		t.Fatalf("the first frame's data is %s, want the no-responder verdict", data)
	}
	pollUntil(t, 5*time.Second, "the view re-arms the session", func() bool { return source.Watches(plannerSessionID) >= 3 })

	source.SetReplay(plannerSessionID, agentstream.Frame(`{"v":1,"kind":"late-replay"}`))
	if data := client.agentFrame(t, "agent", "responder"); data != `{"v":1,"responding":true}` {
		t.Fatalf("the frame after the session answered has data %s, want it responding", data)
	}
	if data := client.agentFrame(t, "agent", "replay"); data != `{"v":1,"kind":"late-replay"}` {
		t.Fatalf("the frame after the responder has data %s, want the session's replay", data)
	}
	armed := source.Watches(plannerSessionID)
	client.quiet(t, 300*time.Millisecond, "after the session answered: a re-arm asks for no second replay")
	if got := source.Watches(plannerSessionID); got <= armed {
		t.Fatalf("the view armed the session %d times by the replay and %d times 300ms later: it stopped re-arming", armed, got)
	}
}

func TestAgentStreamWritesNothingToPostgres(t *testing.T) {
	source := agentstream.NewMemory()
	source.SetReplay(plannerSessionID, agentstream.Frame(`{"v":1,"kind":"replay-body"}`))
	rig := newLiveSocketRig(t, testServerOptions{agentStream: source}, nil)

	// The relay's promise is that a session's conversation reaches a viewer and is never written
	// down, so the proof is that not one row changes anywhere while frames flow.
	before := databaseFingerprint(t, rig.database)

	client := rig.dial(t, asUser("alice"))
	client.subscribeAgent(t, "agent", plannerSessionID)
	client.agentFrame(t, "agent", "responder")
	client.agentFrame(t, "agent", "replay")
	source.Publish(plannerSessionID, agentstream.Frame(`{"v":1,"kind":"tool-result","secret":"hunter2"}`))
	client.agentFrame(t, "agent", "frame")
	_ = client.conn.Close()
	rig.sockets(t, 0)

	after := databaseFingerprint(t, rig.database)
	for name, digest := range after {
		if digest != before[name] {
			t.Fatalf("relaying a session's conversation wrote to %s", name)
		}
	}
}

// A session's own conversation is for a person watching it: a bearer's socket is refused the view,
// and a caller with no credential never gets a socket at all.
func TestAgentStreamRefusesEveryCallerButAHuman(t *testing.T) {
	source := agentstream.NewMemory()
	rig := newLiveSocketRig(t, testServerOptions{agentStream: source}, nil)

	if status := rig.refused(t, nil); status != http.StatusUnauthorized {
		t.Fatalf("an anonymous viewer's upgrade answered %d, want 401", status)
	}
	bearer := rig.dial(t, withBearer("agent-token"))
	bearer.subscribeAgent(t, "agent", plannerSessionID)
	bearer.refusal(t, agentFeedWait, "agent", "HUMAN_ONLY")
	bearer.quiet(t, 300*time.Millisecond, "after the bearer's agent view was refused")
	// A refused caller never reached the session: it was never told a viewer is attached, so
	// the session it asked about stays silent.
	if got := source.Watches(plannerSessionID); got != 0 {
		t.Fatalf("a refused viewer armed the session %d times, want 0", got)
	}
	if got := source.Viewers(plannerSessionID); got != 0 {
		t.Fatalf("a refused viewer holds %d subscriptions to the session's frames, want 0", got)
	}
}

func TestAgentStreamReportsADeploymentWithNoRelay(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	client := rig.dial(t, asUser("alice"))
	client.subscribeAgent(t, "agent", plannerSessionID)
	client.refusal(t, agentFeedWait, "agent", "STREAM_UNAVAILABLE")
}

// A session id goes straight into a NATS subject, where `*` and `>` are wildcards: one view
// naming either would subscribe a single viewer to every armed session on the bus. The view is
// refused anything that is not a session uuid before it touches NATS at all.
func TestAgentStreamRefusesASessionIdThatIsNotAUUID(t *testing.T) {
	source := agentstream.NewMemory()
	rig := newLiveSocketRig(t, testServerOptions{agentStream: source}, nil)
	client := rig.dial(t, asUser("alice"))

	ids := []string{"*", ">", "planner-session", plannerSessionID + ".>", " "}
	for index, id := range ids {
		sub := fmt.Sprintf("agent-%d", index)
		client.subscribeAgent(t, sub, id)
		client.refusal(t, agentFeedWait, sub, "SESSION_ID_INPUT")
	}
	client.send(t, liveSubscribe("no-session", LiveView{DeliveryLiveView: DeliveryLiveView{Kind: liveAgentView}}, ""))
	client.refusal(t, agentFeedWait, "no-session", "SESSION_ID_INPUT")
	client.quiet(t, 300*time.Millisecond, "after every refused session id")
	for _, id := range ids {
		if got := source.Watches(id); got != 0 {
			t.Fatalf("a refused session id %q reached the bus %d times, want 0", id, got)
		}
		if got := source.Viewers(id); got != 0 {
			t.Fatalf("a refused session id %q holds %d subscriptions, want 0", id, got)
		}
	}
}

// An agent view names its kind and its session and nothing else.
func TestAnAgentViewNamesItsKindAndItsSessionAlone(t *testing.T) {
	source := agentstream.NewMemory()
	rig := newLiveSocketRig(t, testServerOptions{agentStream: source}, nil)
	client := rig.dial(t, asUser("alice"))
	preset, query, at, session := 7, "repo=acme/widgets", "2026-10-14T09:00:00Z", plannerSessionID
	for _, frame := range []LiveSubscribe{
		liveSubscribe("a", LiveView{DeliveryLiveView: DeliveryLiveView{Kind: liveAgentView, Window: &DeliveryLiveWindow{Preset: &preset}}, SessionID: &session}, ""),
		liveSubscribe("b", LiveView{DeliveryLiveView: DeliveryLiveView{Kind: liveAgentView, Query: &query}, SessionID: &session}, ""),
		liveSubscribe("c", LiveView{DeliveryLiveView: DeliveryLiveView{Kind: liveAgentView, At: &at}, SessionID: &session}, ""),
	} {
		client.send(t, frame)
		client.refusal(t, agentFeedWait, frame.Sub, "INVALID_FRAME")
	}
	if got := source.Watches(plannerSessionID); got != 0 {
		t.Fatalf("a refused agent view armed the session %d times, want 0", got)
	}
}

// A viewer that falls behind ends its subscription rather than losing a frame. The relay never
// parses a frame, so it cannot tell a superseded snapshot from a tool result or from the settled
// frame of an earlier message; dropping the wrong one would leave a tool call reading "running…"
// with nothing later to correct it. Ending the subscription costs the page a subscribe, and the
// subscribe rebuilds from the session's own replay. The subscription here waits for room as a
// socket's does while its page reads nothing, so it drains nothing of what the session publishes.
func TestAgentStreamEndsTheStreamWhenAViewerFallsBehind(t *testing.T) {
	source := agentstream.NewMemory()
	_, _, _, served := newRegisteredTestServer(t, testServerOptions{agentStream: source})
	stream := newGatedStream()
	running := served.startAgentSubscription(t.Context(), stream, "agent", plannerSessionID)
	t.Cleanup(running.stop)
	pollUntil(t, 5*time.Second, "the view arms the session", func() bool { return source.Watches(plannerSessionID) >= 1 })

	// More frames than the view holds for a page that reads nothing.
	for index := range agentStreamBuffer * 4 {
		source.Publish(plannerSessionID, agentstream.Frame(fmt.Sprintf(`{"seq":%d}`, index)))
	}
	close(stream.gate)
	if frame, ok := stream.next(t, "the responder").(LiveAgentFrame); !ok || frame.Event != "responder" {
		t.Fatalf("first frame = %+v, want the responder", frame)
	}
	// What was held before the view fell behind may still go, in order; after it, the end.
	for last := -1; ; {
		switch frame := stream.next(t, "the end of agent").(type) {
		case LiveAgentFrame:
			var seq int
			if _, err := fmt.Sscanf(string(frame.Data), `{"seq":%d}`, &seq); err != nil || frame.Event != "frame" || seq != last+1 {
				t.Fatalf("frame %+v after seq %d, want the next frame in order", frame, last)
			}
			last = seq
			continue
		case LiveEnded:
			if frame.Sub != "agent" || frame.Reason != "behind" {
				t.Fatalf("ended = %+v, want agent's, behind", frame)
			}
		default:
			t.Fatalf("frame = %+v, want agent's frames and then its end", frame)
		}
		break
	}
	pollUntil(t, 5*time.Second, "the view that fell behind leaves the session's frames", func() bool { return source.Viewers(plannerSessionID) == 0 })
}

// The relay ending a viewer, as a connection lost on the way to the page does, ends the
// subscription as dropped, and the socket stays open: the page subscribes again at once, and that
// subscription rebuilds from the session's replay.
func TestARelayDropEndsTheSubscriptionAsDropped(t *testing.T) {
	source := agentstream.NewMemory()
	source.SetReplay(plannerSessionID, agentstream.Frame(`{"v":1,"kind":"first-replay"}`))
	rig := newLiveSocketRig(t, testServerOptions{agentStream: source}, nil)
	client := rig.dial(t, asUser("alice"))
	client.subscribeAgent(t, "agent", plannerSessionID)
	client.agentFrame(t, "agent", "responder")
	client.agentFrame(t, "agent", "replay")

	source.Drop(plannerSessionID)
	if reason := client.endedAs(t, "agent"); reason != "dropped" {
		t.Fatalf("the dropped view ended %q, want dropped", reason)
	}
	pollUntil(t, 5*time.Second, "the dropped view leaves the session's frames", func() bool { return source.Viewers(plannerSessionID) == 0 })
	source.Publish(plannerSessionID, agentstream.Frame(`{"v":1,"kind":"message","seq":1}`))
	client.quiet(t, 300*time.Millisecond, "after the relay dropped the view")
	if !client.open() {
		t.Fatalf("the socket ended with the view: %v", client.err)
	}

	source.SetReplay(plannerSessionID, agentstream.Frame(`{"v":1,"kind":"second-replay"}`))
	client.subscribeAgent(t, "agent", plannerSessionID)
	client.agentFrame(t, "agent", "responder")
	if data := client.agentFrame(t, "agent", "replay"); data != `{"v":1,"kind":"second-replay"}` {
		t.Fatalf("the subscribe after the drop replays %s, want the session's replay as it is now", data)
	}
}

// A frame is the session's own bytes, which Dispatch never parses but has to carry as JSON: one
// that is not JSON is skipped, as a replay that is not is, rather than closing the page's socket
// (the writer closes a socket with 1011 for a frame it cannot encode). The subscription counts what
// it skipped in one WARN when it ends, the count alone and never the bytes.
func TestAFrameThatIsNotJSONIsSkipped(t *testing.T) {
	logs := &storetest.LockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	source := agentstream.NewMemory()
	source.SetReplay(plannerSessionID, agentstream.Frame(`replay-secret not json`))
	rig := newLiveSocketRig(t, testServerOptions{agentStream: source}, nil)
	client := rig.dial(t, asUser("alice"))
	client.subscribeAgent(t, "agent", plannerSessionID)
	if data := client.agentFrame(t, "agent", "responder"); data != `{"v":1,"responding":true}` {
		t.Fatalf("the first frame's data is %s, want the session responding", data)
	}

	source.Publish(plannerSessionID, agentstream.Frame(`frame-secret not json`))
	source.Publish(plannerSessionID, agentstream.Frame(`{"v":1,"kind":"message","seq":1}`))
	source.Publish(plannerSessionID, agentstream.Frame(`{"half-secret":`))
	source.Publish(plannerSessionID, agentstream.Frame(`{"v":1,"kind":"message","seq":2}`))
	for _, want := range []string{`{"v":1,"kind":"message","seq":1}`, `{"v":1,"kind":"message","seq":2}`} {
		if data := client.agentFrame(t, "agent", "frame"); data != want {
			t.Fatalf("the next frame's data is %s, want %s: a frame that is not JSON was not skipped", data, want)
		}
	}
	if !client.open() {
		t.Fatalf("the socket ended: %v", client.err)
	}

	client.send(t, LiveUnsubscribe{Type: "unsubscribe", Sub: "agent"})
	const message = "dispatch: skipped agent frames that are not JSON"
	pollUntil(t, 5*time.Second, "the ended view logs what it skipped", func() bool { return logs.Lines(message) != "" })
	lines := logs.Lines(message)
	if strings.Count(lines, message) != 1 || !strings.Contains(lines, "level=WARN") || !strings.Contains(lines, "skipped=3") {
		t.Fatalf("the view logged %q, want one WARN counting 3 skipped frames", lines)
	}
	if strings.Contains(logs.String(), "secret") {
		t.Fatalf("a skipped frame's bytes reached the log: %s", logs.String())
	}
}

// One page's socket carries every agent view it shows: each subscription relays its own session
// alone, and one that stops leaves the session's frames while the other goes on.
func TestTwoAgentViewsShareOneSocket(t *testing.T) {
	source := agentstream.NewMemory()
	source.SetReplay(plannerSessionID, agentstream.Frame(`{"v":1,"kind":"planner-replay"}`))
	source.SetResponder(reviewerSessionID, true)
	rig := newLiveSocketRig(t, testServerOptions{agentStream: source}, nil)
	client := rig.dial(t, asUser("alice"))
	client.subscribeAgent(t, "planner", plannerSessionID)
	client.subscribeAgent(t, "reviewer", reviewerSessionID)

	// The two subscriptions run beside each other, so their opening frames interleave as they
	// will; each sub's own come in order.
	want := map[string][]string{
		"planner":  {`responder {"v":1,"responding":true}`, `replay {"v":1,"kind":"planner-replay"}`},
		"reviewer": {`responder {"v":1,"responding":true}`},
	}
	for len(want["planner"])+len(want["reviewer"]) > 0 {
		frame, ok := client.next(t, agentFeedWait, "the views' opening frames").(LiveAgentFrame)
		if !ok || len(want[frame.Sub]) == 0 || want[frame.Sub][0] != frame.Event+" "+string(frame.Data) {
			t.Fatalf("frame = %+v, want the next of %v", frame, want)
		}
		want[frame.Sub] = want[frame.Sub][1:]
	}

	source.Publish(reviewerSessionID, agentstream.Frame(`{"v":1,"kind":"message","seq":1}`))
	if data := client.agentFrame(t, "reviewer", "frame"); data != `{"v":1,"kind":"message","seq":1}` {
		t.Fatalf("the reviewer's frame has data %s", data)
	}
	source.Publish(plannerSessionID, agentstream.Frame(`{"v":1,"kind":"message","seq":2}`))
	if data := client.agentFrame(t, "planner", "frame"); data != `{"v":1,"kind":"message","seq":2}` {
		t.Fatalf("the planner's frame has data %s", data)
	}

	client.send(t, LiveUnsubscribe{Type: "unsubscribe", Sub: "planner"})
	pollUntil(t, 5*time.Second, "the unsubscribed view leaves the planner's frames", func() bool { return source.Viewers(plannerSessionID) == 0 })
	if got := source.Viewers(reviewerSessionID); got != 1 {
		t.Fatalf("the reviewer's frames have %d viewers after the planner's view stopped, want 1", got)
	}
	source.Publish(plannerSessionID, agentstream.Frame(`{"v":1,"kind":"message","seq":3}`))
	source.Publish(reviewerSessionID, agentstream.Frame(`{"v":1,"kind":"message","seq":4}`))
	if data := client.agentFrame(t, "reviewer", "frame"); data != `{"v":1,"kind":"message","seq":4}` {
		t.Fatalf("the frame after the planner's view stopped has data %s, want the reviewer's", data)
	}
	client.quiet(t, 300*time.Millisecond, "after the planner's view stopped")
}

// heldReplaySource is the in-memory relay but for one session whose replay request waits until it
// is released or its request's context ends, as a session slow to answer holds a real request up to
// its timeout.
type heldReplaySource struct {
	*agentstream.Memory
	held    string
	asked   chan struct{}
	release chan struct{}
}

func (s *heldReplaySource) Replay(ctx context.Context, sessionID string) (agentstream.Frame, error) {
	if sessionID == s.held {
		s.asked <- struct{}{}
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.Memory.Replay(ctx, sessionID)
}

// A session's replay can take up to two seconds to answer, and it is asked on the subscription's
// own goroutine: the socket's read loop goes on taking the page's frames, so another view on the
// same socket opens at once, and an unsubscribe ends the waiting view at once, cancelling its
// request rather than waiting the request out.
func TestAnAgentViewAsksForItsReplayOffTheReadLoop(t *testing.T) {
	memory := agentstream.NewMemory()
	memory.SetResponder(plannerSessionID, true)
	memory.SetResponder(reviewerSessionID, true)
	source := &heldReplaySource{Memory: memory, held: plannerSessionID, asked: make(chan struct{}, 4), release: make(chan struct{})}
	t.Cleanup(func() { close(source.release) })
	rig := newLiveSocketRig(t, testServerOptions{agentStream: source}, nil)
	client := rig.dial(t, asUser("alice"))

	client.subscribeAgent(t, "planner", plannerSessionID)
	select {
	case <-source.asked:
	case <-time.After(agentFeedWait):
		t.Fatal("the planner's view never asked for its replay")
	}
	opened := time.Now()
	client.subscribeAgent(t, "reviewer", reviewerSessionID)
	if data := client.agentFrame(t, "reviewer", "responder"); data != `{"v":1,"responding":true}` {
		t.Fatalf("the reviewer's first frame has data %s", data)
	}
	if waited := time.Since(opened); waited >= agentStreamReplayTimeout/2 {
		t.Fatalf("the reviewer's view opened %s after its subscribe, behind the planner's replay", waited)
	}

	stopped := time.Now()
	client.send(t, LiveUnsubscribe{Type: "unsubscribe", Sub: "planner"})
	pollUntil(t, agentStreamReplayTimeout/2, "the unsubscribed view ends while its replay waits", func() bool { return memory.Viewers(plannerSessionID) == 0 })
	t.Logf("the waiting view ended %s after its unsubscribe", time.Since(stopped))
	client.quiet(t, 300*time.Millisecond, "after the waiting view was unsubscribed")
}

// failingSource is the in-memory relay whose subscribe or watch fails, as a NATS connection that is
// closed or reconnecting fails them.
type failingSource struct {
	*agentstream.Memory
	subscribe, watch error
}

func (s *failingSource) Subscribe(sessionID string, deliver func(agentstream.Frame)) (func(), error) {
	if s.subscribe != nil {
		return nil, s.subscribe
	}
	return s.Memory.Subscribe(sessionID, deliver)
}

func (s *failingSource) Watch(sessionID string) error {
	if s.watch != nil {
		return s.watch
	}
	return s.Memory.Watch(sessionID)
}

// A relay that cannot subscribe the view or tell the session it is attached ends the subscription
// as dropped, so its page subscribes again rather than watching a session that never hears of it;
// and it waits a second first, so a relay that keeps failing is asked again at that pace rather than
// as fast as the page can subscribe.
func TestAnAgentViewWhoseRelayFailsEndsAsDropped(t *testing.T) {
	for name, source := range map[string]*failingSource{
		"its subscribe": {Memory: agentstream.NewMemory(), subscribe: errors.New("nats: connection closed")},
		"its watch":     {Memory: agentstream.NewMemory(), watch: errors.New("nats: connection reconnecting")},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, _, served := newRegisteredTestServer(t, testServerOptions{agentStream: source})
			stream := newGatedStream()
			close(stream.gate)
			began := time.Now()
			running := served.startAgentSubscription(t.Context(), stream, "agent", plannerSessionID)
			t.Cleanup(running.stop)
			pollUntil(t, agentStreamRetry/2, "the failed view leaves the session's frames before its end", func() bool {
				return source.Viewers(plannerSessionID) == 0
			})
			ended, ok := stream.next(t, "agent's end").(LiveEnded)
			if !ok || ended.Sub != "agent" || ended.Reason != "dropped" {
				t.Fatalf("the first frame of a view whose relay failed = %+v, want its end, dropped", ended)
			}
			if waited := time.Since(began); waited < agentStreamRetry {
				t.Fatalf("the view ended %s after it started, before the relay's retry (%s)", waited, agentStreamRetry)
			}
			pollUntil(t, 5*time.Second, "the failed view leaves the session's frames", func() bool {
				return source.Viewers(plannerSessionID) == 0
			})
		})
	}
}

// GET /api/v1/agents/{session_id}/stream is gone; the test hook that feeds the in-memory relay
// stays, since the browser suite's agent views read from it.
func TestTheAgentStreamRouteIsGone(t *testing.T) {
	source := agentstream.NewMemory()
	rig := newLiveSocketRig(t, testServerOptions{testHooks: true, agentStream: source}, nil)
	if response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/agents/"+plannerSessionID+"/stream", nil, "alice"); response.Code != http.StatusNotFound {
		t.Errorf("GET the agent stream: status=%d, want 404", response.Code)
	}
	if response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/agents/"+plannerSessionID+"/stream/_test/publish?as=replay", map[string]any{"v": 1}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("the publish hook: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	if replay, err := source.Replay(t.Context(), plannerSessionID); err != nil || string(replay) != `{"v":1}` {
		t.Fatalf("the hook set the replay %s (%v), want the body it was sent", replay, err)
	}
}
