import type { Actor, AskResolution } from "../../api/types";

/** `session:01234567…` — how an untitled session reads everywhere in the SPA. */
export function shortSessionId(id: string): string {
  return `session:${id.slice(0, 8)}…`;
}

/** The one session label: `title` when it has text (the agent registry's live title where a
 *  surface has one, else the title stamped on the write), else `shortSessionId`. The Agents
 *  page, ask cards, an ask's `Reaches` list, Subscribed agents, and the Conversation all read
 *  the same session the same way. */
export function sessionLabel(sessionId: string, title: string | undefined): string {
  const trimmed = title?.trim() ?? "";
  return trimmed === "" ? shortSessionId(sessionId) : trimmed;
}

/** The name half of an actor's label: a user by login; a session by `sessionLabel` — the live
 *  title from `titles` (the agent registry) before the stamped `session_title`. */
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
 *  human. */
export function actorLabel(actor: Actor, titles?: ReadonlyMap<string, string>): string {
  const name = actorName(actor, titles);
  return actor.kind === "session" && actor.owner !== undefined
    ? `${name} (for ${actor.owner})`
    : name;
}

/** The verb-and-actor prefix of a resolution summary, without its free-text reason - callers
 *  that render the reason as Markdown (via `MarkdownBody`) compose this with their own markup
 *  instead of using `describeAskResolution`'s single plain-text string. */
export function describeAskResolutionActor(resolution: AskResolution): string {
  const verb = resolution.kind === "retracted" ? "Retracted" : "Resolved";
  return `${verb} by ${actorLabel(resolution.actor)}`;
}

export function describeAskResolution(resolution: AskResolution): string {
  return `${describeAskResolutionActor(resolution)} - ${resolution.reason}`;
}
