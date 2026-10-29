import { type Actor, serviceSubjectLabel } from "@legion/contracts";

/** How an event, comment, message or ask author reads in the tools: `<kind> <id>`, plus
 *  ` (as <namespace>/<name>)` — the verified service token's subject through
 *  `serviceSubjectLabel`, which keeps the namespace because every namespace has a `default`
 *  service account — when a service token authenticated the write. A claim's holder is named by
 *  `claimText` instead, which follows the dashboard's session label. */
export function actorText(actor: Actor): string {
  const service = actor.kind === "session" ? actor.service : undefined;
  if (service === undefined) {
    return `${actor.kind} ${actor.id}`;
  }
  return `${actor.kind} ${actor.id} (as ${serviceSubjectLabel(service)})`;
}
