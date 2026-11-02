package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
)

const roleTopic = "notifications.role.reviewer"

// grantedNATS starts a NATS server whose one nkey user may publish only to allow, and returns the
// deployed Dispatch server's bus client connected as that user, a plain connection of the same
// user that tests subscribe through, as the listener's lanes would, and the server's grant.
func grantedNATS(t *testing.T, allow ...string) (*bus.Client, *natsgo.Conn, *testnats.NkeyGrant) {
	t.Helper()
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, allow...)
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := bus.ConnectOwningStream([]string{grant.URL})
	if err != nil {
		t.Fatalf("connect as the granted user: %v", err)
	}
	t.Cleanup(client.Close)

	subscriber, err := bus.Dial("test-subscriber", []string{grant.URL}, os.LookupEnv)
	if err != nil {
		t.Fatalf("connect the subscriber: %v", err)
	}
	t.Cleanup(subscriber.Close)
	return client, subscriber, grant
}

// subscribe delivers every message on subject to the returned channel, once the server has the
// subscription.
func subscribe(t *testing.T, conn *natsgo.Conn, subject string) <-chan *natsgo.Msg {
	t.Helper()
	received := make(chan *natsgo.Msg, 8)
	if _, err := conn.ChanSubscribe(subject, received); err != nil {
		t.Fatalf("subscribe to %s: %v", subject, err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the subscription to %s: %v", subject, err)
	}
	return received
}

func publishedDestinations(t *testing.T, database *store.Store, eventID int64) (destinations []string, attempts int) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(),
		`select published_destinations, attempt_count from events where id = $1`, eventID,
	).Scan(&destinations, &attempts); err != nil {
		t.Fatalf("read published destinations: %v", err)
	}
	return destinations, attempts
}

// holderSubject is the agent subject of roleHolder, every role's holder in a test that runs the
// outbox with heldRoles.
var holderSubject = contracts.AgentSubject(roleHolder)

// answering subscribes a role holder on subject that answers every forward with an empty receipt,
// as a session plugin does, and delivers each forward to the returned channel.
func answering(t *testing.T, conn *natsgo.Conn, subject string) <-chan *natsgo.Msg {
	t.Helper()
	received := make(chan *natsgo.Msg, 8)
	if _, err := conn.Subscribe(subject, func(msg *natsgo.Msg) {
		received <- msg
		_ = msg.Respond(nil)
	}); err != nil {
		t.Fatalf("subscribe to %s: %v", subject, err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the subscription to %s: %v", subject, err)
	}
	return received
}

// A role route whose forward to its holder NATS denies (a per-client grant without the holder's
// agent subject) is not published: the outbox does not record the destination, does not mark the
// event published, and backs it off for a retry, logging the route, the holder's subject and the
// violation. The grant covers the role's topic, and nothing crosses it.
func TestRunRetriesARoleForwardNATSDenies(t *testing.T) {
	var logged lockedBuffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))

	client, subscriber, _ := grantedNATS(t, "notifications.dispatch.>", "notifications.role.>", "_INBOX.>", "$JS.API.>")
	received := subscribe(t, subscriber, roleTopic)
	forwarded := answering(t, subscriber, holderSubject)
	database := storetest.Open(t)
	broker := events.NewBroker()
	route := "role:reviewer"
	seedIssue(t, database, "T-1", &route)
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7", IssueKey: new("T-1"), Body: "Route to the reviewer"},
	})
	stop := run(t, database, client, broker)
	defer stop()

	waitFor(t, 10*time.Second, "the outbox's first attempt at the event settled", func() bool {
		_, attempts := publishedDestinations(t, database, event.ID)
		return attempts > 0 || publishedAt(t, database, event.ID) != nil
	})
	destinations, attempts := publishedDestinations(t, database, event.ID)
	if at := publishedAt(t, database, event.ID); at != nil {
		t.Fatalf("event marked published at %s with destinations %v, though NATS denied its role forward", at, destinations)
	}
	if slices.Contains(destinations, roleTopic) {
		t.Fatalf("published_destinations = %v records the denied role route", destinations)
	}
	issueTopic := "notifications.dispatch.issue.T-1.message.created"
	if !slices.Contains(destinations, issueTopic) {
		t.Fatalf("published_destinations = %v, want the granted issue topic %s", destinations, issueTopic)
	}
	if attempts != 1 {
		t.Fatalf("attempt_count = %d, want the one backed-off attempt", attempts)
	}
	for subject, arrived := range map[string]<-chan *natsgo.Msg{roleTopic: received, holderSubject: forwarded} {
		select {
		case message := <-arrived:
			t.Fatalf("%s received %q through a denied forward", subject, message.Data)
		default:
		}
	}
	lines := 0
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.Contains(line, "dispatch outbox: publish event") {
			lines++
			if !strings.Contains(line, `publish route \"role:reviewer\"`) || !strings.Contains(line, holderSubject) ||
				!strings.Contains(line, "Permissions Violation for Publish") {
				t.Fatalf("retry line %q does not name the route, the holder's subject and the violation", line)
			}
		}
	}
	if lines != 1 {
		t.Fatalf("logged %d retry lines, want one:\n%s", lines, logged.String())
	}
}

