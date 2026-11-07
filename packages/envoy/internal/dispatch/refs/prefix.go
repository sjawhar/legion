package refs

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// prefixCandidateLimit bounds what one ResolvePrefix read loads: it has to tell one match from
// more than one and list the matches for a person to choose from, and ten is as long a list as
// anyone reads before writing more of the id. A prefix more items carry is listed to this bound.
const prefixCandidateLimit = 10

// prefixLimit is prefixCandidateLimit as the `limit` of every query of this file.
var prefixLimit = strconv.Itoa(prefixCandidateLimit)

// Candidate is one item an id prefix names, as ResolvePrefix answers with and
// ErrPrefixAmbiguous lists; the API serialises it.
//
// ID is the whole id the prefix shortens: the item's uuid for an ask, a comment, a message and a
// document, and the block id for a block, which is unique within its document alone - Ref
// addresses it. Ref is the whole `dispatch://` reference, written the way text writes one
// (itemRef, ArtifactRef, BlockRef); it is empty for an item that has no address, a direct
// message or a document of an agent's conversation.
//
// IssueKey and Document are the candidate's owner, at most one of them: the issue that owns it,
// or the reference of the project document it is written on where no issue owns it. A block
// takes its document's owner, and a document candidate of a project sets neither, its own Ref
// being that document. An item with no owner of either kind - a direct message - sets neither.
//
// FirstLine is the head of what a person reads to tell two candidates apart, as
// contracts.OneLineSummary cuts it: an ask's question, a comment's or a message's body, a
// document's name, a block's text. A direct message's is left out for a caller that may not read
// the conversation (Viewer).
type Candidate struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	Ref       string `json:"ref"`
	IssueKey  string `json:"issue_key,omitempty"`
	Document  string `json:"document,omitempty"`
	FirstLine string `json:"first_line,omitempty"`
}

// ErrPrefixAmbiguous is the refusal of an id prefix two items or more carry, with the matches for
// the caller to choose from, in id order. Candidates holds at most prefixCandidateLimit of them:
// a prefix more items carry is refused with that many listed.
type ErrPrefixAmbiguous struct {
	Prefix     string
	Candidates []Candidate
}

func (e *ErrPrefixAmbiguous) Error() string {
	return fmt.Sprintf("the id prefix %q names %d items; write more of the id", e.Prefix, len(e.Candidates))
}

// Viewer is who asks, which decides whether a direct message's first line is answered with it. A
// direct conversation belongs to a human and one session: a human reads any of them, a session
// only one it is in, and an index-time resolver, the zero Viewer, none - it writes the reference
// graph, which every authenticated caller reads.
//
// Human is a person, who gets every first line. Session is the session the caller claims, as
// `?session=` on the message routes carries it; it gets a direct message's first line only when
// that session is in the conversation, by the rule messageRead.hasSession applies: the thread
// root targets it, or it authored a reply in the thread. It is the caller's own claim, like
// `actor`, not an authorization boundary: nothing in Dispatch keeps a direct conversation's text
// from an authenticated caller, and this keeps one session from reading another's by mistake.
type Viewer struct {
	Human   bool
	Session string
}

// Scope is where ResolvePrefix looks.
//
// IssueKey and Document name the one owner to look within: the items of that issue, or the items
// written on that document. Both empty looks among every item of the kind, which is what an
// owner-free API route asks for; the resolver that rewrites a short id in a body fills the one
// the reference names. Document is the document's ref_key - `<issue key or project>/<slug>`, or
// `agent/<session id>/<slug>` - the address refs stores an artifact target under, not its
// `dispatch://` reference.
//
// For a block, Document is the document whose blocks the prefix is matched among and is
// required, and Version is the version the reference pins (text.Ref.Pin), 0 for the document's
// latest indexed version.
//
// Naming an owner a kind cannot have - both fields at once, a document or a block owned by a
// document, a message owned by anything but an issue, a version on anything but a block - is a
// programming error at the call site, answered as no match rather than queried for.
type Scope struct {
	IssueKey string
	Document string
	Version  int
}

