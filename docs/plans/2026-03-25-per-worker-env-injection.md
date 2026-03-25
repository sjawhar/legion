# Per-worker Environment Variable Injection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** Add per-worker environment variable injection so worker-specific credentials can be sent via daemon API, retrieved safely at worker startup, and never persisted to disk.
**Architecture:** Keep `WorkerEntry` unchanged and store sensitive environment data in a separate in-memory `workerEnv: Map<string, Record<string, string>>` inside `startServer()`. Extend daemon routing with `GET /workers/:id/env` and wire `POST /workers` env validation/storage through existing worker creation flow. Update CLI dispatch to parse repeatable `--env KEY=VALUE` flags and forward `env` to daemon; update worker startup instructions to fetch and export env after `jj new`.
**Tech Stack:** TypeScript, Bun, citty CLI, Bun test

---

## File Structure

- Modify: `packages/daemon/src/daemon/server.ts`
- Modify: `packages/daemon/src/daemon/__tests__/server.test.ts`
- Modify: `packages/daemon/src/cli/index.ts`
- Modify: `packages/daemon/src/cli/__tests__/index.test.ts`
- Modify: `.opencode/skills/legion-worker/SKILL.md`

---

### Task 1: Daemon env storage + env endpoint — Independent

**Files:**
- Modify: `packages/daemon/src/daemon/server.ts`
- Test: `packages/daemon/src/daemon/__tests__/server.test.ts`

- [ ] **Step 1: Write failing test**
```typescript
// Add to packages/daemon/src/daemon/__tests__/server.test.ts

it("returns worker env from GET /workers/:id/env", async () => {
  await startTestServer();

  const createResponse = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "ENG-106",
      mode: "implement",
      workspace: "/tmp/work-106",
      env: {
        GH_TOKEN: "ghp_123",
        GIT_AUTHOR_NAME: "legion-bot",
      },
    }),
  });

  expect(createResponse.status).toBe(200);
  const created = (await createResponse.json()) as { id: string };

  const envResponse = await requestJson(`/workers/${created.id}/env`);
  expect(envResponse.status).toBe(200);

  const envBody = (await envResponse.json()) as {
    env: Record<string, string>;
  };

  expect(envBody).toEqual({
    env: {
      GH_TOKEN: "ghp_123",
      GIT_AUTHOR_NAME: "legion-bot",
    },
  });
});

it("returns empty env object when worker has no env", async () => {
  await startTestServer();

  const createResponse = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "ENG-107",
      mode: "plan",
      workspace: "/tmp/work-107",
    }),
  });

  expect(createResponse.status).toBe(200);
  const created = (await createResponse.json()) as { id: string };

  const envResponse = await requestJson(`/workers/${created.id}/env`);
  expect(envResponse.status).toBe(200);

  const envBody = (await envResponse.json()) as {
    env: Record<string, string>;
  };

  expect(envBody).toEqual({ env: {} });
});

it("returns 404 from GET /workers/:id/env when worker is missing", async () => {
  await startTestServer();

  const envResponse = await requestJson("/workers/missing-worker/env");
  expect(envResponse.status).toBe(404);

  const envBody = (await envResponse.json()) as {
    error: string;
  };

  expect(envBody).toEqual({ error: "worker not found" });
});

it("rejects POST /workers when env is not a string record", async () => {
  await startTestServer();

  const response = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "ENG-108",
      mode: "implement",
      workspace: "/tmp/work-108",
      env: {
        GH_TOKEN: 123,
      },
    }),
  });

  expect(response.status).toBe(400);
  const body = (await response.json()) as { error: string };
  expect(body).toEqual({
    error: "env must be an object with string values",
  });
});
```

- [ ] **Step 2: Run test to verify failure**
Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts`
Expected: FAIL with missing `/workers/:id/env` route and/or missing env validation error expectation.

- [ ] **Step 3: Implement**
```typescript
// Update packages/daemon/src/daemon/server.ts

// 1) Add helper near existing isRecord()
function isStringRecord(value: unknown): value is Record<string, string> {
  if (!isRecord(value)) {
    return false;
  }

  for (const entry of Object.values(value)) {
    if (typeof entry !== "string") {
      return false;
    }
  }

  return true;
}

