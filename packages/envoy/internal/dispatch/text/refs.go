package text

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// referenceEnd is every character that ends a reference: whitespace (the Unicode space separators
// a no-break or ideographic space belongs to as well), an angle or square bracket, a quote, a
// backtick or a pipe. No Dispatch reference holds one, so a reference in a code span ends at its
// closing backtick, one in a table cell at the cell's `|` however the cell is padded, and the link
// a document stores for the autolink `<dispatch://CORE-1>`,
// `[dispatch://CORE-1](dispatch://CORE-1)`, is two references to CORE-1 rather than one
// unparseable one. The whitespace is spelled out rather than written `\s`, which means another set
// in each language: JavaScript's also matches a vertical tab and U+FEFF. The dashboard composer
// reads text by the same rule, and `testdata/dispatch-text-references.json` is the table both are
// tested against.
const referenceEnd = `\t\n\f\r \p{Z}<>"'` + "`" + `\[\]|`

// referencePattern is a reference span. The one bracket a span may hold is a bracketed IPv6 host
// right after `http(s)://`, so a server at an IPv6 literal keeps its dashboard URLs as references.
var referencePattern = regexp.MustCompile(`(?:dispatch://|https?://(?:\[[0-9A-Fa-f:.]+\])?)[^` + referenceEnd + `]+`)
var issueKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}-[1-9][0-9]*$`)
var projectKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)
var artifactSlugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// versionPattern is a document version as a reference writes one: a positive decimal, with no sign
// and no leading zero.
var versionPattern = regexp.MustCompile(`^[1-9][0-9]*$`)

// ComponentIDPattern is the component id charset (a lowercase slug), the regexp
// IsComponentID matches; the architecture importer quotes it in its file-name error.
const ComponentIDPattern = `^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`

var componentIDPattern = regexp.MustCompile(ComponentIDPattern)

func hasControl(value string) bool {
	return strings.ContainsFunc(value, unicode.IsControl)
}

// IsComponentID reports whether value is a component id: a lowercase slug of
// [a-z0-9] with single-character-bounded hyphens, such as web or dispatch-server.
func IsComponentID(value string) bool {
	return componentIDPattern.MatchString(value)
}

// IsSessionID reports whether value can own an artifact: an Envoy session id as the Agents page
// shows it, which a reference carries as written. It is not empty and holds no `/`, `?` or `#`,
// no whitespace or control character, and none of the characters that end a reference in text
// (referenceEnd), so every owner the API accepts is one a reference can name.
func IsSessionID(value string) bool {
	return value != "" && !strings.ContainsFunc(value, func(r rune) bool {
		return strings.ContainsRune(`/?#<>"'`+"`"+`[]|`, r) || r == '\ufeff' ||
			unicode.IsSpace(r) || unicode.Is(unicode.Z, r) || unicode.IsControl(r)
	})
}

// Ref is a parsed Dispatch target. ID is the issue key for issue references,
// an artifact slug for artifacts, the item identifier for asks and comments, and
// the component id for components. Project is set instead of IssueKey for
// unlinked project-document references and for components, and Session instead of
// both for an artifact an agent's conversation owns (`agent/<session id>/artifact/<slug>`).
// A block of a document (Kind "block") carries its document's fields, the block's id as
// written in the document in Block, and in Pin the version a pinned reference (`@v<N>` after
// the fragment) names, or 0.
type Ref struct {
	Kind     string
	IssueKey string
	Project  string
	Session  string
	ID       string
	Block    string
	Pin      int
}

// Located is a parsed reference and the byte offset of its first character in the body it
// was extracted from.
type Located struct {
	Ref
	Offset int
}

// Extract parses Dispatch links from body. Links outside serverURL stay URL
// references so a post never loses an ordinary external link. It reads what text cites, which the
// graph indexes, so a reference whose target id is past MaxTargetBytes (targetFits) names nothing
// there: a `dispatch://` one is dropped, a dashboard page whose block is past it names its
// document (pageInBound), and one whose document or item is past it is the URL it is, as one that
// names no page is.
func Extract(body, serverURL string) []Ref {
	located := ExtractAt(body, serverURL)
	refs := make([]Ref, len(located))
	for index, item := range located {
		refs[index] = item.Ref
	}
	return refs
}

