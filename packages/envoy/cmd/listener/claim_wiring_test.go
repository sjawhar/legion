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

// envoy-client's machineID() (ENVOY_MACHINE_ID, falling back to the kernel hostname) names
// where the session process actually runs; it must reach the session entry as Host, kept
// separate from MachineID (always the registering listener's own machine, never the caller's).
func TestSessionEntryFromSubscribe_CarriesHost(t *testing.T) {
	got := sessionEntryFromSubscribe(subscribeBody{
		SessionID: "ses_z",
		Port:      1,
		Host:      "example-host-a",
	}, "example-listener")

	if got.Host != "example-host-a" {
		t.Fatalf("expected Host to be carried from the subscribe body, got %+v", got)
	}
	if got.MachineID != "example-listener" {
		t.Fatalf("expected MachineID to stay the registering listener's own machine, got %+v", got)
	}
}

func TestSessionEntryFromSubscribe_DefaultsHostEmpty(t *testing.T) {
	// An older plugin's subscribeBody has no "host" key; sessionEntryFromSubscribe must not
	// invent one — HostOrMachine is where the fallback to MachineID happens, at read time.
	got := sessionEntryFromSubscribe(subscribeBody{SessionID: "ses_old", Port: 1}, "example-listener")

	if got.Host != "" {
		t.Fatalf("expected Host to stay empty for a payload without the field, got %+v", got)
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
