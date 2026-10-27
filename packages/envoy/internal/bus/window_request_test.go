package bus_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// requestClient starts a NATS server of the test's own and connects a client that owns the
// notification stream, which captures every agent subject.
func requestClient(t *testing.T) (*testnats.Server, string, *bus.Client) {
	t.Helper()
	server, url := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	return server, url, client
}

// forwardEnvelope is a role forward as Dispatch sends one to a holder: its topic is the role's,
// whatever subject it is sent to.
func forwardEnvelope(name string) contracts.Envelope {
	return contracts.Envelope{
		EventID:       "evt-" + name,
		Source:        "dispatch",
		SourceEventID: "src-" + name,
		Topic:         "notifications.role.request-test",
		DedupeKey:     "envoy.role.forward.dispatch-" + name,
		IssuedAt:      contracts.NowMillis(),
	}
}

// responder subscribes subject on a connection of its own and answers each message with an empty
// receipt, as a self-subscribed session does.
func responder(t *testing.T, url, subject string) {
	t.Helper()
	conn, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("connect the responder: %v", err)
	}
	t.Cleanup(conn.Close)
	if _, err := conn.Subscribe(subject, func(message *natsgo.Msg) { _ = message.Respond(nil) }); err != nil {
		t.Fatalf("subscribe the responder: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the responder's subscription: %v", err)
	}
}

// answerOf waits up to within for outcome's answer.
func answerOf(t *testing.T, outcome <-chan error, within time.Duration) error {
	t.Helper()
	select {
	case err := <-outcome:
		return err
	case <-time.After(within):
		t.Fatalf("no answer within %s", within)
		return nil
	}
}

// A Request returns on the receiver's empty receipt. The stream that captures the agent subject
// acknowledges its copy too, and without AcceptStreamAck that acknowledgement is passed over.
func TestARequestReturnsOnTheReceiversEmptyReceipt(t *testing.T) {
	_, url, client := requestClient(t)
	subject := contracts.AgentSubject("ses_request_receipt")
	responder(t, url, subject)

	err := answerOf(t, client.Window().Request(subject, forwardEnvelope("receipt"), bus.RequestOptions{Timeout: 2 * time.Second}), 5*time.Second)
	if err != nil {
		t.Fatalf("a Request its receiver answered returned %v, want nil", err)
	}
}

// A Request nobody answers ends when its timeout runs out, as not acknowledged and as no receipt,
// never as a lost connection: the server's no-responders answer is not a receipt and does not end
// the wait.
func TestARequestNobodyAnswersIsNotAcknowledgedWithinItsTimeout(t *testing.T) {
	const timeout = 300 * time.Millisecond
	_, _, client := requestClient(t)

	started := time.Now()
	err := answerOf(t, client.Window().Request("notifications.role.request-nobody", forwardEnvelope("nobody"), bus.RequestOptions{Timeout: timeout}), 5*time.Second)
	elapsed := time.Since(started)
	if !errors.Is(err, bus.ErrNotAcknowledged) || !errors.Is(err, bus.ErrReceiptTimeout) || errors.Is(err, bus.ErrUnavailable) {
		t.Fatalf("an unanswered Request returned %v, want ErrNotAcknowledged and ErrReceiptTimeout, not ErrUnavailable", err)
	}
	if elapsed < timeout || elapsed > 2*time.Second {
		t.Fatalf("an unanswered Request returned after %s, want its %s timeout and not much more", elapsed, timeout)
	}
}

// The stream's acknowledgement of the copy it stored answers a Request only when AcceptStreamAck
// says so, as for a holder delivered from that stored copy; otherwise the Request waits on for a
// receipt that never comes.
func TestARequestCountsTheStreamsAcknowledgementOnlyWhenAskedTo(t *testing.T) {
	_, _, client := requestClient(t)
	subject := contracts.AgentSubject("ses_request_stored")
	window := client.Window()

	err := answerOf(t, window.Request(subject, forwardEnvelope("stored-ignored"), bus.RequestOptions{Timeout: 300 * time.Millisecond}), 5*time.Second)
	if !errors.Is(err, bus.ErrNotAcknowledged) || !errors.Is(err, bus.ErrReceiptTimeout) {
		t.Fatalf("a Request only the stream acknowledged returned %v without AcceptStreamAck, want ErrNotAcknowledged and ErrReceiptTimeout", err)
	}
	err = answerOf(t, window.Request(subject, forwardEnvelope("stored-accepted"), bus.RequestOptions{Timeout: 2 * time.Second, AcceptStreamAck: true}), 5*time.Second)
	if err != nil {
		t.Fatalf("a Request the stream acknowledged returned %v with AcceptStreamAck, want nil", err)
	}
}

