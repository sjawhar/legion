# Restore FileRegistry and SessionLookup Pluggable Interface

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Restore the pluggable `SessionLookup` interface (FileRegistry + NewSessionLookup factory + ENVOY_SESSION_REGISTRY mode switch) that PR #393 over-scoped and removed.

**Architecture:** PR #375 introduced a pluggable `SessionLookup` interface with two implementations: `SessionRegistry` (NATS KV, production) and `FileRegistry` (filesystem, local dev). PR #393 correctly stripped the dual-registry fallback but incorrectly deleted the entire pluggable interface. This plan restores only the pluggable interface — not the fallback. All code to restore exists verbatim on current main (from PR #375).

**Tech Stack:** Go, NATS JetStream KV, filesystem JSON

**Prerequisite:** PR #393 must be merged to main first (or the implementer rebases on branch `sjawhar-legion-373`). The plan assumes the post-#393 state as the starting point.

**Commit strategy:** All work happens in a single jj change. Do NOT create intermediate commits. After all tasks pass verification, describe once with the final message.

---

## File Structure

| File | Action | Responsibility |
|------|--------|---------------|
| `packages/envoy/internal/session/file_registry.go` | **Create** (restore) | FileRegistry struct — filesystem-backed SessionLookup for local dev |
| `packages/envoy/internal/session/file_registry_test.go` | **Create** (restore) | Unit tests for FileRegistry Get/Put/Delete |
| `packages/envoy/internal/session/lookup.go` | **Modify** | Add back RegistryMode constants, NewSessionLookup factory, ParseRegistryMode |
| `packages/envoy/internal/session/lookup_test.go` | **Modify** | Add back ParseRegistryMode and NewSessionLookup tests |
| `packages/envoy/cmd/listener/main.go` | **Modify** | Restore SessionLookup interface usage and ENVOY_SESSION_REGISTRY wiring |

**Files NOT changed (intentionally):**
- `session.go` — RegistryEntry struct was dead code, correctly removed by #393
- `session_test.go` — KV-based Deliverer tests are correct; no need to restore file-based test helpers
- `packages/envoy-plugin/` — plugin file-writing correctly removed by #393
- `packages/envoy/infra/` — ENVOY_REGISTRY_DIR correctly removed from Pulumi (prod uses KV)

---

### Task 1: Restore FileRegistry implementation — Independent

**Files:**
- Create: `packages/envoy/internal/session/file_registry.go`

- [ ] **Step 1: Create file_registry.go**

Create the file with the exact implementation from PR #375:

```go
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
```

- [ ] **Step 2: Verify file compiles**

Run: `cd packages/envoy && go build ./internal/session/`
Expected: No errors. FileRegistry implements SessionLookup interface (Get/Put/Delete match).
---

### Task 2: Restore FileRegistry tests — Depends on: Task 1

**Files:**
- Create: `packages/envoy/internal/session/file_registry_test.go`

- [ ] **Step 1: Create file_registry_test.go**

```go
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
```

- [ ] **Step 2: Run FileRegistry tests**

Run: `cd packages/envoy && go test ./internal/session/ -run TestFileRegistry -v`
Expected: All 7 tests pass. These tests do NOT require NATS (filesystem-only).
---

### Task 3: Restore lookup.go factory and mode switch — Depends on: Task 1

**Files:**
- Modify: `packages/envoy/internal/session/lookup.go`

After PR #393, lookup.go contains only the bare SessionLookup interface (8 lines). Restore the imports, constants, factory, and mode parser.

- [ ] **Step 1: Replace lookup.go with full implementation**

The file should contain exactly:

