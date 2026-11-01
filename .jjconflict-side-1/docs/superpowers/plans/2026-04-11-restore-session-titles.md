# Restore Session Titles in KV Registry — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Restore session title display in Envoy's `/v1/sessions` API response — regression from PR #393 which removed the file registry that previously stored titles.

**Architecture:** The fix threads a `Title` field through three layers: (1) Go `SessionEntry` struct gains a `Title` field persisted in NATS KV, wired through the subscribe handler and `/v1/sessions` endpoint, (2) the OpenCode plugin fetches the title once from the serve API (`GET /session/{id}`) on session activation and includes it in all subsequent heartbeat/subscribe payloads. The `envoy_sessions` KV bucket has a 5-minute TTL with heartbeats every 2 minutes, so titles refresh automatically.

**Tech Stack:** Go (Envoy listener + session registry), TypeScript/Bun (envoy-plugin), NATS JetStream KV

**Note:** The `envoy` CLI script (`~/.dotfiles/scripts/envoy`) also needs a TITLE column added to `envoy ps` output. This is in a separate repo (dotfiles) and should be done as a follow-up, not part of this PR.

---

### Task 1: Add Title to SessionEntry, subscribe handler, and /v1/sessions response — Independent

**Files:**
- Modify: `packages/envoy/internal/session/registry.go:15-20`
- Modify: `packages/envoy/cmd/listener/main.go:112-119` (sessionInfo struct)
- Modify: `packages/envoy/cmd/listener/main.go:141-147` (sessionsHandler body)
- Modify: `packages/envoy/cmd/listener/main.go:301-306` (subscribe body struct)
- Modify: `packages/envoy/cmd/listener/main.go:322-326` (SessionEntry creation)
- Create test: `packages/envoy/cmd/listener/main_test.go` (new test after line 684)

- [ ] **Step 1: Add Title field to SessionEntry struct**

In `packages/envoy/internal/session/registry.go`, add `Title` to the struct at lines 15-20. Insert between `Dir` and `UpdatedAt`:

```go
type SessionEntry struct {
	Port      int    `json:"port"`
	MachineID string `json:"machine_id"`
	Dir       string `json:"dir"`
	Title     string `json:"title"`
	UpdatedAt int64  `json:"updated_at"`
}
```

- [ ] **Step 2: Add Title to sessionInfo struct**

In `packages/envoy/cmd/listener/main.go`, update the `sessionInfo` struct at lines 112-119. Insert `Title` between `Port` and `Topics`:

```go
type sessionInfo struct {
	SessionID string   `json:"session_id"`
	MachineID string   `json:"machine_id"`
	Dir       string   `json:"dir"`
	Port      int      `json:"port"`
	Title     string   `json:"title"`
	Topics    []string `json:"topics"`
	UpdatedAt int64    `json:"updated_at"`
}
```

- [ ] **Step 3: Populate Title in sessionsHandler**

In `packages/envoy/cmd/listener/main.go`, update the sessionsHandler loop at lines 141-147. Add `Title: entry.Title,` between `Port` and `UpdatedAt`:

```go
		info := sessionInfo{
			SessionID: entry.SessionID,
			MachineID: entry.MachineID,
			Dir:       entry.Dir,
			Port:      entry.Port,
			Title:     entry.Title,
			UpdatedAt: entry.UpdatedAt,
		}
```

- [ ] **Step 4: Add Title to subscribe handler body struct**

In `packages/envoy/cmd/listener/main.go`, update the subscribe handler's body struct at lines 301-306. Add `Title` after `Port`:

```go
	var body struct {
		SessionID string   `json:"session_id"`
		Dir       string   `json:"dir"`
		Topics    []string `json:"topics"`
		Port      int      `json:"port"`
		Title     string   `json:"title"`
	}
```

- [ ] **Step 5: Pass Title when creating SessionEntry in subscribe handler**

In `packages/envoy/cmd/listener/main.go`, update the SessionEntry creation at lines 322-326. Add `Title: body.Title,` between `Dir` and the closing `}`:

```go
		if body.Port > 0 {
			if err := d.sessions.Put(body.SessionID, session.SessionEntry{
				Port:      body.Port,
				MachineID: cfg.MachineID,
				Dir:       body.Dir,
				Title:     body.Title,
			}); err != nil {
```

- [ ] **Step 6: Write test for title flowing through /v1/sessions**

Add to `packages/envoy/cmd/listener/main_test.go` after the `TestSessionsHandler_JoinsRegistries` test (after line 684):

