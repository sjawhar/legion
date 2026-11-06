import { type Actor, type ActorAct, serviceSubjectLabel } from "./dispatch-api";

/**
 * The one way an actor is named, shared by every surface that names one: the Dashboard's
 * headers, cards and lists, and the agent tools' own text. Kept in the SPA alone, it would let
 * `dispatch_read` print the title a session stamped on a write while the dashboard printed the
 * live one from the agent registry, and the same holder would read two ways.
 *
 * A caller that can see the registry passes its titles (`session_id` to live title); one that
 * cannot passes nothing and gets the stamped title, which is the same fallback the SPA uses
 * when the registry is unavailable.
 */

/** The person an act is recorded as, with the agent that made it for them beside it:
 *  `<person>, acting through session <id>` when the act names the session a broker-issued token
 *  proved, `<person>, acting through token <name>` when it names only the personal token, and
 *  `person` alone when there is no act, which is every act a person made at a browser.
 *
 *  Every surface that shows who acted or who answered renders it through here, the dashboard's
 *  and the agent tools' alike, so no reader takes an agent's act for the person's own words. An
 *  act that names neither a session nor a token names no agent to show, so it reads as the
 *  person alone. */
export function actorActText(person: string, act: ActorAct | undefined): string {
  if (act === undefined) {
    return person;
  }
  if (act.session !== undefined && act.session !== "") {
    return `${person}, acting through session ${act.session}`;
  }
  if (act.token_name !== undefined && act.token_name !== "") {
    return `${person}, acting through token ${act.token_name}`;
  }
  return person;
}

/** `session:01234567…` — how an untitled session reads everywhere. The Legion daemon's write
 *  identity, `legion-daemon:<PROJECT>`, reads whole: its first 8 characters name nobody. */
export function shortSessionId(id: string): string {
  return id.startsWith("legion-daemon:") ? id : `session:${id.slice(0, 8)}…`;
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

/** What follows an actor's name in its label: ` (for <owner>)` when a personal token attributed
 *  the write to a human, or ` (as <namespace>/<name>)` — the verified token's subject through
 *  `serviceSubjectLabel`, `system:serviceaccount:legion:legion-worker` reading as
 *  `legion/legion-worker` — when a service token did, else `""` (always for a user or system).
 *  The namespace stays because every namespace has a `default` service account. */
export function actorAttribution(actor: Actor): string {
  if (actor.kind !== "session") {
    return "";
  }
  if (actor.owner !== undefined) {
    return ` (for ${actor.owner})`;
  }
  return actor.service === undefined ? "" : ` (as ${serviceSubjectLabel(actor.service)})`;
}

/** An actor's whole label: `actorName` followed by `actorAttribution`. */
export function actorLabel(actor: Actor, titles?: ReadonlyMap<string, string>): string {
  return `${actorName(actor, titles)}${actorAttribution(actor)}`;
}
