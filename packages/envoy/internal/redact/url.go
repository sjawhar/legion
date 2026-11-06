// Package redact is how a URL a person configured is printed: in an error, a log line or a
// message, with nothing in it that can carry a credential.
//
// It works by allow-list. A value prints only when it is provably a plain http(s) URL with no
// place a credential can sit; any other value prints as NotPrinted, whatever it holds. No part of
// a value that fails the check is ever printed, so no encoding, separator or control byte in it
// can make the redactor print a piece it should have hidden.
package redact

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/oauth2"
)

// NotPrinted stands in for a value URL will not print.
const NotPrinted = "(not printed: the value carries a credential or is not a plain http(s) URL)"

// URL is raw when raw is provably clean, the empty string as it is, and NotPrinted for anything
// else. raw is clean when all of these hold:
//   - every byte is printable ASCII other than '@', '%', '?', '#' and '\' (so no user info, no
//     escape, no query, no fragment, no whitespace and no control byte);
//   - net/url parses it, with an http or https scheme (any case), no user, no opaque part;
//   - it reads exactly as scheme "://" host path, with nothing net/url dropped or decoded;
//   - the host is a DNS name, an IPv4 address or a bracketed IPv6 address with no zone, with an
//     optional port of one to five digits;
//   - the path is empty or only letters, digits, '.', '_', '~', '/' and '-'.
//
// A URL with user info (https://user:password@host) prints as NotPrinted too, rather than as
// https://xxxxx@host: a value whose password holds an '@' and that names no host at all
// (https://user:p@ssword) would print the password's tail as the host, and no check on the text
// can tell the two apart.
func URL(raw string) string {
	if raw == "" || clean(raw) {
		return raw
	}
	return NotPrinted
}

// Clean reports whether raw is not empty and URL prints it unchanged.
func Clean(raw string) bool { return raw != "" && clean(raw) }

