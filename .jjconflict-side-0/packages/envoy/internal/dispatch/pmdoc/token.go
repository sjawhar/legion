package pmdoc

import (
	"bytes"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// TokenSpan is where one block-id node's token representation lies in an EncodeTokenJSON buffer.
type TokenSpan struct {
	ID         string
	Start, End int
}

// TokenJSON returns a deterministic semantic representation for optimistic
// concurrency. Block identity and typed-block server state are excluded, while
// inline marks remain part of the state a document edit can destroy.
func (n *Node) TokenJSON() ([]byte, error) {
	return EncodeTokenJSON(n, nil)
}

// EncodeTokenJSON returns n's token representation in a buffer it allocates. The
// representation is a node with the fields type, attrs, content, text and marks, in that order and
// each but type left out when empty: its attributes in canonical form (no block id, no server-owned
// typed attribute, no nil value, every number a float64) and its marks in the order of their
// encodings, so a node's marks in any order are one token. It is byte for byte what encoding/json's
// Marshal makes of that representation, errors included, because callers hold tokens across
// releases as edit preconditions and a single byte of difference refuses every edit.
//
// When blocks is non-nil, EncodeTokenJSON appends to it one TokenSpan for each node carrying a
// block id, inline nodes included, in the order the nodes open; each span's offsets index the
// returned buffer, and its bytes are that node's own token representation. A document read hashes
// the whole buffer for the document token and each span for a block token, so the tree is encoded
// once, into one buffer, for a document and all its blocks, rather than once more for each block
// enclosing a node. The encoder keeps nothing once it returns. It validates n first. On an error,
// EncodeTokenJSON returns nil and the error; the spans it appended to blocks by then are the
// caller's to discard.
func EncodeTokenJSON(n *Node, blocks *[]TokenSpan) ([]byte, error) {
	if err := n.Validate(); err != nil {
		return nil, err
	}
	return encodeTokenJSON(n, blocks)
}

// TokenJSON is EncodeTokenJSON of d's tree, its spans appended to blocks when blocks is non-nil,
// without validating the tree again. The zero ValidDocument is EncodeTokenJSON's error for a nil
// tree.
func (d ValidDocument) TokenJSON(blocks *[]TokenSpan) ([]byte, error) {
	if d.doc == nil {
		return EncodeTokenJSON(nil, blocks)
	}
	return encodeTokenJSON(d.doc, blocks)
}

// encodeTokenJSON is EncodeTokenJSON without the validation, for a tree its caller has validated.
func encodeTokenJSON(n *Node, blocks *[]TokenSpan) ([]byte, error) {
	encoder := tokenEncoder{blocks: blocks}
	encoded, err := encoder.node(nil, n)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// tokenEncoder is the state of one EncodeTokenJSON call. Each map it encodes is canonicalised as
// it is written, through the entries stack, rather than copied into a canonical map first, and a
// node's marks are encoded into one scratch buffer that every node of the call reuses.
type tokenEncoder struct {
	blocks *[]TokenSpan
	// entries holds the entries of every object being written, innermost last: each object pushes
	// its own, sorts and writes them, and pops them before its enclosing object goes on.
	entries []tokenEntry
	// marks and markRanges hold the encodings of one node's marks while they are put in order.
	marks      []byte
	markRanges []tokenRange
}

type tokenEntry struct {
	name  string
	value any
}

type tokenRange struct {
	start, end int
}

// node appends n's token representation to dst, recording a span for it when it carries a block id.
func (e *tokenEncoder) node(dst []byte, n *Node) ([]byte, error) {
	span := -1
	if e.blocks != nil {
		if id, _ := n.Attrs[BlockIDAttr].(string); id != "" {
			span = len(*e.blocks)
			*e.blocks = append(*e.blocks, TokenSpan{ID: id, Start: len(dst)})
		}
	}
	dst = append(dst, `{"type":`...)
	dst = appendTokenString(dst, n.Type)
	var err error
	if start := e.pushNodeEntries(n.Type, n.Attrs); len(e.entries) > start {
		dst = append(dst, `,"attrs":`...)
		if dst, err = e.object(dst, start); err != nil {
			return dst, err
		}
	}
	if len(n.Children) > 0 {
		dst = append(dst, `,"content":[`...)
		for index, child := range n.Children {
			if index > 0 {
				dst = append(dst, ',')
			}
			if dst, err = e.node(dst, child); err != nil {
				return dst, err
			}
		}
		dst = append(dst, ']')
	}
	if n.Text != "" {
		dst = append(dst, `,"text":`...)
		dst = appendTokenString(dst, n.Text)
	}
	if len(n.Marks) > 0 {
		if dst, err = e.nodeMarks(dst, n.Marks); err != nil {
			return dst, err
		}
	}
	dst = append(dst, '}')
	if span >= 0 {
		(*e.blocks)[span].End = len(dst)
	}
	return dst, nil
}

// nodeMarks appends a node's marks field. Marks are encoded in source order, so the first mark
// that cannot be encoded is the one reported, and then written in the order of their encodings.
func (e *tokenEncoder) nodeMarks(dst []byte, marks []Mark) ([]byte, error) {
	dst = append(dst, `,"marks":[`...)
	var err error
	e.marks, e.markRanges = e.marks[:0], e.markRanges[:0]
	for _, mark := range marks {
		start := len(e.marks)
		if e.marks, err = e.mark(e.marks, mark); err != nil {
			return dst, err
		}
		e.markRanges = append(e.markRanges, tokenRange{start: start, end: len(e.marks)})
	}
	slices.SortFunc(e.markRanges, func(left, right tokenRange) int {
		return bytes.Compare(e.marks[left.start:left.end], e.marks[right.start:right.end])
	})
	for index, encoded := range e.markRanges {
		if index > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, e.marks[encoded.start:encoded.end]...)
	}
	return append(dst, ']'), nil
}

// mark appends one mark's encoding: its type, then its canonical attributes unless there are none.
func (e *tokenEncoder) mark(dst []byte, mark Mark) ([]byte, error) {
	dst = append(dst, `{"type":`...)
	dst = appendTokenString(dst, mark.Type)
	if start := e.pushEntries(mark.Attrs); len(e.entries) > start {
		dst = append(dst, `,"attrs":`...)
		var err error
		if dst, err = e.object(dst, start); err != nil {
			return dst, err
		}
	}
	return append(dst, '}'), nil
}

// pushNodeEntries pushes the attributes of a node of type nodeType that its token keeps
// (tokenAttrKept) and returns where they start on the entry stack.
func (e *tokenEncoder) pushNodeEntries(nodeType string, attrs Attrs) (start int) {
	start = len(e.entries)
	server := serverAttributes(nodeType)
	for name, value := range attrs {
		if tokenAttrKept(server, name, value) {
			e.entries = append(e.entries, tokenEntry{name: name, value: value})
		}
	}
	return start
}

// pushEntries pushes attrs' non-nil entries, the entries of a mark's attributes or of a map nested
// in a value, and returns where they start on the entry stack.
func (e *tokenEncoder) pushEntries(attrs map[string]any) (start int) {
	start = len(e.entries)
	for name, value := range attrs {
		if value != nil {
			e.entries = append(e.entries, tokenEntry{name: name, value: value})
		}
	}
	return start
}

// object appends the object of the entries from start to the top of the stack, in the order of
// their names as encoding/json sorts a map's keys (by the bytes of the Go string), and pops them.
func (e *tokenEncoder) object(dst []byte, start int) ([]byte, error) {
	slices.SortFunc(e.entries[start:], func(left, right tokenEntry) int {
		return strings.Compare(left.name, right.name)
	})
	dst = append(dst, '{')
	// A value's own objects push above end and pop before it returns, and may move the stack, so
	// the entries are read by index.
	end := len(e.entries)
	for index := start; index < end; index++ {
		if index > start {
			dst = append(dst, ',')
		}
		entry := e.entries[index]
		dst = appendTokenString(dst, entry.name)
		dst = append(dst, ':')
		var err error
		if dst, err = e.value(dst, entry.value); err != nil {
			return dst, err
		}
	}
	clear(e.entries[start:end])
	e.entries = e.entries[:start]
	return append(dst, '}'), nil
}

// value appends an attribute value in canonical form (canonicalTokenValue): int, int64 and float32
// as float64, []string as an array of strings, and []any, Attrs and map[string]any recursively.
// Every other type is what encoding/json makes of it, error included.
func (e *tokenEncoder) value(dst []byte, value any) ([]byte, error) {
	switch v := value.(type) {
	case nil:
		return append(dst, "null"...), nil
	case string:
		return appendTokenString(dst, v), nil
	case bool:
		return strconv.AppendBool(dst, v), nil
	case float64:
		return appendTokenFloat(dst, v)
	case int:
		return appendTokenFloat(dst, float64(v))
	case int64:
		return appendTokenFloat(dst, float64(v))
	case float32:
		return appendTokenFloat(dst, float64(v))
	case []string:
		dst = append(dst, '[')
		for index, item := range v {
			if index > 0 {
				dst = append(dst, ',')
			}
			dst = appendTokenString(dst, item)
		}
		return append(dst, ']'), nil
	case []any:
		dst = append(dst, '[')
		for index, item := range v {
			if index > 0 {
				dst = append(dst, ',')
			}
			var err error
			if dst, err = e.value(dst, item); err != nil {
				return dst, err
			}
		}
		return append(dst, ']'), nil
	case Attrs:
		return e.object(dst, e.pushEntries(v))
	case map[string]any:
		return e.object(dst, e.pushEntries(v))
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return dst, err
		}
		return append(dst, encoded...), nil
	}
}

