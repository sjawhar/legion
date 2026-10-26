package store

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

const testerToken claim.Token = "legion-legion-legion-209-tester"

func testerConversation(session string, started time.Time) AgentConversation {
	return AgentConversation{
		Session: session, Project: "legion", Issue: "LEGION-209", Role: claim.RoleTester, Token: testerToken,
		Harness: runtime.BackendOmp, Location: "/state/sessions/" + session + ".jsonl", StartedAt: started, RecordedAt: started,
	}
}

// sameConversation compares two rows field by field, with the times compared as instants.
func sameConversation(t *testing.T, got, want AgentConversation) {
	t.Helper()
	if !got.StartedAt.Equal(want.StartedAt) || !got.RecordedAt.Equal(want.RecordedAt) {
		t.Errorf("conversation %s times = %v, %v; want %v, %v", want.Session, got.StartedAt, got.RecordedAt, want.StartedAt, want.RecordedAt)
	}
	got.StartedAt, got.RecordedAt, want.StartedAt, want.RecordedAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	if got != want {
		t.Errorf("conversation reads %+v, want %+v", got, want)
	}
}

// A registration records its conversation; the session registering again rewrites where it lives
// and keeps the first registration's start; a close that forgets the claim's session and a
// suspension that keeps it leave the row as it was.
func TestARegistrationRecordsItsConversationAndNothingAClaimDoesDeletesIt(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	c := tmuxClaim(testerToken)
	c.Role, c.Session = claim.RoleTester, "ses-tester"
	if err := s.PutClaim(ctx, c); err != nil {
		t.Fatal(err)
	}
	first := testerConversation("ses-tester", at(1))
	if _, err := s.RegisterConversation(ctx, first); err != nil {
		t.Fatal(err)
	}
	again := first
	again.Location, again.StartedAt, again.RecordedAt = "/state/sessions/moved.jsonl", at(9), at(9)
	written, err := s.RegisterConversation(ctx, again)
	if err != nil {
		t.Fatal(err)
	}
	want := again
	want.StartedAt, want.Source = at(1), ConversationRegistered
	sameConversation(t, written, want)
	got, ok, err := s.AgentConversation(ctx, "ses-tester")
	if err != nil || !ok {
		t.Fatalf("AgentConversation = %t, %v; want the row", ok, err)
	}
	sameConversation(t, got, want)

	suspended := c
	suspended.State, suspended.SuspendedBy, suspended.Locator = supervise.StateSuspended, supervise.SuspendedByDaemon, nil
	closed := suspended
	closed.State, closed.SuspendedBy, closed.Session, closed.SessionFile = supervise.StateRetired, "", "", ""
	for _, put := range []supervise.Claim{suspended, closed} {
		if err := s.PutClaim(ctx, put); err != nil {
			t.Fatal(err)
		}
		got, ok, err := s.AgentConversation(ctx, "ses-tester")
		if err != nil || !ok {
			t.Fatalf("AgentConversation after the claim went %s = %t, %v; want the row kept", put.State, ok, err)
		}
		sameConversation(t, got, want)
	}
	if _, ok, err := s.AgentConversation(ctx, "ses-unknown"); ok || err != nil {
		t.Errorf("AgentConversation of a session never recorded = %t, %v; want none", ok, err)
	}
}

// A scan records a session no row holds and never rewrites one: a registered row stays as the
// registration wrote it, and a scanned one as the first scan did. A registration of a scanned
// session makes it a registered one, started at the registration.
func TestAScanNeverRewritesARowAndARegistrationOutranksAScan(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	registered := testerConversation("ses-registered", at(5))
	if _, err := s.RegisterConversation(ctx, registered); err != nil {
		t.Fatal(err)
	}
	scanOfRegistered := testerConversation("ses-registered", at(2))
	scanOfRegistered.Location = "/elsewhere.jsonl"
	if recorded, err := s.ScanConversation(ctx, scanOfRegistered); err != nil || recorded {
		t.Fatalf("a scan of a registered session = %t, %v; want nothing recorded", recorded, err)
	}
	registered.Source = ConversationRegistered
	got, _, _ := s.AgentConversation(ctx, "ses-registered")
	sameConversation(t, got, registered)

	scanned := testerConversation("ses-scanned", at(3))
	if recorded, err := s.ScanConversation(ctx, scanned); err != nil || !recorded {
		t.Fatalf("a scan of an unrecorded session = %t, %v; want it recorded", recorded, err)
	}
	if recorded, err := s.ScanConversation(ctx, testerConversation("ses-scanned", at(4))); err != nil || recorded {
		t.Fatalf("a second scan = %t, %v; want nothing recorded", recorded, err)
	}
	scanned.Source = ConversationScanned
	got, _, _ = s.AgentConversation(ctx, "ses-scanned")
	sameConversation(t, got, scanned)

	resumed := testerConversation("ses-scanned", at(20))
	if _, err := s.RegisterConversation(ctx, resumed); err != nil {
		t.Fatal(err)
	}
	resumed.Source = ConversationRegistered
	got, _, _ = s.AgentConversation(ctx, "ses-scanned")
	sameConversation(t, got, resumed)
}

// A role's conversations read newest first, every registered one before every scanned one, so a
// conversation found by its title never reads as the role's newest over a registered one; another
// role's and another project's are not among them, and the project's whole list reads oldest
// first.
func TestRoleConversationsAreNewestFirstWithRegisteredBeforeScanned(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	for _, c := range []AgentConversation{testerConversation("old-registered", at(1)), testerConversation("new-registered", at(4))} {
		if _, err := s.RegisterConversation(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []AgentConversation{testerConversation("old-scanned", at(0)), testerConversation("late-scanned", at(7))} {
		if _, err := s.ScanConversation(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	other := testerConversation("reviewer", at(9))
	other.Role, other.Token = claim.RoleReviewer, "legion-legion-legion-209-reviewer"
	elsewhere := testerConversation("elsewhere", at(9))
	elsewhere.Project, elsewhere.Token = "widgets", "legion-widgets-legion-209-tester"
	for _, c := range []AgentConversation{other, elsewhere} {
		if _, err := s.RegisterConversation(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := s.RoleConversations(ctx, testerToken)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sessionsOf(rows), []string{"new-registered", "old-registered", "late-scanned", "old-scanned"}; !slices.Equal(got, want) {
		t.Errorf("RoleConversations = %v, want %v", got, want)
	}
	all, err := s.AgentConversations(ctx, "legion")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sessionsOf(all), []string{"old-scanned", "old-registered", "new-registered", "late-scanned", "reviewer"}; !slices.Equal(got, want) {
		t.Errorf("AgentConversations = %v, want %v", got, want)
	}
}

func sessionsOf(rows []AgentConversation) []string {
	var sessions []string
	for _, r := range rows {
		sessions = append(sessions, r.Session)
	}
	return sessions
}