// ExtractAt is Extract with each reference's byte offset into body, so a caller can find the
// block a mention sits in. Trailing punctuation the grammar trims never moves the start.
func ExtractAt(body, serverURL string) []Located {
	return extractAt(body, serverURL, true)
}

// ParseRef reads value, the one reference a request names - an API route's `?to=` or `?from=`, or
// a block-links document - by the grammar Extract reads text by, without the target bound: the
// bound decides only which references text indexes, so a node whatever the length of its name is
// read by its own reference. value names a reference when it starts with one and holds no other,
// as Extract would find it in text; an external URL or a dashboard URL that names no page is
// none.
func ParseRef(value, serverURL string) (Ref, bool) {
	located := extractAt(value, serverURL, false)
	if len(located) != 1 || located[0].Offset != 0 || located[0].Kind == "url" {
		return Ref{}, false
	}
	return located[0].Ref, true
}

// extractAt is ExtractAt, holding what each reference names to the target bound when bounded
// (Extract's reading of text) and not when not (ParseRef's of a request).
func extractAt(body, serverURL string, bounded bool) []Located {
	refs := []Located{}
	var base *url.URL
	baseParsed := false
	for _, span := range referencePattern.FindAllStringIndex(body, -1) {
		raw := trimReference(body[span[0]:span[1]])
		if raw == "" {
			continue
		}
		if strings.HasPrefix(raw, "dispatch://") {
			if ref, ok := parseDispatch(strings.TrimPrefix(raw, "dispatch://")); ok && (!bounded || targetFits(ref)) {
				refs = append(refs, Located{Ref: ref, Offset: span[0]})
			}
			continue
		}
		if !baseParsed {
			base, _ = url.Parse(serverURL)
			baseParsed = true
		}
		if ref, ok := parseServer(raw, base); ok {
			if bounded {
				ref, ok = pageInBound(ref)
			}
			if ok {
				refs = append(refs, Located{Ref: ref, Offset: span[0]})
				continue
			}
		}
		refs = append(refs, Located{Ref: Ref{Kind: "url", ID: raw}, Offset: span[0]})
	}
	return refs
}

// pageInBound is what a dashboard page cites in text: what the page names while its target id
// fits (targetFits), else the document of a block that does not, while the document's fits - the
// page still opens that document - else nothing.
func pageInBound(ref Ref) (Ref, bool) {
	if targetFits(ref) {
		return ref, true
	}
	if ref.Kind != "block" {
		return Ref{}, false
	}
	ref.Kind, ref.Block = "artifact", ""
	return ref, targetFits(ref)
}

// trimReference drops what trails a reference in prose: sentence punctuation, the `*`, `_` and `~`
// that close emphasis and strikethrough around it (GFM's autolinks drop the same characters,
// keeping them inside a link), and a closing parenthesis that opens nowhere in the reference. It
// counts the parentheses once and walks back from the end, so a run of closers costs one pass.
func trimReference(raw string) string {
	opened, closed := strings.Count(raw, "("), strings.Count(raw, ")")
	end := len(raw)
	for ; end > 0; end-- {
		switch raw[end-1] {
		case '.', ',', ';', ':', '!', '?', '*', '_', '~':
		case ')':
			if opened >= closed {
				return raw[:end]
			}
			closed--
		default:
			return raw[:end]
		}
	}
	return ""
}