// appendTokenString appends s as encoding/json quotes a string (jsonwire.AppendQuote with
// EscapeForHTML, EscapeForJS and AllowInvalidUTF8): '"' and '\\' escaped by a backslash, \b, \f,
// \n, \r and \t in their short forms, every other control character and '<', '>' and '&' as \u00xx
// in lowercase hex, U+2028 and U+2029 as \u2028 and \u2029, and each byte of invalid UTF-8 replaced
// by U+FFFD, written as itself.
func appendTokenString(dst []byte, s string) []byte {
	const hex = "0123456789abcdef"
	dst = append(dst, '"')
	written := 0
	for index := 0; index < len(s); {
		if c := s[index]; c < utf8.RuneSelf {
			index++
			if c >= 0x20 && c != '"' && c != '\\' && c != '<' && c != '>' && c != '&' {
				continue
			}
			dst = append(dst, s[written:index-1]...)
			switch c {
			case '"', '\\':
				dst = append(dst, '\\', c)
			case '\b':
				dst = append(dst, `\b`...)
			case '\f':
				dst = append(dst, `\f`...)
			case '\n':
				dst = append(dst, `\n`...)
			case '\r':
				dst = append(dst, `\r`...)
			case '\t':
				dst = append(dst, `\t`...)
			default:
				dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
			}
			written = index
			continue
		}
		r, size := utf8.DecodeRuneInString(s[index:])
		index += size
		switch {
		case r == utf8.RuneError && size == 1:
			dst = append(dst, s[written:index-1]...)
			dst = append(dst, "\ufffd"...)
			written = index
		case r == '\u2028':
			dst = append(dst, s[written:index-size]...)
			dst = append(dst, `\u2028`...)
			written = index
		case r == '\u2029':
			dst = append(dst, s[written:index-size]...)
			dst = append(dst, `\u2029`...)
			written = index
		}
	}
	dst = append(dst, s[written:]...)
	return append(dst, '"')
}

