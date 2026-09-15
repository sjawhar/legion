import type { Actor } from "../../api/types";
import { actorLabel } from "../refs/actor";

export interface Author {
  label: string;
  initials: string;
  shape: "round" | "square";
}

/** The Conversation's turn author: the app-wide `actorLabel`, plus the avatar's initials (from
 *  the name alone, never the `(for <owner>)` attribution) and shape (square for sessions). */
export function resolveAuthor(actor: Actor, titles: ReadonlyMap<string, string>): Author {
  const label = actorLabel(actor, titles);
  const name =
    actor.kind === "session" && actor.owner !== undefined
      ? actorLabel({ ...actor, owner: undefined }, titles)
      : label;
  const initials = name
    .split(/\s+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((part) => part[0]?.toUpperCase())
    .join("");

  return { initials, label, shape: actor.kind === "session" ? "square" : "round" };
}