// 2) Inside startServer(), keep WorkerEntry unchanged and add separate env map
const workers = new Map<string, WorkerEntry>();
const workerEnv = new Map<string, Record<string, string>>();
const crashHistory = new Map<string, CrashHistoryEntry>();

// 3) In POST /workers handler, validate optional env and store it in workerEnv
if (method === "POST" && segments.length === 1 && segments[0] === "workers") {
  const body = await parseJson(request);
  if (!isRecord(body)) {
    return badRequest("body must be a JSON object");
  }

  const issueId = body.issueId;
  const mode = body.mode;
  const repo = body.repo;
  const workspace = body.workspace;
  const version = body.version;
  const env = body.env;

  // ...existing issueId/mode/repo/workspace/version validation stays unchanged...

  if (env !== undefined && !isStringRecord(env)) {
    return badRequest("env must be an object with string values");
  }

  const id = `${issueId}-${mode}`.toLowerCase();

  const entry: WorkerEntry = {
    id,
    port,
    pid: undefined,
    sessionId,
    workspace: resolvedWorkspace,
    startedAt,
    status: "starting",
    crashCount: 0,
    lastCrashAt: null,
    version,
  };

  workers.set(id, entry);

  if (env !== undefined) {
    workerEnv.set(id, { ...env });
  } else {
    workerEnv.delete(id);
  }

  await persistState();
  return jsonResponse({ id, port, sessionId });
}

// 4) Add new route: GET /workers/:id/env
if (segments.length === 3 && segments[0] === "workers" && segments[2] === "env") {
  if (method !== "GET") {
    return new Response("Method Not Allowed", {
      status: 405,
      headers: { Allow: "GET" },
    });
  }

  const id = segments[1];
  if (!workers.has(id)) {
    return notFound("worker not found");
  }

  return jsonResponse({ env: workerEnv.get(id) ?? {} });
}

// 5) Extend DELETE /workers/:id cleanup path
if (method === "DELETE") {
  crashHistory.set(id, {
    crashCount: entry.crashCount,
    lastCrashAt: entry.lastCrashAt,
  });
  workers.delete(id);
  workerEnv.delete(id);
  await persistState();
  return jsonResponse({ status: "stopped" });
}
```

- [ ] **Step 4: Run test to verify pass**
Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts`
Expected: PASS

- [ ] **Step 5: Describe and advance**
```bash
jj describe -m "feat(daemon): add per-worker env endpoint and in-memory env map"
jj new
```

---

### Task 2: CLI env flag parsing and forwarding — Depends on: Task 1

**Files:**
- Modify: `packages/daemon/src/cli/index.ts`
- Test: `packages/daemon/src/cli/__tests__/index.test.ts`

- [ ] **Step 1: Write failing test**
```typescript
// Add to packages/daemon/src/cli/__tests__/index.test.ts

it("forwards env in cmdDispatch request body", async () => {
  const fetchMock = installFetchMock();
  fetchMock.mockResolvedValue(new Response(JSON.stringify({ id: "eng-106-implement" }), {
    status: 200,
    headers: { "content-type": "application/json" },
  }));

  await cmdDispatch("ENG-106", "implement", {
    workspace: "/tmp/work-106",
    env: {
      GH_TOKEN: "token-1",
      GIT_AUTHOR_NAME: "bot-a",
    },
  });

  const [, postInit] = fetchMock.mock.calls[0] as [RequestInfo | URL, RequestInit];
  const parsedBody = JSON.parse(postInit.body as string) as {
    env?: Record<string, string>;
  };

  expect(parsedBody.env).toEqual({
    GH_TOKEN: "token-1",
    GIT_AUTHOR_NAME: "bot-a",
  });
});

it("parses repeatable --env KEY=VALUE flags", () => {
  const env = parseEnvAssignments([
    "dispatch",
    "ENG-106",
    "implement",
    "--workspace",
    "/tmp/work-106",
    "--env",
    "GH_TOKEN=token-1",
    "--env",
    "GIT_AUTHOR_NAME=bot-a",
  ]);

  expect(env).toEqual({
    GH_TOKEN: "token-1",
    GIT_AUTHOR_NAME: "bot-a",
  });
});

it("throws on malformed --env assignment", () => {
  expect(() => parseEnvAssignments(["dispatch", "ENG-106", "plan", "--env", "BROKEN"]))
    .toThrow(new CliError("Invalid --env value \"BROKEN\". Expected KEY=VALUE"));
});
```

