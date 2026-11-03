package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
// A spec block X linked from decision D by link L is out of date exactly when D's own text
// changed after it first linked X and X has not changed since D did, both read from the
// document's text at a version (markStaleLinks) rather than from D's changed version, which is
// the greatest of the blocks D holds now and so does not move when a block leaves D or joins it.
// A new decision therefore marks nothing, and neither does the block index's first pass over a
// document, which gives every block and every pair one version.
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
	keys := make([]sectionKey, 0, len(sections))
	seen := map[string]int{}
	for _, section := range sections {
		keys = append(keys, sectionKey{heading: section.heading, occurrence: seen[section.heading]})
		seen[section.heading]++
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
			})
		}
		if len(decision.Asks) > 0 {
			decision.Kind = "person"
		}
		if keep, ok := keeps[section.headingBlock]; ok {
			decision.Kept = &keep
		}
		decision.TextSHA256 = decisionTextSum(decision.Text)
		decision.WaitingReview = decision.Kind == "agent" && !decision.Replaced &&
			(decision.Kept == nil || decision.Kept.TextSHA256 != decision.TextSHA256)
		decisions = append(decisions, decision)
	}
	if err := s.markStaleLinks(ctx, document.ID, markdown, decisions, keys); err != nil {
		return nil, err
	}
	return decisions, nil
}

// markStaleLinks decides each link's `stale`: the decision's own text changed after it first
// linked the spec block, and that spec block has not changed since the decision did.
//
// Both halves are answered by the decision's text at a version, never by its changed version:
// that version is the greatest of the blocks the decision holds now, so a block removed from the
// decision, or moved into or out of it, changes the text and leaves the version where it was
// (the reviewer's measurement on #2626). The text at the version that created the link says
// whether the decision changed since; the text at the version current when the spec block last
// changed says whether it changed since that, which is what clears a link the spec answered.
//
// A version this document does not hold, and a spec block Dispatch records no change for, each
// leave the link as it is: the view marks nothing it cannot read, as the block index's first
// pass over a document marks nothing. Each decision is named across versions by its heading and
// its occurrence among the decisions of that heading (sectionKey), the same pair the view's own
// walk gives it, so a document that carries one heading twice still names each of them.
func (s *server) markStaleLinks(
	ctx context.Context, artifactID, markdown string, decisions []model.Decision, keys []sectionKey,
) error {
	times := map[int64]time.Time{}
	numbers := map[int]bool{}
	for _, decision := range decisions {
		for _, link := range decision.Links {
			numbers[link.CreatedVersion] = true
			if link.To.ChangedAt != nil {
				times[link.To.ChangedAt.UnixNano()] = *link.To.ChangedAt
			}
		}
	}
	if len(numbers) == 0 {
		return nil
	}
	atTime, err := s.versionsAtTimes(ctx, artifactID, sortedTimes(times))
	if err != nil {
		return err
	}
	for _, number := range atTime {
		numbers[number] = true
	}
	sums, err := s.decisionSumsAt(ctx, artifactID, sortedNumbers(numbers))
	if err != nil {
		return err
	}
	current := decisionSums(markdown)
	for i := range decisions {
		decision := &decisions[i]
		now, ok := current[keys[i]]
		if !ok {
			continue
		}
		for j := range decision.Links {
			decision.Links[j].Stale = staleLink(now, keys[i], decision.Links[j], sums, atTime)
		}
	}
	return nil
}

// staleLink is that rule for one link, over the sums markStaleLinks read. A time keys those
// reads by its own instant (UnixNano), since a time read back from Postgres is the same instant
// in another `time.Time` value, which no map matches.
//
// A version this document does not hold leaves the link as it is. A version it holds that gives
// this decision no sum - its heading was renamed since, or that version carried fewer decisions
// of that heading - is text that differs from the text now, which it is.
func staleLink(
	now string,
	key sectionKey,
	link model.DecisionLink,
	sums map[int]map[sectionKey]string,
	atTime map[int64]int,
) bool {
	atCreated, read := sums[link.CreatedVersion]
	if !read || atCreated[key] == now {
		return false
	}
	if link.To.ChangedAt == nil {
		return false
	}
	number, asked := atTime[link.To.ChangedAt.UnixNano()]
	if !asked {
		return false
	}
	if number == 0 {
		// The spec block last changed before this document held a version of its own, so every
		// change of the decision came after it.
		return true
	}
	atChange, read := sums[number]
	return read && atChange[key] != now
}

