// Package airlock holds the airlock's rules file and the two answers the broker's proxy
// port takes from it: whether a request may carry a secret, and the request with the
// grant's placeholder swapped for the secret's real value in the headers the rules name.
//
// The package holds no HTTP and no database code, because a Lean model of exactly these
// functions is proved and a differential fuzz test drives the Go functions against the
// model. The refusal strings, the order they are reported in, and the string handling are
// therefore the contract with that model, not style: a change here is a change there. Each
// string rule below is narrower than the standard library's nearest function, and says why.
package airlock

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Allow is one method and path prefix a secret may travel on.
type Allow struct {
	Method     string `json:"method"`
	PathPrefix string `json:"pathPrefix"`
}

// SecretRule is one secret's entry: where its value may go and in which headers.
type SecretRule struct {
	Name    string   `json:"name"`
	Hosts   []string `json:"hosts"`
	Headers []string `json:"headers"` // nil: the default ["authorization"]; empty: no header
	Allow   []Allow  `json:"allow"`   // nil: any method and path; empty: nothing allowed
	Kind    string   `json:"kind"`    // "", "aws" or "azure"
	Judge   *uint32  `json:"judge"`   // the judge's longest lifetime, in seconds
}

// Rules is the whole airlock.json file.
type Rules struct {
	Secrets []SecretRule `json:"secrets"`
}

// Grant names the secret a connection carries and the placeholder the command holds for it.
type Grant struct {
	Secret      string `json:"secret"`
	Placeholder string `json:"placeholder"`
}

// Request is one request on the proxy port, as much of it as the decision and the swap read.
type Request struct {
	ConnectHost string      `json:"connectHost"`
	SNI         string      `json:"sni"`
	HostHeader  string      `json:"hostHeader"`
	Method      string      `json:"method"`
	Path        string      `json:"path"`
	Headers     [][2]string `json:"headers"`
	Query       string      `json:"query"`
	Body        string      `json:"body"`
}

// maxJudgeSeconds is the longest lifetime a judge entry may ask for.
const maxJudgeSeconds = 900

// Validate reports why the file is not valid, naming the entry by its secret name and its
// index. It reads the entries in order and reports the first failure: a repeated name, an
// entry with no host, a judge above 900 seconds, or a kind other than "", "aws" or "azure".
// Whether it answers nil is the same answer as the Lean model's validate, so a new refusal
// here is a new refusal there.
func Validate(rules Rules) error {
	seen := make(map[string]bool, len(rules.Secrets))
	for i, s := range rules.Secrets {
		switch {
		case seen[s.Name]:
			return fmt.Errorf("%s: the name is repeated", entry(i, s))
		case len(s.Hosts) == 0:
			return fmt.Errorf("%s: no host, so the airlock binds the secret nowhere", entry(i, s))
		case s.Judge != nil && *s.Judge > maxJudgeSeconds:
			return fmt.Errorf("%s: judge %d is above %d seconds", entry(i, s), *s.Judge, maxJudgeSeconds)
		case s.Kind != "" && s.Kind != "aws" && s.Kind != "azure":
			return fmt.Errorf("%s: kind %q is not %q, %q or %q", entry(i, s), s.Kind, "", "aws", "azure")
		}
		seen[s.Name] = true
	}
	return nil
}

// entry names one entry of the file as a Validate error opens with.
func entry(i int, s SecretRule) string {
	return fmt.Sprintf("secrets[%d] %q", i, s.Name)
}

// Lookup finds a secret's entry, or nil. The entry points into r, so a caller that keeps it
// keeps the rules it came from.
func (r Rules) Lookup(name string) *SecretRule {
	for i := range r.Secrets {
		if r.Secrets[i].Name == name {
			return &r.Secrets[i]
		}
	}
	return nil
}

// Digest is the fingerprint of one secret's entry, "" when the file has none for it. It is the
// SHA-256, in hex, of the entry's JSON as encoding/json marshals a SecretRule: no field's tag
// carries omitempty, so every field of the entry is in the fingerprint, in the struct's own
// field order. The broker folds it into its policy version, so a change to one secret's entry
// ends the grants that entry affects and no others.
//
// Digest is no part of the Lean model: it fingerprints a rules file the model says nothing
// about, and no decision of Decide or Rewrite reads it.
func (r Rules) Digest(name string) string {
	s := r.Lookup(name)
	if s == nil {
		return ""
	}
	// Marshal cannot fail for a SecretRule: every field is a string, a slice of strings or of
	// Allow, or a *uint32, and encoding/json refuses none of those.
	encoded, _ := json.Marshal(s)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// HostBound reports whether host matches one of hosts: the same name, or a "*.suffix"
// pattern, which matches one or more labels before the suffix ("*.example.com" matches
// "a.example.com" and "a.b.example.com", not "example.com"). Both sides are compared after
// asciiLower, never strings.EqualFold.
func HostBound(hosts []string, host string) bool {
	bound := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		bound[asciiLower(h)] = true
	}
	h := asciiLower(host)
	if bound[h] {
		return true
	}
	for {
		i := strings.IndexByte(h, '.')
		if i < 0 {
			return false
		}
		h = h[i+1:]
		if bound["*."+h] {
			return true
		}
	}
}

