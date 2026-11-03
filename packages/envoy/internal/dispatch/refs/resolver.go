package refs

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// Mentions is the references one source's text writes, read with the whole grammar
// (text.Mentions): the `dispatch://` and dashboard addresses it spells out, the issue keys it
// writes bare, the `#id`s naming the question blocks of the document its text belongs to, and the
// ask, comment, message and block ids it writes short, each resolved to the whole id, over q, the
// pool or transaction the write runs in. Every indexing path reads its text through this call or
// through the resolver behind it (newResolver), so a comment, an ask, a message, a document, a
// block, an excerpt, the full rebuild (RebuildAll) and the re-index of old text (ReindexOwedItem)
// all store the same edges for the same text. fromKind and fromID are the source whose text body
// is, as refs stores it: they decide which document a `#id` resolves against (ScopeOf). Each
// mention keeps its byte offset, so a caller that places mentions in blocks passes the result
// through unchanged.
//
// A write never fails because of what its body cites: a resolver that cannot reach the database
// answers "nothing resolved" for that kind and the text is stored as it is written, the way a
// mention of a target that does not exist is (CountedChanges).
func Mentions(ctx context.Context, q Queryer, fromKind, fromID, body, serverURL string) []text.Located {
	return newResolver(ctx, q, fromKind, fromID, serverURL).mentions(body)
}

// resolver is the database text.Resolver one write reads its text through, over the pool or
// transaction that write runs in and for the one source whose text it is: its kind and id decide
// the document a `#id` resolves against, and the server URL is the one the mentions are read
// with. It is one write's object, not a cache: a caller builds it, reads every text of that
// source through it - a comment's body, an ask's question and each of its parts - and drops it,
// so the database answers once per kind for a write however many texts it holds, and what it
// answered is the one transaction's view.
//
// Every failure it meets reaching the database is logged here and answered as nothing resolved
// for that kind, and no call site handles one: the swallow lives in this one place because the
// rule is the resolver's own ("A write must never fail because of what its body cites", refs.go,
// CountedChanges), because answering per kind leaves the other kinds of one body resolved where a
// call site could only drop them all, and because it leaves text.Resolver's error return for a
// resolver that has a reason to fail a write. A prefix that names nothing and one two items or
// more carry are no failures at all: the mention stays as the text wrote it, which is what
// text.Mentions does with an answer that leaves it out.
type resolver struct {
	// ctx is the write's context, held because text.Resolver's methods take none: the resolver
	// lives for one write and is never stored.
	ctx       context.Context
	q         Queryer
	kind, id  string
	serverURL string

	// issues is every key this resolver was asked about, to whether it names an issue, so the
	// parts of one ask ask about a repeated key once.
	issues map[string]bool
	// questions is the question map of the source's document, loaded at most once (asked), and
	// nil for a source that belongs to no document or whose map could not be read.
	questions map[string]text.Ref
	asked     bool
	// items is the whole id each (kind, scope, prefix) this resolver resolved names, "" for one
	// that names nothing or two items or more, so a body writing the same short id twice is one
	// query.
	items map[prefixKey]string
}

// prefixKey is one short id as ResolvePrefix is asked it: the kind of item it names, the owner it
// is resolved within, and the prefix itself.
type prefixKey struct {
	kind   string
	scope  Scope
	prefix string
}

func newResolver(ctx context.Context, q Queryer, fromKind, fromID, serverURL string) *resolver {
	return &resolver{ctx: ctx, q: q, kind: fromKind, id: fromID, serverURL: serverURL}
}

// mentions is text.Mentions of body resolved through r.
func (r *resolver) mentions(body string) []text.Located {
	located, err := text.Mentions(body, r.serverURL, r)
	if err != nil {
		// Unreachable: r answers every failure as nothing resolved and returns none. A build
		// that gives it a reason to fail must still not fail the write, so the text is indexed
		// as the grammar alone reads it.
		r.failed("resolve the references of a text", err)
		return text.ExtractAt(body, r.serverURL)
	}
	return located
}

