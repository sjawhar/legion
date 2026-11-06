// grantlog_test.go drives GET /v1/grant-log, the route a quarterly access review reads the
// broker's whole audit trail through, over real HTTP against a real Postgres: the review's own
// caller reads every row of a closed period, Dispatch's server and the Envoy listener read none,
// and the cost of reading such a period is measured rather than assumed.
package api_test

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

const (
	// reviewAccount is the ServiceAccount BROKER_API_ACCOUNTS names as the caller access-review,
	// which holds readsGrantLog and nothing else; listenerAccount is the Envoy listener's, which
	// holds readsTokensBack and nothing else. The two negative controls below call as the
	// listener and as dispatch, the callers that reach every other named-caller route.
	reviewAccount   = "system:serviceaccount:example-ns:example-access-review"
	listenerAccount = "system:serviceaccount:example-ns:example-listener"
	reviewCaller    = "access-review"
	listenerCaller  = "listener"
	// grantLogSessionName and grantLogPodName are the Envoy session names the flow's two
	// enrollments carry, so each event names a session rather than only an enrollment row.
	grantLogSessionName = "grant-log-session"
	grantLogPodName     = "grant-log-pod"
	// listedOwner is the person a listed secret's requesters tag names, which is nobody the
	// flow's sessions act for: that is what makes the refused request's decision
	// requester-not-listed rather than an approval request.
	listedOwner = "mallory@example.com"
)

// grantLogLines are the store.GrantLogMessage lines capture holds, in the order they were
// written, each as its attributes by key, so the test reads the attributes the broker logged
// rather than a rendering of them. Every other line the broker logs is skipped: this is the one
// line the access review's log pipeline keeps. The records are this test's own: logtest puts the
// previous default logger back when it ends.
func grantLogLines(t *testing.T, capture *logtest.Recorder) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, r := range capture.Records() {
		if r.Message != store.GrantLogMessage {
			continue
		}
		attrs := map[string]any{}
		r.Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value.Any()
			return true
		})
		lines = append(lines, attrs)
	}
	return lines
}

// wireGrantLog is GET /v1/grant-log's answer as the wire carries it. The events are decoded into
// the store's own type because that type is the route's contract: it answers each event as
// store.GrantLogEvent's JSON tags name it and adds no vocabulary of its own.
type wireGrantLog struct {
	Events    []store.GrantLogEvent `json:"events"`
	NextAfter int64                 `json:"next_after"`
}

// newGrantLogServer is the test server with the two callers beside Dispatch's that this file
// drives: the access review, listed for readsGrantLog alone, and the Envoy listener, listed for
// introspection alone. It answers the server and each of those two callers' own tokens.
func newGrantLogServer(t *testing.T) (ts *testServer, reviewToken, listenerToken string) {
	t.Helper()
	ts = newTestServerWith(t, func(d *api.Deps) {
		d.APIAccounts[reviewAccount] = brokertest.Caller(t, reviewCaller)
		d.APIAccounts[listenerAccount] = brokertest.Caller(t, listenerCaller)
	})
	return ts, ts.accountToken(t, reviewAccount, "", time.Hour), ts.accountToken(t, listenerAccount, "", time.Hour)
}

// grantLog reads one page of the grant log as the caller whose ServiceAccount token is bearer.
func (ts *testServer) grantLog(t *testing.T, bearer string, params url.Values) (int, []byte) {
	t.Helper()
	return ts.req(t, http.MethodGet, "/v1/grant-log?"+params.Encode(), map[string]string{"Authorization": "Bearer " + bearer}, nil)
}

// grantLogPeriod is the closed period the review reads, and the query that names it.
type grantLogPeriod struct{ since, until time.Time }

func (p grantLogPeriod) params() url.Values {
	return url.Values{"since": {p.since.Format(time.RFC3339)}, "until": {p.until.Format(time.RFC3339)}}
}