// parseDispatch reads a `dispatch://` reference, without its scheme, by the grammar the dashboard's
// `parseDispatchReference` reads: a slug as written, an `@v` version, an agent's session id as
// written, an item id decoded as `itemSegment` decodes it, and a `#b-` block of a document as
// parseBlock reads it. A reference holding a control character names nothing.
func parseDispatch(value string) (Ref, bool) {
	if hasControl(value) {
		return Ref{}, false
	}
	if document, fragment, found := strings.Cut(value, "#"); found {
		return parseBlock(document, fragment)
	}
	key, tail, found := strings.Cut(value, "/")
	if issueKeyPattern.MatchString(key) {
		if !found || tail == "" {
			return Ref{Kind: "issue", IssueKey: key, ID: key}, true
		}
		if tail == "spec" {
			return Ref{Kind: "artifact", IssueKey: key, ID: "spec"}, true
		}
		if artifact, ok := strings.CutPrefix(tail, "artifact/"); ok {
			if slug, ok := parseArtifactSlug(artifact); ok {
				return Ref{Kind: "artifact", IssueKey: key, ID: slug}, true
			}
			return Ref{}, false
		}
		kind, segment, _ := strings.Cut(tail, "/")
		if kind == "ask" || kind == "comment" || kind == "message" {
			if id, ok := itemSegment(segment); ok {
				return Ref{Kind: kind, IssueKey: key, ID: id}, true
			}
		}
		return Ref{}, false
	}
	if key == "agent" {
		return parseAgentArtifact(tail)
	}
	if !projectKeyPattern.MatchString(key) || !found {
		return Ref{}, false
	}
	if id, ok := strings.CutPrefix(tail, "component/"); ok {
		if IsComponentID(id) {
			return Ref{Kind: "component", Project: key, ID: id}, true
		}
		return Ref{}, false
	}
	artifact, ok := strings.CutPrefix(tail, "artifact/")
	if !ok {
		return Ref{}, false
	}
	parts := strings.Split(artifact, "/")
	slug, ok := parseArtifactSlug(parts[0])
	if !ok {
		return Ref{}, false
	}
	switch {
	case len(parts) == 1:
		return Ref{Kind: "artifact", Project: key, ID: slug}, true
	case len(parts) == 3 && (parts[1] == "ask" || parts[1] == "comment"):
		if id, ok := itemSegment(parts[2]); ok {
			return Ref{Kind: parts[1], Project: key, ID: id}, true
		}
	}
	return Ref{}, false
}

// parseAgentArtifact reads what follows `agent/` in a reference: `<session id>/artifact/<slug>`
// with an optional `@v` version, the one thing an agent's conversation owns.
func parseAgentArtifact(tail string) (Ref, bool) {
	session, rest, _ := strings.Cut(tail, "/")
	artifact, ok := strings.CutPrefix(rest, "artifact/")
	if !ok || !IsSessionID(session) {
		return Ref{}, false
	}
	slug, ok := parseArtifactSlug(artifact)
	if !ok {
		return Ref{}, false
	}
	return Ref{Kind: "artifact", Session: session, ID: slug}, true
}

// parseArtifactSlug reads a slug as a reference writes it, whole, with an optional `@v` version.
func parseArtifactSlug(value string) (string, bool) {
	slug, version, versioned := strings.Cut(value, "@")
	if versioned {
		number, ok := strings.CutPrefix(version, "v")
		if !ok || !versionPattern.MatchString(number) {
			return "", false
		}
	}
	if !artifactSlugPattern.MatchString(slug) {
		return "", false
	}
	return slug, true
}

// MaxBlockIDBytes bounds a block id, decoded, in UTF-8 bytes. The reference graph keys a block by
// it in B-tree keys - document_blocks', document_block_texts' and block_refs' primary keys, and a
// reference's refs.to_id - and a Postgres B-tree key holds at most 2704 bytes, so an unbounded id
// fails every write that indexes it. (A block's node id `<ref_key>#<block id>` holds the
// document's unbounded ref_key too, so it is hash indexed, never a B-tree key.) A reference naming
// a longer id names nothing (blockID), and the index makes no node of a block carrying one
// (refs.IndexBlocks). It decides only which blocks the graph keys: a block whose id is longer is
// written, stored, read and rendered as any other. Its twin is MAX_BLOCK_ID_BYTES in
// @legion/contracts.
const MaxBlockIDBytes = 256

// MaxTargetBytes bounds a reference's target id in text, in bytes: TargetID, which refs.to_id and
// block_refs.to_id store in B-tree keys (refs_pkey, refs_to, block_refs_pkey, block_refs_to) of
// at most 2704 bytes. The grammar bounds no part of that id but a block id (MaxBlockIDBytes) and a
// project key, so text citing a target whose id is past this cites nothing (targetFits): Extract
// finds no reference there, and a live document page whose fragment names a block past it names
// the document. The bound decides only what text indexes: data stored under a longer name - a
// document whose ref_key is longer - is read and written all the same, and a request naming it
// reads it (ParseRef). At the bound the widest of those keys, block_refs_pkey with a citing block
// id of MaxBlockIDBytes, is about 2360 bytes, and refs_pkey with a uuid source and a `component`
// target about 2120. Its twin is MAX_REFERENCE_TARGET_BYTES in @legion/contracts.
const MaxTargetBytes = 2048

