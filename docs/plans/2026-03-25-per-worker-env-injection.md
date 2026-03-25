# Per-worker Environment Variable Injection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** Add per-worker environment variable injection so worker-specific credentials can be sent via daemon API, retrieved safely at worker startup, and never persisted to disk.
**Architecture:** Keep `WorkerEntry` unchanged and store sensitive environment data in a separate in-memory `workerEnv: Map<string, Record<string, string>>` inside `startServer()`. Extend daemon routing with `GET /workers/:id/env` and wire `POST /workers` env validation/storage through existing worker creation flow. Update CLI dispatch to parse a JSON `--env` string and forward `env` to daemon; update worker startup instructions to fetch and export env after `jj new`.
**Tech Stack:** TypeScript, Bun, citty CLI, Bun test

---

## File Structure

- Modify: `packages/daemon/src/daemon/server.ts`
- Modify: `packages/daemon/src/daemon/__tests__/server.test.ts`
- Modify: `packages/daemon/src/cli/index.ts`
- Modify: `packages/daemon/src/cli/__tests__/index.test.ts`
- Modify: `.opencode/skills/legion-worker/SKILL.md`

---

### Task 1: Daemon env storage + endpoint + integration coverage — Independent

**Files:**
- Modify: `packages/daemon/src/daemon/server.ts`
- Test: `packages/daemon/src/daemon/__tests__/server.test.ts`

- [ ] **Step 1: Write failing tests**
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
Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts`
Expected: FAIL with missing `/workers/:id/env` route, missing env validation, and/or missing env isolation cleanup behavior.

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

// 3) In POST /workers handler, extract/validate env right after version extraction,
// then store env immediately after workers.set(entry.id, entry).
const payload = await parseJson(request);

const issueId = payload.issueId;
const mode = payload.mode;
const repo = payload.repo;
const workspace = payload.workspace;
const version = typeof payload.version === "number" ? payload.version : 0;
const env = payload.env;

if (env !== undefined && !isStringRecord(env)) {
  return badRequest("env must be an object with string values");
}

const sessionId = computeSessionId(opts.legionId, issueId, mode as WorkerModeLiteral, version);
let actualSessionId = sessionId;
try {
  actualSessionId = await opts.adapter.createSession(sessionId, resolvedWorkspace);
} catch (error) {
  return serverError(`Failed to create session: ${(error as Error).message}`);
}

const entry: WorkerEntry = {
  id: workerId,
  port: opts.adapter.getPort(),
  sessionId: actualSessionId,
  workspace: resolvedWorkspace,
  startedAt: new Date().toISOString(),
  status: "running",
  crashCount: crashHistoryEntry?.crashCount ?? 0,
  lastCrashAt: crashHistoryEntry?.lastCrashAt ?? null,
  version,
};

workers.set(entry.id, entry);
if (env !== undefined) {
  workerEnv.set(entry.id, { ...env });
} else {
  workerEnv.delete(entry.id);
}

await persistState();
return jsonResponse({
  id: entry.id,
  port: opts.adapter.getPort(),
  sessionId: entry.sessionId,
});

// 4) Add GET /workers/:id/env before the existing segments.length === 2 block.
if (segments.length === 3 && segments[0] === "workers" && segments[2] === "env") {
  await stateLoaded;
  if (method !== "GET") {
    return new Response("Method Not Allowed", {
      status: 405,
      headers: { Allow: "GET" },
    });
  }

  const id = segments[1].toLowerCase();
  if (!workers.has(id)) {
    return notFound("worker not found");
  }

  return jsonResponse({ env: workerEnv.get(id) ?? {} });
}

// 5) In DELETE /workers/:id (inside the segments.length === 2 block),
// delete env right after workers.delete(id).
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

### Task 2: CLI env flag (JSON) parsing and forwarding — Depends on: Task 1

**Files:**
- Modify: `packages/daemon/src/cli/index.ts`
- Test: `packages/daemon/src/cli/__tests__/index.test.ts`

- [ ] **Step 1: Write failing tests**
```typescript
// Add to packages/daemon/src/cli/__tests__/index.test.ts

