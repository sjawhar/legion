package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A Sandbox is named for its issue: each role suffix is stripped before the token becomes a
// DNS-1123 label. The locator golden pins the short case, whose claim token carries an upper-case
// issue key.
func TestSandboxNameIsTheIssueAsALabel(t *testing.T) {
	for token, want := range map[claim.Token]string{
		"legion-omp-LEGION-208-tester":       "legion-omp-legion-208",
		"legion-legion-legion-208-architect": "legion-legion-legion-208",
		"-legion_x..LEGION--9-merger-":       "legion-x-legion-9-merger",
	} {
		if got := SandboxName(token); got != want {
			t.Errorf("SandboxName(%q) = %q, want %q", token, got, want)
		}
	}
}

// Past 63 characters the name keeps a readable prefix and ends in 8 hex of the issue token's
// sha256. Role siblings name the same Sandbox, while different issues remain distinct.
func TestSandboxNameOfALongTokenIsAnIssuePrefixAndHash(t *testing.T) {
	long := claim.Token("legion-" + strings.Repeat("averyverylongprojectname", 3) + "-legion-208-implementer")
	sibling := claim.Token("legion-" + strings.Repeat("averyverylongprojectname", 3) + "-legion-208-reviewer")
	issueToken := strings.TrimSuffix(string(long), "-implementer")
	name := SandboxName(long)
	sum := sha256.Sum256([]byte(issueToken))
	if want := strings.ToLower(issueToken)[:54] + "-" + hex.EncodeToString(sum[:])[:8]; name != want {
		t.Fatalf("SandboxName = %q, want %q", name, want)
	}
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		t.Fatalf("SandboxName %q is not a DNS-1123 label: %v", name, errs)
	}
	if SandboxName(long) != name || SandboxName(sibling) != name {
		t.Fatal("role siblings do not share their issue Sandbox name")
	}
}

// The root's tree volume claim is `tree-<root issue Sandbox>`: what the controller names the
// claim it makes from the root's `tree` template (`<template>-<sandbox>`), and what every child
// issue pod mounts.
func TestTreeClaimNameIsTheControllersClaimName(t *testing.T) {
	if got, want := TreeClaimName(rootToken), treeVolume+"-"+SandboxName(rootToken); got != want || got != "tree-legion-legion-legion-208" {
		t.Fatalf("TreeClaimName = %q, want %q", got, want)
	}
}
