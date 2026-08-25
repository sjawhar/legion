/**
 * Tests for retained issue-tracker state types.
 */

import { describe, expect, it } from "bun:test";
import { GitHubPRRef, IssueStatus } from "../types";

describe("IssueStatus.normalize", () => {
	it("returns direct match unchanged", () => {
		expect(IssueStatus.normalize("Todo")).toBe("Todo");
		expect(IssueStatus.normalize("In Progress")).toBe("In Progress");
	});

	it("normalizes alias to canonical form", () => {
		expect(IssueStatus.normalize("In Review")).toBe("Needs Review");
	});

	it("normalizes case-insensitive canonical match", () => {
		expect(IssueStatus.normalize("in progress")).toBe("In Progress");
		expect(IssueStatus.normalize("In progress")).toBe("In Progress");
		expect(IssueStatus.normalize("IN PROGRESS")).toBe("In Progress");
		expect(IssueStatus.normalize("todo")).toBe("Todo");
		expect(IssueStatus.normalize("BACKLOG")).toBe("Backlog");
		expect(IssueStatus.normalize("needs review")).toBe("Needs Review");
	});

	it("normalizes case-insensitive alias match", () => {
		expect(IssueStatus.normalize("in review")).toBe("Needs Review");
		expect(IssueStatus.normalize("IN REVIEW")).toBe("Needs Review");
	});

	it("returns unknown status unchanged", () => {
		expect(IssueStatus.normalize("Unknown")).toBe("Unknown");
		expect(IssueStatus.normalize("Scrapped")).toBe("Scrapped");
	});

	it("normalizes Today to Todo", () => {
		expect(IssueStatus.normalize("Today")).toBe("Todo");
		expect(IssueStatus.normalize("today")).toBe("Todo");
	});

	it("returns empty string for null", () => {
		expect(IssueStatus.normalize(null)).toBe("");
	});
});

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
		expect(
			GitHubPRRef.fromUrl("https://github.com/owner/repo/issues/123"),
		).toBeNull();
		expect(
			GitHubPRRef.fromUrl("https://gitlab.com/owner/repo/pull/123"),
		).toBeNull();
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
