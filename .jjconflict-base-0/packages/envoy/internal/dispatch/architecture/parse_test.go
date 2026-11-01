package architecture

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseValidTree(t *testing.T) {
	model, err := Parse(map[string][]byte{
		"API.md":    []byte("---\ntitle: HTTP API\nparent: server\ndepends_on: [store]\npaths:\n  - internal/api\n---\nThe API surface.\n"),
		"server.md": []byte("---\ntitle: Server\n---\nThe server.\n"),
		"store.md":  []byte("Body without front matter.\n"),
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []Component{
		{ID: "api", Title: "HTTP API", Prose: "The API surface.\n", Parent: "server", DependsOn: []string{"store"}, Paths: []string{"internal/api"}},
		{ID: "server", Title: "Server", Prose: "The server.\n"},
		{ID: "store", Title: "store", Prose: "Body without front matter.\n"},
	}
	if !reflect.DeepEqual(model.Components, want) {
		t.Fatalf("components = %#v\nwant %#v", model.Components, want)
	}
}

// The front-matter closer is a line that is exactly `---`: an empty block is
// defaults, CRLF files close, a horizontal rule in the body is body, and a
// `---` inside a YAML block scalar stays inside the block.
func TestParseFrontMatterDelimiters(t *testing.T) {
	model, err := Parse(map[string][]byte{
		"empty.md": []byte("---\n---\nJust prose.\n"),
		"crlf.md":  []byte("---\r\ntitle: Windows\r\n---\r\nbody\r\n"),
		"rule.md":  []byte("---\ntitle: Rule\n---\nintro\n\n---\n\nmore\n"),
		"block.md": []byte("---\ntitle: |\n  line\n  ---not a close\n---\nbody\n"),
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []Component{
		{ID: "block", Title: "line\n---not a close", Prose: "body\n"},
		{ID: "crlf", Title: "Windows", Prose: "body\r\n"},
		{ID: "empty", Title: "empty", Prose: "Just prose.\n"},
		{ID: "rule", Title: "Rule", Prose: "intro\n\n---\n\nmore\n"},
	}
	if !reflect.DeepEqual(model.Components, want) {
		t.Fatalf("components = %#v\nwant %#v", model.Components, want)
	}
}

func TestParseEmptySetIsValid(t *testing.T) {
	model, err := Parse(map[string][]byte{})
	if err != nil {
		t.Fatalf("parse empty: %v", err)
	}
	if len(model.Components) != 0 {
		t.Fatalf("empty set produced components: %#v", model.Components)
	}
}

func TestParseRejectsTheSetWholeNamingEveryProblem(t *testing.T) {
	for name, tc := range map[string]struct {
		files map[string][]byte
		wants []string
	}{
		"duplicate id (case-insensitive)": {
			files: map[string][]byte{
				"Api.md": []byte("upper\n"),
				"api.md": []byte("lower\n"),
			},
			wants: []string{`duplicate component id "api"`},
		},
		"unknown parent": {
			files: map[string][]byte{
				"api.md": []byte("---\nparent: ghost\n---\n"),
			},
			wants: []string{`unknown parent "ghost"`},
		},
		"unknown dependency": {
			files: map[string][]byte{
				"api.md": []byte("---\ndepends_on: [ghost]\n---\n"),
			},
			wants: []string{`unknown depends_on "ghost"`},
		},
		"self dependency": {
			files: map[string][]byte{
				"api.md": []byte("---\ndepends_on: [api]\n---\n"),
			},
			wants: []string{"depends on itself"},
		},
		"containment cycle": {
			files: map[string][]byte{
				"a.md": []byte("---\nparent: b\n---\n"),
				"b.md": []byte("---\nparent: a\n---\n"),
			},
			wants: []string{"containment cycle a -> b -> a"},
		},
		"self parent": {
			files: map[string][]byte{
				"a.md": []byte("---\nparent: a\n---\n"),
			},
			wants: []string{"containment cycle a -> a"},
		},
		"bad slug file name": {
			files: map[string][]byte{
				"has space.md": []byte("x\n"),
				"notes.txt":    []byte("x\n"),
				"-edge.md":     []byte("x\n"),
			},
			wants: []string{
				"has space.md: file name must be <slug>.md",
				"notes.txt: file name must be <slug>.md",
				"-edge.md: file name must be <slug>.md",
			},
		},
		"bad front matter yaml": {
			files: map[string][]byte{
				"api.md": []byte("---\ntitle: [unclosed\n---\n"),
			},
			wants: []string{"api.md: front matter:"},
		},
		"unclosed front matter": {
			files: map[string][]byte{
				"api.md":   []byte("---\ntitle: x\n"),
				"dash.md":  []byte("---"),
				"dashy.md": []byte("---\ntitle: x\n---foo\n"),
			},
			wants: []string{"api.md: unclosed front matter", "dash.md: unclosed front matter", "dashy.md: unclosed front matter"},
		},
		"unknown front matter key": {
			files: map[string][]byte{
				"api.md": []byte("---\ndepends-on: [store]\nparents: store\n---\n"),
			},
			wants: []string{"api.md: front matter:", "depends-on", "parents"},
		},
		"invalid utf-8": {
			files: map[string][]byte{
				"api.md": []byte("---\ntitle: caf\xe9\n---\nbody\n"),
			},
			wants: []string{"api.md: file must be valid UTF-8 text without NUL bytes"},
		},
		"nul byte": {
			files: map[string][]byte{
				"api.md": []byte("---\ntitle: ok\n---\nbody\x00here\n"),
			},
			wants: []string{"api.md: file must be valid UTF-8 text without NUL bytes"},
		},
		"dot-dot path": {
			files: map[string][]byte{
				"api.md": []byte("---\npaths: ['../secrets', '/abs', 'ok/path']\n---\n"),
			},
			wants: []string{`path "../secrets"`, `path "/abs"`},
		},
		"several problems reported together": {
			files: map[string][]byte{
				"a.md":  []byte("---\nparent: ghost\n---\n"),
				"b!.md": []byte("x\n"),
			},
			wants: []string{`unknown parent "ghost"`, "b!.md: file name must be <slug>.md"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(tc.files)
			if err == nil {
				t.Fatalf("parse accepted an invalid set")
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
		})
	}
}