- [ ] **Step 2: Run test to verify failure**
Run: `bun test packages/daemon/src/cli/__tests__/index.test.ts`
Expected: FAIL with missing `env` on `DispatchOptions`, missing `parseEnvAssignments`, and request body assertion mismatch.

- [ ] **Step 3: Implement**
```typescript
// Update packages/daemon/src/cli/index.ts

interface DispatchOptions {
  legionDir?: string;
  daemonPort?: number;
  prompt?: string;
  repo?: string;
  workspace?: string;
  version?: number;
  env?: Record<string, string>;
}

export function parseEnvAssignments(rawArgv: readonly string[]): Record<string, string> | undefined {
  const assignments: string[] = [];

  for (let i = 0; i < rawArgv.length; i += 1) {
    const token = rawArgv[i];

    if (token === "--env" || token === "-e") {
      const next = rawArgv[i + 1];
      if (!next) {
        throw new CliError("Missing value for --env. Expected KEY=VALUE");
      }
      assignments.push(next);
      i += 1;
      continue;
    }

    if (token.startsWith("--env=")) {
      assignments.push(token.slice("--env=".length));
      continue;
    }

    if (token.startsWith("-e=")) {
      assignments.push(token.slice("-e=".length));
      continue;
    }
  }

  if (assignments.length === 0) {
    return undefined;
  }

  const env: Record<string, string> = {};
  for (const assignment of assignments) {
    const separatorIndex = assignment.indexOf("=");
    if (separatorIndex <= 0) {
      throw new CliError(`Invalid --env value \"${assignment}\". Expected KEY=VALUE`);
    }

    const key = assignment.slice(0, separatorIndex).trim();
    const value = assignment.slice(separatorIndex + 1);

    if (key.length === 0) {
      throw new CliError(`Invalid --env value \"${assignment}\". Expected KEY=VALUE`);
    }

    env[key] = value;
  }

  return env;
}

export async function cmdDispatch(issue: string, mode: string, opts: DispatchOptions): Promise<void> {
  const body: Record<string, unknown> = {
    issueId: issue,
    mode,
    version: opts.version,
  };

  if (opts.repo) {
    body.repo = opts.repo;
  } else if (opts.workspace) {
    body.workspace = opts.workspace;
  } else if (opts.legionDir) {
    body.workspace = opts.legionDir;
  } else {
    throw new CliError("Either --repo or --workspace is required");
  }

  if (opts.env && Object.keys(opts.env).length > 0) {
    body.env = opts.env;
  }

  // ...existing prompt forwarding and fetch logic remains unchanged...
}

export const dispatchCommand = defineCommand({
  meta: { name: "dispatch", description: "Dispatch a worker for an issue" },
  args: {
    issue: { type: "positional", required: true },
    mode: { type: "positional", required: true },
    prompt: { type: "string", required: false },
    repo: { type: "string", alias: "r", required: false },
    workspace: { type: "string", alias: "w", required: false },
    version: { type: "string", alias: "v", required: false },
    env: {
      type: "string",
      alias: "e",
      required: false,
      description: "Worker env assignment (KEY=VALUE). Repeat flag for multiple values.",
    },
  },
  async run({ args }) {
    const version = args.version ? Number.parseInt(args.version, 10) : undefined;
    const env = parseEnvAssignments(process.argv.slice(2));

    await cmdDispatch(args.issue, args.mode, {
      legionDir: process.env.LEGION_DIR,
      prompt: args.prompt,
      repo: args.repo,
      workspace: args.workspace,
      version,
      env,
    });
  },
});
```

- [ ] **Step 4: Run test to verify pass**
Run: `bun test packages/daemon/src/cli/__tests__/index.test.ts`
Expected: PASS

- [ ] **Step 5: Describe and advance**
```bash
jj describe -m "feat(cli): support repeatable --env forwarding in dispatch"
jj new
```

---

### Task 3: Worker env isolation integration coverage — Depends on: Task 1

**Files:**
- Modify: `packages/daemon/src/daemon/__tests__/server.test.ts`

- [ ] **Step 1: Write failing test**
```typescript
// Add to packages/daemon/src/daemon/__tests__/server.test.ts