// allowOK reports whether allow covers the request. A nil list allows any method and path;
// an empty list allows nothing.
func allowOK(allow []Allow, r *Request) bool {
	if allow == nil {
		return true
	}
	for _, a := range allow {
		if a.Method == r.Method && strings.HasPrefix(r.Path, a.PathPrefix) {
			return true
		}
	}
	return false
}

// check is the one decision both Decide and Rewrite take: the entry the request may carry,
// or the reason it may not. The reasons, and the order they are reported in, are the Lean
// model's.
func check(rules Rules, g Grant, value string, r *Request) (*SecretRule, string) {
	if g.Placeholder == "" {
		return nil, "empty placeholder"
	}
	sr := rules.Lookup(g.Secret)
	switch {
	case sr == nil:
		return nil, "unknown secret"
	case len(sr.Hosts) == 0:
		return nil, "secret not behind airlock"
	case !HostBound(sr.Hosts, r.ConnectHost):
		return nil, "host not bound"
	case r.SNI != r.ConnectHost:
		return nil, "sni mismatch"
	case r.HostHeader != r.ConnectHost:
		return nil, "host header mismatch"
	case !allowOK(sr.Allow, r):
		return nil, "not allowed by rule"
	}
	// A request that already carries the real value learnt it somewhere other than this
	// swap, so the airlock refuses it rather than forwarding it. The scan reads every
	// header, not only the ones the entry names, and an entry with a kind is not exempt.
	for _, h := range r.Headers {
		if Carries(value, h[1]) {
			return nil, "request already carries the value"
		}
	}
	return sr, ""
}

// Decide reports whether the request may carry the secret, and the reason it may not.
func Decide(rules Rules, g Grant, value string, r *Request) (bool, string) {
	_, why := check(rules, g, value, r)
	return why == "", why
}

// Rewrite returns the request with the placeholder swapped for value in the headers the
// entry names, or nil when Decide refuses. It changes no header name, no header order, not
// the query and not the body.
func Rewrite(rules Rules, g Grant, value string, r *Request) *Request {
	sr, _ := check(rules, g, value, r)
	if sr == nil {
		return nil
	}
	names := sr.Headers
	if names == nil {
		names = []string{"authorization"}
	}
	out := *r
	out.Headers = make([][2]string, len(r.Headers))
	for i, h := range r.Headers {
		out.Headers[i] = h
		for _, n := range names {
			if asciiLower(h[0]) == asciiLower(n) {
				out.Headers[i][1] = substitute(g.Placeholder, value, h[1])
				break
			}
		}
	}
	return &out
}

// mentions reports whether a header value carries value, directly or inside a Basic
// credential it decodes to.
func mentions(value, v string) bool {
	if strings.Contains(v, value) {
		return true
	}
	payload, ok := basicPayload(v)
	return ok && strings.Contains(payload, value)
}

// Carries reports whether a request header already carries the real value. The empty
// value carries nowhere, because every string contains it.
//
// It is exported for the proxy port (internal/broker/airlockproxy), which reads it to tell
// which of a grant's names one request is for: the name whose placeholder a header already
// holds. It is the same function the Lean model calls carries, unchanged by being exported,
// so the contract with that model is the contract of check and Rewrite as before.
func Carries(value, v string) bool {
	return value != "" && mentions(value, v)
}

// substitute swaps placeholder p for value in one header value, including inside a Basic
// credential (as the user or as the password), which it re-encodes. A value whose Basic
// payload does not carry the placeholder, and a value that is no Basic credential, take the
// plain replacement of every occurrence.
func substitute(p, value, v string) string {
	if payload, ok := basicPayload(v); ok && strings.Contains(payload, p) {
		return v[:6] + base64.StdEncoding.EncodeToString([]byte(strings.ReplaceAll(payload, p, value)))
	}
	return strings.ReplaceAll(v, p, value)
}

// basicPayload decodes the "user:pass" behind a header value of the form "Basic <base64>",
// the scheme compared with asciiLower. Three narrow choices, each a place where the handy
// standard-library call would part from the model:
//   - the credential is trimmed of spaces and horizontal tabs alone (HTTP's optional
//     whitespace), not of every Unicode space that strings.TrimSpace takes;
//   - base64.StdEncoding: the standard alphabet, padding required;
//   - a payload counts only when it is valid UTF-8, because the model's strings are.
func basicPayload(v string) (string, bool) {
	if len(v) <= 6 || asciiLower(v[:6]) != "basic " {
		return "", false
	}
	dec, err := base64.StdEncoding.DecodeString(strings.Trim(v[6:], " \t"))
	if err != nil {
		return "", false
	}
	payload := string(dec)
	if !utf8.ValidString(payload) {
		return "", false
	}
	return payload, true
}

// asciiLower lower-cases A-Z alone. Host names and header names are ASCII tokens, and the
// Unicode folding of strings.ToLower and strings.EqualFold would fold U+212A KELVIN SIGN
// onto "k", so "githu\u212a.com" would reach the rule for "githuk.com"; the model folds no
// such thing.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}
