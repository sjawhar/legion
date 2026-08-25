/**
 * Type definitions shared by retained issue-tracker adapters.
 */

// =============================================================================
// Status Constants and Normalization
// =============================================================================

/**
 * Canonical issue status values.
 */
export type IssueStatusLiteral =
	| "Triage"
	| "Icebox"
	| "Backlog"
	| "Todo"
	| "In Progress"
	| "Testing"
	| "Needs Review"
	| "Retro"
	| "Done";

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
 * Canonical issue status values with normalization.
 */
export const IssueStatus = {
	TRIAGE: "Triage" as IssueStatusLiteral,
	ICEBOX: "Icebox" as IssueStatusLiteral,
	BACKLOG: "Backlog" as IssueStatusLiteral,
	TODO: "Todo" as IssueStatusLiteral,
	IN_PROGRESS: "In Progress" as IssueStatusLiteral,
	TESTING: "Testing" as IssueStatusLiteral,
	NEEDS_REVIEW: "Needs Review" as IssueStatusLiteral,
	RETRO: "Retro" as IssueStatusLiteral,
	DONE: "Done" as IssueStatusLiteral,

	/**
	 * Map status name aliases to canonical names.
	 * Case-insensitive lookup is handled by normalize() — keys here
	 * should be in their most common casing for readability.
	 */
	ALIASES: {
		"In Review": "Needs Review" as IssueStatusLiteral,
		Today: "Todo" as IssueStatusLiteral,
	} as Record<string, IssueStatusLiteral>,

	/**
	 * Normalize a raw status string to canonical form.
	 *
	 * Matching is case-insensitive: "in progress", "In progress",
	 * and "IN PROGRESS" all resolve to "In Progress".
	 *
	 * Resolution order:
	 *   1. Exact match against canonical status names
	 *   2. Case-insensitive match against canonical status names
	 *   3. Case-insensitive match against ALIASES
	 *   4. Return raw value unchanged
	 *
	 * Returns empty string if raw is null.
	 */
	normalize(raw: string | null): IssueStatusLiteral | string {
		if (raw === null) {
			return "";
		}

		// Fast path: exact alias match
		const aliasHit = IssueStatus.ALIASES[raw];
		if (aliasHit) {
			return aliasHit;
		}

		// Case-insensitive lookup against canonical names + aliases
		const lower = raw.toLowerCase();
		const canonical = _lowercaseCanonicalMap.get(lower);
		if (canonical) {
			return canonical;
		}
		const aliasCanonical = _lowercaseAliasMap.get(lower);
		if (aliasCanonical) {
			return aliasCanonical;
		}

		return raw;
	},
} as const;

// Pre-built lowercase lookup maps (populated after IssueStatus is defined)
const _lowercaseCanonicalMap = new Map<string, IssueStatusLiteral>([
	["triage", "Triage"],
	["icebox", "Icebox"],
	["backlog", "Backlog"],
	["todo", "Todo"],
	["in progress", "In Progress"],
	["testing", "Testing"],
	["needs review", "Needs Review"],
	["retro", "Retro"],
	["done", "Done"],
]);

const _lowercaseAliasMap = new Map<string, IssueStatusLiteral>(
	Object.entries(IssueStatus.ALIASES).map(([k, v]) => [k.toLowerCase(), v]),
);

export type { LinearIssueRaw } from "./backends/linear";

// =============================================================================
// Internal Data Structures
// =============================================================================

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
		const match = url.match(
			/^https:\/\/github\.com\/([\w.-]+)\/([\w.-]+)\/pull\/(\d+)$/,
		);
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

/**
 * Structured source reference for an issue.
 * Preserves the full identity so API calls can target the exact issue.
 */
export interface IssueSource {
	owner: string;
	repo: string;
	number: number;
	url: string;
}

/**
 * Parsed issue data from issue tracker API response.
 */
export interface ParsedIssue {
	issueId: string;
	status: IssueStatusLiteral | string; // Canonical status or unknown raw value
	labels: string[];
	prRef: GitHubPRRef | null;
	source: IssueSource | null; // Structured metadata for GitHub issues, null for Linear
	blockedByIds: string[];
	isBlocked: boolean;
}

/**
 * Create a ParsedIssue with computed properties.
 */
export function createParsedIssue(
	issueId: string,
	status: IssueStatusLiteral | string,
	labels: string[],
	prRef: GitHubPRRef | null,
	source: IssueSource | null = null,
	blockedByIds: string[] = [],
): ParsedIssue {
	return {
		issueId,
		status,
		labels,
		prRef,
		source,
		blockedByIds,
		isBlocked: blockedByIds.length > 0,
	};
}
