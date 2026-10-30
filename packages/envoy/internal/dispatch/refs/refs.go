// Package refs indexes Dispatch links and reads the reference graph. Mentions are derived from
// source text into refs; structural relations stay in the columns that own them and the
// graph_edges view unions both into one typed edge relation. Artifact targets use ref_key
// (`<issue-or-project>/<slug>`, or `agent/<session id>/<slug>` for an agent's conversation) so
// every owner's documents and files share one address; artifact sources are the artifact uuid.
// Closure returns at most eight hops and marks the result truncated when it finds a ninth.
// References whose target is absent stay stored but are absent from closures and referenced-by
// results.
package refs

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// Queryer is the query surface shared by a pool and a transaction.
type Queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// Target is one end of a reference edge: the kind and durable id `refs` stores.
type Target struct {
	Kind string
	ID   string
}

// ReplaceCounted reconciles one source's references and resolves the counted targets the event
// that source appends has to name. Every write that indexes a body goes through it or one of its
// variants, so no producer can reconcile the graph without holding what moved: replaceRefs itself
// is unexported. It names the targets whose edges it inserted or deleted, which is all a first
// write of a body moves; an edit whose event names what it moved goes through ReplaceCountedEdit
// (ReplaceCountedItemEdit for a comment), and a document's version through
// ReplaceDocumentCounted, since a kept edge's excerpt can move too. A comment's or an issue
// message's first write goes through ReplaceCountedItem, which indexes its blocks as well.
func ReplaceCounted(
	ctx context.Context,
	tx pgx.Tx,
	fromKind, fromID, body, serverURL string,
) (model.ReferenceChanges, error) {
	changed, err := replaceRefs(ctx, tx, fromKind, fromID, text.Extract(body, serverURL), nil)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	return CountedChanges(ctx, tx, changed)
}

// ReplaceCountedEdit is ReplaceCounted for an edit of an ask's question from previous to body. It
// names the targets whose edges the edit inserted or deleted and those whose excerpt it moved
// (editedHeadTargets).
func ReplaceCountedEdit(
	ctx context.Context,
	tx pgx.Tx,
	fromKind, fromID, previous, body, serverURL string,
) (model.ReferenceChanges, error) {
	found := text.Extract(body, serverURL)
	changed, err := replaceRefs(ctx, tx, fromKind, fromID, found, nil)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	return CountedChanges(ctx, tx, append(changed, editedHeadTargets(previous, body, found)...))
}

// editedHeadTargets is what an edit of an ask's question or a comment's body from previous to body,
// which cites found, moves although its edges stay. Every edge such a source writes shows the head
// of its text as its excerpt (Edges, excerptRunes), so when the edit moves that head, the panel of
// every target the source still cites changes: each of found is returned then, and none otherwise.
func editedHeadTargets(previous, body string, found []text.Ref) []Target {
	if text.HeadRunes(previous, excerptRunes) == text.HeadRunes(body, excerptRunes) {
		return nil
	}
	kinds, ids := refTargets(found)
	targets := make([]Target, len(kinds))
	for index := range kinds {
		targets[index] = Target{Kind: kinds[index], ID: ids[index]}
	}
	return targets
}

// ReplaceDocumentCounted is ReplaceCounted for a document's version, written at versionAt, whose
// mentions are found (a new edge takes versionAt as its created_at). It also stores the excerpt of
// each mention the version writes (RecordExcerpts), or, with excerpts nil (a version with no
// rendering to cut them from), marks them owed (ClearExcerpts). A document mention shows the block
// holding it as its excerpt, so a target whose kept edge now stores another block is named beside
// the ones whose edges the version inserted or deleted, and so is each of moved, the targets the
// same write moved elsewhere - the version's blocks and their mentions (IndexBlocks) - so its event
// names them all.
func ReplaceDocumentCounted(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	version int,
	versionAt time.Time,
	found []text.Ref,
	moved []Target,
	excerpts map[Target]model.GraphExcerpt,
) (model.ReferenceChanges, error) {
	changed, err := replaceRefs(ctx, tx, "artifact", artifactID, found, &versionAt)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	changed = append(changed, moved...)
	if excerpts == nil {
		if err := ClearExcerpts(ctx, tx, artifactID); err != nil {
			return model.ReferenceChanges{}, err
		}
		return CountedChanges(ctx, tx, changed)
	}
	recorded, err := RecordExcerpts(ctx, tx, artifactID, version, excerpts)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	return CountedChanges(ctx, tx, append(changed, recorded...))
}

