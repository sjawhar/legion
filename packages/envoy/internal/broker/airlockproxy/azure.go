// packages/envoy/internal/broker/airlockproxy/azure.go
// The azure kind: an azure-login secret, whose federated token is no credential a service takes
// but an assertion that has to be exchanged at Microsoft's token endpoint for an access token.
//
// Two paths. The exchange: a token request whose client_assertion is the grant's placeholder is
// not sent on as it came - the broker puts the real federated token in the form, calls the
// endpoint itself, keeps the access token it is given for that grant and that scope, and
// answers the command Microsoft's own answer with a placeholder where the access token was.
// The swap: a later request carrying that placeholder is an ordinary header swap of the access
// token behind it (airlock.Decide and airlock.Rewrite, like every other pair), and a pod that
// holds no token for the slot does the exchange for itself from what the slot carries, as each
// pod mints the other kinds for itself.
//
// The exchange is the one place the proxy writes an answer of its own. It still reads no answer
// of any API service, which is what the design's rule is about: what it reads here is the
// answer to a call the broker itself made to a token endpoint, and every answer to a call of
// the command's own is streamed back untouched, uncompressed and unread.
package airlockproxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

// The two bounds on what the broker reads of an exchange: a token request's form, which carries
// an assertion of a few kilobytes, and the endpoint's answer, which carries an access token of
// the same order. Neither is streamed, since the broker reads both whole.
const (
	maxTokenRequest = 1 << 20
	maxTokenAnswer  = 1 << 20
)

// azureExchange is one exchange at a token endpoint: where it is made, what it asks a token
// for, and which application it is made as. It is what an azure-access slot carries.
//
// An access slot - requests.SlotAzureAccessPrefix and what azureAccessSlot writes after it -
// names one access token the proxy exchanged a grant's federated token for. What follows the
// prefix is everything a broker pod needs to make that exchange for itself: this object as
// JSON, base64url with no padding.
//
//	{"endpoint":"https://login.microsoftonline.com/<tenant>/oauth2/v2.0/token",
//	 "scope":"https://management.azure.com/.default",
//	 "client_id":"<the application the federated token stands for>"}
//
// All three, because the pod that serves a request carrying this placeholder is not always the
// pod that minted it: a slot naming the scope alone would leave that pod with no endpoint to
// call - the endpoint is the tenant's own URL, which nothing else of the grant holds - one
// naming the endpoint alone with no scope to ask a token for, and one naming neither with no
// application to ask as. Nothing else of the exchange is in it: the grant type and the
// assertion type are the federated flow's own constants, and the assertion itself is the real
// federated token, which a pod reads from the broker and never from a slot.
//
// base64url with no padding because a slot travels as a database key, a map key and part of a
// log line: that alphabet has no "+", "/" or "=" to escape anywhere, and the same object always
// encodes to the same string - encoding/json writes a struct's fields in the struct's own order
// - so two pods that exchange one scope name one slot and share one placeholder.
type azureExchange struct {
	Endpoint string `json:"endpoint"`
	Scope    string `json:"scope"`
	ClientID string `json:"client_id"`
}

// azureAccessSlot is the slot the access token of x lives under.
func azureAccessSlot(x azureExchange) string {
	// Three strings, so the marshal cannot fail.
	encoded, _ := json.Marshal(x)
	return requests.SlotAzureAccessPrefix + base64.RawURLEncoding.EncodeToString(encoded)
}

// azureExchangeOf reads a slot back into the exchange it names, and answers false for a slot
// that is no access slot and for one whose text this encoding did not write.
func azureExchangeOf(slot string) (azureExchange, bool) {
	encoded, ok := strings.CutPrefix(slot, requests.SlotAzureAccessPrefix)
	if !ok {
		return azureExchange{}, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return azureExchange{}, false
	}
	var x azureExchange
	if err := json.Unmarshal(decoded, &x); err != nil || x.Endpoint == "" || x.Scope == "" {
		return azureExchange{}, false
	}
	return x, true
}

// isAzureAccessSlot reports whether slot is one of the proxy's own, which the broker's read of
// a grant answers no value for: the value behind it is an access token this pod exchanged or
// will exchange.
func isAzureAccessSlot(slot string) bool {
	return strings.HasPrefix(slot, requests.SlotAzureAccessPrefix)
}

// isTokenRequest reports whether r is a request to a token endpoint. Microsoft's are
// /{tenant}/oauth2/v2.0/token and /{tenant}/oauth2/token, and the measured clients - az login
// --federated-token, and the azure-identity library through MSAL - POST a form to one of them.
func isTokenRequest(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/oauth2/") && strings.HasSuffix(r.URL.Path, "/token")
}

// accessKey is one access token this pod holds: whose grant, which name of it, and which
// exchange (the slot). The grant is in the key because two grants of one secret hold two
// federated tokens and so two access tokens.
type accessKey struct{ grant, name, slot string }

