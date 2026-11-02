// packages/envoy/internal/brokerclient/wire_test.go
//
// The Go half of the one wire contract: this replays every exchange in testdata/wire.json against
// a stub broker and holds this client to it, as packages/broker-client's suite replays the same
// file against the TypeScript one. The assertions are set equality, never containment — the
// corpus is the whole of what a request may carry and the whole of what a client may read out of
// an answer — so a field either client starts sending or reading without an entry in that file
// fails here, and a field added to that file fails the client that does not send it yet.

package brokerclient

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// corpusPath is the one file both clients read. It lives in this package's testdata because the
// Go suite is the half that exists today; the TypeScript suite reads it by relative path.
const corpusPath = "testdata/wire.json"

// tokenFieldNames maps each field of Token to the name the corpus gives it in expect.token.
// Every field must be named here: a field added to Token without an entry fails
// TestTheCorpusNamesEveryFieldAClientReads, which is how "the corpus is the whole of what a
// client reads" is kept true as this package changes.
var tokenFieldNames = map[string]string{
	"AccessToken":         "access_token",
	"TokenType":           "token_type",
	"Expiry":              "expires_in",
	"CredentialID":        "credential_id",
	"CredentialExpiresAt": "credential_expires_at",
}

type corpus struct {
	Version  int `json:"version"`
	Endpoint struct {
		Method      string `json:"method"`
		Path        string `json:"path"`
		ContentType string `json:"content_type"`
	} `json:"endpoint"`
	Exchanges []corpusExchange `json:"exchanges"`
}

type corpusExchange struct {
	Name    string      `json:"name"`
	Subject corpusToken `json:"subject"`
	// Actor is the machine a person signed in on; absent from every trade a machine makes for
	// itself.
	Actor   *corpusToken      `json:"actor"`
	Session string            `json:"session"`
	Form    map[string]string `json:"form"`
	Proof   struct {
		Header map[string]any `json:"header"`
		Claims map[string]any `json:"claims"`
	} `json:"proof"`
	Answer struct {
		Status int            `json:"status"`
		Body   map[string]any `json:"body"`
	} `json:"answer"`
	Expect struct {
		Outcome       string         `json:"outcome"`
		Permanent     bool           `json:"permanent"`
		Token         map[string]any `json:"token"`
		ErrorContains string         `json:"error_contains"`
	} `json:"expect"`
}

type corpusToken struct {
	Kind      string `json:"kind"`
	Token     string `json:"token"`
	TokenType string `json:"token_type"`
}

func readCorpus(t *testing.T) corpus {
	t.Helper()
	raw, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatalf("read the wire corpus: %v", err)
	}
	var read corpus
	if err := json.Unmarshal(raw, &read); err != nil {
		t.Fatalf("read the wire corpus: %v", err)
	}
	if len(read.Exchanges) == 0 {
		t.Fatal("the wire corpus names no exchange at all")
	}
	return read
}

