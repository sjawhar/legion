# CLI Error Refactor: Replace process.exit(1) with typed CliError

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace all `process.exit(1)` calls in CLI command functions with a typed `CliError` exception, caught at the citty `run()` boundary.

**Architecture:** Add a `CliError` class that carries an error message and exit code. All `cmd*` functions throw `CliError` instead of calling `process.exit(1)`. Each citty command's `run()` handler catches `CliError`, prints the message to stderr, and calls `process.exit(code)`. This makes `cmd*` functions composable and testable without `process.exit` mocking.

**Tech Stack:** TypeScript, Bun, citty, Bun test runner

**Special case:** `cmdAttach` line 188 has `process.exit(code ?? 0)` inside a `child.on("exit")` callback — this propagates the child process exit code and is NOT an error. Keep it as-is.

---

### Task 1: Add CliError class and export it

**Files:**
- Modify: `packages/daemon/src/cli/index.ts:1-11`

**Step 1: Add CliError class after imports**

Add after the import block (after line 8):

```typescript
export class CliError extends Error {
  constructor(
    message: string,
    public code = 1,
  ) {
    super(message);
  }
}
```

**Step 2: Run type check**

Run: `bunx tsc --noEmit -p packages/daemon/tsconfig.json`
Expected: PASS (no new errors)

---

### Task 2: Refactor cmdDispatch — replace process.exit(1) with throw CliError

**Files:**
- Modify: `packages/daemon/src/cli/index.ts` (function `cmdDispatch`, lines 196-308)

**Step 1: Replace all `console.error(...); process.exit(1)` pairs**

Each occurrence of:
```typescript
console.error(`Some message`);
process.exit(1);
```

Becomes:
```typescript
throw new CliError(`Some message`);
```

Specific replacements in `cmdDispatch`:

1. Lines 202-203 (invalid issue):
```typescript
// Before:
console.error(`Invalid issue identifier: ${issue} (must match [a-zA-Z0-9_-]+)`);
process.exit(1);
// After:
throw new CliError(`Invalid issue identifier: ${issue} (must match [a-zA-Z0-9_-]+)`);
```

2. Lines 206-207 (invalid mode):
```typescript
throw new CliError(`Invalid mode: ${mode} (must match [a-zA-Z0-9_-]+)`);
```

3. Lines 217-218 (daemon not healthy):
```typescript
throw new CliError("Daemon is not healthy. Is it running?");
```

4. Lines 221-223 (connection failed):
```typescript
throw new CliError(`Could not connect to daemon. Is it running?\nTried: ${baseUrl}/health`);
```

5. Lines 237-238 (workspace creation failed):
```typescript
throw new CliError(`Failed to create workspace: ${stderr}`);
```

6. Lines 250-252 (POST connection failed):
```typescript
throw new CliError(`Could not connect to daemon. Is it running?\nTried: ${baseUrl}/workers`);
```

7. Lines 259-260 (non-JSON response):
```typescript
throw new CliError(`Daemon returned non-JSON response (status ${response.status})`);
```

8. Lines 277-278 (crash limit):
```typescript
throw new CliError(`Crash limit exceeded for ${body.id} (${body.crashCount} crashes)\nReset with: legion reset-crashes ${issue} ${mode}`);
```

9. Lines 282-283 (dispatch failed):
```typescript
throw new CliError(`Failed to dispatch: ${JSON.stringify(body)}`);
```

**Step 2: Update dispatchCommand run() handler**

```typescript
export const dispatchCommand = defineCommand({
  meta: { name: "dispatch", description: "Dispatch a worker for an issue" },
  args: {
    issue: {
      type: "positional",
      description: "Issue identifier (e.g., LEG-42)",
      required: true,
    },
    mode: {
      type: "positional",
      description: "Worker mode (architect, plan, implement, review, merge)",
      required: true,
    },
    prompt: { type: "string", description: "Custom initial prompt (default: /legion-worker)" },
    workspace: { type: "string", alias: "w", description: "Override workspace path" },
  },
  async run({ args }) {
    try {
      await cmdDispatch(args.issue, args.mode, {
        legionDir: process.env.LEGION_DIR,
        prompt: args.prompt,
        workspace: args.workspace,
      });
    } catch (e) {
      if (e instanceof CliError) {
        console.error(e.message);
        process.exit(e.code);
      }
      throw e;
    }
  },
});
```

**Step 3: Run type check**

Run: `bunx tsc --noEmit -p packages/daemon/tsconfig.json`
Expected: PASS

---

### Task 3: Refactor cmdPrompt — replace process.exit(1) with throw CliError

**Files:**
- Modify: `packages/daemon/src/cli/index.ts` (function `cmdPrompt`, lines 310-391)

**Step 1: Replace all process.exit(1) calls**