// accessHeld is one such token and the exchange that fills it: two requests of one grant
// arriving together take the exchange in turn, and the second reads what the first was given
// rather than making a second call to the endpoint.
type accessHeld struct {
	exchange sync.Mutex

	mu      sync.Mutex
	value   string
	expires time.Time // zero when the endpoint named no lifetime, which no clock then ends
}

// held is the token when it may still be used: within mintAgainBefore of its expiry it may not,
// for the reason a minted value may not - a call the proxy carries can run for minutes, and a
// token that lapses in the middle of one fails the call rather than being retried.
func (h *accessHeld) held(now time.Time) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.value == "" {
		return "", false
	}
	if !h.expires.IsZero() && h.expires.Sub(now) < mintAgainBefore {
		return "", false
	}
	return h.value, true
}

func (h *accessHeld) keep(value string, expires time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.value, h.expires = value, expires
}

// accessFor is the entry this pod holds one grant's access token of a slot in, made on the
// first request that asks for it.
func (s *Server) accessFor(key accessKey) *accessHeld {
	s.accessMu.Lock()
	defer s.accessMu.Unlock()
	held, ok := s.access[key]
	if !ok {
		held = &accessHeld{}
		s.access[key] = held
	}
	return held
}

// dropAccess forgets every access token exchanged for name, on every grant. The federated token
// behind them has a new version, so the tokens it bought are no longer the ones a request of
// that name must carry.
func (s *Server) dropAccess(name string) {
	s.accessMu.Lock()
	defer s.accessMu.Unlock()
	for key := range s.access {
		if key.name == name {
			delete(s.access, key)
		}
	}
}

// exchangeToken answers one token request of an azure name: the broker makes the exchange
// itself with the real federated token, keeps the access token, and answers the command the
// endpoint's own answer with a placeholder in place of that token. Nothing of this request
// reaches the endpoint as the command wrote it.
func (s *Server) exchangeToken(w http.ResponseWriter, info connInfo, r *http.Request, chosen pair, federated string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxTokenRequest))
	if err != nil {
		s.refuse(w, info, r, chosen, http.StatusBadRequest, "reading this token request's form: "+err.Error())
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		s.refuse(w, info, r, chosen, http.StatusBadRequest, "this token request's body is no application/x-www-form-urlencoded form")
		return
	}
	placeholder := info.grant.Names[chosen.name].Slots[requests.SlotAzureFederated]
	if placeholder == "" || form.Get("client_assertion") != placeholder {
		// Refused rather than forwarded: an assertion that is not this grant's placeholder is
		// a credential the command holds of its own, and the airlock carries no such thing.
		s.refuse(w, info, r, chosen, http.StatusForbidden, "refusing a token request whose client_assertion is not this grant's placeholder")
		return
	}
	x := azureExchange{
		// The endpoint as this connection reached it: the CONNECT target's host, which the
		// decision has already tied to the entry's hosts, the TLS name and the Host header.
		Endpoint: "https://" + hostHeaderOf(info.target) + r.URL.Path,
		Scope:    form.Get("scope"),
		ClientID: form.Get("client_id"),
	}
	form.Set("client_assertion", federated)
	status, answer, err := s.exchangeAt(r.Context(), x.Endpoint, form)
	if err != nil {
		s.refuse(w, info, r, chosen, http.StatusBadGateway, "the exchange of this grant's federated token could not be made at "+x.Endpoint+": "+err.Error())
		return
	}
	if status != http.StatusOK {
		// The endpoint's own status, with the broker's own words: the endpoint's answer to a
		// refused exchange names the tenant and the application, and none of it is read out.
		s.refuse(w, info, r, chosen, status, "the token endpoint refused the exchange of this grant's federated token")
		return
	}
	fields, err := azureAnswerOf(answer)
	if err != nil {
		s.refuse(w, info, r, chosen, http.StatusBadGateway, "the token endpoint's answer to this exchange "+err.Error())
		return
	}
	token, expires := accessTokenOf(fields, s.now())
	slot := azureAccessSlot(x)
	// A read of this grant like any other, so it answers by the one rule (grantRefusal): a
	// grant that ended while the exchange ran is the read's own answer, and a read that did
	// not happen says nothing of the store it went to, which for Postgres would be the
	// database's user, its name and its address.
	minted, err := s.grants.AirlockAccessPlaceholder(r.Context(), info.grant.GrantID, chosen.name, slot)
	if err != nil {
		status, why := s.grantRefusal("azure access placeholder", err)
		s.refuse(w, info, r, chosen, status, why)
		return
	}
	s.accessFor(accessKey{grant: info.grant.GrantID, name: chosen.name, slot: slot}).keep(token, expires)
	document, err := answerWithPlaceholder(fields, minted)
	if err != nil {
		s.refuse(w, info, r, chosen, http.StatusBadGateway, "answering this exchange: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(document)
	s.log.Info(callMessage, s.callFields(info, r, chosen, http.StatusOK, "")...)
}

// azureAccess is the real access token behind an azure-access slot: the one this pod holds for
// that grant and slot, and otherwise the one an exchange it makes for itself answers. A pod
// that minted no placeholder of this slot still serves it, which is what the slot's own
// contents are for: the proxy runs in several pods, a command's connections are spread over
// them, and a pod that could not exchange would have to refuse a placeholder the command was
// rightly given.
func (s *Server) azureAccess(ctx context.Context, info connInfo, chosen pair, secret requests.AirlockSecret) (string, error) {
	held := s.accessFor(accessKey{grant: info.grant.GrantID, name: chosen.name, slot: chosen.slot})
	held.exchange.Lock()
	defer held.exchange.Unlock()
	if token, ok := held.held(s.now()); ok {
		return token, nil
	}
	x, ok := azureExchangeOf(chosen.slot)
	if !ok {
		return "", errors.New("this request carries a placeholder of a slot that names no exchange this broker made")
	}
	endpoint, err := url.Parse(x.Endpoint)
	if err != nil || endpoint.Scheme != "https" {
		return "", errors.New("the slot of this placeholder names no https token endpoint")
	}
	entry := s.rules.Lookup(chosen.name)
	if entry == nil || !airlock.HostBound(entry.Hosts, airlock.ASCIILower(endpoint.Hostname())) {
		// The exchange is a call the broker makes with the real federated token, so where it
		// is made is held to the same hosts as every other call of this name: the rules file's.
		return "", errors.New("the token endpoint this slot names is no host " + chosen.name + " is bound to")
	}
	federated := secret.Slots[requests.SlotAzureFederated]
	if federated == "" {
		return "", errors.New("the broker released no federated token for " + chosen.name + ", so no access token can be exchanged for it")
	}
	status, answer, err := s.exchangeAt(ctx, x.Endpoint, url.Values{
		// The federated flow's own form, which is what the command sent when a pod minted this
		// slot: the two constants, the application and the scope the slot carries, and the
		// real assertion.
		"grant_type":            {"client_credentials"},
		"client_id":             {x.ClientID},
		"scope":                 {x.Scope},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {federated},
	})
	if err != nil {
		return "", errors.New("the exchange of this grant's federated token could not be made at " + x.Endpoint + ": " + err.Error())
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("the token endpoint refused the exchange of this grant's federated token (%d)", status)
	}
	fields, err := azureAnswerOf(answer)
	if err != nil {
		return "", errors.New("the token endpoint's answer to this exchange " + err.Error())
	}
	token, expires := accessTokenOf(fields, s.now())
	held.keep(token, expires)
	return token, nil
}

// exchangeAt makes one exchange: the broker's own POST to the token endpoint, on the transport
// every upstream call of this proxy goes out on, and the endpoint's status and whole answer.
func (s *Server) exchangeAt(ctx context.Context, endpoint string, form url.Values) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	answer, err := s.upstream.RoundTrip(request)
	if err != nil {
		return 0, nil, err
	}
	defer answer.Body.Close()
	body, err := io.ReadAll(io.LimitReader(answer.Body, maxTokenAnswer))
	if err != nil {
		return answer.StatusCode, nil, err
	}
	return answer.StatusCode, body, nil
}

