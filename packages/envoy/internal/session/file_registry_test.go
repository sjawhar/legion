package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFileRegistry_Get_LegacyFormat(t *testing.T) {
	dir := t.TempDir()
	writeRegistryEntry(t, dir, 999, 12345, "ses_direct")

	reg := &FileRegistry{Dir: dir, MachineID: "local-machine"}
	entry, err := reg.Get("ses_direct")
	if err != nil {
		t.Fatalf("expected to find session, got: %v", err)
	}
	if entry.Port != 12345 {
		t.Fatalf("expected port 12345, got %d", entry.Port)
	}
	// Legacy format has no machine_id — FileRegistry stamps its own
	if entry.MachineID != "local-machine" {
		t.Fatalf("expected machine_id 'local-machine', got %q", entry.MachineID)
	}
}

func TestFileRegistry_Get_SessionEntryFormat(t *testing.T) {
	dir := t.TempDir()
	data, _ := json.Marshal(SessionEntry{Port: 9999, MachineID: "my-machine", Dir: "/app"})
	if err := os.WriteFile(filepath.Join(dir, "ses_new.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	reg := &FileRegistry{Dir: dir, MachineID: "local-machine"}
	entry, err := reg.Get("ses_new")
	if err != nil {
		t.Fatalf("expected to find session, got: %v", err)
	}
	if entry.Port != 9999 {
		t.Fatalf("expected port 9999, got %d", entry.Port)
	}
	// SessionEntry format has its own MachineID — don't override
	if entry.MachineID != "my-machine" {
		t.Fatalf("expected machine_id 'my-machine', got %q", entry.MachineID)
	}
}

func TestFileRegistry_Get_NotFound(t *testing.T) {
	dir := t.TempDir()
	reg := &FileRegistry{Dir: dir}
	_, err := reg.Get("ses_nonexistent")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
}

func TestFileRegistry_Get_ZeroPort(t *testing.T) {
	dir := t.TempDir()
	data, _ := json.Marshal(SessionEntry{Port: 0, MachineID: "m", Dir: "/app"})
	os.WriteFile(filepath.Join(dir, "ses_zero.json"), data, 0644)

	reg := &FileRegistry{Dir: dir}
	_, err := reg.Get("ses_zero")
	if err == nil {
		t.Fatal("expected error for zero port")
	}
}

func TestFileRegistry_PutAndGet(t *testing.T) {
	dir := t.TempDir()
	reg := &FileRegistry{Dir: dir, MachineID: "local"}

	entry := SessionEntry{Port: 13381, MachineID: "test-machine", Dir: "/test"}
	if err := reg.Put("ses_roundtrip", entry); err != nil {
		t.Fatalf("put failed: %v", err)
	}

	got, err := reg.Get("ses_roundtrip")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if got.Port != 13381 {
		t.Fatalf("expected port 13381, got %d", got.Port)
	}
	if got.MachineID != "test-machine" {
		t.Fatalf("expected machine_id 'test-machine', got %q", got.MachineID)
	}
	if got.UpdatedAt == 0 {
		t.Fatal("expected non-zero UpdatedAt")
	}
}

func TestFileRegistry_Delete(t *testing.T) {
	dir := t.TempDir()
	reg := &FileRegistry{Dir: dir}

	reg.Put("ses_del", SessionEntry{Port: 1234, MachineID: "m", Dir: "/test"})

	if err := reg.Delete("ses_del"); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	_, err := reg.Get("ses_del")
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestFileRegistry_DeleteNonexistent(t *testing.T) {
	dir := t.TempDir()
	reg := &FileRegistry{Dir: dir}

	// Deleting a non-existent file should not error
	if err := reg.Delete("ses_ghost"); err != nil {
		t.Fatalf("expected nil error for deleting non-existent, got: %v", err)
	}
}
