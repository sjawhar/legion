import type { Actor } from "../../api/types";
import { agentInitials } from "../refs/AgentName";
import { actorLabel } from "../refs/actor";

export interface Author {
  label: string;
  initials: string;
  shape: "round" | "square";
}

/** The Conversation's turn author: the app-wide `actorLabel`, plus the avatar's initials
 *  (`agentInitials`: the name alone, never the `(for <owner>)` attribution) and shape (square
 *  for sessions). */
export function resolveAuthor(actor: Actor, titles: ReadonlyMap<string, string>): Author {
  return {
    initials: agentInitials(actor, titles),
    label: actorLabel(actor, titles),
    shape: actor.kind === "session" ? "square" : "round",
  };
}
