package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// defaultEditNoticeQuiet is how long a document stays quiet after a person's last settled version
// before its edit notice goes, when Deps.EditNoticeQuiet names none.
const defaultEditNoticeQuiet = 30 * time.Second

// openEditNotice opens a document's quiet window at a person's settled version, or moves on the
// one already open: from_version stays the version before the window's first person's version,
// to_version becomes this one, the editors gain this version's people, and the window ends a
// quiet time from the moment this statement runs. That moment is clock_timestamp(), never now():
// now() is the transaction's start, before the owner and advisory locks a busy document makes the
// writer wait on, so a window counted from it could already be over when the notice commits.
// It and holdEditNotice are the settlement side's two writers of the table, and the sweep's
// delete (dueEditNotice) is the third.
const openEditNotice = `
	insert into document_edit_notices (artifact_id, from_version, to_version, editors, last_editor, due_at)
	values ($1, $2 - 1, $2, $3, $4, clock_timestamp() + $5::interval)
	on conflict (artifact_id) do update set
		to_version = excluded.to_version,
		editors = (select coalesce(jsonb_agg(distinct e), '[]'::jsonb)
			from jsonb_array_elements(document_edit_notices.editors || excluded.editors) e),
		last_editor = excluded.last_editor,
		due_at = excluded.due_at`

// holdEditNotice records a person's version whose latest edit has no known person sender in the
// document's window, without naming anyone: a new row starts at the version before this one with
// no last editor and no due time, which the sweep never sends; an open window keeps its last
// editor, gains this version and its people, and when it names someone it moves its due time as
// any later person's version does; a held row stays held. The next version that names a person
// (openEditNotice) keeps the row's from_version, so its notice covers this version too. A held
// row is the one row the sweep never claims, so a document whose issue closes while it is held,
// or that never gets another named version, keeps it until the document is deleted, which takes
// it (on delete cascade). It is one row per document and nothing reads it but the next version.
const holdEditNotice = `
	insert into document_edit_notices (artifact_id, from_version, to_version, editors, last_editor, due_at)
	values ($1, $2 - 1, $2, $3, null, null)
	on conflict (artifact_id) do update set
		to_version = excluded.to_version,
		editors = (select coalesce(jsonb_agg(distinct e), '[]'::jsonb)
			from jsonb_array_elements(document_edit_notices.editors || excluded.editors) e),
		due_at = case when document_edit_notices.last_editor is null then null
			else clock_timestamp() + $4::interval end`

// recordEditNotice opens or moves on artifactID's edit notice for a version just written, when
// that version lists a person: a person's browser edit is what the notice tells the document's
// sessions about. A version that lists no person writes nothing.
//
// Every writer of a version that can list a person calls it in its own transaction, after it took
// the document owner's lock (lockArtifactOwner) and wrote the version, so the row's lock comes
// second; the sweep (sendEditNotice) takes the two in the same order. Those writers are the
// settlement, NamedVersion, SnapshotVersion and a person's rebuild (RebuildDocument): a person's
// edit an agent's version captures before the settlement runs is in that version, and the
// settlement then has nothing of it to version. An upload writes its version through no room and
// lists its uploader alone; it opens no window, and the plan leaves it out on purpose.
//
// The notice's last editor is latest, the sender of the document's latest edit as that version's
// own capture read it (authorCapture.latest, latestEditSource), never a name chosen out of the
// set of people the version credits. That set is what a browser edit leaves when several peers
// were connected: ygo does not say which of them sent the update, so the room credits them all
// and records no sender. A version whose latest edit has no known person sender, or whose sender
// is not a person this version credits, names nobody, and no later notice leaves it out
// (holdEditNotice): in an open window it moves the window on and keeps the window's last editor,
// the last person the window knows edited; with no window open it starts one that names nobody
// and is not due until a later version names a person, whose notice then covers it. While no
// later version names a person, no notice goes for it. A two-peer edit is what reaches this
// branch: an agent's edit after a person's leaves the document's latest person sender as it was
// (latestPersonOf), so a later version still names that person.
func (s *Service) recordEditNotice(ctx context.Context, tx pgx.Tx, artifactID string, version int, authors []model.Actor, latest *model.Actor) error {
	var editors []string
	for index := range authors {
		if authors[index].Kind == "user" {
			editors = append(editors, authors[index].ID)
		}
	}
	if len(editors) == 0 {
		return nil
	}
	editorsJSON, err := json.Marshal(editors)
	if err != nil {
		return fmt.Errorf("encode edit notice editors: %w", err)
	}
	if latest == nil || latest.Kind != "user" || !slices.Contains(editors, latest.ID) {
		slog.Info("dispatch: a version names no latest editor among the people it lists, so its edit notice names nobody new",
			"artifact", artifactID, "version", version, "editors", editors, "latest", latest)
		return writeExec(ctx, tx, "hold edit notice", holdEditNotice, artifactID, version, editorsJSON, s.editNoticeQuiet)
	}
	lastJSON, err := json.Marshal(*latest)
	if err != nil {
		return fmt.Errorf("encode edit notice last editor: %w", err)
	}
	return writeExec(ctx, tx, "open edit notice", openEditNotice, artifactID, version, editorsJSON, lastJSON, s.editNoticeQuiet)
}

