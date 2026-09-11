package outbox

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestRunPublishesPersistedAskOpenedEventID(t *testing.T) {
	database := openTestStore(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"),
		Type:     "ask.opened",
		Actor:    model.Actor{Kind: "session", ID: "session-1"},
		Payload: model.Ask{
			ID:       "5a660655-04ad-4ce0-8a9b-93dd03c412b7",
			IssueKey: new("T-1"),
			Question: "Which option?",
			State:    "open",
		},
	})
	publisher := &recordingPublisher{}
	stop := run(t, database, publisher, broker)
	defer stop()

	waitFor(t, time.Second, "ask opening publication", func() bool {
		return len(publisher.all()) == 1 && publishedAt(t, database, event.ID) != nil
	})
	var stored struct {
		OpenedEventID int64 `json:"opened_event_id"`
	}
	var raw []byte
	if err := database.Pool.QueryRow(context.Background(), `select payload from events where id = $1`, event.ID).Scan(&raw); err != nil {
		t.Fatalf("read stored ask.opened payload: %v", err)
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("decode stored ask.opened payload: %v", err)
	}
	if stored.OpenedEventID != event.ID {
		t.Fatalf("stored opened_event_id=%d, want %d", stored.OpenedEventID, event.ID)
	}

	var published struct {
		Payload struct {
			OpenedEventID int64 `json:"opened_event_id"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(publisher.all()[0].Payload), &published); err != nil {
		t.Fatalf("decode published envelope: %v", err)
	}
	if published.Payload.OpenedEventID != event.ID {
		t.Fatalf("published opened_event_id=%d, want %d", published.Payload.OpenedEventID, event.ID)
	}
}
