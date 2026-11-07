package api

// going_catalogue.go holds the catalogue of operations an agent may propose on Dispatch's
// graph, and serves it at GET /api/v1/going/catalogue.
//
// The catalogue is the dispatch tools that write: the specs in
// packages/contracts/src/dispatch-tools.ts that agents already operate Dispatch with all day,
// each carried whole through contracts.DispatchTools — its own curated description, which is
// what a model chooses on, and its own argument schema. Twenty of the thirty-three tools write,
// which is under the count where a model's choice measurably falls apart, so the list is read
// whole: nothing searches it, and no second schema is authored per route.
//
// Which tools write is not a field a tool spec carries. goingToolRoutes names the rows of the
// server's own table each tool writes, and buildGoingCatalogue holds every one of them to the
// table it mounts: a row that is no row of that table, one the table does not class as a write
// (mutatingRoutes, the set TestEveryMutatingRouteIsClassed reads), a placeholder nothing fills,
// or a relation in neither edge family refuses to mount (register). The other direction is a
// report rather than a refusal, since a write no tool performs is work still to do and not an
// error: TestTheMutatingRoutesNoToolSpecNames names every one of them.
//
// Nothing here decides anything: the catalogue says what an action is and what it takes, and
// GoingCaller performs one an agent already chose. The loop that chooses is not in this package.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// goingCataloguePath is the route that serves the catalogue, named once so the refusal of an
// action the catalogue does not hold can point at the list without spelling it again.
const goingCataloguePath = "/api/v1/going/catalogue"

// goingCallerSession stands, where a row's path binding names it, for the agent session the
// caller acts as rather than for an argument of the tool: a follow records the calling session
// as the ask's follower, and a tool argument names no session at all.
const goingCallerSession = "@session"

// goingToolRoute is one row of the server's table a tool writes: how a call fills the row's
// path, the argument whose value picks the row where a tool writes two that differ by what it
// was asked to do, and the graph relations the write makes.
type goingToolRoute struct {
	Method  string
	Pattern string
	// Path binds each placeholder of Pattern to the tool argument that fills it, or to
	// goingCallerSession. Every placeholder is bound and every binding is a placeholder, which
	// buildGoingCatalogue holds the table to.
	Path map[string]string
	// WhenArgument and WhenValue pick this row among the tool's: a claim and its release are
	// one tool and two rows, told apart by `release`, as a follow and its unfollow are by
	// `action`. An empty WhenArgument is a row nothing guards.
	WhenArgument string
	WhenValue    any
	// Relations are the graph relations this row's write makes, each a structural edge kind
	// (model.GraphEdgeKinds) or a `block[]` attribute of a typed block (pmdoc.LinkKinds).
	Relations []string
}

