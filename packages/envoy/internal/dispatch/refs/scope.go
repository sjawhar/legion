package refs

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// askScope is the document an ask (aliased k) belongs to: the one its block is on, the one it is
// anchored to, or the one that owns it, else its issue's spec (ks, askScopeJoin).
const askScope = `coalesce(k.block_artifact_id, (k.anchor->>'artifact_id')::uuid, k.artifact_id, ks.id)`

const askScopeJoin = `left join artifacts ks on ks.issue_key = k.issue_key and ks.is_primary`

// commentOwnScope is the document a comment (aliased c) names itself: the one it is anchored to,
// the one that owns it, or, for a reply to an ask, that ask's scope (commentOwnScopeJoin). A
// comment naming none is in its parent's scope when it is a reply in a thread, else in its issue's
// spec.
const commentOwnScope = `coalesce((c.anchor->>'artifact_id')::uuid, c.artifact_id, ` + askScope + `)`

const commentOwnScopeJoin = `left join asks k on k.id = c.ask_id ` + askScopeJoin

// ScopeOf is the id of the document the text of one reference source belongs to, the document a
// `#id` in that text resolves against (QuestionMap), or "" when it belongs to none:
//
//   - a document version (kind "artifact"): that document;
//   - an ask: the document its block is on, else the one it is anchored to, else the one that owns
//     it, else its issue's spec;
//   - a comment: the document it is anchored to, else the one that owns it, else for a reply to an
//     ask that ask's scope, else for a reply in a thread its parent's scope (so its thread root's),
//     else its issue's spec;
//   - an issue message: its issue's spec;
//   - a direct message (no issue), any other kind, and an id that is not a uuid: none.
//
// An id written in one of uuid.Parse's other spellings is read, and answered, as the canonical
// one (canonicalUUID).
//
// A reply whose chain of parents loops before any of them names a document has none. ScopeSources
// is its inverse, and the two must agree on every shape above (TestScopeOfAndScopeSourcesAgree).
func ScopeOf(ctx context.Context, q Queryer, kind, id string) (string, error) {
	id, ok := canonicalUUID(id)
	if !ok {
		return "", nil
	}
	switch kind {
	case "artifact":
		return id, nil
	case "ask":
		return scopeRow(q.QueryRow(ctx, `
			select `+askScope+`::text from asks k `+askScopeJoin+` where k.id = $1
		`, id))
	case "comment":
		return commentScope(ctx, q, id)
	case "message":
		return scopeRow(q.QueryRow(ctx, `
			select s.id::text from messages m
			join artifacts s on s.issue_key = m.issue_key and s.is_primary
			where m.id = $1
		`, id))
	}
	return "", nil
}

// commentScope walks a comment's chain of parents up to the first comment that names a document
// (commentOwnScope), or to the thread's root, whose issue's spec it then is.
func commentScope(ctx context.Context, q Queryer, id string) (string, error) {
	seen := map[string]struct{}{}
	for {
		if _, loops := seen[id]; loops {
			return "", nil
		}
		seen[id] = struct{}{}
		var own, parent, spec *string
		err := q.QueryRow(ctx, `
			select `+commentOwnScope+`::text, c.reply_to::text, cs.id::text
			from comments c `+commentOwnScopeJoin+`
			left join artifacts cs on cs.issue_key = c.issue_key and cs.is_primary
			where c.id = $1
		`, id).Scan(&own, &parent, &spec)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("read the scope of comment %s: %w", id, err)
		}
		switch {
		case own != nil:
			return *own, nil
		case parent != nil:
			id = *parent
		case spec != nil:
			return *spec, nil
		default:
			return "", nil
		}
	}
}

func scopeRow(row pgx.Row) (string, error) {
	var scope *string
	if err := row.Scan(&scope); errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	} else if err != nil {
		return "", fmt.Errorf("read a reference source's scope: %w", err)
	}
	if scope == nil {
		return "", nil
	}
	return *scope, nil
}

