# CI Status in State Machine Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add `ciStatus` field to the state machine's collected output so the controller can gate reviewer dispatch and retro transitions on CI health.

**Architecture:** Extend the existing state collection pipeline (`enrichParsedIssues → buildCollectedState`) with a new `getCiStatusBatch()` function that queries GitHub's `statusCheckRollup` via GraphQL, following the same batch-query/retry pattern as `getPrDraftStatusBatch()`. Thread `ciStatus` through all type layers (`FetchedIssueData → IssueState → IssueStateDict`) and add CI gating in `suggestAction()` within the `NEEDS_REVIEW` branch.

**Tech Stack:** TypeScript, Bun runtime, GitHub GraphQL API (`gh api graphql`), bun:test

**Design decisions:**
- `ciStatus: "passing" | "failing" | "pending" | null` — `null` means no PR, no checks configured, or API failure (safe default: don't block)
- `"failing"` → redirect to implementer (`resume_implementer_for_ci_failure`)
- `"pending"` → wait for CI to complete (`retry_ci_check`)
- `"passing"` or `null` → proceed normally
- `prIsDraft` checks take precedence over `ciStatus` (if PR is draft, no need to check CI)
- CI is checked in NEEDS_REVIEW both before dispatching reviewer (worker-done=false) and before transitioning to retro (worker-done=true)
- Separate `getCiStatusBatch()` function (not combined with `getPrDraftStatusBatch`) to avoid changing existing function signatures
- `ciStatus` parameter added as optional with `null` default in `suggestAction()` to avoid test churn on existing tests

---

### Task 1: Add CI status types and interfaces — Independent

**Files:**
- Modify: `packages/daemon/src/state/types.ts`

**Step 1: Add CiStatusLiteral type alias**

Add after `ActionType` definition (after line 62):

```typescript
/**
 * CI check status for a PR.
 * - "passing": all checks succeeded
 * - "failing": one or more checks failed
 * - "pending": checks still running
 * - null: no PR, no checks configured, or couldn't determine
 */
export type CiStatusLiteral = "passing" | "failing" | "pending";
```

**Step 2: Add new ActionType values**

Add to the `ActionType` union (after `"retry_pr_check"`):

```typescript
  | "resume_implementer_for_ci_failure"
  | "retry_ci_check";
```

**Step 3: Add `needsCiStatus` computed getter to `createParsedIssue`**

Add after the `needsPrStatus` getter (after line 307):

```typescript
    get needsCiStatus() {
      return (
        this.status === IssueStatus.NEEDS_REVIEW &&
        this.prRef !== null
      );
    },
```

Also add `readonly needsCiStatus: boolean;` to the `ParsedIssue` interface (after line 245).

**Step 4: Add `ciStatus` field to `FetchedIssueData`**

Add after `prIsDraft` (line 319):

```typescript
  ciStatus: CiStatusLiteral | null; // null if no PR, no checks, or couldn't check
```

**Step 5: Add `ciStatus` field to `IssueStateDict`**

Add after `prIsDraft` (line 339):

```typescript
  ciStatus: CiStatusLiteral | null;
```

**Step 6: Add `ciStatus` field to `IssueState`**

Add after `prIsDraft` (line 363):

```typescript
  ciStatus: CiStatusLiteral | null;
```

**Step 7: Update `IssueState.toDict` to include `ciStatus`**

Add after `prIsDraft` in the dict construction (after line 382):

```typescript
      ciStatus: state.ciStatus,
```

**Step 8: Run type check**

Run: `bunx tsc --noEmit`
Expected: Errors in decision.ts (missing ciStatus in return), fetch.ts (missing ciStatus in enrichment), and test files (missing ciStatus in FetchedIssueData literals). This is expected — subsequent tasks fix these.

**Step 9: Describe and advance**

```bash
jj describe -m "feat(state): add CI status types and interfaces (#62)"
jj new
```

---

### Task 2: Add CI status fetching — Independent

**Files:**
- Modify: `packages/daemon/src/state/fetch.ts`
- Test: `packages/daemon/src/state/__tests__/fetch.test.ts`

**Step 1: Write failing tests for `getCiStatusBatch`**

Add a new `describe("getCiStatusBatch", ...)` block in `fetch.test.ts` after the `getPrDraftStatusBatch` tests (after line 406). Import `getCiStatusBatch` alongside existing imports.

```typescript
describe("getCiStatusBatch", () => {
  it("returns passing for SUCCESS status", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = {
        data: {
          repo0: {
            pr0: {
              commits: {
                nodes: [{ commit: { statusCheckRollup: { state: "SUCCESS" } } }],
              },
            },
          },
        },
      };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    const result = await getCiStatusBatch(
      { "ENG-21": { owner: "owner", repo: "repo", number: 1 } },
      runner
    );
    expect(result).toEqual({ "ENG-21": "passing" });
  });

  it("returns failing for FAILURE status", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = {
        data: {
          repo0: {
            pr0: {
              commits: {
                nodes: [{ commit: { statusCheckRollup: { state: "FAILURE" } } }],
              },
            },
          },
        },
      };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    const result = await getCiStatusBatch(
      { "ENG-21": { owner: "owner", repo: "repo", number: 1 } },
      runner
    );
    expect(result).toEqual({ "ENG-21": "failing" });
  });

  it("returns failing for ERROR status", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = {
        data: {
          repo0: {
            pr0: {
              commits: {
                nodes: [{ commit: { statusCheckRollup: { state: "ERROR" } } }],
              },
            },
          },
        },
      };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    const result = await getCiStatusBatch(
      { "ENG-21": { owner: "owner", repo: "repo", number: 1 } },
      runner
    );
    expect(result).toEqual({ "ENG-21": "failing" });
  });

  it("returns pending for PENDING status", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = {
        data: {
          repo0: {
            pr0: {
              commits: {
                nodes: [{ commit: { statusCheckRollup: { state: "PENDING" } } }],
              },
            },
          },
        },
      };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    const result = await getCiStatusBatch(
      { "ENG-21": { owner: "owner", repo: "repo", number: 1 } },
      runner
    );
    expect(result).toEqual({ "ENG-21": "pending" });
  });

  it("returns pending for EXPECTED status", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = {
        data: {
          repo0: {
            pr0: {
              commits: {
                nodes: [{ commit: { statusCheckRollup: { state: "EXPECTED" } } }],
              },
            },
          },
        },
      };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    const result = await getCiStatusBatch(
      { "ENG-21": { owner: "owner", repo: "repo", number: 1 } },
      runner
    );
    expect(result).toEqual({ "ENG-21": "pending" });
  });

  it("returns null when statusCheckRollup is null (no checks configured)", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = {
        data: {
          repo0: {
            pr0: {
              commits: {
                nodes: [{ commit: { statusCheckRollup: null } }],
              },
            },
          },
        },
      };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    const result = await getCiStatusBatch(
      { "ENG-21": { owner: "owner", repo: "repo", number: 1 } },
      runner
    );
    expect(result).toEqual({ "ENG-21": null });
  });

  it("returns null when commits nodes is empty", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = {
        data: {
          repo0: {
            pr0: {
              commits: { nodes: [] },
            },
          },
        },
      };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    const result = await getCiStatusBatch(
      { "ENG-21": { owner: "owner", repo: "repo", number: 1 } },
      runner
    );
    expect(result).toEqual({ "ENG-21": null });
  });

  it("returns null for missing PR", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = { data: { repo0: { pr0: null } } };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    const result = await getCiStatusBatch(
      { "ENG-21": { owner: "owner", repo: "repo", number: 999 } },
      runner
    );
    expect(result).toEqual({ "ENG-21": null });
  });

  it("handles multiple PRs across repos", async () => {
    const queriesReceived: string[] = [];
    const runner: CommandRunner = async (cmd: string[]) => {
      const query = cmd[cmd.length - 1];
      queriesReceived.push(query);
      const response = {
        data: {
          repo0: {
            pr0: {
              commits: {
                nodes: [{ commit: { statusCheckRollup: { state: "SUCCESS" } } }],
              },
            },
          },
          repo1: {
            pr0: {
              commits: {
                nodes: [{ commit: { statusCheckRollup: { state: "FAILURE" } } }],
              },
            },
          },
        },
      };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    const result = await getCiStatusBatch(
      {
        "ENG-21": { owner: "org", repo: "repo1", number: 1 },
        "ENG-22": { owner: "org", repo: "repo2", number: 2 },
      },
      runner
    );

    expect(queriesReceived).toHaveLength(1);
    expect(result).toEqual({ "ENG-21": "passing", "ENG-22": "failing" });
  });

  it("retries on failure with exponential backoff", async () => {
    let callCount = 0;
    const runner: CommandRunner = async (_cmd: string[]) => {
      callCount++;
      if (callCount < 3) {
        return { stdout: "", stderr: "rate limited", exitCode: 1 };
      }
      const response = {
        data: {
          repo0: {
            pr0: {
              commits: {
                nodes: [{ commit: { statusCheckRollup: { state: "SUCCESS" } } }],
              },
            },
          },
        },
      };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    const result = await getCiStatusBatch(
      { "ENG-21": { owner: "owner", repo: "repo", number: 1 } },
      runner
    );
    expect(result).toEqual({ "ENG-21": "passing" });
    expect(callCount).toBe(3);
  });

  it("throws GitHubAPIError after all retries fail", async () => {
    let callCount = 0;
    const runner: CommandRunner = async (_cmd: string[]) => {
      callCount++;
      return { stdout: "", stderr: "rate limited", exitCode: 1 };
    };

    await Promise.resolve(
      expect(
        getCiStatusBatch({ "ENG-21": { owner: "owner", repo: "repo", number: 1 } }, runner)
      ).rejects.toThrow(GitHubAPIError)
    );
    expect(callCount).toBe(3);
  });

  it("returns empty result for empty pr_refs", async () => {
    let callCount = 0;
    const runner: CommandRunner = async (_cmd: string[]) => {
      callCount++;
      return { stdout: "{}", stderr: "", exitCode: 0 };
    };

    const result = await getCiStatusBatch({}, runner);
    expect(result).toEqual({});
    expect(callCount).toBe(0);
  });

  it("returns null for unknown status string", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = {
        data: {
          repo0: {
            pr0: {
              commits: {
                nodes: [{ commit: { statusCheckRollup: { state: "UNKNOWN_VALUE" } } }],
              },
            },
          },
        },
      };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    const result = await getCiStatusBatch(
      { "ENG-21": { owner: "owner", repo: "repo", number: 1 } },
      runner
    );
    expect(result).toEqual({ "ENG-21": null });
  });
});
```

**Step 2: Run tests to verify they fail**

Run: `bun test packages/daemon/src/state/__tests__/fetch.test.ts`
Expected: FAIL — `getCiStatusBatch` is not defined/exported

**Step 3: Add `mapCiRollupState` mapping function in `fetch.ts`**

Add after the `sleep` function (after line 142), before the `getPrDraftStatusBatch` section:

```typescript
// =============================================================================
// CI Status Mapping
// =============================================================================

/**
 * Map GitHub statusCheckRollup state to CiStatusLiteral.
 *
 * GitHub GraphQL statusCheckRollup.state values:
 * - SUCCESS → "passing"
 * - FAILURE, ERROR → "failing"
 * - PENDING, EXPECTED → "pending"
 * - null or unknown → null
 */
export function mapCiRollupState(state: string | null | undefined): CiStatusLiteral | null {
  if (state === null || state === undefined) {
    return null;
  }
  switch (state) {
    case "SUCCESS":
      return "passing";
    case "FAILURE":
    case "ERROR":
      return "failing";
    case "PENDING":
    case "EXPECTED":
      return "pending";
    default:
      return null;
  }
}
```

Also add the import for `CiStatusLiteral` at the top of fetch.ts:

```typescript
import type {
  CiStatusLiteral,
  FetchedIssueData,
  GitHubPRRef as GitHubPRRefType,
  LinearIssueRaw,
  ParsedIssue,
} from "./types";
```

**Step 4: Implement `getCiStatusBatch` in `fetch.ts`**

Add after the `getPrDraftStatusBatch` function (after line 280). This follows the exact same pattern:

```typescript
// =============================================================================
// GitHub CI Status Fetching
// =============================================================================

/**
 * Fetch CI status for multiple PRs in a single GraphQL query.
 *
 * Uses statusCheckRollup on the latest commit of each PR.
 * Batches all PRs across all repositories into one API call.
 * Retries up to 3 times with exponential backoff on failure.
 *
 * @param prRefs - Dict mapping issue_id to GitHubPRRef
 * @param runner - Command runner for testing
 * @returns Dict mapping issue_id to CI status
 * @throws GitHubAPIError if GraphQL query fails after retries
 */
export async function getCiStatusBatch(
  prRefs: Record<string, GitHubPRRefType>,
  runner: CommandRunner = defaultRunner
): Promise<Record<string, CiStatusLiteral | null>> {
  if (Object.keys(prRefs).length === 0) {
    return {};
  }

  // Group by repository for query structure
  const byRepo = new Map<string, Array<[string, number]>>();
  for (const [issueId, ref] of Object.entries(prRefs)) {
    const key = `${ref.owner}/${ref.repo}`;
    if (!byRepo.has(key)) {
      byRepo.set(key, []);
    }
    byRepo.get(key)?.push([issueId, ref.number]);
  }

  // Build single GraphQL query for all repos and PRs
  const repoAliasMap = new Map<string, [string, string]>();
  const prAliasMap = new Map<string, Map<string, [string, number]>>();

  const queryParts: string[] = [];
  let repoIdx = 0;
  for (const [repoKey, issuePrs] of byRepo) {
    const [owner, repo] = repoKey.split("/");
    const repoAlias = `repo${repoIdx}`;
    repoAliasMap.set(repoAlias, [owner, repo]);
    prAliasMap.set(repoAlias, new Map());

    const prParts: string[] = [];
    for (let prIdx = 0; prIdx < issuePrs.length; prIdx++) {
      const [issueId, prNumber] = issuePrs[prIdx];
      const prAlias = `pr${prIdx}`;
      prAliasMap.get(repoAlias)?.set(prAlias, [issueId, prNumber]);
      prParts.push(
        `${prAlias}: pullRequest(number: ${prNumber}) { commits(last: 1) { nodes { commit { statusCheckRollup { state } } } } }`
      );
    }

    queryParts.push(
      `${repoAlias}: repository(owner: "${owner}", name: "${repo}") { ${prParts.join(" ")} }`
    );
    repoIdx++;
  }

  const query = `query { ${queryParts.join(" ")} }`;

  // Retry loop with exponential backoff (3 attempts)
  const maxAttempts = 3;
  let lastError: GitHubAPIError = new GitHubAPIError("All retry attempts failed");

  for (let attempt = 0; attempt < maxAttempts; attempt++) {
    if (attempt > 0) {
      const waitMs = Math.min(2 ** (attempt - 1) * 1000, 10000);
      await sleep(waitMs);
    }

    const { stdout, stderr, exitCode } = await runner([
      "gh",
      "api",
      "graphql",
      "-f",
      `query=${query}`,
    ]);

    if (exitCode !== 0) {
      lastError = new GitHubAPIError(`GraphQL query failed: ${stderr}`);
      continue;
    }

    let response: { data?: unknown };
    try {
      response = JSON.parse(stdout);
    } catch (e) {
      lastError = new GitHubAPIError(`Failed to parse GraphQL response: ${e}`);
      continue;
    }

    // Success - parse response
    const rawData = response.data;
    const dataObj: Record<string, unknown> =
      rawData !== null &&
      rawData !== undefined &&
      typeof rawData === "object" &&
      !Array.isArray(rawData)
        ? (rawData as Record<string, unknown>)
        : {};

    const result: Record<string, CiStatusLiteral | null> = {};

    for (const [repoAlias, [_owner, _repo]] of repoAliasMap) {
      const rawRepo = dataObj[repoAlias];
      const repoData: Record<string, unknown> =
        rawRepo !== null &&
        rawRepo !== undefined &&
        typeof rawRepo === "object" &&
        !Array.isArray(rawRepo)
          ? (rawRepo as Record<string, unknown>)
          : {};

      const prAliases = prAliasMap.get(repoAlias) ?? new Map();
      for (const [prAlias, [issueId]] of prAliases) {
        const rawPr = repoData[prAlias] as Record<string, unknown> | null | undefined;
        if (
          rawPr === null ||
          rawPr === undefined ||
          typeof rawPr !== "object" ||
          Array.isArray(rawPr)
        ) {
          result[issueId] = null;
          continue;
        }

        // Navigate: pr.commits.nodes[0].commit.statusCheckRollup.state
        const commits = rawPr.commits as { nodes?: unknown[] } | null | undefined;
        const nodes = commits?.nodes;
        if (!Array.isArray(nodes) || nodes.length === 0) {
          result[issueId] = null;
          continue;
        }

        const firstNode = nodes[0] as { commit?: { statusCheckRollup?: { state?: string | null } | null } } | null;
        const rollupState = firstNode?.commit?.statusCheckRollup?.state ?? null;
        result[issueId] = mapCiRollupState(rollupState);
      }
    }

    return result;
  }

  throw lastError;
}
```

**Step 5: Run tests to verify they pass**

Run: `bun test packages/daemon/src/state/__tests__/fetch.test.ts`
Expected: All `getCiStatusBatch` tests PASS. Some `enrichParsedIssues` tests may fail due to missing `ciStatus` field (fixed in Task 3).

**Step 6: Describe and advance**

```bash
jj describe -m "feat(state): add getCiStatusBatch for CI status fetching (#62)"
jj new
```

---

### Task 3: Wire CI status through enrichment pipeline — Depends on: Task 1, Task 2

**Files:**
- Modify: `packages/daemon/src/state/fetch.ts`
- Modify: `packages/daemon/src/state/__tests__/fetch.test.ts`

**Step 1: Write failing test for enrichment with CI status**

Add to the `enrichParsedIssues` describe block (after line 531):

```typescript
  it("enriches issues with CI status from GitHub API", async () => {
    const originalFetch = globalThis.fetch;
    globalThis.fetch = mock(
      async () => new Response(JSON.stringify([]), { status: 200 })
    ) as unknown as typeof fetch;

    try {
      const mockRunner: CommandRunner = async (cmd: string[]) => {
        const queryArg = cmd[cmd.length - 1];
        // Draft status query
        if (queryArg.includes("isDraft")) {
          return {
            stdout: JSON.stringify({ data: { repo0: { pr0: { isDraft: false } } } }),
            stderr: "",
            exitCode: 0,
          };
        }
        // CI status query
        if (queryArg.includes("statusCheckRollup")) {
          return {
            stdout: JSON.stringify({
              data: {
                repo0: {
                  pr0: {
                    commits: {
                      nodes: [{ commit: { statusCheckRollup: { state: "SUCCESS" } } }],
                    },
                  },
                },
              },
            }),
            stderr: "",
            exitCode: 0,
          };
        }
        return { stdout: "{}", stderr: "", exitCode: 0 };
      };

      const issues = [
        createParsedIssue(
          "ENG-21",
          "Needs Review",
          ["worker-done"],
          { owner: "owner", repo: "repo", number: 1 }
        ),
      ];
      const result = await enrichParsedIssues(issues, "http://127.0.0.1:99999", mockRunner);

      expect(result).toHaveLength(1);
      expect(result[0].ciStatus).toBe("passing");
      expect(result[0].prIsDraft).toBe(false);
    } finally {
      globalThis.fetch = originalFetch;
    }
  });

  it("sets ciStatus to null when no PR", async () => {
    const originalFetch = globalThis.fetch;
    globalThis.fetch = mock(async () => {
      throw new Error("Connection refused");
    }) as unknown as typeof fetch;

    try {
      const mockRunner: CommandRunner = async () => ({ stdout: "", stderr: "", exitCode: 1 });
      const issues = [createParsedIssue("ENG-21", "Needs Review", [], null)];
      const result = await enrichParsedIssues(issues, "http://127.0.0.1:99999", mockRunner);

      expect(result).toHaveLength(1);
      expect(result[0].ciStatus).toBeNull();
    } finally {
      globalThis.fetch = originalFetch;
    }
  });

  it("sets ciStatus to null on API failure", async () => {
    const originalFetch = globalThis.fetch;
    globalThis.fetch = mock(
      async () => new Response(JSON.stringify([]), { status: 200 })
    ) as unknown as typeof fetch;

    try {
      const mockRunner: CommandRunner = async () => ({
        stdout: "",
        stderr: "rate limited",
        exitCode: 1,
      });

      const issues = [
        createParsedIssue(
          "ENG-21",
          "Needs Review",
          [],
          { owner: "owner", repo: "repo", number: 1 }
        ),
      ];
      const result = await enrichParsedIssues(issues, "http://127.0.0.1:99999", mockRunner);

      expect(result).toHaveLength(1);
      expect(result[0].ciStatus).toBeNull();
    } finally {
      globalThis.fetch = originalFetch;
    }
  });
```

**Step 2: Run tests to verify they fail**

Run: `bun test packages/daemon/src/state/__tests__/fetch.test.ts`
Expected: FAIL — `ciStatus` not in result

**Step 3: Update `enrichParsedIssues` to collect CI refs and call `getCiStatusBatch`**

In `fetch.ts`, update `enrichParsedIssues` (starting at line 290):

Add CI ref collection after the PR ref collection (after line 300):

```typescript
  const ciRefsForStatus: Record<string, GitHubPRRefType> = {};
  for (const p of parsedIssues) {
    if (p.needsCiStatus && p.prRef !== null) {
      ciRefsForStatus[p.issueId] = p.prRef;
    }
  }
```

Add `ciStatusMap` variable alongside `prDraftMap` (after line 303):

```typescript
  let ciStatusMap: Record<string, CiStatusLiteral | null> = {};
```

Add CI status fetch as a third parallel operation in the `Promise.all` (after the prDraftMap block, before the `]);`):

```typescript
    (async () => {
      if (Object.keys(ciRefsForStatus).length === 0) {
        return;
      }
      try {
        ciStatusMap = await getCiStatusBatch(ciRefsForStatus, runner);
      } catch {
        for (const issueId of Object.keys(ciRefsForStatus)) {
          ciStatusMap[issueId] = null;
        }
      }
    })(),
```

Add `ciStatus` to the return mapping (after `prIsDraft` line 330):

```typescript
      ciStatus: ciStatusMap[issue.issueId] ?? null,
```

**Step 4: Run tests to verify they pass**

Run: `bun test packages/daemon/src/state/__tests__/fetch.test.ts`
Expected: All tests PASS

**Step 5: Describe and advance**

```bash
jj describe -m "feat(state): wire CI status through enrichment pipeline (#62)"
jj new
```

---

### Task 4: Update decision logic with CI gating — Depends on: Task 1

**Files:**
- Modify: `packages/daemon/src/state/decision.ts`
- Test: `packages/daemon/src/state/__tests__/decision.test.ts`
- Test: `packages/daemon/src/state/__tests__/decision-regressions.test.ts`

**Step 1: Write failing tests for CI gating in suggestAction**

Add a new describe block in `decision.test.ts` after the existing `suggestAction` describe (after line 158):

```typescript
describe("suggestAction CI gating", () => {
  it("needs_review_worker_done_ci_failing_resumes_implementer", () => {
    const action = suggestAction(
      IssueStatus.NEEDS_REVIEW, true, false, false, true, false, "failing"
    );
    expect(action).toBe("resume_implementer_for_ci_failure");
  });

  it("needs_review_worker_done_ci_pending_retries", () => {
    const action = suggestAction(
      IssueStatus.NEEDS_REVIEW, true, false, false, true, false, "pending"
    );
    expect(action).toBe("retry_ci_check");
  });

  it("needs_review_worker_done_ci_passing_transitions_to_retro", () => {
    const action = suggestAction(
      IssueStatus.NEEDS_REVIEW, true, false, false, true, false, "passing"
    );
    expect(action).toBe("transition_to_retro");
  });

  it("needs_review_worker_done_ci_null_transitions_to_retro", () => {
    const action = suggestAction(
      IssueStatus.NEEDS_REVIEW, true, false, false, true, false, null
    );
    expect(action).toBe("transition_to_retro");
  });

  it("needs_review_no_worker_done_ci_failing_resumes_implementer", () => {
    const action = suggestAction(
      IssueStatus.NEEDS_REVIEW, false, false, null, true, false, "failing"
    );
    expect(action).toBe("resume_implementer_for_ci_failure");
  });

  it("needs_review_no_worker_done_ci_pending_retries", () => {
    const action = suggestAction(
      IssueStatus.NEEDS_REVIEW, false, false, null, true, false, "pending"
    );
    expect(action).toBe("retry_ci_check");
  });

  it("needs_review_no_worker_done_ci_passing_dispatches_reviewer", () => {
    const action = suggestAction(
      IssueStatus.NEEDS_REVIEW, false, false, null, true, false, "passing"
    );
    expect(action).toBe("dispatch_reviewer");
  });

  it("needs_review_no_worker_done_ci_null_dispatches_reviewer", () => {
    const action = suggestAction(
      IssueStatus.NEEDS_REVIEW, false, false, null, true, false, null
    );
    expect(action).toBe("dispatch_reviewer");
  });

  it("ci_check_not_done_when_pr_is_draft", () => {
    // prIsDraft takes precedence — if PR is draft, redirect to implementer
    // without checking CI
    const action = suggestAction(
      IssueStatus.NEEDS_REVIEW, true, false, true, true, false, "failing"
    );
    expect(action).toBe("resume_implementer_for_changes");
  });

  it("ci_check_not_done_when_no_pr", () => {
    const action = suggestAction(
      IssueStatus.NEEDS_REVIEW, true, false, null, false, false, "failing"
    );
    expect(action).toBe("investigate_no_pr");
  });

  it("needs_review_live_worker_skips_regardless_of_ci", () => {
    const action = suggestAction(
      IssueStatus.NEEDS_REVIEW, false, true, null, true, false, "failing"
    );
    expect(action).toBe("skip");
  });

  it("existing_tests_work_without_ci_parameter", () => {
    // Verify backward compat: omitting ciStatus defaults to null (proceed)
    const action = suggestAction(IssueStatus.NEEDS_REVIEW, true, false, false, true, false);
    expect(action).toBe("transition_to_retro");
  });
});
```

**Step 2: Run tests to verify they fail**

Run: `bun test packages/daemon/src/state/__tests__/decision.test.ts`
Expected: FAIL — new action types not recognized, ciStatus parameter not accepted

**Step 3: Update `suggestAction` to accept and use `ciStatus`**

In `decision.ts`, add the import for `CiStatusLiteral`:

```typescript
import {
  type ActionType,
  type CiStatusLiteral,
  type CollectedState,
  // ... rest of imports
} from "./types";
```

Update `suggestAction` signature (line 20) to add optional `ciStatus` parameter:

```typescript
export function suggestAction(
  status: IssueStatusLiteral | string,
  hasWorkerDone: boolean,
  hasLiveWorker: boolean,
  prIsDraft: boolean | null,
  hasPr: boolean,
  hasTestPassed: boolean,
  ciStatus: CiStatusLiteral | null = null
): ActionType {
```

Update the `NEEDS_REVIEW` case (lines 78-94). Replace the entire case with:

```typescript
    case IssueStatus.NEEDS_REVIEW:
      if (hasWorkerDone) {
        if (!hasPr) {
          return "investigate_no_pr";
        }
        if (prIsDraft === null) {
          return "retry_pr_check";
        }
        if (prIsDraft) {
          return "resume_implementer_for_changes";
        }
        // PR is ready (not draft) — check CI before proceeding
        if (ciStatus === "failing") {
          return "resume_implementer_for_ci_failure";
        }
        if (ciStatus === "pending") {
          return "retry_ci_check";
        }
        return "transition_to_retro";
      }
      if (hasLiveWorker) {
        return "skip";
      }
      // No worker-done, no live worker — check CI before dispatching reviewer
      if (hasPr && ciStatus === "failing") {
        return "resume_implementer_for_ci_failure";
      }
      if (hasPr && ciStatus === "pending") {
        return "retry_ci_check";
      }
      return "dispatch_reviewer";
```

**Step 4: Update `ACTION_TO_MODE` for new action types**

Add after `retry_pr_check` (line 128):

```typescript
  resume_implementer_for_ci_failure: WorkerMode.IMPLEMENT,
  retry_ci_check: WorkerMode.REVIEW,
```

**Step 5: Update `buildIssueState` to pass `ciStatus` to `suggestAction`**

In the `else` branch of `buildIssueState` (line 148), update the `suggestAction` call:

```typescript
    action = suggestAction(
      data.status,
      data.labels.includes("worker-done"),
      data.hasLiveWorker,
      data.prIsDraft,
      data.hasPr,
      data.hasTestPassed ?? false,
      data.ciStatus
    );
```

**Step 6: Add `ciStatus` to the `IssueState` return object**

In `buildIssueState`, add after `prIsDraft` in the return (after line 168):

```typescript
    ciStatus: data.ciStatus,
```

**Step 7: Run tests to verify they pass**

Run: `bun test packages/daemon/src/state/__tests__/decision.test.ts`
Expected: All tests PASS (existing tests use default null ciStatus, new CI tests use explicit values)

**Step 8: Describe and advance**

```bash
jj describe -m "feat(state): add CI gating in NEEDS_REVIEW decision logic (#62)"
jj new
```

---

### Task 5: Update existing tests for new `ciStatus` field — Depends on: Task 1, Task 3, Task 4

**Files:**
- Modify: `packages/daemon/src/state/__tests__/decision.test.ts`
- Modify: `packages/daemon/src/state/__tests__/decision-regressions.test.ts`
- Modify: `packages/daemon/src/state/__tests__/fetch.test.ts`

**Step 1: Add `ciStatus: null` to all `FetchedIssueData` literals in `decision.test.ts`**

Every `const data: FetchedIssueData = { ... }` block in `decision.test.ts` and `decision-regressions.test.ts` needs `ciStatus: null` added after `prIsDraft`. This is a mechanical change — add the field to every FetchedIssueData object literal in both test files.

Pattern to follow for each occurrence:
```typescript
// Before:
      prIsDraft: null,
      hasLiveWorker: false,
// After:
      prIsDraft: null,
      ciStatus: null,
      hasLiveWorker: false,
```

**Step 2: Add `ciStatus: null` to `FetchedIssueData` objects in `fetch.test.ts` (enrichParsedIssues tests)**

Same mechanical change for any `FetchedIssueData` objects or result assertions that check fields. For `enrichParsedIssues` test result assertions, also verify `ciStatus` is present:

```typescript
// In existing enrichParsedIssues tests, add assertion:
expect(result[0].ciStatus).toBeNull();
```

**Step 3: Run full test suite to verify**

Run: `bun test packages/daemon/src/state/__tests__/`
Expected: All tests PASS

**Step 4: Run type check and lint**

Run: `bunx tsc --noEmit && bunx biome check src/`
Expected: Exit code 0

**Step 5: Describe and advance**

```bash
jj describe -m "test(state): update existing tests for ciStatus field (#62)"
jj new
```

---

### Task 6: Add regression tests and verify end-to-end — Depends on: Task 5

**Files:**
- Modify: `packages/daemon/src/state/__tests__/decision-regressions.test.ts`

**Step 1: Add CI-specific regression tests**

Add to `decision-regressions.test.ts`:

```typescript
  it("does not advance to retro when CI is failing on a ready PR", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-23",
      status: "Needs Review",
      labels: ["worker-done"],
      hasPr: true,
      prIsDraft: false,
      ciStatus: "failing",
      hasLiveWorker: false,
      workerMode: null,
      workerStatus: null,
      hasUserFeedback: false,
      hasUserInputNeeded: false,
      hasNeedsApproval: false,
      hasHumanApproved: false,
      hasTestPassed: false,
      hasTestFailed: false,
      source: null,
    };

    const state = buildIssueState(data, "00000000-0000-0000-0000-000000000000");
    expect(state.suggestedAction).toBe("resume_implementer_for_ci_failure");
  });

  it("does not dispatch reviewer when CI is failing", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-24",
      status: "Needs Review",
      labels: [],
      hasPr: true,
      prIsDraft: null,
      ciStatus: "failing",
      hasLiveWorker: false,
      workerMode: null,
      workerStatus: null,
      hasUserFeedback: false,
      hasUserInputNeeded: false,
      hasNeedsApproval: false,
      hasHumanApproved: false,
      hasTestPassed: false,
      hasTestFailed: false,
      source: null,
    };

    const state = buildIssueState(data, "00000000-0000-0000-0000-000000000000");
    expect(state.suggestedAction).toBe("resume_implementer_for_ci_failure");
  });

  it("waits when CI is pending before dispatching reviewer", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-25",
      status: "Needs Review",
      labels: [],
      hasPr: true,
      prIsDraft: null,
      ciStatus: "pending",
      hasLiveWorker: false,
      workerMode: null,
      workerStatus: null,
      hasUserFeedback: false,
      hasUserInputNeeded: false,
      hasNeedsApproval: false,
      hasHumanApproved: false,
      hasTestPassed: false,
      hasTestFailed: false,
      source: null,
    };

    const state = buildIssueState(data, "00000000-0000-0000-0000-000000000000");
    expect(state.suggestedAction).toBe("retry_ci_check");
  });

  it("proceeds normally when CI is null (no checks configured)", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-26",
      status: "Needs Review",
      labels: ["worker-done"],
      hasPr: true,
      prIsDraft: false,
      ciStatus: null,
      hasLiveWorker: false,
      workerMode: null,
      workerStatus: null,
      hasUserFeedback: false,
      hasUserInputNeeded: false,
      hasNeedsApproval: false,
      hasHumanApproved: false,
      hasTestPassed: false,
      hasTestFailed: false,
      source: null,
    };

    const state = buildIssueState(data, "00000000-0000-0000-0000-000000000000");
    expect(state.suggestedAction).toBe("transition_to_retro");
  });
```

**Step 2: Run full test suite**

Run: `bun test packages/daemon/src/state/__tests__/`
Expected: All tests PASS, 0 failures

**Step 3: Run type check and lint**

Run: `bunx tsc --noEmit && bunx biome check src/`
Expected: Exit code 0

**Step 4: Run the full project test suite**

Run: `bun test`
Expected: All tests PASS. If pre-existing failures exist, note them.

**Step 5: Describe and advance**

```bash
jj describe -m "test(state): add CI gating regression tests (#62)"
jj new
```

---

### Task 7: Update AGENTS.md state machine documentation — Depends on: Task 4

**Files:**
- Modify: `packages/daemon/src/state/AGENTS.md`

**Step 1: Update the ActionType State Machine table**

Add CI-related rows to the table in AGENTS.md:

| Status | worker-done? | live worker? | PR state | CI status | test labels | → Action |
|--------|-------------|-------------|----------|-----------|-------------|----------|
| Needs Review | yes | — | ready | failing | — | `resume_implementer_for_ci_failure` |
| Needs Review | yes | — | ready | pending | — | `retry_ci_check` |
| Needs Review | no | no | has PR | failing | — | `resume_implementer_for_ci_failure` |
| Needs Review | no | no | has PR | pending | — | `retry_ci_check` |

**Step 2: Add note about CI status field**

Add a note in the "Data Flow" section mentioning `ciStatus` is fetched via `getCiStatusBatch()` alongside `getPrDraftStatusBatch()`.

**Step 3: Describe and advance**

```bash
jj describe -m "docs(state): document CI status gating in AGENTS.md (#62)"
jj new
```

---

## Dependency Graph

```
Task 1 (types) ─────────────┬─────► Task 3 (wire enrichment) ──► Task 5 (update tests) ──► Task 6 (regression tests)
                             │                                                                      │
Task 2 (getCiStatusBatch) ──┘                                                                       │
                                                                                                     ▼
Task 4 (decision logic) ────────────────────────────────────────────────────────────────────► Task 7 (docs)
```

```
Task 1: Add CI status types and interfaces — Independent
Task 2: Add CI status fetching — Independent
Task 3: Wire CI status through enrichment — Depends on: Task 1, Task 2
Task 4: Update decision logic with CI gating — Depends on: Task 1
Task 5: Update existing tests for ciStatus field — Depends on: Task 1, Task 3, Task 4
Task 6: Add regression tests and verify end-to-end — Depends on: Task 5
Task 7: Update AGENTS.md documentation — Depends on: Task 4
```

---

## Testing Plan

### Setup
- `bun install` (if dependencies not already installed)
- No servers or infrastructure needed — all tests use mocked dependencies

### Health Check
- `bun test packages/daemon/src/state/__tests__/decision.test.ts` returns exit code 0
- `bun test packages/daemon/src/state/__tests__/fetch.test.ts` returns exit code 0

### Verification Steps

1. **CI status type integrity**
   - Action: `bunx tsc --noEmit`
   - Expected: Exit code 0, no type errors
   - Tool: CLI

2. **CI status fetching**
   - Action: `bun test packages/daemon/src/state/__tests__/fetch.test.ts`
   - Expected: All `getCiStatusBatch` tests pass (SUCCESS/FAILURE/ERROR/PENDING/EXPECTED mapping, null handling, retry, batch)
   - Tool: bun:test

3. **Decision logic CI gating**
   - Action: `bun test packages/daemon/src/state/__tests__/decision.test.ts`
   - Expected: All CI gating tests pass (failing→implementer, pending→retry, passing→proceed, null→proceed)
   - Tool: bun:test

4. **Regression tests**
   - Action: `bun test packages/daemon/src/state/__tests__/decision-regressions.test.ts`
   - Expected: All CI regression tests pass (no retro with failing CI, no reviewer with failing CI)
   - Tool: bun:test

5. **Lint and format**
   - Action: `bunx biome check src/`
   - Expected: Exit code 0
   - Tool: CLI

6. **Full test suite**
   - Action: `bun test`
   - Expected: All tests pass (or only pre-existing failures noted)
   - Tool: bun:test

### Tools Needed
- `bun test` for test execution
- `bunx tsc --noEmit` for type checking
- `bunx biome check` for lint/format verification
