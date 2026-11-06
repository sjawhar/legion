package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// The period the seeded rows below fall in, and the two moments outside it: an access review
// reads a closed period, so a row a second before it and a row an hour after it are both out.
var (
	grantLogSince  = time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	grantLogUntil  = grantLogSince.Add(time.Hour)
	grantLogBefore = grantLogSince.Add(-time.Second)
	grantLogAfter  = grantLogUntil.Add(time.Hour)
)

// grantLogSeed is what seedGrantLog wrote, so each assertion names the row it is about.
type grantLogSeed struct {
	enrollment, otherEnrollment string
	request, grant, credential  string
}

// seedGrantLog writes the rows a quarterly access review reads, directly rather than through the
// request machine: the point of these tests is what ReadGrantLog joins, so the fixture spells out
// every column it reads. The audit rows are written oldest first, which is also id order, and two
// of them fall outside the period.
func seedGrantLog(t *testing.T, st *store.Store) grantLogSeed {
	t.Helper()
	ctx := context.Background()
	seed := grantLogSeed{
		enrollment:      uuid.NewString(),
		otherEnrollment: uuid.NewString(),
		request:         uuid.NewString(),
		grant:           uuid.NewString(),
		credential:      uuid.NewString(),
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := st.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
	}
	exec(`insert into launcher_credentials (id, operator, service, host, key_thumbprint, public_jwk, expires_at)
		values ($1,'ada@example.com','example-service','devbox',$2,'{}'::jsonb, now() + interval '30 days')`,
		seed.credential, uuid.NewString())
	exec(`insert into enrollments (id, kind, runtime_id, operator, session_id, thumbprint, launcher_credential_id, lease_expires_at)
		values ($1,'pod','pod-uid-1','ada@example.com','sess-1','tp-1',$2, now() + interval '1 hour')`,
		seed.enrollment, seed.credential)
	exec(`insert into enrollments (id, kind, runtime_id, thumbprint, launcher_credential_id, lease_expires_at)
		values ($1,'session','box-2','tp-2',$2, now() + interval '1 hour')`,
		seed.otherEnrollment, seed.credential)
	exec(`insert into requests (id, enrollment_id, reason, state, allowed_approver, rules_version, lifetime_seconds, decided_by, decision_detail)
		values ($1,$2,'ship the release','granted','ada@example.com','v1',3600,'ada@example.com','approved on the page')`,
		seed.request, seed.enrollment)
	exec(`insert into request_secrets (request_id, name, decision, source) values
		($1,'ZEBRA_KEY','approval','aws:zebra'), ($1,'ALPHA_KEY','automatic','aws:alpha')`, seed.request)
	exec(`insert into grants (id, request_id, enrollment_id, approver, expires_at)
		values ($1,$2,$3,'ada@example.com', now() + interval '1 hour')`, seed.grant, seed.request, seed.enrollment)

	audit := func(at time.Time, kind string, enrollment, request, grant any, actor, detail string) {
		t.Helper()
		exec(`insert into audit (at, kind, enrollment_id, request_id, grant_id, actor, detail) values ($1,$2,$3,$4,$5,$6,$7::jsonb)`,
			at, kind, enrollment, request, grant, actor, detail)
	}
	// One request.created row per request, as insertRequest writes it: the row before the period
	// is an enrollment's, so the period's own rows still read their service off the one creation
	// row their request has.
	audit(grantLogBefore, "enrollment.created", seed.enrollment, nil, nil, "launcher:"+seed.credential, `{"kind":"pod"}`)
	audit(grantLogSince, "request.created", seed.enrollment, seed.request, nil, "session:"+seed.enrollment,
		`{"state":"granted","secrets":["ZEBRA_KEY","ALPHA_KEY"],"person":"ada@example.com","service":"example-service"}`)
	audit(grantLogSince.Add(time.Minute), "request.granted", seed.enrollment, seed.request, seed.grant, "human:ada@example.com", `{"detail":""}`)
	audit(grantLogSince.Add(2*time.Minute), "grant.used", seed.enrollment, seed.request, seed.grant, "session:"+seed.enrollment, `{"secrets":["ALPHA_KEY"]}`)
	audit(grantLogSince.Add(3*time.Minute), "grant.moved", seed.otherEnrollment, nil, seed.grant, "broker",
		`{"from":"`+seed.enrollment+`","to":"`+seed.otherEnrollment+`"}`)
	audit(grantLogSince.Add(4*time.Minute), "launcher_credential.token_login", nil, nil, nil, "launcher:"+seed.credential, `{"host":"devbox"}`)
	audit(grantLogAfter, "grant.revoked", seed.enrollment, seed.request, seed.grant, "broker", `{}`)
	return seed
}

// grantLogPage is the limit the reads below pass when they are not pinning paging itself: a page
// far larger than the handful of rows seedGrantLog writes, so one read answers the whole seeded
// period. ReadGrantLog requires a positive limit (store.ErrGrantLogLimit), so no read here can
// leave it out.
const grantLogPage = 100

