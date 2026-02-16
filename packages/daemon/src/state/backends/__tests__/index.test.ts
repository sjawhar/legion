import { describe, expect, it } from "bun:test";
import { getBackend } from "../index";

describe("getBackend", () => {
  it("returns a LinearTracker for 'linear'", () => {
    const backend = getBackend("linear");
    expect(backend).toBeDefined();
    expect(typeof backend.parseIssues).toBe("function");
    expect(typeof backend.resolveTeamId).toBe("function");
  });

  it("returns a GitHubTracker for 'github'", () => {
    const backend = getBackend("github");
    expect(backend).toBeDefined();
    expect(typeof backend.parseIssues).toBe("function");
    expect(typeof backend.resolveTeamId).toBe("function");
  });

  it("throws for unknown backend", () => {
    expect(() => getBackend("jira" as any)).toThrow("Unknown backend");
  });
});