// A denied role forward holds back none of the event's other destinations: the granted author route
// of the same event is delivered and recorded on the first attempt, and only the role route is left
// for the retry. The grant covers the role's topic, which reaches no holder.
func TestRunDeliversTheGrantedRoutesOfAnEventWhoseRoleForwardNATSDenies(t *testing.T) {
	client, subscriber, _ := grantedNATS(t, "notifications.dispatch.>", "notifications.role.>", "notifications.agent.writer", "_INBOX.>", "$JS.API.>")
	authorTopic := "notifications.agent.writer"
	authorReceived := subscribe(t, subscriber, authorTopic)
	forwarded := answering(t, subscriber, holderSubject)
	database := storetest.Open(t)
	broker := events.NewBroker()
	event := seedThreeDestinationEvent(t, database, broker)
	stop := run(t, database, client, broker)
	defer stop()

	waitFor(t, 10*time.Second, "the outbox's first attempt at the event settled", func() bool {
		_, attempts := publishedDestinations(t, database, event.ID)
		return attempts > 0 || publishedAt(t, database, event.ID) != nil
	})
	destinations, attempts := publishedDestinations(t, database, event.ID)
	if !slices.Contains(destinations, authorTopic) {
		t.Fatalf("published_destinations = %v after the first attempt, want the granted author route %s", destinations, authorTopic)
	}
	if slices.Contains(destinations, roleTopic) || publishedAt(t, database, event.ID) != nil || attempts != 1 {
		t.Fatalf("published_destinations = %v, attempts %d, published %v: want the denied role route left for a retry",
			destinations, attempts, publishedAt(t, database, event.ID))
	}
	select {
	case <-authorReceived:
	case <-time.After(5 * time.Second):
		t.Fatal("the author received nothing, though its route is granted")
	}
	select {
	case message := <-forwarded:
		t.Fatalf("the role's holder received %q through a denied forward", message.Data)
	default:
	}
}

// Once the grant covers the holder's subject, the event's retry delivers the role forward it was
// denied - and only that: the author route the first attempt delivered is not sent again, and the
// event is published with every destination recorded, the role route as its role topic.
func TestTheRetryAfterAGrantDeliversOnlyTheDeniedRoute(t *testing.T) {
	allow := []string{"notifications.dispatch.>", "notifications.agent.writer", "_INBOX.>", "$JS.API.>"}
	client, subscriber, grant := grantedNATS(t, allow...)
	authorTopic := "notifications.agent.writer"
	authorReceived := subscribe(t, subscriber, authorTopic)
	forwarded := answering(t, subscriber, holderSubject)
	database := storetest.Open(t)
	broker := events.NewBroker()
	event := seedThreeDestinationEvent(t, database, broker)
	stop := run(t, database, client, broker)
	defer stop()

	waitFor(t, 10*time.Second, "the first attempt backed off", func() bool {
		_, attempts := publishedDestinations(t, database, event.ID)
		return attempts > 0
	})
	grant.Grant(t, append(allow, holderSubject)...)
	waitFor(t, 20*time.Second, "the retry after the grant", func() bool {
		return publishedAt(t, database, event.ID) != nil
	})

	destinations, _ := publishedDestinations(t, database, event.ID)
	issueTopic := "notifications.dispatch.issue.T-1.comment.created"
	if !slices.Equal(destinations, []string{issueTopic, authorTopic, roleTopic}) {
		t.Fatalf("published_destinations = %v, want the issue topic, the author route, then the role route", destinations)
	}
	select {
	case <-forwarded:
	case <-time.After(5 * time.Second):
		t.Fatal("the role's holder received nothing after the grant")
	}
	// Everything the two attempts published was answered before the event was marked published,
	// so the server has queued it to the subscriber ahead of the PONG this flush waits for.
	if err := subscriber.Flush(); err != nil {
		t.Fatalf("flush the subscriber: %v", err)
	}
	if sent := len(authorReceived); sent != 1 {
		t.Fatalf("the author route was sent %d times across the two attempts, want once", sent)
	}
	if extra := len(forwarded); extra != 0 {
		t.Fatalf("the role forward was sent %d more times after the grant, want once", extra)
	}
}

