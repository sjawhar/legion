package text

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Resolver says what a bare issue key, a `#id` and a short id name for one text. Mentions asks it
// at most once for each, and only when the body holds a candidate of that kind, so a database
// resolver answers every key of one body in one query, loads the text's question map once, and
// resolves every short id of one body in one query.
type Resolver interface {
	// Issues reports which of keys, each a distinct issue key the body writes bare, names an
	// issue that exists. A key absent from the result, or mapped to false, names nothing.
	Issues(keys []string) (map[string]bool, error)
	// Questions is the question map of the document the text belongs to: each question block id
	// to the ask reference a `#` and that id makes. A text that belongs to no document has none.
	Questions() (map[string]Ref, error)
	// Items is the whole id of each short id prefixes writes: for every index of prefixes it
	// resolved, the id of the one ask, comment, message or block that index names. An index the
	// map leaves out stays as the text writes it, so a short id that names nothing, and one that
	// names two items or more, is simply absent.
	Items(prefixes []Prefix) (map[int]string, error)
}

// Prefix is one reference whose id a text writes short, as Mentions asks a Resolver to resolve it.
// Ref is the reference as the grammar read it, and Block says which of its two ids is the short
// one: the block id (Ref.Block) when Block is true, the item id (Ref.ID) when it is false. A
// reference that writes both short is two prefixes.
type Prefix struct {
	Ref   Ref
	Block bool
}

// Mentions is ExtractAt with the references a text writes bare or short: each issue key the
// resolver says exists, as the issue reference `dispatch://<KEY>` makes, each `#id` the resolver's
// question map names, as that ask's reference, and each reference whose item id or block id is
// written short (IsIDPrefix), as the whole id the resolver answers with. Every mention keeps its
// byte offset (a `#id`'s is its `#`), in body order, repeats included, as ExtractAt reports them,
// and a reference whose id is rewritten keeps the offset it was written at. A bare candidate
// inside a reference span (a `dispatch://` or `http(s)://` address, whether or not it names
// anything) is part of that address, never a mention of its own. A nil resolver reads no key, no
// `#id` and no short id, so Mentions is then ExtractAt. Every resolver error is returned.
//
// A bare candidate stands alone (mentionStart): at the start of the text, after whitespace or a
// numeric character reference to whitespace (`&#32;`, as stored markdown writes the space at a
// mark's edge), or after one of ( [ " ' * _ > ` ~ |. A key is an uppercase project key of 2 to 10
// characters, `-` and a number from 1 up, ending as keyEnds says. A `#id` is `#` and the longest
// run of the block-id characters [A-Za-z0-9_-], read as questionRef reads it. There is no markdown
// parse here, so a candidate in code is read, as a reference in code is.
func Mentions(body, serverURL string, r Resolver) ([]Located, error) {
	spans := referencePattern.FindAllStringIndex(body, -1)
	located := extractSpans(body, serverURL, spans, true)
	if r == nil {
		return located, nil
	}
	if err := resolvePrefixes(located, r); err != nil {
		return nil, err
	}

	candidates, keys, hasID := bareCandidates(body, spans)
	if len(candidates) == 0 {
		return located, nil
	}
	var issues map[string]bool
	if len(keys) > 0 {
		var err error
		if issues, err = r.Issues(keys); err != nil {
			return nil, err
		}
	}
	var questions map[string]Ref
	if hasID {
		var err error
		if questions, err = r.Questions(); err != nil {
			return nil, err
		}
	}

	resolved := make([]Located, 0, len(candidates))
	for _, item := range candidates {
		if item.key != "" {
			if issues[item.key] {
				resolved = append(resolved, Located{
					Ref:    Ref{Kind: "issue", IssueKey: item.key, ID: item.key},
					Offset: item.offset,
				})
			}
			continue
		}
		if ref, ok := questionRef(questions, item.id); ok {
			resolved = append(resolved, Located{Ref: ref, Offset: item.offset})
		}
	}
	if len(resolved) == 0 {
		return located, nil
	}

	merged := make([]Located, 0, len(located)+len(resolved))
	for len(located) > 0 && len(resolved) > 0 {
		if located[0].Offset < resolved[0].Offset {
			merged, located = append(merged, located[0]), located[1:]
		} else {
			merged, resolved = append(merged, resolved[0]), resolved[1:]
		}
	}
	merged = append(merged, located...)
	return append(merged, resolved...), nil
}

// bareMention is one candidate a text writes bare: an issue key, or the run after a `#`.
type bareMention struct {
	offset int
	key    string // set for a key
	id     string // the run after `#`, set for a `#id`
}

