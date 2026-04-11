# `legion poll` CLI Command Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `legion poll` CLI command that calls `POST /state/fetch-and-collect` and returns a compact, controller-friendly summary of actionable issues.

**Architecture:** New CLI command (`pollCommand`) in the existing citty `defineCommand` pattern. A separate `poll-formatter.ts` module handles categorization (ACTIONABLE / BLOCKED / SUMMARY) and text formatting — kept separate because `index.ts` is already 1100 lines. The daemon's fetch-and-collect response is enhanced with a `titles` map (3-line change in server.ts) so the CLI can display human-readable issue titles.

**Tech Stack:** TypeScript, Bun, citty CLI framework, existing daemon HTTP API

**Constraints:**
- `POST /state/fetch-and-collect` only supports `github` backend currently — hardcode it, no `--backend` flag.
- Formatter exports only `formatPollOutput()` — internal categorization logic stays private.

---

## File Structure

| File | Responsibility |
|------|---------------|
| `packages/daemon/src/cli/poll-formatter.ts` | **Create.** Pure formatting: categorize issues into ACTIONABLE/BLOCKED/SUMMARY, render text. Single public export: `formatPollOutput()`. No I/O. |
| `packages/daemon/src/cli/__tests__/poll-formatter.test.ts` | **Create.** Output-focused tests for `formatPollOutput()`. |
| `packages/daemon/src/cli/index.ts` | **Modify.** Add `cmdPoll()`, `pollCommand`, register in `mainCommand.subCommands`. |
| `packages/daemon/src/daemon/server.ts` | **Modify.** Add `titles` map to fetch-and-collect response (3 lines). |

---

### Task 1: Create poll formatter module — Independent

**Files:**
- Create: `packages/daemon/src/cli/poll-formatter.ts`
- Create: `packages/daemon/src/cli/__tests__/poll-formatter.test.ts`

Pure module (no I/O) that categorizes issues and renders text output.

- [ ] **Step 1: Write failing tests for formatPollOutput**

Create `packages/daemon/src/cli/__tests__/poll-formatter.test.ts`:

