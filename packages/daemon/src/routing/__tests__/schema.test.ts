import { describe, expect, it } from "bun:test";
import { DomainSchema, RoutingConfigSchema } from "../schema";

describe("DomainSchema", () => {
  it("validates a valid domain", () => {
    const result = DomainSchema.safeParse({
      name: "envoy",
      paths: ["packages/envoy/**"],
      reviewers: ["alice"],
    });
    expect(result.success).toBe(true);
  });

  it("validates domain with multiple paths and reviewers", () => {
    const result = DomainSchema.safeParse({
      name: "envoy",
      paths: ["packages/envoy/**", "packages/contracts/**"],
      reviewers: ["alice", "bob"],
    });
    expect(result.success).toBe(true);
  });

  it("rejects empty name", () => {
    const result = DomainSchema.safeParse({
      name: "",
      paths: ["packages/envoy/**"],
      reviewers: ["alice"],
    });
    expect(result.success).toBe(false);
  });

  it("rejects empty paths array", () => {
    const result = DomainSchema.safeParse({
      name: "envoy",
      paths: [],
      reviewers: ["alice"],
    });
    expect(result.success).toBe(false);
  });

  it("rejects empty reviewers array", () => {
    const result = DomainSchema.safeParse({
      name: "envoy",
      paths: ["packages/envoy/**"],
      reviewers: [],
    });
    expect(result.success).toBe(false);
  });

  it("rejects missing fields", () => {
    expect(DomainSchema.safeParse({}).success).toBe(false);
    expect(DomainSchema.safeParse({ name: "x" }).success).toBe(false);
    expect(DomainSchema.safeParse({ name: "x", paths: ["a"] }).success).toBe(false);
  });

  it("rejects empty string in paths", () => {
    const result = DomainSchema.safeParse({
      name: "envoy",
      paths: [""],
      reviewers: ["alice"],
    });
    expect(result.success).toBe(false);
  });

  it("rejects empty string in reviewers", () => {
    const result = DomainSchema.safeParse({
      name: "envoy",
      paths: ["packages/envoy/**"],
      reviewers: [""],
    });
    expect(result.success).toBe(false);
  });
});

describe("RoutingConfigSchema", () => {
  it("validates a valid config", () => {
    const result = RoutingConfigSchema.safeParse({
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
      ],
    });
    expect(result.success).toBe(true);
    if (result.success) {
      expect(result.data.domains).toHaveLength(2);
    }
  });

  it("rejects empty domains array", () => {
    const result = RoutingConfigSchema.safeParse({ domains: [] });
    expect(result.success).toBe(false);
  });

  it("rejects missing domains key", () => {
    const result = RoutingConfigSchema.safeParse({});
    expect(result.success).toBe(false);
  });

  it("rejects non-object input", () => {
    expect(RoutingConfigSchema.safeParse(null).success).toBe(false);
    expect(RoutingConfigSchema.safeParse("string").success).toBe(false);
    expect(RoutingConfigSchema.safeParse(42).success).toBe(false);
  });
});
