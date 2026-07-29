package main

import (
	"testing"
)

// The subscribe payload carries whether the claiming process is driving the
// session; it must reach the session registry so competing claims can be
// arbitrated (see session.mergeForClaim). Dropping it silently reinstates
// last-writer-wins routing.
func TestSessionEntryFromSubscribe_CarriesDrivingFlag(t *testing.T) {
	got := sessionEntryFromSubscribe(subscribeBody{
		SessionID: "ses_x",
		Dir:       "/work",
		Port:      42145,
		Title:     "ipi eval",
		Driving:   true,
	}, "devbox-sami")

	if !got.Driving {
		t.Fatalf("expected Driving to be carried into the session entry, got %+v", got)
	}
	if got.Port != 42145 || got.Dir != "/work" || got.Title != "ipi eval" || got.MachineID != "devbox-sami" {
		t.Fatalf("unexpected entry mapping: %+v", got)
	}
}

func TestSessionEntryFromSubscribe_DefaultsToNonDriving(t *testing.T) {
	got := sessionEntryFromSubscribe(subscribeBody{SessionID: "ses_y", Port: 34751}, "devbox-sami")

	if got.Driving {
		t.Fatalf("expected a payload without the flag to claim non-driving, got %+v", got)
	}
}
