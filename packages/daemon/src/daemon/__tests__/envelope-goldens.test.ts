import { describe, expect, it } from "bun:test";
import opened from "../../../../contracts/fixtures/github-envelopes/pull-request-opened.json";
import { newLegionState } from "../legion-state";
import { type ReducerConfig, reduceGithubEvent } from "../reducers";

const config: ReducerConfig = {
  appLogins: [],
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
});
