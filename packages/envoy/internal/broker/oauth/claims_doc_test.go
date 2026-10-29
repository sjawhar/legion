package oauth

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The claim list under "The Dispatch identity the broker can issue" in docs/kubernetes.md is what a
// resource server writes its authorization against: it reads "An issued token carries these claims
// and no others", and then trusts both halves of that sentence. A claim this package issues and the
// list omits is a permission nobody downstream knows to check; a claim the list carries and the
// code never issues is a check written against a key that is always absent, which reads as a denial
// the author did not mean. Neither shows up in a test of the exchange itself, because the exchange
// is right in both cases — only the document is wrong, and only a reader would notice.
//
// The list is read as a list, in the one way the precedent in
// packages/daemon/internal/runtime/sandbox/reclaim_doc_test.go had to learn: a claim is the
// backticked name a bullet OPENS with, and a backticked word anywhere later in a bullet's prose is
// not a claim. The `project_scope` bullet is exactly why. It reads "`named` or `person`, which says
// what `project` means", so a check that asked whether the paragraph mentions a claim anywhere
// between its bounds would find `project` in that sentence and pass with the `project` bullet
// deleted — the miss wearing a pass. The em dash the shape puts after the name is required for the
// same reason: it is what makes "the name this bullet is about" a thing the parse can see, rather
// than the first of however many code spans the line happens to carry.
//
// What this cannot do: it compares two sets of names, and nothing more. It does not read a bullet's
// meaning, so a claim whose description is stale, wrong, or swapped with its neighbour's passes
// here; it does not know the order the list is written in; and it says nothing about the claims an
// introspection answer adds around them (`exp`, `client_id`, `jkt`) or about whether the exchange
// actually populates each claim it names. A reader still owns all of that. What it does own is the
// two sets drifting apart while each side looks right on its own.
const (
	// claimsDocMarker is the sentence the list hangs off. It is prose in a hard-wrapped document,
	// so it is found in a whitespace-flattened copy and carried back to the line it starts on; the
	// bullets themselves are read from the lines, since a bullet is a shape of a line and
	// flattening erases it.
	claimsDocMarker = "An issued token carries these claims and no others:"
	// claimsDocSeparator follows the name a bullet opens with. See the comment above: it is the
	// shape that distinguishes the claim a bullet documents from the code spans its prose uses.
	claimsDocSeparator = " — "
)

// The claims docs/kubernetes.md lists are exactly the claims this package puts on an exchanged
// token. The code's side is the four Claim constants, written out here rather than collected from
// the document, so that adding a claim to one side and not the other is what fails.
func TestTheDocumentedClaimsAreTheClaimsAnExchangedTokenCarries(t *testing.T) {
	documented := documentedClaims(t)
	issued := []string{ClaimSession, ClaimPerson, ClaimProjectScope, ClaimProject}

	undocumented := missingFrom(issued, documented)
	unissued := missingFrom(documented, issued)
	if len(undocumented) > 0 || len(unissued) > 0 {
		t.Fatalf("docs/kubernetes.md's claim list and this package's Claim constants disagree:\n"+
			"  issued but not documented: %s\n"+
			"  documented but not issued: %s\n"+
			"the list reads %q, and a resource server believes it",
			claimList(undocumented), claimList(unissued), claimsDocMarker)
	}
}

// documentedClaims reads the claim names from the bullet list under claimsDocMarker: the backticked
// name each bullet opens with, and only that one.
func documentedClaims(t *testing.T) []string {
	t.Helper()
	// The document is at the repository root, five directories above this package. Read it, and
	// fail when it cannot be read: a skip here would be an absence that reads exactly like a pass,
	// and a silent absence is what this check exists to stop.
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "docs", "kubernetes.md"))
	if err != nil {
		t.Fatalf("read docs/kubernetes.md: %v", err)
	}
	lines := strings.Split(string(doc), "\n")
	flat, lineOf := flattenKeepingLines(lines)
	offset := strings.Index(flat, claimsDocMarker)
	if offset < 0 {
		t.Fatalf("docs/kubernetes.md has no %q: the paragraph introducing the claim list was "+
			"reworded, and this check stops checking the moment it cannot find it", claimsDocMarker)
	}

	var claims []string
	// The list follows the marker's own line, after the blank line Markdown requires between a
	// paragraph and a list, and runs to the first line that is neither a bullet nor a wrapped
	// bullet's indented continuation — in the document as written, the blank line under it.
	for i := lineOf[offset+len(claimsDocMarker)-1] + 1; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], " ")
		if strings.TrimSpace(line) == "" {
			if len(claims) > 0 {
				break
			}
			continue
		}
		if len(claims) > 0 && strings.HasPrefix(line, " ") {
			continue
		}
		if !strings.HasPrefix(line, "- ") {
			break
		}
		bullet := strings.TrimSpace(strings.TrimPrefix(line, "- "))
		name, rest, ok := strings.Cut(strings.TrimPrefix(bullet, "`"), "`")
		if !strings.HasPrefix(bullet, "`") || !ok || !strings.HasPrefix(rest, claimsDocSeparator) {
			t.Fatalf("docs/kubernetes.md line %d of the claim list is %q, which does not open with a "+
				"backticked claim name followed by %q: the list's shape is what tells this check "+
				"which of a bullet's code spans is the claim it documents",
				i+1, line, strings.TrimSpace(claimsDocSeparator))
		}
		if !slices.Contains(claims, name) {
			claims = append(claims, name)
		}
	}

	// An empty list is this check failing open: the sentence is still there, the parse matched
	// nothing, and every issued claim would then be reported against a list nobody is reading.
	if len(claims) == 0 {
		t.Fatalf("docs/kubernetes.md names no claim after %q: the bullet list the sentence promises "+
			"is gone or no longer a list, and nothing is being held to it", claimsDocMarker)
	}
	return claims
}

// missingFrom is the names in have that want does not carry, sorted so a failure message reads the
// same on every run.
func missingFrom(have, want []string) []string {
	var missing []string
	for _, name := range have {
		if !slices.Contains(want, name) {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	return missing
}

// claimList renders one side of the difference for a failure message, naming "none" rather than an
// empty line so the reader of a one-sided failure sees which direction held.
func claimList(claims []string) string {
	if len(claims) == 0 {
		return "none"
	}
	return strings.Join(claims, ", ")
}

// flattenKeepingLines collapses the document's whitespace so prose that was hard-wrapped reads as
// one line, and records for every byte of the result the line it came from, which is what turns a
// marker found in the flat text back into the line the list starts under.
func flattenKeepingLines(lines []string) (string, []int) {
	var flat strings.Builder
	lineOf := make([]int, 0, len(lines)*8)
	for i, line := range lines {
		for _, field := range strings.Fields(line) {
			if flat.Len() > 0 {
				flat.WriteByte(' ')
				lineOf = append(lineOf, i)
			}
			flat.WriteString(field)
			for range len(field) {
				lineOf = append(lineOf, i)
			}
		}
	}
	return flat.String(), lineOf
}