it("isolates env per worker across multiple dispatches", async () => {
  await startTestServer();

  const firstCreate = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "ENG-201",
      mode: "implement",
      workspace: "/tmp/work-201",
      env: {
        GH_TOKEN: "token-A",
        GIT_AUTHOR_NAME: "bot-A",
      },
    }),
  });
  expect(firstCreate.status).toBe(200);
  const first = (await firstCreate.json()) as { id: string };

  const secondCreate = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "ENG-202",
      mode: "implement",
      workspace: "/tmp/work-202",
      env: {
        GH_TOKEN: "token-B",
        GIT_AUTHOR_NAME: "bot-B",
      },
    }),
  });
  expect(secondCreate.status).toBe(200);
  const second = (await secondCreate.json()) as { id: string };

  const firstEnvResponse = await requestJson(`/workers/${first.id}/env`);
  const firstEnvBody = (await firstEnvResponse.json()) as {
    env: Record<string, string>;
  };

  const secondEnvResponse = await requestJson(`/workers/${second.id}/env`);
  const secondEnvBody = (await secondEnvResponse.json()) as {
    env: Record<string, string>;
  };

  expect(firstEnvBody).toEqual({
    env: {
      GH_TOKEN: "token-A",
      GIT_AUTHOR_NAME: "bot-A",
    },
  });
  expect(secondEnvBody).toEqual({
    env: {
      GH_TOKEN: "token-B",
      GIT_AUTHOR_NAME: "bot-B",
    },
  });

  expect(firstEnvBody.env.GH_TOKEN).not.toBe(secondEnvBody.env.GH_TOKEN);
  expect(firstEnvBody.env.GIT_AUTHOR_NAME).not.toBe(secondEnvBody.env.GIT_AUTHOR_NAME);
});

it("clears worker env after worker deletion", async () => {
  await startTestServer();

  const createResponse = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "ENG-203",
      mode: "review",
      workspace: "/tmp/work-203",
      env: {
        GH_TOKEN: "token-delete",
      },
    }),
  });

  expect(createResponse.status).toBe(200);
  const created = (await createResponse.json()) as { id: string };

  const deleteResponse = await requestJson(`/workers/${created.id}`, { method: "DELETE" });
  expect(deleteResponse.status).toBe(200);

  const envResponseAfterDelete = await requestJson(`/workers/${created.id}/env`);
  expect(envResponseAfterDelete.status).toBe(404);
});
```

- [ ] **Step 2: Run test to verify failure**
Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts --test-name-pattern "env"`
Expected: FAIL until env map isolation + delete cleanup are implemented.

- [ ] **Step 3: Implement**
```typescript
// No additional production changes if Task 1 implementation is complete.
// Ensure Task 1 includes this cleanup line in DELETE /workers/:id:
workerEnv.delete(id);
```

- [ ] **Step 4: Run test to verify pass**
Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts --test-name-pattern "env"`
Expected: PASS

- [ ] **Step 5: Describe and advance**
```bash
jj describe -m "test(daemon): add env isolation and cleanup integration coverage"
jj new
```

---

### Task 4: Worker startup env bootstrap instructions — Depends on: Task 1

**Files:**
- Modify: `.opencode/skills/legion-worker/SKILL.md`

- [ ] **Step 1: Write failing test**
```typescript
// Add to packages/daemon/src/daemon/__tests__/server.test.ts
// (Documentation behavior is validated by API behavior + startup command assumptions)