// clean reports whether raw passes URL's allow-list.
func clean(raw string) bool {
	for i := range len(raw) {
		if c := raw[i]; c <= ' ' || c >= 0x7f || strings.IndexByte(`@%?#\`, c) >= 0 {
			return false
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	if scheme := strings.ToLower(u.Scheme); scheme != "http" && scheme != "https" {
		return false
	}
	n := len(u.Scheme) + len("://")
	if len(raw) < n || !strings.EqualFold(raw[:n], u.Scheme+"://") || raw[n:] != u.Host+u.Path {
		return false
	}
	return hostPort(u.Host) && plainPath(u.Path)
}

// hostPort reports whether hostport is a DNS name, an IPv4 address or a bracketed IPv6 address
// without a zone, with an optional port of one to five digits.
func hostPort(hostport string) bool {
	host, port, hasPort := hostport, "", false
	if strings.HasPrefix(hostport, "[") {
		end := strings.IndexByte(hostport, ']')
		if end < 0 {
			return false
		}
		host = hostport[1:end]
		if rest := hostport[end+1:]; rest != "" {
			if rest[0] != ':' {
				return false
			}
			port, hasPort = rest[1:], true
		}
		addr, err := netip.ParseAddr(host)
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return false
		}
	} else {
		if i := strings.IndexByte(hostport, ':'); i >= 0 {
			host, port, hasPort = hostport[:i], hostport[i+1:], true
		}
		if addr, err := netip.ParseAddr(host); err == nil {
			if !addr.Is4() {
				return false
			}
		} else if !dnsName(host) {
			return false
		}
	}
	if hasPort && (port == "" || len(port) > 5 || strings.Trim(port, "0123456789") != "") {
		return false
	}
	return true
}

// dnsName reports whether name is a DNS name: dot-separated labels of 1 to 63 letters, digits
// and '-', none starting or ending with '-', 253 bytes at most.
func dnsName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for label := range strings.SplitSeq(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// plainPath reports whether path holds only letters, digits, '.', '_', '~', '/' and '-'.
func plainPath(path string) bool {
	for i := range len(path) {
		c := path[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("._~/-", c) >= 0) {
			return false
		}
	}
	return true
}

// Call is callURL, a request URL a program built as base followed by a broker route, as a message
// may print it: unchanged when base is clean and callURL is base (its scheme in any case, since
// net/url lowers it) followed by nothing or a path under /v1/ of the characters plainPath allows,
// and NotPrinted otherwise - a URL on another host or another path, such as a redirect's target,
// included. Every route the broker serves is under /v1/.
func Call(base, callURL string) string {
	if callClean(base, callURL) {
		return callURL
	}
	return NotPrinted
}

// callClean reports whether Call prints callURL unchanged.
func callClean(base, callURL string) bool {
	if !clean(base) || len(callURL) < len(base) {
		return false
	}
	n := strings.Index(base, "://") + len("://")
	if !strings.EqualFold(callURL[:n], base[:n]) || callURL[n:len(base)] != base[n:] {
		return false
	}
	rest := callURL[len(base):]
	route := rest
	if strings.HasSuffix(base, "/") {
		route = "/" + rest
	}
	return (rest == "" || strings.HasPrefix(route, "/v1/")) && plainPath(rest)
}

// Error is err as a message may print it, given raw, the configured URL err's request was built
// from. It is err itself, so every message reads as before, only when printable proves its text
// holds nothing from a URL it may not print. Otherwise no text of err is printed: the error's
// text names only the class of the failure (a URL that did not parse, failed name resolution, a
// refused connection, a timeout, a TLS failure, an HTTP status, or a request that failed
// otherwise), found on err itself, and the *url.Error's operation with its URL through Call. It
// unwraps to err, so errors.Is and errors.As still classify the failure.
func Error(err error, raw string) error {
	if err == nil {
		return nil
	}
	ue, printable := printable(err, raw)
	if printable {
		return err
	}
	text := class(err)
	if ue != nil {
		text = ue.Op + " " + strconv.Quote(Call(raw, ue.URL)) + ": " + text
	}
	return &classified{text, err}
}

// Cause is Error without the *url.Error's own wrapper, for a message that names the call itself:
// the error the *url.Error wraps when Error would return err unchanged, and the class alone,
// unwrapping to err, when it would not.
func Cause(err error, raw string) error {
	if err == nil {
		return nil
	}
	ue, printable := printable(err, raw)
	switch {
	case !printable:
		return &classified{class(err), err}
	case ue != nil:
		return ue.Err
	default:
		return err
	}
}

// printable reports whether err's text may print, given raw (Error), with the *url.Error it
// holds, if any. All of these must hold:
//   - raw is clean;
//   - the URL the *url.Error names is raw followed by a plain path (callClean): net/http names a
//     redirect's target there, which can sit on another host or path and carry a query;
//   - the error the *url.Error wraps (err itself, with none) is one whose text names no URL: a
//     transport failure class names (a dial or other net.OpError, name resolution, a refused
//     connection, a timeout, TLS), an HTTP status answer, or text the allow-list passes. A
//     malformed Location header, say, puts the whole target in its error's text.
func printable(err error, raw string) (*url.Error, bool) {
	var ue *url.Error
	if !errors.As(err, &ue) {
		ue = nil
	}
	if raw == "" || !clean(raw) {
		return ue, false
	}
	inner := err
	if ue != nil {
		if !callClean(raw, ue.URL) {
			return ue, false
		}
		inner = ue.Err
	}
	if inner == nil {
		return ue, true
	}
	// A second *url.Error inside names a URL of its own, unchecked.
	var nested *url.Error
	return ue, !errors.As(inner, &nested) && (transport(inner) || clean(inner.Error()))
}

// transport reports whether err is a failure class names from the transport, a peer that broke
// the exchange (net/http's fixed ErrSchemeMismatch for an http answer to https, EOF, a reset, a
// broken pipe: the set the client's peerAnswered reads), or an HTTP status answer, none of whose
// text quotes a URL.
func transport(err error) bool {
	var op *net.OpError
	var dns *net.DNSError
	var timeout interface{ Timeout() bool }
	var re *oauth2.RetrieveError
	return errors.As(err, &op) || errors.As(err, &dns) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() ||
		tlsFailure(err) || errors.As(err, &re) ||
		errors.Is(err, http.ErrSchemeMismatch) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

// classified is an error whose text is its class alone.
type classified struct {
	text string
	err  error
}

func (c *classified) Error() string { return c.text }
func (c *classified) Unwrap() error { return c.err }

// class names the kind of failure err is, in fixed text.
func class(err error) string {
	var ue *url.Error
	var dns *net.DNSError
	var timeout interface{ Timeout() bool }
	var re *oauth2.RetrieveError
	switch {
	case errors.As(err, &ue) && ue.Op == "parse":
		return "not a valid URL"
	case errors.As(err, &dns):
		return "name resolution failed"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout():
		return "timed out"
	case tlsFailure(err):
		return "TLS failed"
	case errors.As(err, &re) && re.Response != nil:
		return fmt.Sprintf("the broker answered HTTP %d", re.Response.StatusCode)
	default:
		return "the request failed"
	}
}

// tlsFailure reports whether err is a TLS record, alert or certificate failure.
func tlsFailure(err error) bool {
	var record tls.RecordHeaderError
	var alert tls.AlertError
	var verify *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	return errors.As(err, &record) || errors.As(err, &alert) || errors.As(err, &verify) ||
		errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &invalid)
}