// ResolvePrefix is the one item kind names whose id starts with prefix: an ask, a comment, a
// message, a document (`artifact`) or a document block, looked for where scope says, as viewer.
// Both callers of a short id go through it - the resolver that rewrites a body's short ids as it
// is indexed (text.Resolver.Items), with the zero Viewer and the owner the reference names, and
// the API route that resolves one id for a person or a session, with the viewer it authenticated
// and, where its route names no owner, the whole kind.
//
// It answers the one match, ErrPrefixAmbiguous with the matches listed (at most
// prefixCandidateLimit of them, in id order) where two or more carry the prefix, and
// pgx.ErrNoRows where none does, which is the one no-match signal: a kind it does not know, a
// scope its kind cannot have and a prefix that can start no id are all no match too, not errors
// of their own.
//
// prefix is an id written short as text.IsIDPrefix reads one: at least eight of the hex digits
// and hyphens an id's lowercase text form is written with, compared without case, and never a
// whole id, since an exact id needs no resolving and always wins. A value IsIDPrefix refuses is a
// programming error at the call site - every caller reads the id out of a reference the same
// grammar parsed - and is answered as no match without a query. Among the matches the id written
// exactly as the prefix wins, which is what a whole block id that is another block's prefix
// needs; where the id is a uuid, one that differs from the prefix in case alone wins after it,
// since a uuid's text form is lowercase hex and the prefix is read without case. A block id is
// text its author wrote, so `Deadbeef` and `deadbeef` are two blocks and the prefix names the one
// it is written as.
//
// An item's prefix is read as a range on its table's uuid primary key, the prefix with its
// hyphens where a uuid has them and the rest filled with `0` and with `f`, which that key's btree
// index serves; a prefix whose hyphens are not where a uuid's are can start no id and matches
// nothing without a query. A block's is matched among the block ids of the one document version
// named. Either way it is one query, which fills every candidate's first line too.
func ResolvePrefix(ctx context.Context, q Queryer, kind string, scope Scope, prefix string, viewer Viewer) (Candidate, error) {
	if !text.IsIDPrefix(prefix) {
		return Candidate{}, pgx.ErrNoRows
	}
	var found []Candidate
	var err error
	switch kind {
	case "ask", "comment":
		found, err = itemPrefixCandidates(ctx, q, kind, scope, prefix)
	case "message":
		found, err = messagePrefixCandidates(ctx, q, scope, prefix, viewer)
	case "artifact":
		found, err = artifactPrefixCandidates(ctx, q, scope, prefix)
	case "block":
		found, err = blockPrefixCandidates(ctx, q, scope, prefix)
	}
	if err != nil {
		return Candidate{}, err
	}
	for _, candidate := range found {
		if candidate.ID == prefix {
			return candidate, nil
		}
	}
	if kind != "block" {
		for _, candidate := range found {
			if strings.EqualFold(candidate.ID, prefix) {
				return candidate, nil
			}
		}
	}
	switch len(found) {
	case 0:
		return Candidate{}, pgx.ErrNoRows
	case 1:
		return found[0], nil
	}
	return Candidate{}, &ErrPrefixAmbiguous{Prefix: prefix, Candidates: found}
}

