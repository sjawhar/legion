/**
 * Shared GitHub state types used by PR and CI status fetching.
 */

/**
 * CI check status for a PR.
 * - "passing": all checks succeeded
 * - "failing": one or more checks failed
 * - "pending": checks still running
 * - null: no PR, no checks configured, or couldn't determine
 */
export type CiStatusLiteral = "passing" | "failing" | "pending";

export const CiStatus = {
  PASSING: "passing" as CiStatusLiteral,
  FAILING: "failing" as CiStatusLiteral,
  PENDING: "pending" as CiStatusLiteral,
} as const;

/**
 * PR merge conflict status.
 * - "mergeable": no conflicts, can be merged
 * - "conflicting": has merge conflicts, needs rebase
 * - "unknown": GitHub hasn't computed yet (lazy evaluation)
 * - null: no PR, couldn't check, or not applicable
 */
export type MergeableStatusLiteral = "mergeable" | "conflicting" | "unknown";

export const MergeableStatus = {
  MERGEABLE: "mergeable" as MergeableStatusLiteral,
  CONFLICTING: "conflicting" as MergeableStatusLiteral,
  UNKNOWN: "unknown" as MergeableStatusLiteral,
} as const;

/**
 * PR review state from GitHub's native review API.
 * - "approved": at least one approving review, no outstanding change requests
 * - "changes_requested": most recent review requests changes
 * - null: no reviews yet, no PR, or couldn't check
 */
export type ReviewStateLiteral = "approved" | "changes_requested";

export const ReviewState = {
  APPROVED: "approved" as ReviewStateLiteral,
  CHANGES_REQUESTED: "changes_requested" as ReviewStateLiteral,
} as const;

/**
 * Parsed GitHub PR reference from URL (immutable value object).
 */
export interface GitHubPRRef {
  owner: string;
  repo: string;
  number: number;
}

export const GitHubPRRef = {
  /**
   * Parse a GitHub PR URL into a reference.
   *
   * @param url - GitHub PR URL like https://github.com/owner/repo/pull/123
   * @returns GitHubPRRef or null if URL doesn't match expected format
   */
  fromUrl(url: string): GitHubPRRef | null {
    // Validate URL format and owner/repo characters (alphanumeric, hyphen, underscore, dot)
    const match = url.match(/^https:\/\/github\.com\/([\w.-]+)\/([\w.-]+)\/pull\/(\d+)$/);
    if (!match) {
      return null;
    }

    const prNumber = parseInt(match[3], 10);
    // Guard against unreasonably large PR numbers (GraphQL uses 32-bit int)
    if (prNumber > 2_147_483_647) {
      return null;
    }

    return {
      owner: match[1],
      repo: match[2],
      number: prNumber,
    };
  },
};
