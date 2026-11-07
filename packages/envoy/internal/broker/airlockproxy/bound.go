// packages/envoy/internal/broker/airlockproxy/bound.go
// One request on a bound host: the airlock.Request it is read as, the one (name, slot) pair of
// the grant it is handled for, the real value behind that pair, the decision and the swap
// (airlock.Decide and airlock.Rewrite, which decide and swap everything here), what goes
// upstream, and the line the call logs.
package airlockproxy

import (
	"cmp"
	"context"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http/httpguts"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

// mintAgainBefore is how close to its expiry a minted value may come before the proxy reads the
// grant again, which mints it again. Five minutes, so that no call starts on a token about to
// end: a request the proxy carries may run for minutes - a long poll, a large upload - and a
// token that lapses in the middle of one fails the call rather than being retried.
const mintAgainBefore = 5 * time.Minute

// grantValues is one grant's real values on this pod, read once and then held until a name's
// own reread drops it (DropName), the policy version the read ran under stops being the live
// one, or a minted value comes within mintAgainBefore of its expiry.
type grantValues struct {
	// read is the read itself: two connections of one grant arriving together take it in turn,
	// and the second reads what the first answered rather than running the release checks
	// again.
	read sync.Mutex

	// mu guards the fields below, and is never held across a read of the broker: a reread
	// that drops a name (DropName) runs on the pod's own reread path and must not wait behind
	// one grant's release checks.
	mu sync.Mutex
	// values is nil until the read has succeeded. A read that failed leaves it nil, so a
	// refusal the broker may not repeat - a database that did not answer - is not held against
	// the next connection, while a refusal it will repeat costs one more read and the same
	// answer. It is replaced, never written to: a caller is reading the map it was handed.
	values map[string]requests.AirlockSecret
	// version is the policy version that read ran under, as the broker answered it with the
	// values (AirlockValues).
	version string
	// drops counts the drops this grant has seen, and keptAt the count values was read at.
	// They differ exactly while the held answer is older than the last reread of one of its
	// names, which is what makes the next request read again - a read that began before a
	// drop landed included, since such an answer is its own request's and no later one's.
	drops, keptAt uint64
}

// held is the grant's values when they may still be used, and the drop count the read that took
// them began at. They may not be used when no read has succeeded, when a name of the grant has
// been reread since, when the policy has moved on from the version the read ran under
// (version, against the live one), or when a minted value is
// within mintAgainBefore of its expiry: that one name is what the read is for, but the read is
// of the whole grant, since AirlockValues answers a grant and not a name.
//
// The policy version is what carries a change the broker took by its periodic reload - a retag
// a console or an infra-applier made, a raised tier, a disabled version - to the values a pod
// already holds: a name's reread drops that one name (DropName), and nothing else did. The
// values are read again, which runs the release checks again, and a grant those checks now
// refuse ends there.
//
// keep is Options.KeepValues: it holds the answer across all three, the reread, the moved
// version and the margin alike, which is the negative control of the checks they belong to and
// is refused on a production start.
func (g *grantValues) held(now time.Time, version string, keep bool) (map[string]requests.AirlockSecret, uint64, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.values == nil {
		return nil, g.drops, false
	}
	if keep {
		return g.values, g.drops, true
	}
	if g.keptAt != g.drops || g.version != version {
		return nil, g.drops, false
	}
	for _, secret := range g.values {
		if !secret.Expires.IsZero() && secret.Expires.Sub(now) < mintAgainBefore {
			return nil, g.drops, false
		}
	}
	return g.values, g.drops, true
}

// keep holds what one read answered, and the policy version it ran under, unless a name was
// dropped while that read ran: such an answer is the request that asked for it and no later
// one's.
func (g *grantValues) keep(values map[string]requests.AirlockSecret, version string, drops uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.drops != drops {
		return
	}
	g.values, g.version, g.keptAt = values, version, drops
}

// drop takes name out of this grant's values and leaves every other name of it in place.
func (g *grantValues) drop(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, held := g.values[name]
	if !held && g.values != nil && g.keptAt == g.drops {
		// A whole answer this name was never part of, with no read behind it: this reread is
		// no business of this grant's, and ending the answer would cost it a release check
		// for nothing.
		return
	}
	if held {
		// Copied rather than written to, since a request is reading the map it was handed.
		values := maps.Clone(g.values)
		delete(values, name)
		g.values = values
	}
	// Counted even where nothing was deleted: a read may be running that is about to hold
	// this name's old value, and the count is what makes such an answer its own request's.
	g.drops++
}

// valuesFor is the grant's real values: AirlockValues once per grant per pod, as its own
// contract asks, and the answer kept for every later connection and request of that grant until
// something ends it - the reread of one of its names, a policy version that has moved since the
// read, or a minted value coming within mintAgainBefore of its expiry.
//
// The map holds one entry per grant this pod has served. A grant's values are a handful of
// strings and a pod is rolled by every deployment and by every change to the rules file, so
// nothing evicts them; what ends a grant's reach is the connection check, which refuses a
// placeholder whose grant has ended whatever this map still holds.
func (s *Server) valuesFor(ctx context.Context, grantID string) (map[string]requests.AirlockSecret, error) {
	s.valuesMu.Lock()
	held, ok := s.values[grantID]
	if !ok {
		held = &grantValues{}
		s.values[grantID] = held
	}
	s.valuesMu.Unlock()

	// The live policy version, read once for both of the checks below: the broker holds one
	// snapshot, so this is a read of a pointer and costs no query.
	version := s.grants.PolicyVersion()
	// The common path, checked before the single-flight lock: the grant's values are held and
	// still usable, so every parallel request of one grant answers from them at once. Taking
	// read first would put them all in a queue behind a read that is not going to happen.
	if values, _, fresh := held.held(s.now(), version, s.keepValues); fresh {
		return values, nil
	}

	held.read.Lock()
	defer held.read.Unlock()
	values, drops, fresh := held.held(s.now(), version, s.keepValues)
	if fresh {
		return values, nil
	}
	values, readVersion, err := s.grants.AirlockValues(ctx, grantID)
	if err != nil {
		return nil, err
	}
	if values == nil {
		// A grant that released no airlock name at all: held as an answer, so the release
		// checks run once for it too.
		values = map[string]requests.AirlockSecret{}
	}
	// The version the read itself ran under, not the one read above: a reload between the two
	// would otherwise mark this answer as made under a policy it was not.
	held.keep(values, readVersion, drops)
	return values, nil
}

// DropName drops name's value from every grant this pod holds values for, leaving the rest of
// each grant's names in place: a later request of such a grant reads its values again, and mints
// again what the new version of name needs minting from. The broker calls it wherever one
// secret's name is reread, since the reread is the only signal a stored value has a new version.
//
// Options.KeepValues turns it off altogether, because the drop is what a reread does to a stored
// value: held's keep makes a stale answer usable, but a name this has taken out of the map is
// not there to be used, and the next request reads the grant again and finds the new value. That
// switch is the negative control of the check this behaviour belongs to, and a production start
// with it set is refused.
func (s *Server) DropName(name string) {
	if s.keepValues {
		return
	}
	s.valuesMu.Lock()
	defer s.valuesMu.Unlock()
	for _, held := range s.values {
		held.drop(name)
	}
	s.dropAccess(name)
}

// serveBound answers one request on a connection the proxy decrypted: the whole of what the
// airlock does to a request, in the order the refusals have to run in. The grant is read again
// for each of them, so that what a connection may carry is what the grant holds now and not
// what it held when the CONNECT was answered.
func (s *Server) serveBound(w http.ResponseWriter, r *http.Request) {
	info, ok := r.Context().Value(connKey{}).(connInfo)
	if !ok || info.target == "" {
		// The connection was not registered, which only a drain racing a hand-over leaves.
		// Nothing is forwarded: this request has no CONNECT target to check its Host against.
		http.Error(w, "airlock: this connection's CONNECT target was lost, so nothing can be decided about this request", http.StatusInternalServerError)
		return
	}
	// The grant behind this connection's password, read again for this request. A grant that
	// has ended, been revoked, had its enrollment's lease lapse or had this name withheld
	// since the CONNECT must stop at the next request on the connection, and the connection's
	// own idle limit is no bound on when that is: a command sending one request every few
	// minutes holds one connection open for as long as it likes. The read is the three
	// indexed reads AirlockConnection makes and nothing else, which is what its own contract
	// says it is for - the full release checks are AirlockValues's, once per grant per pod.
	connection, err := s.grants.AirlockConnection(r.Context(), info.password)
	if err != nil {
		// The one rule for a failed grant read (grantRefusal): the read's own answer about
		// this grant is 407 in the broker's words, which a command reads from its proxy, and
		// a read that did not happen is 503 and says nothing of why. The line is logged
		// against the grant the connection was opened for, which is the only grant this
		// request has a name for.
		status, why := s.grantRefusal("connection", err)
		s.refuse(w, info, r, pair{}, status, why)
		return
	}
	// Everything below reads the grant this request was decided for: the names it still
	// carries, the placeholders behind them, and the ids its line logs.
	info.grant = connection
	request := &airlock.Request{
		ConnectHost: info.host,
		// The port the CONNECT named, which is 443 for every connection the proxy decrypts
		// (serveConnect's candidates answer nothing on another port). The decision reads it
		// all the same, so a connection that ever reached here on another port carries no
		// value off it.
		ConnectPort: info.port,
		// The three names the decision compares are ASCII lower-cased, as the entry's hosts
		// are: host names are case-insensitive and the model compares them exactly, so a
		// client that wrote "API.example.com" in one of the three is not a mismatch. The fold
		// is ASCII-only for the reason airlock.ASCIILower gives.
		SNI:        airlock.ASCIILower(sniOf(r)),
		HostHeader: airlock.ASCIILower(hostHeaderOf(r.Host)),
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

	// The (name, slot) pairs this request may be handled for, from the grant as it is now: a
	// name the operator has withheld since the CONNECT is not among them, and its placeholder
	// stops at this request while the grant's other names go on.
	candidates := s.candidates(info.grant, info.host, info.port)
	if len(candidates) == 0 {
		// The host was bound for this connection and no name of the grant binds it now: every
		// name that did has been withheld or has ended since the CONNECT. Refused rather than
		// forwarded, since a connection to a bound host that carries no name has no business
		// on it.
		s.refuse(w, info, r, pair{}, http.StatusForbidden, "no name of this grant is bound to "+info.host)
		return
	}
	chosen := choosePair(candidates, info.grant, request)
	// The entry's kind is what the value behind this name is: "" a string a header carries,
	// which the swap below puts there; "aws" a key pair the call itself has to be signed with
	// (aws.go); "azure" a federated token that has to be exchanged at a token endpoint for an
	// access token (azure.go). Validate allows no other kind, so an entry this reads nothing
	// for is a header swap. It is the kind candidates already read for this name, carried on
	// the pair rather than looked up again.
	kind := chosen.kind
	if s.noKind {
		// Options.NoKind: the entry's kind is read as none, so an azure or aws entry takes
		// the plain header swap below. It is the negative control of the check those two
		// kinds belong to, and is refused on a production start.
		kind = ""
	}
	exchange := kind == airlock.KindAzure && isTokenRequest(r)
	if exchange {
		// A token request carries the grant's placeholder in its form rather than in a header,
		// so what it is handled for is the federated slot whatever choosePair read of its
		// headers. The exchange below checks the form itself.
		chosen.slot = requests.SlotAzureFederated
	}
	placeholder := info.grant.Names[chosen.name].Slots[chosen.slot]
	values, err := s.valuesFor(r.Context(), info.grant.GrantID)
	if err != nil {
		// The same rule as the connection read above (grantRefusal), so that one answer does
		// not say the grant has ended where the other says the broker could not read it: the
		// full release checks ending a grant is 407, and a read of them that did not happen
		// is 503 and carries none of its own words, which here would be the store's.
		status, why := s.grantRefusal("values", err)
		s.refuse(w, info, r, chosen, status, why)
		return
	}
	secret, ok := values[chosen.name]
	if !ok {
		s.refuse(w, info, r, chosen, http.StatusForbidden, "the broker released no value for "+chosen.name)
		return
	}
	value, held := secret.Slots[chosen.slot]
	switch {
	case held:
	case isAzureAccessSlot(chosen.slot):
		// An access token of an exchange some pod made: the broker holds no value for such a
		// slot, and this pod either holds the token already or exchanges for itself to get it.
		value, err = s.azureAccess(r.Context(), info, chosen, secret)
		if err != nil {
			s.refuse(w, info, r, chosen, http.StatusForbidden, err.Error())
			return
		}
	default:
		// The grant holds a placeholder for a slot the broker's read of this name no longer
		// fills: a github-app whose installation on that owner has gone since the release.
		// Refused rather than forwarded with the placeholder in it, which would read as the
		// service's own refusal.
		s.refuse(w, info, r, chosen, http.StatusForbidden, "the broker released no "+chosen.slot+" value for "+chosen.name)
		return
	}

	grant := airlock.Grant{Secret: chosen.name, Placeholder: placeholder}
	// refused answers a request the decision will not carry, and reports that it did. The
	// reason and the status are airlock.Decide's own, as they were when every path through
	// this function asked it first.
	refused := func() bool {
		allowed, why := airlock.Decide(s.rules, grant, value, request)
		if !allowed {
			s.refuse(w, info, r, chosen, http.StatusForbidden, why)
		}
		return !allowed
	}
	if exchange {
		if refused() {
			return
		}
		// The one request the proxy answers itself: the broker makes the exchange with the
		// real federated token and hands the command a placeholder access token.
		s.exchangeToken(w, info, r, chosen, value)
		return
	}

	// What the request leaves with: its headers and its query. A header swap changes the
	// headers the entry names and nothing else; the AWS path replaces the signature, which is
	// the Authorization header, the date, the session token and the canonical query.
	header, rawQuery := http.Header(nil), r.URL.RawQuery
	if kind == airlock.KindAWS {
		if refused() {
			return
		}
		signed, signedQuery, release, why := s.signAWS(r.Context(), r, hostHeaderOf(info.target), info.grant.Names[chosen.name].Slots, secret.Slots)
		// The body signAWS read to hash it is what this request serves upstream, so the
		// budget it is held against is given back when the round trip below has ended and
		// not when the signature was made.
		defer release()
		if why != "" {
			s.refuse(w, info, r, chosen, http.StatusForbidden, why)
			return
		}
		header, rawQuery = signed, signedQuery
	} else {
		// The swap first, and the decision only to explain a refusal. Decide and Rewrite take
		// the one decision (airlock.check), which is deterministic and has no side effect, so
		// Rewrite answers nil for exactly the requests Decide refuses: running it first costs
		// an allowed swap - the commonest request the proxy serves - one decision rather than
		// two, and leaves the refusal, the order its reasons are reported in and the 403
		// exactly as they were.
		rewritten := airlock.Rewrite(s.rules, grant, value, request)
		if rewritten == nil {
			if refused() {
				return
			}
			// Decide allowed what Rewrite refused, which the one decision behind both makes
			// unreachable; refusing rather than forwarding is what keeps it unreachable and
			// harmless if the two ever part.
			s.refuse(w, info, r, chosen, http.StatusForbidden, "the swap refused a request the decision allowed")
			return
		}
		header = headerOf(rewritten.Headers)
	}
	// Headers of the proxy itself, which belong to this connection and not to the service. The
	// swap and the signature have already run, so a header the entry names keeps its value and
	// the signature covers neither of these.
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
				RawQuery: rawQuery,
			}
			pr.Out.Host = hostHeaderOf(info.target)
			pr.Out.Header = header
		},
		// No ModifyResponse. The answer is streamed back as it came: nothing of it is read,
		// buffered, decompressed or changed, because this design changes no answer from an API
		// service - what it guards is what leaves, not what comes back.
		ErrorHandler: s.badGateway(info.host, r.URL.Path),
	}
	if decrypted, ok := r.Context().Value(decryptedKey{}).(net.Conn); ok && upgrading(r) {
		// An upgrade is the one request whose connection outlives it. ReverseProxy joins the
		// two connections inside the call below and copies bytes until one end closes, so no
		// request is read on this one again and the read at the top of this function never
		// runs for it. A watcher reads the grant for it instead, for as long as the join
		// lasts, which is as long as ServeHTTP has not returned.
		watching, stop := context.WithCancel(r.Context())
		defer stop()
		go s.watchJoined(watching, decrypted, info, r, chosen)
	}
	proxy.ServeHTTP(answer, r)
	s.log.Info(callMessage, s.callFields(info, r, chosen, answer.status, "")...)
}