// scopeSourcesQuery lists, by kind and id, every reference source whose ScopeOf is the document $1.
// The comments are found from the thread roots and the comments that name the document
// (commentOwnScope) down through the replies that name none, which is ScopeOf's walk run the
// other way; a loop of replies naming no document is reached from neither.
const scopeSourcesQuery = `
	with recursive
	spec as (
		select issue_key from artifacts where id = $1 and is_primary
	),
	scoped_asks as (
		select k.id from asks k ` + askScopeJoin + `
		where (coalesce(k.block_artifact_id, (k.anchor->>'artifact_id')::uuid) = $1
		       or k.artifact_id = $1
		       or k.issue_key in (select issue_key from spec))
		  and ` + askScope + ` = $1
	),
	scoped_comments(id) as (
		select c.id from comments c ` + commentOwnScopeJoin + `
		left join artifacts cs on cs.issue_key = c.issue_key and cs.is_primary
		where ((c.anchor->>'artifact_id')::uuid = $1
		       or c.artifact_id = $1
		       or c.ask_id in (select id from scoped_asks)
		       or c.issue_key in (select issue_key from spec))
		  and (` + commentOwnScope + ` = $1
		       or (` + commentOwnScope + ` is null and c.reply_to is null and cs.id = $1))
		union
		select c.id from scoped_comments p
		join comments c on c.reply_to = p.id ` + commentOwnScopeJoin + `
		where ` + commentOwnScope + ` is null
	)
	select kind, id::text from (
		select 'artifact' as kind, id from artifacts where id = $1
		union all
		select 'ask', id from scoped_asks
		union all
		select 'comment', id from scoped_comments
		union all
		select 'message', m.id from messages m where m.issue_key in (select issue_key from spec)
	) sources
	order by kind, id`

// ScopeSources is the inverse of ScopeOf: every reference source, by kind and id, whose text
// belongs to the document doc, in kind then id order. A document that does not exist, or an id
// that is not a uuid, has none; one written in another of uuid.Parse's spellings is read as the
// canonical one (canonicalUUID).
func ScopeSources(ctx context.Context, q Queryer, doc string) ([]Target, error) {
	doc, ok := canonicalUUID(doc)
	if !ok {
		return []Target{}, nil
	}
	sources, err := scanTargets(q.Query(ctx, scopeSourcesQuery, doc))
	if err != nil {
		return nil, fmt.Errorf("list the sources in the scope of document %s: %w", doc, err)
	}
	return sources, nil
}

// QuestionMap is the question map of the document doc: each question block id its latest version
// declares (a `:::ask{#id …}` line outside fenced code) to the reference a `dispatch://` address of
// its ask makes, as text.Extract reads that address, so a `#id` edge is stored as that address's
// edge is. The ask is the row settlement made for the block on doc, or, for a copied block, the ask
// settlement wrote into it as copied_from. A block with neither (not settled yet) is absent. It
// reads the latest version's directive lines (sourceText), never parsing the document, and takes
// no lock.
func QuestionMap(ctx context.Context, q Queryer, doc string) (map[string]text.Ref, error) {
	markdown, _, found, err := sourceText(ctx, q, "artifact", doc, false)
	if err != nil {
		return nil, fmt.Errorf("read the latest version of document %s: %w", doc, err)
	}
	if !found {
		return nil, nil
	}
	blocks := questionBlocks(markdown)
	if len(blocks) == 0 {
		return nil, nil
	}
	ids := make([]string, len(blocks))
	for index, block := range blocks {
		ids[index] = block.id
	}
	questions, err := scanQuestions(q.Query(ctx, `
		select k.block_id, k.id::text, k.issue_key, a.project_key, a.slug
		from asks k join artifacts a on a.id = k.block_artifact_id
		where k.block_artifact_id = $1 and k.block_id = any($2::text[])
	`, doc, ids))
	if err != nil {
		return nil, fmt.Errorf("read the questions of document %s: %w", doc, err)
	}
	copiedBy := map[string][]string{}
	sources := []string{}
	for _, block := range blocks {
		if _, own := questions[block.id]; own || block.copiedFrom == "" {
			continue
		}
		if _, listed := copiedBy[block.copiedFrom]; !listed {
			sources = append(sources, block.copiedFrom)
		}
		copiedBy[block.copiedFrom] = append(copiedBy[block.copiedFrom], block.id)
	}
	if len(sources) == 0 {
		return questions, nil
	}
	copied, err := scanQuestions(q.Query(ctx, `
		select k.id::text, k.id::text, k.issue_key, a.project_key, coalesce(a.slug, '')
		from asks k
		left join artifacts a on a.id = coalesce(k.block_artifact_id, (k.anchor->>'artifact_id')::uuid, k.artifact_id)
		where k.id = any($1::uuid[])
	`, sources))
	if err != nil {
		return nil, fmt.Errorf("read the copied questions of document %s: %w", doc, err)
	}
	for source, ref := range copied {
		for _, blockID := range copiedBy[source] {
			questions[blockID] = ref
		}
	}
	return questions, nil
}

