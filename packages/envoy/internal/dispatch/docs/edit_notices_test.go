package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
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

// A named version a person names over their own browser edit opens the notice window, since the
// version lists that person. A named version that lists no person opens none.
func TestEditNoticeANamedVersionByAPersonOverTheirOwnEditOpensTheWindow(t *testing.T) {
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
	row, open := readEditNotice(t, service, artifactID)
	if !open {
		t.Fatal("a person's named version over their own edit opened no notice")
	}
	if !slices.Equal(row.editors, []string{alice.ID}) {
		t.Fatalf("notice editors %v, want alice alone", row.editors)
	}
}

// A named version by an agent, with no person's credit pending, opens no window.
func TestEditNoticeAnAgentOnlyNamedVersionOpensNone(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	session := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{
		{Op: "replace", Find: "First.", With: "First, by the agent."},
	}, session, nil); err != nil {
		t.Fatalf("agent edit: %v", err)
	}
	waitForLandedAppends(t, service, artifactID)
	commitNamedVersion(t, service, artifactID, session)
	if _, open := readEditNotice(t, service, artifactID); open {
		t.Fatal("an agent-only named version opened an edit notice, want none")
	}
}

// A person's browser edit that an agent's named version captures, before the settlement writes
// its own version, still opens the edit notice: every version write that carries a person opens or
// extends the window (writeVersionTx), so the settlement is not the only path.
func TestEditNoticeAnAgentsNamedVersionThatCapturesAPersonsEditOpensTheWindow(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	alice := model.Actor{Kind: "user", ID: "alice@example.com"}
	editAsConnectedPeer(t, service, artifactID, 1, alice, replaceRun("First.", "First, edited."))
	waitForLandedAppends(t, service, artifactID)
	before := latestVersionNumber(t, service, artifactID)
	session := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	commitNamedVersion(t, service, artifactID, session)
	named := latestVersionNumber(t, service, artifactID)
	if named != before+1 {
		t.Fatalf("named version = %d, want %d", named, before+1)
	}
	if authors := latestVersionAuthors(t, service, artifactID); !slices.Contains(authors, alice) {
		t.Fatalf("the named version lists %+v, want alice among them", authors)
	}
	row, open := readEditNotice(t, service, artifactID)
	if !open {
		t.Fatal("the named version that captured alice's edit opened no edit notice")
	}
	if row.from != named-1 || row.to != named || !slices.Equal(row.editors, []string{alice.ID}) || row.lastEditor != alice {
		t.Fatalf("notice = from %d to %d editors %v last %+v, want from %d to %d naming alice alone", row.from, row.to, row.editors, row.lastEditor, named-1, named)
	}
}

// A snapshot version that carries a person's edit opens the same window.
func TestEditNoticeASnapshotVersionThatCapturesAPersonsEditOpensTheWindow(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	alice := model.Actor{Kind: "user", ID: "alice@example.com"}
	editAsConnectedPeer(t, service, artifactID, 1, alice, replaceRun("First.", "First, edited."))
	waitForLandedAppends(t, service, artifactID)
	before := latestVersionNumber(t, service, artifactID)
	snapshotAndCommitVersion(t, service, artifactID, model.Actor{Kind: "session", ID: "session-0123456789abcdef"})
	snapshot := latestVersionNumber(t, service, artifactID)
	if snapshot != before+1 {
		t.Fatalf("snapshot version = %d, want %d", snapshot, before+1)
	}
	row, open := readEditNotice(t, service, artifactID)
	if !open {
		t.Fatal("the snapshot version that captured alice's edit opened no edit notice")
	}
	if row.from != snapshot-1 || row.to != snapshot || !slices.Equal(row.editors, []string{alice.ID}) {
		t.Fatalf("notice = from %d to %d editors %v, want from %d to %d naming alice", row.from, row.to, row.editors, snapshot-1, snapshot)
	}
}