// With the holder's subject granted, the role route is forwarded once, as a listener's role forward
// carries it - the role's topic, the forward's dedupe key - recorded as its role topic, and the
// event marked published on the holder's receipt. Nothing crosses the role's topic.
func TestRunForwardsAGrantedRoleRoute(t *testing.T) {
	client, subscriber, _ := grantedNATS(t, "notifications.dispatch.>", holderSubject, "_INBOX.>", "$JS.API.>")
	received := subscribe(t, subscriber, roleTopic)
	forwarded := answering(t, subscriber, holderSubject)
	database := storetest.Open(t)
	broker := events.NewBroker()
	route := "role:reviewer"
	seedIssue(t, database, "T-1", &route)
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7", IssueKey: new("T-1"), Body: "Route to the reviewer"},
	})
	stop := run(t, database, client, broker)
	defer stop()

	waitFor(t, 10*time.Second, "the event marked published", func() bool {
		return publishedAt(t, database, event.ID) != nil
	})
	destinations, attempts := publishedDestinations(t, database, event.ID)
	if !slices.Contains(destinations, roleTopic) || attempts != 0 {
		t.Fatalf("published_destinations = %v after %d retries, want the role topic on the first attempt", destinations, attempts)
	}
	select {
	case message := <-forwarded:
		var item contracts.Envelope
		if err := json.Unmarshal(message.Data, &item); err != nil {
			t.Fatalf("decode the forward: %v", err)
		}
		if item.Topic != roleTopic || item.SourceEventID == "" || !strings.HasPrefix(item.DedupeKey, contracts.RoleForwardDedupePrefix) {
			t.Fatalf("the role's holder received %+v, want the event's role forward on %s", item, roleTopic)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the role's holder received nothing through a granted forward")
	}
	select {
	case message := <-received:
		t.Fatalf("the role's topic carried %q, want nothing", message.Data)
	default:
	}
}

