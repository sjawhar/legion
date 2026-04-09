package session

import (
	"os"
	"testing"
	"time"
)

func TestParseRegistryMode_Default(t *testing.T) {
	os.Unsetenv("ENVOY_SESSION_REGISTRY")
	mode := ParseRegistryMode()
	if mode != RegistryModeKV {
		t.Fatalf("expected %q, got %q", RegistryModeKV, mode)
	}
}

func TestParseRegistryMode_KV(t *testing.T) {
	t.Setenv("ENVOY_SESSION_REGISTRY", "kv")
	mode := ParseRegistryMode()
	if mode != RegistryModeKV {
		t.Fatalf("expected %q, got %q", RegistryModeKV, mode)
	}
}

func TestParseRegistryMode_File(t *testing.T) {
	t.Setenv("ENVOY_SESSION_REGISTRY", "file")
	mode := ParseRegistryMode()
	if mode != RegistryModeFile {
		t.Fatalf("expected %q, got %q", RegistryModeFile, mode)
	}
}

func TestParseRegistryMode_CaseInsensitive(t *testing.T) {
	t.Setenv("ENVOY_SESSION_REGISTRY", "FILE")
	mode := ParseRegistryMode()
	if mode != RegistryModeFile {
		t.Fatalf("expected %q, got %q", RegistryModeFile, mode)
	}
}

func TestNewSessionLookup_KV(t *testing.T) {
	client := setupNATS(t)
	lookup, err := NewSessionLookup("kv", client.Conn, "", "machine", WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if _, ok := lookup.(*SessionRegistry); !ok {
		t.Fatalf("expected *SessionRegistry, got %T", lookup)
	}
}

func TestNewSessionLookup_File(t *testing.T) {
	dir := t.TempDir()
	lookup, err := NewSessionLookup("file", nil, dir, "machine")
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if _, ok := lookup.(*FileRegistry); !ok {
		t.Fatalf("expected *FileRegistry, got %T", lookup)
	}
}

func TestNewSessionLookup_EmptyDefaultsToKV(t *testing.T) {
	client := setupNATS(t)
	lookup, err := NewSessionLookup("", client.Conn, "", "machine", WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if _, ok := lookup.(*SessionRegistry); !ok {
		t.Fatalf("expected *SessionRegistry for empty mode, got %T", lookup)
	}
}

func TestNewSessionLookup_FileMissingDir(t *testing.T) {
	_, err := NewSessionLookup("file", nil, "", "machine")
	if err == nil {
		t.Fatal("expected error when dir is empty for file mode")
	}
}

func TestNewSessionLookup_KVMissingConn(t *testing.T) {
	_, err := NewSessionLookup("kv", nil, "", "machine")
	if err == nil {
		t.Fatal("expected error when conn is nil for kv mode")
	}
}

func TestNewSessionLookup_InvalidMode(t *testing.T) {
	_, err := NewSessionLookup("postgres", nil, "", "machine")
	if err == nil {
		t.Fatal("expected error for unknown mode")
	}
}
