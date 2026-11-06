package api

// going_catalogue.go builds the catalogue of operations an agent may propose on Dispatch's
// graph, and serves it at GET /api/v1/going/catalogue.
//
// The catalogue is generated from the route table rather than written by hand: which rows belong
// in it is computed here from the same tables the project-scope check reads (routes_table.go,
// project_scope.go), and the arguments of each one come from the generated
// contracts.GoingRequests, whose source is packages/contracts/src/going-catalogue.ts. A row
// added later either reaches the catalogue with a request type of its own or fails
// TestTheGoingCatalogueHoldsEveryEligibleRoute and refuses to mount (register), so an agent can
// name only actions that exist.
//
// Nothing here decides anything: the catalogue says what an action is and what it takes, and
// GoingCaller performs one an agent already chose. The loop that chooses is not in this package.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// goingCataloguePath is the route that serves the catalogue, named once so the refusal of an
// action the catalogue does not hold can point at the list without spelling it again.
const goingCataloguePath = "/api/v1/going/catalogue"

// maxGoingSearchResults is the most actions one search answers, and the ceiling on ?limit=.
const maxGoingSearchResults = 50

// GoingAction is one action the catalogue holds: a route an agent may propose an operation on,
// with the arguments that operation takes.
type GoingAction struct {
	// Action is the catalogue's key, "<METHOD> <pattern>", as the route table names the row.
	Action  string `json:"action"`
	Method  string `json:"method"`
	Pattern string `json:"pattern"`
	// Description is one sentence: the dispatch tool's where a tool performs this route, and the
	// route row's own otherwise.
	Description string `json:"description"`
	// Writes is the server's own classification, read from the mutating set
	// TestEveryMutatingRouteIsClassed computes and never from a field of a tool spec: a row is a
	// write exactly when that set holds it.
	Writes bool `json:"writes"`
	// Tool is the dispatch tool that performs this action, empty where none does.
	Tool string `json:"tool,omitempty"`
	// Relations is the graph relations this action's verbs write, each a structural edge kind
	// (model.GraphEdgeKinds) or a `block[]` attribute of a typed block (pmdoc.LinkKinds).
	Relations []string `json:"relations,omitempty"`
	// Request is the action's arguments: its pattern's placeholders and its body's fields.
	Request []contracts.GoingField `json:"request"`
	// Example is a call this action's schema accepts.
	Example json.RawMessage `json:"example"`

	// terms is Description's searchable words with their counts, built once at mount. It is not
	// served: a reader searches through the route rather than ranking the list itself.
	terms map[string]int
}

// GoingCatalogue is every action an agent may propose, searchable by meaning over the actions'
// own descriptions.
type GoingCatalogue struct {
	actions []GoingAction
	// byAction is the index into actions of each action's name.
	byAction map[string]int
	// idf weighs a term by how few actions use it, so a word every description carries ranks
	// nothing and a rare one decides the order.
	idf map[string]float64
}

// Actions is the catalogue in action order.
func (c GoingCatalogue) Actions() []GoingAction {
	return c.actions
}

// Lookup is the action that name holds, and whether the catalogue holds one at all.
func (c GoingCatalogue) Lookup(name string) (GoingAction, bool) {
	index, held := c.byAction[name]
	if !held {
		return GoingAction{}, false
	}
	return c.actions[index], true
}

// mutatingRoutes is every row of the table that changes something, in the shape
// TestEveryMutatingRouteIsClassed reads the table in: a row whose method is not GET, each of
// which writeClasses must class (undecidableWrites). It is the one classification of a write the
// server has, and the catalogue's Writes is read from it.
func mutatingRoutes(routes []apiRoute) map[string]bool {
	mutating := make(map[string]bool, len(routes))
	for _, route := range routes {
		if route.Method == http.MethodGet {
			continue
		}
		mutating[route.Method+" "+route.Pattern] = true
	}
	return mutating
}

// catalogueEligible answers whether one mutating row belongs in the catalogue. Two rows do not:
// a test hook, which no deployment serves without DISPATCH_TEST_HOOKS and which is nobody's
// operation on the graph, and a row only a bearer token may call, which is a session answering
// for itself rather than an action a person's credential may propose. Every other mutating row
// is eligible, whatever scopes it needs: a person's browser, their personal tokens and their
// machine logins hold both `person` and `approve` (routeScopes).
func catalogueEligible(route apiRoute) bool {
	return !strings.Contains(route.Pattern, "/_test/") && route.Auth != authBearer
}