// humanEdit is one artifact.human_edited event as the event log stores it.
type humanEdit struct {
	issueKey   *string
	artifactID *string
	actor      model.Actor
	notify     bool
	payload    model.ArtifactHumanEditedEventPayload
}

// humanEdits reads every artifact.human_edited event about document, oldest first.
func humanEdits(t *testing.T, database *store.Store, document string) []humanEdit {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select issue_key, artifact_id::text, actor, notify, payload from events
		where type = 'artifact.human_edited' and payload->>'artifact_id' = $1
		order by id
	`, document)
	if err != nil {
		t.Fatalf("read the document's edit notices: %v", err)
	}
	defer rows.Close()
	var edits []humanEdit
	for rows.Next() {
		var edit humanEdit
		var actor, payload []byte
		if err := rows.Scan(&edit.issueKey, &edit.artifactID, &actor, &edit.notify, &payload); err != nil {
			t.Fatalf("scan an edit notice: %v", err)
		}
		if err := json.Unmarshal(actor, &edit.actor); err != nil {
			t.Fatalf("decode an edit notice's actor: %v", err)
		}
		if err := json.Unmarshal(payload, &edit.payload); err != nil {
			t.Fatalf("decode an edit notice's payload: %v", err)
		}
		edits = append(edits, edit)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the document's edit notices: %v", err)
	}
	return edits
}

// makeEditNoticeDue moves document's pending notice into the past, as the quiet time passing does.
func makeEditNoticeDue(t *testing.T, database *store.Store, document string) {
	t.Helper()
	tag, err := database.Pool.Exec(context.Background(),
		`update document_edit_notices set due_at = now() - interval '1 second' where artifact_id = $1`, document)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("make the document's notice due: %v (rows %d)", err, tag.RowsAffected())
	}
}

// versionMarkdown is the stored markdown of version number of document.
func versionMarkdown(t *testing.T, database *store.Store, document string, number int) string {
	t.Helper()
	var markdown string
	if err := database.Pool.QueryRow(context.Background(),
		`select coalesce(markdown, '') from artifact_versions where artifact_id = $1 and number = $2`, document, number,
	).Scan(&markdown); err != nil {
		t.Fatalf("read version %d: %v", number, err)
	}
	return markdown
}

// addPersonsVersion stores markdown as document's version number, written by person, and opens a
// due notice for it from the version before, as a person's settled edit and the quiet time passing
// leave the document.
func addPersonsVersion(t *testing.T, database *store.Store, document string, number int, markdown string, person model.Actor) {
	t.Helper()
	ctx := context.Background()
	authors, err := json.Marshal([]model.Actor{person})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors) values ($1, $2, $3, $4)
	`, document, number, markdown, authors); err != nil {
		t.Fatalf("store the person's version: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into document_edit_notices (artifact_id, from_version, to_version, editors, last_editor, due_at)
		values ($1, $2 - 1, $2, jsonb_build_array($3::text), $4, now() - interval '1 second')
	`, document, number, person.ID, authors[1:len(authors)-1]); err != nil {
		t.Fatalf("open the person's due notice: %v", err)
	}
}

// A due notice becomes one artifact.human_edited event, owned by the document's issue as its
// version events are, naming the person who edited last, notifying, and carrying the two versions'
// diff; the row goes, and the event is published once its transaction commits.
func TestEditNoticeSweepSendsADueNotice(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	alice := model.Actor{Kind: "user", ID: "alice@example.com"}
	editAsConnectedPeer(t, service, artifactID, 1, alice, replaceRun("First.", "First, edited."))
	settleCurrentGeneration(t, service, artifactID)
	edited := latestVersionNumber(t, service, artifactID)
	makeEditNoticeDue(t, service.store, artifactID)
	published, unsubscribe := service.events.Subscribe()
	defer unsubscribe()

	sent, err := service.sendDueEditNotices(context.Background())
	if err != nil || sent != 1 {
		t.Fatalf("sweep = %d sent, %v; want 1 sent", sent, err)
	}
	edits := humanEdits(t, service.store, artifactID)
	if len(edits) != 1 {
		t.Fatalf("edit notices = %d, want 1", len(edits))
	}
	edit := edits[0]
	wantDiff, wantCut := text.UnifiedDiffCut(versionMarkdown(t, service.store, artifactID, edited-1), versionMarkdown(t, service.store, artifactID, edited), 4096)
	want := model.ArtifactHumanEditedEventPayload{
		ArtifactID: artifactID, Name: "document.md", FromVersion: edited - 1, ToVersion: edited,
		Editors: []string{alice.ID}, Diff: wantDiff, DiffTruncated: wantCut,
	}
	if edit.issueKey == nil || *edit.issueKey != "DOC-1" || edit.artifactID != nil || !edit.actor.SameAs(alice) || !edit.notify ||
		fmt.Sprint(edit.payload) != fmt.Sprint(want) {
		t.Fatalf("edit notice = issue %v artifact %v actor %+v notify %t payload %+v; want DOC-1's, by alice, notifying, payload %+v",
			edit.issueKey, edit.artifactID, edit.actor, edit.notify, edit.payload, want)
	}
	if !strings.Contains(edit.payload.Diff, "-First.") || !strings.Contains(edit.payload.Diff, "+First, edited.") {
		t.Fatalf("the notice's diff does not hold alice's change:\n%s", edit.payload.Diff)
	}
	if _, open := readEditNotice(t, service, artifactID); open {
		t.Fatal("the sent notice's row is still there")
	}
	select {
	case event := <-published:
		if event.Type != "artifact.human_edited" {
			t.Fatalf("published %q, want artifact.human_edited", event.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sent notice was never published")
	}
}

// A project document's notice is owned by the document itself, as its version events are.
func TestEditNoticeSweepOwnsAProjectDocumentsNoticeByTheDocument(t *testing.T) {
	service, _ := newTestService(t)
	document := createProjectDocument(t, service.store, "First.")
	addPersonsVersion(t, service.store, document, 2, "Second.\n", model.Actor{Kind: "user", ID: "alice@example.com"})
	if sent, err := service.sendDueEditNotices(context.Background()); err != nil || sent != 1 {
		t.Fatalf("sweep = %d sent, %v; want 1 sent", sent, err)
	}
	edits := humanEdits(t, service.store, document)
	if len(edits) != 1 || edits[0].issueKey != nil || edits[0].artifactID == nil || *edits[0].artifactID != document {
		t.Fatalf("edit notices = %+v, want one owned by the project document", edits)
	}
}

// A notice whose quiet time has not passed stays, and sends nothing.
func TestEditNoticeSweepLeavesANoticeThatIsNotDue(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	editAsConnectedPeer(t, service, artifactID, 1, model.Actor{Kind: "user", ID: "alice@example.com"}, replaceRun("First.", "First, edited."))
	settleCurrentGeneration(t, service, artifactID)
	if sent, err := service.sendDueEditNotices(context.Background()); err != nil || sent != 0 {
		t.Fatalf("sweep = %d sent, %v; want none sent", sent, err)
	}
	if edits := humanEdits(t, service.store, artifactID); len(edits) != 0 {
		t.Fatalf("edit notices = %d, want none", len(edits))
	}
	if _, open := readEditNotice(t, service, artifactID); !open {
		t.Fatal("the notice that is not due is gone")
	}
}

// A diff longer than 4,096 bytes is cut at its last line end at or under 4,096 bytes, and the
// notice says it was cut.
func TestEditNoticeSweepCutsALongDiff(t *testing.T) {
	service, _ := newTestService(t)
	before := strings.Repeat("The export runs at 02:00 UTC on every weekday.\n\n", 120)
	after := strings.Repeat("The export runs at 04:00 UTC on every weekday.\n\n", 120)
	document := createIssueDocument(t, service.store, 2, before)
	addPersonsVersion(t, service.store, document, 2, after, model.Actor{Kind: "user", ID: "alice@example.com"})
	if sent, err := service.sendDueEditNotices(context.Background()); err != nil || sent != 1 {
		t.Fatalf("sweep = %d sent, %v; want 1 sent", sent, err)
	}
	edits := humanEdits(t, service.store, document)
	if len(edits) != 1 {
		t.Fatalf("edit notices = %d, want 1", len(edits))
	}
	diff := edits[0].payload.Diff
	whole, _ := text.UnifiedDiffCut(versionMarkdown(t, service.store, document, 1), after, 1<<30)
	if !edits[0].payload.DiffTruncated || len(diff) > 4096 || len(whole) <= 4096 || !strings.HasSuffix(diff, "\n") || !strings.HasPrefix(whole, diff) {
		t.Fatalf("diff of %d bytes (truncated %t) from a whole diff of %d; want a cut, line-ended prefix within 4,096 bytes",
			len(diff), edits[0].payload.DiffTruncated, len(whole))
	}
	if next := strings.IndexByte(whole[len(diff):], '\n'); len(diff)+next+1 <= 4096 {
		t.Fatalf("the cut at %d bytes leaves out a line that fits", len(diff))
	}
}

// A version a rebuild removed gives the notice an empty diff, and the notice still goes.
func TestEditNoticeSweepSendsAnEmptyDiffForAMissingVersion(t *testing.T) {
	service, _ := newTestService(t)
	document := createIssueDocument(t, service.store, 3, "First.")
	if _, err := service.store.Pool.Exec(context.Background(), `
		insert into document_edit_notices (artifact_id, from_version, to_version, editors, last_editor, due_at)
		values ($1, 1, 9, '["alice@example.com"]', '{"kind":"user","id":"alice@example.com"}', now() - interval '1 second')
	`, document); err != nil {
		t.Fatalf("open a notice naming a version that is not stored: %v", err)
	}
	if sent, err := service.sendDueEditNotices(context.Background()); err != nil || sent != 1 {
		t.Fatalf("sweep = %d sent, %v; want 1 sent", sent, err)
	}
	if edits := humanEdits(t, service.store, document); len(edits) != 1 || edits[0].payload.Diff != "" || edits[0].payload.DiffTruncated {
		t.Fatalf("edit notices = %+v, want one with an empty diff", edits)
	}
}

// A notice on a document whose issue closed inside the quiet window goes with no event: nobody
// can write the closed issue's spec, so its sessions have nothing to change.
func TestEditNoticeSweepDropsANoticeOnAClosedIssue(t *testing.T) {
	service, _ := newTestService(t)
	document := createIssueDocument(t, service.store, 4, "First.")
	addPersonsVersion(t, service.store, document, 2, "Second.\n", model.Actor{Kind: "user", ID: "alice@example.com"})
	if _, err := service.store.Pool.Exec(context.Background(), `update issues set closed_at = now() where key = 'DOC-4'`); err != nil {
		t.Fatalf("close the issue: %v", err)
	}
	if sent, err := service.sendDueEditNotices(context.Background()); err != nil || sent != 0 {
		t.Fatalf("sweep = %d sent, %v; want none sent", sent, err)
	}
	if edits := humanEdits(t, service.store, document); len(edits) != 0 {
		t.Fatalf("edit notices = %d, want none on a closed issue", len(edits))
	}
	var left int
	if err := service.store.Pool.QueryRow(context.Background(), `select count(*) from document_edit_notices where artifact_id = $1`, document).Scan(&left); err != nil || left != 0 {
		t.Fatalf("notice rows left = %d (%v), want the closed issue's notice gone", left, err)
	}
}

// Two envoy-dispatch processes sweeping one database at the same moment, each on a pool of its
// own, send each due notice once: the row is claimed by its delete, under the document owner's
// lock, so the second sweep to reach it finds nothing to delete.
func TestEditNoticeSweepOfTwoServicesSendsEachNoticeOnce(t *testing.T) {
	database := storetest.Open(t)
	first := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	second := New(Deps{Store: storetest.SecondProcess(t, database), Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() {
		_ = first.Shutdown(context.Background())
		_ = second.Shutdown(context.Background())
	})
	const documents = 6
	ids := make([]string, documents)
	for index := range ids {
		ids[index] = createIssueDocument(t, database, index+1, fmt.Sprintf("Document %d.", index))
		addPersonsVersion(t, database, ids[index], 2, fmt.Sprintf("Document %d, edited.\n", index), model.Actor{Kind: "user", ID: "alice@example.com"})
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	totals := make([]int, 2)
	for index, service := range []*Service{first, second} {
		wg.Go(func() {
			<-start
			for range 3 {
				sent, err := service.sendDueEditNotices(store.WithTransactionTracking(context.Background()))
				if err != nil {
					t.Errorf("sweep: %v", err)
				}
				totals[index] += sent
			}
		})
	}
	close(start)
	wg.Wait()
	if totals[0]+totals[1] != documents {
		t.Fatalf("the two sweeps sent %d and %d notices, want %d between them", totals[0], totals[1], documents)
	}
	for _, document := range ids {
		if edits := humanEdits(t, database, document); len(edits) != 1 {
			t.Fatalf("document %s has %d edit notices, want 1", document, len(edits))
		}
	}
}

// A sweep that meets a person's settlement of the same document takes the locks in the
// settlement's order - the document owner's, then the notice row - so the two never deadlock. The
// settlement is held after its version write, holding the owner's lock, while a sweep starts on
// the document's due notice; once the settlement goes on, it moves the notice on and commits, and
// the sweep finds nothing due. Once the quiet time passes, one notice covers both people's
// versions, so each person's version is in exactly one sent notice's range.
func TestEditNoticeSweepDuringSettlement(t *testing.T) {
	logs := &storetest.LockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	service.editNoticeQuiet = 500 * time.Millisecond
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	alice := model.Actor{Kind: "user", ID: "alice@example.com"}
	editAsConnectedPeer(t, service, artifactID, 1, alice, replaceRun("First.", "First, edited."))
	settleCurrentGeneration(t, service, artifactID)
	aliceVersion := latestVersionNumber(t, service, artifactID)
	makeEditNoticeDue(t, service.store, artifactID)

	bob := model.Actor{Kind: "user", ID: "bob@example.com"}
	editAsConnectedPeer(t, service, artifactID, 2, bob, replaceRun("Second.", "Second, edited."))
	type sweepResult struct {
		sent int
		err  error
	}
	swept := make(chan sweepResult, 1)
	held := false
	service.afterSettleVersionWrite = func(room string) error {
		if room != artifactID || held {
			return nil
		}
		held = true
		go func() {
			sent, err := service.sendDueEditNotices(store.WithTransactionTracking(context.Background()))
			swept <- sweepResult{sent, err}
		}()
		deadline := time.Now().Add(10 * time.Second)
		for {
			waiting, err := storetest.CountLockWaits(context.Background(), service.store, "%from issues where key = $1 for no key update%")
			if err != nil {
				t.Errorf("count the sweep's lock waits: %v", err)
				return nil
			}
			if waiting > 0 {
				return nil
			}
			if time.Now().After(deadline) {
				t.Error("the sweep never waited on the document owner's lock the settlement holds")
				return nil
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	settleCurrentGeneration(t, service, artifactID)
	service.afterSettleVersionWrite = nil
	if !held {
		t.Fatal("bob's settlement never reached the window after its version write")
	}
	bobVersion := latestVersionNumber(t, service, artifactID)
	if bobVersion != aliceVersion+1 {
		t.Fatalf("bob's settlement wrote version %d, want %d", bobVersion, aliceVersion+1)
	}
	select {
	case result := <-swept:
		if result.err != nil || result.sent != 0 {
			t.Fatalf("the sweep that met the settlement = %d sent, %v; want none sent, since the settlement moved the notice on", result.sent, result.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the sweep that met the settlement never finished")
	}

	deadline := time.Now().Add(10 * time.Second)
	for len(humanEdits(t, service.store, artifactID)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no notice went once the quiet time passed")
		}
		if _, err := service.sendDueEditNotices(store.WithTransactionTracking(context.Background())); err != nil {
			t.Fatalf("sweep after the quiet time: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	edits := humanEdits(t, service.store, artifactID)
	for _, version := range []int{aliceVersion, bobVersion} {
		covering := 0
		for _, edit := range edits {
			if edit.payload.FromVersion < version && version <= edit.payload.ToVersion {
				covering++
			}
		}
		if covering != 1 {
			t.Errorf("version %d is in %d sent notices' ranges, want exactly 1: %+v", version, covering, edits)
		}
	}
	if !slices.Equal(edits[0].payload.Editors, []string{alice.ID, bob.ID}) || !edits[0].actor.SameAs(bob) {
		t.Errorf("the notice names %v, by %+v; want alice and bob, by bob", edits[0].payload.Editors, edits[0].actor)
	}
	for _, failure := range []string{"40P01", "deadlock detected"} {
		if strings.Contains(logs.String(), failure) {
			t.Fatalf("the settlement and the sweep logged %q:\n%s", failure, logs.String())
		}
	}
}

// A document deleted after the sweep read its id, which took its notice row with it, is skipped:
// the sweep does not fail, and the next due notice of the same sweep still goes.
func TestEditNoticeSweepSkipsDeletedArtifact(t *testing.T) {
	logs := &storetest.LockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	service, _ := newTestService(t)
	gone := createIssueDocument(t, service.store, 5, "Gone.")
	kept := createIssueDocument(t, service.store, 6, "Kept.")
	alice := model.Actor{Kind: "user", ID: "alice@example.com"}
	addPersonsVersion(t, service.store, gone, 2, "Gone, edited.\n", alice)
	addPersonsVersion(t, service.store, kept, 2, "Kept, edited.\n", alice)
	if _, err := service.store.Pool.Exec(context.Background(), `
		update document_edit_notices set due_at = due_at - interval '1 minute' where artifact_id = $1
	`, gone); err != nil {
		t.Fatalf("make the deleted document's notice the first due: %v", err)
	}
	var read []string
	service.afterEditNoticeIDs = func(ids []string) {
		read = slices.Clone(ids)
		ctx := context.Background()
		for _, table := range []string{"doc_snapshots", "doc_updates", "doc_checkpoints", "artifact_versions", "events"} {
			if _, err := service.store.Pool.Exec(ctx, "delete from "+table+" where artifact_id = $1", gone); err != nil {
				t.Errorf("delete %s of the document: %v", table, err)
			}
		}
		if _, err := service.store.Pool.Exec(ctx, `delete from artifacts where id = $1`, gone); err != nil {
			t.Errorf("delete the document: %v", err)
		}
	}
	sent, err := service.sendDueEditNotices(context.Background())
	if err != nil || sent != 1 {
		t.Fatalf("sweep = %d sent, %v; want the kept document's notice sent:\n%s", sent, err, logs.String())
	}
	if !slices.Equal(read, []string{gone, kept}) {
		t.Fatalf("the sweep read the due ids %v, want the deleted document's first, then the kept one's", read)
	}
	if edits := humanEdits(t, service.store, kept); len(edits) != 1 {
		t.Fatalf("the kept document's edit notices = %d, want 1", len(edits))
	}
	if edits := humanEdits(t, service.store, gone); len(edits) != 0 {
		t.Fatalf("the deleted document's edit notices = %d, want none", len(edits))
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("the sweep logged an error for a deleted document:\n%s", logs.String())
	}
}