// goingToolRoutes is the rows of the server's table each dispatch tool writes, by the tool's own
// name. A tool this map does not name writes nothing, so the catalogue does not hold it: the
// read tools, and `dispatch_related`, whose POST row writes nothing (it runs the related-item
// check and records none of it).
//
// The rows of one tool are tried in order, so the row a call performs is the first whose guard
// it satisfies and whose placeholders it fills: a document write names its issue, else its
// project, else the document itself.
var goingToolRoutes = map[string][]goingToolRoute{
	"dispatch_issue": {{
		Method: http.MethodPost, Pattern: "/api/v1/issues", Path: map[string]string{},
		Relations: []string{"child_of", "blocked_by", "affects", "mentions"},
	}},
	"dispatch_issue_update": {{
		Method: http.MethodPatch, Pattern: "/api/v1/issues/{key}",
		Path:      map[string]string{"key": "issue"},
		Relations: []string{"child_of", "blocked_by", "affects"},
	}},
	"dispatch_claim": {
		{
			Method: http.MethodDelete, Pattern: "/api/v1/issues/{key}/claim",
			Path: map[string]string{"key": "issue"}, WhenArgument: "release", WhenValue: true,
		},
		{
			Method: http.MethodPost, Pattern: "/api/v1/issues/{key}/claim",
			Path: map[string]string{"key": "issue"},
		},
	},
	"dispatch_ask": {
		{
			Method: http.MethodPost, Pattern: "/api/v1/issues/{key}/asks",
			Path:      map[string]string{"key": "issue"},
			Relations: []string{"owned_by", "anchored_to", "followed_by", "mentions"},
		},
		{
			Method: http.MethodPost, Pattern: "/api/v1/artifacts/{id}/asks",
			Path:      map[string]string{"id": "artifact"},
			Relations: []string{"owned_by", "anchored_to", "followed_by", "mentions"},
		},
	},
	"dispatch_edit_ask": {{
		Method: http.MethodPatch, Pattern: "/api/v1/asks/{id}",
		Path: map[string]string{"id": "ask"}, Relations: []string{"mentions"},
	}},
	"dispatch_resolve_ask": {{
		Method: http.MethodPost, Pattern: "/api/v1/asks/{id}/resolve",
		Path: map[string]string{"id": "ask"},
	}},
	"dispatch_resolve_comment": {{
		Method: http.MethodPost, Pattern: "/api/v1/comments/{id}/resolve",
		Path: map[string]string{"id": "comment"},
	}},
	"dispatch_follow": {
		{
			Method: http.MethodDelete, Pattern: "/api/v1/asks/{id}/followers/{session_id}",
			Path:         map[string]string{"id": "ask", "session_id": goingCallerSession},
			WhenArgument: "action", WhenValue: "unfollow",
			Relations: []string{"followed_by"},
		},
		{
			Method: http.MethodPut, Pattern: "/api/v1/asks/{id}/followers/{session_id}",
			Path:      map[string]string{"id": "ask", "session_id": goingCallerSession},
			Relations: []string{"followed_by"},
		},
	},
	"dispatch_comment": {
		{
			Method: http.MethodPost, Pattern: "/api/v1/issues/{key}/comments",
			Path:      map[string]string{"key": "issue"},
			Relations: []string{"owned_by", "anchored_to", "answers", "replies_to", "mentions"},
		},
		{
			Method: http.MethodPost, Pattern: "/api/v1/artifacts/{id}/comments",
			Path:      map[string]string{"id": "artifact"},
			Relations: []string{"owned_by", "anchored_to", "answers", "replies_to", "mentions"},
		},
	},
	"dispatch_suggest": {
		{
			Method: http.MethodPost, Pattern: "/api/v1/issues/{key}/comments",
			Path:      map[string]string{"key": "issue"},
			Relations: []string{"owned_by", "anchored_to", "mentions"},
		},
		{
			Method: http.MethodPost, Pattern: "/api/v1/artifacts/{id}/comments",
			Path:      map[string]string{"id": "artifact"},
			Relations: []string{"owned_by", "anchored_to", "mentions"},
		},
	},
	"dispatch_message": {
		{
			Method: http.MethodPost, Pattern: "/api/v1/issues/{key}/messages",
			Path:      map[string]string{"key": "issue"},
			Relations: []string{"replies_to", "mentions"},
		},
		{
			Method: http.MethodPost, Pattern: "/api/v1/messages/{id}/reply",
			Path:      map[string]string{"id": "in_reply_to"},
			Relations: []string{"replies_to", "mentions"},
		},
	},
	// The issue-less send to one session. Its path names the session the tool was asked to
	// reach, not the calling session, so the placeholder binds to the tool's own argument. The
	// body is indexed without its references counted (indexMessage: a message with no issue
	// takes refs.IndexItem), so this write makes no mention edge; a send naming in_reply_to
	// makes the replies_to edge every message reply makes.
	"dispatch_send_to_agent": {{
		Method: http.MethodPost, Pattern: "/api/v1/agents/{session_id}/messages",
		Path: map[string]string{"session_id": "session"}, Relations: []string{"replies_to"},
	}},
	"dispatch_doc_edit": {
		{
			Method: http.MethodPost, Pattern: "/api/v1/issues/{key}/artifacts/{slug}/edits",
			Path:      map[string]string{"key": "issue", "slug": "artifact"},
			Relations: []string{"mentions", "needs", "serves"},
		},
		{
			Method: http.MethodPost, Pattern: "/api/v1/projects/{key}/artifacts/{slug}/edits",
			Path:      map[string]string{"key": "project", "slug": "artifact"},
			Relations: []string{"mentions", "needs", "serves"},
		},
		{
			Method: http.MethodPost, Pattern: "/api/v1/artifacts/{id}/edits",
			Path:      map[string]string{"id": "artifact"},
			Relations: []string{"mentions", "needs", "serves"},
		},
	},
	"dispatch_request_approval": {{
		Method: http.MethodPost, Pattern: "/api/v1/artifacts/{id}/approval-requests",
		Path: map[string]string{"id": "artifact"}, Relations: []string{"owned_by"},
	}},
	"dispatch_plan_approve": {{
		Method: http.MethodPost, Pattern: "/api/v1/artifacts/{id}/plan-reviews",
		Path: map[string]string{"id": "artifact"},
	}},
	"dispatch_result": {{
		Method: http.MethodPost, Pattern: "/api/v1/issues/{key}/results",
		Path: map[string]string{"key": "issue"},
	}},
	"dispatch_artifact": {
		{
			Method: http.MethodPost, Pattern: "/api/v1/issues/{key}/artifacts",
			Path:      map[string]string{"key": "issue"},
			Relations: []string{"attached_to", "mentions"},
		},
		{
			Method: http.MethodPost, Pattern: "/api/v1/projects/{key}/artifacts",
			Path:      map[string]string{"key": "project"},
			Relations: []string{"mentions"},
		},
		{
			Method: http.MethodPost, Pattern: "/api/v1/agents/{session_id}/artifacts",
			Path: map[string]string{"session_id": goingCallerSession},
		},
	},
	"dispatch_project": {{
		Method: http.MethodPost, Pattern: "/api/v1/projects", Path: map[string]string{},
	}},
	"dispatch_architecture_sync": {{
		Method: http.MethodPost, Pattern: "/api/v1/projects/{key}/architecture-sources/sync",
		Path: map[string]string{"key": "project"}, Relations: []string{"depends_on", "affects"},
	}},
	"dispatch_handoff_write": {{
		Method: http.MethodPost, Pattern: "/api/v1/issues/{key}/handoffs/{phase}",
		Path: map[string]string{"key": "issue", "phase": "phase"},
	}},
}

