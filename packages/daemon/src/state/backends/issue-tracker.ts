import type { ParsedIssue } from "../types";

/**
 * Pluggable issue tracker backend interface.
 *
 * Implementations parse raw issue data from their respective APIs
 * into the normalized ParsedIssue format used by the state machine.
 */
export interface IssueTracker {
  /** Parse raw issue data from the tracker into normalized form. */
  parseIssues(raw: unknown): ParsedIssue[];

  /** Resolve a team/project reference to a stable internal ID. */
  resolveTeamId(ref: string): Promise<string>;
}

export type BackendName = "linear" | "github";
