/**
 * Tests for GitHub GraphQL project fetching with organization/user fallback.
 */

import { describe, expect, it } from "bun:test";
import type { CommandRunner } from "../fetch";
import { fetchGitHubProjectItems } from "../github-fetch";

describe("fetchGitHubProjectItems", () => {
  it("successfully fetches from organization", async () => {
    const mockRunner: CommandRunner = async (cmd: string[]) => {
      expect(cmd).toContain("graphql");
      const queryArg = cmd.find((arg) => arg.startsWith("query="));
      expect(queryArg).toContain("organization(login:");

      return {
        stdout: JSON.stringify({
          data: {
            organization: {
              projectV2: {
                items: {
                  pageInfo: { hasNextPage: false, endCursor: null },
                  nodes: [
                    {
                      id: "item1",
                      fieldValueByName: { name: "Todo" },
                      labels: { labels: { nodes: [{ name: "bug" }] } },
                      content: {
                        __typename: "Issue",
                        number: 42,
                        title: "Test Issue",
                        url: "https://github.com/owner/repo/issues/42",
                        repository: { nameWithOwner: "owner/repo" },
                        linkedPullRequests: { nodes: [] },
                      },
                    },
                  ],
                },
              },
            },
          },
        }),
        stderr: "",
        exitCode: 0,
      };
    };

    const result = await fetchGitHubProjectItems("testorg", 1, mockRunner);

    expect(result.items).toHaveLength(1);
    expect(result.items[0]).toMatchObject({
      id: "item1",
      status: "Todo",
      content: {
        type: "Issue",
        number: 42,
        title: "Test Issue",
      },
    });
  });

  it("falls back to user query when organization query returns access error", async () => {
    let callCount = 0;
    const mockRunner: CommandRunner = async (cmd: string[]) => {
      callCount++;
      const queryArg = cmd.find((arg) => arg.startsWith("query="));

      if (callCount === 1) {
        // First call should be organization query
        expect(queryArg).toContain("organization(login:");

        return {
          stdout: JSON.stringify({
            errors: [{ message: "Could not resolve to an Organization with the name 'testuser'" }],
          }),
          stderr: "",
          exitCode: 0,
        };
      } else {
        // Second call should be user query
        expect(queryArg).toContain("user(login:");

        return {
          stdout: JSON.stringify({
            data: {
              user: {
                projectV2: {
                  items: {
                    pageInfo: { hasNextPage: false, endCursor: null },
                    nodes: [
                      {
                        id: "item2",
                        fieldValueByName: { name: "In Progress" },
                        labels: { labels: { nodes: [] } },
                        content: {
                          __typename: "Issue",
                          number: 43,
                          title: "User Issue",
                          url: "https://github.com/testuser/repo/issues/43",
                          repository: { nameWithOwner: "testuser/repo" },
                          linkedPullRequests: { nodes: [] },
                        },
                      },
                    ],
                  },
                },
              },
            },
          }),
          stderr: "",
          exitCode: 0,
        };
      }
    };

    const result = await fetchGitHubProjectItems("testuser", 1, mockRunner);

    expect(callCount).toBe(2);
    expect(result.items).toHaveLength(1);
    expect(result.items[0]).toMatchObject({
      id: "item2",
      status: "In Progress",
      content: {
        type: "Issue",
        number: 43,
        title: "User Issue",
      },
    });
  });

  it("falls back to user query when organization query fails with exception", async () => {
    let callCount = 0;
    const mockRunner: CommandRunner = async (cmd: string[]) => {
      callCount++;
      const queryArg = cmd.find((arg) => arg.startsWith("query="));

      if (callCount === 1) {
        // First call should be organization query and fail
        expect(queryArg).toContain("organization(login:");
        throw new Error("GitHub GraphQL query failed (exit 1): Forbidden");
      } else {
        // Second call should be user query
        expect(queryArg).toContain("user(login:");

        return {
          stdout: JSON.stringify({
            data: {
              user: {
                projectV2: {
                  items: {
                    pageInfo: { hasNextPage: false, endCursor: null },
                    nodes: [],
                  },
                },
              },
            },
          }),
          stderr: "",
          exitCode: 0,
        };
      }
    };

    const result = await fetchGitHubProjectItems("testuser", 1, mockRunner);

    expect(callCount).toBe(2);
    expect(result.items).toHaveLength(0);
  });

  it("continues using user query for pagination after fallback", async () => {
    let callCount = 0;
    const mockRunner: CommandRunner = async (cmd: string[]) => {
      callCount++;
      const queryArg = cmd.find((arg) => arg.startsWith("query="));

      if (callCount === 1) {
        // First call - organization query fails
        expect(queryArg).toContain("organization(login:");

        return {
          stdout: JSON.stringify({
            errors: [{ message: "Could not resolve to an Organization" }],
          }),
          stderr: "",
          exitCode: 0,
        };
      } else if (callCount === 2) {
        // Second call - user query with first page
        expect(queryArg).toContain("user(login:");
        expect(cmd).not.toContain("after=");

        return {
          stdout: JSON.stringify({
            data: {
              user: {
                projectV2: {
                  items: {
                    pageInfo: { hasNextPage: true, endCursor: "cursor1" },
                    nodes: [
                      {
                        id: "item1",
                        fieldValueByName: { name: "Todo" },
                        labels: { labels: { nodes: [] } },
                        content: {
                          __typename: "Issue",
                          number: 1,
                          title: "Issue 1",
                          url: "https://github.com/testuser/repo/issues/1",
                          repository: { nameWithOwner: "testuser/repo" },
                          linkedPullRequests: { nodes: [] },
                        },
                      },
                    ],
                  },
                },
              },
            },
          }),
          stderr: "",
          exitCode: 0,
        };
      } else {
        // Third call - user query with second page
        expect(queryArg).toContain("user(login:");
        expect(cmd).toContain("after=cursor1");

        return {
          stdout: JSON.stringify({
            data: {
              user: {
                projectV2: {
                  items: {
                    pageInfo: { hasNextPage: false, endCursor: null },
                    nodes: [
                      {
                        id: "item2",
                        fieldValueByName: { name: "Done" },
                        labels: { labels: { nodes: [] } },
                        content: {
                          __typename: "Issue",
                          number: 2,
                          title: "Issue 2",
                          url: "https://github.com/testuser/repo/issues/2",
                          repository: { nameWithOwner: "testuser/repo" },
                          linkedPullRequests: { nodes: [] },
                        },
                      },
                    ],
                  },
                },
              },
            },
          }),
          stderr: "",
          exitCode: 0,
        };
      }
    };

    const result = await fetchGitHubProjectItems("testuser", 1, mockRunner);

    expect(callCount).toBe(3);
    expect(result.items).toHaveLength(2);
    expect((result.items[0].content as { number: number }).number).toBe(1);
    expect((result.items[1].content as { number: number }).number).toBe(2);
  });

  it("throws error when both organization and user queries fail", async () => {
    let callCount = 0;
    const mockRunner: CommandRunner = async (cmd: string[]) => {
      callCount++;
      const queryArg = cmd.find((arg) => arg.startsWith("query="));

      if (callCount === 1) {
        expect(queryArg).toContain("organization(login:");
        throw new Error("GitHub GraphQL query failed (exit 1): Forbidden");
      } else {
        expect(queryArg).toContain("user(login:");

        return {
          stdout: JSON.stringify({
            errors: [{ message: "User not found" }],
          }),
          stderr: "",
          exitCode: 0,
        };
      }
    };

    await expect(fetchGitHubProjectItems("nonexistent", 1, mockRunner)).rejects.toThrow(
      "GraphQL errors: User not found"
    );

    expect(callCount).toBe(2);
  });

  it("throws error when organization query has non-access related errors", async () => {
    const mockRunner: CommandRunner = async (cmd: string[]) => {
      const queryArg = cmd.find((arg) => arg.startsWith("query="));
      expect(queryArg).toContain("organization(login:");

      return {
        stdout: JSON.stringify({
          errors: [{ message: "Rate limit exceeded" }],
        }),
        stderr: "",
        exitCode: 0,
      };
    };

    await expect(fetchGitHubProjectItems("testorg", 1, mockRunner)).rejects.toThrow(
      "GraphQL errors: Rate limit exceeded"
    );
  });
});