```go
package session

import (
	"fmt"
	"os"
	"strings"

	"github.com/nats-io/nats.go"
)

// SessionLookup abstracts session port/machine resolution.
// Exactly one implementation is active at runtime — never both.
type SessionLookup interface {
	Get(sessionID string) (SessionEntry, error)
	Put(sessionID string, entry SessionEntry) error
	Delete(sessionID string) error
}

// RegistryMode selects the session registry implementation.
const (
	RegistryModeKV   = "kv"
	RegistryModeFile = "file"
)

// NewSessionLookup creates the appropriate SessionLookup based on mode.
//
//   - "kv" (default): KV-backed registry via NATS JetStream.
//   - "file": file-backed registry reading from dir.
//
// When KV is selected, dir is unused. When file is selected, conn is unused.
// Never both at once — eliminates the dual-registry disagreement.
func NewSessionLookup(mode string, conn *nats.Conn, dir string, machineID string, opts ...SessionRegistryOption) (SessionLookup, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case RegistryModeFile:
		if dir == "" {
			return nil, fmt.Errorf("ENVOY_REGISTRY_DIR is required when ENVOY_SESSION_REGISTRY=file")
		}
		return &FileRegistry{Dir: dir, MachineID: machineID}, nil
	case RegistryModeKV, "":
		if conn == nil {
			return nil, fmt.Errorf("NATS connection is required when ENVOY_SESSION_REGISTRY=kv")
		}
		return OpenSessionRegistry(conn, opts...)
	default:
		return nil, fmt.Errorf("unknown ENVOY_SESSION_REGISTRY value: %q (expected \"kv\" or \"file\")", mode)
	}
}

// ParseRegistryMode reads ENVOY_SESSION_REGISTRY from the environment.
// Returns "kv" when unset or empty (the default).
func ParseRegistryMode() string {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("ENVOY_SESSION_REGISTRY")))
	if mode == "" {
		return RegistryModeKV
	}
	return mode
}
```

- [ ] **Step 2: Verify compilation**

Run: `cd packages/envoy && go build ./internal/session/`
Expected: No errors.
---

### Task 4: Restore lookup_test.go factory/mode tests — Depends on: Task 1, Task 3

**Files:**
- Modify: `packages/envoy/internal/session/lookup_test.go`

After PR #393, lookup_test.go has only a single `TestOpenSessionRegistry_ImplementsSessionLookup` test. Add back the ParseRegistryMode and NewSessionLookup tests while keeping the existing test.

- [ ] **Step 1: Replace lookup_test.go with full test suite**

The file should contain:

```go
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
```