// TargetID is the durable id the reference graph stores an edge to ref under, refs.to_id and
// block_refs.to_id: an issue's key; a document's ref_key, `<issue key or project>/<slug>` (`spec`
// for an issue's spec) or `agent/<session id>/<slug>`; a block's `<document's ref_key>#<block id>`;
// a component's `<project>/<id>` node address; and an ask's, a comment's or a message's id. A
// document's version and a block's pin are not part of it: a pinned and an unpinned reference to
// one block are one target.
func TargetID(ref Ref) string {
	switch ref.Kind {
	case "artifact":
		if ref.Session != "" {
			return "agent/" + ref.Session + "/" + ref.ID
		}
		key := ref.IssueKey
		if key == "" {
			key = ref.Project
		}
		return key + "/" + ref.ID
	case "component":
		return ref.Project + "/" + ref.ID
	case "block":
		document := ref
		document.Kind, document.Block, document.Pin = "artifact", "", 0
		return TargetID(document) + "#" + ref.Block
	}
	return ref.ID
}

// targetFits reports whether ref, a parsed reference, names a target id (TargetID) of at most
// MaxTargetBytes.
func targetFits(ref Ref) bool {
	return len(TargetID(ref)) <= MaxTargetBytes
}

// EncodeBlockID writes a block id as a reference's `#b-` fragment carries it: every UTF-8 byte
// percent-encoded in upper-case hex except ASCII letters, digits and `-`, so no character a
// reference ends at or the grammar trims, and no `@` a pin could be read from, is written bare.
// Its twin is encodeBlockId in @legion/contracts; both are tested against
// `testdata/dispatch-block-id-encodings.json`.
func EncodeBlockID(id string) string {
	const digits = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(id))
	for i := range len(id) {
		c := id[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(digits[c>>4])
		b.WriteByte(digits[c&15])
	}
	return b.String()
}

// parseBlock reads `<document reference>#b-<block id>[@v<N>]`: a block of an issue's spec, an
// issue's document or a project's document, optionally pinned to version N of that document. The
// id is read with any valid percent-encoding (EncodeBlockID writes one), so `%40` is an `@` in the
// id and a raw `@` is only ever the pin. Any other fragment, a raw `@` that does not start a
// well-formed pin, a versioned document, an agent conversation's document, an item, or an id that
// is empty, does not decode or is longer than MaxBlockIDBytes names nothing. (Extract holds the
// block's whole target id to MaxTargetBytes in text, as every reference's.) A document version
// shows only as an `@` in the document part: no key or slug holds one.
func parseBlock(document, fragment string) (Ref, bool) {
	encoded, ok := strings.CutPrefix(fragment, "b-")
	if !ok || strings.Contains(document, "@") {
		return Ref{}, false
	}
	pin := 0
	if unpinned, version, pinned := strings.Cut(encoded, "@"); pinned {
		number, ok := strings.CutPrefix(version, "v")
		if !ok || !versionPattern.MatchString(number) {
			return Ref{}, false
		}
		parsed, err := strconv.Atoi(number)
		if err != nil {
			return Ref{}, false
		}
		encoded, pin = unpinned, parsed
	}
	id, ok := blockID(encoded)
	if !ok {
		return Ref{}, false
	}
	ref, ok := parseDispatch(document)
	if !ok || ref.Kind != "artifact" || ref.Session != "" {
		return Ref{}, false
	}
	ref.Kind, ref.Block, ref.Pin = "block", id, pin
	return ref, true
}

// blockID decodes the id part of a `#b-` fragment: not empty, holding no raw `@`, and decoding
// as decodeItemID decodes an item id to a non-empty id of at most MaxBlockIDBytes.
func blockID(encoded string) (string, bool) {
	if encoded == "" || strings.Contains(encoded, "@") {
		return "", false
	}
	id, ok := decodeItemID(encoded)
	return id, ok && id != "" && len(id) <= MaxBlockIDBytes
}

