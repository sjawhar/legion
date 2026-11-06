// packages/envoy/internal/broker/airlockproxy/bound.go
// One request on a bound host: the airlock.Request it is read as, the one name of the grant it
// is handled for, the real values behind that name, the decision and the swap
// (airlock.Decide and airlock.Rewrite, which decide and swap everything here), what goes
// upstream, and the line the call logs.
package airlockproxy

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"sync"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

// grantValues is one grant's real values on this pod, read once. The mutex is the read itself:
// two connections of one grant arriving together take it in turn, and the second reads what the
// first answered rather than running the release checks again.
type grantValues struct {
	mu sync.Mutex
	// values is nil until the read has succeeded. A read that failed leaves it nil, so a
	// refusal the broker may not repeat - a database that did not answer - is not held against
	// the next connection, while a refusal it will repeat costs one more read and the same
	// answer.
	values map[string]string
}

// valuesFor is the grant's real values: AirlockValues once per grant per pod, as its own
// contract asks, and the answer kept for every later connection and request of that grant.
//
// The map holds one entry per grant this pod has served. A grant's values are a handful of
// strings and a pod is rolled by every deployment and by every change to the rules file, so
// nothing evicts them; what ends a grant's reach is the connection check, which refuses a
// placeholder whose grant has ended whatever this map still holds.
func (s *Server) valuesFor(ctx context.Context, grantID string) (map[string]string, error) {
	s.valuesMu.Lock()
	held, ok := s.values[grantID]
	if !ok {
		held = &grantValues{}
		s.values[grantID] = held
	}
	s.valuesMu.Unlock()

	held.mu.Lock()
	defer held.mu.Unlock()
	if held.values != nil {
		return held.values, nil
	}
	values, err := s.grants.AirlockValues(ctx, grantID)
	if err != nil {
		return nil, err
	}
	if values == nil {
		// A grant that released no airlock name at all: held as an answer, so the release
		// checks run once for it too.
		values = map[string]string{}
	}
	held.values = values
	return values, nil
}

