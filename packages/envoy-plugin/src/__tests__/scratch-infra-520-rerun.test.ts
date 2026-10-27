import { describe, expect, it } from "bun:test";

describe("scratch INFRA-520 re-run", () => {
  it("fails on the run's first attempt and passes on a re-run", () => {
    expect(process.env.GITHUB_RUN_ATTEMPT).not.toBe("1");
  });
});