// upgrading reports whether a request asks to leave HTTP behind: a Connection header naming the
// Upgrade token and an Upgrade header naming a protocol, which is how net/http and
// httputil.ReverseProxy themselves read one.
func upgrading(r *http.Request) bool {
	return httpguts.HeaderValuesContainsToken(r.Header["Connection"], "Upgrade") && r.Header.Get("Upgrade") != ""
}

// joinMessage is the message of the line the close of a joined connection logs. The call itself
// logs its own line when the join ends (callMessage), so this one is no second call line: it
// says why the join ended rather than how the call did.
const joinMessage = "airlock proxy: a joined connection its grant no longer carries"

// watchJoined closes a joined connection once the grant behind it stops carrying it, and it is
// the whole of what holds such a connection to its grant: after the 101 the two connections are
// one, no request is read on them again, and the idle limit bounds nothing on a socket a service
// pings. So the read every other request makes for itself runs here instead, on watchInterval,
// until the caller's cancel ends it.
func (s *Server) watchJoined(ctx context.Context, decrypted net.Conn, info connInfo, r *http.Request, chosen pair) {
	ticker := time.NewTicker(s.watch)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		status, why := s.joinRefusal(ctx, info, chosen.name)
		if why == "" {
			continue
		}
		s.log.Info(joinMessage, s.callFields(info, r, chosen, status, why)...)
		// The close ends the copy in both directions, so the ServeHTTP this watcher was
		// started for returns and the cancel there stops this goroutine.
		decrypted.Close()
		return
	}
}

