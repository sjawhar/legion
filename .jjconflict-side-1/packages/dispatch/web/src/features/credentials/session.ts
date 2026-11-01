import type { Agent, CredentialSession } from "../../api/types";

/** Where a running session runs, as the live agents list lists it. */
export interface CredentialSessionPlace {
  dir: string;
  machineId: string;
}

/** Where a `CredentialSessionLine`'s id came from: the session that enrolled, or the session the
 *  request itself names. The broker verifies the enrollment, not the request's override, which is
 *  an unsigned claim any enrolled process may send; the renderer prefixes only the enrollment line,
 *  and only when both lines show. */
export type CredentialSessionSource = "enrollment" | "request";

/** Whether a line's session runs, by the same rule as `AgentName`'s state mark: `running` (with
 *  where) while the agents list answered and lists it; `not-running` when it answered without
 *  it; `unknown` while it hasn't answered or its latest read failed, which proves nothing. */
export type CredentialSessionStatus =
  | { kind: "running"; place: CredentialSessionPlace }
  | { kind: "not-running" }
  | { kind: "unknown" };

/** One line the Inbox row and the record page both render: one of a credential request's ids,
 *  where it came from, and whether its session runs. The renderer names the id through
 *  `AgentName` and says the status in words. `source` is null only when both ids agree (or only
 *  the enrollment's was ever set); every request-sourced line is `"request"`, except when it
 *  equals a present enrollment id, where the two collapse into one line. */
export interface CredentialSessionLine {
  id: string;
  status: CredentialSessionStatus;
  source: CredentialSessionSource | null;
}

/** One id's line, its status read from the one agents list the caller passes. */
function resolveLine(
  id: string,
  source: CredentialSessionSource | null,
  agents: readonly Agent[],
  unresolved: boolean
): CredentialSessionLine {
  if (unresolved) {
    return { id, source, status: { kind: "unknown" } };
  }
  const agent = agents.find((candidate) => candidate.session_id === id);
  return {
    id,
    source,
    status:
      agent === undefined
        ? { kind: "not-running" }
        : { kind: "running", place: { dir: agent.dir, machineId: agent.host } },
  };
}

/** Whether `session` names any id at all. An older broker's response, with no `session` field,
 *  and a machine login's, with both null, both answer false - the caller can then skip asking the
 *  agents list at all. */
export function credentialSessionNamesAnyone(session: CredentialSession | undefined): boolean {
  return (session?.enrollment ?? null) !== null || (session?.request ?? null) !== null;
}

/** Resolves a credential request's `session` field into the lines to render: empty when neither
 *  id was ever set (including an older broker's response, which carries no `session` field at
 *  all) - the caller then shows "No session named." One line, source null, when the enrollment's
 *  id is the only one set or the two agree. One line, source `"request"`, when only the request
 *  names an id. Two lines, one per source, when both are set and differ; the renderer prefixes
 *  the enrollment's so a reader can tell which one the broker verified. `unresolved` is true
 *  while the agents list hasn't answered yet or answered with an error (Envoy unavailable or not
 *  configured): every line is then `unknown`, since a quiet agents list proves nothing when the
 *  list itself never loaded. */
export function credentialSessionLines(
  session: CredentialSession | undefined,
  agents: readonly Agent[],
  unresolved: boolean
): readonly CredentialSessionLine[] {
  const enrollment = session?.enrollment ?? null;
  const request = session?.request ?? null;
  if (enrollment === null && request === null) {
    return [];
  }
  if (request !== null && request !== enrollment) {
    const lines: CredentialSessionLine[] = [];
    if (enrollment !== null) {
      lines.push(resolveLine(enrollment, "enrollment", agents, unresolved));
    }
    lines.push(resolveLine(request, "request", agents, unresolved));
    return lines;
  }
  // Either the enrollment's id alone, or both set and equal: one unlabeled line.
  const id = enrollment ?? request;
  if (id === null) {
    return [];
  }
  return [resolveLine(id, null, agents, unresolved)];
}
