/**
 * Tests for shared GitHub state types.
 */

import { describe, expect, it } from "bun:test";
import { GitHubPRRef } from "../types";

describe("GitHubPRRef.fromUrl", () => {
  it("parses valid PR URL", () => {
    const url = "https://github.com/owner/repo/pull/123";
    const ref = GitHubPRRef.fromUrl(url);

    expect(ref).not.toBeNull();
    expect(ref?.owner).toBe("owner");
    expect(ref?.repo).toBe("repo");
    expect(ref?.number).toBe(123);
  });

  it("returns null for invalid URL format", () => {
    expect(GitHubPRRef.fromUrl("not-a-url")).toBeNull();
    expect(GitHubPRRef.fromUrl("https://github.com/owner/repo/issues/123")).toBeNull();
    expect(GitHubPRRef.fromUrl("https://gitlab.com/owner/repo/pull/123")).toBeNull();
  });

  it("returns null for unreasonably large PR number", () => {
    const url = "https://github.com/owner/repo/pull/9999999999";
    const ref = GitHubPRRef.fromUrl(url);
    expect(ref).toBeNull();
  });

  it("handles owner and repo with hyphens, underscores, dots", () => {
    const url = "https://github.com/my-org.test/my_repo-2.0/pull/456";
    const ref = GitHubPRRef.fromUrl(url);

    expect(ref).not.toBeNull();
    expect(ref?.owner).toBe("my-org.test");
    expect(ref?.repo).toBe("my_repo-2.0");
    expect(ref?.number).toBe(456);
  });
});