// appendTokenFloat appends f as encoding/json writes a float64 (jsonwire.AppendFloat with 64 bits):
// the shortest decimal that reads back as f, in exponent form below 1e-6 and from 1e21 up with a
// one-digit negative exponent unpadded, and -0 as "-0". NaN and ±Inf are encoding/json's error.
func appendTokenFloat(dst []byte, f float64) ([]byte, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		_, err := json.Marshal(f)
		return dst, err
	}
	format := byte('f')
	if abs := math.Abs(f); abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	dst = strconv.AppendFloat(dst, f, format, -1, 64)
	if format == 'e' {
		// Clean up e-09 to e-9.
		if n := len(dst); n >= 4 && dst[n-4] == 'e' && dst[n-3] == '-' && dst[n-2] == '0' {
			dst[n-2] = dst[n-1]
			dst = dst[:n-1]
		}
	}
	return dst, nil
}

// serverAttributes is the attribute schema of a node of type nodeType when it is a typed block,
// whose server-owned attributes a token leaves out, or nil.
func serverAttributes(nodeType string) map[string]BlockAttributeSchema {
	typ, _ := typedBlock(nodeType)
	return typ.Attributes
}

// tokenAttrKept reports whether a token keeps a node's attribute name holding value: the block id,
// the attributes the server owns (server is serverAttributes of the node's type) and nil values are
// left out. The encoder and tokenAttrs share it, so the representation the readback compares is the
// one tokens hash.
func tokenAttrKept(server map[string]BlockAttributeSchema, name string, value any) bool {
	return value != nil && name != BlockIDAttr && !server[name].Server
}

type tokenMark struct {
	Type  string `json:"type"`
	Attrs Attrs  `json:"attrs,omitempty"`
}

// tokenAttrs is a node's attributes as its token holds them, built whole: the attributes
// tokenAttrKept keeps, in canonical form.
func tokenAttrs(nodeType string, attrs Attrs) Attrs {
	server := serverAttributes(nodeType)
	out := make(Attrs)
	for name, value := range attrs {
		if tokenAttrKept(server, name, value) {
			out[name] = canonicalTokenValue(value)
		}
	}
	return out
}

func canonicalAttrs(attrs Attrs) Attrs {
	out := make(Attrs)
	for name, value := range attrs {
		if value != nil {
			out[name] = canonicalTokenValue(value)
		}
	}
	return out
}

func canonicalTokenValue(value any) any {
	switch v := value.(type) {
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case float32:
		return float64(v)
	case []string:
		out := make([]any, len(v))
		for index := range v {
			out[index] = v[index]
		}
		return out
	case []any:
		out := make([]any, len(v))
		for index := range v {
			out[index] = canonicalTokenValue(v[index])
		}
		return out
	case Attrs:
		return canonicalAttrs(v)
	case map[string]any:
		return canonicalAttrs(Attrs(v))
	default:
		return v
	}
}
