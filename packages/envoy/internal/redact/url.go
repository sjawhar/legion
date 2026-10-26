// Package redact is how a URL a person configured is printed: in an error, a log line or a
// message, with nothing in it that can carry a credential.
package redact

import "strings"

// mask stands in for each part URL removes, so a reader still sees that the configured value
// carries a credential there.
const mask = "xxxxx"

// URL is raw with everything that can carry a credential replaced by "xxxxx": the userinfo,
// user name and password both (a token is often the user name alone), and the query and
// fragment. It reads raw as a string rather than through net/url, so a value no URL parser
// reads (a misconfigured variable is the case this is printed for most) is redacted too, and it
// errs toward hiding: everything after the scheme up to the last '@', then everything from the
// first '?' or '#'. A URL that carries none of these prints unchanged.
func URL(raw string) string {
	scheme, rest := "", raw
	if i := strings.Index(raw, "://"); i >= 0 {
		scheme, rest = raw[:i+len("://")], raw[i+len("://"):]
	}
	if at := strings.LastIndexByte(rest, '@'); at >= 0 {
		rest = mask + "@" + rest[at+1:]
	}
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i] + "?" + mask
	}
	return scheme + rest
}
