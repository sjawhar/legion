package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// replaceReferences is refs.ReplaceCounted bound to this server's URL, which is all an API
// write path needs to say the same thing about what it moved as every other producer.
func (s *server) replaceReferences(
	ctx context.Context,
	tx pgx.Tx,
	fromKind, fromID, body string,
) (model.ReferenceChanges, error) {
	return refs.ReplaceCounted(ctx, tx, fromKind, fromID, body, s.deps.ServerURL)
}

// replaceEditedReferences is refs.ReplaceCountedEdit bound to this server's URL: an edit of an
// ask's question from previous to body. A comment's edit goes through refs.ReplaceCountedItemEdit,
// which indexes its blocks as well.
func (s *server) replaceEditedReferences(
	ctx context.Context,
	tx pgx.Tx,
	fromKind, fromID, previous, body string,
) (model.ReferenceChanges, error) {
	return refs.ReplaceCountedEdit(ctx, tx, fromKind, fromID, previous, body, s.deps.ServerURL)
}

// referencesSinceWait bounds how long a since read waits for the safe horizon to pass every event
// committed before the read (events.Horizon.Await). Only a transaction held open on the Postgres
// instance keeps the horizon back that long, and a poller would rather have the mentions at or
// below the horizon now, and the rest on its next read, than an answer that waits on someone
// else's transaction for as long as it stays open.
const referencesSinceWait = time.Second

