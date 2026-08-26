import { describe, expect, test } from "bun:test";
import {
  controllerToken,
  formatIssueKey,
  LEGION_ROLES,
  parseIssueKey,
  parseRoleToken,
  roleToken,
  roleTopic,
  sanitizeToken,
} from "./legion-roles";

const ENVOY_ROLE_TOKEN = /^[a-z0-9][a-z0-9_-]*$/;

describe("issue keys", () => {
  test("formats and parses owner/repo issue references", () => {
    const issue = formatIssueKey("sjawhar", "legion", 42);

    expect(issue).toBe("sjawhar/legion#42");
    expect(parseIssueKey(issue)).toEqual({ owner: "sjawhar", repo: "legion", number: 42 });
  });

  test("rejects malformed issue references", () => {
    expect(parseIssueKey("sjawhar/legion")).toBeUndefined();
    expect(parseIssueKey("sjawhar/legion#not-a-number")).toBeUndefined();
    expect(parseIssueKey("sjawhar/legion#42-extra")).toBeUndefined();
  });
});

describe("role token grammar", () => {
  test("matches the injective Legion token grammar", () => {
    const token = roleToken("omp", "sjawhar/legion#42", "implementer");

    expect(token).toBe("legion-omp-sjawhar__legion-42-implementer");
    expect(roleTopic(token)).toBe("notifications.role.legion-omp-sjawhar__legion-42-implementer");
  });

  test("round-trips the complete issue key", () => {
    const issue = formatIssueKey("sjawhar", "legion", 42);
    const token = roleToken("omp", issue, "implementer");

    expect(parseRoleToken("omp", token)).toEqual({
      project: "omp",
      issue,
      role: "implementer",
    });
  });

  test("distinguishes repositories that collided after dash sanitization", () => {
    const firstIssue = formatIssueKey("foo-bar", "baz", 17);
    const secondIssue = formatIssueKey("foo", "bar-baz", 17);
    const first = roleToken("omp", firstIssue, "reviewer");
    const second = roleToken("omp", secondIssue, "reviewer");

    expect(first).toBe("legion-omp-foo_hbar__baz-17-reviewer");
    expect(second).toBe("legion-omp-foo__bar_hbaz-17-reviewer");
    expect(first).not.toBe(second);
    expect(parseRoleToken("omp", first)).toEqual({
      project: "omp",
      issue: firstIssue,
      role: "reviewer",
    });
    expect(parseRoleToken("omp", second)).toEqual({
      project: "omp",
      issue: secondIssue,
      role: "reviewer",
    });
  });

  test("round-trips dots, underscores, and hyphens in issue names", () => {
    const issue = formatIssueKey("Owner.Name", "repo_name-with.dot", 9);
    const token = roleToken("omp", issue, "tester");

    expect(token).toBe("legion-omp-owner_dname__repo_uname_hwith_ddot-9-tester");
    expect(parseRoleToken("omp", token)).toEqual({
      project: "omp",
      issue: "owner.name/repo_name-with.dot#9",
      role: "tester",
    });
  });

  test("round-trips the controller token", () => {
    const token = controllerToken("omp");

    expect(token).toBe("legion-omp-controller");
    expect(parseRoleToken("omp", token)).toEqual({ controller: true });
  });

  test("rejects invalid mint projects loudly", () => {
    expect(() => roleToken("omp-tool", "sjawhar/legion#42", "implementer")).toThrow(
      "Invalid Legion project token"
    );
  });

  test("rejects a token for another project or an unknown role", () => {
    expect(parseRoleToken("other", controllerToken("omp"))).toBeUndefined();
    expect(parseRoleToken("omp", "legion-omp-sjawhar__legion-42-operator")).toBeUndefined();
  });
});

describe("sanitizeToken", () => {
  test("lowercases and normalizes dots, uppercase letters, and underscores", () => {
    expect(sanitizeToken("OMP.Project_Name")).toBe("omp-project-name");
    expect(sanitizeToken("...---___")).toBe("x");
  });

  test("always produces an envoy-valid token from fuzzed inputs", () => {
    let seed = 0x12345678;
    const next = () => {
      seed = (seed * 1664525 + 1013904223) >>> 0;
      return seed;
    };
    const randomTokenPart = () => {
      const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._-";
      const length = (next() % 32) + 1;
      let value = "";
      for (let index = 0; index < length; index += 1) {
        value += alphabet[next() % alphabet.length];
      }
      return value;
    };

    for (let index = 0; index < 1_000; index += 1) {
      const owner = randomTokenPart();
      const repo = randomTokenPart();
      const issue = formatIssueKey(owner, repo, (next() % 10_000) + 1);
      const role = LEGION_ROLES[next() % LEGION_ROLES.length];
      const token = roleToken("omp", issue, role);

      expect(sanitizeToken(randomTokenPart())).toMatch(ENVOY_ROLE_TOKEN);
      expect(token).toMatch(ENVOY_ROLE_TOKEN);
    }
  });
});