it("supports worker startup bootstrap contract via GET /workers/:id/env", async () => {
  await startTestServer();

  const createResponse = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "SJAWHAR-LEGION-106",
      mode: "plan",
      workspace: "/tmp/work-boot",
      env: {
        GH_TOKEN: "token-bootstrap",
      },
    }),
  });

  expect(createResponse.status).toBe(200);
  const created = (await createResponse.json()) as { id: string };
  expect(created.id).toBe("sjawhar-legion-106-plan");

  const envResponse = await requestJson(`/workers/${created.id}/env`);
  expect(envResponse.status).toBe(200);
  const body = (await envResponse.json()) as { env: Record<string, string> };

  expect(body.env).toEqual({ GH_TOKEN: "token-bootstrap" });
});
```

- [ ] **Step 2: Run test to verify failure**
Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts --test-name-pattern "bootstrap contract"`
Expected: FAIL until `/workers/:id/env` behavior is implemented.

- [ ] **Step 3: Implement**
```markdown
<!-- Update .opencode/skills/legion-worker/SKILL.md startup section -->

### Starting

Sync with main and create a fresh commit on your branch:

```bash
jj git fetch
jj rebase -d main
jj new  # Fresh commit for this session

# Load per-worker env from daemon (if present)
# Assumption: LEGION_DAEMON_PORT is present in worker shell env via shared serve process environment.
# Assumption: ISSUE_ID and MODE are known from dispatch prompt; set them explicitly if not exported.
ISSUE_ID="${LEGION_ISSUE_ID:-<issue-id-from-dispatch-prompt>}"
MODE="${LEGION_MODE:-<mode-from-dispatch-prompt>}"
WORKER_ID="$(printf "%s-%s" "$ISSUE_ID" "$MODE" | tr '[:upper:]' '[:lower:]')"
WORKER_ENV_JSON="$(curl -fsS "http://127.0.0.1:${LEGION_DAEMON_PORT}/workers/${WORKER_ID}/env")"

eval "$(bun -e '
const payload = JSON.parse(process.argv[1]);
for (const [key, rawValue] of Object.entries(payload.env ?? {})) {
  if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) {
    continue;
  }
  const value = String(rawValue)
    .replace(/\\/g, "\\\\")
    .replace(/"/g, "\\\"")
    .replace(/\$/g, "\\$")
    .replace(/`/g, "\\`");
  console.log(`export ${key}="${value}"`);
}
' "$WORKER_ENV_JSON")"
```

Optionally read prior handoff data (advisory, non-blocking):

```bash
legion handoff read --workspace . 2>/dev/null || echo '{}'
```
```

- [ ] **Step 4: Run test to verify pass**
Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts --test-name-pattern "bootstrap contract"`
Expected: PASS

- [ ] **Step 5: Describe and advance**
```bash
jj describe -m "docs(worker): bootstrap per-worker env from daemon endpoint"
jj new
```

---

## Testing Plan

### Setup
- `bun install`
- `bun test packages/daemon/src/daemon/__tests__/server.test.ts`
- `bun test packages/daemon/src/cli/__tests__/index.test.ts`

### Health Check
- Verify daemon env endpoint behavior:
  - `POST /workers` with `env` returns `200`
  - `GET /workers/:id/env` returns expected `{ env: Record<string, string> }`
  - `GET /workers/:id/env` returns `404` for missing worker
  - `GET /workers/:id/env` returns `{ env: {} }` when env not set
- Verify CLI forwarding behavior:
  - `cmdDispatch(..., { env })` includes `env` in outgoing POST body
  - repeated `--env KEY=VALUE` parsing yields merged record

### Verification Steps
1. Run daemon test suite for env behavior:
   - `bun test packages/daemon/src/daemon/__tests__/server.test.ts`
   - Expect: all new env endpoint + isolation + cleanup tests PASS
2. Run CLI dispatch tests:
   - `bun test packages/daemon/src/cli/__tests__/index.test.ts`
   - Expect: env parsing/forwarding tests PASS
3. Run focused regression check:
   - `bun test`
   - Expect: full repository test suite PASS
4. Manual smoke check (optional during implementation):
   - Start daemon and dispatch two workers with different `--env` assignments
   - Hit `GET /workers/{id}/env` for each
   - Expect: no cross-worker env leakage

### Tools Needed
- Bun (`bun`, `bun test`)
- jj (`jj describe -m ...`, `jj new`)
- curl (for manual env endpoint verification)
