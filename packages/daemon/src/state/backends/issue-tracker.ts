import type { IssueStatusLiteral, ParsedIssue } from "../types";

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

  /**
   * Transition an issue to a new status in the tracker.
   * Implementations use tracker-native APIs (GitHub Projects V2 GraphQL, Linear API).
   * Optional — not all backends support mutations (e.g., Linear mutations use MCP).
   * @throws if the transition fails or is not supported.
   */
  transitionIssue?(issueId: string, newStatus: IssueStatusLiteral): Promise<void>;

  /**
   * Remove a label from an issue.
   * Optional — not all backends support mutations.
   * @throws if the removal fails or is not supported.
   */
  removeLabel?(issueId: string, label: string): Promise<void>;
}

export type BackendName = "linear" | "github";