// A JetStream publish NATS denies is named a denial, counted and backed off, not reported as an
// outage. An issue topic is always a JetStream subject (bus's streamSubjects holds
// notifications.dispatch.>), and the server acknowledges a denied JetStream publish with nothing,
// so the denial is read from the violation the connection recorded under its own subject
// (bus.ErrPublishDenied) rather than left to the publish window to report as an unreachable NATS:
// the outbox records no destination, does not mark the event published, counts the attempt and
// the denied destination, and logs the subject and the violation.
func TestRunRetriesAnIssueTopicNATSDenies(t *testing.T) {
	var logged lockedBuffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))

	// The grant withholds the Dispatch notification subjects and keeps what the run needs:
	// $JS.API.> so the stream is still created and the publish is a JetStream publish the server
	// genuinely denies.
	client, subscriber, _ := grantedNATS(t, "notifications.role.>", "_INBOX.>", "$JS.API.>")
	issueTopic := "notifications.dispatch.issue.T-1.message.created"
	received := subscribe(t, subscriber, issueTopic)
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7", IssueKey: new("T-1"), Body: "Only the issue topic"},
	})
	registry := metrics.New()
	stop := runDeps(t, Deps{
		Store: database, Publisher: client, Broker: broker, Listen: database.Pool.Listener,
		PublishFailures: registry.NewCounter("envoy_dispatch_outbox_publish_failures_total",
			"Destinations of an outbox publish attempt that did not reach NATS"),
	}, 10*time.Second)
	defer stop()

	// A denial the outbox counts settles the wait through attempt_count; one it reads as an
	// outage instead releases the event with no attempt counted and nothing recorded, and only
	// the log says so, so the outage line settles the wait too and the assertions below name it.
	waitFor(t, 30*time.Second, "the outbox's first attempt at the event settled", func() bool {
		_, attempts := publishedDestinations(t, database, event.ID)
		return attempts > 0 || publishedAt(t, database, event.ID) != nil ||
			strings.Contains(logged.String(), bus.ErrUnavailable.Error())
	})
	destinations, attempts := publishedDestinations(t, database, event.ID)
	if at := publishedAt(t, database, event.ID); at != nil {
		t.Fatalf("event marked published at %s with destinations %v, though NATS denied its issue topic", at, destinations)
	}
	if slices.Contains(destinations, issueTopic) {
		t.Fatalf("published_destinations = %v records the denied issue topic", destinations)
	}
	if attempts != 1 {
		t.Fatalf("attempt_count = %d, want the one backed-off attempt", attempts)
	}
	select {
	case message := <-received:
		t.Fatalf("the issue subscriber received %q through a denied publish", message.Data)
	default:
	}
	// The deployment's alert on a denied publish (dispatch://INFRA-749) matches this line and
	// nothing else, so each part of it is asserted as the literal text that alert greps for: the
	// level, the message, the event the line names, the subject that was refused, and
	// bus.ErrPublishDenied's own words. A reword or a lowered level is a change INFRA-749 depends
	// on, and fails here rather than silently stopping the alert.
	lines := 0
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.Contains(line, bus.ErrUnavailable.Error()) {
			t.Fatalf("line %q reports the denied publish as an outage", line)
		}
		if !strings.Contains(line, `msg="dispatch outbox: publish event"`) {
			continue
		}
		lines++
		for _, want := range []string{
			"level=ERROR",
			fmt.Sprintf("event_id=%d", event.ID),
			issueTopic,
			"NATS denied the publish",
			"Permissions Violation for Publish",
		} {
			if !strings.Contains(line, want) {
				t.Fatalf("the denial line %q does not carry %q, which the deployment's alert matches on", line, want)
			}
		}
	}
	if lines != 1 {
		t.Fatalf("logged %d denial lines, want one:\n%s", lines, logged.String())
	}
	response := httptest.NewRecorder()
	registry.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if body := response.Body.String(); !strings.Contains(body, `envoy_dispatch_outbox_publish_failures_total{reason="denied"} 1`) {
		t.Fatalf("/metrics body = %q, want the denied publish counted once", body)
	}
}

