package oauth

import (
	"context"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"authelia.com/provider/oauth2"

	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

// The claim list under "The Dispatch identity the broker can issue" in docs/kubernetes.md is what a
// resource server writes its authorization against: it reads "An issued token carries these claims
// and no others", and then trusts both halves of that sentence. A claim this package issues and the
// list omits is a permission nobody downstream knows to check; a claim the list carries and the
// code never issues is a check written against a key that is always absent, which reads as a denial
// the author did not mean. Neither shows up in a test of the exchange itself, because the exchange
// is right in both cases — only the document is wrong.
//
// What is compared: the names the document's bullets open with, against the keys decide writes into
// an exchange session's extra claims, which is where this provider's opaque tokens carry them. The
// code's side is collected by calling decide twice — once for a pod's subject token, whose entry
// gives the named project scope, and once for a machine access token, whose scope is the person's —
// and taking the union, since `project` is written under one scope and deleted under the other. So
// a claim a new scope or a new decision adds is a claim this fails until the list names it.
//
// A claim is the backticked name a bullet opens with, followed by the em dash the list's shape puts
// after it, and a backticked word later in a bullet's prose is not a claim. The `project_scope`
// bullet is why: it reads "`named` or `person`, which says what `project` means", so a check that
// asked whether the section mentions a name anywhere between its bounds would find `project` there
// and pass with the `project` bullet deleted — the miss wearing a pass.
//
// What this cannot do: it compares two sets of names, and nothing more. It does not read a bullet's
// meaning, so a claim whose description is stale, wrong, or swapped with its neighbour's passes
// here; it does not know the order the list is written in; it says nothing about the claims an
// introspection answer adds around them (`exp`, `client_id`, `jkt`); and the union is over the two
// subject kinds it calls decide with, so a claim only a workflow run's token could carry would be
// missed. A reader still owns all of that. What it does own is the two sets drifting apart while
// each side looks right on its own.
const (
	// claimsDocHeading is the section's heading, matched as a whole line: a heading is a line of
	// its own, so the list is reached without parsing the hard-wrapped prose above it.
	claimsDocHeading = "#### The Dispatch identity the broker can issue"
	// claimsDocSeparator follows the name a bullet opens with. See the comment above: it is the
	// shape that distinguishes the claim a bullet documents from the code spans its prose uses.
	claimsDocSeparator = " — "
)

// The listed identity and the two subject tokens decide is asked to decide on here. The account
// entry fixes no session, so the request names one, and the pod token carries the audience the
// configured issuer accepts and the pod binding Set.Pod requires.
const (
	claimsPodAccount   = "system:serviceaccount:example-ns:example-worker"
	claimsPodUID       = "11111111-2222-3333-4444-555555555555"
	claimsPerson       = "ada@example.com"
	claimsProject      = "ACME"
	claimsSessionName  = "claims-doc-check"
	claimsIssuer       = "https://kubernetes.example/cluster"
	claimsAudience     = "https://broker.example"
	claimsCredentialID = "machine-credential-1"
)

// The claims docs/kubernetes.md lists are exactly the claims an exchanged token carries. Both sides
// are read: the document's bullets, and the keys decide writes for the two project scopes a token
// can be issued under.
func TestTheDocumentedClaimsAreTheClaimsAnExchangedTokenCarries(t *testing.T) {
	documented := documentedClaims(t)
	issued := issuedClaims(t)
	slices.Sort(documented)
	if !slices.Equal(documented, issued) {
		t.Fatalf("docs/kubernetes.md's claim list and the claims the exchange decides on disagree:\n"+
			"  documented: %s\n"+
			"  issued:     %s\n"+
			"the list under %q is what a resource server writes its authorization against",
			strings.Join(documented, ", "), strings.Join(issued, ", "), claimsDocHeading)
	}
}

func TestDocumentedClaimsReadLooseListsAndDetectSetChanges(t *testing.T) {
	document := claimsDocument(t)
	const person = "- `person` — the person the session acts for."
	withoutPerson := strings.Replace(document, person+"\n\n", "", 1)
	if withoutPerson == document {
		t.Fatalf("the documentation copy has no %q bullet", person)
	}
	wantWithoutPerson := slices.DeleteFunc(slices.Clone(issuedClaims(t)), func(claim string) bool {
		return claim == "person"
	})
	gotWithoutPerson := documentedClaimsDocument(t, withoutPerson)
	slices.Sort(gotWithoutPerson)
	if !slices.Equal(gotWithoutPerson, wantWithoutPerson) {
		t.Fatalf("the parser read claims without person as %s, want %s", gotWithoutPerson, wantWithoutPerson)
	}

	const extra = "- `extra` — a claim the exchange does not issue."
	withExtra := strings.Replace(document, person+"\n\n", person+"\n\n"+extra+"\n\n", 1)
	if withExtra == document {
		t.Fatalf("the documentation copy has no %q bullet", person)
	}
	wantWithExtra := append(issuedClaims(t), "extra")
	slices.Sort(wantWithExtra)
	gotWithExtra := documentedClaimsDocument(t, withExtra)
	slices.Sort(gotWithExtra)
	if !slices.Equal(gotWithExtra, wantWithExtra) {
		t.Fatalf("the parser read claims with extra as %s, want %s", gotWithExtra, wantWithExtra)
	}
}

// issuedClaims are the claim names decide writes onto an exchange session, sorted, over both of the
// project scopes it can decide: a pod's projected token, which its entry gives a named project, and
// a machine login's access token, which names none. A scope that wrote a claim nothing documents,
// or stopped writing one the list still carries, lands in this set.
func issuedClaims(t *testing.T) []string {
	t.Helper()
	policy, err := exchange.NewSet([]exchange.AccountEntry{{
		Account: claimsPodAccount,
		Project: claimsProject,
		Person:  claimsPerson,
	}}, nil)
	if err != nil {
		t.Fatalf("exchange.NewSet: %v", err)
	}
	// Keys is left nil: decide verifies no signature. It checks a pod token's audience against the
	// configured issuer (audience, hasAudience) and the library, which is what resolves a token's
	// key, is not in this call at all.
	provider := &Provider{exchange: ExchangeConfig{
		Policy:     policy,
		Kubernetes: IssuerConfig{Issuer: claimsIssuer, Audience: claimsAudience},
	}}

	subjects := []struct {
		tokenType string
		token     map[string]any
	}{{
		tokenType: oauthwire.SubjectTokenTypeKubernetes,
		token: map[string]any{
			"sub":           claimsPodAccount,
			"aud":           claimsAudience,
			"kubernetes.io": map[string]any{"pod": map[string]any{"uid": claimsPodUID}},
		},
	}, {
		tokenType: tokenTypeAccessToken,
		token: map[string]any{
			"sub":                       claimsPerson,
			oauthwire.ExtraCredentialID: claimsCredentialID,
		},
	}}

	issued := map[string]bool{}
	for _, subject := range subjects {
		session := oauthstore.NewExchangeSession()
		request := &oauth2.Request{
			Form: url.Values{
				FormSubjectTokenType: {subject.tokenType},
				FormSession:          {claimsSessionName},
			},
			Session: session,
		}
		if _, err := provider.decide(context.Background(), request, subject.token); err != nil {
			t.Fatalf("the exchange refused the %s subject token this check decides against: %v; "+
				"the claims are then unknown and nothing is being held to the list", subject.tokenType, err)
		}
		// A decision that wrote no claim is this check failing open on the code's side, as an
		// empty bullet list is on the document's.
		if len(session.Extra) == 0 {
			t.Fatalf("the exchange decided the %s subject token and wrote no claim at all", subject.tokenType)
		}
		for name := range session.Extra {
			issued[name] = true
		}
	}
	return slices.Sorted(maps.Keys(issued))
}

// documentedClaims reads the claim names from the bullet list under claimsDocHeading, in the list's
// order: the backticked name each bullet opens with, and only that one. A name the list carries
// twice is returned twice, so the caller's slices.Equal fails on it rather than a set hiding it.
func documentedClaims(t *testing.T) []string {
	t.Helper()
	return documentedClaimsDocument(t, claimsDocument(t))
}

func claimsDocument(t *testing.T) string {
	t.Helper()
	// The document is at the repository root, five directories above this package. Read it, and
	// fail when it cannot be read: a skip here would be an absence that reads exactly like a pass,
	// and a silent absence is what this check exists to stop.
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "docs", "kubernetes.md"))
	if err != nil {
		t.Fatalf("read docs/kubernetes.md: %v", err)
	}
	return string(doc)
}

