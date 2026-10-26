import type { Actor } from "../../api/types";

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