// editNoticeLease names the lease one sweep runs under, editNoticeInterval the sweep's period and
// the wait after each sweep before the next, editNoticeBatch the most due notices one sweep sends,
// and editNoticeDiffBytes the most a notice's diff holds. A notice is due at most
// editNoticeInterval, plus the time the sweep before took, before a sweep sends it, so a person's
// edit reaches its sessions within the quiet time plus five seconds and that sweep's time.
const (
	editNoticeLease     = "edit-notices"
	editNoticeInterval  = 5 * time.Second
	editNoticeBatch     = 20
	editNoticeDiffBytes = 4096
)

// dueEditNotices lists the documents whose quiet window has ended, oldest first. It is read with no
// lock: each id is read again and claimed on its own, under its owner's lock, by dueEditNotice and
// claimEditNotice.
const dueEditNotices = `
	select artifact_id::text from document_edit_notices
	where due_at <= now() order by due_at limit $1`

// dueEditNotice reads one due notice under its document owner's lock, which every writer of the
// row holds - the version writers (recordEditNotice) and the sweep - so the notice it reads is the
// one the claim deletes. A settlement that moved the window on since the unlocked read leaves it not
// due, and another process's sweep that sent it leaves no row: in both it reads nothing and this
// sweep sends nothing. It writes nothing, so the sweep does its document work on the notice before
// its transaction holds a transaction id.
const dueEditNotice = `
	select from_version, to_version, editors, last_editor from document_edit_notices
	where artifact_id = $1 and due_at <= now()`

// claimEditNotice claims the due notice dueEditNotice read by deleting it, the sweep's first
// write; what it returns is checked against what was read.
const claimEditNotice = `
	delete from document_edit_notices where artifact_id = $1 and due_at <= now()
	returning from_version, to_version`

// RunEditNotices sends each document's edit notice once its quiet window ends: one
// artifact.human_edited event, which reaches the sessions the document credits and its issue's
// route (outbox.publishAuthorRoutes). It sweeps at start and then editNoticeInterval after each
// sweep returns (duty.RunLeasedAfterEachPass) until ctx ends, under the edit-notices lease, so one
// process sweeps a period however many serve the database. The window is a row, not a timer, so a
// notice still pending when a process stops is sent by the next sweep of any process.
func (s *Service) RunEditNotices(ctx context.Context, leases duty.Leases) {
	ctx = store.WithTransactionTracking(ctx)
	duty.RunLeasedAfterEachPass(ctx, leases, editNoticeLease, editNoticeInterval, func(ctx context.Context) time.Duration {
		sent, err := s.sendDueEditNotices(ctx)
		if err != nil && ctx.Err() == nil {
			slog.Error("dispatch: send edit notices", "error", err)
		}
		if sent > 0 {
			slog.Info("dispatch: sent edit notices", "notices", sent)
		}
		return editNoticeInterval
	})
}

