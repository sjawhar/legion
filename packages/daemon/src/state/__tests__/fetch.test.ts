/**
 * Tests for state fetch logic.
 *
 * Ported from Python tests:
 * - tests/test_state.py (TestParseLinearIssues, TestGetPrDraftStatusBatch,
 *   TestParseLinearIssuesEdgeCases, TestFetchAllIssueData, TestFetchAllIssueDataErrorHandling)
 */

import { describe, expect, it, mock } from "bun:test";
import {
  type CommandRunner,
  fetchAllIssueData,
  GitHubAPIError,
  getLiveWorkers,
  getPrDraftStatusBatch,
  parseLinearIssues,
} from "../fetch";
import type { LinearIssueRaw } from "../types";

// =============================================================================
// TestParseLinearIssues
// =============================================================================

describe("parseLinearIssues", () => {
  it("parses basic issue", () => {
    const issues: LinearIssueRaw[] = [
      {
        identifier: "ENG-21",
        state: { name: "In Progress" },
        labels: { nodes: [{ name: "worker-done" }] },
      },
    ];
    const result = parseLinearIssues(issues);
    expect(result).toHaveLength(1);
    expect(result[0].issueId).toBe("ENG-21");
    expect(result[0].status).toBe("In Progress");
    expect(result[0].hasWorkerDone).toBe(true);
  });

  it("normalizes status (In Review → Needs Review)", () => {
    const issues: LinearIssueRaw[] = [
      {
        identifier: "ENG-21",
        state: { name: "In Review" },
        labels: { nodes: [] },
      },
    ];
    const result = parseLinearIssues(issues);
    expect(result[0].status).toBe("Needs Review");
  });

  it("skips issues without identifier", () => {
    const issues: LinearIssueRaw[] = [
      { state: { name: "Todo" }, labels: { nodes: [] } },
      {
        identifier: "ENG-21",
        state: { name: "Todo" },
        labels: { nodes: [] },
      },
    ];
    const result = parseLinearIssues(issues);
    expect(result).toHaveLength(1);
  });

  it("handles null state", () => {
    const issues: LinearIssueRaw[] = [
      {
        identifier: "ENG-21",
        state: null as unknown as undefined,
        labels: { nodes: [] },
      },
    ];
    const result = parseLinearIssues(issues);
    expect(result[0].status).toBe("");
  });

  it("handles null labels", () => {
    const issues: LinearIssueRaw[] = [
      {
        identifier: "ENG-21",
        state: { name: "Todo" },
        labels: null as unknown as undefined,
      },
    ];
    const result = parseLinearIssues(issues);
    expect(result[0].labels).toEqual([]);
  });
});

// =============================================================================
// TestParseLinearIssuesEdgeCases
// =============================================================================

describe("parseLinearIssues edge cases", () => {
  it("handles deeply nested nulls", () => {
    const issues: LinearIssueRaw[] = [
      {
        identifier: "ENG-21",
        state: { name: null as unknown as string },
        labels: { nodes: null as unknown as [] },
      },
    ];
    const result = parseLinearIssues(issues);
    expect(result).toHaveLength(1);
    expect(result[0].status).toBe("");
    expect(result[0].labels).toEqual([]);
  });

  it("handles labels with missing name", () => {
    const issues: LinearIssueRaw[] = [
      {
        identifier: "ENG-21",
        state: { name: "Todo" },
        labels: {
          nodes: [{ name: "worker-done" }, {} as { name: string }, { name: "urgent" }],
        },
      },
    ];
    const result = parseLinearIssues(issues);
    expect(result).toHaveLength(1);
    expect(result[0].labels).toEqual(["worker-done", "urgent"]);
  });

  it("handles attachments with invalid URLs", () => {
    const issues: LinearIssueRaw[] = [
      {
        identifier: "ENG-21",
        state: { name: "Needs Review" },
        labels: { nodes: [] },
        attachments: [
          { url: "https://example.com/not-a-pr" },
          { url: "https://github.com/owner/repo/issues/123" },
          { url: "not-even-a-url" },
        ],
      },
    ];
    const result = parseLinearIssues(issues);
    expect(result).toHaveLength(1);
    expect(result[0].prRef).toBeNull();
  });
});

// =============================================================================
// TestGetLiveWorkers (HTTP-based)
// =============================================================================

