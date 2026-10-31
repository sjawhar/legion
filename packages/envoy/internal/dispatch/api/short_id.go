package api

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// A link written by hand carries the first characters of an item's uuid - `dispatch://CORE-1/
// ask/9b31964e`, `/issues/CORE-1/asks/9b31964e` - because that is what a reader can hold in
// their head and what a writer copies out of a command's output. Every such link answered 404
// until this file: the id is resolved here, once, for every route that takes one, so a link
// written short opens the record a link written whole opens.
//
// shortIDMinimum is the fewest hex digits a short id carries. An item's id is a version-4 uuid,
// so its first 8 hex digits are 32 random bits: within one issue or one document, the scope a
// link names and a prefix is resolved under, two items sharing them is out of reach, while
// across a whole workspace a collision is rare but reachable - which is why an ambiguous prefix
// is refused and never guessed. 8 is also the length the links already written carry (a shorter
// prefix would resolve nothing that is written today and only widen what one can mean), and the
// length `@legion/envoy-client` names in its own refusals, so the server and the agent client
// read an id the same way.
const shortIDMinimum = 8

// shortIDCandidates bounds the rows a short id's range scan reads. An ambiguous prefix names the
// first few of them, and the rest only have to be known to exist.
const shortIDCandidates = 11

// shortIDNamed is how many candidates an ambiguous refusal names.
const shortIDNamed = 5

// shortIDRange is the inclusive uuid range a short id names: its hex digits, then zeros for the
// low end and fs for the high. PostgreSQL orders uuids by their bytes, so `id between lo and hi`
// is a range scan of the table's own primary key and needs no index of its own. A value that is
// already a whole uuid, that holds a character which is neither hex nor a hyphen, or that
// carries fewer than shortIDMinimum hex digits is not a short id and is left exactly as written.
func shortIDRange(value string) (lo uuid.UUID, hi uuid.UUID, ok bool) {
	if _, err := uuid.Parse(value); err == nil {
		return uuid.UUID{}, uuid.UUID{}, false
	}
	digits := make([]byte, 0, 32)
	for index := range len(value) {
		character := value[index]
		switch {
		case character == '-':
			continue
		case character >= '0' && character <= '9', character >= 'a' && character <= 'f':
			digits = append(digits, character)
		case character >= 'A' && character <= 'F':
			digits = append(digits, character+('a'-'A'))
		default:
			return uuid.UUID{}, uuid.UUID{}, false
		}
		if len(digits) >= 32 {
			return uuid.UUID{}, uuid.UUID{}, false
		}
	}
	if len(digits) < shortIDMinimum {
		return uuid.UUID{}, uuid.UUID{}, false
	}
	low, lowErr := uuid.Parse(string(digits) + strings.Repeat("0", 32-len(digits)))
	high, highErr := uuid.Parse(string(digits) + strings.Repeat("f", 32-len(digits)))
	if lowErr != nil || highErr != nil {
		return uuid.UUID{}, uuid.UUID{}, false
	}
	return low, high, true
}

// shortIDScope is the owner a short id is resolved under: the issue or the document the link
// carrying it names. Every surface a person follows names one - the page is at
// `/issues/CORE-1/asks/<id>`, the reference is `dispatch://CORE-1/ask/<id>` - and passes it as
// `?issue=` or `?artifact=`, or in the route's own `{key}`. A request that names none resolves
// across the whole workspace, where a prefix two records share is refused rather than guessed.
type shortIDScope struct {
	issue    string
	artifact string
}

// describe names where the lookup looked, with the preposition that reads: "on CORE-1", "in
// this workspace".
func (s shortIDScope) describe() string {
	switch {
	case s.issue != "" && s.artifact != "":
		return "on " + s.issue + " and that document"
	case s.issue != "":
		return "on " + s.issue
	case s.artifact != "":
		return "on that document"
	default:
		return "in this workspace"
	}
}

// shortIDRow is one candidate: its id and the owners a scope is checked against.
type shortIDRow struct {
	id       string
	issue    string
	artifact string
}

// shortIDItem is an item kind a link addresses by uuid. query reads the candidates in a short
// id's range that belong to the scope, with the owners it was held to: a row's issue key, and
// the document it hangs on where it has one. The scope is in the query rather than applied to
// what comes back, so the bound on the rows read is a bound on the scope's own candidates:
// filtering afterwards would let unrelated rows that sort earlier hide, or falsely make unique,
// the rows the link names. Every column is read as text, so no row can fail the read by holding
// a value another type would refuse. $4 is the issue key the lookup is held to and $5 the
// document, each the empty string when the link names none.
type shortIDItem struct {
	noun  string
	query string
}