// bareCandidates reads the gaps between spans, the reference spans of body, for the keys and
// `#id`s the text writes bare, in body order: every candidate, the distinct keys among them, which
// is what Resolver.Issues is asked for, and whether any `#id` is among them, which is what asks
// for the question map.
func bareCandidates(body string, spans [][]int) (candidates []bareMention, keys []string, hasID bool) {
	seen := map[string]struct{}{}
	scan := func(from, to int) {
		for i := from; i < to; i++ {
			c := body[i]
			if c != '#' && (c < 'A' || c > 'Z') || !mentionStart(body, i) {
				continue
			}
			if c == '#' {
				end := i + 1
				for end < len(body) && IsBlockIDByte(body[end]) {
					end++
				}
				if end > i+1 {
					candidates = append(candidates, bareMention{offset: i, id: body[i+1 : end]})
					hasID = true
				}
				i = end - 1
				continue
			}
			end, ok := keyAt(body, i)
			if !ok {
				continue
			}
			key := body[i:end]
			candidates = append(candidates, bareMention{offset: i, key: key})
			if _, found := seen[key]; !found {
				seen[key] = struct{}{}
				keys = append(keys, key)
			}
			i = end - 1
		}
	}
	from := 0
	for _, span := range spans {
		scan(from, span[0])
		from = span[1]
	}
	scan(from, len(body))
	return candidates, keys, hasID
}

// resolvePrefixes rewrites each reference of located whose item id or block id is written short to
// the whole id the resolver answers with, keeping the reference's offset: one call for the whole
// body, and none when the body writes every id whole. Only an ask, a comment, a message and a
// block carries an id a prefix can name, so no other reference is asked about; an exact id is
// never asked about either (IsIDPrefix), since it needs no resolving. A short id the resolver
// leaves out - it names nothing, or two items or more - stays exactly as the text writes it, as
// does one whose whole id would carry the reference past the bounds the index keys (inBound).
func resolvePrefixes(located []Located, r Resolver) error {
	type target struct {
		at    int
		block bool
	}
	var prefixes []Prefix
	var targets []target
	for index, item := range located {
		if IsIDPrefix(item.Block) {
			prefixes = append(prefixes, Prefix{Ref: item.Ref, Block: true})
			targets = append(targets, target{at: index, block: true})
		}
		if (isItemKind(item.Kind) || item.Kind == "block" && isItemKind(item.Item)) && IsIDPrefix(item.ID) {
			prefixes = append(prefixes, Prefix{Ref: item.Ref})
			targets = append(targets, target{at: index})
		}
	}
	if len(prefixes) == 0 {
		return nil
	}
	resolved, err := r.Items(prefixes)
	if err != nil {
		return err
	}
	for index, whole := range resolved {
		if index < 0 || index >= len(targets) || whole == "" {
			continue
		}
		ref := located[targets[index].at].Ref
		if targets[index].block {
			ref.Block = whole
		} else {
			ref.ID = whole
		}
		if inBound(ref) {
			located[targets[index].at].Ref = ref
		}
	}
	return nil
}

// isItemKind reports whether kind is an item whose id is a uuid: an ask, a comment or a message.
func isItemKind(kind string) bool {
	return kind == "ask" || kind == "comment" || kind == "message"
}

// IsIDPrefix reports whether value is an id written short: a hex digit and 7 or more hex digits
// and hyphens, read without case, that is not a whole uuid. Its twin on the web is idPrefixPattern
// in `packages/envoy-client/src/dispatch-execute.ts`, `/^[0-9a-f][0-9a-f-]{7,}$/i`: the two accept
// the same characters at the same length. They differ in the one way each caller needs. The client
// reads a whole uuid first (normalizeUUID) and only then that pattern, so the pattern itself says
// nothing about a whole id; Mentions has no such step, and an exact id always wins, so a value a
// uuid can be read from - 32 hex digits, or 36 characters with hyphens at 8, 13, 18 and 23 - is no
// prefix here.
func IsIDPrefix(value string) bool {
	if len(value) < 8 || !isHexByte(value[0]) {
		return false
	}
	for index := 1; index < len(value); index++ {
		if c := value[index]; c != '-' && !isHexByte(c) {
			return false
		}
	}
	return !isUUID(value)
}

// isUUID reports whether value is a whole uuid of the two spellings an id prefix's characters
// allow: canonical, 8-4-4-4-12 hex digits with hyphens, and 32 hex digits. (uuid.Parse reads the
// `urn:uuid:` and braced spellings as well, which hold characters no id prefix holds.)
func isUUID(value string) bool {
	switch len(value) {
	case 32:
		return isHexRun(value)
	case 36:
		return value[8] == '-' && value[13] == '-' && value[18] == '-' && value[23] == '-' &&
			isHexRun(value[:8]) && isHexRun(value[9:13]) && isHexRun(value[14:18]) &&
			isHexRun(value[19:23]) && isHexRun(value[24:])
	}
	return false
}

func isHexRun(value string) bool {
	for index := range len(value) {
		if !isHexByte(value[index]) {
			return false
		}
	}
	return true
}

func isHexByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// questionRef is the ask a `#id` names in the question map: the id as the text writes it, else the
// id without the trailing `_` and `-` an emphasis or a sentence leaves on it, so `_see #q1_` names
// q1, else, when what is left is an id written short (IsIDPrefix), the one question block id it is
// a prefix of. A declared id wins over a short one, and a short one two question blocks or more
// carry names nothing.
func questionRef(questions map[string]Ref, id string) (Ref, bool) {
	trimmed := strings.TrimRight(id, "_-")
	if trimmed == id || trimmed == "" {
		trimmed = ""
	}
	if ref, ok := questions[id]; ok {
		return ref, true
	}
	if trimmed != "" {
		if ref, ok := questions[trimmed]; ok {
			return ref, true
		}
	}
	if ref, ok := questionPrefix(questions, id); ok {
		return ref, true
	}
	if trimmed != "" {
		return questionPrefix(questions, trimmed)
	}
	return Ref{}, false
}