// catalogueEligibleRoutes is the eligible set, computed from the tables themselves: every
// mutating row (mutatingRoutes) that catalogueEligible admits, in action order.
func catalogueEligibleRoutes(routes []apiRoute) []apiRoute {
	mutating := mutatingRoutes(routes)
	eligible := make([]apiRoute, 0, len(mutating))
	for _, route := range routes {
		if mutating[route.Method+" "+route.Pattern] && catalogueEligible(route) {
			eligible = append(eligible, route)
		}
	}
	sort.Slice(eligible, func(left, right int) bool {
		if eligible[left].Pattern != eligible[right].Pattern {
			return eligible[left].Pattern < eligible[right].Pattern
		}
		return eligible[left].Method < eligible[right].Method
	})
	return eligible
}

// edgeFamilies is every relation name a catalogue entry may declare: the global relation names
// and the `block[]` attributes of the typed blocks, which are the two families of edge the graph
// holds and the two GET /api/v1/references?kind= accepts (references.go).
func edgeFamilies() []string {
	return append(slices.Clone(model.GraphEdgeKinds), pmdoc.LinkKinds()...)
}

// BuildGoingCatalogue is the catalogue of the server's own route table, with no test hooks: what
// `envoy-dispatch` serves and what a caller outside a request builds. register builds the
// server's from the table it mounts.
func BuildGoingCatalogue() (GoingCatalogue, error) {
	return buildGoingCatalogue((&server{}).routes())
}

// buildGoingCatalogue is one entry per eligible route, and an error naming every row the
// catalogue cannot hold: one with no request type of its own, one declaring a relation in
// neither edge family, and a request type for a route that is not eligible. register refuses to
// mount a table it answers an error for, so a route added without reaching the catalogue, or
// without declaring what it takes, breaks the build rather than leaving an agent naming an
// action nothing serves.
func buildGoingCatalogue(routes []apiRoute) (GoingCatalogue, error) {
	eligible := catalogueEligibleRoutes(routes)
	mutating := mutatingRoutes(routes)
	families := edgeFamilies()
	catalogue := GoingCatalogue{
		actions:  make([]GoingAction, 0, len(eligible)),
		byAction: make(map[string]int, len(eligible)),
	}
	var refusals []string
	for _, route := range eligible {
		name := route.Method + " " + route.Pattern
		request, held := contracts.GoingRequests[name]
		if !held {
			refusals = append(refusals, name+": no request type (packages/contracts/src/going-catalogue.ts)")
			continue
		}
		for _, relation := range request.Relations {
			if !slices.Contains(families, relation) {
				refusals = append(refusals, fmt.Sprintf("%s: relation %q is neither a structural edge kind (%s) nor a block[] attribute (%s)",
					name, relation, strings.Join(model.GraphEdgeKinds, ", "), strings.Join(pmdoc.LinkKinds(), ", ")))
			}
		}
		description := firstSentence(route.Description)
		if request.ToolDescription != "" {
			description = firstSentence(request.ToolDescription)
		}
		catalogue.byAction[name] = len(catalogue.actions)
		catalogue.actions = append(catalogue.actions, GoingAction{
			Action:      name,
			Method:      route.Method,
			Pattern:     route.Pattern,
			Description: description,
			Writes:      mutating[name],
			Tool:        request.Tool,
			Relations:   request.Relations,
			Request:     request.Fields,
			Example:     request.Example,
			terms:       goingTerms(description),
		})
	}
	for name := range contracts.GoingRequests {
		if _, held := catalogue.byAction[name]; !held {
			refusals = append(refusals, name+": a request type for a route the catalogue is not eligible to hold")
		}
	}
	if len(refusals) > 0 {
		sort.Strings(refusals)
		return GoingCatalogue{}, fmt.Errorf("%d route(s) the catalogue cannot hold:\n%s", len(refusals), strings.Join(refusals, "\n"))
	}
	catalogue.idf = goingIDF(catalogue.actions)
	return catalogue, nil
}

// firstSentence is the first sentence of text: up to and including the first full stop that ends
// a word, or the whole text when it holds none. A description is one sentence in the catalogue
// whether it came from a tool's reference, which runs to paragraphs, or from the route row's own
// description, which runs to its refusals.
func firstSentence(text string) string {
	for index, char := range text {
		if char != '.' {
			continue
		}
		rest := text[index+1:]
		if rest == "" {
			return text
		}
		if next := rest[0]; next == ' ' || next == '\n' {
			return text[:index+1]
		}
	}
	return text
}

// goingStopWords are the words every description shares, which say nothing about which action a
// query means.
var goingStopWords = map[string]bool{
	"a": true, "an": true, "and": true, "any": true, "are": true, "as": true, "at": true,
	"be": true, "but": true, "by": true, "each": true, "every": true, "for": true, "from": true,
	"has": true, "have": true, "in": true, "into": true, "is": true, "it": true, "its": true,
	"no": true, "not": true, "of": true, "on": true, "one": true, "or": true, "that": true,
	"the": true, "their": true, "them": true, "then": true, "this": true, "to": true,
	"which": true, "while": true, "with": true, "you": true, "your": true,
}