// itemPrefixCandidates is the asks or comments prefix names, oldest id first. An ask and a
// comment are owned by an issue or by a document (asks_one_owner, comments_one_owner), so either
// scope narrows the uuid range by the column that owns the row.
func itemPrefixCandidates(ctx context.Context, q Queryer, kind string, scope Scope, prefix string) ([]Candidate, error) {
	lower, upper, ok := uuidRange(prefix)
	if !ok || scope.Version != 0 || scope.IssueKey != "" && scope.Document != "" {
		return nil, nil
	}
	table, column := "asks", "question"
	if kind == "comment" {
		table, column = "comments", "body"
	}
	var args queryArgs
	query := `select item.id::text, item.issue_key, coalesce(issue.project_key, doc.project_key, ''), coalesce(doc.slug, ''), item.` + column + `
		from ` + table + ` item
		left join issues issue on issue.key = item.issue_key
		left join artifacts doc on doc.id = item.artifact_id
		where item.id >= ` + args.param(lower) + `::uuid and item.id <= ` + args.param(upper) + `::uuid`
	switch {
	case scope.IssueKey != "":
		query += ` and item.issue_key = ` + args.param(scope.IssueKey)
	case scope.Document != "":
		query += ` and item.artifact_id = (select id from artifacts where ref_key = ` + args.param(scope.Document) + `)`
	}
	query += ` order by item.id limit ` + prefixLimit
	rows, err := q.Query(ctx, query, args.values...)
	if err != nil {
		return nil, fmt.Errorf("read the %ss %q names: %w", kind, prefix, err)
	}
	defer rows.Close()
	var found []Candidate
	for rows.Next() {
		var id, project, slug, body string
		var issueKey *string
		if err := rows.Scan(&id, &issueKey, &project, &slug, &body); err != nil {
			return nil, fmt.Errorf("scan the %ss %q names: %w", kind, prefix, err)
		}
		found = append(found, itemCandidate(kind, id, issueKey, project, slug, body))
	}
	return found, rows.Err()
}

// messagePrefixCandidates is the messages prefix names, oldest id first. A message is owned by an
// issue alone, so a document scope names nothing.
//
// A direct message's first line is the one a caller may not read, so for a session viewer the
// same query reports, for each direct message among the candidates, whether that session is in
// its conversation: the thread root, found up in_reply_to as messageThreadCTE walks it, targets
// the session, or the session authored one of the messages of the thread, found back down
// in_reply_to. A human needs neither arm, and nor does an index-time resolver, which reads no
// direct message's first line at all.
func messagePrefixCandidates(ctx context.Context, q Queryer, scope Scope, prefix string, viewer Viewer) ([]Candidate, error) {
	lower, upper, ok := uuidRange(prefix)
	if !ok || scope.Version != 0 || scope.Document != "" {
		return nil, nil
	}
	var args queryArgs
	candidates := `select m.id, m.issue_key, m.body
			from messages m
			where m.id >= ` + args.param(lower) + `::uuid and m.id <= ` + args.param(upper) + `::uuid`
	if scope.IssueKey != "" {
		candidates += ` and m.issue_key = ` + args.param(scope.IssueKey)
	}
	candidates += ` order by m.id limit ` + prefixLimit
	threads, inConversation := "", "false"
	if !viewer.Human && viewer.Session != "" {
		threads = `,
		up as (
			select c.id as candidate, m.id as node, m.target, m.in_reply_to
			from candidates c join messages m on m.id = c.id
			where c.issue_key is null
			union all
			select u.candidate, m.id, m.target, m.in_reply_to
			from up u join messages m on m.id = u.in_reply_to
		),
		root as (select candidate, node, target from up where in_reply_to is null),
		thread as (
			select candidate, node from root
			union all
			select t.candidate, m.id from thread t join messages m on m.in_reply_to = t.node
		)`
		inConversation = `coalesce((select r.target from root r where r.candidate = c.id) = ` + args.param("session:"+viewer.Session) + `, false)
		       or exists (
		         select 1 from thread t join messages m on m.id = t.node
		         where t.candidate = c.id and m.author->>'kind' = 'session' and m.author->>'id' = ` + args.param(viewer.Session) + `
		       )`
	}
	rows, err := q.Query(ctx, `with recursive candidates as (`+candidates+`)`+threads+`
		select c.id::text, c.issue_key, coalesce(i.project_key, ''), c.body, `+inConversation+`
		from candidates c
		left join issues i on i.key = c.issue_key
		order by c.id`, args.values...)
	if err != nil {
		return nil, fmt.Errorf("read the messages %q names: %w", prefix, err)
	}
	defer rows.Close()
	var found []Candidate
	for rows.Next() {
		var id, project, body string
		var issueKey *string
		var inThread bool
		if err := rows.Scan(&id, &issueKey, &project, &body, &inThread); err != nil {
			return nil, fmt.Errorf("scan the messages %q names: %w", prefix, err)
		}
		candidate := itemCandidate("message", id, issueKey, project, "", body)
		if issueKey == nil && !viewer.Human && !inThread {
			candidate.FirstLine = ""
		}
		found = append(found, candidate)
	}
	return found, rows.Err()
}

