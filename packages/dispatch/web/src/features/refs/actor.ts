import type { Actor } from "../../api/types";

/** Whether an actor is the signed-in viewer: a human whose login is theirs, in any casing, as
 *  the server matches it. */
export function isViewer(actor: Actor, viewerLogin: string | undefined): boolean {
  return (
    actor.kind === "user" &&
    viewerLogin !== undefined &&
    actor.id.toLowerCase() === viewerLogin.toLowerCase()
  );
}
