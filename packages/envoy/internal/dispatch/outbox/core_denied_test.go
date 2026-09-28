package outbox

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
)

const deniedRoleTopic = "notifications.role.reviewer"

// grantedNATS starts a NATS server whose one nkey user may publish only to allow, and returns the
// deployed Dispatch server's bus client connected as that user, and a plain connection of the same
// user that subscribes to the role topic, as the listener's role lane would.
func grantedNATS(t *testing.T, allow ...string) (*bus.Client, <-chan *natsgo.Msg) {
	t.Helper()
	user, err := nkeys.CreateUser()
	if err != nil {
		t.Fatalf("create nkey user: %v", err)
	}
	public, err := user.PublicKey()
	if err != nil {
		t.Fatalf("nkey public key: %v", err)
	}
	seed, err := user.Seed()
	if err != nil {
		t.Fatalf("nkey seed: %v", err)
	}
	uri := testnats.StartNkeyPublishAllowed(t, public, allow...)
	t.Setenv("NATS_NKEY_SEED", string(seed))
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect as the granted user: %v", err)
	}
	t.Cleanup(client.Close)

	subscriber, err := natsgo.Connect(uri, natsgo.Nkey(public, func(nonce []byte) ([]byte, error) {
		return user.Sign(nonce)
	}))
	if err != nil {
		t.Fatalf("connect the role subscriber: %v", err)
	}
	t.Cleanup(subscriber.Close)
	received := make(chan *natsgo.Msg, 8)
	if _, err := subscriber.ChanSubscribe(deniedRoleTopic, received); err != nil {
		t.Fatalf("subscribe to the role topic: %v", err)
	}
	if err := subscriber.Flush(); err != nil {
		t.Fatalf("flush the role subscription: %v", err)
	}
	return client, received
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

	client, received := grantedNATS(t, "notifications.dispatch.>", "_INBOX.>", "$JS.API.>")
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
	if slices.Contains(destinations, deniedRoleTopic) {
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
			if !strings.Contains(line, deniedRoleTopic) || !strings.Contains(line, "Permissions Violation for Publish") {
				t.Fatalf("retry line %q does not name the subject and the violation", line)
			}
		}
	}
	if lines != 1 {
		t.Fatalf("logged %d retry lines, want one:\n%s", lines, logged.String())
	}
}

// With notifications.role.> granted, the same role route is published once, recorded, and the
// event marked published, exactly as before the denial check.
func TestRunPublishesAGrantedRoleRoute(t *testing.T) {
	client, received := grantedNATS(t, "notifications.dispatch.>", "notifications.role.>", "_INBOX.>", "$JS.API.>")
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
	if !slices.Contains(destinations, deniedRoleTopic) || attempts != 0 {
		t.Fatalf("published_destinations = %v after %d retries, want the role topic on the first attempt", destinations, attempts)
	}
	select {
	case message := <-received:
		var item contracts.Envelope
		if err := json.Unmarshal(message.Data, &item); err != nil {
			t.Fatalf("decode the role envelope: %v", err)
		}
		if item.Topic != deniedRoleTopic || item.SourceEventID == "" {
			t.Fatalf("role subscriber received %+v, want the event's envelope on %s", item, deniedRoleTopic)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the role subscriber received nothing through a granted publish")
	}
}
