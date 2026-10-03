package outbox

import (
	"context"
	"encoding/json"
	"log/slog"
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
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

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
