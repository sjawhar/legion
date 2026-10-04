package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A Sandbox is named for its claim: the token as a DNS-1123 label. The short case is the one the
// locator golden pins (runtime/locator_test.go), whose token carries an upper-case issue key.
func TestSandboxNameIsTheTokenAsALabel(t *testing.T) {
	for token, want := range map[claim.Token]string{
		"legion-omp-LEGION-208-tester":       "legion-omp-legion-208-tester",
		"legion-legion-legion-208-architect": "legion-legion-legion-208-architect",
		"-legion_x..LEGION--9-merger-":       "legion-x-legion-9-merger",
	} {
		if got := SandboxName(token); got != want {
			t.Errorf("SandboxName(%q) = %q, want %q", token, got, want)
		}
	}
}

// Past 63 characters the name keeps a readable prefix and ends in 8 hex of the whole token's
// sha256, so two long tokens that share the prefix still name different Sandboxes, and the name is
// a valid label every time it is computed.
func TestSandboxNameOfALongTokenIsAPrefixAndAHash(t *testing.T) {
	long := claim.Token("legion-" + strings.Repeat("averyverylongprojectname", 3) + "-legion-208-implementer")
	sibling := claim.Token("legion-" + strings.Repeat("averyverylongprojectname", 3) + "-legion-208-reviewer")
	name := SandboxName(long)
	sum := sha256.Sum256([]byte(long))
	if want := strings.ToLower(string(long))[:54] + "-" + hex.EncodeToString(sum[:])[:8]; name != want {
		t.Fatalf("SandboxName = %q, want %q", name, want)
	}
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		t.Fatalf("SandboxName %q is not a DNS-1123 label: %v", name, errs)
	}
	if SandboxName(long) != name {
		t.Fatal("SandboxName is not stable")
	}
	if other := SandboxName(sibling); other == name {
		t.Fatalf("two long tokens share the name %q", name)
	}
}

// The root's tree volume claim is `tree-<root sandbox>`: what the controller names the claim it
// makes from the root's `tree` template (`<template>-<sandbox>`), and what every worker mounts.
func TestTreeClaimNameIsTheControllersClaimName(t *testing.T) {
	if got, want := TreeClaimName(rootToken), treeVolume+"-"+SandboxName(rootToken); got != want || got != "tree-legion-legion-legion-208-architect" {
		t.Fatalf("TreeClaimName = %q, want %q", got, want)
	}
}
