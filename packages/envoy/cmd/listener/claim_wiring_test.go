package main

import (
	"encoding/json"
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

func TestSessionEntryFromSubscribe_CarriesSelfSubscribedFlag(t *testing.T) {
	// Given
	var body subscribeBody
	if err := json.Unmarshal([]byte(`{"session_id":"ses_omp","self_subscribed":true}`), &body); err != nil {
		t.Fatalf("decode subscribe body: %v", err)
	}

	// When
	entry, err := json.Marshal(sessionEntryFromSubscribe(body, "devbox-sami"))
	if err != nil {
		t.Fatalf("encode session entry: %v", err)
	}

	// Then
	var payload map[string]any
	if err := json.Unmarshal(entry, &payload); err != nil {
		t.Fatalf("decode session entry: %v", err)
	}
	selfSubscribed, ok := payload["self_subscribed"].(bool)
	if !ok || !selfSubscribed {
		t.Fatalf("expected self_subscribed session entry, got %s", entry)
	}
}
