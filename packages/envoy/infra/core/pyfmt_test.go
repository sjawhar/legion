package core_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
)

// Every want below is Python 3.12's own output for the same value: json.dumps with the options
// named, or shlex.quote and shlex.join.
func TestPyJSON(t *testing.T) {
	// Built out of sorted order, so the sort_keys rows show the sort at every depth.
	nested := core.Object{
		{Key: "b", Value: core.Object{{Key: "z", Value: 1}, {Key: "a", Value: []any{"x", true, nil, -7}}}},
		{Key: "a", Value: "v"},
		{Key: "c", Value: core.Object{}},
		{Key: "\u00e9", Value: "\u00e9"},
	}
	quoting := "q\"b\\<>&\u00e9\u2028\U0001F600"
	controls := "a\nb\tc\x01\x1f\x7f\b\f\r"
	cases := []struct {
		name string
		in   any
		opts core.PyJSONOptions
		want string
	}{
		{
			name: "nested objects with sort_keys",
			in:   nested,
			opts: core.PyJSONOptions{SortKeys: true},
			want: "{\"a\": \"v\", \"b\": {\"a\": [\"x\", true, null, -7], \"z\": 1}, \"c\": {}, \"\u00e9\": \"\u00e9\"}",
		},
		{
			name: "nested objects with sort_keys and ensure_ascii",
			in:   nested,
			opts: core.PyJSONOptions{SortKeys: true, EnsureASCII: true},
			want: `{"a": "v", "b": {"a": ["x", true, null, -7], "z": 1}, "c": {}, "\u00e9": "\u00e9"}`,
		},
		{
			name: "quotes, backslash, HTML characters and non-ASCII, raw",
			in:   quoting,
			want: "\"q\\\"b\\\\<>&\u00e9\u2028\U0001F600\"",
		},
		{
			name: "quotes, backslash, HTML characters and non-ASCII, with ensure_ascii",
			in:   quoting,
			opts: core.PyJSONOptions{EnsureASCII: true},
			want: `"q\"b\\<>&\u00e9\u2028\ud83d\ude00"`,
		},
		{
			name: "control characters, raw",
			in:   controls,
			want: `"a\nb\tc\u0001\u001f` + "\x7f" + `\b\f\r"`,
		},
		{
			name: "control characters, with ensure_ascii",
			in:   controls,
			opts: core.PyJSONOptions{EnsureASCII: true},
			want: `"a\nb\tc\u0001\u001f\u007f\b\f\r"`,
		},
		{name: "an empty list", in: []any{}, want: "[]"},
		{name: "a typed list", in: []string{"a", "b"}, want: `["a", "b"]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, core.PyJSON(c.in, c.opts))
		})
	}
}

// A Go map has no key order, so it is no Python dict, and the refusal names the type that is one; a
// float has no Go spelling that is always Python's repr. Both are the caller's error, not data.
func TestPyJSONRefusesWhatItCannotSpellAsPython(t *testing.T) {
	requirePanicNaming(t, "core.PyJSON: map[string]interface {} has no key order; build a core.Object",
		func() { core.PyJSON(map[string]any{"a": 1}, core.PyJSONOptions{SortKeys: true}) })
	requirePanicNaming(t, "core.PyJSON: map[string]int has no key order; build a core.Object",
		func() { core.PyJSON([]any{map[string]int{"a": 1}}, core.PyJSONOptions{}) })
	requirePanicNaming(t, "float64", func() { core.PyJSON([]any{1.5}, core.PyJSONOptions{}) })
}

// An Object is a Python dict built in a known order: PyJSON writes its keys in that order, or
// sorted under SortKeys, as json.dumps writes a dict. Every want is Python 3.12's own output for the
// dict with the same insertion order.
func TestPyJSONObject(t *testing.T) {
	ordered := core.Object{
		{Key: "z", Value: 1},
		{Key: "a", Value: []any{core.Object{{Key: "y", Value: "\u00e9"}, {Key: "b", Value: nil}}, true, []string{"x"}}},
		{Key: "\u00e9", Value: "\u2028\U0001F600"},
		{Key: "c", Value: core.Object{}},
	}
	unsorted := core.Object{
		{Key: "b", Value: core.Object{{Key: "d", Value: 1}, {Key: "c", Value: 2}}},
		{Key: "a", Value: 2},
	}
	cases := []struct {
		name string
		in   any
		opts core.PyJSONOptions
		want string
	}{
		{
			name: "insertion order, non-ASCII raw",
			in:   ordered,
			want: "{\"z\": 1, \"a\": [{\"y\": \"\u00e9\", \"b\": null}, true, [\"x\"]], \"\u00e9\": \"\u2028\U0001F600\", \"c\": {}}",
		},
		{
			name: "insertion order, with ensure_ascii",
			in:   ordered,
			opts: core.PyJSONOptions{EnsureASCII: true},
			want: `{"z": 1, "a": [{"y": "\u00e9", "b": null}, true, ["x"]], "\u00e9": "\u2028\ud83d\ude00", "c": {}}`,
		},
		{
			name: "sort_keys sorts an object at every depth",
			in:   unsorted,
			opts: core.PyJSONOptions{SortKeys: true},
			want: `{"a": 2, "b": {"c": 2, "d": 1}}`,
		},
		{name: "an empty object", in: core.Object{}, want: "{}"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, core.PyJSON(c.in, c.opts))
		})
	}
}

// A dict holds each key once, so an Object naming a key twice has no Python counterpart.
func TestPyJSONRefusesAnObjectWithARepeatedKey(t *testing.T) {
	requirePanicNaming(t, `"name"`, func() {
		core.PyJSON(core.Object{{Key: "name", Value: "a"}, {Key: "name", Value: "b"}}, core.PyJSONOptions{})
	})
}

// requirePanicNaming requires f to panic with a value whose text contains want.
func requirePanicNaming(t *testing.T, want string, f func()) {
	t.Helper()
	defer func() {
		r := recover()
		require.NotNil(t, r, "no panic; want one naming %q", want)
		require.Contains(t, fmt.Sprint(r), want)
	}()
	f()
}

func TestShellQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "''"},
		{"a-b_c", "a-b_c"},
		{"@%+=:,./-", "@%+=:,./-"},
		{"a b", "'a b'"},
		{"it's", `'it'"'"'s'`},
		{"line\nbreak", "'line\nbreak'"},
		{"caf\u00e9", "'caf\u00e9'"},
		{"$JS", "'$JS'"},
	}
	for _, c := range cases {
		require.Equal(t, c.want, core.ShellQuote(c.in), "ShellQuote(%q)", c.in)
	}
}

func TestShellJoin(t *testing.T) {
	require.Equal(t, `nats-server --config '/tmp/x y' -js ''`,
		core.ShellJoin([]string{"nats-server", "--config", "/tmp/x y", "-js", ""}))
}
