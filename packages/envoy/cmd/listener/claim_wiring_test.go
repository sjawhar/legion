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
// where the session process actually runs, which is the address a listener reaches the session's
// serving port at when no host bridge of its own machine applies. It must reach the session entry
// as Host, kept separate from MachineID (always the registering listener's own machine, never the
// caller's), which decides which listener fans a topic out to the session.
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
	// invent one: an empty Host is what sends the owning listener's delivery (the only one; no
	// other listener delivers to the session) to its fallback, host.docker.internal, and display
	// to MachineID (HostOrMachine).
	got := sessionEntryFromSubscribe(subscribeBody{SessionID: "ses_old", Port: 1}, "example-listener")

	if got.Host != "" {
		t.Fatalf("expected Host to stay empty for a payload without the field, got %+v", got)
	}
}

// The plugin a session registers through and that plugin's build reach the session entry, with
// the surrounding space a session id loses dropped too.
func TestSessionEntryFromSubscribe_CarriesThePluginAndItsVersionTrimmed(t *testing.T) {
	got := sessionEntryFromSubscribe(subscribeBody{
		SessionID:     "ses_plugin",
		Plugin:        " @sjawhar/pi-envoy\n",
		PluginVersion: "\t11.0.0 ",
	}, "example-listener")

	if got.Plugin != "@sjawhar/pi-envoy" || got.PluginVersion != "11.0.0" {
		t.Fatalf("expected plugin @sjawhar/pi-envoy at 11.0.0, got %+v", got)
	}
}

func TestSessionEntryFromSubscribe_DefaultsThePluginAndItsVersionEmpty(t *testing.T) {
	// A plugin predating the fields sends neither key; the entry invents neither.
	got := sessionEntryFromSubscribe(subscribeBody{SessionID: "ses_old"}, "example-listener")

	if got.Plugin != "" || got.PluginVersion != "" {
		t.Fatalf("expected no plugin for a payload without the fields, got %+v", got)
	}
}

// The client instance id reaches the session entry, where a soft claim naming a predecessor
// session reads it to tell that predecessor's own instance from another instance's live session.
func TestSessionEntryFromSubscribe_CarriesTheClientInstanceID(t *testing.T) {
	got := sessionEntryFromSubscribe(subscribeBody{
		SessionID:        "ses_instance",
		Port:             42145,
		ClientInstanceID: "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a",
	}, "example-listener")

	if got.ClientInstanceID != "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a" {
		t.Fatalf("expected the client instance id to be carried from the subscribe body, got %+v", got)
	}
}

// The id is compared as stored (session.InstanceMatches trims nothing), so the surrounding space
// a session id loses is dropped here, where the wire value enters the entry.
func TestSessionEntryFromSubscribe_TrimsTheClientInstanceID(t *testing.T) {
	got := sessionEntryFromSubscribe(subscribeBody{
		SessionID:        "ses_instance_spaced",
		ClientInstanceID: " \t4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a\n",
	}, "example-listener")

	if got.ClientInstanceID != "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a" {
		t.Fatalf("expected the client instance id to be trimmed, got %q", got.ClientInstanceID)
	}
}

func TestSessionEntryFromSubscribe_DefaultsTheClientInstanceIDEmpty(t *testing.T) {
	// A client predating the field sends no key; the entry invents none, and the empty id is
	// what keeps the old soft-claim rule for it (session.InstanceMatches). The same empty id is
	// written over any id a previous registration stored, since the entry is built fresh from
	// the body.
	got := sessionEntryFromSubscribe(subscribeBody{SessionID: "ses_old"}, "example-listener")

	if got.ClientInstanceID != "" {
		t.Fatalf("expected no client instance id for a payload without the field, got %+v", got)
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
