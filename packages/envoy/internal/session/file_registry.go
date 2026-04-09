package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// FileRegistry resolves session ports from JSON files on a shared volume.
// Each session is stored as <Dir>/<sessionID>.json.
//
// This is the "file" mode of SessionLookup — for single-machine, local dev.
// The plugin writes files directly; the listener reads them.
type FileRegistry struct {
	Dir       string
	MachineID string // stamped on returned entries (file registry is local-only)
}

func (f *FileRegistry) Get(sessionID string) (SessionEntry, error) {
	path := filepath.Join(f.Dir, sessionID+".json")
	buf, err := os.ReadFile(path)
	if err != nil {
		return SessionEntry{}, fmt.Errorf("session %s: %w", sessionID, os.ErrNotExist)
	}
	// Unmarshal as SessionEntry. This also correctly reads the "port" and "dir"
	// fields from legacy RegistryEntry-format files (they share those JSON keys).
	var entry SessionEntry
	if err := json.Unmarshal(buf, &entry); err != nil {
		return SessionEntry{}, fmt.Errorf("session %s: invalid json: %w", sessionID, err)
	}
	if entry.Port == 0 {
		return SessionEntry{}, fmt.Errorf("session %s: no port in registry file", sessionID)
	}
	// File registry is local-only — stamp the local machine ID if absent.
	if entry.MachineID == "" {
		entry.MachineID = f.MachineID
	}
	return entry, nil
}

func (f *FileRegistry) Put(sessionID string, entry SessionEntry) error {
	entry.UpdatedAt = time.Now().UnixMilli()
	buf, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	path := filepath.Join(f.Dir, sessionID+".json")
	return os.WriteFile(path, buf, 0644)
}

func (f *FileRegistry) Delete(sessionID string) error {
	path := filepath.Join(f.Dir, sessionID+".json")
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil // already gone
	}
	return err
}
