import { describe, expect, it } from "bun:test";
import { formatIssueKey } from "@legion/contracts";
import { reduceCheck, settle } from "../ci-reducer";
import type { PrState } from "../legion-state";

const QUIET_MS = 30_000;

function prState(headSha = "head-1"): PrState {
  return {
    key: formatIssueKey("owner", "repo", 1),
    repo: "owner/repo",
    number: 1,
    headSha,
    checks: {},
    firstRedEmitted: false,
    settledRedEmitted: false,
    greenEmitted: false,
    lastEventAt: 0,
    fixAttempts: 0,
  };
}

function resetForSynchronizedPush(pr: PrState, headSha: string, now: number): void {
  pr.headSha = headSha;
  pr.checks = {};
  pr.firstRedEmitted = false;
  pr.settledRedEmitted = false;
  pr.greenEmitted = false;
  pr.lastEventAt = now;
}

function shuffled<T>(items: readonly T[]): T[] {
  const result = [...items];
  let seed = 0x6d2b79f5;

  for (let index = result.length - 1; index > 0; index--) {
    seed = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    const selected = (seed >>> 0) % (index + 1);
    [result[index], result[selected]] = [result[selected], result[index]];
  }

  return result;
}

describe("CI edge reducer", () => {
  it("drops a check observation for a superseded head without changing the current burst", () => {
    const pr = prState();

    reduceCheck(
      pr,
      {
        sha: "old-head",
        name: "unit",
        status: "completed",
        conclusion: "failure",
      },
      100
    );

    expect(pr.checks).toEqual({});
    expect(pr.lastEventAt).toBe(0);
    expect(settle(pr, 100 + QUIET_MS, QUIET_MS)).toEqual([]);
  });

  it("emits the first current-head failure immediately only once", () => {
    const pr = prState();

    reduceCheck(
      pr,
      {
        sha: pr.headSha,
        name: "lint",
        status: "completed",
        conclusion: "failure",
      },
      100
    );

    expect(settle(pr, 100, QUIET_MS)).toEqual([
      { type: "ci-first-red", check: "lint", sha: pr.headSha },
    ]);
    expect(pr.firstRedEmitted).toBe(true);
    expect(settle(pr, 100, QUIET_MS)).toEqual([]);
  });

  it("emits one complete settled-red batch after a randomized eight-observation burst", () => {
    const pr = prState();
    const burst = shuffled([
      { name: "build", status: "completed", conclusion: "success" },
      { name: "docs", status: "completed", conclusion: "success" },
      { name: "e2e", status: "completed", conclusion: "success" },
      { name: "lint", status: "completed", conclusion: "failure" },
      { name: "package", status: "completed", conclusion: "success" },
      { name: "test", status: "completed", conclusion: "success" },
      { name: "unit", status: "completed", conclusion: "failure" },
      { name: "verify", status: "completed", conclusion: "success" },
    ]);
    const emissions = [];
    let now = 0;

    for (const observation of burst) {
      now += 1_000;
      reduceCheck(pr, { ...observation, sha: pr.headSha }, now);
      emissions.push(...settle(pr, now, QUIET_MS));
    }

    expect(settle(pr, now + QUIET_MS - 1, QUIET_MS)).toEqual([]);
    emissions.push(...settle(pr, now + QUIET_MS, QUIET_MS));
    emissions.push(...settle(pr, now + QUIET_MS, QUIET_MS));

    expect(emissions.map((emission) => emission.type)).toEqual(["ci-first-red", "ci-settled-red"]);
    expect(emissions[1]).toEqual({
      type: "ci-settled-red",
      failing: ["lint", "unit"],
      sha: pr.headSha,
    });
    expect(pr.settledRedEmitted).toBe(true);
  });

  it("waits for every seen check to complete and the quiet window before emitting green", () => {
    const pr = prState();

    reduceCheck(
      pr,
      {
        sha: pr.headSha,
        name: "build",
        status: "completed",
        conclusion: "success",
      },
      100
    );
    reduceCheck(
      pr,
      {
        sha: pr.headSha,
        name: "unit",
        status: "in_progress",
        conclusion: null,
      },
      200
    );

    expect(settle(pr, 200 + QUIET_MS, QUIET_MS)).toEqual([]);

    reduceCheck(
      pr,
      {
        sha: pr.headSha,
        name: "unit",
        status: "completed",
        conclusion: "success",
      },
      300
    );

    expect(settle(pr, 300 + QUIET_MS - 1, QUIET_MS)).toEqual([]);
    expect(settle(pr, 300 + QUIET_MS, QUIET_MS)).toEqual([{ type: "ci-green", sha: pr.headSha }]);
    expect(settle(pr, 300 + QUIET_MS, QUIET_MS)).toEqual([]);
  });

  it("emits green for a new all-green head after synchronize resets the edge flags", () => {
    const pr = prState();

    reduceCheck(
      pr,
      {
        sha: pr.headSha,
        name: "unit",
        status: "completed",
        conclusion: "failure",
      },
      100
    );
    expect(settle(pr, 100, QUIET_MS)).toEqual([
      { type: "ci-first-red", check: "unit", sha: "head-1" },
    ]);

    resetForSynchronizedPush(pr, "head-2", 200);
    reduceCheck(
      pr,
      {
        sha: pr.headSha,
        name: "build",
        status: "completed",
        conclusion: "success",
      },
      300
    );
    reduceCheck(
      pr,
      {
        sha: pr.headSha,
        name: "unit",
        status: "completed",
        conclusion: "success",
      },
      400
    );

    expect(settle(pr, 400 + QUIET_MS, QUIET_MS)).toEqual([{ type: "ci-green", sha: "head-2" }]);
  });

  it("emits nothing for queued and running transitions", () => {
    const pr = prState();

    reduceCheck(pr, { sha: pr.headSha, name: "unit", status: "queued", conclusion: null }, 100);
    reduceCheck(
      pr,
      {
        sha: pr.headSha,
        name: "unit",
        status: "in_progress",
        conclusion: null,
      },
      200
    );

    expect(settle(pr, 200 + QUIET_MS, QUIET_MS)).toEqual([]);
  });
});
