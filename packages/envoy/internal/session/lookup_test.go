package session

import (
	"testing"
	"time"
)

func TestOpenSessionRegistry_ImplementsSessionLookup(t *testing.T) {
	client := setupNATS(t)
	lookup, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	var sessionLookup SessionLookup = lookup
	if _, ok := sessionLookup.(*SessionRegistry); !ok {
		t.Fatalf("expected *SessionRegistry implementing SessionLookup, got %T", sessionLookup)
	}
}