// replaceRefs reconciles the Dispatch references written by one source with found: targets no
// longer mentioned are deleted, new ones inserted with insertedAt as created_at (nil: now()), and
// an edge that persists keeps its created_at and source_seq. It returns the targets whose inbound
// edges moved — deleted or inserted, oldest state first — so ReplaceCounted and its variants can
// resolve the ones a reader refreshes by name (CountedChanges).
func replaceRefs(ctx context.Context, tx pgx.Tx, fromKind, fromID string, found []text.Ref, insertedAt *time.Time) ([]Target, error) {
	kinds, ids := refTargets(found)
	changed, err := scanTargets(tx.Query(ctx, `
		delete from refs
		where from_kind = $1 and from_id = $2
		  and not exists (
		    select 1 from unnest($3::text[], $4::text[]) as target(kind, id)
		    where target.kind = refs.to_kind and target.id = refs.to_id
		  )
		returning to_kind, to_id
	`, fromKind, fromID, kinds, ids))
	if err != nil {
		return nil, fmt.Errorf("clear references: %w", err)
	}
	if len(kinds) == 0 {
		return changed, nil
	}
	added, err := scanTargets(tx.Query(ctx, `
		insert into refs (from_kind, from_id, to_kind, to_id, created_at)
		select $1, $2, target.kind, target.id, coalesce($5::timestamptz, now())
		from unnest($3::text[], $4::text[]) as target(kind, id)
		on conflict (from_kind, from_id, to_kind, to_id) do nothing
		returning to_kind, to_id
	`, fromKind, fromID, kinds, ids, insertedAt))
	if err != nil {
		return nil, fmt.Errorf("write reference: %w", err)
	}
	return append(changed, added...), nil
}

func scanTargets(rows pgx.Rows, err error) ([]Target, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	changed := []Target{}
	for rows.Next() {
		var target Target
		if err := rows.Scan(&target.Kind, &target.ID); err != nil {
			return nil, err
		}
		changed = append(changed, target)
	}
	return changed, rows.Err()
}

// TargetOf is the graph target a parsed reference names, the kind and id refs and graph_edges
// store an edge to it under; an external URL names none. A block of a comment or a message is
// stored under its item's block kind, comment_block or message_block (ItemKind.BlockKind), not
// block: a build that predates item blocks skips a kind it does not know (resolveNodes), where it
// would count a block row and resolve it through document_blocks.
func TargetOf(ref text.Ref) (Target, bool) {
	if ref.Kind == "url" {
		return Target{}, false
	}
	kind := ref.Kind
	if kind == "block" && ref.Item != "" {
		kind = ItemKind(ref.Item).BlockKind()
	}
	return Target{Kind: kind, ID: text.TargetID(ref)}, true
}

// refTargets is the deduplicated (to_kind, to_id) set found names, as parallel arrays.
func refTargets(found []text.Ref) ([]string, []string) {
	kinds, ids := []string{}, []string{}
	seen := make(map[Target]struct{})
	for _, ref := range found {
		target, ok := TargetOf(ref)
		if !ok {
			continue
		}
		if _, dup := seen[target]; dup {
			continue
		}
		seen[target] = struct{}{}
		kinds = append(kinds, target.Kind)
		ids = append(ids, target.ID)
	}
	return kinds, ids
}

