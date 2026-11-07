package requests

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/logtest"
)

// grantLogLine is one captured store.GrantLogMessage line: its attributes by key, and the level
// it was written at.
type grantLogLine struct {
	attrs map[string]any
	level slog.Level
}

// grantLogLines are the grant-log lines capture holds, in the order they were written, so a test
// reads the attributes the request machine logged rather than a rendering of them. Every other
// line the broker logs is skipped: this is the one line the access review's log pipeline keeps.
// The records are one test's own: logtest puts the previous default logger back when it ends.
func grantLogLines(t *testing.T, capture *logtest.Recorder) []grantLogLine {
	t.Helper()
	var lines []grantLogLine
	for _, r := range capture.Records() {
		if r.Message != store.GrantLogMessage {
			continue
		}
		line := grantLogLine{attrs: map[string]any{}, level: r.Level}
		r.Attrs(func(a slog.Attr) bool {
			if _, twice := line.attrs[a.Key]; twice {
				t.Fatalf("the grant-log line carries %q twice", a.Key)
			}
			line.attrs[a.Key] = a.Value.Any()
			return true
		})
		lines = append(lines, line)
	}
	return lines
}

// newGrantLogFixture is newFixture with every log line captured and its enrollment named as a
// session, which is where the route reads the session fields from.
func newGrantLogFixture(t *testing.T, sessionID string) (*logtest.Recorder, *Machine, string, *ecdsa.PrivateKey, string) {
	t.Helper()
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	m, enr, key, approver := newFixture(t)
	nameSession(t, m, enr, sessionID)
	return capture, m, enr, key, approver
}

// nameSession gives an enrollment the Envoy session name it would have been enrolled under, so a
// test reads session fields that are not empty.
func nameSession(t *testing.T, m *Machine, enrollmentID, sessionID string) {
	t.Helper()
	if _, err := m.Store.Pool.Exec(context.Background(), `update enrollments set session_id=$2 where id=$1`, enrollmentID, sessionID); err != nil {
		t.Fatalf("name the enrollment's session: %v", err)
	}
}

// grantLogPage is the limit these reads pass: ReadGrantLog answers a page at a time and requires
// its size (store.ErrGrantLogLimit), and a hundred rows is far more than any test below writes,
// so one read still answers everything it wrote.
const grantLogPage = 100

// readGrantLog is the grant-log route's read of everything the test has written.
func readGrantLog(t *testing.T, m *Machine, since time.Time) []store.GrantLogEvent {
	t.Helper()
	events, err := store.ReadGrantLog(context.Background(), m.Store.Pool, store.GrantLogQuery{
		Since: since, Until: time.Now().Add(time.Hour), Limit: grantLogPage,
	})
	if err != nil {
		t.Fatalf("ReadGrantLog: %v", err)
	}
	return events
}