// joinRefusal is why the grant behind a joined connection no longer carries it, and "" while it
// does: the grant's own expiry, the connection read's refusals, and the name this upgrade was
// decided for going out of the grant, which is how a name an operator withholds reaches a socket
// that reads no request again. Any other error of that read leaves the connection alone, with
// the error logged where grantRefusal logs one: a read that failed says nothing about the grant,
// and a database failover must cut no WebSocket the airlock carries.
func (s *Server) joinRefusal(ctx context.Context, info connInfo, name string) (int, string) {
	if expires := info.grant.Expires; expires != nil && !s.now().Before(*expires) {
		return http.StatusProxyAuthRequired, "this grant expired while this connection was joined to the service"
	}
	grant, err := s.grants.AirlockConnection(ctx, info.password)
	if err != nil {
		if status, why := s.grantRefusal("connection", err); status == http.StatusProxyAuthRequired {
			return status, why
		}
		return 0, ""
	}
	if _, carried := grant.Names[name]; !carried {
		return http.StatusForbidden, name + " is no longer a name of this grant"
	}
	return 0, ""
}

// callMessage is the message of the one line a call to a bound host logs. Every such call logs
// exactly one, allowed or refused.
const callMessage = "airlock proxy: a call to a bound host"

// refuse answers a request the airlock will not carry and logs the same line a carried one logs.
// Nothing reaches the service: the refusal is written here, on the connection the proxy opened.
func (s *Server) refuse(w http.ResponseWriter, info connInfo, r *http.Request, chosen pair, status int, reason string) {
	s.log.Info(callMessage, s.callFields(info, r, chosen, status, reason)...)
	http.Error(w, "airlock: "+reason, status)
}