// sendDueEditNotices sends up to editNoticeBatch due notices, each in a transaction of its own, and
// reports how many it sent. A notice it cannot send is logged and left due for the next sweep, so
// one document never holds back the others; err is the read of the due ids alone.
func (s *Service) sendDueEditNotices(ctx context.Context) (int, error) {
	ctx = store.WithTransactionTracking(ctx)
	rows, err := s.store.Pool.Query(ctx, dueEditNotices, editNoticeBatch)
	if err != nil {
		return 0, fmt.Errorf("list due edit notices: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("list due edit notices: %w", err)
	}
	if s.afterEditNoticeIDs != nil {
		s.afterEditNoticeIDs(ids)
	}
	sent := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			return sent, nil
		}
		ok, err := s.sendEditNotice(ctx, id)
		if err != nil {
			slog.Error("dispatch: send an edit notice", "artifact", id, "error", err)
			continue
		}
		if ok {
			sent++
		}
	}
	return sent, nil
}

// errEditNoticeUnclaimed is a claim of a due notice that did not delete the notice the sweep read
// (sendEditNotice): nothing is sent for it, and the sweep reports no failure.
var errEditNoticeUnclaimed = errors.New("the due edit notice is no longer the one read")

// ignoreUnclaimed is err, nil when it is errEditNoticeUnclaimed.
func ignoreUnclaimed(err error) error {
	if errors.Is(err, errEditNoticeUnclaimed) {
		return nil
	}
	return err
}