// AskRef is the `dispatch://` address of one ask a question map holds, as every writer of an
// ask's address writes it (itemRef): under its issue, or under the project document that owns
// it. It is "" for an ask no address names - one on an agent conversation's document - which is
// still the ask the `#id` names, stored as the edge its id makes.
func AskRef(ref text.Ref) string {
	if ref.IssueKey != "" {
		return itemRef("ask", &ref.IssueKey, "", "", ref.ID)
	}
	return itemRef("ask", nil, ref.Project, ref.Document, ref.ID)
}

// scanQuestions reads rows of (key, ask id, issue key, project key, document slug) into each
// ask's reference by key: an ask on an issue as `dispatch://<KEY>/ask/<id>` reads, one on a
// project document as `dispatch://<PROJECT>/artifact/<slug>/ask/<id>` does (itemRef writes
// both, and the document of the second is the owner its reference names). An ask whose document
// is an agent conversation's has no address (itemRef answers none); it is still the ask the
// `#id` names, and the edge stored for it is the one its id makes.
func scanQuestions(rows pgx.Rows, err error) (map[string]text.Ref, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := map[string]text.Ref{}
	for rows.Next() {
		var key, id, slug string
		var issueKey, project *string
		if err := rows.Scan(&key, &id, &issueKey, &project, &slug); err != nil {
			return nil, err
		}
		ref := text.Ref{Kind: "ask", ID: id}
		if issueKey != nil {
			ref.IssueKey = *issueKey
		} else if project != nil {
			ref.Project, ref.Document = *project, slug
		}
		found[key] = ref
	}
	return found, rows.Err()
}

// questionBlock is one question block a document's text declares: its id and, for a copy, the
// uuid of the ask settlement wrote into it as copied_from.
type questionBlock struct {
	id, copiedFrom string
}

// questionBlocks is every question block markdown declares, in document order, the first of each
// id: each line that, past its containers' quote and list markers, opens a typed block named ask
// holding an id, outside fenced code. A fenced example of a directive is code to pmdoc, so a
// `copied_from` it carries names no question of this document.
func questionBlocks(markdown string) []questionBlock {
	blocks := []questionBlock{}
	seen := map[string]struct{}{}
	var fence codeFence
	for line := range strings.Lines(markdown) {
		rest := trimContainerMarkers(line)
		if fence.run > 0 {
			if fence.closedBy(rest) {
				fence = codeFence{}
			}
			continue
		}
		if fence = openingFence(rest); fence.run > 0 {
			continue
		}
		block, ok := askBlock(rest)
		if !ok {
			continue
		}
		if _, repeated := seen[block.id]; repeated {
			continue
		}
		seen[block.id] = struct{}{}
		blocks = append(blocks, block)
	}
	return blocks
}

// codeFence is the fence a fenced code block opened with: its character, a backtick or a tilde,
// and how many of it the run holds, none when no fence is open.
type codeFence struct {
	char byte
	run  int
}

