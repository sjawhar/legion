package api

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// releaseRecordsURL is the route as a pod calls it: the claim it holds, that claim's secret, and
// the start of the window it reads, all in the query string, since the route is a GET.
func releaseRecordsURL(token claim.Token, secret string, since string) string {
	query := url.Values{"claimToken": {string(token)}, "secret": {secret}, "since": {since}}
	return "/legion/v1/claims/release-records?" + query.Encode()
}

// INFRA-945 R6: a pod reads its project's idle-release records with the one credential it holds —
// its claim's — and gets back every release, resume and hold of the window it named.
func TestClaimReleaseRecordsServeAPodTheProjectsWindow(t *testing.T) {
	h := newHarness(t)
	token, bootToken := h.launch("LEGION-208", claim.RoleImplementer)
	registered := h.registered(bootToken, "ses_implementer")

	released := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	releaseID, err := h.store.RecordPodRelease(h.ctx, store.PodRelease{
		Project: testProject, Issue: "LEGION-208", Generation: 3,
		Roles: []string{"implementer", "tester"}, At: released, BundleBytes: 4096,
	})
	if err != nil {
		t.Fatalf("record the release: %v", err)
	}
	woke := released.Add(30 * time.Minute)
	resumeID, err := h.store.RecordPodWake(h.ctx, store.PodResume{
		Release: releaseID, Project: testProject, Issue: "LEGION-208", Role: "implementer",
		Kind: "delivery", At: woke,
	})
	if err != nil {
		t.Fatalf("record the wake: %v", err)
	}
	if err := h.store.MarkPodResumeReady(h.ctx, resumeID, woke.Add(time.Minute)); err != nil {
		t.Fatalf("stamp the resume ready: %v", err)
	}
	if err := h.store.RecordPodHold(h.ctx, store.PodHold{
		Project: testProject, Issue: "LEGION-209", Reason: "background_work", At: released.Add(time.Hour),
	}); err != nil {
		t.Fatalf("record the hold: %v", err)
	}
	// A release before the window the caller names is not in the answer.
	if _, err := h.store.RecordPodRelease(h.ctx, store.PodRelease{
		Project: testProject, Issue: "LEGION-207", At: released.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("record the older release: %v", err)
	}

	since := released.Add(-time.Minute).Format(time.RFC3339)
	recorder := h.request(http.MethodGet, releaseRecordsURL(token, registered.Secret, since), nil, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("release-records = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	var body ReleaseRecordsResponse
	decodeInto(t, recorder, &body)

	if len(body.Releases) != 1 {
		t.Fatalf("releases = %+v, want only the one inside the window", body.Releases)
	}
	release := body.Releases[0]
	if release.ID != releaseID || release.Issue != "LEGION-208" || release.Generation != 3 || release.BundleBytes != 4096 {
		t.Errorf("release = %+v, want LEGION-208 generation 3 with a 4096-byte bundle", release)
	}
	if len(release.Roles) != 2 || release.Roles[0] != "implementer" || release.Roles[1] != "tester" {
		t.Errorf("release roles = %v, want [implementer tester]", release.Roles)
	}
	if release.At != released.Format(time.RFC3339) {
		t.Errorf("release at = %q, want %q", release.At, released.Format(time.RFC3339))
	}

	if len(body.Resumes) != 1 {
		t.Fatalf("resumes = %+v, want the one wake", body.Resumes)
	}
	resume := body.Resumes[0]
	if resume.ID != resumeID || resume.Release != releaseID || resume.Role != "implementer" || resume.Kind != "delivery" {
		t.Errorf("resume = %+v, want the implementer's delivery wake of release %d", resume, releaseID)
	}
	if resume.At != woke.Format(time.RFC3339) || resume.PodReadyAt != woke.Add(time.Minute).Format(time.RFC3339) {
		t.Errorf("resume times = %q, %q", resume.At, resume.PodReadyAt)
	}
	// Nothing observed the first turn, so it is left out rather than served as a zero instant.
	if resume.FirstTurnAt != "" || resume.Stuck {
		t.Errorf("resume = %+v, want no first turn and no stuck mark", resume)
	}

	if len(body.Holds) != 1 || body.Holds[0].Issue != "LEGION-209" || body.Holds[0].Reason != "background_work" {
		t.Fatalf("holds = %+v, want LEGION-209 held for background work", body.Holds)
	}
}

// Nothing about the project's pods is told to a caller that cannot prove it holds a claim of it:
// no credential at all is the blank-field 400 every claim route answers, and a wrong secret, a
// claim the daemon does not hold and a claim no agent has registered on are all the same 403.
func TestClaimReleaseRecordsRefuseACallerThatCannotProveItsClaim(t *testing.T) {
	h := newHarness(t)
	token, bootToken := h.launch("LEGION-208", claim.RoleImplementer)
	registered := h.registered(bootToken, "ses_implementer")
	since := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)

	bare := h.request(http.MethodGet, "/legion/v1/claims/release-records", nil, nil)
	wantRefusal(t, bare, http.StatusBadRequest, "claimToken is required")

	noSecret := h.request(http.MethodGet, releaseRecordsURL(token, "", since), nil, nil)
	wantRefusal(t, noSecret, http.StatusBadRequest, "secret is required")

	wrongSecret := h.request(http.MethodGet, releaseRecordsURL(token, "not-the-secret", since), nil, nil)
	wantRefusal(t, wrongSecret, claim.InvalidSecret.Status, claim.InvalidSecret.Message)

	unknown := h.request(http.MethodGet,
		releaseRecordsURL("legion-legion-legion-999-implementer", registered.Secret, since), nil, nil)
	wantRefusal(t, unknown, claim.InvalidSecret.Status, claim.InvalidSecret.Message)
}

// The window is the caller's to name: one missing, and one the daemon cannot read as an instant,
// are each refused rather than answered with the whole table.
func TestClaimReleaseRecordsRefuseAnUnreadableSince(t *testing.T) {
	h := newHarness(t)
	token, bootToken := h.launch("LEGION-208", claim.RoleImplementer)
	registered := h.registered(bootToken, "ses_implementer")

	missing := h.request(http.MethodGet, releaseRecordsURL(token, registered.Secret, ""), nil, nil)
	wantRefusal(t, missing, http.StatusBadRequest, "since is required")

	unreadable := h.request(http.MethodGet, releaseRecordsURL(token, registered.Secret, "yesterday"), nil, nil)
	if unreadable.Code != http.StatusBadRequest {
		t.Fatalf("an unreadable since = %d, want 400; body %s", unreadable.Code, unreadable.Body)
	}
}
