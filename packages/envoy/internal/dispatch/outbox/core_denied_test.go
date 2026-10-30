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

// A role route whose core publish NATS denies (a per-client grant without notifications.role.>)
// is not published: core NATS reports the violation only asynchronously, so the outbox must not
// record the destination, must not mark the event published, and must back it off for a retry,
// logging the subject and the violation.
func TestRunRetriesARoleRouteNATSDenies(t *testing.T) {
	var logged lockedBuffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))

	client, subscriber, _ := grantedNATS(t, "notifications.dispatch.>", "_INBOX.>", "$JS.API.>")
	received := subscribe(t, subscriber, roleTopic)
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
		t.Fatalf("event marked published at %s with destinations %v, though NATS denied its role route", at, destinations)
	}
	if slices.Contains(destinations, roleTopic) {
		t.Fatalf("published_destinations = %v records the denied role topic", destinations)
	}
	issueTopic := "notifications.dispatch.issue.T-1.message.created"
	if !slices.Contains(destinations, issueTopic) {
		t.Fatalf("published_destinations = %v, want the granted issue topic %s", destinations, issueTopic)
	}
	if attempts != 1 {
		t.Fatalf("attempt_count = %d, want the one backed-off attempt", attempts)
	}
	select {
	case message := <-received:
		t.Fatalf("the role subscriber received %q through a denied publish", message.Data)
	default:
	}
	lines := 0
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.Contains(line, "dispatch outbox: publish event") {
			lines++
			if !strings.Contains(line, `publish route \"role:reviewer\"`) || !strings.Contains(line, roleTopic) ||
				!strings.Contains(line, "Permissions Violation for Publish") {
				t.Fatalf("retry line %q does not name the route, the subject and the violation", line)
			}
		}
	}
	if lines != 1 {
		t.Fatalf("logged %d retry lines, want one:\n%s", lines, logged.String())
	}
}

// A denied role route holds back none of the event's other destinations: the granted author route
// of the same event is delivered and recorded on the first attempt, and only the role route is left
// for the retry.
func TestRunDeliversTheGrantedRoutesOfAnEventWhoseRoleRouteNATSDenies(t *testing.T) {
	client, subscriber, _ := grantedNATS(t, "notifications.dispatch.>", "notifications.agent.>", "_INBOX.>", "$JS.API.>")
	authorTopic := "notifications.agent.writer"
	authorReceived := subscribe(t, subscriber, authorTopic)
	roleReceived := subscribe(t, subscriber, roleTopic)
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
	case message := <-roleReceived:
		t.Fatalf("the role subscriber received %q through a denied publish", message.Data)
	default:
	}
}

// Once the grant covers the role lane, the event's retry delivers the role route it was denied -
// and only that: the author route the first attempt delivered is not sent again, and the event is
// published with every destination recorded.
func TestTheRetryAfterAGrantDeliversOnlyTheDeniedRoute(t *testing.T) {
	allow := []string{"notifications.dispatch.>", "notifications.agent.>", "_INBOX.>", "$JS.API.>"}
	client, subscriber, grant := grantedNATS(t, allow...)
	authorTopic := "notifications.agent.writer"
	authorReceived := subscribe(t, subscriber, authorTopic)
	roleReceived := subscribe(t, subscriber, roleTopic)
	database := storetest.Open(t)
	broker := events.NewBroker()
	event := seedThreeDestinationEvent(t, database, broker)
	stop := run(t, database, client, broker)
	defer stop()

	waitFor(t, 10*time.Second, "the first attempt backed off", func() bool {
		_, attempts := publishedDestinations(t, database, event.ID)
		return attempts > 0
	})
	grant.Grant(t, append(allow, "notifications.role.>")...)
	waitFor(t, 20*time.Second, "the retry after the grant", func() bool {
		return publishedAt(t, database, event.ID) != nil
	})

	destinations, _ := publishedDestinations(t, database, event.ID)
	issueTopic := "notifications.dispatch.issue.T-1.comment.created"
	if !slices.Equal(destinations, []string{issueTopic, authorTopic, roleTopic}) {
		t.Fatalf("published_destinations = %v, want the issue topic, the author route, then the role route", destinations)
	}
	select {
	case <-roleReceived:
	case <-time.After(5 * time.Second):
		t.Fatal("the role subscriber received nothing after the grant")
	}
	// Everything the two attempts published was accepted before the event was marked published,
	// so the server has queued it to the subscriber ahead of the PONG this flush waits for.
	if err := subscriber.Flush(); err != nil {
		t.Fatalf("flush the subscriber: %v", err)
	}
	if sent := len(authorReceived); sent != 1 {
		t.Fatalf("the author route was sent %d times across the two attempts, want once", sent)
	}
	if extra := len(roleReceived); extra != 0 {
		t.Fatalf("the role route was sent %d more times after the grant, want once", extra)
	}
}