// goingTerms is the searchable words of text with their counts: lowercased runs of letters and
// digits, less the stop words, each folded to its stem so a query's "questions" finds a
// description's "question".
func goingTerms(text string) map[string]int {
	terms := map[string]int{}
	for _, word := range strings.FieldsFunc(strings.ToLower(text), func(char rune) bool {
		return !unicode.IsLetter(char) && !unicode.IsDigit(char)
	}) {
		if len(word) < 2 || goingStopWords[word] {
			continue
		}
		terms[goingStem(word)]++
	}
	return terms
}

// goingStem folds the endings English inflects a verb and a plural with, so "resolves",
// "resolved" and "resolving" are one term. A word is left alone where the fold would leave less
// than four characters, which is where it stops being the same word.
func goingStem(word string) string {
	for _, ending := range [...]string{"ing", "ies", "ed", "es", "s"} {
		if !strings.HasSuffix(word, ending) {
			continue
		}
		stem := strings.TrimSuffix(word, ending)
		if len(stem) < 4 {
			continue
		}
		if ending == "ies" {
			return stem + "y"
		}
		return stem
	}
	return word
}

// goingIDF weighs each term by how few of the catalogue's descriptions hold it.
func goingIDF(actions []GoingAction) map[string]float64 {
	frequency := map[string]int{}
	for _, action := range actions {
		for term := range action.terms {
			frequency[term]++
		}
	}
	idf := make(map[string]float64, len(frequency))
	for term, count := range frequency {
		idf[term] = math.Log(1 + float64(len(actions))/float64(count))
	}
	return idf
}

// GoingMatch is one action of an answer, with the score that ordered it where a query ranked
// the list. A listing nothing ranked carries no score.
type GoingMatch struct {
	GoingAction
	Score float64 `json:"score,omitempty"`
}

// Search ranks the catalogue against a query by meaning over the actions' own descriptions: a
// query is read as its stems, and an action is scored by the stems its description shares with
// it, each weighed by how few descriptions hold it (goingIDF). Because the text searched is what
// the action does rather than what it is called, words sharing no term with an action's name
// still find it.
func (c GoingCatalogue) Search(query string, limit int) []GoingMatch {
	wanted := goingTerms(query)
	if len(wanted) == 0 {
		return nil
	}
	matches := make([]GoingMatch, 0, len(c.actions))
	for _, action := range c.actions {
		score := 0.0
		for term := range wanted {
			count, held := action.terms[term]
			if !held {
				continue
			}
			// A term the description repeats counts for more, but far less than a second term
			// matching: an action is about the words it uses, not about how often it uses one.
			score += c.idf[term] * (1 + math.Log(float64(count)))
		}
		if score > 0 {
			matches = append(matches, GoingMatch{GoingAction: action, Score: score})
		}
	}
	sort.Slice(matches, func(left, right int) bool {
		if matches[left].Score != matches[right].Score {
			return matches[left].Score > matches[right].Score
		}
		return matches[left].Action < matches[right].Action
	})
	if limit > 0 && len(matches) > limit {
		matches = matches[:limit]
	}
	return matches
}

// goingCatalogueBody is what GET /api/v1/going/catalogue answers: every action, or the ones a
// query ranked with the score that ordered each, every one carrying the arguments it takes.
type goingCatalogueBody struct {
	Query   string       `json:"query"`
	Total   int          `json:"total"`
	Actions []GoingMatch `json:"actions"`
}

// getGoingCatalogue serves the catalogue: every action in action order, or, with ?q=, the
// actions that query ranked, most alike first, at most ?limit= of them.
func (s *server) getGoingCatalogue(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := maxGoingSearchResults
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxGoingSearchResults {
			writeError(w, "INVALID_QUERY", http.StatusBadRequest, fmt.Sprintf("limit must be a number from 1 to %d", maxGoingSearchResults))
			return
		}
		limit = parsed
	}
	if query != "" {
		matches := s.going.Search(query, limit)
		WriteJSON(w, http.StatusOK, goingCatalogueBody{Query: query, Total: len(matches), Actions: matches})
		return
	}
	actions := s.going.Actions()
	listed := make([]GoingMatch, 0, min(len(actions), limit))
	for _, action := range actions[:min(len(actions), limit)] {
		listed = append(listed, GoingMatch{GoingAction: action})
	}
	WriteJSON(w, http.StatusOK, goingCatalogueBody{Total: len(actions), Actions: listed})
}