// A notifying event has more than one JetStream destination - its owner topic and a session
// route, a mention's copy, a claimant's or a document author's, every one of them a
// notifications.agent.* subject the stream holds - and a grant that withholds those prefixes
// denies every one of them. Each is named a denial, counted and backed off, as the single
// destination above is: a reader of the issue topic is as silent either way, and the deployment's
// alert is on the line that names the denial.
//
// What makes this its own test is that nats.go keeps one last error per connection, so the
// violations of the destinations one publish window sent together overwrite one another there.
// While window.acknowledged read that last error, it could match only the subject the connection
// kept, and every other denied destination waited out the window's acknowledgement timeout, which
// settle reads as bus.ErrNotAcknowledged - a send NATS gave no answer for, not a refusal of the
// event: the counter recorded `failed` where the denial belongs, no line named the subject or the
// violation, and the batch probed its connection for an outage that was not there. The
// per-subject record (deniedPublish) answers each send its own denial, and this test holds that
// for the shape a notifying event has in production.
func TestRunCountsEveryDeniedJetStreamDestinationOfOneEvent(t *testing.T) {
	var logged lockedBuffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))

	// notifications.dispatch.> (the owner topic) and notifications.agent.> (the session route)
	// are both withheld and both are stream subjects, so the event's two publishes are both
	// JetStream and both denied. $JS.API.> stays, so the stream is still created.
	client, _, _ := grantedNATS(t, "notifications.role.>", "_INBOX.>", "$JS.API.>")
	database := storetest.Open(t)
	broker := events.NewBroker()
	route := "session:33333333-3333-4333-8333-333333333333"
	seedIssue(t, database, "T-1", &route)
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "issue.updated", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: map[string]any{"title": "A person renamed the issue"},
	})
	if !event.Notify {
		t.Fatalf("the appended event does not notify, so it has one destination and proves nothing: %#v", event)
	}
	registry := metrics.New()
	stop := runDeps(t, Deps{
		Store: database, Publisher: client, Broker: broker, Listen: database.Pool.Listener,
		PublishFailures: registry.NewCounter("envoy_dispatch_outbox_publish_failures_total",
			"Destinations of an outbox publish attempt that did not reach NATS"),
	}, 10*time.Second)
	defer stop()

	// An outage line settles the wait as an attempt does, so the assertions below name it rather
	// than this timing out.
	waitFor(t, 30*time.Second, "the outbox's first attempt at the event settled", func() bool {
		_, attempts := publishedDestinations(t, database, event.ID)
		return attempts > 0 || publishedAt(t, database, event.ID) != nil ||
			strings.Contains(logged.String(), bus.ErrUnavailable.Error())
	})
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.Contains(line, bus.ErrUnavailable.Error()) {
			t.Fatalf("line %q reports a denied publish as an outage of the bus", line)
		}
	}
	destinations, attempts := publishedDestinations(t, database, event.ID)
	if at := publishedAt(t, database, event.ID); at != nil {
		t.Fatalf("event marked published at %s with destinations %v, though NATS denied both", at, destinations)
	}
	if len(destinations) != 0 {
		t.Fatalf("published_destinations = %v records a denied destination", destinations)
	}
	if attempts != 1 {
		t.Fatalf("attempt_count = %d, want the one backed-off attempt", attempts)
	}
	response := httptest.NewRecorder()
	registry.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	if !strings.Contains(body, `envoy_dispatch_outbox_publish_failures_total{reason="denied"} 2`) {
		t.Fatalf("/metrics body = %q, want both denied destinations counted as denials", body)
	}
	if strings.Contains(body, `reason="failed"`) {
		t.Fatalf("/metrics body = %q counts a denied destination as a failure", body)
	}
}