Note: `setupNATS(t)` is defined in `registry_test.go` (shared test helper using NATS testcontainers, introduced by PR #393).

- [ ] **Step 2: Run all lookup tests**

Run: `cd packages/envoy && go test ./internal/session/ -run "TestParseRegistryMode|TestNewSessionLookup|TestOpenSessionRegistry" -v`
Expected: All 11 tests pass. ParseRegistryMode tests are filesystem-only; NewSessionLookup_KV tests require NATS container (shared via `setupNATS`).
---

### Task 5: Restore listener main.go wiring — Depends on: Task 1, Task 3

**Files:**
- Modify: `packages/envoy/cmd/listener/main.go`

Three changes:
1. Change `listenerDeps.sessions` type from `*session.SessionRegistry` back to `session.SessionLookup`
2. Restore type assertion for `/v1/sessions` handler (FileRegistry has no List())
3. Replace `OpenSessionRegistry` call with `ParseRegistryMode` + `NewSessionLookup`

- [ ] **Step 1: Update listenerDeps struct**

In the `listenerDeps` struct (around line 29-33), change:
```go
// FROM (PR #393 state):
sessions *session.SessionRegistry
// TO:
sessions session.SessionLookup
```

- [ ] **Step 2: Restore /v1/sessions type assertion**

In the `/v1/sessions` handler (around line 321-326), change:
```go
// FROM (PR #393 state):
v1.HandleFunc("/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
    d := deps.Load()
    sessionsHandler(d.registry, d.sessions).ServeHTTP(w, r)
})
// TO:
v1.HandleFunc("/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
    d := deps.Load()
    reg, _ := d.sessions.(*session.SessionRegistry)
    sessionsHandler(d.registry, reg).ServeHTTP(w, r)
})
```

This is safe: when mode=file, the assertion fails silently, `reg` is nil, and `sessionsHandler` returns 503 ("session registry unavailable"). Production (mode=kv) is unaffected.

- [ ] **Step 3: Restore ParseRegistryMode + NewSessionLookup in main()**

Replace the direct `OpenSessionRegistry` call (around line 412-420) with the mode-switched factory:

```go
// FROM (PR #393 state):
sessions, err := session.OpenSessionRegistry(
    client.Conn,
    session.WithSessionReplicas(cfg.NATSReplicas),
)
if err != nil {
    log.Fatal(err)
}

// TO:
registryMode := session.ParseRegistryMode()
sessions, err := session.NewSessionLookup(
    registryMode,
    client.Conn,
    os.Getenv("ENVOY_REGISTRY_DIR"),
    cfg.MachineID,
    session.WithSessionReplicas(cfg.NATSReplicas),
)
if err != nil {
    log.Fatal(err)
}
log.Printf("session registry mode=%s", registryMode)
```

Ensure `"os"` is in the import list (it should already be present for other `os.Getenv` calls).

- [ ] **Step 4: Verify compilation**

Run: `cd packages/envoy && go build ./cmd/listener/`
Expected: No errors.
---

### Task 6: Run full test suite and verify — Depends on: Task 1, Task 2, Task 3, Task 4, Task 5

- [ ] **Step 1: Run all session package tests**

Run: `cd packages/envoy && go test ./internal/session/ -v -count=1`
Expected: All tests pass, including:
- FileRegistry tests (7 tests, no NATS required)
- ParseRegistryMode tests (4 tests, no NATS required)
- NewSessionLookup tests (6 tests, some require NATS)
- OpenSessionRegistry tests (1 test, requires NATS)
- SessionRegistry KV tests (existing, require NATS)
- Handler tests (existing, require NATS)
- Deliverer tests (existing KV-backed tests from PR #393)

- [ ] **Step 2: Run listener package tests**

Run: `cd packages/envoy && go test ./cmd/listener/ -v -count=1`
Expected: All listener tests pass. The shared NATS container pattern from PR #393 is preserved.

- [ ] **Step 3: Run full envoy package tests**

Run: `cd packages/envoy && go test ./... -count=1`
Expected: All tests pass across all envoy subpackages.

- [ ] **Step 4: Run biome lint check**

Run: `cd packages/envoy && go vet ./...`
Expected: No issues.

- [ ] **Step 5: Describe the final commit**

All work is in a single jj change. Describe it now:

```bash
jj describe -m "fix(envoy): restore FileRegistry and SessionLookup pluggable interface

PR #393 correctly stripped the dual-registry fallback but over-scoped
and removed the pluggable SessionLookup interface entirely. This
restores:

- FileRegistry implementation (filesystem-backed, for local dev)
- NewSessionLookup factory function (mode switch)
- ParseRegistryMode (reads ENVOY_SESSION_REGISTRY env var)
- ENVOY_SESSION_REGISTRY wiring in listener main()
- Full test coverage for all restored code

The KV-only default for production is unchanged. Single-machine setups
can use ENVOY_SESSION_REGISTRY=file to avoid NATS dependency for
session tracking.

Closes #399"
```
---

## Testing Plan

### Setup
- `cd packages/envoy && go mod download`
- Docker must be running (NATS testcontainers require it)

### Health Check
- `docker ps` — Docker daemon running
- `go build ./...` — all packages compile

### Verification Steps

1. **FileRegistry unit tests**
   - Action: `go test ./internal/session/ -run TestFileRegistry -v`
   - Expected: 7 tests pass (no NATS needed)
   - Tool: CLI

2. **ParseRegistryMode tests**
   - Action: `go test ./internal/session/ -run TestParseRegistryMode -v`
   - Expected: 4 tests pass (no NATS needed)
   - Tool: CLI

3. **NewSessionLookup factory tests**
   - Action: `go test ./internal/session/ -run TestNewSessionLookup -v`
   - Expected: 6 tests pass (3 require NATS container, 3 are unit tests)
   - Tool: CLI

4. **Full session package**
   - Action: `go test ./internal/session/ -v -count=1`
   - Expected: All tests pass
   - Tool: CLI

5. **Full envoy package**
   - Action: `go test ./... -count=1`
   - Expected: All tests pass
   - Tool: CLI

### Skills to Invoke
- No project-specific testing skills identified beyond standard Go test runner.

### Tools Needed
- Go test runner
- Docker (for NATS testcontainers)
- `go vet` for static analysis
