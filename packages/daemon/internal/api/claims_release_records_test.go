package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// windowStart is the moment the records tests read their window from: every record a test writes
// is placed around it, so what the window holds is the test's own statement.
var windowStart = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

// seedReleaseRecords writes one release of a pod, the wake that brought it back, one hold of
// another pod, and one hold from before the window, and answers the release's and the resume's
// row ids.
func seedReleaseRecords(t *testing.T, h *harness) (release, resume int64) {
	t.Helper()
	release, err := h.store.RecordPodRelease(h.ctx, store.PodRelease{
		Project: testProject, Issue: "LEGION-208", Generation: 3,
		Roles: []string{"implementer", "tester"}, At: windowStart, BundleBytes: 2048,
	})
	if err != nil {
		t.Fatalf("record a release: %v", err)
	}
	resume, err = h.store.RecordPodWake(h.ctx, store.PodResume{
		Release: release, Project: testProject, Issue: "LEGION-208", Role: "implementer",
		Kind: "delivery", At: windowStart.Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("record a wake: %v", err)
	}
	if err := h.store.MarkPodResumeReady(h.ctx, resume, windowStart.Add(7*time.Minute)); err != nil {
		t.Fatalf("stamp the resume ready: %v", err)
	}
	if err := h.store.RecordPodHold(h.ctx, store.PodHold{
		Project: testProject, Issue: "LEGION-209", Reason: "background work", At: windowStart.Add(time.Minute),
	}); err != nil {
		t.Fatalf("record a hold: %v", err)
	}
	// Older than any window a test reads: a record of the hour before is not this window's.
	if err := h.store.RecordPodHold(h.ctx, store.PodHold{
		Project: testProject, Issue: "LEGION-207", Reason: "an old image", At: windowStart.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("record an older hold: %v", err)
	}
	return release, resume
}

// INFRA-945 R6: a pod holds its claim token and the secret its registration was issued, and no
// operator token, so that credential is what reads the release records. The window is the
// caller's: every release, wake and hold at or after its `since`, and nothing before it.
func TestReleaseRecordsAnswersAPodsClaimCredential(t *testing.T) {
	h := newHarness(t)
	token, bootToken := h.launch("LEGION-208", "implementer")
	registered := h.registered(bootToken, "ses_implementer")
	release, resume := seedReleaseRecords(t, h)

	recorder := h.request(http.MethodPost, "/legion/v1/claims/release-records", ReleaseRecordsRequest{
		ClaimToken: token, Secret: registered.Secret, Since: windowStart.Add(-time.Minute),
	}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("release-records = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	var records ReleaseRecordsResponse
	decodeInto(t, recorder, &records)

	if len(records.Releases) != 1 {
		t.Fatalf("releases = %+v, want the one release of the window", records.Releases)
	}
	got := records.Releases[0]
	if got.ID != release || got.Issue != "LEGION-208" || got.Generation != 3 || got.BundleBytes != 2048 ||
		!got.At.Equal(windowStart) || len(got.Roles) != 2 || got.Roles[0] != "implementer" || got.Roles[1] != "tester" {
		t.Errorf("the release read back as %+v", got)
	}
	if len(records.Resumes) != 1 {
		t.Fatalf("resumes = %+v, want the one wake of the window", records.Resumes)
	}
	wake := records.Resumes[0]
	if wake.ID != resume || wake.Release != release || wake.Issue != "LEGION-208" || wake.Role != "implementer" ||
		wake.Kind != "delivery" || !wake.At.Equal(windowStart.Add(5*time.Minute)) || wake.Stuck {
		t.Errorf("the wake read back as %+v", wake)
	}
	if wake.PodReadyAt == nil || !wake.PodReadyAt.Equal(windowStart.Add(7*time.Minute)) {
		t.Errorf("podReadyAt = %v, want %s", wake.PodReadyAt, windowStart.Add(7*time.Minute))
	}
	// The woken role has taken no turn yet, so the resume carries no first turn at all.
	if wake.FirstTurnAt != nil {
		t.Errorf("firstTurnAt = %s, want none until the role's first turn", wake.FirstTurnAt)
	}
	if len(records.Holds) != 1 || records.Holds[0].Issue != "LEGION-209" || records.Holds[0].Reason != "background work" {
		t.Fatalf("holds = %+v, want the one hold of the window", records.Holds)
	}

	// A window that opens after every record is an empty window, three empty lists rather than
	// nulls: a reader of a quiet hour needs no special case.
	quiet := h.request(http.MethodPost, "/legion/v1/claims/release-records", ReleaseRecordsRequest{
		ClaimToken: token, Secret: registered.Secret, Since: windowStart.Add(time.Hour),
	}, nil)
	if quiet.Code != http.StatusOK {
		t.Fatalf("the later window = %d, want 200; body %s", quiet.Code, quiet.Body)
	}
	if body := quiet.Body.String(); body != "{\"releases\":[],\"resumes\":[],\"holds\":[]}\n" {
		t.Errorf("the later window answered %s, want three empty lists", body)
	}
}

// Nobody who cannot prove a claim of this daemon is told what it released: a wrong secret, a
// claim the daemon does not hold, a body naming no claim and a body naming no window are each
// refused as the sibling claim routes refuse one, and none of them is served a record.
func TestReleaseRecordsRefusesACallerThatCannotProveItsClaim(t *testing.T) {
	h := newHarness(t)
	token, bootToken := h.launch("LEGION-208", "implementer")
	registered := h.registered(bootToken, "ses_implementer")
	seedReleaseRecords(t, h)
	since := windowStart.Add(-time.Minute)

	for _, tc := range []struct {
		name   string
		body   any
		status int
	}{
		{
			name:   "a wrong secret",
			body:   ReleaseRecordsRequest{ClaimToken: token, Secret: "not-the-secret", Since: since},
			status: claim.InvalidSecret.Status,
		},
		{
			name: "a claim the daemon does not hold",
			body: ReleaseRecordsRequest{
				ClaimToken: "legion-legion-legion-999-implementer", Secret: registered.Secret, Since: since,
			},
			status: claim.InvalidSecret.Status,
		},
		{
			name:   "a body naming no claim",
			body:   ReleaseRecordsRequest{Secret: registered.Secret, Since: since},
			status: http.StatusBadRequest,
		},
		{
			name:   "a body naming no secret",
			body:   ReleaseRecordsRequest{ClaimToken: token, Since: since},
			status: http.StatusBadRequest,
		},
		{
			name:   "a body naming no window",
			body:   ReleaseRecordsRequest{ClaimToken: token, Secret: registered.Secret},
			status: http.StatusBadRequest,
		},
		{name: "no body at all", body: nil, status: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := h.request(http.MethodPost, "/legion/v1/claims/release-records", tc.body, nil)
			if recorder.Code != tc.status {
				t.Fatalf("%s = %d, want %d; body %s", tc.name, recorder.Code, tc.status, recorder.Body)
			}
			if body := recorder.Body.String(); strings.Contains(body, "LEGION-208") || strings.Contains(body, "releases") {
				t.Errorf("%s was told what the daemon released: %s", tc.name, body)
			}
		})
	}
}
