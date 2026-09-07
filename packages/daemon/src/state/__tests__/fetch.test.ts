/**
 * Tests for GitHub status fetch helpers.
 */

import { describe, expect, it } from "bun:test";
import {
  type CommandRunner,
  GitHubAPIError,
  getCiStatusBatch,
  getPrReviewStateBatch,
  mapMergeableState,
} from "../fetch";

// =============================================================================
// TestGetPrReviewStateBatch
// =============================================================================

describe("getPrReviewStateBatch", () => {
  it("returns review state for multiple issues", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = {
        data: {
          repo0: {
            pr0: { latestReviews: { nodes: [{ state: "CHANGES_REQUESTED" }] } },
            pr1: { latestReviews: { nodes: [{ state: "APPROVED" }] } },
          },
        },
      };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    const result = await getPrReviewStateBatch(
      {
        "ENG-21": { owner: "owner", repo: "repo", number: 1 },
        "ENG-22": { owner: "owner", repo: "repo", number: 2 },
      },
      runner
    );
    expect(result).toEqual({
      "ENG-21": "changes_requested",
      "ENG-22": "approved",
    });
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

    const result = await getPrReviewStateBatch(
      {
        "ENG-21": { owner: "owner", repo: "repo", number: 999 },
      },
      runner
    );
    expect(result).toEqual({ "ENG-21": null });
  });

  it("returns null for PR with no reviews", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = {
        data: {
          repo0: {
            pr0: { latestReviews: { nodes: [] } },
          },
        },
      };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    const result = await getPrReviewStateBatch(
      {
        "ENG-21": { owner: "owner", repo: "repo", number: 1 },
      },
      runner
    );
    expect(result).toEqual({ "ENG-21": null });
  });

  it("throws GitHubAPIError on command failure after retries", async () => {
    let callCount = 0;
    const runner: CommandRunner = async (_cmd: string[]) => {
      callCount++;
      return { stdout: "", stderr: "rate limited", exitCode: 1 };
    };

    await Promise.resolve(
      expect(
        getPrReviewStateBatch({ "ENG-21": { owner: "owner", repo: "repo", number: 1 } }, runner)
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
        getPrReviewStateBatch({ "ENG-21": { owner: "owner", repo: "repo", number: 1 } }, runner)
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
      const response = {
        data: {
          repo0: { pr0: { latestReviews: { nodes: [{ state: "APPROVED" }] } } },
        },
      };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    const result = await getPrReviewStateBatch(
      { "ENG-21": { owner: "owner", repo: "repo", number: 1 } },
      runner
    );

    expect(result).toEqual({ "ENG-21": "approved" });
    expect(callCount).toBe(3);
  });

  it("handles null data in response", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = { data: null, errors: [{ message: "Not found" }] };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    const result = await getPrReviewStateBatch(
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

    const result = await getPrReviewStateBatch(
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
          repo0: {
            pr0: { latestReviews: { nodes: [{ state: "CHANGES_REQUESTED" }] } },
          },
          repo1: { pr0: { latestReviews: { nodes: [{ state: "APPROVED" }] } } },
        },
      };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    const result = await getPrReviewStateBatch(
      {
        "ENG-21": { owner: "org", repo: "repo1", number: 1 },
        "ENG-22": { owner: "org", repo: "repo2", number: 2 },
      },
      runner
    );

    expect(queriesReceived).toHaveLength(1);
    expect(queriesReceived[0]).toContain("repo1");
    expect(queriesReceived[0]).toContain("repo2");
    expect(result).toEqual({
      "ENG-21": "changes_requested",
      "ENG-22": "approved",
    });
  });

  it("returns empty result for empty pr_refs", async () => {
    let callCount = 0;
    const runner: CommandRunner = async (_cmd: string[]) => {
      callCount++;
      return { stdout: "{}", stderr: "", exitCode: 0 };
    };

    const result = await getPrReviewStateBatch({}, runner);
    expect(result).toEqual({});
    expect(callCount).toBe(0);
  });
});

