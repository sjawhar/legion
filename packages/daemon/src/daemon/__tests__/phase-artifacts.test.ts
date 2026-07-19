import { describe, expect, it } from "bun:test";

// allow: SIZE_OK — independently executable cases cover one GraphQL artifact boundary.
import type { CommandRunner } from "../../state/fetch";
import {
  fetchPhaseArtifacts,
  fetchPhaseArtifactsBatch,
  type IssueRef,
  type PhaseArtifacts,
} from "../phase-artifacts";

const issueRef: IssueRef = {
  issueId: "acme-api-42",
  status: "Needs Review",
  source: { owner: "acme", repo: "api", number: 42 },
  prRef: { owner: "acme", repo: "api", number: 101 },
};

function runnerFor(pullRequest: unknown): CommandRunner {
  return async () => ({
    stdout: JSON.stringify({ data: { repo0: { pr0: pullRequest } } }),
    stderr: "",
    exitCode: 0,
  });
}

function pullRequestWithHead(headSha: string) {
  return {
    isDraft: false,
    headRefOid: headSha,
    merged: false,
    autoMergeRequest: null,
    latestReviews: { nodes: [] },
    commits: { nodes: [] },
  };
}

describe("fetchPhaseArtifacts", () => {
  it("batches PR aliases into one GraphQL request", async () => {
    const secondRef: IssueRef = {
      ...issueRef,
      issueId: "acme-api-43",
      prRef: { owner: "acme", repo: "api", number: 102 },
    };
    const commands: string[][] = [];
    const runner: CommandRunner = async (command) => {
      commands.push(command);
      return {
        stdout: JSON.stringify({
          data: {
            repo0: {
              pr0: {
                isDraft: false,
                headRefOid: "first-head",
                merged: false,
                autoMergeRequest: null,
                latestReviews: { nodes: [] },
                commits: { nodes: [] },
              },
              pr1: {
                isDraft: false,
                headRefOid: "second-head",
                merged: false,
                autoMergeRequest: null,
                latestReviews: { nodes: [] },
                commits: { nodes: [] },
              },
            },
          },
        }),
        stderr: "",
        exitCode: 0,
      };
    };

    const result = await fetchPhaseArtifactsBatch([issueRef, secondRef], {
      reviewerAppId: 42,
      reviewerAppLogin: "legion-reviewer[bot]",
      runner,
    });
    const queryArgument = commands[0]?.find((argument) => argument.startsWith("query="));

    expect(commands).toHaveLength(1);
    expect(queryArgument).toContain("repo0: repository");
    expect(queryArgument).toContain("pr0: pullRequest(number: 101)");
    expect(queryArgument).toContain("pr1: pullRequest(number: 102)");
    expect(queryArgument).toContain("author { login }");
    expect(result.artifacts[issueRef.issueId]?.headSha).toBe("first-head");
    expect(result.artifacts[secondRef.issueId]?.headSha).toBe("second-head");
    expect(result.errors).toEqual([]);
  });

  it("preserves resolved PR aliases when a nonzero GraphQL response contains partial data", async () => {
    const secondRef: IssueRef = {
      ...issueRef,
      issueId: "acme-api-43",
      prRef: { owner: "acme", repo: "api", number: 102 },
    };
    const failedRef: IssueRef = {
      ...issueRef,
      issueId: "acme-api-44",
      prRef: { owner: "acme", repo: "api", number: 103 },
    };

    const result = await fetchPhaseArtifactsBatch([issueRef, secondRef, failedRef], {
      reviewerAppId: 42,
      reviewerAppLogin: "legion-reviewer[bot]",
      runner: async () => ({
        stdout: JSON.stringify({
          data: {
            repo0: {
              pr0: pullRequestWithHead("first-head"),
              pr1: pullRequestWithHead("second-head"),
              pr2: null,
            },
          },
          errors: [{ message: "Pull request 103 was not found", path: ["repo0", "pr2"] }],
        }),
        stderr: "GraphQL: Pull request 103 was not found",
        exitCode: 1,
      }),
    });

    expect(result.artifacts[issueRef.issueId]?.headSha).toBe("first-head");
    expect(result.artifacts[secondRef.issueId]?.headSha).toBe("second-head");
    expect(result.artifacts[failedRef.issueId]?.resolved.headSha).toBe("unresolvable");
    expect(result.errors).toEqual([
      { issueId: failedRef.issueId, message: "Pull request 103 was not found" },
    ]);
  });

  it("marks the whole batch unresolvable when a nonzero GraphQL response has no parseable data", async () => {
    const secondRef: IssueRef = {
      ...issueRef,
      issueId: "acme-api-43",
      prRef: { owner: "acme", repo: "api", number: 102 },
    };

    const result = await fetchPhaseArtifactsBatch([issueRef, secondRef], {
      reviewerAppId: 42,
      reviewerAppLogin: "legion-reviewer[bot]",
      runner: async () => ({
        stdout: "not json",
        stderr: "GraphQL request failed",
        exitCode: 1,
      }),
    });

    expect(result.artifacts[issueRef.issueId]?.resolved.headSha).toBe("unresolvable");
    expect(result.artifacts[secondRef.issueId]?.resolved.headSha).toBe("unresolvable");
    expect(result.errors).toEqual([
      {
        issueId: issueRef.issueId,
        message: "GitHub phase-artifact query failed: GraphQL request failed",
      },
      {
        issueId: secondRef.issueId,
        message: "GitHub phase-artifact query failed: GraphQL request failed",
      },
    ]);
  });

  it("derives head-bound checks, review, and merge facts from a non-draft PR", async () => {
    const artifacts = await fetchPhaseArtifacts(issueRef, {
      reviewerAppId: 42,
      reviewerAppLogin: "legion-reviewer[bot]",
      runner: runnerFor({
        isDraft: false,
        headRefOid: "head-sha",
        merged: false,
        autoMergeRequest: { enabledAt: "2026-07-19T00:00:00Z" },
        latestReviews: {
          nodes: [
            {
              state: "APPROVED",
              commit: { oid: "head-sha" },
              author: { login: "legion-reviewer[bot]" },
            },
          ],
        },
        commits: {
          nodes: [
            {
              commit: {
                oid: "head-sha",
                statusCheckRollup: {
                  contexts: {
                    nodes: [
                      {
                        name: "tester",
                        conclusion: "SUCCESS",
                        checkSuite: { app: { databaseId: 42 } },
                      },
                      {
                        name: "architect",
                        conclusion: "FAILURE",
                        checkSuite: { app: { databaseId: 42 } },
                      },
                      {
                        name: "tester",
                        conclusion: "FAILURE",
                        checkSuite: { app: { databaseId: 99 } },
                      },
                    ],
                  },
                },
              },
            },
          ],
        },
      }),
    });

    expect(artifacts.hasNonDraftPr).toBe(true);
    expect(artifacts.headSha).toBe("head-sha");
    expect(artifacts.testerCheckOnHead).toBe("success");
    expect(artifacts.architectCheckOnHead).toBe("failure");
    expect(artifacts.nativeReviewOnHead).toBe("approved");
    expect(artifacts.autoMergeEnabledOrMerged).toBe(true);
    expect(artifacts.merged).toBe(false);
    expect(artifacts.planHandoff).toBe("unresolvable");
    expect(artifacts.resolved).toMatchObject({
      testerCheckOnHead: "resolved",
      architectCheckOnHead: "resolved",
      nativeReviewOnHead: "resolved",
      planHandoff: "unresolvable",
    });
  });

  it("preserves a draft PR while reporting durable fields as resolved", async () => {
    const artifacts = await fetchPhaseArtifacts(issueRef, {
      reviewerAppId: 42,
      reviewerAppLogin: "legion-reviewer[bot]",
      runner: runnerFor({
        isDraft: true,
        headRefOid: "draft-head",
        merged: false,
        autoMergeRequest: null,
        latestReviews: { nodes: [] },
        commits: {
          nodes: [
            {
              commit: {
                oid: "draft-head",
                statusCheckRollup: { contexts: { nodes: [] } },
              },
            },
          ],
        },
      }),
    });

    expect(artifacts.hasNonDraftPr).toBe(false);
    expect(artifacts.headSha).toBe("draft-head");
    expect(artifacts.testerCheckOnHead).toBeNull();
    expect(artifacts.resolved.testerCheckOnHead).toBe("resolved");
  });

  it("marks PR-dependent facts unresolvable when no PR exists", async () => {
    const artifacts = await fetchPhaseArtifacts(
      { ...issueRef, prRef: null },
      { reviewerAppId: 42, reviewerAppLogin: "legion-reviewer[bot]", runner: runnerFor(null) }
    );

    expect(artifacts.hasNonDraftPr).toBe(false);
    expect(artifacts.testerCheckOnHead).toBeNull();
    expect(artifacts.resolved.testerCheckOnHead).toBe("unresolvable");
    expect(artifacts.resolved.nativeReviewOnHead).toBe("unresolvable");
    expect(artifacts.planHandoff).toBe("unresolvable");
  });

  it("does not treat a review on an old commit as a head review", async () => {
    const artifacts = await fetchPhaseArtifacts(issueRef, {
      reviewerAppId: 42,
      reviewerAppLogin: "legion-reviewer[bot]",
      runner: runnerFor({
        isDraft: false,
        headRefOid: "current-head",
        merged: false,
        autoMergeRequest: null,
        latestReviews: {
          nodes: [
            {
              state: "APPROVED",
              commit: { oid: "old-head" },
              author: { login: "legion-reviewer[bot]" },
            },
          ],
        },
        commits: { nodes: [] },
      }),
    });

    expect(artifacts.nativeReviewOnHead).toBeNull();
    expect(artifacts.resolved.nativeReviewOnHead).toBe("unresolvable");
  });

  it("uses a reviewer-App approval after a human comment on the current head", async () => {
    const artifacts = await fetchPhaseArtifacts(issueRef, {
      reviewerAppId: 42,
      reviewerAppLogin: "legion-reviewer[bot]",
      runner: runnerFor({
        isDraft: false,
        headRefOid: "current-head",
        merged: false,
        autoMergeRequest: null,
        latestReviews: {
          nodes: [
            {
              state: "COMMENTED",
              commit: { oid: "current-head" },
              author: { login: "human-reviewer" },
            },
            {
              state: "APPROVED",
              commit: { oid: "current-head" },
              author: { login: "legion-reviewer[bot]" },
            },
          ],
        },
        commits: { nodes: [] },
      }),
    });

    expect(artifacts.nativeReviewOnHead).toBe("approved");
    expect(artifacts.resolved.nativeReviewOnHead).toBe("resolved");
  });

  it("uses a decisive reviewer-App change request after a stale review", async () => {
    const artifacts = await fetchPhaseArtifacts(issueRef, {
      reviewerAppId: 42,
      reviewerAppLogin: "legion-reviewer[bot]",
      runner: runnerFor({
        isDraft: false,
        headRefOid: "current-head",
        merged: false,
        autoMergeRequest: null,
        latestReviews: {
          nodes: [
            {
              state: "APPROVED",
              commit: { oid: "old-head" },
              author: { login: "legion-reviewer[bot]" },
            },
            {
              state: "CHANGES_REQUESTED",
              commit: { oid: "current-head" },
              author: { login: "legion-reviewer[bot]" },
            },
          ],
        },
        commits: { nodes: [] },
      }),
    });

    expect(artifacts.nativeReviewOnHead).toBe("changes_requested");
    expect(artifacts.resolved.nativeReviewOnHead).toBe("resolved");
  });

  it("does not resolve a decisive review with an unresolvable author", async () => {
    const artifacts = await fetchPhaseArtifacts(issueRef, {
      reviewerAppId: 42,
      reviewerAppLogin: "legion-reviewer[bot]",
      runner: runnerFor({
        isDraft: false,
        headRefOid: "current-head",
        merged: false,
        autoMergeRequest: null,
        latestReviews: {
          nodes: [{ state: "APPROVED", commit: { oid: "current-head" }, author: null }],
        },
        commits: { nodes: [] },
      }),
    });

    expect(artifacts.nativeReviewOnHead).toBeNull();
    expect(artifacts.resolved.nativeReviewOnHead).toBe("unresolvable");
  });

  it("uses an explicit complete artifact shape", () => {
    const artifacts: PhaseArtifacts = {
      hasNonDraftPr: false,
      headSha: null,
      testerCheckOnHead: null,
      architectCheckOnHead: null,
      nativeReviewOnHead: null,
      autoMergeEnabledOrMerged: false,
      merged: false,
      planHandoff: "unresolvable",
      resolved: {
        hasNonDraftPr: "resolved",
        headSha: "unresolvable",
        testerCheckOnHead: "unresolvable",
        architectCheckOnHead: "unresolvable",
        nativeReviewOnHead: "unresolvable",
        autoMergeEnabledOrMerged: "unresolvable",
        merged: "resolved",
        planHandoff: "unresolvable",
      },
    };

    expect(artifacts.resolved.planHandoff).toBe("unresolvable");
  });
});