// sendEditNotice claims artifactID's due notice and appends its artifact.human_edited event in one
// transaction, then publishes the event once it commits, and reports whether it sent one. It takes
// the document owner's lock (lockArtifactOwner), under which it reads the notice, both versions it
// names, tells a server-state-only change from a person's edit and cuts the diff, all before its
// first write; then it claims the notice (claimEditNotice) and appends the event
// (events.Broker.QueueAppend, which takes the owner's lock again and the owner's row) in one
// batch, so the transaction holds a transaction id only for those and the commit. The claim still
// decides: a notice it does not delete as read is not sent, which the claim's callback reports
// (errEditNoticeUnclaimed), stopping the batch before the append is read, and the transaction
// rolls back. No statement here locks `for update`.
//
// A document deleted since the unlocked read has taken its notice with it (on delete cascade), and
// one whose issue has closed takes no more edits: either way nothing is sent, and a closed issue's
// notice is dropped.
func (s *Service) sendEditNotice(ctx context.Context, artifactID string) (bool, error) {
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin edit notice transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	owner, open, err := lockArtifactOwner(ctx, tx, artifactID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var payload model.ArtifactHumanEditedEventPayload
	var editors, lastEditor []byte
	err = tx.QueryRow(ctx, dueEditNotice, artifactID).Scan(&payload.FromVersion, &payload.ToVersion, &editors, &lastEditor)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read due edit notice: %w", err)
	}
	if s.afterEditNoticeRead != nil {
		s.afterEditNoticeRead(artifactID)
	}
	// claimed checks what the claim deleted against the notice read above: errEditNoticeUnclaimed
	// when the row is no longer the one read, which no writer holding the owner's lock leaves, so
	// nothing is sent for it.
	claimed := func(row pgx.Row) error {
		var from, to int
		err := row.Scan(&from, &to)
		if errors.Is(err, pgx.ErrNoRows) {
			return errEditNoticeUnclaimed
		}
		if err != nil {
			return fmt.Errorf("claim edit notice: %w", err)
		}
		if from != payload.FromVersion || to != payload.ToVersion {
			return errEditNoticeUnclaimed
		}
		return nil
	}
	// drop claims the notice and sends nothing: a closed issue's, or a change only the server's
	// own attributes made.
	drop := func() (bool, error) {
		if err := claimed(tx.QueryRow(ctx, claimEditNotice, artifactID)); err != nil {
			return false, ignoreUnclaimed(err)
		}
		return false, tx.Commit(ctx)
	}
	if !open {
		return drop()
	}
	if err := json.Unmarshal(editors, &payload.Editors); err != nil {
		return false, fmt.Errorf("decode edit notice editors: %w", err)
	}
	var actor model.Actor
	if err := json.Unmarshal(lastEditor, &actor); err != nil {
		return false, fmt.Errorf("decode edit notice last editor: %w", err)
	}
	before, beforeFound, err := editNoticeVersionMarkdown(ctx, tx, artifactID, payload.FromVersion)
	if err != nil {
		return false, err
	}
	after, afterFound, err := editNoticeVersionMarkdown(ctx, tx, artifactID, payload.ToVersion)
	if err != nil {
		return false, err
	}
	if beforeFound && afterFound {
		if serverStateOnly(before, after) {
			// An answer, a resolve or a retraction wrote only the server's own attributes of a
			// typed block, as the person who made it: that change reaches the spec's writers as
			// its own ask event, so the window goes with no notice, as a closed issue's does.
			return drop()
		}
		payload.Diff, payload.DiffTruncated = text.UnifiedDiffCut(before, after, editNoticeDiffBytes)
	}
	payload.ArtifactID = artifactID
	payload.Name = owner.Name
	event := model.Event{IssueKey: owner.IssueKey, Type: "artifact.human_edited", Actor: actor, Payload: payload}
	if owner.IssueKey == nil {
		event.ArtifactID = &artifactID
	}
	var appended model.Event
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		b.Queue("", claimEditNotice, artifactID).QueryRow(claimed)
		if err := s.events.QueueAppend(b, event, func(written model.Event) { appended = written }); err != nil {
			return fmt.Errorf("append edit notice: %w", err)
		}
		return nil
	}); err != nil {
		return false, ignoreUnclaimed(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit edit notice: %w", err)
	}
	s.events.Publish(appended)
	return true, nil
}

// serverStateOnly reports whether two stored versions differ only in the attributes the server
// keeps on its typed blocks: an ask's state, who answered it and when, the options selected and
// the answer's words. Both are compared with those attributes at their defaults
// (pmdoc.StripServerOwnedAttrs), so a version a person's answer or resolve wrote is no edit of
// theirs. pmdoc.EqualOutsideServerState is not enough here: it counts the selection and the
// answer as caller text, which they are for the growth check, so an answer would still send a
// notice. Block ids are left out too (pmdoc.Node.Equal): a stored version carries only a typed
// block's id, and each parse mints the others afresh. A version that does not read back is not
// judged, and its notice goes.
func serverStateOnly(before, after string) bool {
	beforeTree, err := pmdoc.ParseRendering(before)
	if err != nil {
		return false
	}
	afterTree, err := pmdoc.ParseRendering(after)
	if err != nil {
		return false
	}
	pmdoc.StripServerOwnedAttrs(beforeTree)
	pmdoc.StripServerOwnedAttrs(afterTree)
	return beforeTree.Equal(afterTree)
}

// editNoticeVersionMarkdown reads the markdown of artifactID's version number, and reports whether that
// version is stored: a rebuild can leave a notice naming a version that is not.
func editNoticeVersionMarkdown(ctx context.Context, tx pgx.Tx, artifactID string, number int) (string, bool, error) {
	var markdown string
	err := tx.QueryRow(ctx, `
		select coalesce(markdown, '') from artifact_versions where artifact_id = $1 and number = $2
	`, artifactID, number).Scan(&markdown)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read document version %d: %w", number, err)
	}
	return markdown, true, nil
}
