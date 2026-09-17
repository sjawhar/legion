package text

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var referencePattern = regexp.MustCompile(`dispatch://[^\s<>"']+|https?://[^\s<>"']+`)
var issueKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}-[1-9][0-9]*$`)
var projectKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)
var artifactSlugPrefixPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*`)
var artifactSlugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// componentIDPattern is the architecture importer's component id charset (a lowercase slug).
var componentIDPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

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
		if ref, ok := parseServer(raw, serverURL); ok {
			refs = append(refs, Located{Ref: ref, Offset: span[0]})
			continue
		}
		refs = append(refs, Located{Ref: Ref{Kind: "url", ID: raw}, Offset: span[0]})
	}
	return refs
}

func trimReference(raw string) string {
	for {
		trimmed := strings.TrimRight(raw, ".,;:!?")
		if trimmed != raw {
			raw = trimmed
			continue
		}
		if raw == "" {
			return raw
		}
		last := raw[len(raw)-1]
		var opener byte
		switch last {
		case ')':
			opener = '('
		case ']':
			opener = '['
		case '>':
			opener = '<'
		default:
			return raw
		}
		if strings.Count(raw, string(opener)) >= strings.Count(raw, string(last)) {
			return raw
		}
		raw = raw[:len(raw)-1]
	}
}

func parseDispatch(value string) (Ref, bool) {
	key, tail, found := strings.Cut(value, "/")
	if issueKeyPattern.MatchString(key) {
		if !found || tail == "" {
			return Ref{Kind: "issue", IssueKey: key, ID: key}, true
		}
		if tail == "spec" {
			return Ref{Kind: "artifact", IssueKey: key, ID: "spec"}, true
		}
		if artifact, ok := strings.CutPrefix(tail, "artifact/"); ok {
			slug, ok := parseArtifactSlug(artifact)
			if ok {
				return Ref{Kind: "artifact", IssueKey: key, ID: slug}, true
			}
			return Ref{}, false
		}
		if id, ok := strings.CutPrefix(tail, "ask/"); ok && validItemID(id) {
			return Ref{Kind: "ask", IssueKey: key, ID: id}, true
		}
		if id, ok := strings.CutPrefix(tail, "comment/"); ok && validItemID(id) {
			return Ref{Kind: "comment", IssueKey: key, ID: id}, true
		}
		if id, ok := strings.CutPrefix(tail, "message/"); ok && validItemID(id) {
			return Ref{Kind: "message", IssueKey: key, ID: id}, true
		}
		return Ref{}, false
	}
	if !projectKeyPattern.MatchString(key) || !found {
		return Ref{}, false
	}
	if id, ok := strings.CutPrefix(tail, "component/"); ok {
		if componentIDPattern.MatchString(id) {
			return Ref{Kind: "component", Project: key, ID: id}, true
		}
		return Ref{}, false
	}
	artifact, ok := strings.CutPrefix(tail, "artifact/")
	if !ok {
		return Ref{}, false
	}
	parts := strings.Split(artifact, "/")
	if len(parts) == 1 {
		slug, ok := parseArtifactSlug(parts[0])
		if !ok {
			return Ref{}, false
		}
		return Ref{Kind: "artifact", Project: key, ID: slug}, true
	}
	if len(parts) == 3 && validItemID(parts[2]) {
		if _, ok := parseArtifactSlug(parts[0]); ok {
			switch parts[1] {
			case "ask":
				return Ref{Kind: "ask", Project: key, ID: parts[2]}, true
			case "comment":
				return Ref{Kind: "comment", Project: key, ID: parts[2]}, true
			}
		}
	}
	return Ref{}, false
}

func parseArtifactSlug(value string) (string, bool) {
	slug, version, hasVersion := strings.Cut(value, "@")
	if hasVersion {
		number, err := strconv.Atoi(strings.TrimPrefix(version, "v"))
		if !strings.HasPrefix(version, "v") || err != nil || number < 1 {
			return "", false
		}
	}
	if strings.Contains(slug, "/") {
		return "", false
	}
	slug = artifactSlugPrefixPattern.FindString(slug)
	if !artifactSlugPattern.MatchString(slug) {
		return "", false
	}
	return slug, true
}

func validItemID(value string) bool {
	return value != "" && !strings.Contains(value, "/")
}

func parseServer(raw, serverURL string) (Ref, bool) {
	base, err := url.Parse(serverURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
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
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) >= 2 && parts[0] == "issues" {
		key, err := url.PathUnescape(parts[1])
		if err != nil || !issueKeyPattern.MatchString(key) {
			return Ref{}, false
		}
		if len(parts) == 2 {
			return Ref{Kind: "issue", IssueKey: key, ID: key}, true
		}
		if len(parts) == 3 && parts[2] == "spec" {
			return Ref{Kind: "artifact", IssueKey: key, ID: "spec"}, true
		}
		if len(parts) == 4 && parts[2] == "artifacts" {
			slug, err := url.PathUnescape(parts[3])
			if err == nil && artifactSlugPattern.MatchString(slug) {
				if rawVersion := value.Query().Get("v"); rawVersion != "" {
					number, err := strconv.Atoi(rawVersion)
					if err != nil || number < 1 {
						return Ref{}, false
					}
				}
				return Ref{Kind: "artifact", IssueKey: key, ID: slug}, true
			}
		}
		if len(parts) == 4 && parts[2] == "asks" && validItemID(parts[3]) {
			return Ref{Kind: "ask", IssueKey: key, ID: parts[3]}, true
		}
		if len(parts) == 4 && parts[2] == "comments" && validItemID(parts[3]) {
			return Ref{Kind: "comment", IssueKey: key, ID: parts[3]}, true
		}
		if len(parts) == 4 && parts[2] == "messages" && validItemID(parts[3]) {
			return Ref{Kind: "message", IssueKey: key, ID: parts[3]}, true
		}
		return Ref{}, false
	}
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != "documents" {
		return Ref{}, false
	}
	project, err := url.PathUnescape(parts[1])
	if err != nil || !projectKeyPattern.MatchString(project) {
		return Ref{}, false
	}
	slug, err := url.PathUnescape(parts[3])
	if err != nil || !artifactSlugPattern.MatchString(slug) {
		return Ref{}, false
	}
	query := value.Query()
	if rawVersion := query.Get("version"); rawVersion != "" {
		number, err := strconv.Atoi(rawVersion)
		if err != nil || number < 1 {
			return Ref{}, false
		}
	}
	ask, comment := query.Get("ask"), query.Get("comment")
	if ask != "" && comment != "" {
		return Ref{}, false
	}
	if validItemID(ask) {
		return Ref{Kind: "ask", Project: project, ID: ask}, true
	}
	if validItemID(comment) {
		return Ref{Kind: "comment", Project: project, ID: comment}, true
	}
	if ask != "" || comment != "" {
		return Ref{}, false
	}
	return Ref{Kind: "artifact", Project: project, ID: slug}, true
}