// itemCandidate is one ask, comment or message row as a candidate, addressed as the graph
// addresses it (itemRef) and owned by its issue, or by the project document it is written on.
func itemCandidate(kind, id string, issueKey *string, project, slug, body string) Candidate {
	candidate := Candidate{
		Kind:      kind,
		ID:        id,
		Ref:       itemRef(kind, issueKey, project, slug, id),
		FirstLine: contracts.OneLineSummary(body),
	}
	switch {
	case issueKey != nil:
		candidate.IssueKey = *issueKey
	case project != "" && slug != "":
		candidate.Document = ArtifactRef(nil, project, nil, slug, "", false)
	}
	return candidate
}

// artifactPrefixCandidates is the documents and files prefix names, oldest id first. A document
// is owned by an issue or by a project, never by a document, so a document scope names nothing.
func artifactPrefixCandidates(ctx context.Context, q Queryer, scope Scope, prefix string) ([]Candidate, error) {
	lower, upper, ok := uuidRange(prefix)
	if !ok || scope.Version != 0 || scope.Document != "" {
		return nil, nil
	}
	var args queryArgs
	query := `select ` + documentColumns("a") + `, a.name
		from artifacts a
		where a.id >= ` + args.param(lower) + `::uuid and a.id <= ` + args.param(upper) + `::uuid`
	if scope.IssueKey != "" {
		query += ` and a.issue_key = ` + args.param(scope.IssueKey)
	}
	query += ` order by a.id limit ` + prefixLimit
	rows, err := q.Query(ctx, query, args.values...)
	if err != nil {
		return nil, fmt.Errorf("read the documents %q names: %w", prefix, err)
	}
	defer rows.Close()
	var found []Candidate
	for rows.Next() {
		var document documentParent
		var name string
		if err := rows.Scan(append(document.targets(), &name)...); err != nil {
			return nil, fmt.Errorf("scan the documents %q names: %w", prefix, err)
		}
		candidate := Candidate{Kind: "artifact", ID: *document.id, FirstLine: contracts.OneLineSummary(name)}
		if parent := document.parent(); parent != nil {
			candidate.Ref = parent.Ref
		}
		if document.issueKey != nil {
			candidate.IssueKey = *document.issueKey
		}
		found = append(found, candidate)
	}
	return found, rows.Err()
}