1. Lines 316-317 (invalid issue):
```typescript
throw new CliError(`Invalid issue identifier: ${issue} (must match [a-zA-Z0-9_-]+)`);
```

2. Lines 325-327 (daemon not responding):
```typescript
throw new CliError("Could not connect to daemon.");
```

3. Lines 331-332 (connection failed):
```typescript
throw new CliError("Could not connect to daemon. Is it running?");
```

4. Lines 347-359 (no worker found) — this one has multi-line output, combine into one message:
```typescript
let msg = `No active worker found for: ${issue}${opts.mode ? ` (mode: ${opts.mode})` : ""}`;
const alive = workers.filter(
  (worker) => worker.status === "running" || worker.status === "starting"
);
if (alive.length > 0) {
  msg += "\n\nActive workers:";
  for (const worker of alive) {
    msg += `\n  - ${worker.id}`;
  }
}
throw new CliError(msg);
```

5. Lines 362-368 (multiple workers) — combine into one message:
```typescript
let msg = `Multiple workers found for ${issue}:`;
for (const worker of matches) {
  msg += `\n  - ${worker.id}`;
}
msg += `\n\nSpecify mode: legion prompt ${issue} --mode <mode> "${prompt}"`;
throw new CliError(msg);
```

6. Lines 383-384 (worker rejected):
```typescript
throw new CliError(`Worker rejected prompt (status ${promptResponse.status}): ${worker.id}`);
```

7. Lines 388-389 (send failed):
```typescript
throw new CliError(`Failed to send prompt to ${worker.id} (port ${worker.port})`);
```

**Step 2: Update promptCommand run() handler**

```typescript
export const promptCommand = defineCommand({
  meta: { name: "prompt", description: "Send a prompt to an existing worker" },
  args: {
    issue: {
      type: "positional",
      description: "Issue identifier (e.g., LEG-42)",
      required: true,
    },
    prompt: { type: "positional", description: "Prompt text to send", required: true },
    mode: { type: "string", description: "Worker mode (to disambiguate)" },
  },
  async run({ args }) {
    try {
      await cmdPrompt(args.issue, args.prompt, { mode: args.mode });
    } catch (e) {
      if (e instanceof CliError) {
        console.error(e.message);
        process.exit(e.code);
      }
      throw e;
    }
  },
});
```

**Step 3: Run type check**

Run: `bunx tsc --noEmit -p packages/daemon/tsconfig.json`
Expected: PASS

---

### Task 4: Refactor cmdResetCrashes — replace process.exit(1) with throw CliError

**Files:**
- Modify: `packages/daemon/src/cli/index.ts` (function `cmdResetCrashes`, lines 393-427)

**Step 1: Replace all process.exit(1) calls**

1. Lines 399-400 (invalid issue):
```typescript
throw new CliError(`Invalid issue identifier: ${issue} (must match [a-zA-Z0-9_-]+)`);
```

2. Lines 403-404 (invalid mode):
```typescript
throw new CliError(`Invalid mode: ${mode} (must match [a-zA-Z0-9_-]+)`);
```

3. Lines 420-421 (failed response):
```typescript
throw new CliError(`Failed to reset crashes: ${response.status}`);
```

4. Lines 424-425 (connection failed):
```typescript
throw new CliError("Could not connect to daemon. Is it running?");
```

**Step 2: Update resetCrashesCommand run() handler**

```typescript
export const resetCrashesCommand = defineCommand({
  meta: { name: "reset-crashes", description: "Reset crash history for a worker" },
  args: {
    issue: { type: "positional", description: "Issue identifier", required: true },
    mode: { type: "positional", description: "Worker mode", required: true },
  },
  async run({ args }) {
    try {
      await cmdResetCrashes(args.issue, args.mode);
    } catch (e) {
      if (e instanceof CliError) {
        console.error(e.message);
        process.exit(e.code);
      }
      throw e;
    }
  },
});
```

**Step 3: Run type check**

Run: `bunx tsc --noEmit -p packages/daemon/tsconfig.json`
Expected: PASS

---

### Task 5: Refactor cmdAttach — replace process.exit(1) with throw CliError

**Files:**
- Modify: `packages/daemon/src/cli/index.ts` (function `cmdAttach`, lines 140-194)

**Step 1: Replace process.exit(1) calls (NOT the child process exit)**

1. Lines 150-151 (daemon not responding):
```typescript
throw new CliError("Could not connect to daemon. Is it running?");
```

2. Lines 160-166 (no worker found) — combine output:
```typescript
let msg = `No worker found for issue: ${issue}`;
msg += "\n\nAvailable workers:";
for (const worker of workers) {
  msg += `\n  - ${worker.id}`;
}
throw new CliError(msg);
```

