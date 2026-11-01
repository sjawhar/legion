package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// An issue's decisions are its document `decisions.md`, and this file is the server's half of the
// Decisions view: the view itself, Keep, and each person's last read version of a document.
//
// Each decision is one `##` section of that document: its heading block and every block after it
// up to the next heading of its level or higher. Its heading names the choice, its blocks give the
// reason and the options weighed, and each spec block it shaped is linked
// `dispatch://KEY/spec#b-<id>`, which the block index records as a link from the decision's own
// block to that spec block. The section headed exactly `## Replaced` holds replaced decisions as
// `###` sections, a paragraph that starts `Unsure:` marks a decision unsure, and a person's
// decision links its answered ask.
//
// A spec block X linked from decision D by link L is out of date exactly when D changed after it
// first linked X (D's changed version is above L's created version) and X has not changed since D
// did. A new decision therefore marks nothing, and neither does the block index's first pass over
// a document, which gives every block and every pair one version.
//
// The links are read through refs.BlockLinks, the read GET /api/v1/block-links already serves, and
// each block's own changed version through refs.BlockChanges: this view writes no query for a link
// the block graph already answers.

const (
	// decisionsDocumentName is the document an issue's decisions live in, and
	// decisionsDocumentSlug the slug its reference names
	// (`dispatch://KEY/artifact/decisions-md`). The view reads the slug first and falls back to
	// the filename, as every document reference resolves.
	decisionsDocumentName = "decisions.md"
	decisionsDocumentSlug = "decisions-md"
	// replacedHeading is the section that holds replaced decisions; its own `###` sections are
	// the replaced ones, and it is no decision itself.
	replacedHeading = "Replaced"
	// unsurePrefix opens the paragraph that marks a decision unsure and says what would change
	// the agent's mind.
	unsurePrefix = "Unsure:"
)

// issueDecisions is GET /api/v1/issues/{key}/decisions: the issue's decisions document, the open
// questions its spec still holds, and each decision of that document in document order. document
// is null, with no decisions, when the issue has no decisions.md.
type issueDecisions struct {
	Document      *string          `json:"document"`
	OpenQuestions []issueAskRow    `json:"open_questions"`
	Decisions     []model.Decision `json:"decisions"`
}

// decisionSection is one decision as the document's own blocks give it, before the links, asks and
// keeps that complete it are read. Start and End are its first and last block's bounds in the
// document's canonical markdown.
type decisionSection struct {
	headingBlock string
	heading      string
	level        int
	replaced     bool
	unsure       bool
	blockIDs     []string
	start        int
	end          int
}