// GoingDoer is the one thing a caller needs of an HTTP client, so a test watches what was sent
// and whether anything was sent at all.
type GoingDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// GoingCaller performs an action an agent already chose. It holds the catalogue to the proposal
// before it builds anything: an action the catalogue does not hold, and a call the action's
// schema refuses, each answer an error of their own with no request made.
type GoingCaller struct {
	Catalogue GoingCatalogue
	// BaseURL is the Dispatch server the action is performed against, without a trailing slash.
	BaseURL string
	Doer    GoingDoer
}

// GoingUnknownActionError is the refusal of an action the catalogue does not hold. It is the
// caller's own error, raised before any request: there is nothing to send, since nothing names
// what to send it to.
type GoingUnknownActionError struct {
	Action string
}

func (e GoingUnknownActionError) Error() string {
	return fmt.Sprintf("%q is no action of the catalogue: GET %s lists every action", e.Action, goingCataloguePath)
}

// GoingArgumentError is the refusal of a call the action's own schema does not accept, raised
// before any request for the same reason.
type GoingArgumentError struct {
	Action string
	Reason string
}

func (e GoingArgumentError) Error() string {
	return fmt.Sprintf("%s: %s", e.Action, e.Reason)
}

// Do performs one catalogue action with the arguments given: the pattern's placeholders fill its
// path and every other argument becomes its JSON body. Nothing is sent until the catalogue holds
// the action and its schema accepts the call.
func (c GoingCaller) Do(ctx context.Context, action string, arguments map[string]any) (*http.Response, error) {
	entry, held := c.Catalogue.Lookup(action)
	if !held {
		return nil, GoingUnknownActionError{Action: action}
	}
	if reason := checkGoingArguments(entry.Request, arguments); reason != "" {
		return nil, GoingArgumentError{Action: action, Reason: reason}
	}
	path, body := entry.Pattern, map[string]any{}
	for name, value := range arguments {
		placeholder := "{" + name + "}"
		if strings.Contains(path, placeholder) {
			path = strings.ReplaceAll(path, placeholder, url.PathEscape(fmt.Sprint(value)))
			continue
		}
		body[name] = value
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", action, err)
	}
	request, err := http.NewRequestWithContext(ctx, entry.Method, c.BaseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	return c.Doer.Do(request)
}

// checkGoingArguments holds a proposed call to one action's schema: every required argument is
// named, every named argument is one the action takes, and each value is what its field holds.
// The answer is the refusal, empty for a call the schema accepts.
func checkGoingArguments(fields []contracts.GoingField, arguments map[string]any) string {
	declared := make(map[string]contracts.GoingField, len(fields))
	for _, field := range fields {
		declared[field.Name] = field
		value, named := arguments[field.Name]
		if !named {
			if field.Required {
				return "argument " + strconv.Quote(field.Name) + " is required"
			}
			continue
		}
		if reason := checkGoingValue(field, value); reason != "" {
			return "argument " + strconv.Quote(field.Name) + " " + reason
		}
	}
	names := make([]string, 0, len(arguments))
	for name := range arguments {
		if _, takes := declared[name]; !takes {
			names = append(names, strconv.Quote(name))
		}
	}
	if len(names) > 0 {
		sort.Strings(names)
		return "this action takes no argument " + strings.Join(names, ", ")
	}
	return ""
}

// checkGoingValue answers why value is not what field holds, empty where it is.
func checkGoingValue(field contracts.GoingField, value any) string {
	if value == nil {
		if field.Nullable {
			return ""
		}
		return "is null, which it may not be"
	}
	switch field.Kind {
	case contracts.GoingFieldString:
		if _, ok := value.(string); !ok {
			return "must be a string"
		}
	case contracts.GoingFieldEnum:
		text, ok := value.(string)
		if !ok || !slices.Contains(field.Values, text) {
			return "must be one of " + strings.Join(field.Values, ", ")
		}
	case contracts.GoingFieldNumber:
		switch value.(type) {
		case float64, float32, int, int64, json.Number:
		default:
			return "must be a number"
		}
	case contracts.GoingFieldBoolean:
		if _, ok := value.(bool); !ok {
			return "must be true or false"
		}
	case contracts.GoingFieldArray:
		items, ok := value.([]any)
		if !ok {
			return "must be an array"
		}
		for index, item := range items {
			if reason := checkGoingValue(*field.Item, item); reason != "" {
				return fmt.Sprintf("item %d %s", index, reason)
			}
		}
	case contracts.GoingFieldObject:
		nested, ok := value.(map[string]any)
		if !ok {
			return "must be an object"
		}
		if reason := checkGoingArguments(field.Fields, nested); reason != "" {
			return "holds no valid value: " + reason
		}
	case contracts.GoingFieldAny:
	}
	return ""
}
