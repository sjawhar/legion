import { describe, expect, it } from "bun:test";
import { formatIssueKey } from "@legion/contracts";
import closedMerged from "../../../../contracts/fixtures/github-envelopes/pull-request-closed-merged.json";
import opened from "../../../../contracts/fixtures/github-envelopes/pull-request-opened.json";
import synchronized from "../../../../contracts/fixtures/github-envelopes/pull-request-synchronize.json";
import { newLegionState } from "../legion-state";
import { type ReducerConfig, reduceGithubEvent } from "../reducers";

const config: ReducerConfig = {
  boardProjectIds: [],
  appLogins: [],
  maxFixAttempts: 3,
};

describe("Envoy GitHub envelope goldens", () => {
  it("reduces the pull request lifecycle from shared normalized fixtures", () => {
    const state = newLegionState("omp", 4);
    const repo = "example-org/example-repo";
    const number = 42;
    const issue = formatIssueKey("example-org", "example-repo", number);
    const prKey = `${repo}#${number}`;
    state.issues[issue] = {
      key: issue,
      title: "Fixture issue",
      state: "open",
      children: [],
      released: false,
      labels: [],
    };

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
    );

    expect(state.prs[prKey]).toMatchObject({ headSha: opened.payload.head_sha });
    Object.assign(state.prs[prKey], {
      verdict: "red",
      failing: ["unit"],
      ciSettledAt: 1,
      ciGeneration: 1,
      reviewDecision: "approved",
    });

    reduceGithubEvent(
      state,
      synchronized.topic,
      {
        event_id: "golden-synchronize",
        issued_at: 0,
        payload_summary: synchronized.payload_summary,
        payload: synchronized.payload,
      },
      config
    );

    expect(state.prs[prKey]).toMatchObject({
      headSha: synchronized.payload.head_sha,
      headUpdatedAt: Date.parse(synchronized.payload.updated_at),
      verdict: null,
      failing: [],
      ciSettledAt: null,
      ciGeneration: null,
      fixAttempts: 1,
    });

    expect(
      reduceGithubEvent(
        state,
        closedMerged.topic,
        {
          event_id: "golden-closed-merged",
          issued_at: 0,
          payload_summary: closedMerged.payload_summary,
          payload: closedMerged.payload,
        },
        config
      )
    ).toEqual([]);
    expect(state.prs[prKey]).toBeDefined();
  });
});