// callFields is what one call to a bound host logs: who made it, which secret it was handled
// for, where it went, and how it ended. Never the placeholder and never the value - the airlock
// exists so that neither is ever written down, and a log line is the first place one would
// leak. The reason is there for a refusal alone, so a line reads as allowed or as refused and
// why.
func (s *Server) callFields(info connInfo, r *http.Request, chosen pair, status int, reason string) []any {
	fields := []any{
		"enrollment", info.grant.EnrollmentID,
		"grant", info.grant.GrantID,
		"secret", chosen.name,
		"issue", info.grant.Issue,
		"host", info.host,
		"method", r.Method,
		// The path alone: a query can carry anything a command put in it.
		"path", r.URL.Path,
		"status", status,
		"allowed", reason == "",
	}
	if chosen.slot != "" && chosen.slot != requests.SlotValue {
		// Which of the name's values the call used, where the name has more than one: a
		// github-app secret holds one token per installation, and an operator reading this
		// line has to know which account's token went to the service, not only which secret
		// it came from. A name whose value is the one "value" slot says nothing by naming it.
		fields = append(fields, "slot", chosen.slot)
	}
	if reason != "" {
		fields = append(fields, "reason", reason)
	}
	return fields
}

// pair is one (name, slot) of a grant: which secret a request may be handled for, and which of
// that secret's values. A github-app name has one slot per installation and no value slot, so
// the pair rather than the name is what says which token a request's placeholder stands for.
// kind is the entry's own kind, read when the pair was made, so that the request this pair is
// chosen for needs no second Lookup of the name.
type pair struct{ name, slot, kind string }

