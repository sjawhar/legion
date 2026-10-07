import type { Agent, CredentialSession } from "../../api/types";
import { sessionLabel } from "../refs/actor";

/** One id a credential request names, resolved against the live agents list. */
export type CredentialSessionStatus =
  | { kind: "running"; id: string; label: string; dir: string; machineId: string }
  | { kind: "not-running"; id: string }
  // Envoy unavailable or not configured - the agents list itself never answered, so an id that
  // is not in it proves nothing.
  | { kind: "unknown"; id: string };

/** Where a `CredentialSessionLine`'s id came from: the session that enrolled, or the session the
 *  request itself names. The broker verifies the enrollment, not the request's override, which is
 *  an unsigned claim any enrolled process may send; the renderer prefixes only the enrollment line,
 *  and only when both lines show. */
export type CredentialSessionSource = "enrollment" | "request";

/** One line the Inbox row and the record page both render: one of a credential request's ids,
 *  resolved to a `CredentialSessionStatus`, and where it came from. `source` is null only when
 *  both ids agree (or only the enrollment's was ever set); every request-sourced line is
 *  `"request"`, except when it equals a present enrollment id, where the two collapse into one
 *  line. */
export interface CredentialSessionLine {
  source: CredentialSessionSource | null;
  status: CredentialSessionStatus;
}

function resolveStatus(
  id: string,
  agents: readonly Agent[],
  unresolved: boolean
): CredentialSessionStatus {
  const agent = agents.find((candidate) => candidate.session_id === id);
  if (agent !== undefined) {
    return {
      dir: agent.dir,
      id,
      kind: "running",
      label: sessionLabel(id, agent.title),
      machineId: agent.machine_id,
    };
  }
  return { id, kind: unresolved ? "unknown" : "not-running" };
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
 *  configured): every id that isn't a live agent then reads "couldn't check" rather than "not
 *  running", since a quiet agents list proves nothing when the list itself never loaded. */
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
      lines.push({ source: "enrollment", status: resolveStatus(enrollment, agents, unresolved) });
    }
    lines.push({ source: "request", status: resolveStatus(request, agents, unresolved) });
    return lines;
  }
  // Either the enrollment's id alone, or both set and equal: one unlabeled line.
  const id = enrollment ?? request;
  if (id === null) {
    return [];
  }
  return [{ source: null, status: resolveStatus(id, agents, unresolved) }];
}
