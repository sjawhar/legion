// Package refs indexes Dispatch links and reads the reference graph. Mentions are derived from
// source text into refs; structural relations stay in the columns that own them and the
// graph_edges view unions both into one typed edge relation. Artifact targets use ref_key
// (`<issue-or-project>/<slug>`) so issue and project documents share one address; artifact
// sources are the artifact uuid. Closure returns at most eight hops and marks the result
// truncated when it finds a ninth. References whose target is absent stay stored but are absent
// from closures and referenced-by results.
package refs

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

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

// ToID returns the durable target identifier for a parsed Dispatch reference: the item id,
// an artifact's ref_key, or a component's '<project>/<id>' node address.
func ToID(ref text.Ref) string {
	switch ref.Kind {
	case "artifact":
		key := ref.IssueKey
		if key == "" {
			key = ref.Project
		}
		return key + "/" + ref.ID
	case "component":
		return ref.Project + "/" + ref.ID
	}
	return ref.ID
}

// Target is one end of a reference edge: the kind and durable id `refs` stores.
type Target struct {
	Kind string
	ID   string
}

// ReplaceCounted reconciles one source's references and resolves the counted targets the event
// that source appends has to name. Every write that indexes a body goes through it, so no
// producer can reconcile the graph without holding what moved: replace itself is unexported.
func ReplaceCounted(
	ctx context.Context,
	tx pgx.Tx,
	fromKind, fromID, body, serverURL string,
) (model.ReferenceChanges, error) {
	changed, err := replace(ctx, tx, fromKind, fromID, body, serverURL)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	return CountedChanges(ctx, tx, changed)
}

// replace reconciles the Dispatch references written by one source with body: targets no
// longer mentioned are deleted, new ones inserted, and an edge that persists keeps its
// created_at and source_seq. It returns the targets whose inbound edges moved — deleted or
// inserted, oldest state first — so ReplaceCounted can resolve the ones whose readers carry a
// batched backlink count.
func replace(ctx context.Context, tx pgx.Tx, fromKind, fromID, body, serverURL string) ([]Target, error) {
	kinds, ids := targets(body, serverURL)
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
		insert into refs (from_kind, from_id, to_kind, to_id)
		select $1, $2, target.kind, target.id from unnest($3::text[], $4::text[]) as target(kind, id)
		on conflict (from_kind, from_id, to_kind, to_id) do nothing
		returning to_kind, to_id
	`, fromKind, fromID, kinds, ids))
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

// targets is the deduplicated (to_kind, to_id) set body mentions, as parallel arrays.
func targets(body, serverURL string) ([]string, []string) {
	kinds, ids := []string{}, []string{}
	seen := make(map[[2]string]struct{})
	for _, ref := range text.Extract(body, serverURL) {
		if ref.Kind == "url" {
			continue
		}
		target := [2]string{ref.Kind, ToID(ref)}
		if _, dup := seen[target]; dup {
			continue
		}
		seen[target] = struct{}{}
		kinds = append(kinds, target[0])
		ids = append(ids, target[1])
	}
	return kinds, ids
}

// Stamp records eventID as the write that introduced every edge of one source that has no
// provenance yet. It runs after the source's event is appended, in the same transaction, and
// leaves edges that survived a reconcile untouched.
func Stamp(ctx context.Context, tx pgx.Tx, fromKind, fromID string, eventID int64) error {
	if _, err := tx.Exec(ctx, `
		update refs set source_seq = $3
		where from_kind = $1 and from_id = $2 and source_seq is null
	`, fromKind, fromID, eventID); err != nil {
		return fmt.Errorf("stamp references: %w", err)
	}
	return nil
}

// CountedChangeLimit bounds the targets one event names. A body citing more counted nodes than
// this is rare enough that carrying an unbounded array on every event is the wrong trade: the
// event says it was truncated and the reader refreshes its lists wholesale instead.
const CountedChangeLimit = 20

// CountedChanges names the changed targets whose readers carry a batched backlink count — an
// issue (its detail) and an ask (every list that carries the ask row, plus the human Inbox) —
// in the shape those readers are keyed by. Other kinds are dropped: nothing counts a comment,
// a message or a document, and the panels that list their edges refresh on the graph itself.
// Past CountedChangeLimit it returns truncated, and the reader falls back to refreshing every
// such list rather than carrying an unbounded array on an event.
//
// A reference the grammar accepts need not address anything: `dispatch://CORE-1/ask/hello`
// parses and is stored, so every lookup here compares text and a target that resolves to no
// row is simply not named. A write must never fail because of what its body cites.
func CountedChanges(ctx context.Context, q Queryer, changed []Target) (model.ReferenceChanges, error) {
	issueKeys := []string{}
	askIDs := []string{}
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
	references := make([]model.ChangedReference, 0, len(ordered))
	for _, target := range ordered {
		if target.Kind == "issue" {
			if _, found := issues[target.ID]; found {
				references = append(references, model.ChangedReference{Kind: "issue", ID: target.ID})
			}
			continue
		}
		if reference, found := asks[target.ID]; found {
			references = append(references, reference)
		}
	}
	if len(references) > CountedChangeLimit {
		return model.ReferenceChanges{Truncated: true}, nil
	}
	if len(references) == 0 {
		return model.ReferenceChanges{}, nil
	}
	return model.ReferenceChanges{Targets: references}, nil
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

// ReferencedBy lists Dispatch items and artifacts that mention an artifact reference key, in
// the shape the document surfaces read (kind, id order).
func ReferencedBy(ctx context.Context, q Queryer, refKey string) ([]model.ReferencedBy, error) {
	edges, err := resolveEdges(ctx, q, Query{Direction: "in", Kind: "artifact", ID: refKey, Kinds: []string{"mentions"}})
	if err != nil {
		return nil, err
	}
	references := make([]model.ReferencedBy, 0, len(edges))
	for _, edge := range edges {
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

// Outgoing lists Dispatch mentions written by an artifact, resolving artifact targets.
func Outgoing(ctx context.Context, q Queryer, artifactID string) ([]model.OutgoingReference, error) {
	rows, err := q.Query(ctx, `
		select e.to_kind, e.to_id,
		       a.id::text, a.issue_key, coalesce(a.project_key, ''), coalesce(a.ref_key, ''), coalesce(a.slug, ''), coalesce(a.name, ''), coalesce(a.kind, ''), coalesce(a.is_primary, false),
		       coalesce(a.created_by, '{}'::jsonb), coalesce(a.created_at, timestamptz 'epoch'),
		       coalesce(v.number, 0), coalesce(v.named, false), v.summary, coalesce(v.authors, '[]'::jsonb), coalesce(v.created_at, timestamptz 'epoch'), v.size, v.mime, v.sha256
		from graph_edges e
		left join artifacts a on e.to_kind = 'artifact' and a.ref_key = e.to_id
		left join artifact_versions v on v.artifact_id = a.id
		where e.from_kind = 'artifact' and e.from_id = $1 and e.kind = 'mentions'
		order by e.to_kind, e.to_id, v.number
	`, artifactID)
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