// blockPrefixCandidates is the blocks of one version of the document scope names whose id starts
// with prefix, in block id order. The version is the document's latest indexed one (Version 0),
// whose blocks document_blocks holds with the excerpt a mention of each shows, or the one the
// reference pins, whose blocks are the ids document_block_texts records a text of at or before
// it, the latest of those texts each, and none at all unless a block-aware build indexed that
// version (BlockTextAt reads one block the same way). A block id is text, not a uuid, so the
// prefix is matched on it, which is a read of the one document's blocks and not a scan.
func blockPrefixCandidates(ctx context.Context, q Queryer, scope Scope, prefix string) ([]Candidate, error) {
	if scope.Document == "" || scope.IssueKey != "" || scope.Version < 0 || scope.Version > math.MaxInt32 {
		return nil, nil
	}
	args := []any{scope.Document, strings.ToLower(prefix)}
	query := `select d.block_id, d.excerpt, ` + documentColumns("a") + `
		from document_blocks d
		join artifacts a on a.id = d.artifact_id
		where a.ref_key = $1 and starts_with(lower(d.block_id), $2)
		order by d.block_id limit ` + prefixLimit
	if scope.Version > 0 {
		args = append(args, scope.Version)
		query = `select t.block_id, t.text, ` + documentColumns("a") + `
			from artifacts a
			cross join lateral (
				select distinct on (b.block_id) b.block_id, b.text
				from document_block_texts b
				where b.artifact_id = a.id and b.version <= $3 and starts_with(lower(b.block_id), $2)
				order by b.block_id, b.version desc
			) t
			where a.ref_key = $1 and t.text is not null
			  and exists (select 1 from document_indexed_versions i where i.artifact_id = a.id and i.version = $3)
			order by t.block_id limit ` + prefixLimit
	}
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read the blocks of %s %q names: %w", scope.Document, prefix, err)
	}
	defer rows.Close()
	var found []Candidate
	for rows.Next() {
		var blockID, body string
		var document documentParent
		if err := rows.Scan(append([]any{&blockID, &body}, document.targets()...)...); err != nil {
			return nil, fmt.Errorf("scan the blocks of %s %q names: %w", scope.Document, prefix, err)
		}
		candidate := Candidate{Kind: "block", ID: blockID, FirstLine: contracts.OneLineSummary(body)}
		if parent := document.parent(); parent != nil {
			candidate.Ref = BlockRef(parent.Ref, blockID)
			if document.issueKey != nil {
				candidate.IssueKey = *document.issueKey
			} else if document.project != "" {
				candidate.Document = parent.Ref
			}
		}
		found = append(found, candidate)
	}
	return found, rows.Err()
}

// queryArgs are the arguments of a query this file builds by concatenation: param appends one
// and answers the placeholder that reads it, so the text and the arguments of a query whose
// shape depends on its scope cannot drift apart.
type queryArgs struct {
	values []any
}

func (a *queryArgs) param(value any) string {
	a.values = append(a.values, value)
	return "$" + strconv.Itoa(len(a.values))
}

// uuidRange is the range of uuids prefix names: the prefix, lowercased, with its remaining hex
// digits filled with `0` and with `f` and its remaining hyphens where a uuid writes them, so a
// comparison against it is one range on a uuid primary key. ok is false when prefix can start no
// uuid's text form - it is longer than one, or writes a hyphen where a uuid has a hex digit, or a
// hex digit where a uuid has a hyphen, as a prefix of an id whose hyphens it drops does - and
// nothing is then queried, since nothing can match.
func uuidRange(prefix string) (lower, upper string, ok bool) {
	// The canonical form a uuid's text is compared as: 8-4-4-4-12 hex digits.
	const canonical = "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
	if len(prefix) > len(canonical) {
		return "", "", false
	}
	low, high := []byte(canonical), []byte(canonical)
	for at := range len(canonical) {
		hyphen := canonical[at] == '-'
		if at >= len(prefix) {
			if hyphen {
				low[at], high[at] = '-', '-'
			} else {
				low[at], high[at] = '0', 'f'
			}
			continue
		}
		digit := prefix[at]
		if digit >= 'A' && digit <= 'F' {
			digit += 'a' - 'A'
		}
		if hyphen != (digit == '-') || !hyphen && !isHexDigit(digit) {
			return "", "", false
		}
		low[at], high[at] = digit, digit
	}
	return string(low), string(high), true
}

// CanStartUUID reports whether prefix can start a uuid's text form, which is uuidRange's own ok:
// a caller that has to tell a prefix no item carries from one no item can carry - the 404 an id
// route answers for a value that is no prefix of any id (api.itemNotFound) - asks the rule the
// range itself is built by rather than keeping a second copy of it.
func CanStartUUID(prefix string) bool {
	_, _, ok := uuidRange(prefix)
	return ok
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f'
}