func documentedClaimsDocument(t *testing.T, doc string) []string {
	t.Helper()
	lines := strings.Split(doc, "\n")
	heading := slices.Index(lines, claimsDocHeading)
	if heading < 0 {
		t.Fatalf("docs/kubernetes.md has no line %q: the section holding the claim list was renamed, "+
			"and this check stops checking the moment it cannot find it", claimsDocHeading)
	}

	var claims []string
	// The list is the section's first one: the prose lines between the heading and it are skipped,
	// and the list runs to the first line that is neither a bullet, a blank between bullets, nor a
	// wrapped bullet's indented continuation.
	for i := heading + 1; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], " ")
		if len(claims) > 0 && (line == "" || strings.HasPrefix(line, " ")) {
			continue
		}
		if !strings.HasPrefix(line, "- ") {
			if len(claims) > 0 {
				break
			}
			if strings.HasPrefix(line, "#") {
				t.Fatalf("docs/kubernetes.md reaches %q under %q with no bullet list between them: "+
					"the claims are no longer a list, and nothing is being held to them", line, claimsDocHeading)
			}
			continue
		}
		bullet := strings.TrimSpace(strings.TrimPrefix(line, "- "))
		inner, opened := strings.CutPrefix(bullet, "`")
		name, rest, closed := strings.Cut(inner, "`")
		if !opened || !closed || !strings.HasPrefix(rest, claimsDocSeparator) {
			t.Fatalf("docs/kubernetes.md line %d of the claim list is %q, which does not open with a "+
				"backticked claim name followed by %q: the list's shape is what tells this check "+
				"which of a bullet's code spans is the claim it documents",
				i+1, line, strings.TrimSpace(claimsDocSeparator))
		}
		claims = append(claims, name)
	}

	// An empty list is this check failing open: the heading is still there, the parse matched
	// nothing, and every issued claim would then be reported against a list nobody is reading.
	if len(claims) == 0 {
		t.Fatalf("docs/kubernetes.md names no claim under %q: the bullet list the section promises "+
			"is gone or no longer a list, and nothing is being held to it", claimsDocHeading)
	}
	return claims
}