// sameAttr compares one attribute of the machine's line with the same attribute of the route's.
// A raw detail is compared as JSON, because Postgres's jsonb holds an object's keys in its own
// order and the broker's own marshalling holds them in Go's.
func sameAttr(got, want any) bool {
	if reflect.DeepEqual(got, want) {
		return true
	}
	gotText, gotIsText := got.(string)
	wantText, wantIsText := want.(string)
	if !gotIsText || !wantIsText {
		return false
	}
	var a, b any
	if json.Unmarshal([]byte(gotText), &a) != nil || json.Unmarshal([]byte(wantText), &b) != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

// routeLine is ev as store.LogGrantEvent writes it, captured. The drift test compares the
// machine's line with the route's event through that one writer rather than rendering the event
// a second way of its own, so what it pins is the two constructions and nothing else.
func routeLine(t *testing.T, capture *logtest.Recorder, ev store.GrantLogEvent) grantLogLine {
	t.Helper()
	before := len(grantLogLines(t, capture))
	store.LogGrantEvent(ev)
	lines := grantLogLines(t, capture)
	if len(lines) != before+1 {
		t.Fatalf("LogGrantEvent wrote %d lines, want one", len(lines)-before)
	}
	return lines[before]
}

// lineFor is the one captured line naming kind and request.
func lineFor(t *testing.T, lines []grantLogLine, kind, requestID string) grantLogLine {
	t.Helper()
	var found []grantLogLine
	for _, line := range lines {
		if line.attrs["kind"] == kind && line.attrs["request_id"] == requestID {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the broker logged %d %s lines for request %s, want exactly one", len(found), kind, requestID)
	}
	return found[0]
}

// eventFor is the one audit row of that kind and request as the route reads it.
func eventFor(t *testing.T, events []store.GrantLogEvent, kind, requestID string) store.GrantLogEvent {
	t.Helper()
	var found []store.GrantLogEvent
	for _, ev := range events {
		if ev.Kind == kind && ev.RequestID == requestID {
			found = append(found, ev)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the audit table holds %d %s rows for request %s, want exactly one", len(found), kind, requestID)
	}
	return found[0]
}

// TestGrantLogLineAndTheRoutesEventSayTheSameThing is the drift test the two constructions need:
// the line the request machine writes from what its transaction had in hand, and the event the
// grant-log route reads back from the audit table for that same row, must agree on every field
// the line carries. A later change that fills one and not the other fails here. The route's
// event is rendered through store.LogGrantEvent itself, so what is compared is the two
// constructions and not a third rendering of either.
func TestGrantLogLineAndTheRoutesEventSayTheSameThing(t *testing.T) {
	capture, m, enr, key, approver := newGrantLogFixture(t, "sess-drift")
	ctx := context.Background()
	since := time.Now().Add(-time.Hour)
	// The three shapes a decision comes in: the policy granting a person's session at once, the
	// policy granting a service's pod at once (where the verified service and the credential's
	// own claim both have a value), and a person's approval of a pending request.
	pod, podKey := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-drift")
	nameSession(t, m, pod, "sess-pod")

	automatic, err := m.Create(ctx, enr, signRequest(t, m, key, "read the ledger", "AUTO_TOKEN"), "")
	if err != nil || automatic.State != "granted" {
		t.Fatalf("Create(AUTO_TOKEN) = %+v, %v; want granted", automatic, err)
	}
	service, err := m.Create(ctx, pod, signRequest(t, m, podKey, "run the job", "SERVICE_KEY"), "")
	if err != nil || service.State != "granted" {
		t.Fatalf("Create(SERVICE_KEY) = %+v, %v; want granted", service, err)
	}
	pending, err := m.Create(ctx, enr, signRequest(t, m, key, "ship the release", "DEEL_API_KEY"), "")
	if err != nil || pending.RecordID == nil {
		t.Fatalf("Create(DEEL_API_KEY) = %+v, %v; want pending", pending, err)
	}
	if decision, err := m.ApplyDecision(ctx, *pending.RecordID, true, approver); err != nil || decision.State != "granted" {
		t.Fatalf("ApplyDecision = %+v, %v; want granted", decision, err)
	}

	machineLines := grantLogLines(t, capture)
	events := readGrantLog(t, m, since)
	for _, tc := range []struct {
		name, kind, request string
		// carries is what this call site had in hand, so a change that stops the line naming
		// one of them is a change in what the log pipeline keeps, and fails here.
		carries []string
	}{
		{
			name: "a request the policy granted a person's session at once", kind: "request.created", request: automatic.ID,
			carries: []string{"kind", "actor", "enrollment_id", "request_id", "session", "person", "secrets", "state", "lifetime_seconds", "reason", "raw"},
		},
		{
			name: "a request the policy granted a service's pod at once", kind: "request.created", request: service.ID,
			carries: []string{"kind", "actor", "enrollment_id", "request_id", "session", "service", "claimed_service", "secrets", "state", "lifetime_seconds", "reason", "raw"},
		},
		{
			name: "a request a person approved", kind: "request.granted", request: pending.ID,
			carries: []string{"kind", "actor", "enrollment_id", "request_id", "grant_id", "session", "person", "state", "approver", "decided_by", "lifetime_seconds", "reason", "raw"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := lineFor(t, machineLines, tc.kind, tc.request)
			if line.level != slog.LevelInfo {
				t.Fatalf("the grant-log line is at %s, want INFO", line.level)
			}
			route := routeLine(t, capture, eventFor(t, events, tc.kind, tc.request))
			for key, got := range line.attrs {
				want, ok := route.attrs[key]
				if !ok {
					t.Fatalf("the line carries %s=%v, which the route's event for the same row does not carry at all", key, got)
				}
				if !sameAttr(got, want) {
					t.Fatalf("the line carries %s=%v and the route's event %v for the same audit row", key, got, want)
				}
			}
			for _, key := range tc.carries {
				if _, ok := line.attrs[key]; !ok {
					t.Fatalf("the line does not carry %s, which this decision had in hand: %v", key, line.attrs)
				}
			}
		})
	}
}

// loggedKind reports whether an audit row of that kind is one the broker logs a grant-log line
// for: a request decision and a grant's end, never a release (grant.used) and never an
// enrollment's own row.
func loggedKind(kind string) bool {
	return strings.HasPrefix(kind, "request.") || kind == "grant.revoked" || kind == "grant.withheld"
}

// auditTrail is every audit row the test wrote, oldest first, as kind, request and grant.
func auditTrail(t *testing.T, m *Machine) [][3]string {
	t.Helper()
	rows, err := m.Store.Pool.Query(context.Background(),
		`select kind, coalesce(request_id::text,''), coalesce(grant_id::text,'') from audit order by id`)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	defer rows.Close()
	var trail [][3]string
	for rows.Next() {
		var row [3]string
		if err := rows.Scan(&row[0], &row[1], &row[2]); err != nil {
			t.Fatalf("read audit: %v", err)
		}
		trail = append(trail, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	return trail
}

// loggedRows are the audit rows a grant-log line is written for, in the table's own order.
func loggedRows(trail [][3]string) [][3]string {
	var logged [][3]string
	for _, row := range trail {
		if loggedKind(row[0]) {
			logged = append(logged, row)
		}
	}
	return logged
}

// linedRows are the captured lines in the same shape as loggedRows, so one compares with the
// other.
func linedRows(t *testing.T, lines []grantLogLine) [][3]string {
	t.Helper()
	var lined [][3]string
	for _, line := range lines {
		var row [3]string
		for i, key := range []string{"kind", "request_id", "grant_id"} {
			value, ok := line.attrs[key]
			if !ok {
				continue
			}
			text, isText := value.(string)
			if !isText {
				t.Fatalf("the line's %s is %v, want a string", key, value)
			}
			row[i] = text
		}
		lined = append(lined, row)
	}
	return lined
}

// TestGrantLogLogsEveryDecisionAndEveryGrantEndAndNothingElse pins the volume: one line per
// request decision and per grant end, in the order the audit table holds those rows, and none at
// all for a release (grant.used) or for an enrollment's own row. A session that reads its grant
// every few seconds writes one audit row per release and no line for any of them.
func TestGrantLogLogsEveryDecisionAndEveryGrantEndAndNothingElse(t *testing.T) {
	capture, m, enr, key, approver := newGrantLogFixture(t, "sess-volume")
	ctx := context.Background()

	automatic, err := m.Create(ctx, enr, signRequest(t, m, key, "read the ledger", "AUTO_TOKEN"), "")
	if err != nil || automatic.GrantID == nil {
		t.Fatalf("Create(AUTO_TOKEN) = %+v, %v; want a grant", automatic, err)
	}
	for range 3 {
		if _, _, err := m.Values(ctx, *automatic.GrantID, enr, nil); err != nil {
			t.Fatalf("Values: %v", err)
		}
	}
	cancelled, err := m.Create(ctx, enr, signRequest(t, m, key, "never mind", "DEEL_API_KEY"), "")
	if err != nil || cancelled.RecordID == nil {
		t.Fatalf("Create(DEEL_API_KEY) = %+v, %v; want pending", cancelled, err)
	}
	if err := m.Cancel(ctx, cancelled.ID, enr); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	approved, err := m.Create(ctx, enr, signRequest(t, m, key, "share it", "SHARED_KEY"), "")
	if err != nil || approved.RecordID == nil {
		t.Fatalf("Create(SHARED_KEY) = %+v, %v; want pending", approved, err)
	}
	decision, err := m.ApplyDecision(ctx, *approved.RecordID, true, approver)
	if err != nil || decision.GrantID == "" {
		t.Fatalf("ApplyDecision = %+v, %v; want a grant", decision, err)
	}
	expired, err := m.Create(ctx, enr, signRequest(t, m, key, "ask alice", "ALICE_KEY"), "")
	if err != nil || expired.RecordID == nil {
		t.Fatalf("Create(ALICE_KEY) = %+v, %v; want pending", expired, err)
	}
	if _, err := m.Store.Pool.Exec(ctx, `update requests set pending_expires_at = now() - interval '1 minute' where id=$1`, expired.ID); err != nil {
		t.Fatalf("age the pending request: %v", err)
	}
	if ended, err := m.ExpirePending(ctx, time.Now()); err != nil || ended != 1 {
		t.Fatalf("ExpirePending = %d, %v; want the one aged request", ended, err)
	}
	if err := m.RevokeGrant(ctx, *automatic.GrantID, enr); err != nil {
		t.Fatalf("RevokeGrant: %v", err)
	}
	// A max-grant below the approved grant's own lifetime ends that grant at its next release,
	// which is the broker's own grant end (endGrant).
	capped := policytest.Secret("SHARED_KEY", policy.OwnerShared, policy.TierHuman, "shared-v1")
	capped.Tags[policy.TagMaxGrant] = "1m"
	retag(t, m, capped)
	if _, _, err := m.Values(ctx, decision.GrantID, enr, nil); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("Values under a max-grant below the grant's own lifetime = %v, want ErrGrantNotLive", err)
	}

	trail := auditTrail(t, m)
	uses := 0
	for _, row := range trail {
		if row[0] == "grant.used" {
			uses++
		}
	}
	if uses != 3 {
		t.Fatalf("the audit table holds %d grant.used rows, want one per release: the exclusion has to be tested against real rows", uses)
	}
	lines := grantLogLines(t, capture)
	for _, line := range lines {
		kind, _ := line.attrs["kind"].(string)
		if !loggedKind(kind) {
			t.Fatalf("the broker logged a grant-log line for %s, which is neither a request decision nor a grant end", kind)
		}
	}
	want := loggedRows(trail)
	if got := linedRows(t, lines); !reflect.DeepEqual(got, want) {
		t.Fatalf("the broker logged\n%v\nwant one line per audit row of a logged kind, in the table's own order:\n%v", got, want)
	}
	kinds := make([]string, len(want))
	for i, row := range want {
		kinds[i] = row[0]
	}
	wantKinds := []string{"request.created", "request.created", "request.cancelled", "request.created",
		"request.granted", "request.created", "request.expired", "grant.revoked", "grant.revoked"}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("the flow wrote %v, want %v: every logged call site has to be exercised", kinds, wantKinds)
	}
}

// TestGrantLogLogsOneLinePerAuditRowAndNotOnePerTransaction pins the rule for a writer that ends
// more than one grant at once: an operator's revoke withholds the names its grant got
// automatically and ends every other live grant of the session that got one of them, each with a
// grant.revoked audit row of its own, so the log carries one line per grant rather than one per
// revoke.
func TestGrantLogLogsOneLinePerAuditRowAndNotOnePerTransaction(t *testing.T) {
	capture, m, enr, key, approver := newGrantLogFixture(t, "sess-withhold")
	ctx := context.Background()

	automatic, err := m.Create(ctx, enr, signRequest(t, m, key, "read the ledger", "AUTO_TOKEN"), "")
	if err != nil || automatic.GrantID == nil {
		t.Fatalf("Create(AUTO_TOKEN) = %+v, %v; want a grant", automatic, err)
	}
	both, err := m.Create(ctx, enr, signRequest(t, m, key, "both of them", "AUTO_TOKEN", "SHARED_KEY"), "")
	if err != nil || both.RecordID == nil {
		t.Fatalf("Create(AUTO_TOKEN, SHARED_KEY) = %+v, %v; want pending", both, err)
	}
	decision, err := m.ApplyDecision(ctx, *both.RecordID, true, approver)
	if err != nil || decision.GrantID == "" {
		t.Fatalf("ApplyDecision = %+v, %v; want a grant", decision, err)
	}
	before := len(grantLogLines(t, capture))
	if err := m.RevokeByApprover(ctx, *automatic.GrantID, fixtureOperator, "human:"+fixtureOperator); err != nil {
		t.Fatalf("RevokeByApprover: %v", err)
	}

	lines := grantLogLines(t, capture)[before:]
	if len(lines) != 2 {
		t.Fatalf("the revoke logged %d lines, want one per grant it ended: its own and the one the withhold ended", len(lines))
	}
	var grants []string
	for _, line := range lines {
		if line.attrs["kind"] != "grant.revoked" {
			t.Fatalf("the revoke logged a %v line, want grant.revoked", line.attrs["kind"])
		}
		grant, _ := line.attrs["grant_id"].(string)
		grants = append(grants, grant)
	}
	slices.Sort(grants)
	wantGrants := []string{*automatic.GrantID, decision.GrantID}
	slices.Sort(wantGrants)
	if !reflect.DeepEqual(grants, wantGrants) {
		t.Fatalf("the revoke logged grants %v, want the revoked grant and the one the withhold ended, %v", grants, wantGrants)
	}
	logged := loggedRows(auditTrail(t, m))
	if got := linedRows(t, lines); !reflect.DeepEqual(got, logged[len(logged)-2:]) {
		t.Fatalf("the two lines are %v, want the two audit rows the revoke wrote, in the table's order: %v", got, logged[len(logged)-2:])
	}
}
