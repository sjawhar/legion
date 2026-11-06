package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// writeAmbiguousID refuses an id written as a prefix two items or more carry, listing them for
// the caller to choose between or to write more of the id. It is writeAmbiguousTarget's shape -
// {error, code, candidates} under a 409 - because it is the same kind of answer: the request
// named something the server cannot narrow to one thing, and the caller picks. The page that
// lists the candidates is the dashboard's, rendered from this answer; there is no route of its
// own to read them from.
func writeAmbiguousID(w http.ResponseWriter, ambiguous *refs.ErrPrefixAmbiguous) {
	WriteJSON(w, http.StatusConflict, map[string]any{
		"error":      ambiguous.Error(),
		"code":       contracts.IDAmbiguous,
		"candidates": ambiguous.Candidates,
	})
}

// canonicalizeUUIDPath rewrites the path value `name`, where it is a whole uuid written in one
// of the other spellings uuid.Parse reads - `urn:uuid:<uuid>`, the braced one, and 32 hex
// digits - to the canonical spelling. It is the one place the spelling of a path id is decided:
// the prefix layer runs it for every route that takes an id, ahead of the scope checks, and
// every reader behind it parses the value as it stands (requireUUIDPath, getBroadcast,
// sessionGroupID, documentRefKey), so the check that decides whether a write is the caller's to
// make and the handler that then makes it cannot disagree about which object the path named
// (project_scope.go).
//
// Such a value is no id written short (text.IsIDPrefix refuses its colons, braces, and a whole
// uuid alike), so without this it would reach a `::uuid` column as the caller spelled it.
// Postgres reads the braced and 32-digit spellings and refuses `urn:uuid:` ("invalid input
// syntax for type uuid"), so that one address, which this server reads, would answer 500 where
// the canonical spelling gets the route's own answer. A value that is no uuid at all is left
// alone, and answered as written, for the route to refuse.
func canonicalizeUUIDPath(r *http.Request, name string) {
	value := r.PathValue(name)
	parsed, err := uuid.Parse(value)
	if err != nil {
		return
	}
	if canonical := parsed.String(); canonical != value {
		r.SetPathValue(name, canonical)
	}
}

// resolveIDPrefixes rewrites a route's {id}, and the {block_id} of a document block, from an id
// written short to the whole id it names, before anything behind it reads that value: register
// wraps it around every route outside the two scope checks, so the check that decides whether a
// write is the caller's to make and the handler that then makes it read one value and cannot
// disagree about which object the path named (project_scope.go). What each route's wildcards
// name, which routes name none of the five kinds, and what 404 each route answers for an id it
// does not hold is idRoutes; a route whose pattern carries no id is returned unwrapped and pays
// nothing.
//
// A value that is no id written short (text.IsIDPrefix — at least eight hex digits and hyphens,
// never a whole uuid) is left as written with no query, so a whole id, a slug, a numbered id, a
// seven-character stub and a block id such as b1 or ship_it cost nothing and get the answer they
// always got, the 400 of a route that parses its id included. The one such value this does
// rewrite is a whole uuid an {id} spells another of uuid.Parse's ways, which is rewritten to the
// canonical spelling for every route that takes an id, one that resolves no prefix included:
// that rewrite is the whole of this layer's work there, a parse and at most one SetPathValue
// before the scope checks, with no query and no actor resolved (canonicalizeUUIDPath).
//
// A value that is one is looked for among the items of its kind, within the owner the route
// names — the issue of /issues/{key}/messages/{id}, the document of
// /artifacts/{id}/blocks/{block_id} — and otherwise among every item of the kind, the same set
// a whole id is looked for in; the route then applies its own rule to the item resolved, the
// direct-message guard and the project check included. One match rewrites the value
// (SetPathValue). A prefix two items or more carry is 409 ID_AMBIGUOUS. One nothing carries is
// the 404 that route answers for a whole id of its kind that names no item (idRoute.NotFound),
// so a short id fails where a whole one fails; the one route that answers no 404 for an id it
// does not hold keeps that answer (notFoundNone). Either way the handler never runs, which is
// where this differs from a whole id on a request that is malformed besides: the whole id would
// reach the handler and be refused for that other reason first, and the prefix is refused here.
//
// Nothing is read for a caller that did not authenticate: every route with an id in its path is
// an authenticated one, and its own refusal is the handler's to write.
func (s *server) resolveIDPrefixes(route apiRoute, handler http.HandlerFunc) http.HandlerFunc {
	entry, named := idRoutes[route.Method+" "+route.Pattern]
	if !named {
		return handler
	}
	return func(w http.ResponseWriter, r *http.Request) {
		canonicalizeUUIDPath(r, "id")
		var id, blockID string
		if entry.Kind != "" {
			if value := r.PathValue("id"); text.IsIDPrefix(value) {
				id = value
			}
		}
		if entry.Blocks {
			if value := r.PathValue("block_id"); text.IsIDPrefix(value) {
				blockID = value
			}
		}
		// Nothing to look up: a value that is no id written short, and every value of a row that
		// resolves nothing, whose Kind is empty and Blocks false. For either, the canonicalized
		// id is the whole of this layer's work, with no query and no actor resolved.
		if id == "" && blockID == "" {
			handler(w, r)
			return
		}
		actor, human, err := s.optionalActor(r)
		if err != nil {
			handler(w, r)
			return
		}
		// The caller is resolved once a request: every layer below and the handler read this.
		r = r.WithContext(context.WithValue(r.Context(), resolvedCallerKey{}, resolvedCaller{actor: actor, human: human}))
		// Who asks, which decides only whether a direct message's first line is listed among
		// the candidates: a person reads any, and a bearer the conversations the session it
		// names in ?session= is in, the claim the message routes already read.
		viewer := refs.Viewer{Human: human, Session: strings.TrimSpace(r.URL.Query().Get("session"))}
		if id != "" {
			scope := refs.Scope{}
			if entry.WithinIssue {
				scope.IssueKey = r.PathValue("key")
			}
			if !s.resolvePathValue(w, r, "id", entry.Kind, scope, id, viewer, itemNotFound(entry.NotFound, id)) {
				return
			}
		}
		if blockID != "" {
			// A block id is unique within its document alone, so it is matched among the
			// blocks of the document the {id} names, read under the id the step above left
			// there. An {id} that is no document's names no blocks to match among, and the
			// route answers for that {id} rather than for its block.
			document, err := s.documentRefKey(r.Context(), r.PathValue("id"))
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			if document != "" {
				scope := refs.Scope{Document: document}
				if !s.resolvePathValue(w, r, "block_id", "block", scope, blockID, viewer, entry.BlockNotFound) {
					return
				}
			}
		}
		handler(w, r)
	}
}

