import { type Actor, type ActorAct, serviceSubjectLabel } from "@legion/contracts";

/** The person an act is recorded as, with the agent that made it for them beside it:
 *  `<person>, acting through session <id>` when the act names the session a broker-issued token
 *  proved, `<person>, acting through token <name>` when it names only the personal token, and
 *  `person` alone when there is no act, which is every act a person made at a browser.
 *
 *  Every reader of an act in this package renders it through here, so no reader takes an agent's
 *  act for the person's own words. An act that names neither a session nor a token names no
 *  agent to show, so it reads as the person alone. */
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

/** How an event, comment, message or ask author reads in the tools: `<kind> <id>`, plus
 *  ` (as <namespace>/<name>)` — the verified service token's subject through
 *  `serviceSubjectLabel`, which keeps the namespace because every namespace has a `default`
 *  service account — when a service token authenticated the write, or `actorActText`'s act when
 *  an agent made the write for the person it acts for. A claim's holder is named by `claimText`
 *  instead, which follows the dashboard's session label. */
export function actorText(actor: Actor): string {
  if (actor.kind === "user") {
    return actorActText(`user ${actor.id}`, actor.act);
  }
  const service = actor.kind === "session" ? actor.service : undefined;
  if (service === undefined) {
    return `${actor.kind} ${actor.id}`;
  }
  return `${actor.kind} ${actor.id} (as ${serviceSubjectLabel(service)})`;
}
