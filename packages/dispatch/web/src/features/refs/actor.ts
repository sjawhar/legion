import { actorLabel } from "@legion/contracts";
import type { AskResolution } from "../../api/types";

// How an actor is named lives in `@legion/contracts` (`src/actor-label.ts`), so the dashboard
// and the agent tools cannot name the same session two ways. Re-exported here because every
// SPA call site already imports it from this module.
export { actorLabel, actorName, sessionLabel, shortSessionId } from "@legion/contracts";

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