// GoingRoute is one row of the server's table an action writes, as the catalogue serves it.
type GoingRoute struct {
	Method  string `json:"method"`
	Pattern string `json:"pattern"`

	// path and the guard are how a call reaches this row (goingToolRoute); they are the
	// caller's business rather than the reader's, so neither is served.
	path         map[string]string
	whenArgument string
	whenValue    any
}

// GoingAction is one action an agent may propose: a dispatch tool that writes, with the
// description and the arguments the tool spec itself states.
type GoingAction struct {
	// Action is the catalogue's key: the tool's own name, such as "dispatch_issue".
	Action string `json:"action"`
	// Description is the tool spec's own description, whole and unchanged. It is what a model
	// chooses on, and the confidence threshold and the model bake-off are tuned against these
	// words, so nothing here shortens or rewrites them.
	Description string `json:"description"`
	// Routes are the rows of the server's table this action writes, in the order a call is
	// matched against them.
	Routes []GoingRoute `json:"routes"`
	// Relations is every graph relation this action's verbs write, over all its rows, each a
	// structural edge kind (model.GraphEdgeKinds) or a `block[]` attribute of a typed block
	// (pmdoc.LinkKinds).
	Relations []string `json:"relations,omitempty"`
	// Request is the action's arguments: the tool spec's own schema.
	Request []contracts.ToolField `json:"request"`
	// Example is a call this action's schema accepts, the tool spec's own.
	Example json.RawMessage `json:"example"`
}

