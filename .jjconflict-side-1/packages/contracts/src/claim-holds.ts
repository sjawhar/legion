import type { IssueClaim } from "./dispatch-api";

/**
 * Whether an issue's claim still holds it, judged against the live agent registry. The dashboard's
 * claim chip and the agent tools' claim lines both judge a claim here, so a person and an agent
 * reading the same issue see the same answer.
 *
 * - `holds`: a person's claim, which never lapses (there is no session to end, and only a person
 *   releases or forces it), or a session's the loaded registry lists.
 * - `lapsed`: a session's claim the loaded registry does not list, which is exactly when the server
 *   lets any agent take the issue.
 * - `unknown`: a session's claim with no registry to judge it against (`titles` undefined: not yet
 *   loaded, or it could not be read). Treating that as lapsed would mark every claim free whenever
 *   the listener is unreachable.
 */
export type ClaimHolding = "holds" | "lapsed" | "unknown";

/** `titles` is the live registry's session titles by session id, or undefined without one. */
export function claimHolds(
  claim: IssueClaim,
  titles: ReadonlyMap<string, string> | undefined
): ClaimHolding {
  if (claim.actor.kind !== "session") {
    return "holds";
  }
  if (titles === undefined) {
    return "unknown";
  }
  return titles.has(claim.actor.id) ? "holds" : "lapsed";
}
