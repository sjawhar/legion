import { type Actor, type IssueClaim, serviceSubjectLabel } from "./dispatch-api";

/**
 * The one way an actor is named, shared by every surface that names one: the Dashboard's
 * headers, cards and lists, and the agent tools' own text. It lived only in the SPA, so
 * `dispatch_read` printed the title a session stamped on a write while the dashboard printed
 * the live one from the agent registry, and the same holder read two ways.
 *
 * A caller that can see the registry passes its titles (`session_id` to live title); one that
 * cannot passes nothing and gets the stamped title, which is the same fallback the SPA uses
 * when the registry is unavailable.
 */

/** `session:01234567…` — how an untitled session reads everywhere. */
export function shortSessionId(id: string): string {
  return `session:${id.slice(0, 8)}…`;
}

/** The one session label: the live registry title, else the title stamped on the write, else
 *  `shortSessionId`. */
export function sessionLabel(sessionId: string, title: string | undefined): string {
  const trimmed = title?.trim() ?? "";
  return trimmed === "" ? shortSessionId(sessionId) : trimmed;
}

/** The name half of an actor's label: a user by login; a session by `sessionLabel`, preferring
 *  the live title in `titles` over the `session_title` stamped on the write. */
export function actorName(actor: Actor, titles?: ReadonlyMap<string, string>): string {
  if (actor.kind !== "session") {
    return actor.id;
  }
  const live = titles?.get(actor.id)?.trim();
  return sessionLabel(
    actor.id,
    live === undefined || live === "" ? actor.origin?.session_title : live
  );
}

/** `actorName` followed by ` (for <owner>)` when a personal token attributed the write to a
 *  human, or ` (as <namespace>/<name>)` — the verified token's subject through
 *  `serviceSubjectLabel`, `system:serviceaccount:legion:legion-worker` reading as
 *  `legion/legion-worker` — when a service token did. The namespace stays because every
 *  namespace has a `default` service account. */
export function actorLabel(actor: Actor, titles?: ReadonlyMap<string, string>): string {
  const name = actorName(actor, titles);
  if (actor.kind !== "session") {
    return name;
  }
  if (actor.owner !== undefined) {
    return `${name} (for ${actor.owner})`;
  }
  const service = actor.service;
  if (service === undefined) {
    return name;
  }
  return `${name} (as ${serviceSubjectLabel(service)})`;
}

/**
 * Whether a claim still holds its issue, judged against the live agent registry: `titles` by
 * session id once it has loaded, or `undefined` while it has not or when it could not be read. A
 * person's claim always holds: there is no session to end, and only a person releases or forces
 * it. A session's claim holds while the registry lists the session, and has lapsed (`false`) once a
 * loaded registry does not, which is exactly when the server lets any agent take the issue. With no
 * registry nothing can say (`undefined`); a surface that read that as lapsed would mark every claim
 * free whenever the listener is unreachable. The dashboard's claim chip and the agent tools both
 * judge a claim here.
 */
export function claimHolds(
  claim: IssueClaim,
  titles: ReadonlyMap<string, string> | undefined
): boolean | undefined {
  if (claim.actor.kind !== "session") {
    return true;
  }
  if (titles === undefined) {
    return undefined;
  }
  return titles.has(claim.actor.id);
}