// serveBound answers one request on a connection the proxy decrypted: the whole of what the
// airlock does to a request, in the order the refusals have to run in.
func (s *Server) serveBound(w http.ResponseWriter, r *http.Request) {
	info, ok := r.Context().Value(connKey{}).(connInfo)
	if !ok || info.target == "" {
		// The connection was not registered, which only a drain racing a hand-over leaves.
		// Nothing is forwarded: this request has no CONNECT target to check its Host against.
		http.Error(w, "airlock: this connection's CONNECT target was lost, so nothing can be decided about this request", http.StatusInternalServerError)
		return
	}
	request := &airlock.Request{
		ConnectHost: info.host,
		// The three names the decision compares are ASCII lower-cased, as the entry's hosts
		// are: host names are case-insensitive and the model compares them exactly, so a
		// client that wrote "API.example.com" in one of the three is not a mismatch. The fold
		// is ASCII-only for the reason asciiLower gives.
		SNI:        asciiLower(sniOf(r)),
		HostHeader: asciiLower(hostHeaderOf(r.Host)),
		Method:     r.Method,
		Path:       r.URL.Path,
		Headers:    headersOf(r.Header),
		Query:      r.URL.RawQuery,
		// The body is never read and never changed. The airlock swaps headers alone, so
		// nothing here depends on the body; reading one would mean holding every upload a
		// command makes in the broker's memory before it could be forwarded, and the proxy
		// streams instead.
		Body: "",
	}

	candidates := s.candidates(info.grant, info.host)
	if len(candidates) == 0 {
		// The host was bound when the connection was opened and is not now, which one reload
		// of nothing can produce: the rules are read once at boot. Refused rather than
		// forwarded, since a connection to a bound host that carries no name has no business
		// on it.
		s.refuse(w, info, r, "", http.StatusForbidden, "no name of this grant is bound to "+info.host)
		return
	}
	name := chooseName(candidates, info.grant, request)
	held := info.grant.Names[name]
	placeholder := held.Slots[requests.SlotValue]
	if placeholder == "" {
		// The seam for the kinds whose released value is more than one string. A stored
		// secret and a google-login's access token fill the value slot alone, and the swap is
		// that one string. A name of kind "aws" is three slots and a signature rather than a
		// header; an "azure-login" is a federated token that has to be exchanged for an access
		// token; a "github-app" is one slot per installation owner, chosen by what the request
		// asks for. Each needs a step the proxy does not carry yet, so such a name is refused
		// here, naming its kind, rather than forwarded with nothing swapped - which would send
		// the command's placeholder to the service and read as the service's refusal.
		s.refuse(w, info, r, name, http.StatusForbidden, fmt.Sprintf("the airlock does not yet carry %s, a secret of kind %q: it has no value slot", name, held.Kind))
		return
	}
	values, err := s.valuesFor(r.Context(), info.grant.GrantID)
	if err != nil {
		// The full release checks refused this grant. ErrGrantNotLive is 407 as the connection
		// check's own refusals are - the grant is gone and the command's placeholder opens
		// nothing any more - and everything else is 403, a request this grant may not make.
		status := http.StatusForbidden
		if errors.Is(err, requests.ErrGrantNotLive) {
			status = http.StatusProxyAuthRequired
		}
		s.refuse(w, info, r, name, status, err.Error())
		return
	}
	value, ok := values[name]
	if !ok {
		s.refuse(w, info, r, name, http.StatusForbidden, "the broker released no value for "+name)
		return
	}

	grant := airlock.Grant{Secret: name, Placeholder: placeholder}
	if allowed, why := airlock.Decide(s.rules, grant, value, request); !allowed {
		s.refuse(w, info, r, name, http.StatusForbidden, why)
		return
	}
	rewritten := airlock.Rewrite(s.rules, grant, value, request)
	if rewritten == nil {
		// Decide and Rewrite take the one decision (airlock.check), so this is unreachable;
		// refusing rather than forwarding is what keeps it unreachable and harmless if the two
		// ever part.
		s.refuse(w, info, r, name, http.StatusForbidden, "the swap refused a request the decision allowed")
		return
	}

	header := headerOf(rewritten.Headers)
	// Hop-by-hop headers of the proxy itself, which belong to this connection and not to the
	// service. The swap has already run, so a header the entry names keeps its value whatever
	// these were.
	header.Del("Proxy-Authorization")
	header.Del("Proxy-Connection")
	answer := &statusWriter{ResponseWriter: w}
	proxy := &httputil.ReverseProxy{
		Transport: s.upstream,
		// -1: every write is flushed as it arrives, so a streamed answer - a long poll, a
		// server-sent event stream, a large download - reaches the command as the service
		// sends it rather than in buffers.
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = &url.URL{
				Scheme:   "https",
				Host:     info.target,
				Path:     r.URL.Path,
				RawPath:  r.URL.RawPath,
				RawQuery: r.URL.RawQuery,
			}
			pr.Out.Host = hostHeaderOf(info.target)
			pr.Out.Header = header
		},
		// No ModifyResponse. The answer is streamed back as it came: nothing of it is read,
		// buffered, decompressed or changed, because this design changes no answer from an API
		// service - what it guards is what leaves, not what comes back.
		ErrorHandler: s.badGateway(info.host, r.URL.Path),
	}
	proxy.ServeHTTP(answer, r)
	s.log.Info(callMessage, s.callFields(info, r, name, answer.status, "")...)
}

// callMessage is the message of the one line a call to a bound host logs. Every such call logs
// exactly one, allowed or refused.
const callMessage = "airlock proxy: a call to a bound host"

// refuse answers a request the airlock will not carry and logs the same line a carried one logs.
// Nothing reaches the service: the refusal is written here, on the connection the proxy opened.
func (s *Server) refuse(w http.ResponseWriter, info connInfo, r *http.Request, name string, status int, reason string) {
	s.log.Info(callMessage, s.callFields(info, r, name, status, reason)...)
	http.Error(w, "airlock: "+reason, status)
}