// itemSegment is an item id as a reference writes it, decoded, as the dashboard reads one: written
// without a `/`, `?`, `#` or whitespace (JavaScript's `\s`, which adds U+FEFF to the Unicode space
// separators; its ASCII whitespace is control characters, which no reference holds), then decoded
// by `decodeItemID`.
func itemSegment(raw string) (string, bool) {
	if raw == "" || strings.ContainsFunc(raw, func(r rune) bool {
		return r == '/' || r == '?' || r == '#' || r == '\ufeff' || unicode.Is(unicode.Z, r)
	}) {
		return "", false
	}
	return decodeItemID(raw)
}

// decodeItemID decodes an item id as JavaScript's decodeURIComponent does, refusing a `%` that two
// hex digits do not follow and escapes that do not decode to UTF-8, and refuses an id that decodes
// to a control character, as a reference written with one is refused: no item has such an id, and
// the index binds ids as `text[]`, where Postgres refuses a NUL and fails the write holding it.
func decodeItemID(value string) (string, bool) {
	decoded, err := url.PathUnescape(value)
	return decoded, err == nil && utf8.ValidString(decoded) && !hasControl(decoded)
}

// parseServer reads a dashboard URL against its parsed server URL as the dashboard's
// `referenceRouteFromHref` reads it: the key and the path's item id as written, the query as
// `searchParams` reads it, a version a positive decimal, and a `#b-` fragment on a document page
// as documentPage reads it. A URL holding a control character names nothing; net/url refuses only
// the ASCII ones.
func parseServer(raw string, base *url.URL) (Ref, bool) {
	if base == nil || base.Scheme == "" || base.Host == "" {
		return Ref{}, false
	}
	if hasControl(raw) {
		return Ref{}, false
	}
	value, err := url.Parse(raw)
	if err != nil || value.Scheme != base.Scheme || value.Host != base.Host {
		return Ref{}, false
	}
	basePath := strings.TrimSuffix(base.EscapedPath(), "/")
	path := value.EscapedPath()
	if basePath != "" {
		if !strings.HasPrefix(path, basePath+"/") {
			return Ref{}, false
		}
		path = strings.TrimPrefix(path, basePath)
	}
	query := searchParams(value.RawQuery)
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) >= 2 && parts[0] == "issues" {
		key := parts[1]
		if !issueKeyPattern.MatchString(key) {
			return Ref{}, false
		}
		switch {
		case len(parts) == 2:
			return Ref{Kind: "issue", IssueKey: key, ID: key}, true
		// A document page of the issue, which `dispatch search` links to with the item in its query
		// for an anchored comment or ask.
		case len(parts) == 3 && parts[2] == "spec":
			return documentPage(Ref{Kind: "artifact", IssueKey: key, ID: "spec"}, query, "v", value)
		case len(parts) == 4 && parts[2] == "artifacts":
			slug, err := url.PathUnescape(parts[3])
			if err != nil || !artifactSlugPattern.MatchString(slug) {
				return Ref{}, false
			}
			if version := query.Get("v"); version != "" && !versionPattern.MatchString(version) {
				return Ref{}, false
			}
			return documentPage(Ref{Kind: "artifact", IssueKey: key, ID: slug}, query, "v", value)
		case len(parts) == 4 && (parts[2] == "asks" || parts[2] == "comments" || parts[2] == "messages"):
			if id, ok := itemSegment(parts[3]); ok {
				return Ref{Kind: strings.TrimSuffix(parts[2], "s"), IssueKey: key, ID: id}, true
			}
		}
		return Ref{}, false
	}
	if len(parts) == 4 && parts[0] == "agents" && parts[2] == "artifacts" {
		return agentArtifactPage(parts[1], parts[3], query)
	}
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != "documents" ||
		!projectKeyPattern.MatchString(parts[1]) {
		return Ref{}, false
	}
	slug, err := url.PathUnescape(parts[3])
	if err != nil || !artifactSlugPattern.MatchString(slug) {
		return Ref{}, false
	}
	if version := query.Get("version"); version != "" && !versionPattern.MatchString(version) {
		return Ref{}, false
	}
	return documentPage(Ref{Kind: "artifact", Project: parts[1], ID: slug}, query, "version", value)
}

