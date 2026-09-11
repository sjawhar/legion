import { describe, expect, it } from "bun:test";
import { roleToken } from "@legion/contracts";
import opened from "../../../../contracts/fixtures/github-envelopes/pull-request-opened.json";
import { type LegionState, newLegionState } from "../legion-state";
import { type ReducerConfig, reduceGithubEvent } from "../reducers";

const config: ReducerConfig = {
  maxFixAttempts: 3,
};

describe("Envoy GitHub envelope goldens", () => {
  it("ignores a pull request without a Dispatch branch or body linkage", () => {
    const state = newLegionState("omp", 4);

    expect(
      reduceGithubEvent(
        state,
        opened.topic,
        {
          event_id: "golden-opened",
          issued_at: 0,
          payload_summary: opened.payload_summary,
          payload: opened.payload,
        },
        config
      )
    ).toEqual([]);
    expect(state.prs).toEqual({});
  });

  // No Go-generated golden fixture for `pull_request_review` exists yet under
  // `packages/contracts/fixtures/github-envelopes/` — this is the inline stand-in, matching
  // `githubPayload` in `packages/envoy/internal/contracts/normalize.go`: flat strings,
  // `commit_id` the sha the review was submitted against, `head_sha` the PR's head at delivery.
  it("records an approval at the current head and wakes the active role", () => {
    const state: LegionState = newLegionState("omp", 4);
    const issue = "WIDGETS-1";
    const architect = roleToken(state.project, issue, "architect");
    state.issues[issue] = { key: issue, title: "Issue one", status: "in_progress", children: [] };
    state.trees[issue] = { root: issue, generation: 1, status: "active", launchFailures: 0 };
    state.roles[architect] = { issue, role: "architect" };
    state.prs["example-org/example-repo#42"] = {
      key: issue,
      repo: "example-org/example-repo",
      number: 42,
      headSha: "1111111111111111111111111111111111111111",
      verdict: null,
      failing: [],
      failingStatuses: [],
      ciSettledAt: null,
      ciCheckRuns: null,
      ciSettlementGeneration: null,
      ciSnapshot: null,
      ciReconciled: false,
      fixAttempts: 0,
    };

    const reviewPayload = {
      kind: "review",
      action: "submitted",
      repo: "example-org/example-repo",
      number: "42",
      title: "Add local acceptance driver",
      parent_kind: "pr",
      author: "octocat",
      url: "https://example.invalid/example-org/example-repo/pull/42#pullrequestreview-1",
      state: "approved",
      body: "Looks good",
      commit_id: "1111111111111111111111111111111111111111",
      head_sha: "1111111111111111111111111111111111111111",
    };

    expect(
      reduceGithubEvent(
        state,
        "notifications.github.example-org.example-repo.pr.42.review",
        {
          event_id: "golden-review-approved",
          issued_at: 0,
          payload: reviewPayload,
        },
        config
      )
    ).toContainEqual({
      kind: "publish",
      role: architect,
      payload: {
        type: "pr-review",
        state: "approved",
        author: "octocat",
        body: "Looks good",
      },
    });
    expect(state.prs["example-org/example-repo#42"]?.reviewDecision).toBe("approved");
  });
});