```go
func TestSessionsHandler_IncludesTitle(t *testing.T) {
	conn := natstest.StartServer(t)
	registry, err := store.OpenRegistry(conn, store.WithReplicas(1))
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.OpenSessionRegistry(conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Upsert(store.Interest{
		SessionID: "ses_titled",
		MachineID: "test-machine",
		Dir:       "/test/ses_titled",
	}, []string{contracts.AgentSubject("ses_titled")}); err != nil {
		t.Fatal(err)
	}
	if err := sessions.Put("ses_titled", session.SessionEntry{
		Port:      13382,
		MachineID: "test-machine",
		Dir:       "/test/ses_titled",
		Title:     "My Session Title",
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)

	handler := sessionsHandler(registry, sessions)
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var items []sessionInfo
	if err := json.NewDecoder(rec.Body).Decode(&items); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 session, got %d", len(items))
	}
	if items[0].Title != "My Session Title" {
		t.Fatalf("expected title 'My Session Title', got %q", items[0].Title)
	}
}
```

- [ ] **Step 7: Run test to verify it passes**

Run: `cd packages/envoy && go test ./cmd/listener/ -run TestSessionsHandler_IncludesTitle -v -count=1`

Expected: PASS — title persisted in KV and returned in /v1/sessions response.

- [ ] **Step 8: Run full test suites to verify no regressions**

Run: `cd packages/envoy && go test ./internal/session/... -v -count=1 && go test ./cmd/listener/ -v -count=1`

Expected: All tests pass. Existing `SessionEntry{}` literals without Title are valid Go — the field gets its zero value (empty string).

- [ ] **Step 9: Describe and advance**

```bash
jj describe -m "fix(envoy): add Title field to SessionEntry, subscribe handler, and /v1/sessions response"
jj new
```

---

### Task 2: Plugin fetches and sends session title — Depends on: Task 1

**Files:**
- Modify: `packages/envoy-plugin/src/index.ts`
- Modify: `packages/envoy-plugin/src/__tests__/index.test.ts`

The plugin fetches the session title once from the OpenCode serve API (`GET /session/{id}`) when a session becomes active. It caches the title and includes it in all subsequent heartbeat and subscribe payloads.

- [ ] **Step 1: Add title state variable**

In `packages/envoy-plugin/src/index.ts`, add after the `activeSessionID` declaration (after line 20):

```typescript
let activeSessionTitle: string | null = null;
```

- [ ] **Step 2: Add title-fetching function**

In `packages/envoy-plugin/src/index.ts`, add after the `syncPort` function (after line 46):

```typescript
  /** Fetch session title from the OpenCode serve API. Best-effort — returns null on failure. */
  const fetchTitle = async (sessionID: string): Promise<string | null> => {
    try {
      const res = await fetch(`${input.serverUrl.href}session/${sessionID}`, {
        signal: AbortSignal.timeout(CALL_TIMEOUT_MS),
      });
      if (!res.ok) return null;
      const data = (await res.json()) as { title?: string };
      return data.title ?? null;
    } catch {
      return null;
    }
  };
```

- [ ] **Step 3: Update session status handler to fetch title and include in subscribe**

In `packages/envoy-plugin/src/index.ts`, replace the session status handler block (lines 92-114) with:

```typescript
      if (
        event.type === "session.status" &&
        (event.properties?.status as { type?: string } | undefined)?.type === "busy"
      ) {
        const sessionID = event.properties?.sessionID as string | undefined;
        if (sessionID && sessionID !== activeSessionID) {
          activeSessionID = sessionID;
          activeSessionTitle = null;
          await syncPort();
          const port = currentPort();
          // Fetch title — best-effort, non-blocking for initial subscribe
          const titlePromise = fetchTitle(sessionID).then((t) => {
            if (t) activeSessionTitle = t;
          });
          if (port) {
            call("/v1/interests/subscribe", {
              method: "POST",
              headers: { "Content-Type": "application/json" },
              body: JSON.stringify({
                session_id: sessionID,
                dir: process.cwd(),
                topics: [`notifications.agent.${sessionID}`],
                port,
                title: activeSessionTitle ?? "",
              }),
            }).catch(() => {});
            // After title arrives, send one follow-up subscribe with title populated
            titlePromise.then(() => {
              if (activeSessionTitle && activeSessionID === sessionID) {
                call("/v1/interests/subscribe", {
                  method: "POST",
                  headers: { "Content-Type": "application/json" },
                  body: JSON.stringify({
                    session_id: sessionID,
                    dir: process.cwd(),
                    topics: [`notifications.agent.${sessionID}`],
                    port: currentPort() ?? 0,
                    title: activeSessionTitle,
                  }),
                }).catch(() => {});
              }
            });
          }
        }
      }
```

- [ ] **Step 4: Update heartbeat to include cached title**

In `packages/envoy-plugin/src/index.ts`, update the heartbeat interval (lines 59-77). Add `title` to the JSON body:

```typescript
  const heartbeatInterval = setInterval(
    () => {
      if (!activeSessionID) return;
      const port = currentPort();
      if (!port) return;
      call("/v1/interests/subscribe", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          session_id: activeSessionID,
          dir: process.cwd(),
          topics: [`notifications.agent.${activeSessionID}`],
          port,
          title: activeSessionTitle ?? "",
        }),
      }).catch(() => {});
    },
    2 * 60 * 1000
  );
```

