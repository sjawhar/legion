import type { ParsedIssue } from "../types";

/**
 * Pluggable issue tracker backend interface.
 *
 * Implementations parse raw issue data from their respective APIs
 * into the normalized ParsedIssue format used by the state machine.
 *
 * Expected raw input shapes:
 * - **Linear:** Array of issue objects from Linear API (GraphQL nodes).
 * - **GitHub:** Array of project items from `gh project item-list --format json`,
 *   or an `{ items: [...] }` envelope wrapping the same array.
 */
export interface IssueTracker {
  /**
   * Parse raw issue data from the tracker into normalized form.
   * Silently skips malformed entries rather than throwing.
   */
  parseIssues(raw: unknown): ParsedIssue[];
}

export type BackendName = "linear" | "github";
