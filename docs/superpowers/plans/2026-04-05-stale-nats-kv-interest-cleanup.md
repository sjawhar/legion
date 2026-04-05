# Stale NATS KV Interest Cleanup — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prevent stale `envoy_interests` KV entries from causing failed deliveries and NAK retry loops by adding admin cleanup endpoints for observability and emergency removal, plus automatic daemon-side unsubscribe when workers are deleted.

**Architecture:** Defense-in-depth with 2 layers: (1) admin list+delete HTTP endpoints on the listener for observability and manual cleanup, (2) fire-and-forget envoy unsubscribe when daemon removes workers via `DELETE /workers/:id`. The `envoy_interests` bucket intentionally has **no TTL** — interests are permanent by design (see `docs/solutions/architecture-patterns/nats-kv-dual-bucket-lifecycle.md`). Cleanup is explicit, not time-based.

**Tech Stack:** Go (envoy listener, NATS KV), TypeScript/Bun (daemon server), testcontainers (Go integration tests), Bun test runner (TS tests).

**Design constraint (non-negotiable):** The dual-bucket lifecycle is load-bearing. `envoy_interests` = permanent, cached subscriptions. `envoy_sessions` = ephemeral, TTL-backed liveness. Do NOT add TTL to the interests bucket. Do NOT merge the buckets. Stale interest cleanup is handled by explicit removal (admin endpoint or daemon unsubscribe), never by automatic expiry.

**Working directories for commands:**
- Go tests and builds: run from `packages/envoy/` (where `go.mod` lives)
- Daemon tests and type checks: run from repo root
- Biome lint: `bunx biome check packages/daemon/src/` from repo root

**Key patterns from codebase (follow these exactly):**
- `packages/envoy/internal/store/kv.go`: `Registry` struct with cache, `Remove(sessionID, nil)` deletes entire entry via `kv.Delete()`
- `packages/envoy/cmd/listener/main.go`: inline handler closures with `deps.Load()`, extract to named functions for testability (see `publishHandler` pattern)
- `packages/envoy/cmd/listener/main_test.go`: same-package handler tests using `httptest`, `atomic.Pointer[listenerDeps]`, table-driven cases
- `packages/daemon/src/daemon/server.ts:141-167`: `subscribeWorkerToEnvoy()` fire-and-forget pattern → copy for unsubscribe
- `packages/daemon/src/daemon/__tests__/server.test.ts:1336-1502`: `describe("Envoy worker auto-subscribe")` block with `EnvoySubscribeCall` interface, `mockFetchForEnvoy()`, `repoPaths`, `repoManagerDeps` — new unsubscribe tests go inside this block
- `packages/envoy/internal/integration/delivery_test.go`: `setupTestEnv()` with variadic options, `registerInterest()` / `registerKVSession()` helpers

**Relevant learnings from prior work (preloaded from docs/solutions/index.json):**

1. [docs/solutions/architecture-patterns/nats-kv-dual-bucket-lifecycle.md]: [NATS KV dual-bucket lifecycle | tags: nats, kv, ttl, envoy] Interests = permanent, sessions = ephemeral. Do NOT add TTL to interests or cache to sessions.
2. [docs/solutions/testing/nats-kv-testing-patterns.md]: [Testing NATS KV with real containers | tags: nats, kv, testcontainers] Real NATS via testcontainers. Functional options for test configurability. Decomposed setup helpers.
3. [docs/solutions/testing/bun-fetch-mocking-patterns.md]: [Bun Fetch Mocking Patterns | tags: bun, fetch, mocking, fire-and-forget] Object.assign with preconnect for globalThis.fetch mock. Capture array + Bun.sleep(50) for fire-and-forget.
4. [docs/solutions/daemon/envoy-auto-subscription-patterns.md]: [Envoy Auto-Subscription Patterns | tags: envoy, fire-and-forget, subscription] Fire-and-forget: void return, .catch(() => {}), place after persistState().
5. [docs/solutions/envoy/contracts-and-handler-patterns.md]: [Contracts generation pipeline and listener handler testability | tags: contracts, handler-extraction, testing] Extract handlers as named functions taking deps for testability. Table-driven tests. Validation before deps access.