// candidates is every (name, slot) pair of the grant whose name's entry binds the CONNECT host
// on the port the CONNECT named, sorted by name and then by slot. Sorted because the choice
// below takes the first of them, and a map's order would make which value a request is handled
// for depend on the run.
//
// The port is read here because the entry's hosts carry no port, so an entry binds a name on
// 443 and nowhere else, and another port of that name is another service, which no name of
// this grant is a candidate for. An empty answer is therefore also what says a CONNECT is a
// tunnel rather than a connection the proxy reads (serveConnect): one function decides what is
// bound, and nothing beside it repeats that walk.
//
// It is a pure function of the grant, the host, the port and the rules, and it runs once per
// request on a bound connection, over the grant that request read for itself: the grant is what
// changes between two requests of one connection - a name withheld, a name ended - and a list
// built once at the CONNECT would answer for the grant as it was then.
func (s *Server) candidates(grant requests.AirlockConnection, host string, port uint16) []pair {
	if port != 443 {
		return nil
	}
	var pairs []pair
	for name, held := range grant.Names {
		entry := s.rules.Lookup(name)
		if entry == nil || !airlock.HostBound(entry.Hosts, host) {
			continue
		}
		for slot := range held.Slots {
			pairs = append(pairs, pair{name: name, slot: slot, kind: entry.Kind})
		}
	}
	slices.SortFunc(pairs, func(a, b pair) int {
		return cmp.Or(strings.Compare(a.name, b.name), strings.Compare(a.slot, b.slot))
	})
	return pairs
}

