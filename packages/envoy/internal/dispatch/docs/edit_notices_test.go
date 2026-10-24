package docs

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// editNoticeRow is a document's pending edit notice as document_edit_notices holds it, with the
// database's own now() at the read beside its due time.
type editNoticeRow struct {
	from, to   int
	editors    []string
	lastEditor model.Actor
	dueAt, now time.Time
}

// readEditNotice reads artifactID's pending edit notice, and reports whether there is one.
func readEditNotice(t *testing.T, service *Service, artifactID string) (editNoticeRow, bool) {
	t.Helper()
	var row editNoticeRow
	var editors, lastEditor []byte
	err := service.store.Pool.QueryRow(context.Background(), `
		select from_version, to_version, editors, last_editor, due_at, now()
		from document_edit_notices where artifact_id = $1
	`, artifactID).Scan(&row.from, &row.to, &editors, &lastEditor, &row.dueAt, &row.now)
	if err != nil {
		var none int
		if countErr := service.store.Pool.QueryRow(context.Background(),
			`select count(*) from document_edit_notices where artifact_id = $1`, artifactID).Scan(&none); countErr == nil && none == 0 {
			return editNoticeRow{}, false
		}
		t.Fatalf("read the document's edit notice: %v", err)
	}
	if err := json.Unmarshal(editors, &row.editors); err != nil {
		t.Fatalf("decode the notice's editors: %v", err)
	}
	if err := json.Unmarshal(lastEditor, &row.lastEditor); err != nil {
		t.Fatalf("decode the notice's last editor: %v", err)
	}
	slices.Sort(row.editors)
	return row, true
}

// requireDueWithin requires the notice to be due quiet after the database's now() at the read,
// within slack: the settlement that wrote it set due_at to now() + quiet a moment before.
func requireDueWithin(t *testing.T, row editNoticeRow, quiet, slack time.Duration) {
	t.Helper()
	if gap := row.dueAt.Sub(row.now); gap > quiet || gap < quiet-slack {
		t.Fatalf("the notice is due %v after the read, want within %v of %v", gap, slack, quiet)
	}
}

// A person's settled browser edit opens the document's quiet window: one row, from the version
// before theirs to theirs, naming them, due a quiet time after the settlement. A second person's
// edit inside the window keeps where it starts, moves where it ends and when it is due, and names
// both people.
func TestEditNoticeAPersonsSettledEditOpensAndExtendsTheWindow(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	service.editNoticeQuiet = 30 * time.Second
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	if _, open := readEditNotice(t, service, artifactID); open {
		t.Fatal("the seed's settlement opened an edit notice, want none before a person edits")
	}
	before := latestVersionNumber(t, service, artifactID)

	alice := model.Actor{Kind: "user", ID: "alice@example.com"}
	editAsConnectedPeer(t, service, artifactID, 1, alice, replaceRun("First.", "First, edited."))
	settleCurrentGeneration(t, service, artifactID)
	first := latestVersionNumber(t, service, artifactID)
	if first != before+1 {
		t.Fatalf("alice's settlement wrote version %d, want %d", first, before+1)
	}
	row, open := readEditNotice(t, service, artifactID)
	if !open {
		t.Fatal("alice's settled edit opened no edit notice")
	}
	if row.from != first-1 || row.to != first || !slices.Equal(row.editors, []string{alice.ID}) || row.lastEditor != alice {
		t.Fatalf("notice after alice = from %d to %d editors %v last %+v, want from %d to %d naming alice", row.from, row.to, row.editors, row.lastEditor, first-1, first)
	}
	requireDueWithin(t, row, 30*time.Second, 2*time.Second)

	bob := model.Actor{Kind: "user", ID: "bob@example.com"}
	editAsConnectedPeer(t, service, artifactID, 2, bob, replaceRun("Second.", "Second, edited."))
	settleCurrentGeneration(t, service, artifactID)
	second := latestVersionNumber(t, service, artifactID)
	if second != first+1 {
		t.Fatalf("bob's settlement wrote version %d, want %d", second, first+1)
	}
	extended, open := readEditNotice(t, service, artifactID)
	if !open {
		t.Fatal("bob's settled edit left no edit notice")
	}
	if extended.from != first-1 || extended.to != second || !slices.Equal(extended.editors, []string{alice.ID, bob.ID}) || extended.lastEditor != bob {
		t.Fatalf("notice after bob = from %d to %d editors %v last %+v, want from %d to %d naming alice and bob, bob last", extended.from, extended.to, extended.editors, extended.lastEditor, first-1, second)
	}
	requireDueWithin(t, extended, 30*time.Second, 2*time.Second)
	if !extended.dueAt.After(row.dueAt) {
		t.Fatalf("bob's edit left the notice due at %v, want after alice's %v", extended.dueAt, row.dueAt)
	}
}