// openingFence is the fence text, a line past its containers' markers, opens, as CommonMark reads
// one: a run of three or more backticks or tildes, a backtick run followed by no other backtick
// (such a line is an inline code span). Text that opens none is the zero codeFence.
func openingFence(text string) codeFence {
	fence := fenceRun(text)
	if fence.run < 3 || fence.char == '`' && strings.ContainsRune(text[fence.run:], '`') {
		return codeFence{}
	}
	return fence
}

// closedBy reports whether text, a line past its containers' markers, closes the fence: a run of
// its character at least as long, followed by nothing but whitespace. A fence nothing closes runs
// to the document's end.
func (fence codeFence) closedBy(text string) bool {
	closer := fenceRun(text)
	return closer.char == fence.char && closer.run >= fence.run && strings.TrimRight(text[closer.run:], " \t\r\n") == ""
}

// fenceRun is the run of backticks or tildes text opens with, if any.
func fenceRun(text string) codeFence {
	if text == "" || text[0] != '`' && text[0] != '~' {
		return codeFence{}
	}
	run := 1
	for run < len(text) && text[run] == text[0] {
		run++
	}
	return codeFence{char: text[0], run: run}
}

// askBlock is the question block one line opens, the line past its containers' markers
// (trimContainerMarkers) and without its line ending, as pmdoc's parser is passed it: a typed
// block named ask that holds an id, with the uuid of the ask its `copied_from` names where it
// carries one. ok is false for every other line.
//
// The line is read by the one parser of a typed block's opening line
// (pmdoc.ParseTypedDirectiveOpen), never by a grammar of its own, so this reads a line exactly
// as the document's own parse reads it: an id written either way (`#id` and `id="…"`,
// pmdoc.TypedBlockIDAttribute), and no block at all on a line that parser refuses. What stays
// here is the line and fence scan alone (questionBlocks): refs parses no document.
func askBlock(line string) (questionBlock, bool) {
	name, attrs, ok := pmdoc.ParseTypedDirectiveOpen(strings.TrimRight(line, "\r\n"))
	if !ok || name != "ask" {
		return questionBlock{}, false
	}
	id, _ := attrs[pmdoc.BlockIDAttr].(string)
	if id == "" {
		return questionBlock{}, false
	}
	block := questionBlock{id: id}
	// Only the one attribute carries an ask: every other value on the line - the state, the
	// asker, the question's own wording - is left unread.
	if copied, ok := attrs["copied_from"].(string); ok {
		if parsed, err := uuid.Parse(copied); err == nil {
			block.copiedFrom = parsed.String()
		}
	}
	return block, true
}

// trimContainerMarkers drops the indentation, quote markers (`>`) and list markers (`-`, `*`, `+`,
// or a number and `.` or `)`, each followed by a space) a block nested in a quote or a list item
// is written after.
func trimContainerMarkers(line string) string {
	for {
		line = strings.TrimLeft(line, " \t")
		switch {
		case strings.HasPrefix(line, ">"):
			line = line[1:]
		case len(line) > 1 && (line[0] == '-' || line[0] == '*' || line[0] == '+') && (line[1] == ' ' || line[1] == '\t'):
			line = line[2:]
		default:
			digits := 0
			for digits < len(line) && line[digits] >= '0' && line[digits] <= '9' {
				digits++
			}
			if digits == 0 || digits+1 >= len(line) || (line[digits] != '.' && line[digits] != ')') || (line[digits+1] != ' ' && line[digits+1] != '\t') {
				return line
			}
			line = line[digits+2:]
		}
	}
}

// canonicalUUID is id in the one spelling the graph and Postgres read it by, and false for a
// value that is no uuid at all. uuid.Parse reads `urn:uuid:<uuid>`, the braced spelling and 32
// hex digits beside the canonical one, and every id this package stores, binds and answers with
// is the canonical one: Postgres refuses `urn:uuid:` on a uuid column, so a value bound as
// written would fail the read where the id it names answers it, and a document answered back in
// another spelling would name a node nothing else writes.
func canonicalUUID(id string) (string, bool) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return "", false
	}
	return parsed.String(), true
}