// getReferences reads one node's edges in the reference graph: ?to= lists every edge pointing
// at the node (backlinks), ?from= every edge it writes; exactly one is required. ?kind= narrows
// edge types and ?since= keeps mentions introduced after an events.id, which excludes
// structural edges. Cross-project by design: no project filter. A block reference names a
// block node carrying its whole current text; one pinned to a version of its document, or a
// revision of its comment, also reads the block as that version or revision recorded it
// (blockReferenceNode, itemBlockReferenceNode). A ?kind= this build does not emit - a newer
// build's, during a release - has no edges here, and the answer lists it in unknown_kinds rather
// than refusing the read.
//
// A since read keeps only mentions whose event is at or below the safe horizon
// (events.Horizon), at or below which every event is final. A poller passes the highest
// source_seq it was shown as its next since, and event ids are not commit order: a mention whose
// event drew a lower id and committed after a higher one was read would otherwise fall below
// that since and never be shown. The read caps at the horizon sinceHorizon answers, which is at
// or above every event committed before the read unless an open transaction holds the horizon
// back past referencesSinceWait: so a since read answers every mention committed before it, the
// caller's own included, and one held back answers the mentions at or below the horizon and
// leaves the rest to a later read.
func (s *server) getReferences(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query, ref, unknownKinds, err := parseReferencesQuery(r, s.deps.ServerURL)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var node model.GraphNode
	if ref.Kind == "block" {
		if ref.Item != "" {
			node, err = s.itemBlockReferenceNode(r.Context(), query, ref)
		} else {
			node, err = s.blockReferenceNode(r.Context(), query, ref)
		}
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if node.Pin != nil && node.Pin.State == "removed" {
			// The graph lists no link to a block no current version holds.
			WriteJSON(w, http.StatusOK, model.GraphReferences{Node: node, Edges: []model.GraphEdge{}, UnknownKinds: unknownKinds})
			return
		}
	} else {
		if query.Direction == "out" && ref.Kind == "artifact" {
			if err := s.deps.Store.Pool.QueryRow(r.Context(), `select id::text from artifacts where ref_key = $1`, query.ID).Scan(&query.ID); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					s.writeHandlerError(w, errorf(http.StatusNotFound, "NOT_FOUND", "no document at %s", query.ID))
					return
				}
				s.writeHandlerError(w, err)
				return
			}
		}
		var found bool
		node, found, err = refs.Node(r.Context(), s.deps.Store.Pool, query.Kind, query.ID, query.Direction == "in")
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if !found {
			s.writeHandlerError(w, errorf(http.StatusNotFound, "NOT_FOUND", "no %s at %s", query.Kind, query.ID))
			return
		}
	}
	// When this build knows none of the kinds the caller named, no edge is of them; refs.Edges
	// with no Kinds would read every kind.
	edges := []model.GraphEdge{}
	if len(query.Kinds) > 0 || len(unknownKinds) == 0 {
		if query.Since != nil {
			safe, err := s.sinceHorizon(r.Context())
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			query.Through = &safe
		}
		edges, err = refs.Edges(r.Context(), s.deps.Store.Pool, query)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if query.Direction == "in" {
		if err := s.documentMentionExcerpts(r.Context(), query, edges); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	WriteJSON(w, http.StatusOK, model.GraphReferences{Node: node, Edges: edges, UnknownKinds: unknownKinds})
}

// sinceHorizon is the horizon a since read caps at: H once an observation taken after the call is
// proven safe (Horizon.Await), so H is at or above every event committed before the read. Safe
// alone would not do: an observation is proven only by a later one, so on a process where
// nothing else observes (no event stream has started the relay, no Delivery page the hub) the
// first read after boot or a Reset would answer H = 0, and each later read stop at the previous
// read's observation, however old. Await takes longer than an observation or two only while a
// transaction that holds a transaction id on the instance holds the horizon back; past
// referencesSinceWait, with the request still live, the read takes the horizon as an observation
// started after that finds it (Horizon.Safe), and a mention above it waits for a later read. A
// request that ends first answers Await's error.
func (s *server) sinceHorizon(ctx context.Context) (int64, error) {
	wait, cancel := context.WithTimeout(ctx, referencesSinceWait)
	safe, err := s.horizon().Await(wait)
	cancel()
	if err == nil || ctx.Err() != nil {
		return safe, err
	}
	return s.horizon().Safe(ctx)
}

// blockReferenceNode is the block node ref names (query addresses it), with its whole current
// text. A block no current version of its document holds answers 404, unless ref is pinned to a
// version N that held it: the node then carries what N recorded (model.BlockPin), "unchanged"
// or "changed" by comparing the current text with N's, or "removed" with the version that deleted
// it, as a node with no text or changed version whose facts are a removed block's
// (refs.RemovedBlockNode); a pin to a current block carries the current block's facts. A pin to
// a version the document lacks, to one whose blocks are not on record (an older build wrote it and
// none indexed it) or to one that did not hold the block answers 404, each saying which.
func (s *server) blockReferenceNode(ctx context.Context, query refs.Query, ref text.Ref) (model.GraphNode, error) {
	pool := s.deps.Store.Pool
	documentKey, _, _ := strings.Cut(query.ID, "#")
	document, found, err := refs.Node(ctx, pool, "artifact", documentKey, true)
	if err != nil {
		return model.GraphNode{}, err
	}
	if !found {
		return model.GraphNode{}, errorf(http.StatusNotFound, "NOT_FOUND", "no document at %s", documentKey)
	}
	node, current, err := refs.Node(ctx, pool, "block", query.ID, true)
	if err != nil {
		return model.GraphNode{}, err
	}
	if ref.Pin == 0 {
		if !current {
			return model.GraphNode{}, errorf(http.StatusNotFound, "NOT_FOUND", "no block %q in %s", ref.Block, document.Ref)
		}
		node.Text, err = refs.CurrentBlockText(ctx, pool, document.ID, ref.Block)
		return node, err
	}
	at, err := refs.BlockTextAt(ctx, pool, document.ID, ref.Block, ref.Pin)
	if err != nil {
		return model.GraphNode{}, err
	}
	switch {
	case !at.Exists:
		return model.GraphNode{}, errorf(http.StatusNotFound, "NOT_FOUND", "no version %d of %s", ref.Pin, document.Ref)
	case !at.Indexed && at.Earliest == 0:
		return model.GraphNode{}, errorf(http.StatusNotFound, "NOT_FOUND", "version %d of %s has no record of its blocks; no version of it has one yet", ref.Pin, document.Ref)
	case !at.Indexed:
		return model.GraphNode{}, errorf(http.StatusNotFound, "NOT_FOUND", "version %d of %s has no record of its blocks; the earliest is version %d", ref.Pin, document.Ref, at.Earliest)
	case at.Text == nil:
		return model.GraphNode{}, errorf(http.StatusNotFound, "NOT_FOUND", "no block %q in %s at version %d", ref.Block, document.Ref, ref.Pin)
	}
	blockRef := refs.BlockRef(document.Ref, ref.Block)
	pin := &model.BlockPin{Version: ref.Pin, Ref: blockRef + "@v" + strconv.Itoa(ref.Pin), Text: *at.Text}
	if !current {
		pin.State, pin.RemovedVersion = "removed", at.RemovedVersion
		removed, err := refs.RemovedBlockNode(ctx, pool, document, query.ID, ref.Block, at.RemovedVersion)
		if err != nil {
			return model.GraphNode{}, err
		}
		removed.Pin = pin
		return removed, nil
	}
	if node.Text, err = refs.CurrentBlockText(ctx, pool, document.ID, ref.Block); err != nil {
		return model.GraphNode{}, err
	}
	pin.State = "unchanged"
	if node.Text != pin.Text {
		pin.State = "changed"
	}
	node.Pin = pin
	return node, nil
}

// itemBlockReferenceNode is the block node ref, a block of a comment or a message, names (query
// addresses it), with its whole current text (refs.CurrentItemBlockText). A block no current
// revision of its item holds answers 404, as a document's does, unless ref is pinned to a revision
// N of its comment that held it: the node then carries what N recorded (model.BlockPin, its
// Version the revision), "unchanged" or "changed" by comparing the current text with N's, or
// "removed" with the revision that removed it (RemovedVersion), as a node with no text or changed
// revision whose facts are a removed block's (refs.RemovedCommentBlockNode). A pin past the
// comment's current revision, or to one that did not hold the block, answers 404, each saying
// which. A message never changes, so no reference pins a message's block (parseReferencesQuery
// refuses one).
func (s *server) itemBlockReferenceNode(ctx context.Context, query refs.Query, ref text.Ref) (model.GraphNode, error) {
	pool := s.deps.Store.Pool
	item, found, err := refs.Node(ctx, pool, ref.Item, ref.ID, true)
	if err != nil {
		return model.GraphNode{}, err
	}
	if !found {
		return model.GraphNode{}, errorf(http.StatusNotFound, "NOT_FOUND", "no %s %s", ref.Item, ref.ID)
	}
	node, current, err := refs.Node(ctx, pool, query.Kind, query.ID, true)
	if err != nil {
		return model.GraphNode{}, err
	}
	if current {
		if node.Text, err = refs.CurrentItemBlockText(ctx, pool, refs.ItemKind(ref.Item), query.ID); err != nil {
			return model.GraphNode{}, err
		}
	}
	if ref.Pin == 0 {
		if !current {
			return model.GraphNode{}, errorf(http.StatusNotFound, "NOT_FOUND", "no block %q in %s %s", ref.Block, ref.Item, item.ID)
		}
		return node, nil
	}
	at, err := refs.CommentBlockAt(ctx, pool, item.ID, ref.Block, ref.Pin)
	if err != nil {
		return model.GraphNode{}, err
	}
	switch {
	case ref.Pin > at.Current:
		return model.GraphNode{}, errorf(http.StatusNotFound, "NOT_FOUND", "no revision %d of comment %s; its current revision is %d", ref.Pin, item.ID, at.Current)
	case at.Text == nil:
		return model.GraphNode{}, errorf(http.StatusNotFound, "NOT_FOUND", "no block %q in comment %s at revision %d", ref.Block, item.ID, ref.Pin)
	}
	pin := &model.BlockPin{Version: ref.Pin, Ref: refs.BlockRef(item.Ref, ref.Block) + "@v" + strconv.Itoa(ref.Pin), Text: *at.Text}
	if !current {
		removed, found, err := refs.RemovedCommentBlockNode(ctx, pool, query.ID, at.RemovedRevision)
		if err != nil {
			return model.GraphNode{}, err
		}
		if !found {
			return model.GraphNode{}, errorf(http.StatusNotFound, "NOT_FOUND", "no block %q in comment %s", ref.Block, item.ID)
		}
		pin.State, pin.RemovedVersion = "removed", at.RemovedRevision
		removed.Pin = pin
		return removed, nil
	}
	pin.State = "unchanged"
	if node.Text != pin.Text {
		pin.State = "changed"
	}
	node.Pin = pin
	return node, nil
}

// blockPin is a trailing `@v<N>` pin, as a block reference writes one.
var blockPin = regexp.MustCompile(`@v[0-9]+$`)

// pinsAMessageBlock reports whether raw, a reference text.ParseRef refused, is a block of a
// message pinned to a revision: the grammar reads none, since a message never changes.
func pinsAMessageBlock(raw, serverURL string) bool {
	unpinned := blockPin.ReplaceAllString(raw, "")
	if unpinned == raw {
		return false
	}
	ref, ok := text.ParseRef(unpinned, serverURL)
	return ok && ref.Kind == "block" && ref.Item == "message"
}

// parseReferencesQuery turns ?to=|?from=, ?kind=, and ?since= into an edge query addressed the
// way graph_edges stores the node (refs.TargetOf): text.TargetID for the to side (ref_key for
// artifacts; a comment's or message's block under its storage kind), the
// item id for the from side (the caller swaps an artifact's ref_key for its uuid). The reference
// is read by text.ParseRef, so a node is read whatever the length of its name: the target bound
// decides only what text indexes, and a node no index holds an edge to simply has fewer edges.
// It returns apart the ?kind= values that are no edge kind of this build.
func parseReferencesQuery(r *http.Request, serverURL string) (refs.Query, text.Ref, []string, error) {
	values := r.URL.Query()
	to, from := strings.TrimSpace(values.Get("to")), strings.TrimSpace(values.Get("from"))
	if (to == "") == (from == "") {
		return refs.Query{}, text.Ref{}, nil, errorf(http.StatusBadRequest, "INVALID_REFERENCE", "exactly one of to or from is required")
	}
	raw, direction := to, "in"
	if from != "" {
		raw, direction = from, "out"
	}
	ref, ok := text.ParseRef(raw, serverURL)
	if !ok {
		if pinsAMessageBlock(raw, serverURL) {
			return refs.Query{}, text.Ref{}, nil, errorf(http.StatusBadRequest, "INVALID_REFERENCE", "%q pins a block of a message, which never changes; name the block without @v<N>", raw)
		}
		return refs.Query{}, text.Ref{}, nil, errorf(http.StatusBadRequest, "INVALID_REFERENCE", "%q is not a dispatch:// reference to an issue, document, block, ask, comment, message, or component", raw)
	}
	// The node as graph_edges stores it: a block of a comment or message under its storage kind.
	target, _ := refs.TargetOf(ref)
	query := refs.Query{Direction: direction, Kind: target.Kind, ID: target.ID}
	var unknownKinds []string
	if kinds := strings.TrimSpace(values.Get("kind")); kinds != "" {
		for _, kind := range strings.Split(kinds, ",") {
			kind = strings.TrimSpace(kind)
			if !refs.KnownKind(kind) {
				unknownKinds = append(unknownKinds, kind)
				continue
			}
			query.Kinds = append(query.Kinds, kind)
		}
	}
	// An ask's inbound `replies_to` edges are its own clarification thread, which every ask
	// surface renders inline; "what references this ask" never means them. A caller that asks
	// for that kind explicitly still gets it — this is the default, not a hidden rule.
	if direction == "in" && ref.Kind == "ask" && len(query.Kinds) == 0 {
		query.ExcludeKinds = refs.AskBacklinkExclusions
	}
	// A document's, comment's or message's blocks are each part_of it; its links are what its
	// read means, so those edges are listed only when asked for by kind.
	if (ref.Kind == "artifact" || ref.Kind == "comment" || ref.Kind == "message") && len(query.Kinds) == 0 {
		query.ExcludeKinds = []string{"part_of"}
	}
	if since := strings.TrimSpace(values.Get("since")); since != "" {
		value, err := strconv.ParseInt(since, 10, 64)
		if err != nil || value < 0 {
			return refs.Query{}, text.Ref{}, nil, errorf(http.StatusBadRequest, "INVALID_SINCE", "since must be an event id")
		}
		query.Since = &value
	}
	return query, ref, unknownKinds, nil
}

// documentMentionExcerpts replaces the excerpt of every mention written by a document with the
// excerpt stored when the mention was indexed (refs.RecordExcerpts): the block holding the mention,
// its id a #b-<id> jump target, and its markdown. The route reads no document: a mention whose
// excerpt the reconciliation has not stored yet, is owed a fresh one, or lies in no block keeps
// the document's name. The reconciliation serves the next stored block only after it records a
// non-null excerpt_version. During the deploy that ships migration 0092, a stale excerpt whose
// version is still set is served until the reconciliation marks it owed (refs.StoredExcerpts).
func (s *server) documentMentionExcerpts(ctx context.Context, query refs.Query, edges []model.GraphEdge) error {
	stored, err := refs.StoredExcerpts(ctx, s.deps.Store.Pool, query.Kind, query.ID)
	if err != nil {
		return err
	}
	for index := range edges {
		edge := &edges[index]
		if edge.Kind != "mentions" || edge.Node.Kind != "artifact" {
			continue
		}
		if excerpt, found := stored[edge.Node.ID]; found {
			edge.Excerpt = &excerpt
		}
	}
	return nil
}

// getIssueReferences answers an issue's reference closure. ?versions=latest leaves each member's
// artifact.versions holding only its latest version, under its own ETag; absent, empty or all is
// the full answer. A repeated versions is refused rather than read by its first value.
func (s *server) getIssueReferences(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	versionsValues := r.URL.Query()["versions"]
	if len(versionsValues) > 1 {
		s.writeHandlerError(w, errorf(http.StatusBadRequest, "INVALID_QUERY", "versions must be given once, as all or latest"))
		return
	}
	var latestOnly bool
	if len(versionsValues) == 1 {
		switch versionsValues[0] {
		case "", "all":
		case "latest":
			latestOnly = true
		default:
			s.writeHandlerError(w, errorf(http.StatusBadRequest, "INVALID_QUERY", "versions must be all or latest"))
			return
		}
	}
	issue, err := s.loadIssue(r.Context(), s.deps.Store.Pool, r.PathValue("key"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	closure, truncated, err := refs.Closure(r.Context(), s.deps.Store.Pool, issue.Key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	refKeys := make([]string, len(closure))
	for index, member := range closure {
		refKeys[index] = member.RefKey
	}
	artifacts, err := s.loadArtifactsByRefKey(r.Context(), s.deps.Store.Pool, refKeys, latestOnly)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	members := make([]model.ReferenceMember, 0, len(closure))
	for _, closureMember := range closure {
		artifact, ok := artifacts[closureMember.RefKey]
		if !ok {
			s.writeHandlerError(w, fmt.Errorf("reference target %q disappeared while loading closure", closureMember.RefKey))
			return
		}
		members = append(members, model.ReferenceMember{
			Artifact: *artifact,
			Depth:    closureMember.Depth,
			Via:      closureMember.Via,
		})
	}
	etag := referencesETag(issue.LastSeq, members, truncated, latestOnly)
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	WriteJSON(w, http.StatusOK, model.IssueReferences{Members: members, Truncated: truncated})
}

func (s *server) getArtifactReferences(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	artifact, err := s.loadArtifact(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	outgoing, err := refs.Outgoing(r.Context(), s.deps.Store.Pool, artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	referencedBy, err := refs.ReferencedBy(r.Context(), s.deps.Store.Pool, artifact.RefKey)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, model.ArtifactReferences{Outgoing: outgoing, ReferencedBy: referencedBy})
}

// referencesETag hashes what an issue references answer depends on. The trimmed answer
// (latestOnly) hashes one more line naming its variant, so the full and trimmed answers of one
// issue never share an ETag; the full answer hashes no such line.
func referencesETag(lastSeq int, members []model.ReferenceMember, truncated, latestOnly bool) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%d\n%t\n", lastSeq, truncated)
	if latestOnly {
		_, _ = io.WriteString(hash, "versions=latest\n")
	}
	for _, member := range members {
		_, _ = fmt.Fprintf(hash, "%s@%d\n", member.Artifact.ID, member.Artifact.Versions[len(member.Artifact.Versions)-1].Number)
	}
	return `"` + hex.EncodeToString(hash.Sum(nil)) + `"`
}

// loadArtifactsByRefKey reads the documents refKeys name, each with every version in number
// order, or with latestOnly only its latest version. A document with no version is absent.
func (s *server) loadArtifactsByRefKey(ctx context.Context, q queryer, refKeys []string, latestOnly bool) (map[string]*model.Artifact, error) {
	artifacts := make(map[string]*model.Artifact, len(refKeys))
	if len(refKeys) == 0 {
		return artifacts, nil
	}
	versions := `join artifact_versions v on v.artifact_id = a.id`
	if latestOnly {
		versions = `join lateral (
			select * from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1
		) v on true`
	}
	rows, err := q.Query(ctx, `
		select a.id::text, a.issue_key, coalesce(a.project_key, ''), a.session_id, a.ref_key, a.slug, a.name, a.kind, a.is_primary,
		       a.created_by, a.created_at,
		       `+versionColumns+`
		from artifacts a
		`+versions+`
		where a.ref_key = any($1)
		order by a.ref_key, v.number
	`, refKeys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// authorsByJSON decodes each distinct author list once, and every version storing those bytes
	// shares the slice. That is safe because what this loader returns is read-only, encoded once
	// by getIssueReferences, and the memo lives for this one call only.
	authorsByJSON := map[string][]model.Actor{}
	for rows.Next() {
		var artifact model.Artifact
		var createdBy, authors []byte
		var version model.Version
		if err := rows.Scan(
			&artifact.ID,
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
		current, found := artifacts[artifact.RefKey]
		if !found {
			if err := json.Unmarshal(createdBy, &artifact.CreatedBy); err != nil {
				return nil, fmt.Errorf("decode artifact author: %w", err)
			}
			current = &artifact
			artifacts[artifact.RefKey] = current
		}
		decoded, ok := authorsByJSON[string(authors)]
		if !ok {
			if err := json.Unmarshal(authors, &decoded); err != nil {
				return nil, fmt.Errorf("decode artifact version authors: %w", err)
			}
			decoded = slices.Clip(decoded)
			authorsByJSON[string(authors)] = decoded
		}
		version.Authors = decoded
		current.Versions = append(current.Versions, version)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return artifacts, nil
}
