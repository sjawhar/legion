import { describe, expect, test } from "bun:test";
import {
  controllerToken,
  LEGION_ROLES,
  parseRoleToken,
  roleToken,
  roleTopic,
  sanitizeToken,
} from "./legion-roles";

const ENVOY_ROLE_TOKEN = /^[a-z0-9][a-z0-9_-]*$/;

describe("role token grammar", () => {
  test("matches the injective Legion token grammar", () => {
    const token = roleToken("omp", "LEGION-42", "implementer");

    expect(token).toBe("legion-omp-legion-42-implementer");
    expect(roleTopic(token)).toBe("notifications.role.legion-omp-legion-42-implementer");
  });

  test("round-trips the complete issue key", () => {
    const issue = "LEGION-42";
    const token = roleToken("omp", issue, "implementer");

    expect(parseRoleToken("omp", token)).toEqual({
      project: "omp",
      issue,
      role: "implementer",
    });
  });

  test("encodes and round-trips a Dispatch issue key", () => {
    const token = roleToken("legion", "LEGION-7", "architect");

    expect(token).toBe("legion-legion-legion-7-architect");
    expect(parseRoleToken("legion", token)).toEqual({
      project: "legion",
      issue: "LEGION-7",
      role: "architect",
    });
  });

  test("rejects an invalid Dispatch issue key", () => {
    expect(() => roleToken("omp", "sjawhar/legion#42", "implementer")).toThrow(
      "Invalid IssueKey: sjawhar/legion#42"
    );
  });

  test("round-trips the controller token", () => {
    const token = controllerToken("omp");

    expect(token).toBe("legion-omp-controller");
    expect(parseRoleToken("omp", token)).toEqual({ controller: true });
  });

  test("rejects invalid mint projects loudly", () => {
    expect(() => roleToken("omp-tool", "LEGION-42", "implementer")).toThrow(
      "Invalid Legion project token"
    );
  });

  test("rejects a token for another project or an unknown role", () => {
    expect(parseRoleToken("other", controllerToken("omp"))).toBeUndefined();
    expect(parseRoleToken("omp", "legion-omp-legion-42-operator")).toBeUndefined();
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
    const randomDispatchProject = () => {
      const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789";
      const length = (next() % 10) + 1;
      let value = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"[next() % 26];
      for (let index = 1; index < length; index += 1) {
        value += alphabet[next() % alphabet.length];
      }
      return value;
    };

    for (let index = 0; index < 1_000; index += 1) {
      const issue = `${randomDispatchProject()}-${(next() % 10_000) + 1}`;
      const role = LEGION_ROLES[next() % LEGION_ROLES.length];
      const token = roleToken("omp", issue, role);

      expect(sanitizeToken(randomTokenPart())).toMatch(ENVOY_ROLE_TOKEN);
      expect(token).toMatch(ENVOY_ROLE_TOKEN);
    }
  });
});