// Issues reports which of keys names an issue that exists, so a bare key is stored as a
// reference only when it has a target. Mentions asks with the distinct keys of one body, and
// this reads the ones this resolver has not been asked about yet, which is all of them for the
// first text it reads and the new ones alone for every text after it.
func (r *resolver) Issues(keys []string) (map[string]bool, error) {
	unknown := make([]string, 0, len(keys))
	for _, key := range keys {
		if _, known := r.issues[key]; !known {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		found, err := r.readIssues(unknown)
		if err != nil {
			r.failed("read the issues a text writes bare", err)
			return nil, nil
		}
		if r.issues == nil {
			r.issues = make(map[string]bool, len(unknown))
		}
		for _, key := range unknown {
			_, exists := found[key]
			r.issues[key] = exists
		}
	}
	answer := make(map[string]bool, len(keys))
	for _, key := range keys {
		answer[key] = r.issues[key]
	}
	return answer, nil
}

func (r *resolver) readIssues(keys []string) (map[string]struct{}, error) {
	rows, err := r.q.Query(r.ctx, `select key from issues where key = any($1)`, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := make(map[string]struct{}, len(keys))
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		found[key] = struct{}{}
	}
	return found, rows.Err()
}

// Questions is the question map of the document the source's text belongs to (ScopeOf, then
// QuestionMap), loaded at most once per resolver and not at all unless a body holds a `#`
// candidate, which is when text.Mentions asks. A source that belongs to no document - a direct
// message, a reply whose thread names none - has no question, and a text on an issue but on no
// document resolves against the issue's spec, which ScopeOf answers with.
func (r *resolver) Questions() (map[string]text.Ref, error) {
	if r.asked {
		return r.questions, nil
	}
	r.asked = true
	document, err := ScopeOf(r.ctx, r.q, r.kind, r.id)
	if err != nil {
		r.failed("read the document a text belongs to", err)
		return nil, nil
	}
	if document == "" {
		return nil, nil
	}
	questions, err := QuestionMap(r.ctx, r.q, document)
	if err != nil {
		r.failed("read the question blocks a text's `#id` names", err)
		return nil, nil
	}
	r.questions = questions
	return r.questions, nil
}

// Items is the whole id of each short id the text writes, resolved within the owner the reference
// itself names and not across the whole kind (ResolvePrefix, as the zero Viewer: the index writes
// the graph every authenticated caller reads, so it reads no direct message's first line). A
// prefix that names nothing, one two items or more carry, and one whose owner no Scope holds are
// all left out of the answer, which leaves the mention as the text wrote it.
func (r *resolver) Items(prefixes []text.Prefix) (map[int]string, error) {
	resolved := make(map[int]string, len(prefixes))
	for index, prefix := range prefixes {
		key, ok := prefixScope(prefix)
		if !ok {
			continue
		}
		whole, known := r.items[key]
		if !known {
			var err error
			if whole, err = r.resolveItem(key); err != nil {
				r.failed("resolve the short ids a text writes", err)
				return resolved, nil
			}
			if r.items == nil {
				r.items = make(map[prefixKey]string, len(prefixes))
			}
			r.items[key] = whole
		}
		if whole != "" {
			resolved[index] = whole
		}
	}
	return resolved, nil
}

// resolveItem is the whole id key names, "" where it names nothing or two items or more. An error
// is a failure reaching the database alone.
func (r *resolver) resolveItem(key prefixKey) (string, error) {
	candidate, err := ResolvePrefix(r.ctx, r.q, key.kind, key.scope, key.prefix, Viewer{})
	switch {
	case err == nil:
		return candidate.ID, nil
	case errors.Is(err, pgx.ErrNoRows), errors.As(err, new(*ErrPrefixAmbiguous)):
		return "", nil
	}
	return "", err
}

// prefixScope is what one short id asks ResolvePrefix: the kind the reference names and the owner
// it names, as a Scope. A block's prefix is matched among the blocks of the document the block is
// of, at the version the reference pins, and an item's within its issue.
//
// ok is false for a reference whose owner no Scope holds, which is then left as the text wrote
// it: an ask's or a comment's on a project document, since `dispatch://<PROJECT>/artifact/<slug>/
// ask/<id>` parses to the project alone and the slug no Scope can be built without is no part of
// it, and a block of an ask, a comment or a message, whose blocks are numbered (`b2`) and are no
// id a prefix can shorten.
func prefixScope(prefix text.Prefix) (prefixKey, bool) {
	ref := prefix.Ref
	if prefix.Block {
		if ref.Item != "" {
			return prefixKey{}, false
		}
		document := ref
		document.Kind, document.Block, document.Pin = "artifact", "", 0
		return prefixKey{
			kind:   "block",
			scope:  Scope{Document: text.TargetID(document), Version: ref.Pin},
			prefix: ref.Block,
		}, true
	}
	kind := ref.Kind
	if kind == "block" {
		kind = ref.Item
	}
	if ref.IssueKey == "" {
		return prefixKey{}, false
	}
	return prefixKey{kind: kind, scope: Scope{IssueKey: ref.IssueKey}, prefix: ref.ID}, true
}

// failed logs a failure reaching the database, which leaves the text indexed as it is written.
func (r *resolver) failed(what string, err error) {
	slog.Error("dispatch: could not "+what+"; it is indexed as it is written",
		"kind", r.kind, "id", r.id, "error", err)
}
