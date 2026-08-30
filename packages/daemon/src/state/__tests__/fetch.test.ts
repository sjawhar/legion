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
      "ENG-21": { ciStatus: "passing", mergeableStatus: "mergeable" },
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
      "ENG-21": { ciStatus: "failing", mergeableStatus: "mergeable" },
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
      "ENG-21": { ciStatus: "failing", mergeableStatus: "mergeable" },
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
      "ENG-21": { ciStatus: "pending", mergeableStatus: "mergeable" },
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
      "ENG-21": { ciStatus: "pending", mergeableStatus: "mergeable" },
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
      "ENG-21": { ciStatus: null, mergeableStatus: "mergeable" },
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
      "ENG-21": { ciStatus: null, mergeableStatus: null },
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
      "ENG-21": { ciStatus: null, mergeableStatus: null },
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
      "ENG-21": { ciStatus: "passing", mergeableStatus: "mergeable" },
      "ENG-22": { ciStatus: "failing", mergeableStatus: "conflicting" },
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
      "ENG-21": { ciStatus: "passing", mergeableStatus: "mergeable" },
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
      "ENG-21": { ciStatus: null, mergeableStatus: "unknown" },
    });
  });
});
