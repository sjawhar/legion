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
  enrichParsedIssues,
  fetchAllIssueData,
  GitHubAPIError,
  getLiveWorkers,
  getPrDraftStatusBatch,
} from "../fetch";
import type { LinearIssueRaw } from "../types";
import { createParsedIssue } from "../types";

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
      expect(result).toEqual({
        "ENG-21": { mode: "implement", status: "running" },
        "ENG-22": { mode: "plan", status: "running" },
      });
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
      expect(result).toEqual({
        "ENG-21": { mode: "implement", status: "running" },
        "ENG-22": { mode: "plan", status: "running" },
      });
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
      expect(result).toEqual({ "ENG-21": { mode: "implement", status: "running" } });
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
      expect(result).toEqual({
        "ENG-21": { mode: "implement", status: "starting" },
        "ENG-22": { mode: "plan", status: "running" },
      });
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
      expect(result).toEqual({ "ENG-21": { mode: "implement", status: "running" } });
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
        "TEAM-PROJECT-123": { mode: "implement", status: "running" },
        "MY-COMPLEX-ISSUE-456": { mode: "plan", status: "running" },
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

describe("enrichParsedIssues", () => {
  it("enriches issues with worker status from daemon", async () => {
    const originalFetch = globalThis.fetch;
    globalThis.fetch = mock(async () => {
      throw new Error("Connection refused");
    }) as unknown as typeof fetch;

    try {
      const mockRunner: CommandRunner = async () => ({ stdout: "", stderr: "", exitCode: 1 });
      const issues = [createParsedIssue("ENG-21", "In Progress", ["worker-active"], null)];
      const result = await enrichParsedIssues(issues, "http://127.0.0.1:99999", mockRunner);
      expect(result).toHaveLength(1);
      expect(result[0].issueId).toBe("ENG-21");
      expect(result[0].hasLiveWorker).toBe(false);
      expect(result[0].prIsDraft).toBeNull();
    } finally {
      globalThis.fetch = originalFetch;
    }
  });

  it("handles GitHub-style issues with null prRef", async () => {
    const originalFetch = globalThis.fetch;
    globalThis.fetch = mock(async () => {
      throw new Error("Connection refused");
    }) as unknown as typeof fetch;

    try {
      const mockRunner: CommandRunner = async () => ({ stdout: "", stderr: "", exitCode: 1 });
      const issues = [createParsedIssue("GH-42", "Todo", [], null)];
      const result = await enrichParsedIssues(issues, "http://127.0.0.1:99999", mockRunner);
      expect(result).toHaveLength(1);
      expect(result[0].prIsDraft).toBeNull();
      expect(result[0].hasPr).toBe(false);
    } finally {
      globalThis.fetch = originalFetch;
    }
  });
});
