package core

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// PyJSONOptions are the json.dumps options PyJSON reproduces.
type PyJSONOptions struct {
	SortKeys    bool // sort_keys=True
	EnsureASCII bool // ensure_ascii=True
}

// PyJSON returns v as Python's json.dumps writes it with its default separators (", " and ": ")
// and the options given, so text a Python program rendered can be rendered again byte for byte.
// v is built from nil, bools, integers, strings, slices and arrays, and maps with string keys,
// through interfaces and pointers. A nil slice is an empty list.
//
// A Go map has no key order, so a map is written only with SortKeys. A float has no spelling that
// is always Python's repr, and a string that is not valid UTF-8 has no Python counterpart. Each of
// these panics: it is the caller's error, never data.
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
		if v.Type().Key().Kind() != reflect.String {
			panic(fmt.Sprintf("core.PyJSON: %s has keys that are not strings", v.Type()))
		}
		if !opts.SortKeys {
			panic("core.PyJSON: a Go map has no key order; set SortKeys")
		}
		keys := make([]string, 0, v.Len())
		for _, key := range v.MapKeys() {
			keys = append(keys, key.String())
		}
		// Python sorts str keys by code point; UTF-8 bytes sort the same way.
		sort.Strings(keys)
		b.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			writePyString(b, key, opts.EnsureASCII)
			b.WriteString(": ")
			writePyJSON(b, v.MapIndex(reflect.ValueOf(key).Convert(v.Type().Key())), opts)
		}
		b.WriteByte('}')
	case reflect.Float32, reflect.Float64:
		panic(fmt.Sprintf("core.PyJSON: %s is not supported: Python's float repr is not Go's", v.Type()))
	default:
		panic(fmt.Sprintf("core.PyJSON: %s is not a JSON value", v.Type()))
	}
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
