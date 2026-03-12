/**
 * Fetch all GitHub project items using GraphQL cursor-based pagination.
 *
 * Replaces `gh project item-list --format json` which silently drops items
 * when projects have many entries (see: sjawhar/legion#82).
 *
 * Returns items in the same shape as `gh project item-list --format json`
 * so the existing GitHub backend parser can consume them directly.
 */

import type { CommandResult, CommandRunner } from "./fetch";
import { defaultRunner } from "./fetch";

interface GitHubProjectItemNode {
  id: string;
  fieldValueByName: {
    name?: string;
  } | null;
  labels: {
    nodes: Array<{ name: string }>;
  };
  content:
    | {
        __typename: "Issue" | "PullRequest" | "DraftIssue";
        number?: number;
        title?: string;
        url?: string;
        repository?: {
          nameWithOwner: string;
        };
        linkedPullRequests?: {
          nodes: Array<{ url: string }>;
        } | null;
      }
    | Record<string, never>;
}

interface GitHubProjectItemsPage {
  data?: {
    organization?: {
      projectV2?: {
        items: {
          pageInfo: {
            hasNextPage: boolean;
            endCursor: string | null;
          };
          nodes: GitHubProjectItemNode[];
        };
      };
    };
  };
  errors?: Array<{ message: string }>;
}

const ITEMS_PER_PAGE = 100;

const QUERY = `
query($owner: String!, $number: Int!, $first: Int!, $after: String) {
  organization(login: $owner) {
    projectV2(number: $number) {
      items(first: $first, after: $after) {
        pageInfo {
          hasNextPage
          endCursor
        }
        nodes {
          id
          fieldValueByName(name: "Status") {
            ... on ProjectV2ItemFieldSingleSelectValue {
              name
            }
          }
          labels: fieldValueByName(name: "Labels") {
            ... on ProjectV2ItemFieldLabelValue {
              labels(first: 20) {
                nodes { name }
              }
            }
          }
          content {
            __typename
            ... on Issue {
              number
              title
              url
              repository { nameWithOwner }
              linkedPullRequests: closedByPullRequestsReferences(first: 10, includeClosedPrs: true) {
                nodes { url }
              }
            }
            ... on PullRequest {
              number
              title
              url
              repository { nameWithOwner }
            }
            ... on DraftIssue {
              title
            }
          }
        }
      }
    }
  }
}`;

/**
 * Convert a GraphQL node into the shape expected by `gh project item-list --format json`.
 */
function nodeToProjectItem(node: GitHubProjectItemNode): Record<string, unknown> | null {
  const content = node.content;
  if (!content || !("__typename" in content)) {
    return null;
  }

  const typename = content.__typename;

  // Build content field matching gh CLI format
  const itemContent: Record<string, unknown> = {};
  if (typename === "Issue" || typename === "PullRequest") {
    itemContent.type = typename;
    itemContent.number = content.number;
    itemContent.title = content.title;
    itemContent.url = content.url;
    itemContent.repository = content.repository?.nameWithOwner;
  } else if (typename === "DraftIssue") {
    itemContent.type = "DraftIssue";
    itemContent.title = content.title;
  } else {
    return null;
  }

  // Extract labels
  const labelsField = node.labels as unknown;
  let labels: string[] = [];
  if (
    labelsField &&
    typeof labelsField === "object" &&
    "labels" in (labelsField as Record<string, unknown>)
  ) {
    const labelsObj = (labelsField as { labels?: { nodes?: Array<{ name: string }> } }).labels;
    labels = labelsObj?.nodes?.map((l) => l.name) ?? [];
  }

  // Extract linked PRs
  const linkedPRs: string[] = [];
  if (typename === "Issue" && content.linkedPullRequests?.nodes) {
    for (const pr of content.linkedPullRequests.nodes) {
      if (pr.url) linkedPRs.push(pr.url);
    }
  }

  return {
    id: node.id,
    content: itemContent,
    status: node.fieldValueByName?.name ?? null,
    labels,
    ...(linkedPRs.length > 0 ? { "linked pull requests": linkedPRs } : {}),
  };
}

/**
 * Fetch all items from a GitHub Project V2 using cursor-based GraphQL pagination.
 *
 * @param owner - GitHub organization or user
 * @param projectNumber - Project number (from the URL)
 * @param runner - Command runner (for testing)
 * @returns Items in the same shape as `gh project item-list --format json`
 */
export async function fetchGitHubProjectItems(
  owner: string,
  projectNumber: number,
  runner: CommandRunner = defaultRunner
): Promise<{ items: Record<string, unknown>[] }> {
  const allItems: Record<string, unknown>[] = [];
  let cursor: string | null = null;
  let hasNextPage = true;

  while (hasNextPage) {
    const variables: Record<string, unknown> = {
      owner,
      number: projectNumber,
      first: ITEMS_PER_PAGE,
    };
    if (cursor) {
      variables.after = cursor;
    }

    const cmd: string[] = [
      "gh",
      "api",
      "graphql",
      "-f",
      `query=${QUERY}`,
      "-f",
      `owner=${owner}`,
      "-F",
      `number=${projectNumber}`,
      "-F",
      `first=${ITEMS_PER_PAGE}`,
    ];
    if (cursor) {
      cmd.push("-f", `after=${cursor}`);
    }

    const result: CommandResult = await runner(cmd);

    if (result.exitCode !== 0) {
      throw new Error(`GitHub GraphQL query failed (exit ${result.exitCode}): ${result.stderr}`);
    }

    let response: GitHubProjectItemsPage;
    try {
      response = JSON.parse(result.stdout) as GitHubProjectItemsPage;
    } catch {
      throw new Error(`Failed to parse GraphQL response: ${result.stdout.slice(0, 200)}`);
    }

    if (response.errors?.length) {
      throw new Error(`GraphQL errors: ${response.errors.map((e) => e.message).join(", ")}`);
    }

    const items = response.data?.organization?.projectV2?.items;
    if (!items) {
      throw new Error("Unexpected GraphQL response structure — missing projectV2.items");
    }

    for (const node of items.nodes) {
      const item = nodeToProjectItem(node);
      if (item) {
        allItems.push(item);
      }
    }

    hasNextPage = items.pageInfo.hasNextPage;
    cursor = items.pageInfo.endCursor;
  }

  return { items: allItems };
}
