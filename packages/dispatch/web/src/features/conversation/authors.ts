import { cutUnits } from "@legion/contracts";

import type { Actor } from "../../api/types";
import { actorLabel, actorName } from "../refs/actor";

export interface Author {
  label: string;
  initials: string;
  shape: "round" | "square";
}

/** The Conversation's turn author: the app-wide `actorLabel`, plus the avatar's initials (from
 *  `actorName` alone, never the `(for <owner>)` attribution; a word's first unit, none for a word
 *  opening on an emoji: `cutUnits`) and shape (square for sessions). */
export function resolveAuthor(actor: Actor, titles: ReadonlyMap<string, string>): Author {
  const label = actorLabel(actor, titles);
  const name = actorName(actor, titles);
  const initials = name
    .split(/\s+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((part) => cutUnits(part, 1).toUpperCase())
    .join("");

  return { initials, label, shape: actor.kind === "session" ? "square" : "round" };
}
