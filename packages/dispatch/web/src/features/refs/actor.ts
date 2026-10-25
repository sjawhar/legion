import { actorLabel } from "@legion/contracts";
import type { Actor, AskResolution } from "../../api/types";

// How an actor is named lives in `@legion/contracts` (`src/actor-label.ts`), so the dashboard
// and the agent tools cannot name the same session two ways. Re-exported here because every
// SPA call site already imports it from this module.
export { actorLabel, actorName, sessionLabel, shortSessionId } from "@legion/contracts";

/** Whether an actor is the signed-in viewer: a human whose login is theirs, in any casing, as
 *  the server matches it. */
export function isViewer(actor: Actor, viewerLogin: string | undefined): boolean {
  return (
    actor.kind === "user" &&
    viewerLogin !== undefined &&
    actor.id.toLowerCase() === viewerLogin.toLowerCase()
  );
}

/** The verb-and-actor prefix of a resolution summary, without its free-text reason - callers
 *  that render the reason as Markdown (via `MarkdownBody`) compose this with
 *  `askResolutionReason` and their own markup instead of using `describeAskResolution`'s single
 *  plain-text string. A withdrawal is the close of the ask's issue, so it names the closer. */
export function describeAskResolutionActor(resolution: AskResolution): string {
  const actor = actorLabel(resolution.actor);
  switch (resolution.kind) {
    case "withdrawn":
      return `Withdrawn when ${actor} closed the issue`;
    case "retracted":
      return `Retracted by ${actor}`;
    case "resolved":
      return `Resolved by ${actor}`;
  }
}

/** The free-text reason a resolution summary shows after its actor, or `undefined` for a
 *  withdrawal, whose server-written reason only repeats what the actor sentence says. */
export function askResolutionReason(resolution: AskResolution): string | undefined {
  return resolution.kind === "withdrawn" ? undefined : resolution.reason;
}

/** The badge naming how an ask was resolved. */
export function askResolutionBadge(resolution: AskResolution): string {
  switch (resolution.kind) {
    case "withdrawn":
      return "Withdrawn";
    case "retracted":
      return "Retracted";
    case "resolved":
      return "Resolved";
  }
}

export function describeAskResolution(resolution: AskResolution): string {
  const actor = describeAskResolutionActor(resolution);
  const reason = askResolutionReason(resolution);
  return reason === undefined ? actor : `${actor} - ${reason}`;
}