// Two events a batch sends through one publish window under a grant that denies their subjects
// are each answered their own denial, counted and backed off, exactly as the one send above is:
// the server refuses each message and reports a violation of each, so each send must read the
// violation of its own message.
//
// What makes this its own test is that a record holding one violation per connection, or one per
// subject, answers one of the window's sends and leaves the rest to the acknowledgement timeout,
// which settle reads as bus.ErrNotAcknowledged: those events are backed off naming a timeout
// rather than the refusal, the counter records `failed` where the denial belongs, no line names
// the subject, and the batch probes a connection nothing is wrong with. The contract the bus owes
// is one violation per refused message.
//
// The two events are of two issues, which is what puts two messages in one window here: the
// sends of one owner to one destination key are one chain, in id order, so a second comment on
// the same issue waits for the first to reach its topic instead of riding the same window
// (planChains, sendChains). That a window answers each of two sends of one subject its own
// denial is held where that shape is reachable, on the bus itself:
// internal/bus's TestAWindowAnswersEachDeniedSendOfOneSubjectItsOwnDenial.
func TestRunCountsADeniedSubjectOncePerRefusedMessage(t *testing.T) {
	var logged lockedBuffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))

	client, _, _ := grantedNATS(t, "notifications.role.>", "_INBOX.>", "$JS.API.>")
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	// The second issue goes in beside the first, in the project seedIssue created with it.
	if _, err := database.Pool.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('T-2', 'TT', 2, 'Another issue', '{"kind":"user","id":"alice"}', 'V')
	`); err != nil {
		t.Fatalf("create issue T-2: %v", err)
	}
	// One comment on each issue, appended before the scan, so one batch sends both through one
	// window, each to its own issue's topic under the denied notifications.dispatch. family. The
	// actor is a session, so neither notifies and each has its owner topic alone: the only thing
	// this test varies from the single-send case above is that the window carries two messages.
	agent := model.Actor{Kind: "session", ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7"}
	first := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: agent,
		Payload: map[string]any{"id": "comment-1", "body": "The first of two comments."},
	})
	second := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-2"), Type: "comment.created", Actor: agent,
		Payload: map[string]any{"id": "comment-2", "body": "The second, a moment later."},
	})
	registry := metrics.New()
	stop := runDeps(t, Deps{
		Store: database, Publisher: client, Broker: broker, Listen: database.Pool.Listener,
		PublishFailures: registry.NewCounter("envoy_dispatch_outbox_publish_failures_total",
			"Destinations of an outbox publish attempt that did not reach NATS"),
	}, 10*time.Second)
	defer stop()

	// An outage line settles the wait as a counted attempt does, so the assertions below name it
	// rather than this timing out.
	waitFor(t, 30*time.Second, "the outbox's first attempt at both events settled", func() bool {
		_, firstAttempts := publishedDestinations(t, database, first.ID)
		_, secondAttempts := publishedDestinations(t, database, second.ID)
		return (firstAttempts > 0 && secondAttempts > 0) ||
			strings.Contains(logged.String(), bus.ErrUnavailable.Error())
	})
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.Contains(line, bus.ErrUnavailable.Error()) {
			t.Fatalf("line %q reports a denied publish as an outage of the bus", line)
		}
	}
	for _, event := range []model.Event{first, second} {
		destinations, attempts := publishedDestinations(t, database, event.ID)
		if at := publishedAt(t, database, event.ID); at != nil {
			t.Fatalf("event %d marked published at %s with destinations %v, though NATS denied its topic",
				event.ID, at, destinations)
		}
		if len(destinations) != 0 {
			t.Fatalf("event %d: published_destinations = %v records the denied topic", event.ID, destinations)
		}
		if attempts != 1 {
			t.Fatalf("event %d: attempt_count = %d, want the one backed-off attempt", event.ID, attempts)
		}
	}
	response := httptest.NewRecorder()
	registry.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	if !strings.Contains(body, `envoy_dispatch_outbox_publish_failures_total{reason="denied"} 2`) {
		t.Fatalf("/metrics body = %q, want each refused message counted as a denial", body)
	}
	if strings.Contains(body, `reason="failed"`) {
		t.Fatalf("/metrics body = %q counts a denied publish as a failure", body)
	}
}

// A destination NATS is unreachable for is counted, once: it is the outage the metric exists
// for. Its send stops the batch, so the event's other destinations are not attempted and count
// nothing, and the event is released for the next batch rather than backed off.
func TestAPublishFailureThatEndsTheAttemptIsCountedOnce(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7", IssueKey: new("T-1"), Body: "NATS is unreachable"},
	})
	registry := metrics.New()
	publisher := &countingPublisher{fail: fmt.Errorf("bus: %w", bus.ErrUnavailable)}
	deps := Deps{Store: database, Publisher: publisher,
		PublishFailures: registry.NewCounter("envoy_dispatch_outbox_publish_failures_total",
			"Destinations of an outbox publish attempt that did not reach NATS")}

	if _, err := scanBatch(context.Background(), deps, unfenced{}, holding(t, deps).instance); err != nil {
		t.Fatalf("scan batch: %v", err)
	}
	if topics := publisher.attempted(); len(topics) != 1 {
		t.Fatalf("published %#v, want the batch stopped at the unreachable destination", topics)
	}
	if row := readEventRow(t, database, event.ID); row.publishedAt != nil || row.attempts != 0 || row.nextAttempt != nil {
		t.Fatalf("event %d has published_at %v, attempt_count %d and next_attempt_at %v, want it released: unpublished, no attempt counted, no back-off", event.ID, row.publishedAt, row.attempts, row.nextAttempt)
	}
	response := httptest.NewRecorder()
	registry.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if body := response.Body.String(); !strings.Contains(body, `envoy_dispatch_outbox_publish_failures_total{reason="failed"} 1`) {
		t.Fatalf("/metrics body = %q, want the unreachable destination counted once", body)
	}
}