// GoingCatalogue is every action an agent may propose, read whole.
type GoingCatalogue struct {
	actions []GoingAction
	// byAction is the index into actions of each action's name.
	byAction map[string]int
}

// Actions is the catalogue in the order the tool specs state the tools.
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
// server has, and the catalogue is held to it.
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

// edgeFamilies is every relation name a catalogue entry may declare: the global relation names
// and the `block[]` attributes of the typed blocks, which are the two families of edge the graph
// holds and the two GET /api/v1/references?kind= accepts (references.go).
func edgeFamilies() []string {
	return append(slices.Clone(model.GraphEdgeKinds), pmdoc.LinkKinds()...)
}

// goingPlaceholders is the placeholders of a pattern, in the order it spells them.
func goingPlaceholders(pattern string) []string {
	var placeholders []string
	for rest := pattern; ; {
		opened := strings.Index(rest, "{")
		if opened < 0 {
			return placeholders
		}
		rest = rest[opened+1:]
		closed := strings.Index(rest, "}")
		if closed < 0 {
			return placeholders
		}
		placeholders = append(placeholders, rest[:closed])
		rest = rest[closed+1:]
	}
}

// BuildGoingCatalogue is the catalogue of the server's own route table, with no test hooks: what
// `envoy-dispatch` serves and what a caller outside a request builds. register builds the
// server's from the table it mounts.
func BuildGoingCatalogue() (GoingCatalogue, error) {
	return buildGoingCatalogue((&server{}).routes())
}

// buildGoingCatalogue is one entry per dispatch tool that writes, and an error naming every row
// the catalogue cannot hold: a row that is no row of this table, a row the table does not class
// as a write, a placeholder no argument of the tool fills, a relation in neither edge family,
// and a tool name no tool spec has. register refuses to mount a table it answers an error for,
// so a catalogue entry can only ever name an action the server serves.
func buildGoingCatalogue(routes []apiRoute) (GoingCatalogue, error) {
	mutating := mutatingRoutes(routes)
	served := make(map[string]bool, len(routes))
	for _, route := range routes {
		served[route.Method+" "+route.Pattern] = true
	}
	families := edgeFamilies()
	catalogue := GoingCatalogue{
		actions:  make([]GoingAction, 0, len(goingToolRoutes)),
		byAction: make(map[string]int, len(goingToolRoutes)),
	}
	var refusals []string
	for _, tool := range contracts.DispatchTools {
		rows, writes := goingToolRoutes[tool.Name]
		if !writes {
			continue
		}
		takes := make(map[string]bool, len(tool.Arguments))
		for _, argument := range tool.Arguments {
			takes[argument.Name] = true
		}
		entry := GoingAction{
			Action:      tool.Name,
			Description: tool.Description,
			Routes:      make([]GoingRoute, 0, len(rows)),
			Request:     tool.Arguments,
			Example:     tool.Example,
		}
		for _, row := range rows {
			name := row.Method + " " + row.Pattern
			switch {
			case !served[name]:
				refusals = append(refusals, fmt.Sprintf("%s: %s is no row of the route table", tool.Name, name))
			case !mutating[name]:
				refusals = append(refusals, fmt.Sprintf("%s: %s is no write of the route table", tool.Name, name))
			}
			for _, placeholder := range goingPlaceholders(row.Pattern) {
				if _, bound := row.Path[placeholder]; !bound {
					refusals = append(refusals, fmt.Sprintf("%s: %s binds nothing to {%s}", tool.Name, name, placeholder))
				}
			}
			for placeholder, argument := range row.Path {
				if !strings.Contains(row.Pattern, "{"+placeholder+"}") {
					refusals = append(refusals, fmt.Sprintf("%s: %s has no placeholder {%s}", tool.Name, name, placeholder))
				}
				if argument != goingCallerSession && !takes[argument] {
					refusals = append(refusals, fmt.Sprintf("%s: {%s} is bound to %q, which is no argument of the tool", tool.Name, placeholder, argument))
				}
			}
			if row.WhenArgument != "" && !takes[row.WhenArgument] {
				refusals = append(refusals, fmt.Sprintf("%s: %s is picked by %q, which is no argument of the tool", tool.Name, name, row.WhenArgument))
			}
			for _, relation := range row.Relations {
				if !slices.Contains(families, relation) {
					refusals = append(refusals, fmt.Sprintf("%s: relation %q is neither a structural edge kind (%s) nor a block[] attribute (%s)",
						tool.Name, relation, strings.Join(model.GraphEdgeKinds, ", "), strings.Join(pmdoc.LinkKinds(), ", ")))
				}
				if !slices.Contains(entry.Relations, relation) {
					entry.Relations = append(entry.Relations, relation)
				}
			}
			entry.Routes = append(entry.Routes, GoingRoute{
				Method:       row.Method,
				Pattern:      row.Pattern,
				path:         row.Path,
				whenArgument: row.WhenArgument,
				whenValue:    row.WhenValue,
			})
		}
		catalogue.byAction[tool.Name] = len(catalogue.actions)
		catalogue.actions = append(catalogue.actions, entry)
	}
	for name := range goingToolRoutes {
		if _, held := catalogue.byAction[name]; !held {
			refusals = append(refusals, name+": no dispatch tool of that name (packages/contracts/src/dispatch-tools.ts)")
		}
	}
	if len(refusals) > 0 {
		sort.Strings(refusals)
		return GoingCatalogue{}, fmt.Errorf("%d catalogue entry problem(s):\n%s", len(refusals), strings.Join(refusals, "\n"))
	}
	return catalogue, nil
}

