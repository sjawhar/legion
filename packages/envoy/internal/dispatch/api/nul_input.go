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
)

// PostgreSQL's text and jsonb cannot hold U+0000, so any statement given caller text holding one
// fails. The API's input layer refuses such text before anything is written: a JSON body when it
// is decoded (decodeJSON, and the two readers that read one themselves: an artifact upload's and a
// credential relay's), a multipart upload's fields and its markdown file, and every path and query
// parameter (Register). Each refusal is 400 NUL_CHARACTER naming where the caller wrote the
// character. A browser's edit reaches a document past this layer; the text Dispatch takes from the
// document's tree carries a U+0000 as U+FFFD (pmdoc.NulAsReplacement).

// nulCharacter refuses value when it holds U+0000, naming field and the character's position,
// counted from 1 in UTF-16 units as the caps count (len16).
func nulCharacter(field, value string) *apiError {
	at := strings.IndexByte(value, 0)
	if at < 0 {
		return nil
	}
	return errorf(http.StatusBadRequest, "NUL_CHARACTER",
		"%s holds a NUL character (U+0000) at character %d, which Dispatch cannot store", field, len16(value[:at])+1)
}

// nulEscape is the only way JSON carries U+0000: a decoder refuses the raw control character.
var nulEscape = []byte(`\u0000`)

// jsonStep is one level of where a JSON value stands: an element of an array, or a member of an
// object, which next reads a member name until it reads that member's value.
type jsonStep struct {
	array    bool
	index    int
	member   string
	readName bool
}

// nulInJSON refuses the first string of the JSON value data opens with - a member name or a
// value - that holds U+0000, naming it as the caller wrote it under prefix: title,
// options[1].label, ops[0].attributes.title. A body without the escape is passed over unread.
// data has already decoded, so a token error only ends the walk.
func nulInJSON(prefix string, data []byte) *apiError {
	if !bytes.Contains(data, nulEscape) {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var steps []jsonStep
	// valueEnded moves past the value that just ended, and reports whether it was the whole body.
	valueEnded := func() bool {
		if len(steps) == 0 {
			return true
		}
		top := &steps[len(steps)-1]
		if top.array {
			top.index++
		} else {
			top.readName = true
		}
		return false
	}
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil
		}
		if n := len(steps); n > 0 && steps[n-1].readName {
			if token == json.Delim('}') {
				steps = steps[:n-1]
				if valueEnded() {
					return nil
				}
				continue
			}
			name, _ := token.(string)
			steps[n-1].member, steps[n-1].readName = strings.ReplaceAll(name, "\x00", `\u0000`), false
			if refusal := nulCharacter(jsonPath(prefix, steps), name); refusal != nil {
				return refusal
			}
			continue
		}
		switch value := token.(type) {
		case json.Delim:
			switch value {
			case '{':
				steps = append(steps, jsonStep{readName: true})
				continue
			case '[':
				steps = append(steps, jsonStep{array: true})
				continue
			default: // ']': an object's '}' comes where it reads a member name, above
				steps = steps[:len(steps)-1]
			}
		case string:
			if refusal := nulCharacter(jsonPath(prefix, steps), value); refusal != nil {
				return refusal
			}
		}
		if valueEnded() {
			return nil
		}
	}
}

// jsonPath names where steps stand under prefix, or "body" for the body itself.
func jsonPath(prefix string, steps []jsonStep) string {
	var path strings.Builder
	path.WriteString(prefix)
	for _, step := range steps {
		if step.array {
			fmt.Fprintf(&path, "[%d]", step.index)
			continue
		}
		if path.Len() > 0 {
			path.WriteByte('.')
		}
		path.WriteString(step.member)
	}
	if path.Len() == 0 {
		return "body"
	}
	return path.String()
}

// nulInForm refuses a multipart form field holding U+0000, naming the field.
func nulInForm(values map[string][]string) *apiError {
	for _, name := range slices.Sorted(maps.Keys(values)) {
		for _, value := range values[name] {
			if refusal := nulCharacter(name, value); refusal != nil {
				return refusal
			}
		}
	}
	return nil
}

// patternWildcard is a ServeMux pattern's {name} or {name...}.
var patternWildcard = regexp.MustCompile(`\{([^}.$]+)(?:\.\.\.)?\}`)

// refuseNulParameters serves handler only when none of the request's path or query parameters
// holds U+0000: a parameter reaches a query as text, where PostgreSQL refuses it on a read as on
// a write. pattern is the route's ServeMux pattern, whose wildcards name the path parameters.
func refuseNulParameters(pattern string, handler http.HandlerFunc) http.HandlerFunc {
	var wildcards []string
	for _, match := range patternWildcard.FindAllStringSubmatch(pattern, -1) {
		wildcards = append(wildcards, match[1])
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if refusal := nulInParameters(r, wildcards); refusal != nil {
			writeError(w, refusal.code, refusal.status, refusal.message)
			return
		}
		handler(w, r)
	}
}

func nulInParameters(r *http.Request, wildcards []string) *apiError {
	for _, wildcard := range wildcards {
		if refusal := nulCharacter("path parameter "+wildcard, r.PathValue(wildcard)); refusal != nil {
			return refusal
		}
	}
	query := r.URL.Query()
	for _, name := range slices.Sorted(maps.Keys(query)) {
		field := "query parameter " + strings.ReplaceAll(name, "\x00", `\u0000`)
		if refusal := nulCharacter(field, name); refusal != nil {
			return refusal
		}
		for _, value := range query[name] {
			if refusal := nulCharacter(field, value); refusal != nil {
				return refusal
			}
		}
	}
	return nil
}
