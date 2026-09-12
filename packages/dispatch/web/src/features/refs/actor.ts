import type { Actor, AskResolution } from "../../api/types";

/** Renders an actor with a session title when available and its token owner when attributed. */
export function actorLabel(actor: Actor): string {
  const title = actor.kind === "session" ? actor.origin?.session_title?.trim() : undefined;
  const label = title === undefined || title === "" ? actor.id : title;
  return actor.kind === "session" && actor.owner !== undefined
    ? `${label} (for ${actor.owner})`
    : label;
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
