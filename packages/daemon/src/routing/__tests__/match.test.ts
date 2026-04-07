import { describe, expect, it } from "bun:test";
import { matchRouting } from "../match";
import type { RoutingConfig } from "../schema";

const testConfig: RoutingConfig = {
  domains: [
    {
      name: "envoy",
      paths: ["packages/envoy/**", "packages/contracts/**"],
      reviewers: ["envoy-expert"],
    },
    {
      name: "daemon",
      paths: ["packages/daemon/**"],
      reviewers: ["daemon-expert"],
    },
    {
      name: "skills",
      paths: [".opencode/skills/**"],
      reviewers: ["skills-expert", "daemon-expert"],
    },
  ],
};

describe("matchRouting", () => {
  it("matches a single domain by glob pattern", () => {
    const result = matchRouting(testConfig, ["packages/envoy/src/foo.ts"]);
    expect(result.reviewers).toEqual(["envoy-expert"]);
    expect(result.matchedDomains).toEqual([{ name: "envoy", reviewers: ["envoy-expert"] }]);
  });

  it("matches nested files within a glob pattern", () => {
    const result = matchRouting(testConfig, ["packages/daemon/src/state/decision.ts"]);
    expect(result.reviewers).toEqual(["daemon-expert"]);
    expect(result.matchedDomains).toHaveLength(1);
    expect(result.matchedDomains[0].name).toBe("daemon");
  });

  it("returns no matches when no files match any domain", () => {
    const result = matchRouting(testConfig, ["README.md", "package.json"]);
    expect(result.reviewers).toEqual([]);
    expect(result.matchedDomains).toEqual([]);
  });

  it("matches multiple domains when files span domains", () => {
    const result = matchRouting(testConfig, [
      "packages/envoy/src/foo.ts",
      "packages/daemon/src/bar.ts",
    ]);
    expect(result.reviewers).toContain("envoy-expert");
    expect(result.reviewers).toContain("daemon-expert");
    expect(result.matchedDomains).toHaveLength(2);
  });

  it("deduplicates reviewers across domains", () => {
    // daemon-expert appears in both "daemon" and "skills" domains
    const result = matchRouting(testConfig, [
      "packages/daemon/src/bar.ts",
      ".opencode/skills/legion-worker/SKILL.md",
    ]);
    expect(result.reviewers).toContain("daemon-expert");
    expect(result.reviewers).toContain("skills-expert");
    // daemon-expert should appear only once
    const daemonCount = result.reviewers.filter((r) => r === "daemon-expert").length;
    expect(daemonCount).toBe(1);
  });

  it("matches when any file in the list matches a domain", () => {
    const result = matchRouting(testConfig, [
      "README.md",
      "packages/envoy/src/deep/nested/file.ts",
      "docs/plan.md",
    ]);
    expect(result.reviewers).toEqual(["envoy-expert"]);
  });

  it("matches contracts path under envoy domain", () => {
    const result = matchRouting(testConfig, ["packages/contracts/src/types.ts"]);
    expect(result.reviewers).toEqual(["envoy-expert"]);
    expect(result.matchedDomains[0].name).toBe("envoy");
  });

  it("handles empty file list", () => {
    const result = matchRouting(testConfig, []);
    expect(result.reviewers).toEqual([]);
    expect(result.matchedDomains).toEqual([]);
  });

  it("matches all three domains when files span all of them", () => {
    const result = matchRouting(testConfig, [
      "packages/envoy/src/foo.ts",
      "packages/daemon/src/bar.ts",
      ".opencode/skills/legion-controller/SKILL.md",
    ]);
    expect(result.matchedDomains).toHaveLength(3);
    expect(result.reviewers).toContain("envoy-expert");
    expect(result.reviewers).toContain("daemon-expert");
    expect(result.reviewers).toContain("skills-expert");
  });
});