```typescript
import { describe, expect, it } from "bun:test";
import { formatPollOutput } from "../poll-formatter";
import type { IssueStateDict } from "../../state/types";

function makeIssue(overrides: Partial<IssueStateDict>): IssueStateDict {
  return {
    status: "In Progress",
    labels: [],
    hasPr: false,
    prIsDraft: null,
    ciStatus: null,
    mergeableStatus: null,
    hasLiveWorker: false,
    workerMode: null,
    workerStatus: null,
    suggestedAction: "skip",
    sessionId: "ses_test",
    hasUserFeedback: false,
    isBlocked: false,
    source: null,
    ...overrides,
  };
}

describe("formatPollOutput", () => {
  it("renders actionable issues grouped by action with titles", () => {
    const issues: Record<string, IssueStateDict> = {
      "acme-repo-42": makeIssue({
        suggestedAction: "dispatch_implementer",
        status: "In Progress",
        source: { owner: "acme", repo: "repo", number: 42, url: "" },
      }),
      "acme-repo-43": makeIssue({
        suggestedAction: "dispatch_implementer",
        status: "In Progress",
        source: { owner: "acme", repo: "repo", number: 43, url: "" },
      }),
      "acme-repo-44": makeIssue({
        suggestedAction: "dispatch_planner",
        status: "Todo",
        source: { owner: "acme", repo: "repo", number: 44, url: "" },
      }),
    };
    const titles: Record<string, string> = {
      "acme-repo-42": "Fix widget alignment",
      "acme-repo-43": "Add dark mode",
      "acme-repo-44": "Redesign settings page",
    };

    const output = formatPollOutput(issues, titles);
    expect(output).toContain("ACTIONABLE (3):");
    expect(output).toContain("dispatch_implementer:");
    expect(output).toContain('#42  In Progress  "Fix widget alignment"');
    expect(output).toContain("dispatch_planner:");
    expect(output).toContain('#44  Todo  "Redesign settings page"');
  });

  it("renders blocked issues (user-input-needed, stale worker-active, dependency-blocked)", () => {
    const issues: Record<string, IssueStateDict> = {
      "acme-repo-50": makeIssue({
        suggestedAction: "skip",
        status: "In Progress",
        labels: ["user-input-needed"],
        source: { owner: "acme", repo: "repo", number: 50, url: "" },
      }),
      "acme-repo-51": makeIssue({
        suggestedAction: "skip",
        status: "In Progress",
        labels: ["worker-active"],
        hasLiveWorker: false,
        source: { owner: "acme", repo: "repo", number: 51, url: "" },
      }),
    };
    const titles: Record<string, string> = {
      "acme-repo-50": "Zendesk ticketing",
      "acme-repo-51": "Profile pic storage",
    };

    const output = formatPollOutput(issues, titles);
    expect(output).toContain("BLOCKED (2):");
    expect(output).toContain('#50  user-input-needed  "Zendesk ticketing"');
    expect(output).toContain('#51  worker-active (stale)  "Profile pic storage"');
  });

  it("renders summary counts for non-actionable skip issues", () => {
    const issues: Record<string, IssueStateDict> = {
      "acme-repo-100": makeIssue({ suggestedAction: "skip", status: "Done" }),
      "acme-repo-101": makeIssue({ suggestedAction: "skip", status: "Done" }),
      "acme-repo-102": makeIssue({ suggestedAction: "skip", status: "Icebox" }),
    };
    const output = formatPollOutput(issues, {});
    expect(output).toContain("SUMMARY:");
    expect(output).toContain("Done: 2");
    expect(output).toContain("Icebox: 1");
  });

  it("omits empty sections", () => {
    const issues: Record<string, IssueStateDict> = {
      "acme-repo-42": makeIssue({
        suggestedAction: "dispatch_planner",
        status: "Todo",
      }),
    };
    const output = formatPollOutput(issues, {});
    expect(output).not.toContain("BLOCKED");
    expect(output).not.toContain("SUMMARY");
  });

  it("falls back to issueId when source is null", () => {
    const issues: Record<string, IssueStateDict> = {
      "acme-repo-42": makeIssue({
        suggestedAction: "dispatch_planner",
        status: "Todo",
        source: null,
      }),
    };
    const output = formatPollOutput(issues, {});
    expect(output).toContain("acme-repo-42  Todo");
  });
});
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `bun test packages/daemon/src/cli/__tests__/poll-formatter.test.ts`
Expected: FAIL — module not found

- [ ] **Step 3: Implement poll-formatter.ts**

Create `packages/daemon/src/cli/poll-formatter.ts`:

```typescript
import type { ActionType, IssueStateDict, IssueSource } from "../state/types";

interface ActionableIssue {
  issueId: string;
  action: ActionType;
  status: string;
  source: IssueSource | null;
}

interface BlockedIssue {
  issueId: string;
  reason: string;
  source: IssueSource | null;
}

interface CategorizedIssues {
  actionable: ActionableIssue[];
  blocked: BlockedIssue[];
  summary: Record<string, number>;
}

function categorizeIssues(
  issues: Record<string, IssueStateDict>,
): CategorizedIssues {
  const actionable: ActionableIssue[] = [];
  const blocked: BlockedIssue[] = [];
  const summary: Record<string, number> = {};

  for (const [issueId, issue] of Object.entries(issues)) {
    if (issue.suggestedAction !== "skip") {
      actionable.push({
        issueId,
        action: issue.suggestedAction,
        status: issue.status,
        source: issue.source,
      });
      continue;
    }

    // Skip items — check for blocking conditions
    if (issue.labels.includes("user-input-needed")) {
      blocked.push({ issueId, reason: "user-input-needed", source: issue.source });
      continue;
    }

    if (issue.labels.includes("worker-active") && !issue.hasLiveWorker) {
      blocked.push({ issueId, reason: "worker-active (stale)", source: issue.source });
      continue;
    }

    if (issue.isBlocked) {
      blocked.push({ issueId, reason: "blocked", source: issue.source });
      continue;
    }

    // Non-actionable, non-blocked — count in summary
    summary[issue.status] = (summary[issue.status] ?? 0) + 1;
  }

  return { actionable, blocked, summary };
}