3. Lines 169-175 (multiple workers) — combine output:
```typescript
let msg = `Multiple workers found for ${issue}:`;
for (const worker of matches) {
  msg += `\n  - ${worker.id} (port ${worker.port})`;
}
msg += "\nBe more specific, e.g.: legion attach eng-21-implement";
throw new CliError(msg);
```

4. Lines 191-192 (attach failed):
```typescript
throw new CliError(`Failed to attach: ${error}`);
```

**KEEP line 188 as-is:** `process.exit(code ?? 0)` — this is child process lifecycle.

**Step 2: Update attachCommand run() handler**

```typescript
export const attachCommand = defineCommand({
  meta: { name: "attach", description: "Attach to a worker session" },
  args: {
    team: { type: "positional", description: "Team key or UUID", required: true },
    issue: { type: "positional", description: "Issue key or identifier", required: true },
  },
  async run({ args }) {
    try {
      await cmdAttach(args.team, args.issue);
    } catch (e) {
      if (e instanceof CliError) {
        console.error(e.message);
        process.exit(e.code);
      }
      throw e;
    }
  },
});
```

**Step 3: Run type check**

Run: `bunx tsc --noEmit -p packages/daemon/tsconfig.json`
Expected: PASS

---

### Task 6: Update tests — remove process.exit mocking, use CliError assertions

**Files:**
- Modify: `packages/daemon/src/cli/__tests__/index.test.ts`

**Step 1: Import CliError**

Add `CliError` to the import from `"../index"`.

**Step 2: Update cmdDispatch tests**

Remove `originalExit` / `process.exit = originalExit` from beforeEach/afterEach.

a) "fails gracefully when daemon is not running" (lines 239-259):
- Remove the `exitMock` and `process.exit` assignment
- Change assertion to:
```typescript
return expect(
  cmdDispatch("LEG-42", "implement", { legionDir, daemonPort: 13373 })
).rejects.toThrow(CliError);
```

b) "reports 429 when crash limit exceeded" (lines 292-326):
- Remove the `exitMock` and `process.exit` assignment
- Change assertion to:
```typescript
return expect(
  cmdDispatch("LEG-42", "implement", { legionDir, daemonPort: 13375 })
).rejects.toThrow(CliError);
```

**Step 3: Update cmdPrompt tests**

Remove `originalExit` / `process.exit = originalExit` from the describe block.

a) "fails when no worker found" (lines 485-520):
- Remove `exitMock` and `process.exit` assignment
- Remove the try/catch wrapper
- Replace with:
```typescript
await expect(
  cmdPrompt("LEG-42", "Check the Linear comments", { daemonPort: 13378 })
).rejects.toThrow(CliError);

// Verify error message content
try {
  await cmdPrompt("LEG-42", "Check the Linear comments", { daemonPort: 13378 });
} catch (e) {
  expect((e as CliError).message).toContain("No active worker found for: LEG-42");
  expect((e as CliError).message).toContain("leg-99-implement");
}
```

Wait — cleaner approach: just catch and check.

```typescript
let caught: CliError | null = null;
try {
  await cmdPrompt("LEG-42", "Check the Linear comments", { daemonPort: 13378 });
} catch (error) {
  caught = error as CliError;
}
expect(caught).toBeInstanceOf(CliError);
expect(caught?.message).toContain("No active worker found for: LEG-42");
expect(caught?.message).toContain("leg-99-implement");
```

**Step 4: Update cmdResetCrashes tests**

Remove `originalExit` / `process.exit = originalExit` from the describe block.

a) "fails when daemon is not running" (lines 556-569):
- Remove `exitMock` and `process.exit` assignment
- Change to:
```typescript
return expect(cmdResetCrashes("LEG-42", "implement", { daemonPort: 13380 })).rejects.toThrow(
  CliError
);
```

b) "exits with error on non-200 response" (lines 572-589):
- Remove `exitMock` and `process.exit` assignment
- Change to:
```typescript
let caught: CliError | null = null;
try {
  await cmdResetCrashes("LEG-42", "implement", { daemonPort: 13381 });
} catch (error) {
  caught = error as CliError;
}
expect(caught).toBeInstanceOf(CliError);
expect(caught?.message).toContain("Failed to reset crashes: 404");
```

**Step 5: Run tests**

Run: `bun test packages/daemon/src/cli/__tests__/index.test.ts`
Expected: ALL PASS

---

### Task 7: Final verification

**Step 1: Run full lint**

Run: `bunx biome check packages/daemon/src/cli/`
Expected: PASS

**Step 2: Run type check**

Run: `bunx tsc --noEmit -p packages/daemon/tsconfig.json`
Expected: PASS

**Step 3: Run full test suite**

Run: `bun test`
Expected: ALL PASS (172 tests)