// readGrantLog is ReadGrantLog over the seeded period under grantLogPage, failing t on an error.
// A query of its own period or its own limit keeps them.
func readGrantLog(t *testing.T, st *store.Store, q store.GrantLogQuery) []store.GrantLogEvent {
	t.Helper()
	if q.Since.IsZero() {
		q.Since, q.Until = grantLogSince, grantLogUntil
	}
	if q.Limit == 0 {
		q.Limit = grantLogPage
	}
	events, err := store.ReadGrantLog(context.Background(), st.Pool, q)
	if err != nil {
		t.Fatalf("ReadGrantLog: %v", err)
	}
	return events
}

// sameJSON reports whether two JSON documents say the same thing, whatever order Postgres's jsonb
// holds their keys in.
func sameJSON(t *testing.T, got json.RawMessage, want string) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("decode %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatalf("decode %s: %v", want, err)
	}
	return reflect.DeepEqual(a, b)
}

// TestGrantLogReadsEveryRowOfThePeriodWithItsOwnIdentity pins what the access review reads: every
// audit row of the period in id order, each naming the session it was written under, that
// session's person, the service the broker verified for the request, the secrets and their
// decisions, and the decision itself. A row that names no enrollment leaves the identity fields
// empty and still carries its own detail, and a grant.moved row names a grant and no request.
func TestGrantLogReadsEveryRowOfThePeriodWithItsOwnIdentity(t *testing.T) {
	st := storetest.Open(t)
	seed := seedGrantLog(t, st)

	events := readGrantLog(t, st, store.GrantLogQuery{})
	kinds := make([]string, len(events))
	for i, ev := range events {
		kinds[i] = ev.Kind
	}
	want := []string{"request.created", "request.granted", "grant.used", "grant.moved", "launcher_credential.token_login"}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("the period reads as %v, want %v: rows outside [Since, Until) must not be answered, and ids must come in order", kinds, want)
	}
	for i := 1; i < len(events); i++ {
		if events[i].ID <= events[i-1].ID {
			t.Fatalf("row %d has id %d, not past the previous row's %d", i, events[i].ID, events[i-1].ID)
		}
		if events[i].At.Before(grantLogSince) || !events[i].At.Before(grantLogUntil) {
			t.Fatalf("row %d is at %s, outside [%s, %s)", i, events[i].At, grantLogSince, grantLogUntil)
		}
	}

	created := events[0]
	if created.EnrollmentID != seed.enrollment || created.RequestID != seed.request || created.GrantID != "" {
		t.Fatalf("request.created names enrollment %q, request %q, grant %q; want %q, %q and none",
			created.EnrollmentID, created.RequestID, created.GrantID, seed.enrollment, seed.request)
	}
	if created.Person != "ada@example.com" {
		t.Fatalf("request.created names person %q, want the enrollment's operator", created.Person)
	}
	if created.Service != "example-service" {
		t.Fatalf("request.created names service %q, want the service its own request.created detail records as verified", created.Service)
	}
	if created.ClaimedService != "example-service" {
		t.Fatalf("request.created names claimed_service %q, want the launcher credential's own claim", created.ClaimedService)
	}
	if (created.Session != store.GrantLogSession{Kind: "pod", SessionID: "sess-1", RuntimeID: "pod-uid-1"}) {
		t.Fatalf("request.created names session %+v, want the enrollment's kind, session id and runtime id", created.Session)
	}
	wantSecrets := []store.GrantLogSecret{{Name: "ALPHA_KEY", Decision: "automatic"}, {Name: "ZEBRA_KEY", Decision: "approval"}}
	if !reflect.DeepEqual(created.Secrets, wantSecrets) {
		t.Fatalf("request.created names secrets %+v, want %+v in name order", created.Secrets, wantSecrets)
	}
	if created.State != "granted" || created.Approver != "ada@example.com" || created.DecidedBy != "ada@example.com" {
		t.Fatalf("request.created reads state %q, approver %q, decided_by %q; want granted, ada@example.com and ada@example.com",
			created.State, created.Approver, created.DecidedBy)
	}
	if created.LifetimeSeconds != 3600 || created.Reason != "ship the release" || created.Detail != "approved on the page" {
		t.Fatalf("request.created reads lifetime %d, reason %q, detail %q; want 3600, the request's reason and its decision_detail",
			created.LifetimeSeconds, created.Reason, created.Detail)
	}
	if !sameJSON(t, created.Raw, `{"state":"granted","secrets":["ZEBRA_KEY","ALPHA_KEY"],"person":"ada@example.com","service":"example-service"}`) {
		t.Fatalf("request.created carries raw %s, want its own audit detail", created.Raw)
	}

	moved := events[3]
	if moved.GrantID != seed.grant || moved.RequestID != "" {
		t.Fatalf("grant.moved names grant %q and request %q, want %q and no request", moved.GrantID, moved.RequestID, seed.grant)
	}
	if moved.State != "" || len(moved.Secrets) != 0 || moved.Approver != "" {
		t.Fatalf("grant.moved reads state %q, secrets %+v, approver %q; a row that names no request has none of them",
			moved.State, moved.Secrets, moved.Approver)
	}
	if (moved.Session != store.GrantLogSession{Kind: "session", RuntimeID: "box-2"}) || moved.Person != "" {
		t.Fatalf("grant.moved names session %+v and person %q, want the successor enrollment's own identity and no operator", moved.Session, moved.Person)
	}

	login := events[4]
	if login.EnrollmentID != "" || login.Person != "" || login.Service != "" || login.ClaimedService != "" || (login.Session != store.GrantLogSession{}) {
		t.Fatalf("a launcher_credential row reads enrollment %q, person %q, service %q/%q and session %+v; a row with no enrollment has none of them",
			login.EnrollmentID, login.Person, login.Service, login.ClaimedService, login.Session)
	}
	if !sameJSON(t, login.Raw, `{"host":"devbox"}`) {
		t.Fatalf("a launcher_credential row carries raw %s, want its own detail, so a kind the route does not know is still readable", login.Raw)
	}
	if login.Actor != "launcher:"+seed.credential {
		t.Fatalf("a launcher_credential row names actor %q, want the credential that logged in", login.Actor)
	}
}