describe("getLiveWorkers", () => {
  it("parses worker response from daemon HTTP API", async () => {
    const originalFetch = globalThis.fetch;
    globalThis.fetch = mock(
      async () =>
        new Response(JSON.stringify([{ id: "ENG-21-implement" }, { id: "ENG-22-plan" }]), {
          status: 200,
        })
    ) as unknown as typeof fetch;

    try {
      const result = await getLiveWorkers("http://localhost:3000");
      expect(result).toEqual({ "ENG-21": "implement", "ENG-22": "plan" });
    } finally {
      globalThis.fetch = originalFetch;
    }
  });

  it("normalizes issue IDs to uppercase", async () => {
    const originalFetch = globalThis.fetch;
    globalThis.fetch = mock(
      async () =>
        new Response(JSON.stringify([{ id: "eng-21-implement" }, { id: "Eng-22-plan" }]), {
          status: 200,
        })
    ) as unknown as typeof fetch;

    try {
      const result = await getLiveWorkers("http://localhost:3000");
      expect(result).toEqual({ "ENG-21": "implement", "ENG-22": "plan" });
    } finally {
      globalThis.fetch = originalFetch;
    }
  });

  it("filters out dead workers", async () => {
    const originalFetch = globalThis.fetch;
    globalThis.fetch = mock(
      async () =>
        new Response(
          JSON.stringify([
            { id: "eng-21-implement", status: "running" },
            { id: "eng-22-plan", status: "dead" },
            { id: "eng-23-review", status: "stopped" },
          ]),
          { status: 200 }
        )
    ) as unknown as typeof fetch;

    try {
      const result = await getLiveWorkers("http://localhost:3000");
      expect(result).toEqual({ "ENG-21": "implement" });
    } finally {
      globalThis.fetch = originalFetch;
    }
  });

  it("includes starting workers", async () => {
    const originalFetch = globalThis.fetch;
    globalThis.fetch = mock(
      async () =>
        new Response(
          JSON.stringify([
            { id: "eng-21-implement", status: "starting" },
            { id: "eng-22-plan", status: "running" },
          ]),
          { status: 200 }
        )
    ) as unknown as typeof fetch;

    try {
      const result = await getLiveWorkers("http://localhost:3000");
      expect(result).toEqual({ "ENG-21": "implement", "ENG-22": "plan" });
    } finally {
      globalThis.fetch = originalFetch;
    }
  });

  it("handles workers without status field (backwards compat)", async () => {
    const originalFetch = globalThis.fetch;
    globalThis.fetch = mock(
      async () => new Response(JSON.stringify([{ id: "eng-21-implement" }]), { status: 200 })
    ) as unknown as typeof fetch;

    try {
      const result = await getLiveWorkers("http://localhost:3000");
      expect(result).toEqual({ "ENG-21": "implement" });
    } finally {
      globalThis.fetch = originalFetch;
    }
  });

  it("returns empty dict on HTTP error", async () => {
    const originalFetch = globalThis.fetch;
    globalThis.fetch = mock(
      async () => new Response("Internal Server Error", { status: 500 })
    ) as unknown as typeof fetch;

    try {
      const result = await getLiveWorkers("http://localhost:3000");
      expect(result).toEqual({});
    } finally {
      globalThis.fetch = originalFetch;
    }
  });

  it("returns empty dict on network failure", async () => {
    const originalFetch = globalThis.fetch;
    globalThis.fetch = mock(async () => {
      throw new Error("Connection refused");
    }) as unknown as typeof fetch;

    try {
      const result = await getLiveWorkers("http://localhost:3000");
      expect(result).toEqual({});
    } finally {
      globalThis.fetch = originalFetch;
    }
  });

  it("returns empty dict on empty worker list", async () => {
    const originalFetch = globalThis.fetch;
    globalThis.fetch = mock(
      async () => new Response(JSON.stringify([]), { status: 200 })
    ) as unknown as typeof fetch;

    try {
      const result = await getLiveWorkers("http://localhost:3000");
      expect(result).toEqual({});
    } finally {
      globalThis.fetch = originalFetch;
    }
  });

  it("handles worker IDs with multiple hyphens", async () => {
    const originalFetch = globalThis.fetch;
    globalThis.fetch = mock(
      async () =>
        new Response(
          JSON.stringify([
            { id: "TEAM-PROJECT-123-implement" },
            { id: "MY-COMPLEX-ISSUE-456-plan" },
          ]),
          { status: 200 }
        )
    ) as unknown as typeof fetch;

    try {
      const result = await getLiveWorkers("http://localhost:3000");
      expect(result).toEqual({
        "TEAM-PROJECT-123": "implement",
        "MY-COMPLEX-ISSUE-456": "plan",
      });
    } finally {
      globalThis.fetch = originalFetch;
    }
  });
});