// Stamp records eventID as the write that introduced every edge of one source that has no
// provenance yet: the source's own refs and, for a document, a comment or a message, the mentions
// its blocks hold. It runs after the source's event is appended, in the same transaction, and
// leaves edges that survived a reconcile untouched.
func Stamp(ctx context.Context, tx pgx.Tx, fromKind, fromID string, eventID int64) error {
	if _, err := tx.Exec(ctx, `
		update refs set source_seq = $3
		where from_kind = $1 and from_id = $2 and source_seq is null
	`, fromKind, fromID, eventID); err != nil {
		return fmt.Errorf("stamp references: %w", err)
	}
	var blockRefs string
	switch fromKind {
	case "artifact":
		blockRefs = `update block_refs set source_seq = $2 where artifact_id = $1 and source_seq is null`
	case string(ItemComment), string(ItemMessage):
		// An item's blocks are keyed by its uuid; an id that is none names no item and holds no
		// block mention.
		if uuid.Validate(fromID) != nil {
			return nil
		}
		tables, _ := ItemKind(fromKind).tables()
		blockRefs = `update ` + tables.refs + ` set source_seq = $2 where ` + tables.item + ` = $1::uuid and source_seq is null`
	default:
		return nil
	}
	if _, err := tx.Exec(ctx, blockRefs, fromID, eventID); err != nil {
		return fmt.Errorf("stamp block references: %w", err)
	}
	return nil
}

// CountedChangeLimit bounds the targets of one kind (issue, ask, artifact, block) an event names,
// so an event carries at most four times as many. Each kind is capped on its own: an index whose
// one paragraph cites forty documents still names every one and every issue beside them, and a
// reader falls back only for the kind it was not told about. A hundred entries of the longest
// kind, a block (its node id, its reference and its document's uuid, about 200 bytes), is some
// 20 KB, so a full event of four kinds stays under 100 KB: about what a comment at
// contracts.DispatchBodyMax already carries, and far under NATS's 1 MiB maximum payload that the
// outbox publishes it through. A body citing more is rare enough that carrying an unbounded array
// on every event is the wrong trade.
const CountedChangeLimit = 100

// countedKinds is the order CountedChanges lists the kinds it truncated in.
var countedKinds = [...]string{"issue", "ask", "artifact", "block"}

