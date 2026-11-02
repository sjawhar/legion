// packages/envoy/internal/brokerclient/wire_test.go
//
// The wire contract, replayed. packages/broker-client/testdata/wire.json holds one entry per
// exchange the broker's token endpoint defines, and this suite drives this client through every
// one of them against a stub broker, comparing what it posted with that entry **as a set**: every
// field the entry names, and no field it does not. The TypeScript client reads the same file and
// is held to it the same way, so a field one side starts sending with no entry here fails that
// side's test at once, and a field added to the corpus fails the other side's until it sends it.

package brokerclient

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

// corpusPath is the one contract file, which the Bun suite beside it reads as well. It sits
// outside this module on purpose: a corpus either client owned would be that client's
// description of the wire rather than the wire itself.
const corpusPath = "../../../broker-client/testdata/wire.json"

// The placeholder tokens this suite substitutes for the corpus's, and the session it asks under.
const (
	corpusSubjectToken = "example-subject-token"
	corpusActorToken   = "example-actor-token"
	corpusSession      = "example-box-session"
)

type corpus struct {
	Contract          string            `json:"contract"`
	Version           int               `json:"version"`
	TokenEndpointPath string            `json:"token_endpoint_path"`
	NeverSent         map[string]string `json:"never_sent"`
	DPoP              struct {
		Header                  string   `json:"header"`
		Typ                     string   `json:"typ"`
		Alg                     string   `json:"alg"`
		EmbedsPublicKey         bool     `json:"embeds_public_key"`
		PrivateMembersForbidden []string `json:"private_members_forbidden"`
		Claims                  []string `json:"claims"`
		HTM                     string   `json:"htm"`
		HTU                     string   `json:"htu"`
	} `json:"dpop"`
	Accept struct {
		Status          int               `json:"status"`
		Fields          map[string]string `json:"fields"`
		OptionalFields  map[string]string `json:"optional_fields"`
		ForbiddenFields map[string]string `json:"forbidden_fields"`
	} `json:"accept"`
	Refuse []struct {
		Name   string         `json:"name"`
		Status int            `json:"status"`
		Body   map[string]any `json:"body"`
		Client string         `json:"client"`
	} `json:"refuse"`
	Exchanges []struct {
		Name string            `json:"name"`
		What string            `json:"what"`
		Form map[string]string `json:"form"`
	} `json:"exchanges"`
}

func readCorpus(t *testing.T) corpus {
	t.Helper()
	raw, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatalf("read the shared wire contract: %v", err)
	}
	var c corpus
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("the shared wire contract at %s is not the JSON object it must be: %v", corpusPath, err)
	}
	if len(c.Exchanges) == 0 || c.TokenEndpointPath == "" {
		t.Fatalf("the shared wire contract at %s names no exchange or no token endpoint", corpusPath)
	}
	return c
}

// sourceFor is the client's source for the subject token type an entry names. A type with no
// source here is an exchange this client cannot make, which is a failure of the client rather
// than of the corpus.
func sourceFor(t *testing.T, subjectType string) Source {
	t.Helper()
	switch subjectType {
	case oauthwire.SubjectTokenTypeKubernetes:
		return &FileSource{Path: writeTokenFile(t, corpusSubjectToken)}
	case oauthwire.SubjectTokenTypeGoogle:
		return &StaticSource{Value: corpusSubjectToken, Type: oauthwire.SubjectTokenTypeGoogle, What: "a machine's Google identity token"}
	case oauthwire.SubjectTokenTypePerson:
		return &StaticSource{Value: corpusSubjectToken, Type: oauthwire.SubjectTokenTypePerson, What: "a person's Google identity token"}
	case oauthwire.SubjectTokenTypeAccessToken:
		return &StaticSource{Value: corpusSubjectToken, Type: oauthwire.SubjectTokenTypeAccessToken, What: "the access token this session holds"}
	default:
		t.Fatalf("the contract names the subject token type %q, which this client has no source for", subjectType)
		return nil
	}
}

