package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// PostgreSQL's text and jsonb cannot hold U+0000, nor its text a byte sequence that is not UTF-8,
// so any statement given caller text holding either fails. The API's input layer refuses such text
// before anything is written: a JSON body when it is decoded (decodeJSON, and the two readers that
// read one themselves: an artifact upload's and a credential relay's), a multipart upload's fields
// and its markdown file, every path and query parameter, and the actor a document websocket's
// bearer names (the last two wrapped around every route in Register). Each refusal is 400,
// NUL_CHARACTER or INVALID_UTF8, naming where the caller wrote the character. A JSON body can only
// hold the first, since decoding writes each byte that is not UTF-8 as U+FFFD. A browser's edit
// reaches a document past this layer: the document's update decoder refuses text that is not UTF-8,
// and the text Dispatch takes from the document's tree carries a U+0000 as U+FFFD
// (pmdoc.NulAsReplacement).

// unstorableText refuses value when it holds U+0000 or a byte that is not UTF-8, naming field and
// where the first such character stands, counted from 1 in UTF-16 units as the caps count (len16).
func unstorableText(field, value string) *apiError {
	if strings.IndexByte(value, 0) < 0 && utf8.ValidString(value) {
		return nil
	}
	for at := 0; at < len(value); {
		char, width := utf8.DecodeRuneInString(value[at:])
		switch {
		case char == 0:
			return errorf(http.StatusBadRequest, "NUL_CHARACTER",
				"%s holds a NUL character (U+0000) at character %d, which Dispatch cannot store", field, len16(value[:at])+1)
		case char == utf8.RuneError && width == 1:
			return errorf(http.StatusBadRequest, "INVALID_UTF8",
				"%s holds a byte that is not UTF-8 (0x%02X) at character %d, which Dispatch cannot store", field, value[at], len16(value[:at])+1)
		}
		at += width
	}
	return nil
}

// nulEscape is the only way JSON carries U+0000: a decoder refuses the raw control character.
var nulEscape = []byte(`\u0000`)

// unstorableJSON refuses the first string of the JSON value data opens with - a member name or a
// value, an object's members read in name order - that holds U+0000, naming it as the caller wrote
// it under prefix: title, options[1].label, ops[0].attributes.title, or prefix itself, "body" when
// prefix is empty, for a bare string. A body without the escape is passed over unread. The value is
// read as a route's own decoder reads it, the first value in data and the last of a repeated member,
// so what is checked is what is kept; data that does not decode holds nothing a route keeps.
func unstorableJSON(prefix string, data []byte) *apiError {
	if !bytes.Contains(data, nulEscape) {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil
	}
	return unstorableJSONValue(prefix, value)
}

func unstorableJSONValue(path string, value any) *apiError {
	switch value := value.(type) {
	case string:
		return unstorableText(jsonField(path), value)
	case []any:
		for index, element := range value {
			if refusal := unstorableJSONValue(fmt.Sprintf("%s[%d]", path, index), element); refusal != nil {
				return refusal
			}
		}
	case map[string]any:
		for _, name := range slices.Sorted(maps.Keys(value)) {
			member := strings.ReplaceAll(name, "\x00", `\u0000`)
			if path != "" {
				member = path + "." + member
			}
			if refusal := unstorableText(member, name); refusal != nil {
				return refusal
			}
			if refusal := unstorableJSONValue(member, value[name]); refusal != nil {
				return refusal
			}
		}
	}
	return nil
}

// jsonField names a JSON value at path, "body" for the body itself.
func jsonField(path string) string {
	if path == "" {
		return "body"
	}
	return path
}

// unstorableForm refuses a multipart form field holding text unstorableText refuses, naming the
// field.
func unstorableForm(values map[string][]string) *apiError {
	for _, name := range slices.Sorted(maps.Keys(values)) {
		for _, value := range values[name] {
			if refusal := unstorableText(name, value); refusal != nil {
				return refusal
			}
		}
	}
	return nil
}

// patternWildcard is a ServeMux pattern's {name} or {name...}.
var patternWildcard = regexp.MustCompile(`\{([^}.$]+)(?:\.\.\.)?\}`)

// refuseUnstorableParameters serves handler only when none of the request's path or query
// parameters holds text unstorableText refuses: a parameter reaches a query as text, where
// PostgreSQL refuses it on a read as on a write. pattern is the route's ServeMux pattern, whose
// wildcards name the path parameters.
func (s *server) refuseUnstorableParameters(pattern string, handler http.HandlerFunc) http.HandlerFunc {
	var wildcards []string
	for _, match := range patternWildcard.FindAllStringSubmatch(pattern, -1) {
		wildcards = append(wildcards, match[1])
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if refusal := unstorableParameter(r, wildcards); refusal != nil {
			s.writeHandlerError(w, refusal)
			return
		}
		handler(w, r)
	}
}

func unstorableParameter(r *http.Request, wildcards []string) *apiError {
	for _, wildcard := range wildcards {
		if refusal := unstorableText("path parameter "+wildcard, r.PathValue(wildcard)); refusal != nil {
			return refusal
		}
	}
	query := r.URL.Query()
	for _, name := range slices.Sorted(maps.Keys(query)) {
		field := "query parameter " + strings.ReplaceAll(name, "\x00", `\u0000`)
		if refusal := unstorableText(field, name); refusal != nil {
			return refusal
		}
		for _, value := range query[name] {
			if refusal := unstorableText(field, value); refusal != nil {
				return refusal
			}
		}
	}
	return nil
}

// documentActorHeader is the header a document websocket's bearer names its session in, as JSON
// (docs.Service.requestActor).
const documentActorHeader = "X-Dispatch-Actor"

// refuseUnstorableActor serves a document websocket's handler only when the actor its bearer names
// holds no U+0000: the header is JSON, whose escape spells one past net/http's check of the header
// itself, and the actor's id and origin are the author of every version the connection's edits
// make.
func (s *server) refuseUnstorableActor(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if refusal := unstorableJSON(documentActorHeader, []byte(r.Header.Get(documentActorHeader))); refusal != nil {
			s.writeHandlerError(w, refusal)
			return
		}
		handler(w, r)
	}
}