// =============================================================================
// TestGetPrDraftStatusBatch
// =============================================================================

describe("getPrDraftStatusBatch", () => {
  it("returns draft status for multiple issues", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = {
        data: {
          repo0: {
            pr0: { isDraft: true },
            pr1: { isDraft: false },
          },
        },
      };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    const result = await getPrDraftStatusBatch(
      {
        "ENG-21": { owner: "owner", repo: "repo", number: 1 },
        "ENG-22": { owner: "owner", repo: "repo", number: 2 },
      },
      runner
    );
    expect(result).toEqual({ "ENG-21": true, "ENG-22": false });
  });

  it("returns null for missing PR", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = {
        data: {
          repo0: {
            pr0: null,
          },
        },
      };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    const result = await getPrDraftStatusBatch(
      {
        "ENG-21": { owner: "owner", repo: "repo", number: 999 },
      },
      runner
    );
    expect(result).toEqual({ "ENG-21": null });
  });

  it("handles null isDraft value", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = {
        data: {
          repo0: {
            pr0: { isDraft: null },
          },
        },
      };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    const result = await getPrDraftStatusBatch(
      {
        "ENG-21": { owner: "owner", repo: "repo", number: 1 },
      },
      runner
    );
    expect(result).toEqual({ "ENG-21": false });
  });

  it("throws GitHubAPIError on command failure after retries", async () => {
    let callCount = 0;
    const runner: CommandRunner = async (_cmd: string[]) => {
      callCount++;
      return { stdout: "", stderr: "rate limited", exitCode: 1 };
    };

    await Promise.resolve(
      expect(
        getPrDraftStatusBatch({ "ENG-21": { owner: "owner", repo: "repo", number: 1 } }, runner)
      ).rejects.toThrow(GitHubAPIError)
    );

    expect(callCount).toBe(3);
  });

  it("throws GitHubAPIError on malformed JSON after retries", async () => {
    let callCount = 0;
    const runner: CommandRunner = async (_cmd: string[]) => {
      callCount++;
      return { stdout: "not valid json {[", stderr: "", exitCode: 0 };
    };

    await Promise.resolve(
      expect(
        getPrDraftStatusBatch({ "ENG-21": { owner: "owner", repo: "repo", number: 1 } }, runner)
      ).rejects.toThrow(GitHubAPIError)
    );

    expect(callCount).toBe(3);
  });

  it("succeeds after transient failures", async () => {
    let callCount = 0;
    const runner: CommandRunner = async (_cmd: string[]) => {
      callCount++;
      if (callCount < 3) {
        return { stdout: "", stderr: "temporary network error", exitCode: 1 };
      }
      const response = { data: { repo0: { pr0: { isDraft: false } } } };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    const result = await getPrDraftStatusBatch(
      { "ENG-21": { owner: "owner", repo: "repo", number: 1 } },
      runner
    );

    expect(result).toEqual({ "ENG-21": false });
    expect(callCount).toBe(3);
  });

  it("handles null data in response", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = { data: null, errors: [{ message: "Not found" }] };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    const result = await getPrDraftStatusBatch(
      { "ENG-21": { owner: "owner", repo: "repo", number: 1 } },
      runner
    );
    expect(result).toEqual({ "ENG-21": null });
  });

  it("handles non-dict data in response", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = { data: "unexpected string" };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    const result = await getPrDraftStatusBatch(
      { "ENG-21": { owner: "owner", repo: "repo", number: 1 } },
      runner
    );
    expect(result).toEqual({ "ENG-21": null });
  });

  it("handles non-dict repo in response", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = { data: { repo0: "unexpected string" } };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    const result = await getPrDraftStatusBatch(
      { "ENG-21": { owner: "owner", repo: "repo", number: 1 } },
      runner
    );
    expect(result).toEqual({ "ENG-21": null });
  });

  it("handles non-dict PR in response", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = { data: { repo0: { pr0: "unexpected string" } } };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    const result = await getPrDraftStatusBatch(
      { "ENG-21": { owner: "owner", repo: "repo", number: 1 } },
      runner
    );
    expect(result).toEqual({ "ENG-21": null });
  });

  it("batches multiple repos in single query", async () => {
    const queriesReceived: string[] = [];
    const runner: CommandRunner = async (cmd: string[]) => {
      const query = cmd[cmd.length - 1]; // "query=..."
      queriesReceived.push(query);

      const response = {
        data: {
          repo0: { pr0: { isDraft: true } },
          repo1: { pr0: { isDraft: false } },
        },
      };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    const result = await getPrDraftStatusBatch(
      {
        "ENG-21": { owner: "org", repo: "repo1", number: 1 },
        "ENG-22": { owner: "org", repo: "repo2", number: 2 },
      },
      runner
    );

    expect(queriesReceived).toHaveLength(1);
    expect(queriesReceived[0]).toContain("repo1");
    expect(queriesReceived[0]).toContain("repo2");
    expect(result).toEqual({ "ENG-21": true, "ENG-22": false });
  });

  it("returns empty result for empty pr_refs", async () => {
    let callCount = 0;
    const runner: CommandRunner = async (_cmd: string[]) => {
      callCount++;
      return { stdout: "{}", stderr: "", exitCode: 0 };
    };

    const result = await getPrDraftStatusBatch({}, runner);
    expect(result).toEqual({});
    expect(callCount).toBe(0);
  });
});

