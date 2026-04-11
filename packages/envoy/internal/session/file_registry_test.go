package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileRegistry_Get_LegacyFormat(t *testing.T) {
	dir := t.TempDir()
	raw := `{"pid":12345,"port":9999,"dir":"/test","session":{"id":"ses_abc","title":"test"}}`
	os.WriteFile(filepath.Join(dir, "ses_abc.json"), []byte(raw), 0644)
	reg := &FileRegistry{Dir: dir, MachineID: "local-machine"}
	entry, err := reg.Get("ses_abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entry.Port != 9999 {
		t.Fatalf("expected port 9999, got %d", entry.Port)
	}
	// Legacy format has no machine_id — FileRegistry stamps its own
	if entry.MachineID != "local-machine" {
		t.Fatalf("expected machine_id %q, got %q", "local-machine", entry.MachineID)
	}
}

func TestFileRegistry_Get_SessionEntryFormat(t *testing.T) {
	dir := t.TempDir()
	raw := `{"port":8080,"machine_id":"remote-box","dir":"/app","updated_at":1234567890}`
	os.WriteFile(filepath.Join(dir, "ses_xyz.json"), []byte(raw), 0644)
	reg := &FileRegistry{Dir: dir, MachineID: "local-machine"}
	entry, err := reg.Get("ses_xyz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entry.Port != 8080 {
		t.Fatalf("expected port 8080, got %d", entry.Port)
	}
	if entry.Dir != "/app" {
		t.Fatalf("expected dir /app, got %s", entry.Dir)
	}
	// SessionEntry format has its own machine_id — FileRegistry preserves it
	if entry.MachineID != "remote-box" {
		t.Fatalf("expected machine_id %q, got %q", "remote-box", entry.MachineID)
	}
}

func TestFileRegistry_Get_NotFound(t *testing.T) {
	dir := t.TempDir()
	reg := &FileRegistry{Dir: dir}
	_, err := reg.Get("ses_missing")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
}

func TestFileRegistry_Get_ZeroPort(t *testing.T) {
	dir := t.TempDir()
	raw := `{"port":0,"dir":"/test"}`
	os.WriteFile(filepath.Join(dir, "ses_zero.json"), []byte(raw), 0644)
	reg := &FileRegistry{Dir: dir}
	_, err := reg.Get("ses_zero")
	if err == nil {
		t.Fatal("expected error for zero port")
	}
}

func TestFileRegistry_PutAndGet(t *testing.T) {
	dir := t.TempDir()
	reg := &FileRegistry{Dir: dir, MachineID: "local"}
	err := reg.Put("ses_new", SessionEntry{Port: 5555, Dir: "/work", MachineID: "local"})
	if err != nil {
		t.Fatalf("put failed: %v", err)
	}
	entry, err := reg.Get("ses_new")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if entry.Port != 5555 {
		t.Fatalf("expected port 5555, got %d", entry.Port)
	}
	if entry.Dir != "/work" {
		t.Fatalf("expected dir /work, got %s", entry.Dir)
	}
	if entry.UpdatedAt == 0 {
		t.Fatal("expected non-zero updated_at")
	}
}

func TestFileRegistry_Delete(t *testing.T) {
	dir := t.TempDir()
	reg := &FileRegistry{Dir: dir}
	os.WriteFile(filepath.Join(dir, "ses_del.json"), []byte(`{"port":1}`), 0644)
	err := reg.Delete("ses_del")
	if err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	_, err = os.Stat(filepath.Join(dir, "ses_del.json"))
	if !os.IsNotExist(err) {
		t.Fatal("expected file to be deleted")
	}
}

func TestFileRegistry_DeleteNonexistent(t *testing.T) {
	dir := t.TempDir()
	reg := &FileRegistry{Dir: dir}
	err := reg.Delete("ses_nope")
	if err != nil {
		t.Fatalf("expected nil error for nonexistent delete, got: %v", err)
	}
}

func TestFileRegistry_Get_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ses_bad.json"), []byte(`not valid json`), 0644)
	reg := &FileRegistry{Dir: dir}
	_, err := reg.Get("ses_bad")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}