func (s *server) getIssueDecisions(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	answer := issueDecisions{OpenQuestions: []issueAskRow{}, Decisions: []model.Decision{}}
	spec, specFound, err := s.issueSpecDocument(r.Context(), key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if specFound {
		if answer.OpenQuestions, err = s.specOpenQuestions(r.Context(), key, spec.ID); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	document, found, err := s.issueDecisionsDocument(r.Context(), key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !found {
		WriteJSON(w, http.StatusOK, answer)
		return
	}
	reference := artifactReference(document)
	answer.Document = &reference
	specID := ""
	if specFound {
		specID = spec.ID
	}
	if answer.Decisions, err = s.readDecisions(r.Context(), document, reference, specID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}

// artifactReference is a document's own dispatch:// address.
func artifactReference(document model.Artifact) string {
	return refs.ArtifactRef(document.IssueKey, document.Project, document.SessionID, document.Slug,
		document.Kind, document.Primary)
}

// issueSpecDocument is the issue's primary document, the spec a decision's links point into. An
// issue with none, and an issue that does not exist, answer found false: the view is a read, and
// an issue with no spec simply holds no decision link.
func (s *server) issueSpecDocument(ctx context.Context, key string) (model.Artifact, bool, error) {
	artifact, err := s.loadArtifactRow(ctx, s.deps.Store.Pool, s.deps.Store.Pool.QueryRow(ctx, `
		select `+artifactColumns+`
		from artifacts where issue_key = $1 and is_primary and kind = 'doc'
	`, key))
	if err != nil {
		if artifactMissing(err) {
			return model.Artifact{}, false, nil
		}
		return model.Artifact{}, false, err
	}
	return artifact, true, nil
}

// issueDecisionsDocument is the issue's decisions.md: the document at the slug its reference
// names, else the one whose filename it is, as every document reference resolves.
func (s *server) issueDecisionsDocument(ctx context.Context, key string) (model.Artifact, bool, error) {
	target := artifactTarget{IssueKey: &key}
	document, err := s.loadArtifactByRefKey(ctx, s.deps.Store.Pool, target.refPrefix()+"/"+decisionsDocumentSlug)
	if err == nil {
		return document, true, nil
	}
	if !isArtifactNotFound(err) {
		return model.Artifact{}, false, err
	}
	document, err = s.loadArtifactByDocumentReference(ctx, s.deps.Store.Pool, target, decisionsDocumentName)
	if err != nil {
		if artifactMissing(err) {
			return model.Artifact{}, false, nil
		}
		return model.Artifact{}, false, err
	}
	return document, true, nil
}

// artifactMissing reports whether err says the issue holds no such document: no row, no document
// of that name, or more than one, each of which this view reads as an issue with no decisions.md
// rather than as a refusal of the read.
func artifactMissing(err error) bool {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.code == "ARTIFACT_NOT_FOUND" || apiErr.code == "ARTIFACT_REFERENCE_AMBIGUOUS"
}

// specOpenQuestions are the issue's open asks whose block is in its spec, oldest first, in the
// shape every ask read has (askReadColumns, with the newest reply beside the row).
func (s *server) specOpenQuestions(ctx context.Context, key, specID string) ([]issueAskRow, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select `+askReadColumns+` `+askReadFrom+`
		where a.issue_key = $1 and a.state = 'open' and a.block_artifact_id = $2::uuid
		order by a.created_at, a.id
	`, key, specID)
	if err != nil {
		return nil, err
	}
	questions := []issueAskRow{}
	for rows.Next() {
		ask, reply, err := scanAskRead(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		questions = append(questions, issueAskRow{Ask: ask, LastReply: reply})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	asks := make([]*model.Ask, len(questions))
	for index := range questions {
		asks[index] = &questions[index].Ask
	}
	if err := s.attachOpenedEventIDs(ctx, s.deps.Store.Pool, asks); err != nil {
		return nil, err
	}
	if err := attachAskBacklinkCounts(ctx, s.deps.Store.Pool, asks); err != nil {
		return nil, err
	}
	return questions, nil
}

// readDecisions builds every decision of the decisions document: its sections from the document's
// own blocks, its links from the block graph (refs.BlockLinks), its asks from the block mentions
// its blocks hold, and its keeps from decision_keeps. specID is "" when the issue has no spec, and
// every decision then has no links.
func (s *server) readDecisions(ctx context.Context, document model.Artifact, reference, specID string) ([]model.Decision, error) {
	markdown, sections, err := s.decisionSections(ctx, document.ID)
	if err != nil {
		return nil, err
	}
	changes, err := refs.BlockChanges(ctx, s.deps.Store.Pool, document.ID)
	if err != nil {
		return nil, err
	}
	var links []model.BlockLink
	if specID != "" {
		if links, err = refs.BlockLinks(ctx, s.deps.Store.Pool, document.ID, specID); err != nil {
			return nil, err
		}
	}
	asks, err := decisionBlockAsks(ctx, s.deps.Store.Pool, document.ID)
	if err != nil {
		return nil, err
	}
	keeps, err := decisionKeeps(ctx, s.deps.Store.Pool, document.ID)
	if err != nil {
		return nil, err
	}
	decisions := make([]model.Decision, 0, len(sections))
	for _, section := range sections {
		decision := model.Decision{
			BlockID:  section.headingBlock,
			Heading:  section.heading,
			Ref:      refs.BlockRef(reference, section.headingBlock),
			Text:     sliceMarkdown(markdown, section.start, section.end),
			Kind:     "agent",
			Unsure:   section.unsure,
			Replaced: section.replaced,
			Asks:     []string{},
			Links:    []model.DecisionLink{},
		}
		held := make(map[string]bool, len(section.blockIDs))
		for _, blockID := range section.blockIDs {
			held[blockID] = true
			if change, ok := changes[blockID]; ok && change.Version > decision.ChangedVersion {
				at := change.At
				decision.ChangedVersion, decision.ChangedAt = change.Version, &at
			}
			decision.Asks = append(decision.Asks, asks[blockID]...)
		}
		for _, link := range links {
			if !held[link.From.BlockID] {
				continue
			}
			decision.Links = append(decision.Links, model.DecisionLink{
				FromBlock:      link.From.BlockID,
				To:             link.To,
				CreatedVersion: link.CreatedVersion,
				CreatedAt:      link.CreatedAt,
				Stale:          staleLink(decision, link),
			})
		}
		if len(decision.Asks) > 0 {
			decision.Kind = "person"
		}
		if keep, ok := keeps[section.headingBlock]; ok {
			decision.Kept = &keep
		}
		decision.WaitingReview = decision.Kind == "agent" && !decision.Replaced &&
			(decision.Kept == nil || decision.Kept.ChangedVersion < decision.ChangedVersion)
		decisions = append(decisions, decision)
	}
	return decisions, nil
}

// staleLink is the out-of-date rule: the decision changed after it first linked the spec block,
// and that spec block has not changed since the decision did. A decision the block index has not
// reached has no changed version and marks nothing.
func staleLink(decision model.Decision, link model.BlockLink) bool {
	if decision.ChangedAt == nil || decision.ChangedVersion <= link.CreatedVersion {
		return false
	}
	return link.To.ChangedAt != nil && link.To.ChangedAt.Before(*decision.ChangedAt)
}

// decisionSections cuts the decisions document into its sections: each `##` heading outside
// `## Replaced`, and each `###` heading under it, with every block after the heading up to the
// next heading of its level or higher.
func (s *server) decisionSections(ctx context.Context, artifactID string) (string, []decisionSection, error) {
	markdown, blocks, err := s.deps.Docs.TextWithBlocks(ctx, artifactID)
	if err != nil {
		return "", nil, err
	}
	sort.SliceStable(blocks, func(a, b int) bool { return blocks[a].From < blocks[b].From })
	var sections []decisionSection
	var current *decisionSection
	inReplaced := false
	for _, block := range blocks {
		level, title := headingOf(markdown, block)
		if level > 0 {
			if current != nil && level <= current.level {
				sections = append(sections, *current)
				current = nil
			}
			if level <= 2 {
				inReplaced = level == 2 && title == replacedHeading
			}
			if current == nil {
				if !isDecisionHeading(level, inReplaced) {
					continue
				}
				current = &decisionSection{
					headingBlock: block.ID,
					heading:      title,
					level:        level,
					replaced:     inReplaced,
					blockIDs:     []string{block.ID},
					start:        block.From,
					end:          block.To,
				}
				continue
			}
		}
		if current == nil {
			continue
		}
		current.blockIDs = append(current.blockIDs, block.ID)
		if block.To > current.end {
			current.end = block.To
		}
		if block.Type == "paragraph" &&
			strings.HasPrefix(strings.TrimSpace(sliceMarkdown(markdown, block.From, block.To)), unsurePrefix) {
			current.unsure = true
		}
	}
	if current != nil {
		sections = append(sections, *current)
	}
	return markdown, sections, nil
}

// isDecisionHeading reports whether a heading of this level opens a decision: a `##` section
// outside `## Replaced`, or a `###` section under it.
func isDecisionHeading(level int, inReplaced bool) bool {
	if inReplaced {
		return level == 3
	}
	return level == 2
}

// headingOf reads a block's heading level and title from its own canonical markdown: `## Title` is
// level 2 with the title "Title". A block that is no heading answers level 0.
func headingOf(markdown string, block model.ArtifactBlock) (int, string) {
	if block.Type != "heading" {
		return 0, ""
	}
	text := strings.TrimSpace(sliceMarkdown(markdown, block.From, block.To))
	level := 0
	for level < len(text) && text[level] == '#' {
		level++
	}
	if level == 0 {
		return 0, ""
	}
	return level, strings.TrimSpace(text[level:])
}

// sliceMarkdown is markdown[from:to] held to the string's own bounds: a block range the document
// read gives lies inside it, and a range that does not answers "" rather than panicking.
func sliceMarkdown(markdown string, from, to int) string {
	if from < 0 || to > len(markdown) || from > to {
		return ""
	}
	return markdown[from:to]
}

// decisionBlockAsks is each block of the decisions document that mentions an ask, with the asks it
// mentions: the block index's own record of what the document's text cites.
func decisionBlockAsks(ctx context.Context, q queryer, artifactID string) (map[string][]string, error) {
	rows, err := q.Query(ctx, `
		select block_id, to_id from block_refs
		where artifact_id = $1::uuid and to_kind = 'ask' and kind = 'mentions'
		order by block_id, to_id
	`, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	asks := map[string][]string{}
	for rows.Next() {
		var blockID, askID string
		if err := rows.Scan(&blockID, &askID); err != nil {
			return nil, err
		}
		asks[blockID] = append(asks[blockID], askID)
	}
	return asks, rows.Err()
}

// decisionKeeps is every decision of the document a person has kept, by heading block.
func decisionKeeps(ctx context.Context, q queryer, artifactID string) (map[string]model.DecisionKeep, error) {
	rows, err := q.Query(ctx, `
		select block_id, changed_version, kept_by, kept_at
		from decision_keeps where artifact_id = $1::uuid
	`, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keeps := map[string]model.DecisionKeep{}
	for rows.Next() {
		keep := model.DecisionKeep{ArtifactID: artifactID}
		if err := rows.Scan(&keep.BlockID, &keep.ChangedVersion, &keep.KeptBy, &keep.KeptAt); err != nil {
			return nil, err
		}
		keeps[keep.BlockID] = keep
	}
	return keeps, rows.Err()
}

// keepDecision is POST /api/v1/artifacts/{id}/blocks/{block_id}/keep: the caller agrees with the
// decision that heading block opens, at the version its text stands at now. Any other block - one
// that is no decision heading, or a decision under `## Replaced`, which is no longer the design -
// is 404 TARGET_NOT_FOUND.
func (s *server) keepDecision(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	document, err := s.loadArtifact(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	blockID := r.PathValue("block_id")
	decision, err := s.keptDecision(r.Context(), document, blockID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	keep := model.DecisionKeep{ArtifactID: document.ID}
	if err := tx.QueryRow(r.Context(), `
		insert into decision_keeps (artifact_id, block_id, changed_version, kept_by)
		values ($1::uuid, $2, $3, $4)
		on conflict (artifact_id, block_id) do update
			set changed_version = excluded.changed_version, kept_by = excluded.kept_by, kept_at = now()
		returning block_id, changed_version, kept_by, kept_at
	`, document.ID, blockID, decision.ChangedVersion, model.CanonicalLogin(actor.ID)).
		Scan(&keep.BlockID, &keep.ChangedVersion, &keep.KeptBy, &keep.KeptAt); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, issueOwner(*document.IssueKey).event("decision.kept", actor, map[string]any{
		"artifact_id":     document.ID,
		"block_id":        blockID,
		"changed_version": keep.ChangedVersion,
	}))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	WriteJSON(w, http.StatusOK, keep)
}

// keptDecision is the decision blockID opens in document, refused unless document is an issue's
// decisions.md and that block is the heading of a decision outside `## Replaced`.
func (s *server) keptDecision(ctx context.Context, document model.Artifact, blockID string) (model.Decision, error) {
	notFound := errorf(http.StatusNotFound, "TARGET_NOT_FOUND", "no decision %q in %s", blockID, document.Name)
	if document.IssueKey == nil || document.Name != decisionsDocumentName {
		return model.Decision{}, notFound
	}
	decisions, err := s.readDecisions(ctx, document, artifactReference(document), "")
	if err != nil {
		return model.Decision{}, err
	}
	for _, decision := range decisions {
		if decision.BlockID == blockID && !decision.Replaced {
			return decision, nil
		}
	}
	return model.Decision{}, notFound
}

// getDocumentRead is GET /api/v1/me/documents/{id}/read: the caller's last read version of the
// document, null when they have read none. It is per-person state, as GET /api/v1/me/state is.
func (s *server) getDocumentRead(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	document, err := s.loadArtifact(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	read := model.DocumentRead{ArtifactID: document.ID}
	var version *int
	var at *time.Time
	err = s.deps.Store.Pool.QueryRow(r.Context(), `
		select version, read_at from user_document_reads where login = $1 and artifact_id = $2::uuid
	`, model.CanonicalLogin(actor.ID), document.ID).Scan(&version, &at)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.writeHandlerError(w, err)
		return
	}
	read.Version, read.ReadAt = version, at
	WriteJSON(w, http.StatusOK, read)
}

// putDocumentRead is PUT /api/v1/me/documents/{id}/read {"version": n}: the caller has read the
// document through that version. It is the caller's own view state, written as PUT
// /api/v1/me/issues/{key}/state is, and announces nothing.
func (s *server) putDocumentRead(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	var input struct {
		Version *int `json:"version"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.Version == nil || *input.Version < 0 {
		writeError(w, "INVALID_STATE", http.StatusBadRequest, "version must be a whole number that is not negative")
		return
	}
	document, err := s.loadArtifact(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	read := model.DocumentRead{ArtifactID: document.ID}
	var version int
	var at time.Time
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `
		insert into user_document_reads (login, artifact_id, version) values ($1, $2::uuid, $3)
		on conflict (login, artifact_id) do update set version = excluded.version, read_at = now()
		returning version, read_at
	`, model.CanonicalLogin(actor.ID), document.ID, *input.Version).Scan(&version, &at); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	read.Version, read.ReadAt = &version, &at
	WriteJSON(w, http.StatusOK, read)
}