// =============================================================================
// TestFetchAllIssueData
// =============================================================================

describe("fetchAllIssueData", () => {
  it("fetches data for issues", async () => {
    const originalFetch = globalThis.fetch;
    globalThis.fetch = mock(
      async () => new Response(JSON.stringify([{ id: "ENG-21-implement" }]), { status: 200 })
    ) as unknown as typeof fetch;

    try {
      const runner: CommandRunner = async (_cmd: string[]) => {
        return { stdout: JSON.stringify({ data: {} }), stderr: "", exitCode: 0 };
      };

      const linearIssues: LinearIssueRaw[] = [
        {
          identifier: "ENG-21",
          state: { name: "In Progress" },
          labels: { nodes: [] },
        },
      ];

      const result = await fetchAllIssueData(linearIssues, "http://localhost:3000", runner);

      expect(result).toHaveLength(1);
      expect(result[0].issueId).toBe("ENG-21");
      expect(result[0].hasLiveWorker).toBe(true);
    } finally {
      globalThis.fetch = originalFetch;
    }
  });

  it("GitHub API failure sets prIsDraft to null", async () => {
    const originalFetch = globalThis.fetch;
    globalThis.fetch = mock(
      async () => new Response(JSON.stringify([]), { status: 200 })
    ) as unknown as typeof fetch;

    try {
      const runner: CommandRunner = async (_cmd: string[]) => {
        return { stdout: "", stderr: "rate limited", exitCode: 1 };
      };

      const linearIssues: LinearIssueRaw[] = [
        {
          identifier: "ENG-21",
          state: { name: "Needs Review" },
          labels: { nodes: [{ name: "worker-done" }] },
          attachments: [{ url: "https://github.com/owner/repo/pull/1" }],
        },
      ];

      const result = await fetchAllIssueData(linearIssues, "http://localhost:3000", runner);

      expect(result).toHaveLength(1);
      expect(result[0].hasPr).toBe(true);
      expect(result[0].prIsDraft).toBeNull();
    } finally {
      globalThis.fetch = originalFetch;
    }
  });

  it("handles malformed Linear issue data", async () => {
    const originalFetch = globalThis.fetch;
    globalThis.fetch = mock(
      async () => new Response(JSON.stringify([]), { status: 200 })
    ) as unknown as typeof fetch;

    try {
      const linearIssues: LinearIssueRaw[] = [
        {} as LinearIssueRaw,
        { identifier: "ENG-21" } as LinearIssueRaw,
        { state: { name: "Todo" } } as LinearIssueRaw,
      ];

      const result = await fetchAllIssueData(linearIssues, "http://localhost:3000");

      expect(result).toHaveLength(1);
      expect(result[0].issueId).toBe("ENG-21");
    } finally {
      globalThis.fetch = originalFetch;
    }
  });
});
