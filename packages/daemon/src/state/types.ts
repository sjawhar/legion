/**
 * Type definitions for Legion state collection.
 *
 * Contains:
 * - Interfaces for internal data structures
 * - Type aliases for external API response shapes
 * - Constants and functions for status normalization
 * - Session ID computation utilities
 *
 * Ported from Python: src/legion/state/types.py
 */

import { v5 as uuidv5 } from "uuid";

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
 * Worker mode values for session ID computation.
 */
export type WorkerModeLiteral = "architect" | "plan" | "implement" | "test" | "review" | "merge";

/**
 * Action types for state machine transitions.
 */
export type ActionType =
  | "skip"
  | "investigate_no_pr"
  | "dispatch_architect"
  | "dispatch_planner"
  | "dispatch_implementer"
  | "dispatch_implementer_for_retro"
  | "dispatch_tester"
  | "transition_to_testing"
  | "resume_implementer_for_test_failure"
  | "dispatch_reviewer"
  | "dispatch_merger"
  | "resume_implementer_for_changes"
  | "resume_implementer_for_retro"
  | "transition_to_in_progress"
  | "transition_to_needs_review"
  | "transition_to_retro"
  | "transition_to_todo"
  | "relay_user_feedback"
  | "remove_worker_active_and_redispatch"
  | "add_needs_approval"
  | "retry_pr_check"
  | "resume_implementer_for_ci_failure"
  | "retry_ci_check";

/**
 * CI check status for a PR.
 * - "passing": all checks succeeded
 * - "failing": one or more checks failed
 * - "pending": checks still running
 * - null: no PR, no checks configured, or couldn't determine
 */
export type CiStatusLiteral = "passing" | "failing" | "pending";

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
  Object.entries(IssueStatus.ALIASES).map(([k, v]) => [k.toLowerCase(), v])
);

/**
 * Worker mode constants for session ID computation.
 */
export const WorkerMode = {
  ARCHITECT: "architect" as WorkerModeLiteral,
  PLAN: "plan" as WorkerModeLiteral,
  IMPLEMENT: "implement" as WorkerModeLiteral,
  TEST: "test" as WorkerModeLiteral,
  REVIEW: "review" as WorkerModeLiteral,
  MERGE: "merge" as WorkerModeLiteral,
} as const;

export interface GitHubLabel {
  name: string;
}

export interface GitHubPR {
  labels: GitHubLabel[] | null;
}

export type {
  LinearAttachment,
  LinearIssue,
  LinearIssueRaw,
  LinearLabelNode,
  LinearLabelsContainer,
  LinearStateDict,
} from "./backends/linear";

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

  // Computed properties (implemented as getters)
  readonly hasWorkerDone: boolean;
  readonly hasUserFeedback: boolean;
  readonly hasUserInputNeeded: boolean;
  readonly hasWorkerActive: boolean;
  readonly hasNeedsApproval: boolean;
  readonly hasHumanApproved: boolean;
  readonly hasTestPassed: boolean;
  readonly hasTestFailed: boolean;
  readonly hasPr: boolean;
  readonly needsPrStatus: boolean;
  readonly needsCiStatus: boolean;
}

/**
 * Create a ParsedIssue with computed properties.
 */
export function createParsedIssue(
  issueId: string,
  status: IssueStatusLiteral | string,
  labels: string[],
  prRef: GitHubPRRef | null,
  source: IssueSource | null = null
): ParsedIssue {
  return {
    issueId,
    status,
    labels,
    prRef,
    source,

    get hasWorkerDone() {
      return this.labels.includes("worker-done");
    },

    get hasUserFeedback() {
      return this.labels.includes("user-feedback-given");
    },

    get hasUserInputNeeded() {
      return this.labels.includes("user-input-needed");
    },

    get hasWorkerActive() {
      return this.labels.includes("worker-active");
    },

    get hasNeedsApproval() {
      return this.labels.includes("needs-approval");
    },

    get hasHumanApproved() {
      return this.labels.includes("human-approved");
    },

    get hasTestPassed() {
      return this.labels.includes("test-passed");
    },

    get hasTestFailed() {
      return this.labels.includes("test-failed");
    },

    get hasPr() {
      return this.prRef !== null;
    },

    get needsPrStatus() {
      return (
        this.status === IssueStatus.NEEDS_REVIEW &&
        this.labels.includes("worker-done") &&
        this.prRef !== null
      );
    },

    get needsCiStatus() {
      return (
        this.status === IssueStatus.NEEDS_REVIEW &&
        this.prRef !== null
      );
    },
  };
}

/**
 * Complete fetched data for an issue.
 */
export interface FetchedIssueData {
  issueId: string;
  status: IssueStatusLiteral | string; // Canonical status or unknown raw value
  labels: string[];
  hasPr: boolean; // True if issue has a linked PR
  prIsDraft: boolean | null; // null if no PR or couldn't check status
  ciStatus: CiStatusLiteral | null; // null if no PR, no checks, or couldn't check
  hasLiveWorker: boolean;
  workerMode: string | null;
  workerStatus: string | null;
  hasUserFeedback: boolean;
  hasUserInputNeeded: boolean;
  hasNeedsApproval: boolean;
  hasHumanApproved: boolean;
  hasTestPassed: boolean;
  hasTestFailed: boolean;
  source: IssueSource | null; // Canonical identity for GitHub issues, null for Linear
}