- [ ] **Step 5: Update envoy_subscribe tool to include title**

In `packages/envoy-plugin/src/index.ts`, update the `envoy_subscribe` tool's execute method (lines 127-139). Add `title` to the JSON body:

```typescript
        async execute(args, ctx) {
          ctx.metadata({ title: "Envoy subscribe" });
          return call("/v1/interests/subscribe", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
              session_id: ctx.sessionID,
              dir: ctx.directory,
              topics: args.topics,
              port: currentPort() ?? 0,
              title: activeSessionTitle ?? "",
            }),
          });
        },
```

- [ ] **Step 6: Add focused plugin test for title in subscribe payload**

Add to `packages/envoy-plugin/src/__tests__/index.test.ts`:

```typescript
describe("session title", () => {
  it("includes title in subscribe payload after session activation", async () => {
    const originalEnvoyUrl = process.env.ENVOY_URL;
    process.env.ENVOY_URL = "http://127.0.0.1:59999";

    const fetchCalls: { url: string; body: string }[] = [];
    const originalFetch = globalThis.fetch;
    globalThis.fetch = async (input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (init?.body) {
        fetchCalls.push({ url, body: init.body as string });
      }
      if (url.includes("/session/ses_test_title")) {
        return new Response(JSON.stringify({ id: "ses_test_title", title: "Test Title" }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      throw new Error("connection refused");
    };

    try {
      const mod = await import("../index");
      const hooks = await mod.default({ serverUrl: new URL("http://127.0.0.1:13381") } as never);

      await hooks.event({
        event: {
          type: "session.status",
          properties: {
            sessionID: "ses_test_title",
            status: { type: "busy" },
          },
        },
      });

      // Allow async title fetch to complete
      await new Promise((r) => setTimeout(r, 200));

      const subscribeCalls = fetchCalls.filter((c) => c.url.includes("/v1/interests/subscribe"));
      const hasTitle = subscribeCalls.some((c) => {
        const body = JSON.parse(c.body);
        return body.title === "Test Title";
      });
      expect(hasTitle).toBe(true);
    } finally {
      globalThis.fetch = originalFetch;
      process.env.ENVOY_URL = originalEnvoyUrl;
    }
  });
});
```

- [ ] **Step 7: Build the plugin**

Run: `cd packages/envoy-plugin && bun run build`

Expected: Build succeeds.

- [ ] **Step 8: Run plugin tests**

Run: `cd packages/envoy-plugin && bun test`

Expected: All tests pass, including the new title test.

- [ ] **Step 9: Describe and advance**

```bash
jj describe -m "fix(envoy): plugin sends session title in heartbeat/subscribe payloads"
jj new
```

---

## Testing Plan

### Setup
- Envoy listener running: `curl -fsS http://127.0.0.1:9020/healthz` returns `{"status":"healthy"}`
- OpenCode serve running: `curl -fsS http://127.0.0.1:13381/session | jq length` returns a number

### Health Check
- `curl -fsS http://127.0.0.1:9020/healthz` returns `{"status":"healthy"}` (or `{"status":"starting"}` — retry for 30s before declaring failure)

### Verification Steps

1. **Title flows through Go pipeline (unit test)**
   - Action: `cd packages/envoy && go test ./cmd/listener/ -run TestSessionsHandler_IncludesTitle -v -count=1`
   - Expected: PASS
   - Tool: Go test runner

2. **No regressions in session registry**
   - Action: `cd packages/envoy && go test ./internal/session/... -v -count=1`
   - Expected: All tests pass
   - Tool: Go test runner

3. **No regressions in listener**
   - Action: `cd packages/envoy && go test ./cmd/listener/ -v -count=1`
   - Expected: All tests pass
   - Tool: Go test runner

4. **Plugin builds cleanly**
   - Action: `cd packages/envoy-plugin && bun run build`
   - Expected: Build succeeds
   - Tool: Bun bundler

5. **Plugin tests pass (including title test)**
   - Action: `cd packages/envoy-plugin && bun test`
   - Expected: All tests pass
   - Tool: Bun test runner

6. **Title field present in /v1/sessions response (integration — requires live Envoy)**
   - Action: `curl -fsS http://127.0.0.1:9020/v1/sessions | jq '.[0] | keys'`
   - Expected: Response keys include `"title"` (value may be empty for sessions registered before fix)
   - Tool: curl + jq

### Tools Needed
- Go test runner (`go test`)
- Bun test runner (`bun test`)
- Bun bundler (`bun run build`)
- curl + jq (API verification)

### Skills to Invoke
- No project-specific testing skills identified beyond standard Legion workflows.

### Optional Follow-up (separate repo)
- Update `~/.dotfiles/scripts/envoy` to add TITLE column to `envoy ps` output: add `TITLE` to header and `(.title // "")` to jq expression in `_envoy_ps` function (lines 39-40).