it("forwards env in cmdDispatch request body", async () => {
  const fetchMock = installFetchMock((input: string | URL) => {
    const url = input.toString();
    if (url.endsWith("/health")) {
      return Promise.resolve(
        new Response(JSON.stringify({ status: "ok" }), { status: 200 }),
      );
    }
    if (url.endsWith("/workers")) {
      return Promise.resolve(
        new Response(
          JSON.stringify({ id: "eng-106-implement", port: 18000, sessionId: "s-1" }),
          {
            status: 200,
            headers: { "content-type": "application/json" },
          },
        ),
      );
    }
    if (url.includes("/workers/") && url.endsWith("/prompt")) {
      return Promise.resolve(new Response("{}", { status: 200 }));
    }
    return Promise.reject(new Error(`Unexpected fetch: ${url}`));
  });

  await cmdDispatch("ENG-106", "implement", {
    workspace: "/tmp/work-106",
    env: {
      GH_TOKEN: "token-1",
      GIT_AUTHOR_NAME: "bot-a",
    },
  });

  const [, postInit] = fetchMock.mock.calls[1] as [RequestInfo | URL, RequestInit];
  const parsedBody = JSON.parse(postInit.body as string) as {
    env?: Record<string, string>;
  };

  expect(parsedBody.env).toEqual({
    GH_TOKEN: "token-1",
    GIT_AUTHOR_NAME: "bot-a",
  });
});

it("parses --env JSON object", () => {
  const env = parseEnvJson('{"GH_TOKEN":"token-1","GIT_AUTHOR_NAME":"bot-a"}');

  expect(env).toEqual({
    GH_TOKEN: "token-1",
    GIT_AUTHOR_NAME: "bot-a",
  });
});

it("throws when --env JSON is invalid", () => {
  expect(() => parseEnvJson("{not-json}"))
    .toThrow(new CliError("Invalid --env value: must be valid JSON"));
});

it("throws when --env JSON value is not string", () => {
  expect(() => parseEnvJson('{"GH_TOKEN":123}'))
    .toThrow(new CliError('Invalid --env value: key "GH_TOKEN" must have a string value'));
});
```

- [ ] **Step 2: Run test to verify failure**
Run: `bun test packages/daemon/src/cli/__tests__/index.test.ts`
Expected: FAIL with missing `env` on `DispatchOptions`, missing `parseEnvJson`, and request body assertion mismatch.

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

export function parseEnvJson(raw: string): Record<string, string> {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    throw new CliError("Invalid --env value: must be valid JSON");
  }
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new CliError("Invalid --env value: must be a JSON object");
  }
  for (const [key, value] of Object.entries(parsed as Record<string, unknown>)) {
    if (typeof value !== "string") {
      throw new CliError(`Invalid --env value: key "${key}" must have a string value`);
    }
  }
  return parsed as Record<string, string>;
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
      description: "Worker env as JSON object string",
    },
  },
  async run({ args }) {
    const version = args.version ? Number.parseInt(args.version, 10) : undefined;
    const env = args.env ? parseEnvJson(args.env) : undefined;

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
jj describe -m "feat(cli): support JSON --env forwarding in dispatch"
jj new
```

---

### Task 3: Worker SKILL.md startup env load update — Depends on: Task 1

**Files:**
- Modify: `.opencode/skills/legion-worker/SKILL.md`

- [ ] **Step 1: Implement**
```markdown
<!-- Update .opencode/skills/legion-worker/SKILL.md startup section -->

### Starting

Sync with main and create a fresh commit on your branch:

```bash
jj git fetch
jj rebase -d main
jj new  # Fresh commit for this session
```

Load per-worker environment from daemon (non-blocking):

```bash
# Construct worker ID from issue ID and mode (extracted from dispatch prompt above).
# Example: issue "sjawhar-legion-106" + mode "plan" → worker ID "sjawhar-legion-106-plan"
WORKER_ID="$(echo "${ISSUE_ID}-${MODE}" | tr '[:upper:]' '[:lower:]')"