// CountedChanges names the changed targets a reader refreshes by name — an issue (its detail and
// its count), an ask (every list that carries the ask row, plus the human Inbox), a document or
// file (its Referenced by panel) and a block of a document (its own panel and every link to it,
// which read one graph read, and its document's panel, which lists the edges at its blocks) — in
// the shape those readers are keyed by. Comments, messages and components are dropped: nothing
// counts them, and their panels refresh on the graph itself. A kind with more than
// CountedChangeLimit named targets names none of them and is listed in TruncatedKinds, and the
// reader falls back to refreshing every read of that kind; the other kinds are still named.
//
// A reference the grammar accepts need not address anything: `dispatch://CORE-1/ask/hello`
// parses and is stored, so every lookup here compares text and a target that resolves to no
// row is simply not named. A write must never fail because of what its body cites, with one
// exception: a typed block's `block[]` attribute is a schema field, not prose, and an agent's or
// API write introducing a pointer to an existing block of a type the attribute's targets do not
// allow is refused (pmdoc.CheckBlockPointers). A target that does not exist is still accepted.
func CountedChanges(ctx context.Context, q Queryer, changed []Target) (model.ReferenceChanges, error) {
	issueKeys := []string{}
	askIDs := []string{}
	artifactKeys := []string{}
	ordered := make([]Target, 0, len(changed))
	seen := make(map[[2]string]struct{}, len(changed))
	for _, target := range changed {
		if _, duplicate := seen[[2]string{target.Kind, target.ID}]; duplicate {
			continue
		}
		seen[[2]string{target.Kind, target.ID}] = struct{}{}
		switch target.Kind {
		case "issue":
			issueKeys = append(issueKeys, target.ID)
			ordered = append(ordered, target)
		case "ask":
			askIDs = append(askIDs, target.ID)
			ordered = append(ordered, target)
		case "artifact":
			artifactKeys = append(artifactKeys, target.ID)
			ordered = append(ordered, target)
		case "block":
			// A block is resolved through its document, the ref_key before the first '#'.
			document, _, _ := strings.Cut(target.ID, "#")
			artifactKeys = append(artifactKeys, document)
			ordered = append(ordered, target)
		}
	}
	issues, err := resolveIssueTargets(ctx, q, issueKeys)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	asks, err := resolveAskTargets(ctx, q, askIDs)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	artifacts, err := resolveArtifactTargets(ctx, q, artifactKeys)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	references := make([]model.ChangedReference, 0, len(ordered))
	for _, target := range ordered {
		switch target.Kind {
		case "issue":
			if _, found := issues[target.ID]; found {
				references = append(references, model.ChangedReference{Kind: "issue", ID: target.ID})
			}
		case "ask":
			if reference, found := asks[target.ID]; found {
				references = append(references, reference)
			}
		case "artifact":
			if reference, found := artifacts[target.ID]; found {
				references = append(references, reference)
			}
		case "block":
			document, blockID, _ := strings.Cut(target.ID, "#")
			if reference, found := artifacts[document]; found {
				references = append(references, model.ChangedReference{
					Kind:       "block",
					ID:         target.ID,
					ArtifactID: &reference.ID,
					Ref:        BlockRef(reference.Ref, blockID),
				})
			}
		}
	}
	named := make(map[string]int, len(countedKinds))
	for _, reference := range references {
		named[reference.Kind]++
	}
	changes := model.ReferenceChanges{}
	for _, kind := range countedKinds {
		if named[kind] > CountedChangeLimit {
			changes.TruncatedKinds = append(changes.TruncatedKinds, kind)
		}
	}
	for _, reference := range references {
		if named[reference.Kind] <= CountedChangeLimit {
			changes.Targets = append(changes.Targets, reference)
		}
	}
	return changes, nil
}

// MergeChanges is what two counted parts of one write moved, for an event that reports both: each
// target once, first's before second's. A kind either part truncated stays truncated and names
// none of its targets, and a kind whose merged targets pass CountedChangeLimit is truncated, as
// CountedChanges would have left it.
func MergeChanges(first, second model.ReferenceChanges) model.ReferenceChanges {
	truncated := make(map[string]bool, len(countedKinds))
	for _, kind := range slices.Concat(first.TruncatedKinds, second.TruncatedKinds) {
		truncated[kind] = true
	}
	references := make([]model.ChangedReference, 0, len(first.Targets)+len(second.Targets))
	seen := make(map[[2]string]struct{}, len(first.Targets)+len(second.Targets))
	named := make(map[string]int, len(countedKinds))
	for _, reference := range slices.Concat(first.Targets, second.Targets) {
		key := [2]string{reference.Kind, reference.ID}
		if _, duplicate := seen[key]; duplicate || truncated[reference.Kind] {
			continue
		}
		seen[key] = struct{}{}
		references = append(references, reference)
		named[reference.Kind]++
	}
	merged := model.ReferenceChanges{}
	for _, kind := range countedKinds {
		if truncated[kind] || named[kind] > CountedChangeLimit {
			merged.TruncatedKinds = append(merged.TruncatedKinds, kind)
		}
	}
	for _, reference := range references {
		if named[reference.Kind] <= CountedChangeLimit {
			merged.Targets = append(merged.Targets, reference)
		}
	}
	return merged
}

func resolveIssueTargets(ctx context.Context, q Queryer, keys []string) (map[string]struct{}, error) {
	found := map[string]struct{}{}
	if len(keys) == 0 {
		return found, nil
	}
	rows, err := q.Query(ctx, `select key from issues where key = any($1)`, keys)
	if err != nil {
		return nil, fmt.Errorf("resolve changed issue references: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scan changed issue reference: %w", err)
		}
		found[key] = struct{}{}
	}
	return found, rows.Err()
}