// Requests of one envelope to one subject under its ForwardMsgID leave the stream one copy however
// often they are sent, the repeat's duplicate acknowledgement still answering it; the same envelope
// to another subject is a copy of its own there.
func TestRequestsUnderOneMsgIDLeaveTheStreamOneCopy(t *testing.T) {
	_, _, client := requestClient(t)
	window := client.Window()
	item := forwardEnvelope("once")
	holder := contracts.AgentSubject("ses_request_once")
	other := contracts.AgentSubject("ses_request_other")

	for attempt := 1; attempt <= 2; attempt++ {
		opts := bus.RequestOptions{MsgID: bus.ForwardMsgID(item.DedupeKey, holder), AcceptStreamAck: true, Timeout: 2 * time.Second}
		if err := answerOf(t, window.Request(holder, item, opts), 5*time.Second); err != nil {
			t.Fatalf("attempt %d returned %v, want nil", attempt, err)
		}
	}
	opts := bus.RequestOptions{MsgID: bus.ForwardMsgID(item.DedupeKey, other), AcceptStreamAck: true, Timeout: 2 * time.Second}
	if err := answerOf(t, window.Request(other, item, opts), 5*time.Second); err != nil {
		t.Fatalf("the Request to the second subject returned %v, want nil", err)
	}

	info, err := client.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: "notifications.agent.>"})
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if got := info.State.Subjects[holder]; got != 1 {
		t.Errorf("the stream holds %d copies on %s after two Requests, want 1", got, holder)
	}
	if got := info.State.Subjects[other]; got != 1 {
		t.Errorf("the stream holds %d copies on %s, want 1", got, other)
	}
}

// A Request whose connection is lost before an answer answers ErrUnavailable once the loss is
// seen, without waiting out its timeout, and a Request after it answers ErrUnavailable before
// sending anything.
func TestARequestWhoseConnectionIsLostAnswersUnavailable(t *testing.T) {
	server, _, client := requestClient(t)
	window := client.Window()

	pending := window.Request("notifications.role.request-lost", forwardEnvelope("lost"), bus.RequestOptions{Timeout: 30 * time.Second})
	server.Stop(t)
	if err := answerOf(t, pending, 10*time.Second); !errors.Is(err, bus.ErrUnavailable) || errors.Is(err, bus.ErrNotAcknowledged) {
		t.Fatalf("a Request whose connection was lost returned %v, want ErrUnavailable", err)
	}

	select {
	case err := <-window.Request("notifications.role.request-lost", forwardEnvelope("after"), bus.RequestOptions{}):
		if !errors.Is(err, bus.ErrUnavailable) {
			t.Fatalf("a Request on a lost window answered %v, want ErrUnavailable", err)
		}
	default:
		t.Fatal("a Request on a lost window returned with no answer, as if it had sent something")
	}
}

// A Request to a subject NATS does not accept is refused before anything is sent.
func TestARequestToASubjectNATSRefusesIsRefusedAtOnce(t *testing.T) {
	_, _, client := requestClient(t)
	select {
	case err := <-client.Window().Request("notifications.agent..bad", forwardEnvelope("bad"), bus.RequestOptions{}):
		if !errors.Is(err, bus.ErrRefused) {
			t.Fatalf("a Request to an invalid subject answered %v, want ErrRefused", err)
		}
	default:
		t.Fatal("a Request to an invalid subject returned with no answer")
	}
}

// Many Requests in flight on one window, sent from several goroutines while others publish on it,
// are each answered by their own receipt.
func TestManyRequestsInFlightOnOneWindowEachTakeTheirOwnAnswer(t *testing.T) {
	const senders = 20
	_, url, client := requestClient(t)
	subject := contracts.AgentSubject("ses_request_many")
	responder(t, url, subject)
	window := client.Window()

	var wg sync.WaitGroup
	failures := make(chan error, 2*senders)
	for index := range senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := fmt.Sprintf("many-%d", index)
			request := window.Request(subject, forwardEnvelope(name), bus.RequestOptions{Timeout: 5 * time.Second})
			publish := window.PublishAsync(numberedEnvelope(orderedStreamTopic, index))
			if err := <-request; err != nil {
				failures <- fmt.Errorf("request %s: %w", name, err)
			}
			if err := <-publish; err != nil {
				failures <- fmt.Errorf("publish %d: %w", index, err)
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
}

// Probe answers whether the window's connection still answers: nil while its server answers a
// flush, ErrUnavailable when the flush runs out of time on a connection that stays connected (a
// silent server), and ErrUnavailable once the connection is lost.
func TestProbeAnswersWhetherTheWindowsConnectionStillAnswers(t *testing.T) {
	server, _, client := requestClient(t)
	window := client.Window()

	if err := window.Probe(time.Second); err != nil {
		t.Fatalf("a Probe of a live connection returned %v, want nil", err)
	}

	thaw := server.Freeze(t)
	err := window.Probe(200 * time.Millisecond)
	thaw()
	if !errors.Is(err, bus.ErrUnavailable) {
		t.Fatalf("a Probe of a silent server returned %v, want ErrUnavailable", err)
	}
	if !client.Connected() {
		t.Fatal("the client lost its connection while frozen; the Probe proves nothing about a silent server")
	}
	if err := window.Probe(time.Second); err != nil {
		t.Fatalf("a Probe once the server answers again returned %v, want nil", err)
	}

	server.Stop(t)
	deadline := time.Now().Add(5 * time.Second)
	for client.Connected() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := window.Probe(200 * time.Millisecond); !errors.Is(err, bus.ErrUnavailable) {
		t.Fatalf("a Probe of a lost connection returned %v, want ErrUnavailable", err)
	}
}
