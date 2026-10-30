package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
)

// declaredMembers holds the JSON member names one struct type declares, its embedded structs'
// included, keyed by reflect.Type: a route's body type is read once, not on every request.
var declaredMembers sync.Map

// membersDeclaredBy answers the JSON member names t declares, or nil when t is not a struct (a
// pointer is followed).
func membersDeclaredBy(t reflect.Type) map[string]struct{} {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	if cached, ok := declaredMembers.Load(t); ok {
		return cached.(map[string]struct{})
	}
	names := make(map[string]struct{})
	collectDeclaredMembers(t, names)
	cached, _ := declaredMembers.LoadOrStore(t, names)
	return cached.(map[string]struct{})
}

// collectDeclaredMembers adds to names each member encoding/json decodes into t: an exported
// field by its tag's name or else its own, and the members of an embedded struct with no tag name.
func collectDeclaredMembers(t reflect.Type, names map[string]struct{}) {
	for i := range t.NumField() {
		field := t.Field(i)
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if field.Anonymous && name == "" {
			embedded := field.Type
			if embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				collectDeclaredMembers(embedded, names)
				continue
			}
		}
		if !field.IsExported() {
			continue
		}
		if name == "" {
			name = field.Name
		}
		names[name] = struct{}{}
	}
}

// undeclaredMembers is the top-level members of body that value's type does not declare, sorted,
// and nil when value is no struct or body is no JSON object. encoding/json matches a member to a
// field ignoring case, so this does too.
func undeclaredMembers(value any, body []byte) []string {
	declared := membersDeclaredBy(reflect.TypeOf(value))
	if declared == nil {
		return nil
	}
	var members map[string]json.RawMessage
	if json.Unmarshal(body, &members) != nil {
		return nil
	}
	var undeclared []string
	for member := range members {
		if !declaresMember(declared, member) {
			undeclared = append(undeclared, member)
		}
	}
	slices.Sort(undeclared)
	return undeclared
}

// logUndeclaredMembers logs at debug, with r's route pattern, the top-level members of body that
// value's type does not declare, so a caller sending a member this build ignores can be found. It
// does no work while debug logging is off.
func logUndeclaredMembers(r *http.Request, value any, body []byte) {
	ctx := r.Context()
	if !slog.Default().Enabled(ctx, slog.LevelDebug) {
		return
	}
	undeclared := undeclaredMembers(value, body)
	if len(undeclared) == 0 {
		return
	}
	slog.DebugContext(ctx, "dispatch: request body members the route does not declare were ignored", "route", r.Pattern, "members", undeclared)
}

func declaresMember(declared map[string]struct{}, member string) bool {
	if _, ok := declared[member]; ok {
		return true
	}
	for name := range declared {
		if strings.EqualFold(name, member) {
			return true
		}
	}
	return false
}