// goingCatalogueBody is what GET /api/v1/going/catalogue answers: every action, each carrying
// the description it is chosen on and the arguments it takes.
type goingCatalogueBody struct {
	Total   int           `json:"total"`
	Actions []GoingAction `json:"actions"`
}

// getGoingCatalogue serves the catalogue whole. Twenty actions is a list a reader scans, so
// there is nothing to search and nothing to page.
func (s *server) getGoingCatalogue(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	actions := s.going.Actions()
	WriteJSON(w, http.StatusOK, goingCatalogueBody{Total: len(actions), Actions: actions})
}

// GoingDoer is the one thing a caller needs of an HTTP client, so a test watches what was sent
// and whether anything was sent at all.
type GoingDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// GoingCaller performs an action an agent already chose. It holds the catalogue to the proposal
// before it builds anything: an action the catalogue does not hold, a call the action's schema
// refuses, and a call no row of the action takes, each answer an error of their own with no
// request made.
type GoingCaller struct {
	Catalogue GoingCatalogue
	// BaseURL is the Dispatch server the action is performed against, without a trailing slash.
	BaseURL string
	// Session is the agent session the caller acts as, which fills the rows addressing it: a
	// follow records this session as the ask's follower. A caller that is no session leaves it
	// empty, and the rows needing one are passed over.
	Session string
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

// Do performs one catalogue action with the arguments given: the action's first row the call
// fills (chooseGoingRoute) takes its path from the arguments its placeholders name, and every
// other argument becomes the request's JSON body. Nothing is sent until the catalogue holds the
// action and the tool's own schema accepts the call. An argument naming a document by slug is
// the caller's to resolve to the id a row's path takes.
func (c GoingCaller) Do(ctx context.Context, action string, arguments map[string]any) (*http.Response, error) {
	entry, held := c.Catalogue.Lookup(action)
	if !held {
		return nil, GoingUnknownActionError{Action: action}
	}
	if reason := checkGoingArguments(entry.Request, arguments); reason != "" {
		return nil, GoingArgumentError{Action: action, Reason: reason}
	}
	route, path, chosen := chooseGoingRoute(entry, arguments, c.Session)
	if !chosen {
		return nil, GoingArgumentError{Action: action, Reason: goingNoRouteReason(entry)}
	}
	inPath := make(map[string]bool, len(route.path))
	for _, argument := range route.path {
		inPath[argument] = true
	}
	body := map[string]any{}
	for name, value := range arguments {
		if inPath[name] {
			continue
		}
		body[name] = value
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", action, err)
	}
	request, err := http.NewRequestWithContext(ctx, route.Method, c.BaseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	return c.Doer.Do(request)
}

// chooseGoingRoute is the row a call performs and the path it performs it on: the action's first
// row whose guard the call satisfies and every one of whose placeholders the call fills, so a
// document write naming its issue takes the issue's row and the same write naming the document
// alone takes the document's.
func chooseGoingRoute(entry GoingAction, arguments map[string]any, session string) (GoingRoute, string, bool) {
	for _, route := range entry.Routes {
		if route.whenArgument != "" && arguments[route.whenArgument] != route.whenValue {
			continue
		}
		path, filled := route.Pattern, true
		for placeholder, argument := range route.path {
			value := session
			if argument != goingCallerSession {
				named, held := arguments[argument]
				text, isText := named.(string)
				if !held || !isText {
					filled = false
					break
				}
				value = text
			}
			if value == "" {
				filled = false
				break
			}
			path = strings.ReplaceAll(path, "{"+placeholder+"}", url.PathEscape(value))
		}
		if filled {
			return route, path, true
		}
	}
	return GoingRoute{}, "", false
}

// goingNoRouteReason says what each of the action's rows wanted and the call did not name, so a
// proposal missing the one argument that chooses a row is told which.
func goingNoRouteReason(entry GoingAction) string {
	wants := make([]string, 0, len(entry.Routes))
	for _, route := range entry.Routes {
		named := make([]string, 0, len(route.path))
		for _, argument := range route.path {
			if argument == goingCallerSession {
				argument = "the calling session"
			}
			named = append(named, argument)
		}
		sort.Strings(named)
		want := route.Method + " " + route.Pattern
		if len(named) > 0 {
			want += " needs " + strings.Join(named, ", ")
		}
		if route.whenArgument != "" {
			want += fmt.Sprintf(" with %s %v", route.whenArgument, route.whenValue)
		}
		wants = append(wants, want)
	}
	return "these arguments name no route of this action: " + strings.Join(wants, "; ")
}

// checkGoingArguments holds a proposed call to one action's schema: every required argument is
// named, every named argument is one the action takes, and each value is what its field holds.
// The answer is the refusal, empty for a call the schema accepts.
func checkGoingArguments(fields []contracts.ToolField, arguments map[string]any) string {
	// The names first, whole: the "takes no argument" check below reads every field, not the
	// ones a refusal did not stop at.
	declared := make(map[string]contracts.ToolField, len(fields))
	for _, field := range fields {
		declared[field.Name] = field
	}
	for _, field := range fields {
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
func checkGoingValue(field contracts.ToolField, value any) string {
	if value == nil {
		if field.Nullable {
			return ""
		}
		return "is null, which it may not be"
	}
	switch field.Kind {
	case contracts.ToolFieldString:
		if _, ok := value.(string); !ok {
			return "must be a string"
		}
	case contracts.ToolFieldEnum:
		text, ok := value.(string)
		if !ok || !slices.Contains(field.Values, text) {
			return "must be one of " + strings.Join(field.Values, ", ")
		}
	case contracts.ToolFieldNumber:
		switch value.(type) {
		case float64, float32, int, int64, json.Number:
		default:
			return "must be a number"
		}
	case contracts.ToolFieldBoolean:
		if _, ok := value.(bool); !ok {
			return "must be true or false"
		}
	case contracts.ToolFieldArray:
		items, ok := value.([]any)
		if !ok {
			return "must be an array"
		}
		for index, item := range items {
			if reason := checkGoingValue(*field.Item, item); reason != "" {
				return fmt.Sprintf("item %d %s", index, reason)
			}
		}
	case contracts.ToolFieldObject:
		nested, ok := value.(map[string]any)
		if !ok {
			return "must be an object"
		}
		if reason := checkGoingArguments(field.Fields, nested); reason != "" {
			return "holds no valid value: " + reason
		}
	case contracts.ToolFieldAny:
	}
	return ""
}
