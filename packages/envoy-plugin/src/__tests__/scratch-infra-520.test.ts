import { describe, expect, it } from "bun:test";

describe("scratch INFRA-520", () => {
  it("fails on purpose so ci-ok goes red", () => {
    expect(1).toBe(2);
  });
});