---

## File Structure

| File | Action | Responsibility |
|------|--------|---------------|
| `packages/envoy/internal/store/kv.go` | Modify | Add `List() []Interest` method |
| `packages/envoy/cmd/listener/main.go` | Modify | Replace `/v1/interests/` handler with method-routed version: GET list, GET by session, DELETE by session |
| `packages/envoy/cmd/listener/main_test.go` | Modify | Add handler tests for admin interests endpoint (same package as handler) |
| `packages/envoy/internal/integration/delivery_test.go` | Modify | Add `Registry.List()` integration test |
| `packages/daemon/src/daemon/server.ts` | Modify | Add `unsubscribeWorkerFromEnvoy()`, call from DELETE /workers handler |
| `packages/daemon/src/daemon/__tests__/server.test.ts` | Modify | Extend `mockFetchForEnvoy` to capture unsubscribe calls, add DELETE-triggers-unsubscribe tests inside existing `describe("Envoy worker auto-subscribe")` block |

---

### Task 1: Registry.List Method + Admin Endpoints (Go) — Independent

**Files:**
- Modify: `packages/envoy/internal/store/kv.go` (add List method after Match, ~line 181)
- Modify: `packages/envoy/cmd/listener/main.go:213-223` (replace inline handler with extracted function)
- Modify: `packages/envoy/cmd/listener/main_test.go` (add handler tests — same `package main`)
- Modify: `packages/envoy/internal/integration/delivery_test.go` (add `List()` integration test)

- [ ] **Step 1: Write failing integration test for `Registry.List()`**

Add to `packages/envoy/internal/integration/delivery_test.go`:

