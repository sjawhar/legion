package main

import (
	"path/filepath"
	"testing"
)

// TestOnlyTheRenewCredentialListsEnrollmentNotLive reads the broker's own API package, as the docs
// build does, and checks that the renew route's credential alone lists ENROLLMENT_NOT_LIVE, while
// every other session route's lists PROOF_INVALID and not that code: the error and API pages are
// generated from these classes, so a shared authenticate clause would put the renew-only code on
// every session route.
func TestOnlyTheRenewCredentialListsEnrollmentNotLive(t *testing.T) {
	a, err := readAPI(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]map[string]bool{}
	for _, c := range a.classes {
		codes[c.Wrapper] = map[string]bool{}
		for _, o := range refusals(c.Refusals) {
			codes[c.Wrapper][o.Code] = true
		}
	}
	if !codes["renewAuth"]["ENROLLMENT_NOT_LIVE"] || !codes["renewAuth"]["PROOF_INVALID"] {
		t.Errorf("renewAuth refusals = %v, want ENROLLMENT_NOT_LIVE and PROOF_INVALID", codes["renewAuth"])
	}
	if codes["sessionAuth"]["ENROLLMENT_NOT_LIVE"] || !codes["sessionAuth"]["PROOF_INVALID"] {
		t.Errorf("sessionAuth refusals = %v, want PROOF_INVALID and not ENROLLMENT_NOT_LIVE", codes["sessionAuth"])
	}
	if credentialLabels["renewAuth"] == credentialLabels["sessionAuth"] {
		t.Errorf("renewAuth and sessionAuth share the label %q, so the pages cannot tell their rows apart", credentialLabels["sessionAuth"])
	}
}
