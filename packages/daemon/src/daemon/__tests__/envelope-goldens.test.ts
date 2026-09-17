import { describe, expect, it } from "bun:test";
import { roleToken } from "@legion/contracts";
import opened from "../../../../contracts/fixtures/github-envelopes/pull-request-opened.json";
import push from "../../../../contracts/fixtures/github-envelopes/push.json";
import { type LegionState, newLegionState } from "../legion-state";
import { type ReducerConfig, reduceGithubEvent } from "../reducers";

const config: ReducerConfig = {
  maxFixAttempts: 3,
  projects: { WIDGETS: { repo: "example-org/example-repo" } },
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

  // The push golden is regenerated from normalize.go, so this proves the TypeScript classifier
  // reads exactly the field shapes the listener emits (`changed_paths` newline-joined,
  // `changed_paths_truncated` as the string "false").
  it("counts the Go-normalized push golden as a real fix attempt", () => {
    const state: LegionState = newLegionState("omp", 4);
    const issue = "WIDGETS-1";
    const architect = roleToken(state.project, issue, "architect");
    state.issues[issue] = { key: issue, title: "Issue one", status: "in_progress", children: [] };
    state.trees[issue] = { root: issue, generation: 1, status: "active", launchFailures: 0 };
    state.roles[architect] = { issue, role: "architect" };
    const prKey = "example-org/example-repo#42";
    state.prs[prKey] = {
      key: issue,
      repo: "example-org/example-repo",
      number: 42,
      headSha: push.payload.before,
      verdict: "red",
      failing: ["unit"],
      failingStatuses: [],
      ciSettledAt: 1,
      ciCheckRuns: null,
      ciSettlementGeneration: null,
      ciSnapshot: null,
      ciReconciled: false,
      fixAttempts: 0,
    };
    state.prByBranch["example-org/example-repo@main"] = prKey;

    expect(
      reduceGithubEvent(
        state,
        push.topic,
        {
          event_id: "golden-push",
          issued_at: 0,
          payload_summary: push.payload_summary,
          payload: push.payload,
        },
        config
      )
    ).toEqual([]);
    expect(state.prs[prKey]?.pendingPush).toEqual({ sha: push.payload.after, handoffOnly: false });

    expect(
      reduceGithubEvent(
        state,
        "notifications.github.example-org.example-repo.pr.42",
        {
          event_id: "golden-push-synchronize",
          issued_at: 0,
          payload: {
            kind: "pr",
            action: "synchronize",
            repo: "example-org/example-repo",
            number: "42",
            head_ref: "main",
            head_sha: push.payload.after,
          },
        },
        config
      )
    ).toEqual([]);
    expect(state.prs[prKey]).toMatchObject({
      headSha: push.payload.after,
      fixAttempts: 1,
      headCounted: true,
    });
    expect(state.prs[prKey]?.pendingPush).toBeUndefined();
  });
});
