// Package redact is how a URL a person configured is printed: in an error, a log line or a
// message, with nothing in it that can carry a credential.
package redact

import (
	"errors"
	"net/url"
	"strings"
)

// mask stands in for each part URL removes, so a reader still sees that the configured value
// carries something there.
const mask = "xxxxx"

// parts is raw split as URL reads it. Each field is a slice of raw: prefix is the scheme with
// its separator as written ("https://", or "http:/" and "https//" for a mistyped http(s)), and
// the others hold no separator: userinfo ends before its '@', query follows its '?' and
// fragment its '#'.
type parts struct {
	prefix, userinfo, rest, query, fragment string
	hasUser, hasQuery, hasFragment          bool
}

// split reads raw as a string rather than through net/url, so a value no URL parser reads (a
// misconfigured variable is the case it is printed for most) is read too, and errs toward hiding:
//   - the prefix is a scheme name (a letter, then letters, digits, '+', '-' or '.') followed by
//     "://", or "http" or "https" followed by ":/" or "//"; anything else has no prefix, so a
//     user name before a ':' is never printed as a scheme;
//   - the userinfo is everything after the prefix up to the last '@' anywhere in raw. A password
//     or token holding a '/', '?', '#', ':' or digits (which a URL must escape, or which reads as
//     a port) therefore hides whole; an '@' in a path or query hides the host before it too,
//     which a broker's base URL, having neither, never shows;
//   - the query is what follows the first '?' after the userinfo, up to a '#', and the fragment
//     what follows the first '#'.
func split(raw string) parts {
	var p parts
	p.prefix = prefix(raw)
	s := raw[len(p.prefix):]
	if at := strings.LastIndexByte(s, '@'); at >= 0 {
		p.userinfo, s, p.hasUser = s[:at], s[at+1:], true
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		tail := s[i+1:]
		if s[i] == '?' {
			p.hasQuery = true
			if j := strings.IndexByte(tail, '#'); j >= 0 {
				p.query, p.fragment, p.hasFragment = tail[:j], tail[j+1:], true
			} else {
				p.query = tail
			}
		} else {
			p.fragment, p.hasFragment = tail, true
		}
		s = s[:i]
	}
	p.rest = s
	return p
}

// prefix is raw's scheme with its separator as split reads it, or "" for none.
func prefix(raw string) string {
	n := 0
	for n < len(raw) {
		c := raw[n]
		letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if !letter && (n == 0 || !(c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.')) {
			break
		}
		n++
	}
	if n == 0 {
		return ""
	}
	name, after := raw[:n], raw[n:]
	if strings.HasPrefix(after, "://") {
		return name + "://"
	}
	if strings.EqualFold(name, "http") || strings.EqualFold(name, "https") {
		for _, sep := range []string{":/", "//"} {
			if strings.HasPrefix(after, sep) {
				return name + sep
			}
		}
	}
	return ""
}

// URL is raw with everything that can carry a credential replaced by "xxxxx": the userinfo,
// user name and password both (a token is often the user name alone), the query and the
// fragment, each keeping its separator ("xxxxx@", "?xxxxx", "#xxxxx"). A URL that carries none of
// them prints unchanged. split says how raw is read.
func URL(raw string) string {
	p := split(raw)
	var b strings.Builder
	b.WriteString(p.prefix)
	if p.hasUser {
		b.WriteString(masked(p.userinfo) + "@")
	}
	b.WriteString(p.rest)
	if p.hasQuery {
		b.WriteString("?" + masked(p.query))
	}
	if p.hasFragment {
		b.WriteString("#" + masked(p.fragment))
	}
	return b.String()
}

// masked is mask for a part that holds anything, and the empty part as it is.
func masked(part string) string {
	if part == "" {
		return ""
	}
	return mask
}

// hidden is what URL hides of raw, each user name, password, query and fragment, as written and
// unescaped, for a test of whether a message repeats one.
func hidden(raw string) []string {
	p := split(raw)
	var out []string
	add := func(s string) {
		if s == "" {
			return
		}
		out = append(out, s)
		if unescaped, err := url.PathUnescape(s); err == nil && unescaped != s {
			out = append(out, unescaped)
		}
	}
	if p.hasUser {
		add(p.userinfo)
		// Each piece between separators too, so a cause naming the host net/url read out of a
		// token holding a '/' (a DNS lookup of it, say) is caught.
		for _, piece := range strings.FieldsFunc(p.userinfo, func(r rune) bool { return strings.ContainsRune(":/?#@", r) }) {
			add(piece)
		}
	}
	add(p.query)
	add(p.fragment)
	return out
}

// withheld stands in for the error a *url.Error wraps when that error's text could repeat part
// of the URL: its text says only why, and it unwraps to the original, so errors.Is and errors.As
// still classify the failure.
type withheld struct {
	text string
	err  error
}

func (w withheld) Error() string { return w.text }
func (w withheld) Unwrap() error { return w.err }

// Timeout and Temporary answer as the withheld error does, since (*url.Error).Timeout and
// Temporary ask the error they wrap directly, not through Unwrap.
func (w withheld) Timeout() bool {
	t, ok := w.err.(interface{ Timeout() bool })
	return ok && t.Timeout()
}

func (w withheld) Temporary() bool {
	t, ok := w.err.(interface{ Temporary() bool })
	return ok && t.Temporary()
}

// Error is err with the URL of the *url.Error it holds (net/http's and net/url's errors name the
// URL they were given; net/http masks a password as "***" but prints the user name and the
// query) printed through URL, and that error's own cause withheld when its text could repeat a
// credential: always for a URL that did not parse (its cause quotes the part it could not read,
// a bad escape in a password among them), which then reads "not a valid URL", and for any other
// cause whose text names a part URL hides (a scheme-less value whose user name net/url read as a
// scheme, say). It changes err in place and returns it; an err holding no *url.Error is returned
// as it is.
func Error(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	raw := ue.URL
	ue.URL = URL(raw)
	if _, done := ue.Err.(withheld); done || ue.Err == nil {
		return err
	}
	if ue.Op == "parse" {
		ue.Err = withheld{"not a valid URL", ue.Err}
		return err
	}
	text := ue.Err.Error()
	for _, part := range hidden(raw) {
		if strings.Contains(text, part) {
			ue.Err = withheld{"(withheld: it repeats a part of the URL that can carry a credential)", ue.Err}
			break
		}
	}
	return err
}