describe("mapMergeableState", () => {
  it("maps MERGEABLE to mergeable", () => {
    expect(mapMergeableState("MERGEABLE")).toBe("mergeable");
  });

  it("maps CONFLICTING to conflicting", () => {
    expect(mapMergeableState("CONFLICTING")).toBe("conflicting");
  });

  it("maps UNKNOWN to unknown", () => {
    expect(mapMergeableState("UNKNOWN")).toBe("unknown");
  });

  it("maps null to null", () => {
    expect(mapMergeableState(null)).toBeNull();
  });

  it("maps undefined to null", () => {
    expect(mapMergeableState(undefined)).toBeNull();
  });

  it("maps unrecognized value to null", () => {
    expect(mapMergeableState("INVALID")).toBeNull();
  });
});

// =============================================================================
// TestGetCiStatusBatch
// =============================================================================

describe("getCiStatusBatch", () => {
  it("returns passing for SUCCESS status", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = {
        data: {
          repo0: {
            pr0: {
              mergeable: "MERGEABLE",
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
    expect(result).toEqual({
      "ENG-21": {
        ciStatus: "passing",
        mergeableStatus: "mergeable",
        headSha: null,
        updatedAt: null,
        checkRuns: {},
        isOpen: false,
      },
    });
  });

  it("returns failing for FAILURE status", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = {
        data: {
          repo0: {
            pr0: {
              mergeable: "MERGEABLE",
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
    expect(result).toEqual({
      "ENG-21": {
        ciStatus: "failing",
        mergeableStatus: "mergeable",
        failingChecks: [],

        cancelledCount: 0,
        headSha: null,
        updatedAt: null,
        checkRuns: {},
        isOpen: false,
      },
    });
  });

  it("returns failed check names from capped rollup contexts", async () => {
    let query = "";
    const runner: CommandRunner = async (cmd: string[]) => {
      query = cmd.find((argument) => argument.startsWith("query=")) ?? "";
      const response = {
        data: {
          repo0: {
            pr0: {
              mergeable: "MERGEABLE",
              commits: {
                nodes: [
                  {
                    commit: {
                      statusCheckRollup: {
                        state: "FAILURE",
                        contexts: {
                          nodes: [
                            {
                              name: "lint",
                              conclusion: "FAILURE",
                              detailsUrl: "https://example.test/checks/lint",
                            },
                            {
                              name: "unit",
                              conclusion: "SUCCESS",
                              detailsUrl: "https://example.test/checks/unit",
                            },
                            {
                              name: "legacy",
                              conclusion: "ERROR",
                              detailsUrl: "https://example.test/checks/legacy",
                            },
                          ],
                        },
                      },
                    },
                  },
                ],
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

    expect(query).toContain("contexts(first: 100)");
    expect(query).toContain("name conclusion");
    expect(query).not.toContain("detailsUrl");
    expect(query).not.toContain("targetUrl");
    expect(result).toEqual({
      "ENG-21": {
        ciStatus: "failing",
        mergeableStatus: "mergeable",
        failingChecks: ["lint", "legacy"],

        cancelledCount: 0,
        headSha: null,
        updatedAt: null,
        checkRuns: {},
        isOpen: false,
      },
    });
  });

  it("collects every failing context page from the originally fetched head", async () => {
    const queries: string[][] = [];
    let calls = 0;
    const runner: CommandRunner = async (cmd: string[]) => {
      queries.push(cmd);
      calls += 1;
      const response =
        calls === 1
          ? {
              data: {
                repo0: {
                  pr0: {
                    state: "OPEN",
                    updatedAt: "2026-08-24T00:00:00.000Z",
                    mergeable: "MERGEABLE",
                    commits: {
                      nodes: [
                        {
                          commit: {
                            oid: "head-1",
                            statusCheckRollup: {
                              state: "FAILURE",
                              contexts: {
                                pageInfo: { hasNextPage: true, endCursor: "cursor-1" },
                                nodes: [
                                  {
                                    name: "lint",
                                    conclusion: "FAILURE",
                                    databaseId: 800,
                                    completedAt: "2026-08-24T00:00:01.000Z",
                                  },
                                ],
                              },
                            },
                          },
                        },
                      ],
                    },
                  },
                },
              },
            }
          : {
              data: {
                repository: {
                  object: {
                    statusCheckRollup: {
                      contexts: {
                        pageInfo: { hasNextPage: false, endCursor: null },
                        nodes: [
                          {
                            name: "unit",
                            conclusion: "ERROR",
                            databaseId: 900,
                            completedAt: "2026-08-24T00:00:02.000Z",
                          },
                        ],
                      },
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

    expect(queries).toHaveLength(2);
    expect(queries[0]?.find((argument) => argument.startsWith("query="))).toContain(
      "pageInfo { hasNextPage endCursor }"
    );
    expect(queries[0]?.find((argument) => argument.startsWith("query="))).toContain("databaseId");
    expect(queries[1]?.find((argument) => argument.startsWith("query="))).toContain(
      'object(oid: "head-1")'
    );
    expect(queries[1]?.find((argument) => argument.startsWith("query="))).toContain("databaseId");
    expect(queries[1]).toContain("after=cursor-1");
    expect(result).toEqual({
      "ENG-21": {
        ciStatus: "failing",
        mergeableStatus: "mergeable",
        failingChecks: ["lint", "unit"],

        cancelledCount: 0,
        headSha: "head-1",
        updatedAt: "2026-08-24T00:00:00.000Z",
        checkRuns: { lint: 800, unit: 900 },
        isOpen: true,
      },
    });
  });

  it("pages a passing rollup for the fence but returns no failing names", async () => {
    const queries: string[][] = [];
    let calls = 0;
    const runner: CommandRunner = async (cmd: string[]) => {
      queries.push(cmd);
      calls += 1;
      const response =
        calls === 1
          ? {
              data: {
                repo0: {
                  pr0: {
                    state: "OPEN",
                    updatedAt: "2026-08-24T00:00:00.000Z",
                    mergeable: "MERGEABLE",
                    commits: {
                      nodes: [
                        {
                          commit: {
                            oid: "head-1",
                            statusCheckRollup: {
                              state: "SUCCESS",
                              contexts: {
                                pageInfo: { hasNextPage: true, endCursor: "cursor-1" },
                                nodes: [{ name: "unit", conclusion: "SUCCESS", databaseId: 900 }],
                              },
                            },
                          },
                        },
                      ],
                    },
                  },
                },
              },
            }
          : {
              data: {
                repository: {
                  object: {
                    statusCheckRollup: {
                      contexts: {
                        pageInfo: { hasNextPage: false, endCursor: null },
                        nodes: [{ name: "e2e", conclusion: "SUCCESS", databaseId: 950 }],
                      },
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

    // The second page is fetched from the head commit for the fence; the
    // highest id lives there.
    expect(queries).toHaveLength(2);
    expect(queries[1]?.join(" ")).toContain('object(oid: "head-1")');
    expect(result).toEqual({
      "ENG-21": {
        ciStatus: "passing",
        mergeableStatus: "mergeable",
        headSha: "head-1",
        updatedAt: "2026-08-24T00:00:00.000Z",
        checkRuns: { e2e: 950, unit: 900 },
        isOpen: true,
      },
    });
  });

  it("returns failing for ERROR status", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = {
        data: {
          repo0: {
            pr0: {
              mergeable: "MERGEABLE",
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
    expect(result).toEqual({
      "ENG-21": {
        ciStatus: "failing",
        mergeableStatus: "mergeable",
        failingChecks: [],

        cancelledCount: 0,
        headSha: null,
        updatedAt: null,
        checkRuns: {},
        isOpen: false,
      },
    });
  });

  it("returns pending for PENDING status", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = {
        data: {
          repo0: {
            pr0: {
              mergeable: "MERGEABLE",
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
    expect(result).toEqual({
      "ENG-21": {
        ciStatus: "pending",
        mergeableStatus: "mergeable",
        headSha: null,
        updatedAt: null,
        checkRuns: {},
        isOpen: false,
      },
    });
  });

  it("returns pending for EXPECTED status", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = {
        data: {
          repo0: {
            pr0: {
              mergeable: "MERGEABLE",
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
    expect(result).toEqual({
      "ENG-21": {
        ciStatus: "pending",
        mergeableStatus: "mergeable",
        headSha: null,
        updatedAt: null,
        checkRuns: {},
        isOpen: false,
      },
    });
  });

  it("returns null when statusCheckRollup is null (no checks configured)", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = {
        data: {
          repo0: {
            pr0: {
              mergeable: "MERGEABLE",
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
    expect(result).toEqual({
      "ENG-21": {
        ciStatus: null,
        mergeableStatus: "mergeable",
        headSha: null,
        updatedAt: null,
        checkRuns: {},
        isOpen: false,
      },
    });
  });

  it("returns null when commits nodes is empty", async () => {
    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = {
        data: {
          repo0: {
            pr0: {
              mergeable: "MERGEABLE",
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
    expect(result).toEqual({
      "ENG-21": {
        ciStatus: null,
        mergeableStatus: null,
        headSha: null,
        updatedAt: null,
        checkRuns: {},
        isOpen: false,
      },
    });
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
    expect(result).toEqual({
      "ENG-21": {
        ciStatus: null,
        mergeableStatus: null,
        headSha: null,
        updatedAt: null,
        checkRuns: {},
        isOpen: false,
      },
    });
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
              mergeable: "MERGEABLE",
              commits: {
                nodes: [{ commit: { statusCheckRollup: { state: "SUCCESS" } } }],
              },
            },
          },
          repo1: {
            pr0: {
              mergeable: "CONFLICTING",
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
    expect(result).toEqual({
      "ENG-21": {
        ciStatus: "passing",
        mergeableStatus: "mergeable",
        headSha: null,
        updatedAt: null,
        checkRuns: {},
        isOpen: false,
      },
      "ENG-22": {
        ciStatus: "failing",
        mergeableStatus: "conflicting",
        failingChecks: [],

        cancelledCount: 0,
        headSha: null,
        updatedAt: null,
        checkRuns: {},
        isOpen: false,
      },
    });
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
              mergeable: "MERGEABLE",
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
    expect(result).toEqual({
      "ENG-21": {
        ciStatus: "passing",
        mergeableStatus: "mergeable",
        headSha: null,
        updatedAt: null,
        checkRuns: {},
        isOpen: false,
      },
    });
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

  it("returns a typed owner failure after retrying its owner-scoped batch", async () => {
    let callCount = 0;
    const runner: CommandRunner = async () => {
      callCount += 1;
      return { stdout: "", stderr: "owner request failed", exitCode: 1 };
    };

    const result = await getCiStatusBatch(
      { "ENG-21": { owner: "acme", repo: "repo", number: 1 } },
      runner,
      async () => ({ env: { GH_TOKEN: "owner-token" } })
    );

    expect(callCount).toBe(3);
    expect(result).toEqual({
      "ENG-21": {
        owner: "acme",
        error: "GraphQL query failed: owner request failed",
      },
    });
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
              mergeable: "UNKNOWN",
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
    expect(result).toEqual({
      "ENG-21": {
        ciStatus: null,
        mergeableStatus: "unknown",
        headSha: null,
        updatedAt: null,
        checkRuns: {},
        isOpen: false,
      },
    });
  });
});
