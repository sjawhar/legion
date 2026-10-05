import type { Agent, CredentialSession } from "../../api/types";
import { sessionLabel } from "../refs/actor";

/** One id a credential request names, resolved against the live agents list. */
export type CredentialSessionStatus =
  | { kind: "running"; id: string; label: string; dir: string; machineId: string }
  | { kind: "not-running"; id: string }
  // Envoy unavailable or not configured - the agents list itself never answered, so an id that
  // is not in it proves nothing.
  | { kind: "unknown"; id: string };

/** One line the Inbox row and the record page both render: one of a credential request's ids,
 *  resolved to a `CredentialSessionStatus`, and where it came from - `null` when there is only
 *  one id to show (the other was never set, or both agree), so nothing needs attributing. */
export interface CredentialSessionLine {
  source: "enrollment" | "request" | null;
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
 *  all) - the caller then shows "No session named." One line when both ids agree or only one was
 *  set - unambiguous, so it names no source. Two - each naming where its id came from - when both
 *  are set and differ. `unresolved` is true while the agents list hasn't answered yet or answered
 *  with an error (Envoy unavailable or not configured): every id that isn't a live agent then
 *  reads "couldn't check" rather than "not running", since a quiet agents list proves nothing
 *  when the list itself never loaded. */
export function credentialSessionLines(
  session: CredentialSession | undefined,
  agents: readonly Agent[],
  unresolved: boolean
): readonly CredentialSessionLine[] {
  const enrollment = session?.enrollment ?? null;
  const request = session?.request ?? null;
  const ids: Array<{ source: "enrollment" | "request"; id: string }> = [];
  if (enrollment !== null) ids.push({ id: enrollment, source: "enrollment" });
  if (request !== null && request !== enrollment) ids.push({ id: request, source: "request" });
  const named = ids.length === 2;
  return ids.map(({ source, id }) => ({
    source: named ? source : null,
    status: resolveStatus(id, agents, unresolved),
  }));
}