```go
func TestE2E_RegistryListReturnsSortedInterests(t *testing.T) {
	env := setupTestEnv(t)

	env.registerInterest("ses_charlie", []string{"notifications.test.>"})
	env.registerInterest("ses_alpha", []string{"notifications.test.>"})
	env.registerInterest("ses_bravo", []string{"notifications.test.>"})

	// Allow watcher to propagate Upsert events to cache
	time.Sleep(500 * time.Millisecond)

	items := env.registry.List()
	if len(items) != 3 {
		t.Fatalf("expected 3 interests, got %d", len(items))
	}
	if items[0].SessionID != "ses_alpha" {
		t.Fatalf("expected first item to be ses_alpha, got %s", items[0].SessionID)
	}
	if items[1].SessionID != "ses_bravo" {
		t.Fatalf("expected second item to be ses_bravo, got %s", items[1].SessionID)
	}
	if items[2].SessionID != "ses_charlie" {
		t.Fatalf("expected third item to be ses_charlie, got %s", items[2].SessionID)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run (from `packages/envoy/`): `go test ./internal/integration/ -run TestE2E_RegistryListReturnsSortedInterests -v -timeout 30s`
Expected: FAIL — `List()` method does not exist on `*store.Registry`.

- [ ] **Step 3: Implement `List()` method on Registry**

Add to `packages/envoy/internal/store/kv.go`, after the `Match` method (after line 181):

```go
// List returns all cached interests sorted by SessionID for deterministic output.
func (r *Registry) List() []Interest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Interest, 0, len(r.cache))
	for _, item := range r.cache {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].SessionID < out[j].SessionID
	})
	return out
}
```

Note: `"sort"` is already imported in `kv.go`.

- [ ] **Step 4: Run List test to verify it passes**

Run (from `packages/envoy/`): `go test ./internal/integration/ -run TestE2E_RegistryListReturnsSortedInterests -v -timeout 30s`
Expected: PASS.

- [ ] **Step 5: Extract the admin interests handler as a named function in main.go**

In `packages/envoy/cmd/listener/main.go`:

Add `"errors"` to the import block (after `"encoding/json"`, line 4).

Add the extracted handler function **before** `main()` (after `publishHandler`, around line 93). This follows the established `publishHandler` extraction pattern from `contracts-and-handler-patterns.md`:

```go
// adminInterestsHandler handles GET (list all / get by session) and DELETE (remove by session)
// on the /v1/interests/ path.
func adminInterestsHandler(registry *store.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := strings.TrimPrefix(r.URL.Path, "/v1/interests/")

		if sessionID == "" {
			// /v1/interests/ with no session ID → list all
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			items := registry.List()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(items)
			return
		}

		switch r.Method {
		case http.MethodGet:
			item, err := registry.Get(sessionID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(item)
		case http.MethodDelete:
			if err := registry.Remove(sessionID, nil); err != nil {
				// Idempotent: not-found is not an error for admin cleanup
				if !errors.Is(err, nats.ErrKeyNotFound) {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}
```

The `nats` import (`"github.com/nats-io/nats.go"`) is already present.

Then replace the existing inline handler (lines 213-223) with:

```go
	v1.HandleFunc("/v1/interests/", func(w http.ResponseWriter, r *http.Request) {
		d := deps.Load()
		adminInterestsHandler(d.registry).ServeHTTP(w, r)
	})
```

- [ ] **Step 6: Write handler tests in main_test.go**

Add to `packages/envoy/cmd/listener/main_test.go`. These are same-package tests that can access `adminInterestsHandler` directly. They use a real NATS+Registry via testcontainers:

Add these imports to the import block (merge with existing):
```go
import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)
```

Add the test helper and tests:

```go
// setupAdminTestRegistry creates a real NATS-backed interest registry for
// handler tests. Returns the registry pre-loaded with the given interests.
func setupAdminTestRegistry(t *testing.T, interests map[string][]string) *store.Registry {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcnats.Run(ctx, "nats:2.10")
	if err != nil {
		t.Fatalf("failed to start NATS: %v", err)
	}
	t.Cleanup(func() { ctr.Terminate(ctx) })
	uri, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("failed to get NATS URI: %v", err)
	}
	client, err := bus.Connect([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("failed to connect bus: %v", err)
	}
	t.Cleanup(func() { client.Conn.Close() })
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}
	for sessionID, topics := range interests {
		allTopics := append([]string{contracts.AgentSubject(sessionID)}, topics...)
		if _, err := registry.Upsert(store.Interest{
			SessionID: sessionID,
			MachineID: "test-machine",
			Dir:       "/test",
		}, allTopics); err != nil {
			t.Fatalf("failed to upsert interest for %s: %v", sessionID, err)
		}
	}
	// Allow watcher to propagate all Upsert events to cache
	time.Sleep(500 * time.Millisecond)
	return registry
}

func TestAdminInterestsHandler_ListAll(t *testing.T) {
	registry := setupAdminTestRegistry(t, map[string][]string{
		"ses_list_b": {"notifications.test.>"},
		"ses_list_a": {"notifications.github.>"},
	})
	handler := adminInterestsHandler(registry)

	req := httptest.NewRequest(http.MethodGet, "/v1/interests/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var items []store.Interest
	if err := json.NewDecoder(rec.Body).Decode(&items); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 interests, got %d", len(items))
	}
	if items[0].SessionID != "ses_list_a" {
		t.Fatalf("expected first item ses_list_a, got %s", items[0].SessionID)
	}
}

func TestAdminInterestsHandler_GetBySession(t *testing.T) {
	registry := setupAdminTestRegistry(t, map[string][]string{
		"ses_get_test": {"notifications.test.>"},
	})
	handler := adminInterestsHandler(registry)

	req := httptest.NewRequest(http.MethodGet, "/v1/interests/ses_get_test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var item store.Interest
	if err := json.NewDecoder(rec.Body).Decode(&item); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if item.SessionID != "ses_get_test" {
		t.Fatalf("expected ses_get_test, got %s", item.SessionID)
	}
}

func TestAdminInterestsHandler_DeleteBySession(t *testing.T) {
	registry := setupAdminTestRegistry(t, map[string][]string{
		"ses_delete_target": {"notifications.test.>"},
	})
	handler := adminInterestsHandler(registry)

	req := httptest.NewRequest(http.MethodDelete, "/v1/interests/ses_delete_target", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Allow watcher to propagate delete event to cache
	time.Sleep(500 * time.Millisecond)
	if _, err := registry.Get("ses_delete_target"); err == nil {
		t.Fatal("expected interest to be deleted")
	}
}

func TestAdminInterestsHandler_DeleteIdempotent(t *testing.T) {
	registry := setupAdminTestRegistry(t, nil)
	handler := adminInterestsHandler(registry)

	req := httptest.NewRequest(http.MethodDelete, "/v1/interests/ses_nonexistent", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 for non-existent session, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdminInterestsHandler_MethodNotAllowed(t *testing.T) {
	registry := setupAdminTestRegistry(t, nil)
	handler := adminInterestsHandler(registry)

	// POST to /v1/interests/ is not allowed
	req := httptest.NewRequest(http.MethodPost, "/v1/interests/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}
```

- [ ] **Step 7: Run handler tests to verify they pass**

Run (from `packages/envoy/`): `go test ./cmd/listener/ -run "TestAdminInterestsHandler" -v -timeout 60s`
Expected: PASS (all 5 handler tests).

- [ ] **Step 8: Run existing tests to verify no regression**

Run (from `packages/envoy/`): `go test ./... -timeout 120s`
Expected: All existing tests pass. The handler extraction preserves existing GET-by-session behavior.

- [ ] **Step 9: Describe and advance**

```bash
jj describe -m "feat(envoy): add admin list and delete endpoints for interest cleanup"
jj new
```

---

### Task 2: Daemon Worker Exit Cleanup (TypeScript) — Independent

**Files:**
- Modify: `packages/daemon/src/daemon/server.ts` (~line 167 for function, ~line 705 for DELETE handler)
- Modify: `packages/daemon/src/daemon/__tests__/server.test.ts` (~line 1346 for mock, ~line 1500 for new tests)

- [ ] **Step 1: Extend `mockFetchForEnvoy` to capture unsubscribe calls**

In `packages/daemon/src/daemon/__tests__/server.test.ts`, find the `mockFetchForEnvoy` function inside the `describe("Envoy worker auto-subscribe")` block (line 1342). Modify the URL check on line 1346 to also intercept unsubscribe calls:

Change:
```typescript
        if (url.includes("/v1/interests/subscribe")) {
```
to:
```typescript
        if (url.includes("/v1/interests/subscribe") || url.includes("/v1/interests/unsubscribe")) {
```

This allows the existing capture array to record both subscribe and unsubscribe calls.

- [ ] **Step 2: Write failing tests for DELETE /workers triggering envoy unsubscribe**

Add these tests **inside** the existing `describe("Envoy worker auto-subscribe")` block (after the last `it(...)` around line 1519), so they have access to `EnvoySubscribeCall`, `mockFetchForEnvoy`, `repoPaths`, and `repoManagerDeps`:

```typescript
    it("unsubscribes worker from envoy on delete", async () => {
      const envoySubscribeCalls: EnvoySubscribeCall[] = [];
      mockFetchForEnvoy(envoySubscribeCalls);

      await startTestServer({ paths: repoPaths, repoManagerDeps });

      // Create a worker with repo (not workspace) to trigger envoy subscribe path
      const createResponse = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-250",
          mode: "implement",
          repo: "acme/widgets",
          issueNumber: 250,
        }),
      });
      expect(createResponse.status).toBe(200);
      const created = (await createResponse.json()) as { id: string; sessionId: string };
      await Bun.sleep(50);

      // Clear subscribe calls from worker creation
      envoySubscribeCalls.length = 0;

      // Delete the worker
      const deleteResponse = await requestJson(`/workers/${created.id}`, { method: "DELETE" });
      expect(deleteResponse.status).toBe(200);

      // Flush fire-and-forget microtasks
      await Bun.sleep(50);

      // Verify unsubscribe was called
      const unsubCalls = envoySubscribeCalls.filter((c) =>
        c.url.includes("/v1/interests/unsubscribe"),
      );
      expect(unsubCalls).toHaveLength(1);
      expect(unsubCalls[0].body).toEqual({
        session_id: created.sessionId,
        topics: [],
      });
    });

    it("delete succeeds even when envoy unsubscribe fails", async () => {
      const envoySubscribeCalls: EnvoySubscribeCall[] = [];
      mockFetchForEnvoy(envoySubscribeCalls, 500);

      await startTestServer({ paths: repoPaths, repoManagerDeps });

      const createResponse = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-251",
          mode: "implement",
          repo: "acme/widgets",
          issueNumber: 251,
        }),
      });
      expect(createResponse.status).toBe(200);
      const created = (await createResponse.json()) as { id: string };

      const deleteResponse = await requestJson(`/workers/${created.id}`, { method: "DELETE" });
      expect(deleteResponse.status).toBe(200);
      expect(await deleteResponse.json()).toEqual({ status: "stopped" });
    });
```

- [ ] **Step 3: Run test to verify it fails**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts -t "unsubscribes worker from envoy on delete"`
Expected: FAIL — no unsubscribe call is made by the DELETE handler.

- [ ] **Step 4: Add `unsubscribeWorkerFromEnvoy` to server.ts**

In `packages/daemon/src/daemon/server.ts`, add this function after `subscribeWorkerToEnvoy` (after ~line 167):

```typescript
function unsubscribeWorkerFromEnvoy(sessionId: string): void {
  const envoyUrl = process.env.ENVOY_URL ?? "http://127.0.0.1:9020";
  fetch(`${envoyUrl}/v1/interests/unsubscribe`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ session_id: sessionId, topics: [] }),
  })
    .then((res) => {
      if (!res.ok) {
        console.warn(
          `Envoy worker unsubscribe returned ${res.status} for session=${sessionId} (non-fatal)`,
        );
      }
    })
    .catch((err) => {
      console.warn(
        `Envoy worker unsubscribe failed for session=${sessionId} (non-fatal): ${err}`,
      );
    });
}
```

- [ ] **Step 5: Call unsubscribe from DELETE handler**

In the DELETE handler (around line 705-712), add the unsubscribe call after `persistState()` and before the return:

```typescript
          if (method === "DELETE") {
            crashHistory.set(id, {
              crashCount: entry.crashCount,
              lastCrashAt: entry.lastCrashAt,
            });
            workers.delete(id);
            await persistState();
            unsubscribeWorkerFromEnvoy(entry.sessionId);
            return jsonResponse({ status: "stopped" });
          }
```

The call is placed after `persistState()` following the established pattern: worker must be untracked before unsubscribing, so the ordering is: delete from map → persist → unsubscribe (fire-and-forget).

- [ ] **Step 6: Run tests to verify they pass**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts -t "unsubscribes worker|delete succeeds even when envoy"`
Expected: PASS (both tests).

- [ ] **Step 7: Run full daemon test suite to verify no regression**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts`
Expected: All tests pass.

- [ ] **Step 8: Run type check and lint**

```bash
bunx tsc --noEmit && bunx biome check packages/daemon/src/
```

Expected: No errors.

- [ ] **Step 9: Describe and advance**

```bash
jj describe -m "fix(daemon): unsubscribe workers from envoy on DELETE /workers/:id"
jj new
```

---

### Task 3: Cross-Layer Verification — Depends on: Task 1, Task 2

**Files:** None (run-only — no new code)

- [ ] **Step 1: Run full Go test suite**

Run (from `packages/envoy/`): `go test ./... -timeout 120s`
Expected: All tests pass including new admin endpoint tests and List integration test.

- [ ] **Step 2: Run full daemon test suite**

Run (from repo root): `bun test packages/daemon/`
Expected: All tests pass including new unsubscribe tests.

- [ ] **Step 3: Run type checks and lint**

```bash
bunx tsc --noEmit && bunx biome check packages/daemon/src/
```

And from `packages/envoy/`:
```bash
go vet ./...
```

Expected: Clean.

- [ ] **Step 4: Describe and advance**

```bash
jj describe -m "chore: verify cross-layer interest cleanup — admin endpoints + daemon unsubscribe"
jj new
```

---

## Testing Plan

### Setup
- Go integration tests (from `packages/envoy/`): `go test ./internal/integration/ -v -timeout 120s`
- Go handler tests (from `packages/envoy/`): `go test ./cmd/listener/ -v -timeout 60s`
- Daemon tests (from repo root): `bun test packages/daemon/src/daemon/__tests__/server.test.ts`
- Type check (from repo root): `bunx tsc --noEmit`
- Lint (from repo root): `bunx biome check packages/daemon/src/`
- Go vet (from `packages/envoy/`): `go vet ./...`

### Health Check
- Go tests self-start NATS via testcontainers (Docker required, auto-pulled)
- Bun tests use mocked fetch — no external dependencies
- Retry for 30s if Docker is slow to pull `nats:2.10`

### Verification Steps

1. **Registry.List sorted output**
   - Action (from `packages/envoy/`): `go test ./internal/integration/ -run TestE2E_RegistryListReturnsSortedInterests -v`
   - Expected: Returns 3 interests sorted alphabetically by session_id.
   - Tool: Go test runner + testcontainers

2. **Admin GET /v1/interests/ (list all)**
   - Action (from `packages/envoy/`): `go test ./cmd/listener/ -run TestAdminInterestsHandler_ListAll -v`
   - Expected: HTTP 200, JSON array with 2 entries sorted by session_id.
   - Tool: Go test runner + testcontainers + httptest

3. **Admin GET /v1/interests/:sessionId (existing behavior preserved)**
   - Action (from `packages/envoy/`): `go test ./cmd/listener/ -run TestAdminInterestsHandler_GetBySession -v`
   - Expected: HTTP 200 with interest JSON.
   - Tool: Go test runner + testcontainers + httptest

4. **Admin DELETE /v1/interests/:sessionId**
   - Action (from `packages/envoy/`): `go test ./cmd/listener/ -run TestAdminInterestsHandler_DeleteBySession -v`
   - Expected: HTTP 204. Subsequent GET returns not-found.
   - Tool: Go test runner + testcontainers + httptest

5. **Admin DELETE idempotent (not-found → 204)**
   - Action (from `packages/envoy/`): `go test ./cmd/listener/ -run TestAdminInterestsHandler_DeleteIdempotent -v`
   - Expected: HTTP 204 for non-existent session.
   - Tool: Go test runner + httptest

6. **Method not allowed**
   - Action (from `packages/envoy/`): `go test ./cmd/listener/ -run TestAdminInterestsHandler_MethodNotAllowed -v`
   - Expected: HTTP 405 for POST to /v1/interests/.
   - Tool: Go test runner + httptest

7. **Daemon unsubscribe on worker delete**
   - Action: `bun test packages/daemon/src/daemon/__tests__/server.test.ts -t "unsubscribes worker from envoy on delete"`
   - Expected: DELETE /workers triggers POST to /v1/interests/unsubscribe with `{session_id, topics: []}`.
   - Tool: Bun test runner + mocked fetch

8. **Daemon delete resilience**
   - Action: `bun test packages/daemon/src/daemon/__tests__/server.test.ts -t "delete succeeds even when envoy"`
   - Expected: DELETE /workers returns 200 even when envoy returns 500.
   - Tool: Bun test runner + mocked fetch

### Tools Needed
- Go test runner with testcontainers (Docker)
- Bun test runner
- `go vet` (from packages/envoy/), `bunx biome check packages/daemon/src/`, `bunx tsc --noEmit` for lint/type checks

---

## Required Skills

The following project-specific skills should be loaded by downstream workers:

| Phase | Skills |
|-------|--------|
| Implement | `envoy`, `using-jj` |
| Test | (standard Legion workflows) |
| Review | (standard Legion workflows) |

Workers: invoke these skills at the start of your workflow before beginning work. If a skill is unavailable in your environment, proceed without it.