// TestTheWireCorpusHoldsThisClient drives this client through every exchange the corpus names and
// asserts the request it made and the answer it took, whole.
func TestTheWireCorpusHoldsThisClient(t *testing.T) {
	read := readCorpus(t)
	for _, exchange := range read.Exchanges {
		t.Run(exchange.Name, func(t *testing.T) {
			broker := newStubBroker(t, func(int, url.Values) (int, map[string]any) {
				return exchange.Answer.Status, exchange.Answer.Body
			})
			client, err := New(Config{
				BrokerURL: broker.URL(),
				Key:       testKey(t),
				Subject:   corpusSource(t, exchange.Subject),
				Actor:     corpusActor(t, exchange.Actor),
				Session:   exchange.Session,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			token, err := client.Token(context.Background())
			requests := broker.Requests()
			if len(requests) != 1 {
				t.Fatalf("the client made %d trades, want exactly one", len(requests))
			}
			checkRequest(t, read, exchange, requests[0])
			switch exchange.Expect.Outcome {
			case "accept":
				if err != nil {
					t.Fatalf("Token: %v, want the token the corpus names", err)
				}
				checkToken(t, exchange, token)
			case "refuse":
				checkRefusal(t, exchange, token, err)
			default:
				t.Fatalf("the corpus entry's expect.outcome is %q, and this suite knows accept and refuse", exchange.Expect.Outcome)
			}
		})
	}
}

// corpusSource is the identity an entry's subject names, built from the real reader wherever one
// exists: a projected token is read from a file this test writes, and a machine's Google
// identity token is read from a metadata server this test serves. A person's sign-in is scripted
// here, its own reader being the device grant, which TestDeviceSignIn drives against a real
// issuer.
func corpusSource(t *testing.T, spec corpusToken) Source {
	t.Helper()
	switch spec.Kind {
	case "kubernetes-projected-file":
		path := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(path, []byte(spec.Token+"\n"), 0o600); err != nil {
			t.Fatalf("write the projected token: %v", err)
		}
		return NewProjectedToken(path)
	case "google-metadata":
		return metadataSource(t, spec.Token)
	case "person-device-sign-in":
		return &scriptedSource{token: spec.Token, tokenType: spec.TokenType, describe: "a person's sign-in"}
	default:
		t.Fatalf("the corpus names the subject kind %q, which this suite does not know", spec.Kind)
		return nil
	}
}

func corpusActor(t *testing.T, spec *corpusToken) Source {
	t.Helper()
	if spec == nil {
		return nil
	}
	return corpusSource(t, *spec)
}

// metadataSource serves one Compute Engine metadata server answering token, and the identity
// that reads it. It asserts the reader asked the way the contract says: the identity path, the
// Metadata-Flavor header, format=full and the audience.
func metadataSource(t *testing.T, token string) Source {
	t.Helper()
	const audience = "example-machine-audience"
	server := newStubMetadata(t, audience, token)
	t.Setenv("GCE_METADATA_HOST", server)
	return NewMetadataIdentity(audience)
}

// checkRequest holds one trade to the corpus entry: the endpoint, the whole form, and the whole
// proof.
func checkRequest(t *testing.T, read corpus, exchange corpusExchange, request stubRequest) {
	t.Helper()
	if request.Method != read.Endpoint.Method || request.Path != read.Endpoint.Path {
		t.Errorf("the trade went to %s %s, want %s %s", request.Method, request.Path, read.Endpoint.Method, read.Endpoint.Path)
	}
	if contentType, _, _ := strings.Cut(request.ContentType, ";"); contentType != read.Endpoint.ContentType {
		t.Errorf("the trade's content type is %q, want %q", request.ContentType, read.Endpoint.ContentType)
	}
	form := map[string]string{}
	for name, values := range request.Form {
		if len(values) != 1 {
			t.Errorf("the trade carries %s %d times, and the corpus names one value for each field", name, len(values))
			continue
		}
		form[name] = values[0]
	}
	checkNames(t, "the trade's form", keysOf(exchange.Form), keysOf(form))
	for name, want := range exchange.Form {
		got, sent := form[name]
		if !sent {
			continue // checkNames has already named it
		}
		if resolved, substituted := resolvePlaceholder(exchange, want); substituted {
			want = resolved
		}
		if got != want {
			t.Errorf("the trade's %s is %q, want %q", name, got, want)
		}
	}
	checkProof(t, exchange, request)
}

// checkProof holds the DPoP proof to the corpus: its header and claim sets whole, the literal
// claims, and the shape of each placeholder — including that the embedded key carries no private
// member, which is the whole of what the broker may ever receive of a client's key.
func checkProof(t *testing.T, exchange corpusExchange, request stubRequest) {
	t.Helper()
	checkNames(t, "the proof's header", keysOf(exchange.Proof.Header), keysOf(request.ProofHeader))
	checkNames(t, "the proof's claims", keysOf(exchange.Proof.Claims), keysOf(request.ProofClaims))
	for name, want := range exchange.Proof.Header {
		if name == "jwk" {
			checkPublicJWK(t, request.ProofHeader["jwk"])
			continue
		}
		if got := request.ProofHeader[name]; got != want {
			t.Errorf("the proof's %s header is %v, want %v", name, got, want)
		}
	}
	for name, want := range exchange.Proof.Claims {
		got := request.ProofClaims[name]
		switch want {
		case "${jti}":
			if value, _ := got.(string); value == "" {
				t.Errorf("the proof's jti is %v, want a value of this request's own", got)
			}
		case "${iat}":
			seconds, ok := got.(float64)
			if !ok || math.Abs(float64(time.Now().Unix())-seconds) > 60 {
				t.Errorf("the proof's iat is %v, want the time it was signed in whole seconds", got)
			}
		case "${token_endpoint}":
			// The proof names the endpoint without query or fragment; the stub's own URL.
			if value, _ := got.(string); !strings.HasSuffix(value, TokenPath) || !strings.HasPrefix(value, "http://") {
				t.Errorf("the proof's htu is %v, want the token endpoint it was sent to", got)
			}
		default:
			if got != want {
				t.Errorf("the proof's %s claim is %v, want %v", name, got, want)
			}
		}
	}
}

// checkPublicJWK is the contract's own clause that only the public half of a key ever leaves the
// process: the embedded JWK is an EC P-256 public key, and carries no d member whatever.
func checkPublicJWK(t *testing.T, embedded any) {
	t.Helper()
	jwk, ok := embedded.(map[string]any)
	if !ok {
		t.Fatalf("the proof embeds %v in place of a JWK", embedded)
	}
	if _, private := jwk["d"]; private {
		t.Fatal("the proof's embedded key carries a d member: the broker must receive the public half and nothing else")
	}
	for name, want := range map[string]any{"kty": "EC", "crv": "P-256"} {
		if jwk[name] != want {
			t.Errorf("the proof's embedded key has %s %v, want %v", name, jwk[name], want)
		}
	}
	for _, name := range []string{"x", "y"} {
		if value, _ := jwk[name].(string); value == "" {
			t.Errorf("the proof's embedded key carries no %s", name)
		}
	}
}

// checkToken holds what the client took out of the answer to the corpus's expect.token, whole:
// every field of Token is read through tokenFieldNames, so a field this package adds without an
// entry in the corpus is caught here as well as in the name test below.
func checkToken(t *testing.T, exchange corpusExchange, token *Token) {
	t.Helper()
	got := tokenAsCorpus(t, token)
	checkNames(t, "the token the client took", keysOf(exchange.Expect.Token), keysOf(got))
	for name, want := range exchange.Expect.Token {
		value, read := got[name]
		if !read {
			continue
		}
		if fmt.Sprint(value) != fmt.Sprint(want) {
			t.Errorf("the client read %s as %v, want %v", name, value, want)
		}
	}
}

// tokenAsCorpus is a Token in the corpus's own names and values: expires_in is the seconds left,
// rounded, since a client reads a lifetime and holds an instant.
func tokenAsCorpus(t *testing.T, token *Token) map[string]any {
	t.Helper()
	value := reflect.ValueOf(*token)
	fields := value.Type()
	read := map[string]any{}
	for i := range fields.NumField() {
		name, named := tokenFieldNames[fields.Field(i).Name]
		if !named {
			t.Fatalf("Token has the field %s, which testdata/wire.json names nowhere: add it to the corpus, so the TypeScript client is held to it too", fields.Field(i).Name)
		}
		switch name {
		case "expires_in":
			read[name] = int(math.Round(time.Until(value.Field(i).Interface().(time.Time)).Seconds()))
		case "credential_expires_at":
			at := value.Field(i).Interface().(time.Time)
			if at.IsZero() {
				continue
			}
			read[name] = at.UTC().Format(time.RFC3339)
		default:
			read[name] = value.Field(i).Interface()
		}
	}
	return read
}

// checkRefusal holds a refused exchange: no token at all, the broker's own words in the error,
// and whether the client reads the refusal as one no retry can cure.
func checkRefusal(t *testing.T, exchange corpusExchange, token *Token, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("Token answered %+v, want the refusal the corpus names", token)
	}
	if token != nil {
		t.Errorf("Token answered both a token and an error; a refused trade must hand its caller no identity at all")
	}
	if !strings.Contains(err.Error(), exchange.Expect.ErrorContains) {
		t.Errorf("Token's error is %q, want it to carry %q", err, exchange.Expect.ErrorContains)
	}
	var refused *RefusedError
	if permanent := asRefused(err, &refused); permanent != exchange.Expect.Permanent {
		t.Errorf("the client reads the refusal as permanent=%t, want %t: only a refusal the broker made of the identity stops a renewal loop", permanent, exchange.Expect.Permanent)
	}
}

func asRefused(err error, target **RefusedError) bool {
	for err != nil {
		if refused, ok := err.(*RefusedError); ok {
			*target = refused
			return true
		}
		unwrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapped.Unwrap()
	}
	return false
}

// TestTheCorpusNamesEveryFieldAClientReads is the other direction of the same rule, checked
// without a stub broker: every field of Token is named in the corpus, so neither client can read
// a member of an answer that the other knows nothing about.
func TestTheCorpusNamesEveryFieldAClientReads(t *testing.T) {
	read := readCorpus(t)
	named := map[string]bool{}
	for _, exchange := range read.Exchanges {
		for name := range exchange.Expect.Token {
			named[name] = true
		}
	}
	fields := reflect.TypeOf(Token{})
	for i := range fields.NumField() {
		name, inMap := tokenFieldNames[fields.Field(i).Name]
		if !inMap {
			t.Fatalf("Token.%s is in no corpus entry's expect.token: add it to testdata/wire.json and to tokenFieldNames", fields.Field(i).Name)
		}
		if !named[name] {
			t.Errorf("no corpus entry names %q in its expect.token, so the TypeScript client is held to nothing about it", name)
		}
	}
}

// keysOf is the sorted names of a map, whatever its value type.
func keysOf[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// checkNames is the set equality every assertion of this contract rests on: what the corpus
// names and what the client sent or read, neither more nor less.
func checkNames(t *testing.T, what string, want, got []string) {
	t.Helper()
	if slices.Equal(want, got) {
		return
	}
	t.Errorf("%s carries %v, and the corpus names %v: a field one client sends or reads must be in testdata/wire.json, or the other client is held to nothing about it", what, got, want)
}

// resolvePlaceholder is the corpus value with this exchange's own substitution applied, and
// reports whether it was one of the placeholders a form field may carry.
func resolvePlaceholder(exchange corpusExchange, value string) (string, bool) {
	switch value {
	case "${subject_token}":
		return exchange.Subject.Token, true
	case "${actor_token}":
		if exchange.Actor == nil {
			return "", true
		}
		return exchange.Actor.Token, true
	case "${session}":
		return exchange.Session, true
	}
	return value, false
}

// newStubMetadata serves a Compute Engine metadata server answering token for audience, and
// answers its host, what GCE_METADATA_HOST takes. It refuses a request that does not ask the way
// internal/googleidentity must: the identity path, the Metadata-Flavor header, format=full and
// the audience.
func newStubMetadata(t *testing.T, audience, token string) string {
	t.Helper()
	server := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path != "/computeMetadata/v1/instance/service-accounts/default/identity":
			http.Error(w, "not the identity path", http.StatusNotFound)
		case r.Header.Get("Metadata-Flavor") != "Google":
			http.Error(w, "no Metadata-Flavor header", http.StatusForbidden)
		case r.URL.Query().Get("format") != "full":
			http.Error(w, "the identity must be asked for with format=full", http.StatusBadRequest)
		case r.URL.Query().Get("audience") != audience:
			http.Error(w, "another audience", http.StatusBadRequest)
		default:
			_, _ = w.Write([]byte(token))
		}
	})
	return strings.TrimPrefix(server, "http://")
}