// documentPage reads a document page of an issue or a project: the item its query names, as
// documentItem reads it, else the block its `#b-<block id>` fragment names (blockID; a raw `@` is
// no pin here, since pins are written only as `dispatch://` references), else the document. A
// version page (`versionParam` set) is the document whatever its fragment: a version's stored
// markdown carries no block ids to land on. Extract holds what this names to MaxTargetBytes in
// text (pageInBound): a block past it is the document, a document or item past it nothing.
func documentPage(document Ref, query url.Values, versionParam string, page *url.URL) (Ref, bool) {
	ref, ok := documentItem(document, query)
	if !ok || ref.Kind != "artifact" || query.Get(versionParam) != "" {
		return ref, ok
	}
	if encoded, found := strings.CutPrefix(page.EscapedFragment(), "b-"); found {
		if id, ok := blockID(encoded); ok {
			ref.Kind, ref.Block = "block", id
		}
	}
	return ref, true
}

// agentArtifactPage reads the dashboard page of an artifact an agent's conversation owns,
// `/agents/<session id>/artifacts/<slug>[?v=N]`: both segments decoded as the browser encoded
// them, the session id then held to IsSessionID. The conversation owns no comment or ask, so a
// `?comment=` or `?ask=` there, which Dispatch never emits, names nothing.
func agentArtifactPage(sessionSegment, slugSegment string, query url.Values) (Ref, bool) {
	session, err := url.PathUnescape(sessionSegment)
	if err != nil || !IsSessionID(session) {
		return Ref{}, false
	}
	slug, err := url.PathUnescape(slugSegment)
	if err != nil || !artifactSlugPattern.MatchString(slug) {
		return Ref{}, false
	}
	if version := query.Get("v"); version != "" && !versionPattern.MatchString(version) {
		return Ref{}, false
	}
	if query.Has("comment") || query.Has("ask") {
		return Ref{}, false
	}
	return Ref{Kind: "artifact", Session: session, ID: slug}, true
}

// documentItem is the one link rule every Dispatch URL parser applies: `?comment=` or `?ask=` on a
// document path names that item, not the document. A query Dispatch never emits - both parameters
// at once, or an id that is empty, holds a `/` or does not decode (`decodeItemID`) - names nothing
// rather than a guess. The same rule is `itemFromSearch` in `@legion/contracts` for the SPA and the
// agent client, and `testdata/dispatch-href-references.json` is the one table all three are tested
// against.
func documentItem(document Ref, query url.Values) (Ref, bool) {
	hasAsk, hasComment := query.Has("ask"), query.Has("comment")
	if !hasAsk && !hasComment {
		return document, true
	}
	if hasAsk && hasComment {
		return Ref{}, false
	}
	kind := "comment"
	if hasAsk {
		kind = "ask"
	}
	raw := query.Get(kind)
	if raw == "" || strings.Contains(raw, "/") {
		return Ref{}, false
	}
	id, ok := decodeItemID(raw)
	if !ok {
		return Ref{}, false
	}
	return Ref{Kind: kind, IssueKey: document.IssueKey, Project: document.Project, ID: id}, true
}

// searchParams reads a query as the browser's URLSearchParams does, which is how the dashboard
// reads one: pairs split on `&` alone, `+` a space, `%` and two hex digits a byte and any other
// `%` itself, and the bytes UTF-8. net/url's ParseQuery is not that reading: it also splits on
// `;`, and drops a pair holding one or holding a bad escape, so it names the document where the
// dashboard names the item, or a version the dashboard refuses.
func searchParams(rawQuery string) url.Values {
	params := url.Values{}
	for pair := range strings.SplitSeq(rawQuery, "&") {
		if pair != "" {
			name, value, _ := strings.Cut(pair, "=")
			params.Add(formDecode(name), formDecode(value))
		}
	}
	return params
}

func formDecode(value string) string {
	decoded := make([]byte, 0, len(value))
	for index := 0; index < len(value); index++ {
		char := value[index]
		if char == '%' && index+2 < len(value) {
			if escaped, err := strconv.ParseUint(value[index+1:index+3], 16, 8); err == nil {
				decoded = append(decoded, byte(escaped))
				index += 2
				continue
			}
		}
		if char == '+' {
			char = ' '
		}
		decoded = append(decoded, char)
	}
	return strings.ToValidUTF8(string(decoded), "\uFFFD")
}