// With notifications.role.> granted, the same role route is published once, recorded, and the
// event marked published.
func TestRunPublishesAGrantedRoleRoute(t *testing.T) {
	client, subscriber, _ := grantedNATS(t, "notifications.dispatch.>", "notifications.role.>", "_INBOX.>", "$JS.API.>")
	received := subscribe(t, subscriber, roleTopic)
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
	case message := <-received:
		var item contracts.Envelope
		if err := json.Unmarshal(message.Data, &item); err != nil {
			t.Fatalf("decode the role envelope: %v", err)
		}
		if item.Topic != roleTopic || item.SourceEventID == "" {
			t.Fatalf("role subscriber received %+v, want the event's envelope on %s", item, roleTopic)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the role subscriber received nothing through a granted publish")
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
		Store: database, Publisher: client, Broker: broker,
		PublishFailures: registry.NewCounter("envoy_dispatch_outbox_publish_failures_total",
			"Destinations of an outbox publish attempt that did not reach NATS"),
	})
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
// kept, and every other denied destination answered the window's acknowledgement timeout, which
// settle reads as bus.ErrUnavailable and so as an outage of the bus rather than a refusal of the
// event: the event was released with no attempt counted, the counter recorded `failed` where the
// denial belongs, no line named the subject, and publishBatch abandoned the rest of that scan's
// backlog. The per-subject record (deniedPublish) answers each send its own denial, and this test
// holds that for the shape a notifying event has in production.
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
		Store: database, Publisher: client, Broker: broker,
		PublishFailures: registry.NewCounter("envoy_dispatch_outbox_publish_failures_total",
			"Destinations of an outbox publish attempt that did not reach NATS"),
	})
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

// Two events of one type on one issue - two comments a person writes a moment apart - share an
// owner topic, and the outbox's scan sends a whole batch through one publish window. Under a
// grant that denies that subject, the server refuses each message and reports a violation of
// each, so each send must be answered its own denial, counted and backed off, exactly as the one
// send above is.
//
// What makes this its own test is that a record of one violation per subject answers only one of
// them: the second send of the same subject finds the record taken or overwritten and falls
// through to its acknowledgement timeout, which settle reads as bus.ErrUnavailable and so as an
// outage of the bus. That event is released with no attempt counted, the counter records `failed`
// where the denial belongs, no line names the subject, and publishBatch abandons the rest of that
// scan's backlog - the same pathology as the test above, reached by the commonest traffic there
// is. The contract the bus owes is one violation per refused message, not one per subject.
func TestRunCountsADeniedSubjectOncePerRefusedMessage(t *testing.T) {
	var logged lockedBuffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))

	client, _, _ := grantedNATS(t, "notifications.role.>", "_INBOX.>", "$JS.API.>")
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	// Two events of one type on one issue, appended before the scan, so one batch sends both
	// through one window and both to notifications.dispatch.issue.T-1.comment.created. The actor
	// is a session, so neither notifies and each has its owner topic alone: the only thing this
	// test varies from the single-send case above is that the subject is published twice.
	agent := model.Actor{Kind: "session", ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7"}
	first := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: agent,
		Payload: map[string]any{"id": "comment-1", "body": "The first of two comments."},
	})
	second := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: agent,
		Payload: map[string]any{"id": "comment-2", "body": "The second, a moment later."},
	})
	registry := metrics.New()
	stop := runDeps(t, Deps{
		Store: database, Publisher: client, Broker: broker,
		PublishFailures: registry.NewCounter("envoy_dispatch_outbox_publish_failures_total",
			"Destinations of an outbox publish attempt that did not reach NATS"),
	})
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
