import type { Actor, AskResolution } from "../../api/types";

/** Renders a session actor by the human-readable title the harness recorded, falling back to
 *  the raw actor id when no title is present or for user actors (whose id is their login). */
export function actorLabel(actor: Actor): string {
  if (actor.kind === "session") {
    const title = actor.origin?.session_title?.trim();
    if (title !== undefined && title !== "") {
      return title;
    }
  }
  return actor.id;
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
