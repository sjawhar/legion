import type { Actor, ActorAct, AskResolution } from "../../api/types";

/** The person an act is recorded as, with the agent that made it for them beside it:
 *  `<person>, acting through session <id>` when the act names the session a broker-issued token
 *  proved, `<person>, acting through token <name>` when it names only the personal token, and
 *  `person` alone when there is no act, which is every act a person made at this dashboard.
 *
 *  Every surface that shows who acted or who answered renders it through here, so no reader
 *  takes an agent's act for the person's own words. An act that names neither a session nor a
 *  token names no agent to show, so it reads as the person alone. */
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