// azureAnswerOf takes the endpoint's answer apart once, keeping every field's own bytes, so the
// command is answered the shape Microsoft wrote and not one rebuilt from the fields this code
// happens to know.
func azureAnswerOf(answer []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(answer, &fields); err != nil {
		return nil, errors.New("is no JSON object")
	}
	var token string
	if err := json.Unmarshal(fields["access_token"], &token); err != nil || token == "" {
		return nil, errors.New("holds no access_token")
	}
	return fields, nil
}

// accessTokenOf is the access token the endpoint answered and when it stops working:
// expires_in seconds from now, and no expiry at all for an answer that named no lifetime, which
// is then held until the name is reread.
func accessTokenOf(fields map[string]json.RawMessage, now time.Time) (string, time.Time) {
	var token string
	_ = json.Unmarshal(fields["access_token"], &token)
	var seconds json.Number
	if err := json.Unmarshal(fields["expires_in"], &seconds); err != nil {
		return token, time.Time{}
	}
	lifetime, err := seconds.Int64()
	if err != nil || lifetime <= 0 {
		return token, time.Time{}
	}
	return token, now.Add(time.Duration(lifetime) * time.Second)
}

// answerWithPlaceholder is the endpoint's own answer with the placeholder where the access
// token was: every other field as the endpoint wrote it, except a field whose name ends in
// _token, which is left out. An id_token or a refresh_token is a credential of its own, and the
// airlock hands the command no credential - the one token it names is the one it holds a
// placeholder for.
func answerWithPlaceholder(fields map[string]json.RawMessage, placeholder string) ([]byte, error) {
	document := make(map[string]json.RawMessage, len(fields))
	for name, value := range fields {
		if name != "access_token" && strings.HasSuffix(name, "_token") {
			continue
		}
		document[name] = value
	}
	minted, err := json.Marshal(placeholder)
	if err != nil {
		return nil, err
	}
	document["access_token"] = minted
	return json.Marshal(document)
}
