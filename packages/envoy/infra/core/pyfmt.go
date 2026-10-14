package core

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// PyJSONOptions are the json.dumps options PyJSON reproduces.
type PyJSONOptions struct {
	SortKeys    bool // sort_keys=True
	EnsureASCII bool // ensure_ascii=True
}

// Field is one key of an Object and its value.
type Field struct {
	Key   string
	Value any
}

// Object is a Python dict built in a known order. PyJSON writes its keys in the order given, as
// json.dumps writes a dict in insertion order, or sorted under SortKeys, as it writes any dict.
type Object []Field

// PyJSON returns v as Python's json.dumps writes it with its default separators (", " and ": ")
// and the options given, so text a Python program rendered can be rendered again byte for byte.
// v is built from nil, bools, integers, strings, slices and arrays, and Objects, through
// interfaces and pointers. A nil slice is an empty list.
//
// A Go map has no key order, so it is no dict: build an Object. A float has no spelling that is
// always Python's repr, a string that is not valid UTF-8 has no Python counterpart, and neither has
// an Object holding a key twice. Each of these panics: it is the caller's error, never data.
func PyJSON(v any, opts PyJSONOptions) string {
	var b strings.Builder
	writePyJSON(&b, reflect.ValueOf(v), opts)
	return b.String()
}

func writePyJSON(b *strings.Builder, v reflect.Value, opts PyJSONOptions) {
	if !v.IsValid() {
		b.WriteString("null")
		return
	}
	if v.Type() == objectType {
		writePyObject(b, v.Interface().(Object), opts)
		return
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			b.WriteString("null")
			return
		}
		writePyJSON(b, v.Elem(), opts)
	case reflect.Bool:
		b.WriteString(strconv.FormatBool(v.Bool()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		b.WriteString(strconv.FormatInt(v.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		b.WriteString(strconv.FormatUint(v.Uint(), 10))
	case reflect.String:
		writePyString(b, v.String(), opts.EnsureASCII)
	case reflect.Slice, reflect.Array:
		b.WriteByte('[')
		for i := range v.Len() {
			if i > 0 {
				b.WriteString(", ")
			}
			writePyJSON(b, v.Index(i), opts)
		}
		b.WriteByte(']')
	case reflect.Map:
		panic(fmt.Sprintf("core.PyJSON: %s has no key order; build a core.Object, a dict in a known order", v.Type()))
	case reflect.Float32, reflect.Float64:
		panic(fmt.Sprintf("core.PyJSON: %s is not supported: Python's float repr is not Go's", v.Type()))
	default:
		panic(fmt.Sprintf("core.PyJSON: %s is not a JSON value", v.Type()))
	}
}

var objectType = reflect.TypeFor[Object]()

// writePyObject writes o as json.dumps writes a dict: its keys in o's order, or sorted under
// SortKeys. Python sorts str keys by code point, and UTF-8 bytes sort the same way.
func writePyObject(b *strings.Builder, o Object, opts PyJSONOptions) {
	seen := make(map[string]bool, len(o))
	for _, field := range o {
		if seen[field.Key] {
			panic(fmt.Sprintf("core.PyJSON: an Object holds the key %q twice; a dict holds each key once", field.Key))
		}
		seen[field.Key] = true
	}
	if opts.SortKeys {
		o = slices.Clone(o)
		slices.SortFunc(o, func(x, y Field) int { return strings.Compare(x.Key, y.Key) })
	}
	b.WriteByte('{')
	for i, field := range o {
		if i > 0 {
			b.WriteString(", ")
		}
		writePyString(b, field.Key, opts.EnsureASCII)
		b.WriteString(": ")
		writePyJSON(b, reflect.ValueOf(field.Value), opts)
	}
	b.WriteByte('}')
}

// pyEscapes are the short escapes json.dumps writes; every other character below U+0020 is \u00XX.
var pyEscapes = map[rune]string{'"': `\"`, '\\': `\\`, '\b': `\b`, '\f': `\f`, '\n': `\n`, '\r': `\r`, '\t': `\t`}

// writePyString writes s as a JSON string as json.dumps does. Without ensureASCII it escapes only
// the quote, the backslash and U+0000-U+001F, and writes everything else raw, U+007F and U+2028
// included. With ensureASCII it also escapes every character outside U+0020-U+007E, above U+FFFF
// as a surrogate pair, in lower-case hexadecimal.
func writePyString(b *strings.Builder, s string, ensureASCII bool) {
	if !utf8.ValidString(s) {
		panic(fmt.Sprintf("core.PyJSON: %q is not valid UTF-8", s))
	}
	b.WriteByte('"')
	for _, r := range s {
		if escape, ok := pyEscapes[r]; ok {
			b.WriteString(escape)
			continue
		}
		switch {
		case r < 0x20:
			fmt.Fprintf(b, `\u%04x`, r)
		case !ensureASCII || r <= 0x7e:
			b.WriteRune(r)
		case r < 0x10000:
			fmt.Fprintf(b, `\u%04x`, r)
		default:
			n := r - 0x10000
			fmt.Fprintf(b, `\u%04x\u%04x`, 0xd800|(n>>10)&0x3ff, 0xdc00|n&0x3ff)
		}
	}
	b.WriteByte('"')
}

// ShellQuote quotes s for a POSIX shell as Python's shlex.quote does: an empty pair of single
// quotes for an empty string, s unchanged when every character is in [A-Za-z0-9_@%+=:,./-], and
// otherwise s in single quotes, each ' in it written as '"'"'.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, unsafeInShell) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func unsafeInShell(r rune) bool {
	switch {
	case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9':
		return false
	}
	return !strings.ContainsRune("_@%+=:,./-", r)
}

// ShellJoin is Python's shlex.join: ShellQuote over each argument, joined by single spaces.
func ShellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = ShellQuote(arg)
	}
	return strings.Join(quoted, " ")
}