/**
 * Serialized form of IssueState.
 */
export interface IssueStateDict {
  status: IssueStatusLiteral | string;
  labels: string[];
  hasPr: boolean;
  prIsDraft: boolean | null;
  ciStatus: CiStatusLiteral | null;
  hasLiveWorker: boolean;
  workerMode: string | null;
  workerStatus: string | null;
  suggestedAction: ActionType;
  sessionId: string;
  hasUserFeedback: boolean;
  source: IssueSource | null;
}

/**
 * Serialized form of CollectedState.
 */
export interface CollectedStateDict {
  issues: Record<string, IssueStateDict>;
}

/**
 * Final state for an issue with suggested action.
 */
export interface IssueState {
  status: IssueStatusLiteral | string; // Canonical status or unknown raw value
  labels: string[];
  hasPr: boolean; // Whether issue has a linked PR
  prIsDraft: boolean | null; // null if couldn't check status, true if draft, false if ready
  ciStatus: CiStatusLiteral | null;
  hasLiveWorker: boolean;
  workerMode: string | null;
  workerStatus: string | null;
  suggestedAction: ActionType;
  sessionId: string;
  hasUserFeedback: boolean;
  source: IssueSource | null; // Canonical identity for GitHub issues, null for Linear
}

export const IssueState = {
  /**
   * Convert to dictionary for JSON serialization.
   */
  toDict(state: IssueState): IssueStateDict {
    const dict: IssueStateDict = {
      status: state.status,
      labels: state.labels,
      hasPr: state.hasPr,
      prIsDraft: state.prIsDraft,
      ciStatus: state.ciStatus,
      hasLiveWorker: state.hasLiveWorker,
      workerMode: state.workerMode,
      workerStatus: state.workerStatus,
      suggestedAction: state.suggestedAction,
      sessionId: state.sessionId,
      hasUserFeedback: state.hasUserFeedback,
      source: state.source,
    };
    return dict;
  },
};

/**
 * Complete state collection result.
 */
export interface CollectedState {
  issues: Record<string, IssueState>;
}

export const CollectedState = {
  /**
   * Convert to dictionary for JSON serialization.
   */
  toDict(state: CollectedState): CollectedStateDict {
    return {
      issues: Object.fromEntries(
        Object.entries(state.issues).map(([k, v]) => [k, IssueState.toDict(v)])
      ),
    };
  },
};

// =============================================================================
// Utility Functions
// =============================================================================

const BASE62_CHARS = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz";

/**
 * Fixed namespace UUID for deriving team ID namespaces.
 * Used to convert arbitrary team ID strings into deterministic UUID namespaces.
 */
const LEGION_NAMESPACE = "a1b2c3d4-e5f6-7890-abcd-ef1234567890";
const LEGION_NAMESPACE_UUID = uuidv5(LEGION_NAMESPACE, "6ba7b810-9dad-11d1-80b4-00c04fd430c8");

/**
 * Convert any team ID string into a UUID namespace.
 */
function teamIdToNamespace(teamId: string): string {
  return uuidv5(teamId, LEGION_NAMESPACE_UUID);
}

/**
 * Convert UUID to OpenCode session ID format: ses_ + 12 hex + 14 Base62.
 *
 * Uses the 16 bytes of the UUID deterministically:
 * - First 6 bytes → 12 lowercase hex chars
 * - Remaining 10 bytes → 14 Base62 chars (big-endian encoding)
 */
function uuidToSessionId(uuid: string): string {
  const hex = uuid.replace(/-/g, "");
  const hexPart = hex.slice(0, 12);

  let value = 0n;
  for (let i = 12; i < hex.length; i += 2) {
    value = (value << 8n) | BigInt(parseInt(hex.slice(i, i + 2), 16));
  }

  const base62Chars: string[] = [];
  for (let i = 0; i < 14; i++) {
    base62Chars.unshift(BASE62_CHARS[Number(value % 62n)]);
    value = value / 62n;
  }

  return `ses_${hexPart}${base62Chars.join("")}`;
}

/**
 * Compute deterministic session ID for a worker.
 *
 * Session IDs match OpenCode's format: ses_ + 12 hex + 14 Base62.
 * Pattern: ^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$
 *
 * @param teamId - Team identifier (UUID or arbitrary string)
 * @param issueId - Issue identifier (e.g., "ENG-21")
 * @param mode - Worker mode (e.g., "implement", "review")
 * @returns Session ID string matching OpenCode format
 */
export function computeSessionId(teamId: string, issueId: string, mode: WorkerModeLiteral): string {
  const namespace = teamIdToNamespace(teamId);
  const uuid = uuidv5(`${issueId.toLowerCase()}:${mode}`, namespace);
  return uuidToSessionId(uuid);
}

/**
 * Compute deterministic session ID for controller.
 *
 * Session IDs match OpenCode's format: ses_ + 12 hex + 14 Base62.
 *
 * @param teamId - Team identifier (UUID or arbitrary string)
 * @returns Session ID string matching OpenCode format
 */
export function computeControllerSessionId(teamId: string): string {
  const namespace = teamIdToNamespace(teamId);
  const uuid = uuidv5("controller", namespace);
  return uuidToSessionId(uuid);
}
