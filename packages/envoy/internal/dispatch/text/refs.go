package text

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var referencePattern = regexp.MustCompile(`dispatch://[^\s<>"']+|https?://[^\s<>"']+`)
var issueKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}-[1-9][0-9]*$`)

// Ref is a parsed Dispatch target. ID is the issue key for issue references,
// artifact slug for artifacts, and the item identifier for asks and comments.
type Ref struct {
	Kind     string
	IssueKey string
	ID       string
}

// Extract parses Dispatch links from body. Links outside serverURL stay URL
// references so a post never loses an ordinary external link.
func Extract(body, serverURL string) []Ref {
	refs := []Ref{}
	for _, raw := range referencePattern.FindAllString(body, -1) {
		raw = trimReference(raw)
		if raw == "" {
			continue
		}
		if strings.HasPrefix(raw, "dispatch://") {
			if ref, ok := parseDispatch(strings.TrimPrefix(raw, "dispatch://")); ok {
				refs = append(refs, ref)
			}
			continue
		}
		if ref, ok := parseServer(raw, serverURL); ok {
			refs = append(refs, ref)
			continue
		}
		refs = append(refs, Ref{Kind: "url", ID: raw})
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
	if !issueKeyPattern.MatchString(key) {
		return Ref{}, false
	}
	if !found || tail == "" {
		return Ref{Kind: "issue", IssueKey: key, ID: key}, true
	}
	if tail == "spec" {
		return Ref{Kind: "artifact", IssueKey: key, ID: "spec"}, true
	}
	if artifact, ok := strings.CutPrefix(tail, "artifact/"); ok {
		slug, version, hasVersion := strings.Cut(artifact, "@")
		if hasVersion {
			number, err := strconv.Atoi(strings.TrimPrefix(version, "v"))
			if !strings.HasPrefix(version, "v") || err != nil || number < 1 {
				return Ref{}, false
			}
		}
		if slug != "" && !strings.ContainsAny(slug, "/@") {
			return Ref{Kind: "artifact", IssueKey: key, ID: slug}, true
		}
		return Ref{}, false
	}
	if id, ok := strings.CutPrefix(tail, "ask/"); ok && id != "" && !strings.Contains(id, "/") {
		return Ref{Kind: "ask", IssueKey: key, ID: id}, true
	}
	if id, ok := strings.CutPrefix(tail, "comment/"); ok && id != "" && !strings.Contains(id, "/") {
		return Ref{Kind: "comment", IssueKey: key, ID: id}, true
	}
	return Ref{}, false
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
	if len(parts) < 2 || parts[0] != "issues" {
		return Ref{}, false
	}
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
		if err == nil && slug != "" {
			if rawVersion := value.Query().Get("v"); rawVersion != "" {
				if _, err := strconv.Atoi(rawVersion); err != nil {
					return Ref{}, false
				}
			}
			return Ref{Kind: "artifact", IssueKey: key, ID: slug}, true
		}
	}
	if len(parts) == 4 && parts[2] == "asks" && parts[3] != "" {
		return Ref{Kind: "ask", IssueKey: key, ID: parts[3]}, true
	}
	if len(parts) == 4 && parts[2] == "comments" && parts[3] != "" {
		return Ref{Kind: "comment", IssueKey: key, ID: parts[3]}, true
	}
	return Ref{}, false
}