// questionPrefix is the ask of the one question block whose id value is a short id of, compared
// without case; a value that is no short id (IsIDPrefix), that no block id starts with, or that
// two block ids or more start with, names nothing.
func questionPrefix(questions map[string]Ref, value string) (Ref, bool) {
	if !IsIDPrefix(value) {
		return Ref{}, false
	}
	found, count := Ref{}, 0
	for id, ref := range questions {
		if len(id) < len(value) || !strings.EqualFold(id[:len(value)], value) {
			continue
		}
		if count++; count > 1 {
			return Ref{}, false
		}
		found = ref
	}
	return found, count == 1
}

// keyAt reads the issue key starting at start, `[A-Z][A-Z0-9]{1,9}-[1-9][0-9]*` as
// issueKeyPattern spells it, and returns where it ends; ok is false when none starts there or
// what follows it does not end it (keyEnds).
func keyAt(body string, start int) (end int, ok bool) {
	end = start + 1
	for end < len(body) && (body[end] >= 'A' && body[end] <= 'Z' || isDigitByte(body[end])) {
		end++
	}
	if end-start < 2 || end-start > 10 || end+1 >= len(body) || body[end] != '-' ||
		body[end+1] < '1' || body[end+1] > '9' {
		return 0, false
	}
	end += 2
	for end < len(body) && isDigitByte(body[end]) {
		end++
	}
	return end, keyEnds(body, end)
}

// keyEnds reports whether a key that runs up to end stands alone on its right: at the end of the
// text, or before a character that is not a letter, a digit, `-`, `/`, `#` or `@`. Sentence
// punctuation and the emphasis and strikethrough delimiters (`.` `,` `:` `;` `_` `*` `~`) end it
// only when no letter or digit follows them, so `CORE-1's` and `_CORE-1_.` read CORE-1, and
// `CORE-1.5`, `CORE-1_x`, `CORE-1/spec` and `CORE-1#q1` read nothing.
func keyEnds(body string, end int) bool {
	if end == len(body) {
		return true
	}
	next, size := utf8.DecodeRuneInString(body[end:])
	switch next {
	case '-', '/', '#', '@':
		return false
	case '.', ',', ':', ';', '_', '*', '~':
		after, _ := utf8.DecodeRuneInString(body[end+size:])
		return end+size == len(body) || !isLetterOrDigit(after)
	}
	return !isLetterOrDigit(next)
}

// mentionStart reports whether a bare key or `#id` may start at offset: the start of the text,
// whitespace as referenceEnd spells it, a numeric character reference to such whitespace, or one
// of ( [ " ' * _ > ` ~ |, the characters that open a parenthesis, a link's text, a quote,
// emphasis, a quote block, a code span, strikethrough or a table cell.
func mentionStart(body string, offset int) bool {
	if offset == 0 {
		return true
	}
	before, _ := utf8.DecodeLastRuneInString(body[:offset])
	switch before {
	case '(', '[', '"', '\'', '*', '_', '>', '`', '~', '|':
		return true
	case ';':
		return endsWithWhitespaceReference(body[:offset])
	}
	return isReferenceSpace(before)
}

// endsWithWhitespaceReference reports whether prefix ends with a numeric character reference, as
// CommonMark reads one (`&#` and 1 to 7 decimal digits, or `&#x` and 1 to 6 hex digits, then
// `;`), to a character isReferenceSpace accepts.
func endsWithWhitespaceReference(prefix string) bool {
	window := prefix[max(0, len(prefix)-11) : len(prefix)-1]
	amp := strings.LastIndex(window, "&#")
	if amp < 0 {
		return false
	}
	digits, base, limit := window[amp+2:], 10, 7
	if len(digits) > 0 && (digits[0] == 'x' || digits[0] == 'X') {
		digits, base, limit = digits[1:], 16, 6
	}
	if digits == "" || len(digits) > limit {
		return false
	}
	value, err := strconv.ParseUint(digits, base, 32)
	return err == nil && isReferenceSpace(rune(value))
}

// isReferenceSpace is the whitespace that ends a reference (referenceEnd): tab, line feed, form
// feed, carriage return and the Unicode space separators, not a vertical tab or U+FEFF.
func isReferenceSpace(r rune) bool {
	return r == '\t' || r == '\n' || r == '\f' || r == '\r' || unicode.Is(unicode.Z, r)
}

func isLetterOrDigit(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func isDigitByte(c byte) bool {
	return c >= '0' && c <= '9'
}

// IsBlockIDByte reports whether c is a character of a question block's id, as the `:::ask{#id}`
// line declares it; refs reads a declaration's id and attribute names by the same set.
func IsBlockIDByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || isDigitByte(c) || c == '_' || c == '-'
}