function issueDisplayId(issueId: string, source: IssueSource | null): string {
  if (source?.number) {
    return `#${source.number}`;
  }
  return issueId;
}

/**
 * Format the fetch-and-collect response into a compact, controller-friendly summary.
 *
 * Output sections (empty sections are omitted):
 * - ACTIONABLE: issues grouped by suggestedAction
 * - BLOCKED: skip issues with blocking labels
 * - SUMMARY: counts of remaining skip issues by status
 */
export function formatPollOutput(
  issues: Record<string, IssueStateDict>,
  titles: Record<string, string>,
): string {
  const { actionable, blocked, summary } = categorizeIssues(issues);
  const lines: string[] = [];

  // ACTIONABLE section
  if (actionable.length > 0) {
    lines.push(`ACTIONABLE (${actionable.length}):`);

    // Group by action
    const byAction = new Map<string, ActionableIssue[]>();
    for (const item of actionable) {
      const group = byAction.get(item.action) ?? [];
      group.push(item);
      byAction.set(item.action, group);
    }

    for (const [action, items] of byAction) {
      lines.push(`  ${action}:`);
      for (const item of items) {
        const id = issueDisplayId(item.issueId, item.source);
        const title = titles[item.issueId];
        const titlePart = title ? `  "${title}"` : "";
        lines.push(`    ${id}  ${item.status}${titlePart}`);
      }
    }
  }

  // BLOCKED section
  if (blocked.length > 0) {
    if (lines.length > 0) lines.push("");
    lines.push(`BLOCKED (${blocked.length}):`);
    for (const item of blocked) {
      const id = issueDisplayId(item.issueId, item.source);
      const title = titles[item.issueId];
      const titlePart = title ? `  "${title}"` : "";
      lines.push(`  ${id}  ${item.reason}${titlePart}`);
    }
  }

  // SUMMARY section
  const summaryEntries = Object.entries(summary);
  if (summaryEntries.length > 0) {
    if (lines.length > 0) lines.push("");
    const parts = summaryEntries.map(([status, count]) => `${status}: ${count}`);
    lines.push("SUMMARY:");
    lines.push(`  ${parts.join(" | ")}`);
  }

  return lines.join("\n");
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `bun test packages/daemon/src/cli/__tests__/poll-formatter.test.ts`
Expected: PASS (all 5 tests)

- [ ] **Step 5: Commit**

```bash
jj describe -m "feat(cli): add poll formatter module"
jj new
```

---

### Task 2: Add cmdPoll, pollCommand, and titles response — Depends on: Task 1

**Files:**
- Modify: `packages/daemon/src/cli/index.ts`
- Modify: `packages/daemon/src/daemon/server.ts:1366`

- [ ] **Step 1: Add titles to fetch-and-collect response**

In `packages/daemon/src/daemon/server.ts`, modify the fetch-and-collect handler's return (around line 1366):

```typescript
// BEFORE:
return jsonResponse(CollectedState.toDict(state));

// AFTER:
const titles: Record<string, string> = {};
for (const [id, title] of extractGitHubIssueTitles(rawIssues)) {
  titles[id] = title;
}
return jsonResponse({ ...CollectedState.toDict(state), titles });
```

- [ ] **Step 2: Add the import to index.ts**

At the top of `packages/daemon/src/cli/index.ts`, add:
```typescript
import { formatPollOutput } from "./poll-formatter";
```

- [ ] **Step 3: Add cmdPoll function**

Add after other `cmd*` functions, before the command definitions:

```typescript
export async function cmdPoll(team: string, opts: { json: boolean }): Promise<void> {
  const legionId = await resolveLegionId(team, {});
  const daemonPort = await getDaemonPort(legionId);
  const baseUrl = `http://127.0.0.1:${daemonPort}`;

  let response: Response;
  try {
    response = await fetch(`${baseUrl}/state/fetch-and-collect`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ backend: "github" }),
    });
  } catch (_error) {
    throw new CliError(
      `Could not connect to daemon. Is it running?\nTried: ${baseUrl}/state/fetch-and-collect`,
    );
  }

  if (!response.ok) {
    const body = await response.text();
    throw new CliError(`Daemon returned ${response.status}: ${body}`);
  }

  const data = (await response.json()) as {
    issues: Record<string, import("../state/types").IssueStateDict>;
    titles?: Record<string, string>;
  };

  if (opts.json) {
    process.stdout.write(`${JSON.stringify(data, null, 2)}\n`);
    return;
  }

  const output = formatPollOutput(data.issues, data.titles ?? {});
  if (output) {
    console.log(output);
  } else {
    console.log("No issues found.");
  }
}
```

- [ ] **Step 4: Add pollCommand definition**

Add alongside other command definitions:

```typescript
export const pollCommand = defineCommand({
  meta: { name: "poll", description: "Poll state machine for compact actionable summary" },
  args: {
    team: { type: "positional", description: "Legion key or UUID", required: true },
    json: {
      type: "boolean",
      description: "Output raw JSON from fetch-and-collect",
      default: false,
    },
  },
  async run({ args }) {
    try {
      await cmdPoll(args.team, { json: args.json });
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

- [ ] **Step 5: Register pollCommand in mainCommand.subCommands**

In the `mainCommand` definition, add `poll: pollCommand` to `subCommands`:

```typescript
subCommands: {
    start: startCommand,
    stop: stopCommand,
    status: statusCommand,
    attach: attachCommand,
    dispatch: dispatchCommand,
    prompt: promptCommand,
    "reset-crashes": resetCrashesCommand,
    teams: legionsCommand,
    "collect-state": collectStateCommand,
    poll: pollCommand,
    handoff: handoffCommand,
},
```

- [ ] **Step 6: Commit**

```bash
jj describe -m "feat(cli): add legion poll command with formatted and JSON output"
jj new
```

---

### Task 3: Verify — lint, typecheck, full test suite — Depends on: Task 2

- [ ] **Step 1: Run Biome lint**

Run: `bunx biome check src/` (cwd: `packages/daemon`)
Expected: Clean. If formatting issues: `bunx biome check --write src/`

- [ ] **Step 2: Run TypeScript type check**

Run: `bunx tsc --noEmit` (cwd: `packages/daemon`)
Expected: No type errors.

- [ ] **Step 3: Run full test suite**

Run: `bun test` (cwd: `packages/daemon`)
Expected: All tests pass including the new formatter tests.

- [ ] **Step 4: Commit if fixes were needed**

Only if steps 1-3 required changes:
```bash
jj describe -m "chore: lint and type fixes for poll command"
jj new
```

---

## Testing Plan

### Setup
- `bun install` (from repo root)
- Ensure a daemon is running: `legion start <team> -w .`

### Health Check
- `curl -s http://127.0.0.1:13370/health` returns `{"status":"ok",...}`
- Retry for 10s before declaring failure

### Verification Steps

1. **Formatted output (default)**
   - Action: `legion poll <team>`
   - Expected: Multi-section output with ACTIONABLE, BLOCKED (if any), SUMMARY sections. Issue titles in quotes.
   - Tool: CLI

2. **JSON output**
   - Action: `legion poll <team> --json`
   - Expected: Raw JSON with `issues` and `titles` fields, parseable by `jq`
   - Tool: CLI + `jq`

3. **Error handling — no daemon**
   - Action: `legion poll nonexistent-team` (with daemon stopped)
   - Expected: Error message about daemon not reachable, non-zero exit code
   - Tool: CLI

### Tools Needed
- CLI (`legion poll`)
- `curl` for health check
- `jq` for JSON validation

### Skills to Invoke
- No project-specific testing skills needed — standard CLI testing with `bun test`

---

## Dependency Graph

```
Task 1 (poll formatter)  ──► Task 2 (cmdPoll + titles + command) ──► Task 3 (verify)
```

- **Task 1**: Independent — pure formatter module with tests
- **Task 2**: Depends on Task 1 — wires formatter to CLI, adds titles to daemon response
- **Task 3**: Depends on Task 2 — final lint/typecheck/test verification
