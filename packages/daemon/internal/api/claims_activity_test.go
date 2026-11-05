package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// storedActivity is the four activity columns of a claim's row, which is where the report lives
// and what the idle rule reads: a route that wrote onto the claim's machine and nothing else
// would pass against the machine and fail here. No claim read carries them.
type storedActivity struct {
	at         time.Time
	background bool
	kinds      string
	reminder   time.Time
}

func readActivity(t *testing.T, h *harness, token claim.Token) storedActivity {
	t.Helper()
	var read storedActivity
	var at, reminder *time.Time
	if err := h.store.Pool().QueryRow(h.ctx,
		`select activity_at, activity_background, activity_kinds, activity_next_reminder_at from claims where token = $1`,
		string(token)).Scan(&at, &read.background, &read.kinds, &reminder); err != nil {
		t.Fatalf("read the activity of %s: %v", token, err)
	}
	if at != nil {
		read.at = *at
	}
	if reminder != nil {
		read.reminder = *reminder
	}
	return read
}

// INFRA-945: the four values the idle rule reads of a live session reach the claim's row and come
// back from it. The route decides nothing about the claim, so nothing but those four moves.
func TestClaimActivityIsWrittenOntoTheClaimAndReadBack(t *testing.T) {
	h := newHarness(t)
	token, bootToken := h.launch("LEGION-208", "implementer")
	registered := h.registered(bootToken, "ses_implementer")

	read := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	wake := read.Add(5 * time.Minute)
	recorder := h.request(http.MethodPost, "/legion/v1/claims/activity", claim.ActivityRequest{
		ClaimToken: token, SessionID: "ses_implementer", Secret: registered.Secret,
		Activity: claim.SessionActivity{
			At:             read,
			BackgroundWork: claim.BackgroundWork{Running: true, Kinds: []string{"async-job", "subagent"}},
			Phase:          claim.PhaseActivity{NextReminderAt: &wake},
		},
	}, nil)
	if recorder.Code != http.StatusNoContent || recorder.Body.Len() != 0 {
		t.Fatalf("activity = %d with body %s, want 204 and nothing", recorder.Code, recorder.Body)
	}

	stored := readActivity(t, h, token)
	if !stored.at.Equal(read) {
		t.Errorf("activity_at = %s, want %s", stored.at, read)
	}
	if !stored.background {
		t.Error("activity_background = false, want true")
	}
	if stored.kinds != "async-job,subagent" {
		t.Errorf("activity_kinds = %q, want \"async-job,subagent\"", stored.kinds)
	}
	if !stored.reminder.Equal(wake) {
		t.Errorf("activity_next_reminder_at = %s, want %s", stored.reminder, wake)
	}

	// The work ended and no reminder is armed any more: an empty kind list and no wake are
	// written as such, never left at what the last report said.
	later := read.Add(time.Minute)
	if recorder := h.request(http.MethodPost, "/legion/v1/claims/activity", claim.ActivityRequest{
		ClaimToken: token, SessionID: "ses_implementer", Secret: registered.Secret,
		Activity: claim.SessionActivity{
			At:    later,
			Phase: claim.PhaseActivity{},
		},
	}, nil); recorder.Code != http.StatusNoContent {
		t.Fatalf("the second activity = %d; body %s", recorder.Code, recorder.Body)
	}
	quiet := readActivity(t, h, token)
	if !quiet.at.Equal(later) || quiet.background || quiet.kinds != "" || !quiet.reminder.IsZero() {
		t.Errorf("the quiet report read back as %+v", quiet)
	}
}

// A report nobody can prove they own tells the daemon nothing: a wrong secret, a claim the daemon
// does not hold, and a body naming no claim are all refused, and the claim keeps what it had.
func TestClaimActivityRefusesACallerThatCannotProveItsClaim(t *testing.T) {
	h := newHarness(t)
	token, bootToken := h.launch("LEGION-208", "implementer")
	registered := h.registered(bootToken, "ses_implementer")
	read := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	activity := claim.SessionActivity{At: read, Phase: claim.PhaseActivity{}}

	wrongSecret := h.request(http.MethodPost, "/legion/v1/claims/activity", claim.ActivityRequest{
		ClaimToken: token, SessionID: "ses_implementer", Secret: "not-the-secret", Activity: activity,
	}, nil)
	if wrongSecret.Code != claim.InvalidSecret.Status {
		t.Errorf("a wrong secret = %d, want %d; body %s", wrongSecret.Code, claim.InvalidSecret.Status, wrongSecret.Body)
	}

	unknown := h.request(http.MethodPost, "/legion/v1/claims/activity", claim.ActivityRequest{
		ClaimToken: "legion-legion-legion-999-implementer", SessionID: "ses_other",
		Secret: registered.Secret, Activity: activity,
	}, nil)
	if unknown.Code != claim.InvalidSecret.Status {
		t.Errorf("a claim the daemon does not hold = %d, want %d; body %s", unknown.Code, claim.InvalidSecret.Status, unknown.Body)
	}

	nameless := h.request(http.MethodPost, "/legion/v1/claims/activity", claim.ActivityRequest{
		SessionID: "ses_implementer", Secret: registered.Secret, Activity: activity,
	}, nil)
	if nameless.Code != http.StatusBadRequest {
		t.Errorf("a body naming no claim = %d, want 400; body %s", nameless.Code, nameless.Body)
	}

	undated := h.request(http.MethodPost, "/legion/v1/claims/activity", claim.ActivityRequest{
		ClaimToken: token, SessionID: "ses_implementer", Secret: registered.Secret,
	}, nil)
	if undated.Code != http.StatusBadRequest {
		t.Errorf("a report with no reading time = %d, want 400; body %s", undated.Code, undated.Body)
	}

	if stored := readActivity(t, h, token); !stored.at.IsZero() {
		t.Errorf("a refused report left activity_at at %s", stored.at)
	}
}