// callFields is what one call to a bound host logs: who made it, which secret it was handled
// for, where it went, and how it ended. Never the placeholder and never the value - the airlock
// exists so that neither is ever written down, and a log line is the first place one would
// leak. The reason is there for a refusal alone, so a line reads as allowed or as refused and
// why.
func (s *Server) callFields(info connInfo, r *http.Request, name string, status int, reason string) []any {
	fields := []any{
		"enrollment", info.grant.EnrollmentID,
		"grant", info.grant.GrantID,
		"secret", name,
		"issue", info.grant.Issue,
		"host", info.host,
		"method", r.Method,
		// The path alone: a query can carry anything a command put in it.
		"path", r.URL.Path,
		"status", status,
		"allowed", reason == "",
	}
	if reason != "" {
		fields = append(fields, "reason", reason)
	}
	return fields
}

// candidates is every name of the grant whose entry binds the CONNECT host, sorted. Sorted
// because the choice below takes the first of them, and a map's order would make which secret a
// request is handled for depend on the run.
func (s *Server) candidates(grant requests.AirlockConnection, host string) []string {
	var names []string
	for name := range grant.Names {
		if entry := s.rules.Lookup(name); entry != nil && airlock.HostBound(entry.Hosts, host) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// chooseName is the one name a request is handled for: the first candidate a header of the
// request already carries a placeholder of, and otherwise the first candidate.
//
// Why one name at all. The decision and the swap are over one grant and one secret
// (airlock.Grant), and the model that is proved is over that pair, so a request is decided for
// one secret or it is decided by something other than the model. Two secrets of one grant can
// be bound to one host - two GitHub tokens of different reach both bind api.github.com - so
// which one a request is for cannot come from the host. What the request itself says is which
// placeholder it put in a header, and that is what is read here (airlock.Carries, the same
// function the model calls carries).
//
// A request that carries no placeholder of any candidate is still decided, for the first
// candidate: the entry's hosts, method and path rules apply to it, and it is forwarded with
// nothing swapped. That is what makes an entry's allow list a bound on the whole connection
// rather than on the requests that happen to ask for a swap.
func chooseName(candidates []string, grant requests.AirlockConnection, request *airlock.Request) string {
	for _, name := range candidates {
		for _, placeholder := range grant.Names[name].Slots {
			if placeholder == "" {
				continue
			}
			for _, header := range request.Headers {
				if airlock.Carries(placeholder, header[1]) {
					return name
				}
			}
		}
	}
	return candidates[0]
}

// sniOf is the name the command asked for in its TLS handshake, "" for a client that sent none.
// net/http fills r.TLS for a connection it reads as a *tls.Conn, which is every connection the
// inner server serves.
func sniOf(r *http.Request) string {
	if r.TLS == nil {
		return ""
	}
	return r.TLS.ServerName
}

// headersOf is the request's headers as the decision reads them: every header, every value, in
// one stable order - by name, and within a name in the order they arrived. Stable because the
// swap answers the same list and the headers that go upstream are rebuilt from it, and because
// two runs of one request must be decided alike.
func headersOf(h http.Header) [][2]string {
	names := slices.Sorted(maps.Keys(h))
	headers := make([][2]string, 0, len(h))
	for _, name := range names {
		for _, value := range h[name] {
			headers = append(headers, [2]string{name, value})
		}
	}
	return headers
}

// headerOf turns the swap's answer back into headers for the request upstream. Rewrite changes
// no name and no order, so this is headersOf read backwards; the names are net/http's own
// canonical form, since that is what they came out of the request as.
func headerOf(headers [][2]string) http.Header {
	h := make(http.Header, len(headers))
	for _, header := range headers {
		h[header[0]] = append(h[header[0]], header[1])
	}
	return h
}

// statusWriter records the status the service answered, which the call's log line names. It
// unwraps, so the flush that streams each write (FlushInterval -1) and every other
// http.ResponseController call reach the real writer.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