// grantLogFlow is what the flow below wrote, so each assertion names the thing it is about.
type grantLogFlow struct {
	enrollmentID, runtimeID string
	podEnrollmentID, podUID string
	granted                 wireCreateRequestResponse
	refused                 wireCreateRequestResponse
	approved                wireCreateRequestResponse
	podRequest              wireCreateRequestResponse
	grantID                 string
	reason                  string
}

// driveGrantLogFlow writes, over real HTTP, every kind of thing a review has to be able to read:
// a machine login and the session it enrolls, a request the policy grants at once, one it refuses
// because the secret lists who may ask for it, one a person approves on the broker's own page,
// that grant's release and its revocation, and a pod enrolled by a service login whose claimed
// service the broker does not verify.
func driveGrantLogFlow(t *testing.T, ts *testServer) grantLogFlow {
	t.Helper()
	flow := grantLogFlow{runtimeID: "box-grant-log", podUID: uuid.NewString(), reason: "close the quarter"}

	// A listed secret nobody in this flow may ask for, so one request is refused by the
	// requesters rule rather than by an owner's denial.
	listed := policytest.ListedSecret("LISTED_KEY", testApprover, policy.TierAgent, listedOwner, "listed-v1")
	ts.Secrets.Put(listed)
	if status, body := ts.req(t, http.MethodPost, "/v1/secrets/LISTED_KEY/reread", nil, nil); status != http.StatusOK {
		t.Fatalf("reread LISTED_KEY = %d %s, want 200", status, body)
	}

	credentialID, machineKey := ts.mintLauncherCredential(t, testApprover, "example-host-devbox")
	sessionKey := newSigningKey(t)
	status, body := ts.launcher(t, machineKey, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "session", "runtime_id": flow.runtimeID, "operator": testApprover,
		"thumbprint": thumbprintOf(t, sessionKey), "session_id": grantLogSessionName,
	})
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/enrollments = %d %s, want 201", status, body)
	}
	flow.enrollmentID = decode[wireEnrolled](t, body).EnrollmentID

	request := func(key *ecdsa.PrivateKey, enrollmentID, reason string, lifetime int, names ...string) wireCreateRequestResponse {
		t.Helper()
		object := signAgentSecretRequestFor(t, key, ts.URL, reason, lifetime, names...)
		status, body := ts.session(t, key, enrollmentID, http.MethodPost, "/v1/requests", map[string]any{"request": object})
		if status != http.StatusOK {
			t.Fatalf("POST /v1/requests (%v) = %d %s, want 200", names, status, body)
		}
		return decode[wireCreateRequestResponse](t, body)
	}

	flow.granted = request(sessionKey, flow.enrollmentID, flow.reason, 3600, "WORKER_TOKEN")
	if flow.granted.State != "granted" {
		t.Fatalf("WORKER_TOKEN = %+v, want granted at once", flow.granted)
	}
	flow.refused = request(sessionKey, flow.enrollmentID, flow.reason, 3600, "LISTED_KEY")
	if flow.refused.State != "denied" {
		t.Fatalf("LISTED_KEY = %+v, want denied: the secret lists who may ask for it", flow.refused)
	}
	flow.approved = request(sessionKey, flow.enrollmentID, flow.reason, 3600, "DEEL_API_KEY")
	if flow.approved.State != "pending" || flow.approved.RecordID == nil {
		t.Fatalf("DEEL_API_KEY = %+v, want a pending record", flow.approved)
	}
	ts.decide(t, testApprover, *flow.approved.RecordID, true)

	status, body = ts.session(t, sessionKey, flow.enrollmentID, http.MethodGet, "/v1/requests/"+flow.approved.RequestID, nil)
	read := decode[wireRequestStatus](t, body)
	if status != http.StatusOK || read.State != "granted" || read.GrantID == nil {
		t.Fatalf("GET /v1/requests/%s after approval = %d %s, want granted with a grant", flow.approved.RequestID, status, body)
	}
	flow.grantID = *read.GrantID

	if status, body := ts.session(t, sessionKey, flow.enrollmentID, http.MethodPost, "/v1/grants/"+flow.grantID+"/values", nil); status != http.StatusOK {
		t.Fatalf("POST /v1/grants/%s/values = %d %s, want 200", flow.grantID, status, body)
	}
	if status, body := ts.session(t, sessionKey, flow.enrollmentID, http.MethodPost, "/v1/grants/"+flow.grantID+"/revoke", nil); status != http.StatusOK {
		t.Fatalf("POST /v1/grants/%s/revoke = %d %s, want 200", flow.grantID, status, body)
	}

	// A pod of an account BROKER_SERVICES maps to nothing, enrolled by the daemon's own service
	// login, whose launcher credential claims legion-daemon: the broker records the claim and
	// verifies no service for it.
	daemonCredential, daemonKey := ts.mintServiceLauncherCredential(t)
	podKey := newSigningKey(t)
	status, body = ts.launcher(t, daemonKey, daemonCredential, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "pod", "runtime_id": flow.podUID, "thumbprint": thumbprintOf(t, podKey),
		"pod_token": ts.podToken(t, workerAccount, flow.podUID), "session_id": grantLogPodName,
	})
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/enrollments (pod) = %d %s, want 201", status, body)
	}
	flow.podEnrollmentID = decode[wireEnrolled](t, body).EnrollmentID
	flow.podRequest = request(podKey, flow.podEnrollmentID, "run the job", 3600, "WORKER_TOKEN")
	if flow.podRequest.State != "granted" {
		t.Fatalf("WORKER_TOKEN as the pod = %+v, want granted at once", flow.podRequest)
	}
	return flow
}

