package refs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
// A reply whose chain of parents loops before any of them names a document has none. ScopeSources
// is its inverse, and the two must agree on every shape above (TestScopeOfAndScopeSourcesAgree).
func ScopeOf(ctx context.Context, q Queryer, kind, id string) (string, error) {
	if !isUUID(id) {
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
// that is not a uuid, has none.
func ScopeSources(ctx context.Context, q Queryer, doc string) ([]Target, error) {
	if !isUUID(doc) {
		return []Target{}, nil
	}
	sources, err := scanTargets(q.Query(ctx, scopeSourcesQuery, doc))
	if err != nil {
		return nil, fmt.Errorf("list the sources in the scope of document %s: %w", doc, err)
	}
	return sources, nil
}

// QuestionMap is the question map of the document doc: each question block id its latest version
// declares (a `:::ask{#id …}` line) to the reference a `dispatch://` address of its ask makes, as
// text.Extract reads that address, so a `#id` edge is stored as that address's edge is. The ask is
// the row settlement made for the block on doc, or, for a copied block, the ask settlement wrote
// into it as copied_from. A block with neither (not settled yet) is absent. It reads the latest
// version's directive lines, never parsing the document, and takes no lock.
func QuestionMap(ctx context.Context, q Queryer, doc string) (map[string]text.Ref, error) {
	source, err := LoadSource(ctx, q, "artifact", doc)
	if err != nil {
		return nil, fmt.Errorf("read the latest version of document %s: %w", doc, err)
	}
	if !source.Exists {
		return nil, nil
	}
	blocks := questionBlocks(source.Body)
	if len(blocks) == 0 {
		return nil, nil
	}
	ids := make([]string, len(blocks))
	for index, block := range blocks {
		ids[index] = block.id
	}
	questions, err := scanQuestions(q.Query(ctx, `
		select k.block_id, k.id::text, k.issue_key, a.project_key
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
		select k.id::text, k.id::text, k.issue_key, a.project_key
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

// scanQuestions reads rows of (key, ask id, issue key, project key) into each ask's reference by
// key: an ask on an issue as `dispatch://<KEY>/ask/<id>` reads, one on a project document as
// `dispatch://<PROJECT>/artifact/<slug>/ask/<id>` does.
func scanQuestions(rows pgx.Rows, err error) (map[string]text.Ref, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := map[string]text.Ref{}
	for rows.Next() {
		var key, id string
		var issueKey, project *string
		if err := rows.Scan(&key, &id, &issueKey, &project); err != nil {
			return nil, err
		}
		ref := text.Ref{Kind: "ask", ID: id}
		if issueKey != nil {
			ref.IssueKey = *issueKey
		} else if project != nil {
			ref.Project = *project
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
// with three or more colons and holds `#id` among its attributes.
func questionBlocks(markdown string) []questionBlock {
	blocks := []questionBlock{}
	seen := map[string]struct{}{}
	for line := range strings.Lines(markdown) {
		block, ok := askDirective(line)
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

// askDirective reads a `:::ask{…}` line's id and copied_from by the attribute grammar of
// pmdoc's parseDirectiveAttributes: `#` and a run of [A-Za-z0-9_-] is the id, `name="value"` an
// attribute whose value runs to the next unescaped quote.
func askDirective(line string) (questionBlock, bool) {
	rest := trimContainerMarkers(line)
	colons := 0
	for colons < len(rest) && rest[colons] == ':' {
		colons++
	}
	if colons < 3 {
		return questionBlock{}, false
	}
	rest, ok := strings.CutPrefix(rest[colons:], "ask{")
	if !ok {
		return questionBlock{}, false
	}
	attrs, ok := strings.CutSuffix(strings.TrimRight(rest, " \t\r\n"), "}")
	if !ok {
		return questionBlock{}, false
	}
	var block questionBlock
	for offset := 0; offset < len(attrs); {
		switch {
		case attrs[offset] == '#':
			start := offset + 1
			offset = start
			for offset < len(attrs) && text.IsBlockIDByte(attrs[offset]) {
				offset++
			}
			if block.id == "" {
				block.id = attrs[start:offset]
			}
		case text.IsBlockIDByte(attrs[offset]):
			start := offset
			for offset < len(attrs) && text.IsBlockIDByte(attrs[offset]) {
				offset++
			}
			name := attrs[start:offset]
			if offset+1 >= len(attrs) || attrs[offset] != '=' || attrs[offset+1] != '"' {
				continue
			}
			offset += 2
			valueStart := offset
			escaped := false
			for offset < len(attrs) && (attrs[offset] != '"' || escaped) {
				escaped = attrs[offset] == '\\' && !escaped
				offset++
			}
			if parsed, err := uuid.Parse(attrs[valueStart:offset]); err == nil && name == "copied_from" {
				block.copiedFrom = parsed.String()
			}
			offset++
		default:
			offset++
		}
	}
	return block, block.id != ""
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

func isUUID(id string) bool {
	_, err := uuid.Parse(id)
	return err == nil
}

// resolver is text.Resolver for the text of one reference source, read through q, the write's
// transaction: a bare key names an issue that exists there, and a `#id` the question of that id
// in the source's scope (ScopeOf, QuestionMap), which is read only when the text holds a `#id`.
// A resolver made with the question map of a document (NewScopedResolver) answers that map, unread
// again, while the source's scope is still that document.
//
// It remembers its answers: a write that reads the same text's mentions twice in its transaction -
// its index, then a document's excerpts (ReplaceCountedWith, MentionExcerpts) - asks the database
// once. Nothing between those reads writes an issue, an ask, a version or a source's scope, so the
// second read's answer is the first's.
type resolver struct {
	ctx       context.Context
	q         Queryer
	fromKind  string
	fromID    string
	scope     string
	questions map[string]text.Ref

	// issueKeys and issues are the last Issues call's keys and answer, while issuesRead says
	// there was one.
	issueKeys  []string
	issues     map[string]bool
	issuesRead bool
	// ownQuestions is Questions' first answer, while questionsRead says there was one; it is kept
	// apart from questions, the map a NewScopedResolver was made with.
	ownQuestions  map[string]text.Ref
	questionsRead bool
}

// NewResolver is the text.Resolver for the text the source (fromKind, fromID) writes, read through
// q: the transaction that writes it, or the pool for a reader outside one.
func NewResolver(ctx context.Context, q Queryer, fromKind, fromID string) text.Resolver {
	return &resolver{ctx: ctx, q: q, fromKind: fromKind, fromID: fromID}
}

// NewScopedResolver is NewResolver for a source of the scope of the document doc whose question map,
// questions, its caller read once for every source of that scope it re-indexes (docs.Service's
// drain of a scope row), so a scope of many citing sources reads the map once and not once each.
// The source's scope is still read through q, in the source's own transaction: a source moved out
// of doc's scope since its caller listed it (a comment anchored elsewhere) resolves against its
// own scope's map, as NewResolver's does, and never against doc's.
func NewScopedResolver(ctx context.Context, q Queryer, fromKind, fromID, doc string, questions map[string]text.Ref) text.Resolver {
	return &resolver{ctx: ctx, q: q, fromKind: fromKind, fromID: fromID, scope: doc, questions: questions}
}

func (r *resolver) Issues(keys []string) (map[string]bool, error) {
	if r.issuesRead && slices.Equal(r.issueKeys, keys) {
		return r.issues, nil
	}
	issues, err := existingIssues(r.ctx, r.q, keys)
	if err != nil {
		return nil, err
	}
	r.issueKeys, r.issues, r.issuesRead = slices.Clone(keys), issues, true
	return issues, nil
}

func (r *resolver) Questions() (map[string]text.Ref, error) {
	if r.questionsRead {
		return r.ownQuestions, nil
	}
	questions, err := r.readQuestions()
	if err != nil {
		return nil, err
	}
	r.ownQuestions, r.questionsRead = questions, true
	return questions, nil
}

func (r *resolver) readQuestions() (map[string]text.Ref, error) {
	doc, err := ScopeOf(r.ctx, r.q, r.fromKind, r.fromID)
	if err != nil || doc == "" {
		return nil, err
	}
	if doc == r.scope {
		return r.questions, nil
	}
	return QuestionMap(r.ctx, r.q, doc)
}