// TestGrantLogPagingAnswersEveryRowOnceAndNoneTwice pins the cursor the route pages a closed
// period with: each page resumes after the last row's id, every row of the period comes back
// exactly once, and a short page is the end.
func TestGrantLogPagingAnswersEveryRowOnceAndNoneTwice(t *testing.T) {
	st := storetest.Open(t)
	seedGrantLog(t, st)

	whole := readGrantLog(t, st, store.GrantLogQuery{})
	var paged []store.GrantLogEvent
	var after int64
	for pages := 0; ; pages++ {
		if pages > len(whole)+1 {
			t.Fatalf("paging did not end after %d pages for %d rows", pages, len(whole))
		}
		page := readGrantLog(t, st, store.GrantLogQuery{After: after, Limit: 2})
		paged = append(paged, page...)
		if len(page) < 2 {
			break
		}
		after = page[len(page)-1].ID
	}
	if len(paged) != len(whole) {
		t.Fatalf("paging read %d rows, want the period's %d", len(paged), len(whole))
	}
	seen := map[int64]int{}
	for i, ev := range paged {
		if ev.ID != whole[i].ID || ev.Kind != whole[i].Kind {
			t.Fatalf("paged row %d is %d/%s, want %d/%s", i, ev.ID, ev.Kind, whole[i].ID, whole[i].Kind)
		}
		seen[ev.ID]++
	}
	for id, times := range seen {
		if times != 1 {
			t.Fatalf("row %d came back %d times", id, times)
		}
	}
}

// TestGrantLogReadsNothingOutsideItsPeriod pins the half-open period itself: a row written at
// Until belongs to the next period, and one written before Since to the last.
func TestGrantLogReadsNothingOutsideItsPeriod(t *testing.T) {
	st := storetest.Open(t)
	seedGrantLog(t, st)

	for _, tc := range []struct {
		name         string
		since, until time.Time
		want         int
	}{
		{"the whole seeded period", grantLogSince, grantLogUntil, 5},
		{"the moment before it", grantLogBefore, grantLogSince, 1},
		{"its first minute", grantLogSince, grantLogSince.Add(time.Minute), 1},
		{"after everything", grantLogAfter.Add(time.Hour), grantLogAfter.Add(2 * time.Hour), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := readGrantLog(t, st, store.GrantLogQuery{Since: tc.since, Until: tc.until})
			if len(events) != tc.want {
				kinds := make([]string, len(events))
				for i, ev := range events {
					kinds[i] = ev.Kind
				}
				t.Fatalf("[%s, %s) reads %d rows %v, want %d", tc.since, tc.until, len(events), kinds, tc.want)
			}
		})
	}
}

// TestGrantLogRefusesAPageOfNoSize pins that the page's size is the caller's to state: a limit of
// zero or less is refused, and the read answers no rows at all rather than a whole period's.
func TestGrantLogRefusesAPageOfNoSize(t *testing.T) {
	st := storetest.Open(t)
	seedGrantLog(t, st)

	for _, limit := range []int{0, -1} {
		events, err := store.ReadGrantLog(context.Background(), st.Pool, store.GrantLogQuery{
			Since: grantLogSince, Until: grantLogUntil, Limit: limit,
		})
		if !errors.Is(err, store.ErrGrantLogLimit) || events != nil {
			t.Errorf("ReadGrantLog(limit %d) = %d events, %v; want no events and ErrGrantLogLimit", limit, len(events), err)
		}
	}
}