// resolvePathValue rewrites the path value `name`, written as the prefix of an id of kind, to
// the whole id it names within scope, and reports whether the request may go on: false once it
// has written the answer itself. notFound is the 404 code this route answers for an id of that
// kind it does not hold, and notFoundNone the route that answers no 404 for one, whose value is
// then left as written for the handler to answer for. It is the one caller of wholeID that has
// a route code, so it is the one that writes a 404.
func (s *server) resolvePathValue(
	w http.ResponseWriter,
	r *http.Request,
	name, kind string,
	scope refs.Scope,
	prefix string,
	viewer refs.Viewer,
	notFound string,
) bool {
	whole, err := s.wholeID(r.Context(), kind, scope, prefix, viewer)
	if err != nil {
		s.writeIDPrefixError(w, err)
		return false
	}
	if whole == "" {
		if notFound == notFoundNone {
			return true
		}
		writeError(w, notFound, http.StatusNotFound, fmt.Sprintf("no %s whose id starts with %q", kind, prefix))
		return false
	}
	r.SetPathValue(name, whole)
	return true
}

// wholeID is the whole id prefix names within scope, and "" for a prefix nothing carries, which
// each caller answers for itself: the prefix layer with the 404 its route answers for a whole id
// of that kind (resolvePathValue), and the reference reads by leaving the reference as written.
// An error is the answer the caller has to write rather than one it can go on from, and
// writeIDPrefixError writes it: the 409 of a prefix two items or more carry, or a database that
// could not be read.
func (s *server) wholeID(
	ctx context.Context,
	kind string,
	scope refs.Scope,
	prefix string,
	viewer refs.Viewer,
) (string, error) {
	candidate, err := refs.ResolvePrefix(ctx, s.deps.Store.Pool, kind, scope, prefix, viewer)
	switch {
	case err == nil:
		return candidate.ID, nil
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil
	}
	return "", err
}

// writeIDPrefixError writes what wholeID could not resolve a prefix through: the 409 of a prefix
// two items or more carry, listing them (writeAmbiguousID), and otherwise the 500 of a database
// that could not be read.
func (s *server) writeIDPrefixError(w http.ResponseWriter, err error) {
	var ambiguous *refs.ErrPrefixAmbiguous
	if errors.As(err, &ambiguous) {
		writeAmbiguousID(w, ambiguous)
		return
	}
	s.writeHandlerError(w, err)
}

// itemNotFound is the 404 an item prefix that names nothing gets on a route whose own is
// notFound: that answer where the value can start a uuid, and notFoundNone where it cannot.
//
// A value text.IsIDPrefix accepts can still be no prefix of any id: one longer than a uuid, or
// one writing a hyphen where a uuid has a hex digit — `a061-e3f2-…`, the spelling with a hyphen
// after every four digits, which Postgres reads as a whole uuid and every handler behind this
// reads as the row it names (project_scope.go, INFRA-264). Such a value matches nothing because
// it can start no id, not because no item carries it, so it is left as written and the route
// reads it as it always did; refs.ResolvePrefix answers both as no match, and refs.CanStartUUID
// is the rule its uuidRange tells the two apart by.
func itemNotFound(notFound, prefix string) string {
	if !refs.CanStartUUID(prefix) {
		return notFoundNone
	}
	return notFound
}

// documentRefKey is the ref_key of the document id names, the address refs.Scope holds a
// document under; "" for an id that is no uuid or that no document has, which is an id the
// route behind this is about to refuse anyway.
//
// The id is the one this layer canonicalized a step earlier (canonicalizeUUIDPath), so it is
// sent as it stands, and the parse here decides one thing only: whether the value is an id at
// all, since a value that is none names no document.
func (s *server) documentRefKey(ctx context.Context, id string) (string, error) {
	if _, err := uuid.Parse(id); err != nil {
		return "", nil
	}
	var refKey string
	if err := s.deps.Store.Pool.QueryRow(ctx, `select ref_key from artifacts where id = $1::uuid`, id).Scan(&refKey); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("read the document %s a block prefix is matched within: %w", id, err)
	}
	return refKey, nil
}
