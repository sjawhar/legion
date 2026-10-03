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

// Ref is a parsed Dispatch target. ID is the issue key for issue references,
// an artifact slug for artifacts, the item identifier for asks and comments, and
// the component id for components. Project is set instead of IssueKey for
// unlinked project-document references and for components.
type Ref struct {
	Kind     string
	IssueKey string
	Project  string
	ID       string
}

// Located is a parsed reference and the byte offset of its first character in the body it
// was extracted from.
type Located struct {
	Ref
	Offset int
}

// Extract parses Dispatch links from body. Links outside serverURL stay URL
// references so a post never loses an ordinary external link.
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
	refs := []Located{}
	var base *url.URL
	baseParsed := false
	for _, span := range referencePattern.FindAllStringIndex(body, -1) {
		raw := trimReference(body[span[0]:span[1]])
		if raw == "" {
			continue
		}
		if strings.HasPrefix(raw, "dispatch://") {
			if ref, ok := parseDispatch(strings.TrimPrefix(raw, "dispatch://")); ok {
				refs = append(refs, Located{Ref: ref, Offset: span[0]})
			}
			continue
		}
		if !baseParsed {
			base, _ = url.Parse(serverURL)
			baseParsed = true
		}
		if ref, ok := parseServer(raw, base); ok {
			refs = append(refs, Located{Ref: ref, Offset: span[0]})
			continue
		}
		refs = append(refs, Located{Ref: Ref{Kind: "url", ID: raw}, Offset: span[0]})
	}
	return refs
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
// `parseDispatchReference` reads: a slug as written, an `@v` version, and an item id decoded as
// `itemSegment` decodes it. A reference holding a control character names nothing.
func parseDispatch(value string) (Ref, bool) {
	if hasControl(value) {
		return Ref{}, false
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
// `searchParams` reads it, a version a positive decimal. A URL holding a control character names
// nothing; net/url refuses only the ASCII ones.
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
		// A document page of the issue, which `dispatch_search` links to with the item in its query
		// for an anchored comment or ask.
		case len(parts) == 3 && parts[2] == "spec":
			return documentItem(Ref{Kind: "artifact", IssueKey: key, ID: "spec"}, query)
		case len(parts) == 4 && parts[2] == "artifacts":
			slug, err := url.PathUnescape(parts[3])
			if err != nil || !artifactSlugPattern.MatchString(slug) {
				return Ref{}, false
			}
			if version := query.Get("v"); version != "" && !versionPattern.MatchString(version) {
				return Ref{}, false
			}
			return documentItem(Ref{Kind: "artifact", IssueKey: key, ID: slug}, query)
		case len(parts) == 4 && (parts[2] == "asks" || parts[2] == "comments" || parts[2] == "messages"):
			if id, ok := itemSegment(parts[3]); ok {
				return Ref{Kind: strings.TrimSuffix(parts[2], "s"), IssueKey: key, ID: id}, true
			}
		}
		return Ref{}, false
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
	return documentItem(Ref{Kind: "artifact", Project: parts[1], ID: slug}, query)
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
