package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Two envoy-dispatch processes run on hosts whose clocks need not agree, so every time a write
// records, or a check judges against, is the database's: a claim's claimed_at, an answer's `at`,
// and the "in the future" a snooze must be. Each is the writing transaction's now(), which every
// other column that transaction stamps (updated_at, an event's created_at) shares to the
// microsecond, and which no host clock ever equals.

// timedEvent is one event of an issue's log as GET /api/v1/issues/{key}/events serves it: its
// type, when its transaction stamped it, and its payload.
type timedEvent struct {
	Type      string          `json:"type"`
	CreatedAt time.Time       `json:"created_at"`
	Payload   json.RawMessage `json:"payload"`
}

// issueEventsOfType is the issue's events of eventType, oldest first.
func issueEventsOfType(t *testing.T, handler http.Handler, key, eventType string) []timedEvent {
	t.Helper()
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/events?limit=200", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("read %s's events: status=%d body=%s", key, listed.Code, listed.Body.String())
	}
	var matched []timedEvent
	for _, event := range decodeBody[[]timedEvent](t, listed) {
		if event.Type == eventType {
			matched = append(matched, event)
		}
	}
	return matched
}

// askAnsweredAt is the created_at of the one ask.answered event for askID whose answer was given
// at `at`, failing t when no such event is in the issue's log.
func askAnsweredAt(t *testing.T, handler http.Handler, key, askID string, at time.Time) time.Time {
	t.Helper()
	for _, event := range issueEventsOfType(t, handler, key, "ask.answered") {
		var payload struct {
			ID     string          `json:"id"`
			Answer model.AskAnswer `json:"answer"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode ask.answered payload %s: %v", event.Payload, err)
		}
		if payload.ID == askID && payload.Answer.At.Equal(at) {
			return event.CreatedAt
		}
	}
	t.Fatalf("no ask.answered event for ask %s carries the answer at %s", askID, at.Format(time.RFC3339Nano))
	return time.Time{}
}

// databaseClockAhead is a store on database whose connections read now() by ahead of the
// database's own clock: the clock of a database host that runs ahead of the Dispatch host's. A
// function public.now() shadows pg_catalog.now() for a connection whose search_path names public
// first, which only this store's connections do; every other connection, and every column
// default, still reads pg_catalog's.
func databaseClockAhead(t *testing.T, database *store.Store, ahead time.Duration) *store.Store {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, fmt.Sprintf(
		`create function public.now() returns timestamptz language sql stable as $$ select pg_catalog.now() + interval '%d seconds' $$`,
		int64(ahead/time.Second))); err != nil {
		t.Fatalf("create the shadowing now(): %v", err)
	}
	config := database.Pool.Config()
	config.ConnConfig.RuntimeParams["search_path"] = "public, pg_catalog"
	opened, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open a pool reading the clock ahead: %v", err)
	}
	pool := store.NewPool(opened)
	t.Cleanup(pool.Close)
	// Without this check a search_path the server ignored would leave the test judging against
	// the true clock and passing for the wrong reason.
	var skew int64
	if err := pool.QueryRow(ctx, `select extract(epoch from now() - pg_catalog.now())::bigint`).Scan(&skew); err != nil {
		t.Fatalf("read the shadowed clock: %v", err)
	}
	if skew != int64(ahead/time.Second) {
		t.Fatalf("the shadowed clock reads %d s ahead, want %v", skew, ahead)
	}
	return &store.Store{Pool: pool}
}

// A claim's time is the transaction's that writes it: the answer's claim.at is its updated_at,
// which the same update stamps, and the issue.claimed event's created_at.
func TestAClaimTakesItsTimeFromTheDatabase(t *testing.T) {
	handler := newTestHandler(t)
	key := claimIssueKey(t, handler, "todo")

	claimed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{}, "alice")
	if claimed.Code != http.StatusOK {
		t.Fatalf("claim: status=%d body=%s", claimed.Code, claimed.Body.String())
	}
	answer := decodeBody[struct {
		UpdatedAt time.Time `json:"updated_at"`
		Claim     *struct {
			At time.Time `json:"at"`
		} `json:"claim"`
	}](t, claimed)
	if answer.Claim == nil {
		t.Fatalf("claim answer holds no claim: %s", claimed.Body.String())
	}
	if !answer.Claim.At.Equal(answer.UpdatedAt) {
		t.Errorf("claim.at %s, updated_at %s: want the one transaction time both are stamped with",
			answer.Claim.At.Format(time.RFC3339Nano), answer.UpdatedAt.Format(time.RFC3339Nano))
	}
	events := issueEventsOfType(t, handler, key, "issue.claimed")
	if len(events) != 1 || !events[0].CreatedAt.Equal(answer.Claim.At) {
		t.Errorf("issue.claimed events %+v, want one created at the claim's %s", events, answer.Claim.At.Format(time.RFC3339Nano))
	}
}

// An answer's time is the transaction's that writes it, a first answer and a changed one alike:
// each answer's `at` is its ask.answered event's created_at.
func TestAnAnswerTakesItsTimeFromTheDatabase(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Answer on the database clock", "A spec")
	ask := createChangeableAsk(t, handler, issue.Key)

	first := answerAskForChange(t, handler, ask.ID, "alice", "Ship", nil)
	if first.Answer == nil {
		t.Fatal("first answer is missing")
	}
	if at := askAnsweredAt(t, handler, issue.Key, ask.ID, first.Answer.At); !at.Equal(first.Answer.At) {
		t.Errorf("first answer at %s, its ask.answered created at %s: want the one transaction time",
			first.Answer.At.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano))
	}
	firstAt := timestampValue(first.Answer.At)
	changed := answerAskForChange(t, handler, ask.ID, "alice", "Hold", &firstAt)
	if changed.Answer == nil || !changed.Answer.At.After(first.Answer.At) {
		t.Fatalf("changed answer %#v, want one after the first", changed.Answer)
	}
	if at := askAnsweredAt(t, handler, issue.Key, ask.ID, changed.Answer.At); !at.Equal(changed.Answer.At) {
		t.Errorf("changed answer at %s, its ask.answered created at %s: want the one transaction time",
			changed.Answer.At.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano))
	}
}

// The carry answers an open approval request in the transaction that writes the version
// (docs.answerApprovalAsk): the request's answer `at` is that transaction's time, its
// ask.answered event's created_at.
func TestACarriedApprovalAnswerTakesItsTimeFromTheDatabase(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Carry on the database clock", "Context\n")
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	asks := insertDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveAsk, 2, map[string]string{"wave": "How many regions take the first wave?"})
	request := requestApproval(t, handler, issue.PrimaryArtifactID)

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 3)
	requireCarriedReview(t, handler, issue.PrimaryArtifactID, 3, 1, &request.ID)
	answered := readBlockAsk(t, handler, request.ID)
	if answered.State != "answered" || answered.Answer == nil {
		t.Fatalf("request after the carry = state %q answer %#v, want answered", answered.State, answered.Answer)
	}
	if at := askAnsweredAt(t, handler, issue.Key, request.ID, answered.Answer.At); !at.Equal(answered.Answer.At) {
		t.Errorf("carried answer at %s, its ask.answered created at %s: want the one transaction time",
			answered.Answer.At.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano))
	}
}

// "In the future" is the database's future: with the database's clock a day ahead of this host's,
// a snooze an hour out by this host is already past and refused 400, one two days out is stored,
// and an ask that does not exist is still 404 rather than either.
func TestASnoozeIsInTheFutureByTheDatabasesClock(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "CORE", "Snooze on the database clock", "A spec")
	askID := createSnoozeAsk(t, handler, "/api/v1/issues/"+issue.Key+"/asks", "Issue question")

	ahead := deps
	ahead.Store = databaseClockAhead(t, database, 24*time.Hour)
	snoozes := http.NewServeMux()
	snoozes.HandleFunc("PUT /api/v1/me/asks/{id}/snooze", directServer(ahead).putAskSnooze)

	// Microseconds, as Postgres stores a timestamp, so the stored snooze reads back equal.
	hostNow := time.Now().UTC().Truncate(time.Microsecond)
	for _, probe := range []struct {
		name   string
		askID  string
		until  time.Time
		status int
		code   string
	}{
		{"an hour out by this host, past by the database", askID, hostNow.Add(time.Hour), http.StatusBadRequest, "SNOOZE_INPUT"},
		{"an ask that does not exist", "00000000-0000-4000-8000-000000000000", hostNow.Add(48 * time.Hour), http.StatusNotFound, "NOT_FOUND"},
		{"two days out", askID, hostNow.Add(48 * time.Hour), http.StatusOK, ""},
	} {
		response := dispatchRequest(t, snoozes, http.MethodPut, "/api/v1/me/asks/"+probe.askID+"/snooze",
			map[string]any{"snoozed_until": probe.until.Format(time.RFC3339Nano)}, "alice")
		if response.Code != probe.status {
			t.Errorf("snooze %s: status=%d body=%s, want %d", probe.name, response.Code, response.Body.String(), probe.status)
			continue
		}
		if probe.code != "" {
			if got := decodeBody[struct {
				Code string `json:"code"`
			}](t, response); got.Code != probe.code {
				t.Errorf("snooze %s: code=%q, want %q", probe.name, got.Code, probe.code)
			}
			continue
		}
		if stored := decodeBody[userAskSnooze](t, response); !parseTimestamp(t, stored.SnoozedUntil).Equal(probe.until) {
			t.Errorf("snooze %s: stored %s, want %s", probe.name, stored.SnoozedUntil, probe.until.Format(time.RFC3339Nano))
		}
	}
}