// shortIDOwned is the scope predicate every query carries, over the two owner expressions it
// selects: an empty scope holds every row.
const shortIDOwned = ` and ($4 = '' or upper(%s) = upper($4)) and ($5 = '' or %s = $5)`

// shortIDItems names, for each route segment that an `{id}` follows, the item that id addresses.
// A segment absent here - `runs` (a GitHub run number), `session-groups`, `credential-requests`,
// `credential-grants`, `machine-logins`, `agent-tokens`, `threads` - carries an id of another
// shape, or of a record no link a person follows addresses, and is left exactly as written.
var shortIDItems = map[string]shortIDItem{
	"asks": {noun: "ask", query: shortIDQuery("asks",
		"coalesce(issue_key, '')",
		"coalesce(artifact_id::text, block_artifact_id::text, anchor->>'artifact_id', approval->>'artifact_id', '')")},
	"comments": {noun: "comment", query: shortIDQuery("comments",
		"coalesce(issue_key, '')",
		"coalesce(artifact_id::text, anchor->>'artifact_id', '')")},
	"messages": {noun: "message", query: shortIDQuery("messages", "coalesce(issue_key, '')", "''")},
	"artifacts": {noun: "document", query: shortIDQuery("artifacts",
		"coalesce(issue_key, project_key, '')", "id::text")},
	"broadcasts": {noun: "broadcast", query: shortIDQuery("broadcasts", "''", "''")},
}

// shortIDQuery builds one item's candidate read from its table and the two expressions that
// name a row's owners, which are both selected and held to the scope.
func shortIDQuery(table, issue, artifact string) string {
	return fmt.Sprintf(
		`select id::text, %s, %s from %s where id between $1 and $2`+shortIDOwned+` order by id limit $3`,
		issue, artifact, table, issue, artifact)
}

// patternIDSegment is the route segment an `{id}` wildcard follows, which names the item that id
// addresses: `asks` in `/api/v1/asks/{id}` and in `/api/v1/me/asks/{id}/snooze`.
var patternIDSegment = regexp.MustCompile(`/([a-z-]+)/\{id\}`)

// patternIssueKey is a route that names its issue in the path, so a short id under it is scoped
// to that issue without the caller passing one: `/api/v1/issues/{key}/messages/{id}`.
var patternIssueKey = regexp.MustCompile(`^/api/v1/issues/\{key\}/`)

// resolveShortIDs serves handler with the route's `{id}` replaced by the full id a short one
// names. It is the innermost layer, so every check above it - the unstorable-text refusal, the
// login check, the keyed-write layer - reads the id as the caller wrote it, and the handler
// below reads the id the record carries. A value that is already a uuid, or is no short id at
// all, is passed straight through: the common request pays one uuid parse and no query.
//
// auth is the route's own class, and a caller it does not admit is handed to the handler
// untouched, which answers as it always has: a bearer on a human-only route reads its 403
// rather than a 404 or a list of candidate ids it may not see.
func (s *server) resolveShortIDs(pattern string, auth routeAuth, handler http.HandlerFunc) http.HandlerFunc {
	segment := patternIDSegment.FindStringSubmatch(pattern)
	if segment == nil {
		return handler
	}
	item, known := shortIDItems[segment[1]]
	if !known {
		return handler
	}
	issueInPath := patternIssueKey.MatchString(pattern)
	return func(w http.ResponseWriter, r *http.Request) {
		low, high, short := shortIDRange(r.PathValue("id"))
		if !short {
			handler(w, r)
			return
		}
		// A caller that did not authenticate, or that this route does not admit, never reaches
		// a lookup. The caller it resolves is kept on the request, so the handler's own read of
		// it costs nothing more.
		actor, human, err := s.optionalActor(r)
		if err != nil || (auth == authHuman && !human) || (auth == authBearer && human) {
			handler(w, r)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), resolvedCallerKey{}, resolvedCaller{actor: actor, human: human}))
		scope := shortIDScope{
			issue:    strings.TrimSpace(r.URL.Query().Get("issue")),
			artifact: strings.TrimSpace(r.URL.Query().Get("artifact")),
		}
		if issueInPath {
			scope = shortIDScope{issue: r.PathValue("key")}
		}
		full, refusal := s.resolveShortID(r.Context(), s.deps.Store.Pool, item, scope, r.PathValue("id"), low, high)
		if refusal != nil {
			s.writeHandlerError(w, refusal)
			return
		}
		r.SetPathValue("id", full)
		handler(w, r)
	}
}