// writeTokenFile is a projected token on disk, as a pod's runtime mounts one.
func writeTokenFile(t *testing.T, token string) string {
	t.Helper()
	path := t.TempDir() + "/token"
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTheWireContractIsWhatThisClientPosts drives every exchange the shared corpus defines and
// holds the request to that entry, field for field and claim for claim.
func TestTheWireContractIsWhatThisClientPosts(t *testing.T) {
	contract := readCorpus(t)
	if contract.TokenEndpointPath != TokenPath {
		t.Fatalf("the contract's token endpoint is %q and this client posts to %q", contract.TokenEndpointPath, TokenPath)
	}
	for _, exchange := range contract.Exchanges {
		t.Run(exchange.Name, func(t *testing.T) {
			stub := newStubBroker(t)
			key, err := NewKey()
			if err != nil {
				t.Fatal(err)
			}
			client := &Client{BrokerURL: stub.URL(), Key: key, Session: corpusSession,
				Subject: sourceFor(t, exchange.Form["subject_token_type"])}
			if _, carries := exchange.Form["actor_token"]; carries {
				client.Actor = &StaticSource{Value: corpusActorToken, Type: oauthwire.ActorTokenTypeMachine, What: "this machine's Google identity token"}
			}
			token, err := client.Trade(t.Context())
			if err != nil {
				t.Fatalf("the %s exchange: %v", exchange.Name, err)
			}
			if token.AccessToken == "" || token.Expiry.IsZero() {
				t.Fatalf("the %s exchange answered %+v; want the access token and the expiry the contract's accept block names", exchange.Name, token)
			}
			form, proof := stub.call(t, 0)

			// The form, as a set: every field the entry names, with the value it names, and no
			// field it does not name.
			posted := slices.Sorted(maps.Keys(form))
			want := slices.Sorted(maps.Keys(exchange.Form))
			if !slices.Equal(posted, want) {
				t.Fatalf("the %s exchange posted the fields %v; the contract names %v. A field this client sends must have an entry in %s, and a field the contract names must be sent.",
					exchange.Name, posted, want, corpusPath)
			}
			for field, expected := range exchange.Form {
				if got := form.Get(field); got != expand(expected, stub.URL()) {
					t.Errorf("the %s exchange posted %s=%q; the contract names %q", exchange.Name, field, got, expand(expected, stub.URL()))
				}
			}
			for field, why := range contract.NeverSent {
				if form.Has(field) {
					t.Errorf("the %s exchange posted %s, which the contract never sends: %s", exchange.Name, field, why)
				}
			}

			// The proof, as a set: the claims the contract names and no others, its header, and
			// the public key alone.
			if proof.header["typ"] != contract.DPoP.Typ || proof.header["alg"] != contract.DPoP.Alg {
				t.Errorf("the proof's header is typ %v alg %v; the contract names typ %s alg %s",
					proof.header["typ"], proof.header["alg"], contract.DPoP.Typ, contract.DPoP.Alg)
			}
			claims := slices.Sorted(maps.Keys(proof.claims))
			wantClaims := slices.Sorted(slices.Values(contract.DPoP.Claims))
			if !slices.Equal(claims, wantClaims) {
				t.Fatalf("the proof carries the claims %v; the contract names %v", claims, wantClaims)
			}
			if proof.claims["htm"] != contract.DPoP.HTM || proof.claims["htu"] != expand(contract.DPoP.HTU, stub.URL()) {
				t.Errorf("the proof names htm %v htu %v; the contract names %s %s",
					proof.claims["htm"], proof.claims["htu"], contract.DPoP.HTM, expand(contract.DPoP.HTU, stub.URL()))
			}
			if contract.DPoP.EmbedsPublicKey && len(proof.jwk) == 0 {
				t.Fatal("the proof embeds no JWK, so the broker has no key to bind its token to")
			}
			for _, member := range contract.DPoP.PrivateMembersForbidden {
				if _, there := proof.jwk[member]; there {
					t.Errorf("the embedded JWK carries %q: the broker is sent the public half of the key and nothing else", member)
				}
			}
		})
	}
}

// expand substitutes the corpus's placeholders for this run's own values: the stub broker's URL
// is chosen when it starts, so the token endpoint the proof names is read back through here.
func expand(value, broker string) string {
	return strings.NewReplacer(
		"${subject_token}", corpusSubjectToken,
		"${actor_token}", corpusActorToken,
		"${session}", corpusSession,
		"${token_endpoint}", broker+TokenPath,
		"${broker}", broker,
	).Replace(value)
}

// TestTheClientAcceptsAndRefusesTheAnswersTheContractNames walks the corpus's accept block and
// every refusal in it: the answer the client must take, and the three it must not.
func TestTheClientAcceptsAndRefusesTheAnswersTheContractNames(t *testing.T) {
	contract := readCorpus(t)

	t.Run("accept", func(t *testing.T) {
		body := map[string]any{}
		for field := range contract.Accept.Fields {
			switch field {
			case "access_token":
				body[field] = "example-access-token"
			case "token_type":
				body[field] = "DPoP"
			case "issued_token_type":
				body[field] = oauthwire.SubjectTokenTypeAccessToken
			case "expires_in":
				body[field] = 3600
			default:
				t.Fatalf("the contract's accept block names the field %q, which this suite does not build", field)
			}
		}
		body["credential_id"] = "example-credential"
		body["credential_expires_at"] = "2126-01-02T15:04:05Z"
		token, err := tradeAgainst(t, stubAnswer{status: contract.Accept.Status, body: mustJSON(t, body)})
		if err != nil {
			t.Fatalf("the client refused the answer the contract says it must accept: %v", err)
		}
		if token.AccessToken != "example-access-token" || token.CredentialID != "example-credential" || token.CredentialExpiresAt.IsZero() {
			t.Fatalf("the client read %+v; want the access token and the launcher credential the optional fields carry", token)
		}
	})

	for _, refusal := range contract.Refuse {
		t.Run(refusal.Name, func(t *testing.T) {
			_, err := tradeAgainst(t, stubAnswer{status: refusal.Status, body: mustJSON(t, refusal.Body)})
			if err == nil {
				t.Fatalf("the client took an answer the contract refuses (%s); it must %s", refusal.Name, refusal.Client)
			}
			switch refusal.Name {
			case "the broker refuses the subject token":
				var broker *Error
				if !errors.As(err, &broker) || broker.Code != refusal.Body["error"] || broker.Description != refusal.Body["error_description"] {
					t.Fatalf("the client answered %v; want the broker's own error and description, which it must %s", err, refusal.Client)
				}
			case "the answer carries a refresh token", "the answer names no access token":
				field := "refresh_token"
				if refusal.Name == "the answer names no access token" {
					field = "access_token"
				}
				if !strings.Contains(err.Error(), field) {
					t.Fatalf("the client answered %v; the refusal must name %s", err, field)
				}
			default:
				t.Fatalf("the contract names the refusal %q, which this suite does not check", refusal.Name)
			}
		})
	}
}

// tradeAgainst makes one exchange against a stub broker answering exactly answer.
func tradeAgainst(t *testing.T, answer stubAnswer) (*Token, error) {
	t.Helper()
	stub := newStubBroker(t, answer)
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{BrokerURL: stub.URL(), Key: key, Session: corpusSession,
		Subject: &FileSource{Path: writeTokenFile(t, corpusSubjectToken)}}
	return client.Trade(t.Context())
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