# Fetch and export env vars (silently skip if daemon unreachable or no env set)
_ENV_FILE="$(mktemp)" && \
  curl -fsS "http://127.0.0.1:${LEGION_DAEMON_PORT}/workers/${WORKER_ID}/env" \
    | bun -e 'const {env}=JSON.parse(await Bun.stdin.text());for(const [k,v] of Object.entries(env)){if(/^[A-Za-z_]\w*$/.test(k))console.log(`export ${k}=${JSON.stringify(v)}`)}' \
    > "$_ENV_FILE" && \
  source "$_ENV_FILE"; \
  rm -f "$_ENV_FILE"
```

Where `ISSUE_ID` and `MODE` are the values you extracted from the dispatch prompt in the "Context from Prompt" section above. For example, if dispatched with "plan mode for sjawhar-legion-106", then `ISSUE_ID=sjawhar-legion-106` and `MODE=plan`.
```

- [ ] **Step 2: Verify SKILL.md changes**

Run: `grep -A 20 'Load per-worker environment' .opencode/skills/legion-worker/SKILL.md`
Expected output must contain:
- `WORKER_ID=` construction from `ISSUE_ID` and `MODE` (lowercased)
- `curl -fsS` to `http://127.0.0.1:${LEGION_DAEMON_PORT}/workers/${WORKER_ID}/env`
- `mktemp` + `source` pattern (no `eval`)
- `rm -f` cleanup of temp file

Run: `grep -c 'eval' .opencode/skills/legion-worker/SKILL.md`
Expected: 0 matches in the startup section (no eval usage)

Smoke test the env export Bun snippet (standalone, no daemon needed):
```bash
echo '{"env":{"GH_TOKEN":"test123","BAD KEY":"skip"}}' | bun -e 'const {env}=JSON.parse(await Bun.stdin.text());for(const [k,v] of Object.entries(env)){if(/^[A-Za-z_]\w*$/.test(k))console.log(`export ${k}=${JSON.stringify(v)}`)}'
```
Expected output: `export GH_TOKEN="test123"` (one line; `BAD KEY` is silently skipped)

- [ ] **Step 3: Describe and advance**
```bash
jj describe -m "docs(worker): load per-worker env from daemon at startup"
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
  - worker env is isolated per worker across dispatches
  - worker env is cleared on worker deletion
- Verify CLI forwarding behavior:
  - `cmdDispatch(..., { env })` includes `env` in outgoing POST body
  - `--env '{"GH_TOKEN":"xxx"}'` parses as JSON object and forwards as string record

### Verification Steps
1. Run daemon test suite for env behavior:
   - `bun test packages/daemon/src/daemon/__tests__/server.test.ts`
   - Expect: all new env endpoint + isolation + cleanup tests PASS
2. Run CLI dispatch tests:
   - `bun test packages/daemon/src/cli/__tests__/index.test.ts`
   - Expect: JSON env parsing/forwarding tests PASS
3. Run focused regression check:
   - `bun test`
   - Expect: full repository test suite PASS
4. SKILL.md env bootstrap verification:
   - Run: `grep -A 20 'Load per-worker environment' .opencode/skills/legion-worker/SKILL.md`
   - Expect: curl → mktemp → source → rm pattern, no `eval`, prompt-derived WORKER_ID
   - Run: `echo '{"env":{"KEY":"val"}}' | bun -e 'const {env}=JSON.parse(await Bun.stdin.text());for(const [k,v] of Object.entries(env)){if(/^[A-Za-z_]\w*$/.test(k))console.log("export "+k+"="+JSON.stringify(v))}'`
   - Expect: `export KEY="val"`

### Tools Needed
- Bun (`bun`, `bun test`)
- jj (`jj describe -m ...`, `jj new`)
- curl (for manual env endpoint verification)