// versionsAtTimes is, for each time, the document's greatest version written at or before it:
// the version of decisions.md that stood when a spec block changed, keyed by the time's own
// instant. A time before the document's first version answers 0.
func (s *server) versionsAtTimes(
	ctx context.Context, artifactID string, times []time.Time,
) (map[int64]int, error) {
	at := map[int64]int{}
	if len(times) == 0 {
		return at, nil
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select asked.ord, version.number
		from unnest($2::timestamptz[]) with ordinality as asked(at, ord)
		left join lateral (
			select v.number from artifact_versions v
			where v.artifact_id = $1::uuid and v.created_at <= asked.at
			order by v.number desc limit 1
		) version on true
	`, artifactID, times)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ordinal int
		var number *int
		if err := rows.Scan(&ordinal, &number); err != nil {
			return nil, err
		}
		if ordinal < 1 || ordinal > len(times) {
			continue
		}
		at[times[ordinal-1].UnixNano()] = 0
		if number != nil {
			at[times[ordinal-1].UnixNano()] = *number
		}
	}
	return at, rows.Err()
}

// decisionSumsAt is each named version's decisions, with the sum of each one's text: the
// version's own stored markdown, cut by the same rule as the document as it stands.
func (s *server) decisionSumsAt(
	ctx context.Context, artifactID string, numbers []int,
) (map[int]map[sectionKey]string, error) {
	sums := map[int]map[sectionKey]string{}
	if len(numbers) == 0 {
		return sums, nil
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select number, coalesce(markdown, '') from artifact_versions
		where artifact_id = $1::uuid and number = any($2::int[])
	`, artifactID, numbers)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var number int
		var markdown string
		if err := rows.Scan(&number, &markdown); err != nil {
			return nil, err
		}
		sums[number] = decisionSums(markdown)
	}
	return sums, rows.Err()
}

// decisionSums cuts a document's markdown into its decisions and sums each one's text. It cuts
// by the rule the block walk cuts the document that stands by (decisionSections): a `##` heading
// outside `## Replaced` and a `###` heading under it opens a decision, a heading of its own
// level or higher closes it - `#` included, which opens none - and a deeper heading is text of
// the decision it stands in.
//
// A stored version holds no block id, so a decision is keyed by its heading and by how many
// decisions before it carry that heading (sectionKey): a document that carries one heading twice,
// which a replaced decision made again does, is two decisions here as it is two in the view,
// rather than one the sum cannot name. Its sum is taken over the section's text with the blank
// lines at its end removed, so the same section of two versions sums alike whatever follows it.
func decisionSums(markdown string) map[sectionKey]string {
	sums := map[sectionKey]string{}
	seen := map[string]int{}
	type section struct {
		key   sectionKey
		level int
		start int
	}
	var open *section
	inReplaced := false
	fenced := ""
	offset := 0
	closeAt := func(end int) {
		if open == nil {
			return
		}
		sums[open.key] = decisionTextSum(strings.TrimRight(markdown[open.start:end], "\n"))
		open = nil
	}
	for _, line := range strings.SplitAfter(markdown, "\n") {
		if line == "" {
			break
		}
		trimmed := strings.TrimRight(line, "\n")
		if fence := codeFence(trimmed); fence != "" {
			if fenced == "" {
				fenced = fence
			} else if strings.HasPrefix(fence, fenced[:1]) && len(fence) >= len(fenced) {
				fenced = ""
			}
			offset += len(line)
			continue
		}
		if fenced == "" {
			if level, heading := markdownHeading(trimmed); level > 0 {
				if open != nil && level <= open.level {
					closeAt(offset)
				}
				if level <= 2 {
					inReplaced = level == 2 && heading == replacedHeading
				}
				if open == nil && isDecisionHeading(level, inReplaced) {
					occurrence := seen[heading]
					seen[heading] = occurrence + 1
					open = &section{
						key:   sectionKey{heading: heading, occurrence: occurrence},
						level: level,
						start: offset,
					}
				}
			}
		}
		offset += len(line)
	}
	closeAt(len(markdown))
	return sums
}

// sectionKey names one decision of a document across its versions: its heading, and how many
// decisions before it carry that heading.
type sectionKey struct {
	heading    string
	occurrence int
}

// codeFence is the fence a line opens or closes a fenced code block with (three or more
// backticks or tildes, at most three spaces in), else "". A heading inside such a block is the
// code's own text.
func codeFence(line string) string {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return ""
	}
	for _, marker := range []string{"```", "~~~"} {
		if strings.HasPrefix(trimmed, marker) {
			run := 0
			for run < len(trimmed) && trimmed[run] == marker[0] {
				run++
			}
			return trimmed[:run]
		}
	}
	return ""
}

