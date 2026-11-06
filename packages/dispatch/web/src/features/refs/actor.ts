import type { Actor, AskResolution } from "../../api/types";

/** Whether an actor is the signed-in viewer: a human whose login is theirs, in any casing, as
 *  the server matches it. */
export function isViewer(actor: Actor, viewerLogin: string | undefined): boolean {
  return (
    actor.kind === "user" &&
    viewerLogin !== undefined &&
    actor.id.toLowerCase() === viewerLogin.toLowerCase()
  );
}

/** The free-text reason a resolution summary shows after its actor, or `undefined` for a
 *  withdrawal, whose server-written reason only repeats what the actor sentence says. */
export function askResolutionReason(resolution: AskResolution): string | undefined {
  return resolution.kind === "withdrawn" ? undefined : resolution.reason;
}

/** The badge naming how an ask was resolved, which is also the verb of its resolution line. */
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