// resolveShortAsk reads the one ask of owner whose id begins with a short one, for a write that
// names an ask by id in its body rather than in its path. ok is false when the value written is
// no short id at all, which the caller refuses as it always has.
func (s *server) resolveShortAsk(ctx context.Context, q queryer, owner owner, written string) (id string, refusal *apiError, ok bool) {
	low, high, short := shortIDRange(written)
	if !short {
		return "", nil, false
	}
	scope := shortIDScope{}
	if owner.IssueKey != nil {
		scope.issue = *owner.IssueKey
	}
	if owner.ArtifactID != nil {
		scope.artifact = *owner.ArtifactID
	}
	id, refusal = s.resolveShortID(ctx, q, shortIDItems["asks"], scope, written, low, high)
	return id, refusal, true
}

// resolveShortID reads the one record of item whose id begins with the short id written, within
// scope. None is the 404 the full id would have answered, naming where it looked; more than one
// is refused and named, since a prefix resolved to the wrong record silently is worse than a
// link that does not open.
func (s *server) resolveShortID(
	ctx context.Context,
	q queryer,
	item shortIDItem,
	scope shortIDScope,
	written string,
	low, high uuid.UUID,
) (string, *apiError) {
	rows, err := q.Query(ctx, item.query, low, high, shortIDCandidates, scope.issue, scope.artifact)
	if err != nil {
		return "", errorf(http.StatusInternalServerError, "INTERNAL", "resolve %s id: %v", item.noun, err)
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (shortIDRow, error) {
		var found shortIDRow
		return found, row.Scan(&found.id, &found.issue, &found.artifact)
	})
	if err != nil {
		return "", errorf(http.StatusInternalServerError, "INTERNAL", "resolve %s id: %v", item.noun, err)
	}
	matches := candidates
	if len(matches) == 1 {
		return matches[0].id, nil
	}
	if len(matches) == 0 {
		return "", errorf(http.StatusNotFound, "NOT_FOUND",
			"no %s %s has an id starting %s", item.noun, scope.describe(), written)
	}
	return "", errorf(http.StatusConflict, "AMBIGUOUS_ID",
		"%s id %s names %s %s: %s. Use the full id.",
		item.noun, written, countedMatches(len(matches), item.noun), scope.describe(), namedMatches(matches))
}

// countedMatches reads "3 asks", and "more than 10 asks" once the range scan hit its bound.
func countedMatches(count int, noun string) string {
	if count >= shortIDCandidates {
		return fmt.Sprintf("more than %d %ss", shortIDCandidates-1, noun)
	}
	return fmt.Sprintf("%d %ss", count, noun)
}

// namedMatches lists the first few candidates, each with the issue that holds it where it has
// one, so the reader can pick the record they meant without another lookup.
func namedMatches(matches []shortIDRow) string {
	named := make([]string, 0, shortIDNamed)
	for _, match := range matches[:min(len(matches), shortIDNamed)] {
		if match.issue == "" {
			named = append(named, match.id)
			continue
		}
		named = append(named, match.id+" on "+match.issue)
	}
	if len(matches) > len(named) {
		named = append(named, "…")
	}
	return strings.Join(named, ", ")
}

// shortIDMatch reports whether written, a short id, is a prefix of id: the comparison the
// document's own block ids are resolved by, where an id is text rather than a uuid.
func shortIDMatch(written, id string) bool {
	return len(written) < len(id) && strings.EqualFold(written, id[:len(written)])
}

// resolveShortBlockID reads the one block of a document whose id begins with a short one. A
// block id is text, not a uuid - a document can carry one an author wrote, such as `q:1` - so
// the match is on the id as written and the scope is the document itself, which is the only
// place a block id has to be unique. An empty answer with no refusal means the value is no
// short id, and the caller keeps the 404 the exact lookup already gave.
func (s *server) resolveShortBlockID(ctx context.Context, artifactID, written string) (string, *apiError) {
	if _, _, short := shortIDRange(written); !short {
		return "", nil
	}
	blocks, err := s.deps.Docs.Blocks(ctx, artifactID)
	if err != nil {
		return "", errorf(http.StatusInternalServerError, "INTERNAL", "resolve block id: %v", err)
	}
	matches := make([]string, 0, 2)
	for _, block := range blocks {
		for _, id := range append([]string{block.ID}, block.DescendantIDs...) {
			if shortIDMatch(written, id) {
				matches = append(matches, id)
			}
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) == 0 {
		return "", nil
	}
	return "", errorf(http.StatusConflict, "AMBIGUOUS_ID",
		"block id %s names %d blocks of this document: %s. Use the full id.",
		written, len(matches), strings.Join(matches[:min(len(matches), shortIDNamed)], ", "))
}