// resolveAskTargets skips a citation that is not a uuid rather than casting the column to text:
// `dispatch://CORE-1/ask/hello` parses and must never fail the write, and a target nothing can
// address resolves to nothing either way — while `id = any(...)` keeps the asks primary key,
// which matters because this runs inside the write transaction of every body that cites an ask.
func resolveAskTargets(ctx context.Context, q Queryer, ids []string) (map[string]model.ChangedReference, error) {
	found := map[string]model.ChangedReference{}
	addressable := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		parsed, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		addressable = append(addressable, parsed)
	}
	if len(addressable) == 0 {
		return found, nil
	}
	rows, err := q.Query(ctx, `
		select id::text, issue_key, artifact_id::text
		from asks
		where id = any($1)
	`, addressable)
	if err != nil {
		return nil, fmt.Errorf("resolve changed ask references: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		reference := model.ChangedReference{Kind: "ask"}
		if err := rows.Scan(&reference.ID, &reference.IssueKey, &reference.ArtifactID); err != nil {
			return nil, fmt.Errorf("scan changed ask reference: %w", err)
		}
		found[reference.ID] = reference
	}
	return found, rows.Err()
}

// resolveArtifactTargets resolves cited artifacts by the ref_key `refs` stores for them, the
// address GET /api/v1/references?to= resolves too, into the entry a Referenced by panel is
// matched by: the artifact's uuid (its node id in that read) and its dispatch:// address (the
// reference the dashboard opens the panel on, refs.ArtifactRef). Keyed by ref_key.
func resolveArtifactTargets(ctx context.Context, q Queryer, refKeys []string) (map[string]model.ChangedReference, error) {
	found := map[string]model.ChangedReference{}
	if len(refKeys) == 0 {
		return found, nil
	}
	rows, err := q.Query(ctx, `
		select id::text, ref_key, issue_key, coalesce(project_key, ''), session_id, slug, kind, is_primary
		from artifacts
		where ref_key = any($1)
	`, refKeys)
	if err != nil {
		return nil, fmt.Errorf("resolve changed artifact references: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, refKey, project, slug, kind string
		var issueKey, session *string
		var primary bool
		if err := rows.Scan(&id, &refKey, &issueKey, &project, &session, &slug, &kind, &primary); err != nil {
			return nil, fmt.Errorf("scan changed artifact reference: %w", err)
		}
		found[refKey] = model.ChangedReference{
			Kind: "artifact",
			ID:   id,
			Ref:  ArtifactRef(issueKey, project, session, slug, kind, primary),
		}
	}
	return found, rows.Err()
}

// ReferencedBy lists Dispatch items and artifacts that mention an artifact reference key or one
// of its current blocks, once per source, in the shape the document surfaces read (kind, id
// order). A citing document, comment or message is listed itself, not by the block holding its
// link.
func ReferencedBy(ctx context.Context, q Queryer, refKey string) ([]model.ReferencedBy, error) {
	edges, err := resolveEdges(ctx, q, Query{Direction: "in", Kind: "artifact", ID: refKey, Kinds: []string{"mentions"}, WholeDocuments: true})
	if err != nil {
		return nil, err
	}
	references := make([]model.ReferencedBy, 0, len(edges))
	// Every edge of one source carries that source's node and excerpt, so the entry kept is
	// the same whichever of its edges comes first.
	seen := make(map[[2]string]struct{}, len(edges))
	for _, edge := range edges {
		source := [2]string{edge.Node.Kind, edge.Node.ID}
		if _, listed := seen[source]; listed {
			continue
		}
		seen[source] = struct{}{}
		reference := model.ReferencedBy{
			Kind:     edge.Node.Kind,
			ID:       edge.Node.ID,
			IssueKey: edge.Node.IssueKey,
			Project:  edge.Node.Project,
			RefKey:   edge.refKey,
		}
		if edge.Excerpt != nil {
			reference.Excerpt = edge.Excerpt.Text
		}
		references = append(references, reference)
	}
	sort.Slice(references, func(left, right int) bool {
		if references[left].Kind != references[right].Kind {
			return references[left].Kind < references[right].Kind
		}
		return references[left].ID < references[right].ID
	})
	return references, nil
}

// Outgoing lists Dispatch mentions written by an artifact, resolving artifact targets. A block
// target keeps its own kind and id and carries its document as the artifact, while a current
// version holds the block; a block none holds is omitted, as is a target of a node kind this
// build does not know. A block of a comment or a message, which this shape has no field for, is
// listed by the graph read (Edges) alone. Any other unresolved target is listed.
func Outgoing(ctx context.Context, q Queryer, artifactID string) ([]model.OutgoingReference, error) {
	rows, err := q.Query(ctx, `
		select e.to_kind, e.to_id,
		       a.id::text, a.issue_key, coalesce(a.project_key, ''), a.session_id, coalesce(a.ref_key, ''), coalesce(a.slug, ''), coalesce(a.name, ''), coalesce(a.kind, ''), coalesce(a.is_primary, false),
		       coalesce(a.created_by, '{}'::jsonb), coalesce(a.created_at, timestamptz 'epoch'),
		       coalesce(v.number, 0), coalesce(v.named, false), v.summary, coalesce(v.authors, '[]'::jsonb), coalesce(v.created_at, timestamptz 'epoch'), `+model.VersionSize+`, v.mime, v.sha256
		from graph_edges e
		left join document_blocks d on e.to_kind = 'block' and d.node_id = e.to_id
		left join artifacts a on a.ref_key = case e.to_kind when 'artifact' then e.to_id when 'block' then split_part(d.node_id, '#', 1) end
		left join artifact_versions v on v.artifact_id = a.id
		where e.from_kind = 'artifact' and e.from_id = $1 and e.kind = 'mentions' and e.to_kind = any($2)
		  and e.to_kind not in ('comment_block', 'message_block')
		  and (e.to_kind <> 'block' or d.node_id is not null)
		order by e.to_kind, e.to_id, v.number
	`, artifactID, nodeKinds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	outgoing := []model.OutgoingReference{}
	positions := make(map[string]int)
	for rows.Next() {
		var reference model.OutgoingReference
		var artifactIDValue *string
		var artifact model.Artifact
		var createdBy, authors []byte
		var version model.Version
		if err := rows.Scan(
			&reference.Kind,
			&reference.ToID,
			&artifactIDValue,
			&artifact.IssueKey,
			&artifact.Project,
			&artifact.SessionID,
			&artifact.RefKey,
			&artifact.Slug,
			&artifact.Name,
			&artifact.Kind,
			&artifact.Primary,
			&createdBy,
			&artifact.CreatedAt,
			&version.Number,
			&version.Named,
			&version.Summary,
			&authors,
			&version.CreatedAt,
			&version.Size,
			&version.MIME,
			&version.SHA256,
		); err != nil {
			return nil, err
		}
		if artifactIDValue == nil {
			outgoing = append(outgoing, reference)
			continue
		}
		artifact.ID = *artifactIDValue
		if err := json.Unmarshal(createdBy, &artifact.CreatedBy); err != nil {
			return nil, fmt.Errorf("decode artifact author: %w", err)
		}
		if err := json.Unmarshal(authors, &version.Authors); err != nil {
			return nil, fmt.Errorf("decode artifact version authors: %w", err)
		}
		if position, found := positions[reference.ToID]; found {
			outgoing[position].Artifact.Versions = append(outgoing[position].Artifact.Versions, version)
			continue
		}
		artifact.Versions = []model.Version{version}
		reference.Artifact = &artifact
		positions[reference.ToID] = len(outgoing)
		outgoing = append(outgoing, reference)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return outgoing, nil
}
