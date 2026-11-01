package oauth

import (
	"context"
	"fmt"
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
// code's side is collected by calling decide for each kind of subject token — a pod's, whose entry
// gives the named project scope, a machine access token's, whose scope is the person's, and a
// machine's Google identity token, whose entry names a service — and taking the union, since
// `project` is written under one scope and deleted under the other. So a claim a new scope or a
// new decision adds is a claim this fails until the list names it. The one claim no decision
// writes, the launcher credential a machine's exchange mints (machineCredential, which runs after
// decide because the credential does not exist until the request's key is bound), is added to that
// union by name; what proves it is written is the live exchange in cmd/broker.
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
	// The Google half: the issuer a machine's identity token comes from, the audience it must
	// carry, and the account an entry lists for a fleet that names no person.
	claimsGoogleIssuer   = "https://accounts.google.example"
	claimsGoogleAudience = "https://broker.example/machines"
	claimsGoogleAccount  = "example-fleet@example-project.iam.gserviceaccount.com"
	claimsGoogleService  = "example-fleet"
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

// issuedClaims are the claim names an exchanged token carries, sorted: the keys decide writes onto
// an exchange session over each kind of subject token and each project scope, and the launcher
// credential a machine's exchange adds after decide has run. A scope that wrote a claim nothing
// documents, or stopped writing one the list still carries, lands in this set.
func issuedClaims(t *testing.T) []string {
	t.Helper()
	policy, err := exchange.NewSet([]exchange.AccountEntry{{
		Account: claimsPodAccount,
		Project: claimsProject,
		Person:  claimsPerson,
	}}, nil, []exchange.GoogleEntry{{Account: claimsGoogleAccount, Service: claimsGoogleService}})
	if err != nil {
		t.Fatalf("exchange.NewSet: %v", err)
	}
	// Keys is left nil: decide verifies no signature. It checks a pod token's audience against the
	// configured issuer (audience, hasAudience) and the library, which is what resolves a token's
	// key, is not in this call at all.
	provider := &Provider{exchange: ExchangeConfig{
		Policy:     policy,
		Kubernetes: IssuerConfig{Issuer: claimsIssuer, Audience: claimsAudience},
		Google:     IssuerConfig{Issuer: claimsGoogleIssuer, Audience: claimsGoogleAudience},
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
	}, {
		tokenType: oauthwire.SubjectTokenTypeGoogle,
		token: map[string]any{
			"aud":            claimsGoogleAudience,
			"email":          claimsGoogleAccount,
			"email_verified": true,
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
	// The launcher credential a machine's exchange answers with, written onto the session by
	// machineCredential once the request's key is bound and the credential exists, which is
	// after decide has run. It is named here rather than decided, so what holds the code to it
	// is the live exchange in cmd/broker, not this check.
	issued[oauthwire.ExtraCredentialID] = true
	return slices.Sorted(maps.Keys(issued))
}

// documentedClaims reads the claim names from the bullet list under claimsDocHeading, in the list's
// order: the backticked name each bullet opens with, and only that one. A name the list carries
// twice is returned twice, so the caller's slices.Equal fails on it rather than a set hiding it.
func documentedClaims(t *testing.T) []string {
	t.Helper()
	// The document is at the repository root, five directories above this package. Read it, and
	// fail when it cannot be read: a skip here would be an absence that reads exactly like a pass,
	// and a silent absence is what this check exists to stop.
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "docs", "kubernetes.md"))
	if err != nil {
		t.Fatalf("read docs/kubernetes.md: %v", err)
	}
	claims, err := claimListNames(string(doc))
	if err != nil {
		t.Fatalf("docs/kubernetes.md %v", err)
	}
	return claims
}

// claimListNames reads the claim names from the first bullet list under claimsDocHeading in doc, in
// the list's order. The list runs through the blank lines between its items and the indented lines
// of a wrapped item, and ends at the prose under it.
func claimListNames(doc string) ([]string, error) {
	lines := strings.Split(doc, "\n")
	heading := slices.Index(lines, claimsDocHeading)
	if heading < 0 {
		return nil, fmt.Errorf("has no line %q: the section holding the claim list was renamed, "+
			"and this check stops checking the moment it cannot find it", claimsDocHeading)
	}

	var claims []string
	// The list is the section's first one: the prose lines between the heading and it are skipped.
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
				return nil, fmt.Errorf("reaches %q under %q with no bullet list between them: "+
					"the claims are no longer a list, and nothing is being held to them", line, claimsDocHeading)
			}
			continue
		}
		bullet := strings.TrimSpace(strings.TrimPrefix(line, "- "))
		inner, opened := strings.CutPrefix(bullet, "`")
		name, rest, closed := strings.Cut(inner, "`")
		if !opened || !closed || !strings.HasPrefix(rest, claimsDocSeparator) {
			return nil, fmt.Errorf("line %d of the claim list is %q, which does not open with a "+
				"backticked claim name followed by %q: the list's shape is what tells this check "+
				"which of a bullet's code spans is the claim it documents",
				i+1, line, strings.TrimSpace(claimsDocSeparator))
		}
		claims = append(claims, name)
	}

	// An empty list is this check failing open: the heading is still there, the parse matched
	// nothing, and every issued claim would then be reported against a list nobody is reading.
	if len(claims) == 0 {
		return nil, fmt.Errorf("names no claim under %q: the bullet list the section promises "+
			"is gone or no longer a list, and nothing is being held to it", claimsDocHeading)
	}
	return claims, nil
}

// The claim list is read whether its items follow one another directly or stand apart with a blank
// line between them, as the guide's one-sentence-per-line layout writes every list; an item wrapped
// onto an indented line is still one item; and the list ends at the prose under it, so a later
// list in the same section is never read as the claims.
func TestClaimListNamesReadsTightAndBlankSeparatedLists(t *testing.T) {
	const intro = claimsDocHeading + "\n\nAn issued token carries these claims and no others:\n\n"
	const after = "\nThe subject token's own kind decides those claims.\n\n- `other` — a later list, not a claim.\n"
	want := []string{"session", "person", "project_scope", "project"}
	for _, shape := range []struct{ name, list string }{{
		name: "tight",
		list: "- `session` — the session.\n" +
			"- `person` — the person.\n" +
			"- `project_scope` — `named` or `person`, which says what `project` means.\n" +
			"- `project` — the one project key.\n" +
			"  A `person` token carries none.\n",
	}, {
		name: "blank-separated",
		list: "- `session` — the session.\n\n" +
			"- `person` — the person.\n\n" +
			"- `project_scope` — `named` or `person`, which says what `project` means.\n\n" +
			"- `project` — the one project key.\n" +
			"  A `person` token carries none.\n",
	}} {
		t.Run(shape.name, func(t *testing.T) {
			got, err := claimListNames(intro + shape.list + after)
			if err != nil {
				t.Fatalf("claimListNames: %v", err)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("claimListNames read %v, want %v", got, want)
			}
		})
	}
}