// auditRow is one row of the audit table as this test reads it back, to hold the route's answer
// against the table it answers from.
type auditRow struct {
	id                   int64
	kind, request, grant string
}

// auditRows is every audit row of the period, oldest first, read straight from the table: the
// route's answer is held against this rather than against a list the test wrote by hand, so a
// row the broker writes and the route drops fails here.
func (ts *testServer) auditRows(t *testing.T, period grantLogPeriod) []auditRow {
	t.Helper()
	rows, err := ts.Store.Pool.Query(context.Background(),
		`select id, kind, coalesce(request_id::text,''), coalesce(grant_id::text,'') from audit where at >= $1 and at < $2 order by id`,
		period.since, period.until)
	if err != nil {
		t.Fatalf("read the audit table: %v", err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var row auditRow
		if err := rows.Scan(&row.id, &row.kind, &row.request, &row.grant); err != nil {
			t.Fatalf("read the audit table: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the audit table: %v", err)
	}
	return out
}

// closeGrantLogPeriod moves every row the flow wrote into a period that ended two days ago and
// answers that period. A period's end must be at least an hour before now for the route to read
// it, and the flow's rows were written now, so the test moves them rather than waiting an hour:
// what it is about is which rows the route answers, not the clock.
func closeGrantLogPeriod(t *testing.T, ts *testServer) grantLogPeriod {
	t.Helper()
	at := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	if _, err := ts.Store.Pool.Exec(context.Background(), `update audit set at = $1`, at); err != nil {
		t.Fatalf("move the flow's audit rows into a closed period: %v", err)
	}
	return grantLogPeriod{since: at.Add(-time.Hour), until: at.Add(time.Hour)}
}

// eventFor is the one event of the page naming kind and request, failing t when the page holds no
// such event or more than one.
func eventFor(t *testing.T, events []store.GrantLogEvent, kind, requestID string) store.GrantLogEvent {
	t.Helper()
	var found []store.GrantLogEvent
	for _, ev := range events {
		if ev.Kind == kind && ev.RequestID == requestID {
			found = append(found, ev)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the grant log holds %d %s events for request %s, want exactly one", len(found), kind, requestID)
	}
	return found[0]
}

// TestGrantLog pins what the quarterly access review reads: the caller named access-review reads
// every audit row of a closed period, in the order the broker wrote them, each naming the session
// that wrote it, that session's person, the service the broker verified and the one its
// credential merely claimed, the secrets, the decision and who decided; it pages by audit id, its
// page is held at a thousand rows, and a period that has not closed is refused. The broker logs
// one line per decision and grant end beside those rows, and none per row of this read. The two
// callers that reach every other named-caller route — Dispatch's server and the Envoy listener —
// read nothing here at all.
func TestGrantLog(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	ts, reviewToken, listenerToken := newGrantLogServer(t)
	flow := driveGrantLogFlow(t, ts)
	period := closeGrantLogPeriod(t, ts)
	table := ts.auditRows(t, period)

	status, body := ts.grantLog(t, reviewToken, period.params())
	if status != http.StatusOK {
		t.Fatalf("GET /v1/grant-log as access-review = %d %s, want 200", status, body)
	}
	page := decode[wireGrantLog](t, body)

	t.Run("every audit row of the period, in id order", func(t *testing.T) {
		if len(page.Events) != len(table) {
			t.Fatalf("the grant log answered %d events for a period holding %d audit rows: the review reads every row or it is not a review",
				len(page.Events), len(table))
		}
		for i, ev := range page.Events {
			if ev.ID != table[i].id || ev.Kind != table[i].kind {
				t.Fatalf("event %d is %d %s, want the table's own row %d %s: the log is read in id order, which is write order",
					i, ev.ID, ev.Kind, table[i].id, table[i].kind)
			}
		}
		if !slices.ContainsFunc(page.Events, func(ev store.GrantLogEvent) bool { return ev.Kind == "enrollment.created" }) {
			t.Fatalf("the grant log holds no enrollment.created event: the review reads every audit row of the period, not only the requests")
		}
		if page.NextAfter != page.Events[len(page.Events)-1].ID {
			t.Fatalf("next_after = %d, want the last event's id %d", page.NextAfter, page.Events[len(page.Events)-1].ID)
		}
	})

	t.Run("a request event names its session, person, secrets and decision", func(t *testing.T) {
		ev := eventFor(t, page.Events, "request.created", flow.approved.RequestID)
		if ev.Session.Kind != "session" || ev.Session.SessionID != grantLogSessionName || ev.Session.RuntimeID != flow.runtimeID {
			t.Errorf("the approved request's session = %+v, want the session enrollment %s on %s", ev.Session, grantLogSessionName, flow.runtimeID)
		}
		if ev.Person != testApprover {
			t.Errorf("the approved request names person %q, want the operator %q", ev.Person, testApprover)
		}
		if ev.EnrollmentID != flow.enrollmentID {
			t.Errorf("the approved request names enrollment %q, want %q", ev.EnrollmentID, flow.enrollmentID)
		}
		want := []store.GrantLogSecret{{Name: "DEEL_API_KEY", Decision: "approval"}}
		if !slices.Equal(ev.Secrets, want) {
			t.Errorf("the approved request's secrets = %+v, want %+v", ev.Secrets, want)
		}
		if ev.State != "granted" || ev.Approver != testApprover || ev.DecidedBy != testApprover {
			t.Errorf("the approved request = state %q approver %q decided_by %q, want granted, approved and decided by %s",
				ev.State, ev.Approver, ev.DecidedBy, testApprover)
		}
		if ev.Reason != flow.reason || ev.LifetimeSeconds != 3600 {
			t.Errorf("the approved request = reason %q lifetime %d, want %q and 3600", ev.Reason, ev.LifetimeSeconds, flow.reason)
		}
		// The person's own session proves no service, so the pair below says exactly that: the
		// broker verified none and the machine's credential claimed none either.
		if ev.Service != "" || ev.ClaimedService != "" {
			t.Errorf("the approved request = service %q claimed_service %q, want neither: a person's own machine acts for no service", ev.Service, ev.ClaimedService)
		}
		granted := eventFor(t, page.Events, "request.granted", flow.approved.RequestID)
		if granted.Actor != "human:"+testApprover || granted.GrantID == "" {
			t.Errorf("the approval event = actor %q grant %q, want the approving person and the grant it opened", granted.Actor, granted.GrantID)
		}
	})

	t.Run("a pod's claimed service is not the service the broker verified", func(t *testing.T) {
		ev := eventFor(t, page.Events, "request.created", flow.podRequest.RequestID)
		if ev.Session.Kind != "pod" || ev.Session.RuntimeID != flow.podUID {
			t.Errorf("the pod's request session = %+v, want the pod %s", ev.Session, flow.podUID)
		}
		if ev.ClaimedService != "legion-daemon" {
			t.Errorf("the pod's request claimed_service = %q, want legion-daemon, which its launcher credential claims", ev.ClaimedService)
		}
		if ev.Service != "" {
			t.Errorf("the pod's request service = %q, want empty: BROKER_SERVICES maps the pod's account to nothing, so the broker verified no service for it", ev.Service)
		}
	})

	t.Run("the refused request says which rule refused it", func(t *testing.T) {
		ev := eventFor(t, page.Events, "request.created", flow.refused.RequestID)
		if ev.State != "denied" {
			t.Errorf("the refused request's state = %q, want denied", ev.State)
		}
		if !strings.Contains(ev.Detail, policy.ReasonRequesterNotListed) {
			t.Errorf("the refused request's detail = %q, want it to carry %q: the review reads why a request was refused", ev.Detail, policy.ReasonRequesterNotListed)
		}
		want := []store.GrantLogSecret{{Name: "LISTED_KEY", Decision: "deny"}}
		if !slices.Equal(ev.Secrets, want) {
			t.Errorf("the refused request's secrets = %+v, want %+v", ev.Secrets, want)
		}
	})

	t.Run("paging by after and limit answers each row once", func(t *testing.T) {
		var got []int64
		after := int64(0)
		for pages := 0; ; pages++ {
			if pages > len(table) {
				t.Fatalf("paging the period did not end after %d pages", pages)
			}
			params := period.params()
			params.Set("limit", "2")
			if after > 0 {
				params.Set("after", strconv.FormatInt(after, 10))
			}
			status, body := ts.grantLog(t, reviewToken, params)
			if status != http.StatusOK {
				t.Fatalf("GET /v1/grant-log page %d = %d %s, want 200", pages, status, body)
			}
			answer := decode[wireGrantLog](t, body)
			for _, ev := range answer.Events {
				got = append(got, ev.ID)
			}
			if len(answer.Events) == 0 {
				if answer.NextAfter != after {
					t.Fatalf("an empty page answered next_after %d, want the request's own after %d", answer.NextAfter, after)
				}
				break
			}
			if answer.NextAfter != answer.Events[len(answer.Events)-1].ID {
				t.Fatalf("page %d answered next_after %d, want its last event's id %d", pages, answer.NextAfter, answer.Events[len(answer.Events)-1].ID)
			}
			after = answer.NextAfter
			if len(answer.Events) < 2 {
				break
			}
		}
		want := make([]int64, len(table))
		for i, row := range table {
			want[i] = row.id
		}
		if !slices.Equal(got, want) {
			t.Fatalf("paging answered %v, want every row of the period once, in id order: %v", got, want)
		}
	})

	t.Run("a limit above the cap, and one of zero, are both answered with the cap", func(t *testing.T) {
		// A thousand and two hundred rows of their own, in a period of their own, so that the
		// cap is read off a real answer: the page the route sends back is counted, which is the
		// only way the caller can see the cap at all.
		bulk := seedBulkAudit(t, ts, 1200)
		params := bulk.params()
		params.Set("limit", "5000")
		status, body := ts.grantLog(t, reviewToken, params)
		if status != http.StatusOK {
			t.Fatalf("GET /v1/grant-log limit=5000 = %d %s, want 200", status, body)
		}
		first := decode[wireGrantLog](t, body)
		if len(first.Events) != 1000 {
			t.Fatalf("limit=5000 over a period of 1200 rows answered %d events, want the cap of 1000", len(first.Events))
		}
		if first.NextAfter != first.Events[999].ID {
			t.Fatalf("next_after = %d, want the thousandth event's id %d", first.NextAfter, first.Events[999].ID)
		}
		params.Set("after", strconv.FormatInt(first.NextAfter, 10))
		status, body = ts.grantLog(t, reviewToken, params)
		second := decode[wireGrantLog](t, body)
		if status != http.StatusOK || len(second.Events) != 200 {
			t.Fatalf("the second page = %d, %d events, want 200 and the period's remaining 200 rows", status, len(second.Events))
		}
		// Zero names no page at all, which store.ReadGrantLog refuses; the route reads it as
		// naming nothing and answers the same cap, so a caller sending it is never refused.
		params.Del("after")
		params.Set("limit", "0")
		status, body = ts.grantLog(t, reviewToken, params)
		zero := decode[wireGrantLog](t, body)
		if status != http.StatusOK || len(zero.Events) != 1000 {
			t.Fatalf("limit=0 = %d with %d events, want 200 and the cap's 1000", status, len(zero.Events))
		}
	})

	t.Run("a period that has not closed is refused", func(t *testing.T) {
		params := url.Values{
			"since": {period.since.Format(time.RFC3339)},
			"until": {time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)},
		}
		status, body := ts.grantLog(t, reviewToken, params)
		if status != http.StatusBadRequest || decode[wireError](t, body).Code != "PERIOD_NOT_CLOSED" {
			t.Fatalf("an until a minute before now = %d %s, want 400 PERIOD_NOT_CLOSED: an audit id is not commit order", status, body)
		}
	})

	t.Run("a period or a cursor the broker cannot read is refused", func(t *testing.T) {
		for name, c := range map[string]struct {
			params url.Values
			code   string
			says   string
		}{
			"no since": {url.Values{"until": {period.until.Format(time.RFC3339)}}, "PERIOD_REQUIRED", "since"},
			"no until": {url.Values{"since": {period.since.Format(time.RFC3339)}}, "PERIOD_REQUIRED", "until"},
			"a since that is not a time": {url.Values{
				"since": {"last tuesday"}, "until": {period.until.Format(time.RFC3339)}}, "PERIOD_INVALID", "since"},
			"an until that is not a time": {url.Values{
				"since": {period.since.Format(time.RFC3339)}, "until": {"soon"}}, "PERIOD_INVALID", "until"},
			"an after that is not a number": {withParam(period.params(), "after", "first"), "PAGE_INVALID", "after"},
			"a negative after":              {withParam(period.params(), "after", "-1"), "PAGE_INVALID", "after"},
			"a limit that is not a number":  {withParam(period.params(), "limit", "lots"), "PAGE_INVALID", "limit"},
			"a negative limit":              {withParam(period.params(), "limit", "-5"), "PAGE_INVALID", "limit"},
		} {
			status, body := ts.grantLog(t, reviewToken, c.params)
			refusal := decode[wireError](t, body)
			if status != http.StatusBadRequest || refusal.Code != c.code || !strings.Contains(refusal.Error, c.says) {
				t.Errorf("%s = %d %s, want 400 %s naming %q", name, status, body, c.code, c.says)
			}
		}
	})

	t.Run("the broker logs one line per decision and grant end, and none per row read", func(t *testing.T) {
		uses := 0
		var logged []auditRow
		for _, row := range table {
			switch {
			case row.kind == "grant.used":
				uses++
			case strings.HasPrefix(row.kind, "request.") || row.kind == "grant.revoked" || row.kind == "grant.withheld":
				logged = append(logged, row)
			}
		}
		if uses == 0 {
			t.Fatal("the flow wrote no grant.used row, so the exclusion of a release from the log is not being tested against a real row")
		}
		lines := grantLogLines(t, capture)
		if len(lines) != len(logged) {
			t.Fatalf("the broker logged %d grant-log lines for %d audit rows of a logged kind: one line per decision and grant end, and no line for a release, an enrollment or a row this route read",
				len(lines), len(logged))
		}
		for i, line := range lines {
			kind, _ := line["kind"].(string)
			request, _ := line["request_id"].(string)
			if kind != logged[i].kind || request != logged[i].request {
				t.Fatalf("line %d is %s for request %q, want the audit row %s for %q, in the table's own order",
					i, kind, request, logged[i].kind, logged[i].request)
			}
		}
	})

	t.Run("no other caller reads the grant log", func(t *testing.T) {
		for name, bearer := range map[string]string{"dispatch": ts.uiToken, "listener": listenerToken} {
			t.Run("as "+name, func(t *testing.T) {
				status, body := ts.grantLog(t, bearer, period.params())
				if status != http.StatusForbidden || decode[wireError](t, body).Code != "ACCOUNT_LACKS_POWER" {
					t.Fatalf("GET /v1/grant-log as %s = %d %s, want 403 ACCOUNT_LACKS_POWER: the review's power is the review's alone", name, status, body)
				}
				if answer := decode[wireGrantLog](t, body); len(answer.Events) != 0 {
					t.Fatalf("the refusal to %s carried %d events", name, len(answer.Events))
				}
			})
		}
	})
}

// withParam is params with one more parameter set, so a table of refusals reads as one line each.
func withParam(params url.Values, name, value string) url.Values {
	out := url.Values{}
	for k, v := range params {
		out[k] = v
	}
	out.Set(name, value)
	return out
}

// seedBulkAudit writes n audit rows into a period of their own, ten days back, and answers that
// period. They are written straight to the table because what they are for is the count of a
// page: a thousand rows driven through the request machine would say nothing more about the cap
// and would take minutes to write.
func seedBulkAudit(t *testing.T, ts *testServer, n int) grantLogPeriod {
	t.Helper()
	at := time.Now().Add(-10 * 24 * time.Hour).UTC().Truncate(time.Second)
	if _, err := ts.Store.Pool.Exec(context.Background(),
		`insert into audit (at, kind, actor) select $1, 'grant.used', 'broker' from generate_series(1, $2)`, at, n); err != nil {
		t.Fatalf("seed %d audit rows: %v", n, err)
	}
	return grantLogPeriod{since: at.Add(-time.Hour), until: at.Add(time.Hour)}
}

// explainTracer runs `explain (analyze, buffers)` for every statement the pool it is attached to
// runs over the audit table, on a connection of its own, and keeps the plan. Tracing the pool is
// what lets the measurement be of the read store.ReadGrantLog really makes, with the parameters
// it really makes it with, instead of a copy of that SQL kept in this test and free to drift
// from it.
type explainTracer struct {
	// plain is the pool the EXPLAIN itself runs on: the traced connection is in the middle of
	// its own query, so the plan cannot be asked for over it.
	plain *pgxpool.Pool
	on    bool
	plans []string
	fails []error
}

func (e *explainTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !e.on || !strings.Contains(data.SQL, "from audit") {
		return ctx
	}
	rows, err := e.plain.Query(ctx, "explain (analyze, buffers) "+data.SQL, data.Args...)
	if err != nil {
		e.fails = append(e.fails, err)
		return ctx
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			e.fails = append(e.fails, err)
			return ctx
		}
		plan = append(plan, line)
	}
	if err := rows.Err(); err != nil {
		e.fails = append(e.fails, err)
		return ctx
	}
	e.plans = append(e.plans, strings.Join(plan, "\n"))
	return ctx
}

func (e *explainTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// grantLogPage is the page the route answers with (handlers_grantlog.go's grantLogLimit),
// so the pages measured below are the pages a review really reads.
const grantLogPage = 1000

// TestGrantLogPagesByPrimaryKey measures what reading a closed period costs when the audit table
// has no index on at, which this change may not add: rows are seeded before, inside and after a
// period, the period is read page by page, and the plan of the first page and of the last are
// recorded with `explain (analyze, buffers)` and written to the test's own output.
//
// It asserts correctness only — every row of the period, each once, and nothing from outside it —
// and deliberately asserts nothing about the plan's shape or its timing: a test that pinned a
// sequential scan would fail the day an index made the read faster, and one that pinned a
// duration would fail on a loaded machine. The numbers are here to be read. If they ever say a
// quarterly read is not viable, that is a measurement to take to the architect, not a migration
// to add quietly.
//
// The plans it records are of a table of a few thousand rows, where Postgres reads the whole of
// audit and sorts it rather than walking audit_pkey, because at that size the scan is the cheaper
// plan. That is not what a deployment's table does, and the test says nothing about which plan is
// right: measured on this change at 2,000,000 audit rows with a 256,413-row quarter (PostgreSQL
// 18.6), the planner reads audit_pkey forward and sorts nothing, which is what
// store.readGrantLog's own comment describes — 725 ms for the first page, which walks the rows
// older than since, 55 ms for a page inside the period and 484 ms for the last, which walks to
// the end of the table, so the whole quarter is 257 pages and about 15 s.
//
// It reads through store.ReadGrantLog rather than over HTTP because the plan is of that one
// statement, and tracing it needs a pool of the test's own; the route's own answer over HTTP,
// its paging included, is TestGrantLog's.
func TestGrantLogPagesByPrimaryKey(t *testing.T) {
	ctx := context.Background()
	databaseURL := storetest.URL(t)
	plain, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open the plan pool: %v", err)
	}
	t.Cleanup(plain.Close)
	tracer := &explainTracer{plain: plain}
	traced, err := store.Open(ctx, databaseURL, func(config *pgxpool.Config) { config.ConnConfig.Tracer = tracer })
	if err != nil {
		t.Fatalf("open the traced pool: %v", err)
	}
	t.Cleanup(traced.Close)

	at := time.Now().Add(-30 * 24 * time.Hour).UTC().Truncate(time.Second)
	period := grantLogPeriod{since: at.Add(-time.Hour), until: at.Add(time.Hour)}
	seed := func(when time.Time, n int, kind string) {
		t.Helper()
		if _, err := plain.Exec(ctx, `insert into audit (at, kind, actor) select $1, $3, 'broker' from generate_series(1, $2)`, when, n, kind); err != nil {
			t.Fatalf("seed %d %s rows: %v", n, kind, err)
		}
	}
	// Rows on both sides of the period, so the read is one that has to find the period's start
	// and stop at its end rather than one that happens to read the whole table.
	const inside = 2500
	seed(period.since.Add(-24*time.Hour), 2000, "grant.used")
	seed(at, inside, "request.created")
	seed(period.until.Add(24*time.Hour), 2000, "grant.revoked")

	var want []int64
	rows, err := plain.Query(ctx, `select id from audit where at >= $1 and at < $2 order by id`, period.since, period.until)
	if err != nil {
		t.Fatalf("read the period's ids: %v", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("read the period's ids: %v", err)
		}
		want = append(want, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read the period's ids: %v", err)
	}
	if len(want) != inside {
		t.Fatalf("the period holds %d rows, want the %d seeded inside it", len(want), inside)
	}

	tracer.on = true
	var got []int64
	after := int64(0)
	for pages := 0; ; pages++ {
		if pages > inside/grantLogPage+2 {
			t.Fatalf("paging the period did not end after %d pages", pages)
		}
		events, err := store.ReadGrantLog(ctx, traced.Pool, store.GrantLogQuery{
			Since: period.since, Until: period.until, After: after, Limit: grantLogPage,
		})
		if err != nil {
			t.Fatalf("ReadGrantLog page %d: %v", pages, err)
		}
		for _, ev := range events {
			got = append(got, ev.ID)
		}
		if len(events) < grantLogPage {
			break
		}
		after = events[len(events)-1].ID
	}
	tracer.on = false

	if !slices.Equal(got, want) {
		t.Fatalf("paging answered %d ids, want the period's %d rows each once, in id order", len(got), len(want))
	}
	for _, err := range tracer.fails {
		t.Fatalf("explain the page: %v", err)
	}
	if len(tracer.plans) < 2 {
		t.Fatalf("the read was traced %d times, want a plan for each page", len(tracer.plans))
	}
	t.Log(fmt.Sprintf("explain (analyze, buffers), FIRST page of the period (%d rows before it, %d inside, %d after):\n%s",
		2000, inside, 2000, tracer.plans[0]))
	t.Log(fmt.Sprintf("explain (analyze, buffers), LAST page of the period:\n%s", tracer.plans[len(tracer.plans)-1]))
}