// One settlement that versions two people's edits names the one who edited last as the notice's
// last editor, whatever order their ids sort in: contract C4's actor is the person whose edit was
// last in the quiet window.
func TestEditNoticeNamesThePersonWhoEditedLast(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	zoe := model.Actor{Kind: "user", ID: "zoe@example.com"}
	adam := model.Actor{Kind: "user", ID: "adam@example.com"}
	editAsConnectedPeer(t, service, artifactID, 1, zoe, replaceRun("First.", "First, by zoe."))
	editAsConnectedPeer(t, service, artifactID, 2, adam, replaceRun("Second.", "Second, by adam."))
	settleCurrentGeneration(t, service, artifactID)
	row, open := readEditNotice(t, service, artifactID)
	if !open {
		t.Fatal("the two people's settled edits opened no edit notice")
	}
	if !slices.Equal(row.editors, []string{adam.ID, zoe.ID}) || row.lastEditor != adam {
		t.Fatalf("notice editors %v last %+v, want adam and zoe with adam, who edited last, last", row.editors, row.lastEditor)
	}
}

// A zero quiet time in the service's dependencies is the 30-second default.
func TestEditNoticeQuietDefaultsToThirtySeconds(t *testing.T) {
	service, _ := newTestService(t)
	if service.editNoticeQuiet != 30*time.Second {
		t.Fatalf("editNoticeQuiet = %v, want 30s", service.editNoticeQuiet)
	}
}

// An agent's settled edit opens no quiet window: the notice is for a person's edit alone.
func TestEditNoticeAnAgentsSettledEditOpensNone(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	before := latestVersionNumber(t, service, artifactID)
	session := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	editAsConnectedPeer(t, service, artifactID, 1, session, replaceRun("First.", "First, by the agent."))
	settleCurrentGeneration(t, service, artifactID)
	if latest := latestVersionNumber(t, service, artifactID); latest != before+1 {
		t.Fatalf("the agent's settlement wrote version %d, want %d", latest, before+1)
	}
	if _, open := readEditNotice(t, service, artifactID); open {
		t.Fatal("an agent's settled edit opened an edit notice, want none")
	}
}

// A named version, which already wakes the document's watchers itself, opens no quiet window, even
// when a person names it over their own browser edit.
func TestEditNoticeANamedVersionOpensNone(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	alice := model.Actor{Kind: "user", ID: "alice@example.com"}
	editAsConnectedPeer(t, service, artifactID, 1, alice, replaceRun("First.", "First, edited."))
	waitForLandedAppends(t, service, artifactID)
	before := latestVersionNumber(t, service, artifactID)
	commitNamedVersion(t, service, artifactID, alice)
	if latest := latestVersionNumber(t, service, artifactID); latest != before+1 {
		t.Fatalf("the named version is %d, want %d", latest, before+1)
	}
	if _, open := readEditNotice(t, service, artifactID); open {
		t.Fatal("a named version opened an edit notice, want none")
	}
}
