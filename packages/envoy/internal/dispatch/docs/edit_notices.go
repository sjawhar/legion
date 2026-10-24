package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// defaultEditNoticeQuiet is how long a document stays quiet after a person's last settled version
// before its edit notice goes, when Deps.EditNoticeQuiet names none.
const defaultEditNoticeQuiet = 30 * time.Second

// openEditNotice opens a document's quiet window at a person's settled version, or moves on the
// one already open: from_version stays the version before the window's first person's version,
// to_version becomes this one, the editors gain this version's people, and the window ends a
// quiet time from now. It is a statement of its own so the sweep's delete (dueEditNotice) and it
// are the only two writers of the table.
const openEditNotice = `
	insert into document_edit_notices (artifact_id, from_version, to_version, editors, last_editor, due_at)
	values ($1, $2 - 1, $2, $3, $4, now() + $5::interval)
	on conflict (artifact_id) do update set
		to_version = excluded.to_version,
		editors = (select coalesce(jsonb_agg(distinct e), '[]'::jsonb)
			from jsonb_array_elements(document_edit_notices.editors || excluded.editors) e),
		last_editor = excluded.last_editor,
		due_at = excluded.due_at`

// recordEditNotice opens or moves on artifactID's edit notice for the version a settlement has
// just written, when that version lists a person: a person's browser edit is what the notice
// tells the document's sessions about. A version that lists no person writes nothing.
//
// The settlement calls it in its own transaction, after it took the document owner's lock
// (lockArtifactOwner) and wrote the version, so the row's lock comes second; the sweep
// (sendEditNotice) takes the two in the same order. Only a settlement calls it: an agent's write,
// an upload and a named version, which wakes the document's watchers itself (events.Broker.Notify),
// open no window.
//
// The notice's last editor is the room's latest editor, who is actor when the version lists them
// (settlementAuthors), and otherwise the last person authors lists.
func (s *Service) recordEditNotice(ctx context.Context, tx pgx.Tx, artifactID string, version int, authors []model.Actor, actor model.Actor) error {
	var editors []string
	var last *model.Actor
	for index := range authors {
		if authors[index].Kind == "user" {
			editors = append(editors, authors[index].ID)
			last = &authors[index]
		}
	}
	if len(editors) == 0 {
		return nil
	}
	if actor.Kind == "user" {
		last = &actor
	}
	editorsJSON, err := json.Marshal(editors)
	if err != nil {
		return fmt.Errorf("encode edit notice editors: %w", err)
	}
	lastJSON, err := json.Marshal(last)
	if err != nil {
		return fmt.Errorf("encode edit notice last editor: %w", err)
	}
	if _, err := tx.Exec(ctx, openEditNotice, artifactID, version, editorsJSON, lastJSON, s.editNoticeQuiet); err != nil {
		return fmt.Errorf("open edit notice: %w", err)
	}
	return nil
}
