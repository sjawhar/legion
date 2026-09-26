package githubapp_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
)

const deliveriesClientID = "Iv1.deliveries"

func newDeliveriesClient(t *testing.T) (*githubapp.Client, *githubapptest.Webhook) {
	t.Helper()
	key, pemText := githubapptest.Key(t)
	webhook := githubapptest.NewWebhook(t, &key.PublicKey, deliveriesClientID, nil)
	client, err := githubapp.New(&auth.AppConfig{ClientID: deliveriesClientID, ClientSecret: "secret", PEM: pemText}, webhook.URL())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client, webhook
}

// The listing returns every attempt whatever its status, follows GitHub's Link cursor across
// pages, and stops at the first attempt older than since; it returns them newest first.
func TestDeliveriesPagesNewestFirstAndStopsAtSince(t *testing.T) {
	client, webhook := newDeliveriesClient(t)
	base := time.Date(2026, 9, 26, 4, 0, 0, 0, time.UTC)
	for i := range 500 {
		code := http.StatusOK
		if i%2 == 1 {
			code = http.StatusServiceUnavailable
		}
		webhook.Record(githubapptest.Attempt{GUID: fmt.Sprintf("guid-%04d", i), Event: "check_run"}, "created", 42, code, base.Add(time.Duration(i)*time.Second))
	}
	since := base.Add(100 * time.Second)

	got, err := client.Deliveries(context.Background(), since)
	if err != nil {
		t.Fatalf("deliveries: %v", err)
	}

	var want []int64
	log := webhook.Log()
	for i := len(log) - 1; i >= 0; i-- {
		if !log[i].DeliveredAt.Before(since) {
			want = append(want, log[i].ID)
		}
	}
	ids := make([]int64, len(got))
	for i, delivery := range got {
		ids[i] = delivery.ID
		wantStatus := "OK"
		if delivery.StatusCode != http.StatusOK {
			wantStatus = "Invalid HTTP Response: 503"
		}
		if delivery.Status != wantStatus || delivery.Event != "check_run" || delivery.Action != "created" || delivery.RepositoryID != 42 {
			t.Fatalf("delivery %d decoded as %+v", i, delivery)
		}
	}
	if len(want) != 400 || !slices.Equal(ids, want) {
		t.Fatalf("listed %d attempts, want the %d at or after since, newest first", len(ids), len(want))
	}
	if webhook.Listings() < 2 {
		t.Fatalf("listing served %d page(s), want the Link cursor followed past the first", webhook.Listings())
	}
}

// A redelivery request answered 202 succeeds; any other status is an error naming it.
func TestRedeliverRequestsTheAttemptAndReportsARefusal(t *testing.T) {
	client, webhook := newDeliveriesClient(t)
	now := time.Now()
	accepted := webhook.Record(githubapptest.Attempt{GUID: "guid-accepted", Event: "pull_request_review"}, "submitted", 7, http.StatusServiceUnavailable, now)
	refused := webhook.Record(githubapptest.Attempt{GUID: "guid-refused", Event: "pull_request_review"}, "submitted", 7, http.StatusServiceUnavailable, now)
	webhook.Refuse(refused.ID, http.StatusUnprocessableEntity)

	if err := client.Redeliver(context.Background(), accepted.ID); err != nil {
		t.Fatalf("redeliver an accepted attempt: %v", err)
	}
	err := client.Redeliver(context.Background(), refused.ID)
	if err == nil || !strings.Contains(err.Error(), "422") {
		t.Fatalf("redeliver a refused attempt: got %v, want an error naming 422", err)
	}
	if got, want := webhook.Requests(), []int64{accepted.ID, refused.ID}; !slices.Equal(got, want) {
		t.Fatalf("redelivery requests %v, want %v", got, want)
	}
}

// A client with no App key cannot sign the JWT these endpoints require.
func TestDeliveriesWithoutAnAppKeyAreErrNoAppKey(t *testing.T) {
	var client *githubapp.Client
	if _, err := client.Deliveries(context.Background(), time.Now()); !errors.Is(err, githubapp.ErrNoAppKey) {
		t.Fatalf("list without a key: got %v, want ErrNoAppKey", err)
	}
	if err := client.Redeliver(context.Background(), 1); !errors.Is(err, githubapp.ErrNoAppKey) {
		t.Fatalf("redeliver without a key: got %v, want ErrNoAppKey", err)
	}
}