// choosePair is the one (name, slot) pair a request is handled for: the first candidate a header
// of the request already carries the placeholder of, and otherwise the first candidate.
//
// Why one pair at all. The decision and the swap are over one grant and one secret
// (airlock.Grant), and the model that is proved is over that pair, so a request is decided for
// one secret or it is decided by something other than the model. Two secrets of one grant can
// be bound to one host - two GitHub tokens of different reach both bind api.github.com - and so
// can two slots of one secret, since a github-app name holds one token per installation, so
// which one a request is for cannot come from the host. What the request itself says is which
// placeholder it put in a header, and that is what is read here (airlock.Carries, the same
// function the model calls carries).
//
// A request that carries no placeholder of any candidate is still decided, for the first
// candidate: the entry's hosts, method and path rules apply to it, and it is forwarded with
// nothing swapped. That is what makes an entry's allow list a bound on the whole connection
// rather than on the requests that happen to ask for a swap.
func choosePair(candidates []pair, grant requests.AirlockConnection, request *airlock.Request) pair {
	for _, candidate := range candidates {
		placeholder := grant.Names[candidate.name].Slots[candidate.slot]
		if placeholder == "" {
			continue
		}
		for _, header := range request.Headers {
			if airlock.Carries(placeholder, header[1]) {
				return candidate
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