// markdownHeading is a line's ATX heading level and title, or level 0 for a line that is no
// heading. It reads a title as `headingOf` reads one off a heading block's own markdown, closing
// hashes and all, so the heading a version is keyed by is the heading the view names.
func markdownHeading(line string) (int, string) {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level >= len(line) || line[level] != ' ' {
		return 0, ""
	}
	return level, strings.TrimSpace(line[level:])
}

// sortedNumbers and sortedTimes are the keys of a set, in order, so one read asks for each once.
func sortedNumbers(set map[int]bool) []int {
	numbers := make([]int, 0, len(set))
	for number := range set {
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)
	return numbers
}

func sortedTimes(set map[int64]time.Time) []time.Time {
	times := make([]time.Time, 0, len(set))
	for _, at := range set {
		times = append(times, at)
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	return times
}

// decisionTextSum is the sum a Keep records and the view compares: the decision's whole text, so
// a block removed from the decision, or moved into or out of it, asks for review again as a
// reworded block does. The decision's changed version cannot do that work, since it is the
// greatest of the blocks the decision holds now and a removal leaves every remaining block at
// the version it had.
func decisionTextSum(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
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
		select block_id, text_sha256, changed_version, kept_by, kept_at
		from decision_keeps where artifact_id = $1::uuid
	`, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keeps := map[string]model.DecisionKeep{}
	for rows.Next() {
		keep := model.DecisionKeep{ArtifactID: artifactID}
		if err := rows.Scan(&keep.BlockID, &keep.TextSHA256, &keep.ChangedVersion,
			&keep.KeptBy, &keep.KeptAt); err != nil {
			return nil, err
		}
		keeps[keep.BlockID] = keep
	}
	return keeps, rows.Err()
}

// keepDecision is POST /api/v1/artifacts/{id}/blocks/{block_id}/keep {"text_sha256": "<sum>"}:
// the caller agrees with the decision that heading block opens, as the text they read it at.
// The sum is the `text_sha256` the view served them, and a decision whose text has changed
// since is 409 DECISION_CHANGED with nothing written: a Keep over text nobody read is the one
// thing Keep exists to prevent, as `409 ASK_EDITED` holds an answer to the wording it was given
// on. Any other block - one that is no decision heading, or a decision under `## Replaced`,
// which is no longer the design - is 404 TARGET_NOT_FOUND.
func (s *server) keepDecision(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	var input struct {
		TextSHA256 string `json:"text_sha256"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.TextSHA256 == "" {
		writeError(w, "INVALID_KEEP", http.StatusBadRequest,
			"text_sha256 is the text_sha256 the decision was read at")
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
	if decision.TextSHA256 != input.TextSHA256 {
		writeError(w, "DECISION_CHANGED", http.StatusConflict,
			"the decision changed since it was read; read it again and keep what it says now")
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
		insert into decision_keeps (artifact_id, block_id, text_sha256, changed_version, kept_by)
		values ($1::uuid, $2, $3, $4, $5)
		on conflict (artifact_id, block_id) do update
			set text_sha256 = excluded.text_sha256, changed_version = excluded.changed_version,
				kept_by = excluded.kept_by, kept_at = now()
		returning block_id, text_sha256, changed_version, kept_by, kept_at
	`, document.ID, blockID, decisionTextSum(decision.Text), decision.ChangedVersion,
		model.CanonicalLogin(actor.ID)).
		Scan(&keep.BlockID, &keep.TextSHA256, &keep.ChangedVersion, &keep.KeptBy, &keep.KeptAt); err != nil {
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

// keptDecision is the decision blockID opens in document, refused unless document is the
// decisions.md of its issue - resolved as the view resolves it, by slug and then by filename, so
// Keep and the view never disagree about which document holds the decisions - and that block is
// the heading of a decision outside `## Replaced`.
func (s *server) keptDecision(ctx context.Context, document model.Artifact, blockID string) (model.Decision, error) {
	notFound := errorf(http.StatusNotFound, "TARGET_NOT_FOUND", "no decision %q in %s", blockID, document.Name)
	if document.IssueKey == nil {
		return model.Decision{}, notFound
	}
	decisionsDocument, found, err := s.issueDecisionsDocument(ctx, *document.IssueKey)
	if err != nil {
		return model.Decision{}, err
	}
	if !found || decisionsDocument.ID != document.ID {
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
