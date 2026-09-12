import type { Actor } from "../../api/types";
import { shortSessionId } from "./conversation-model";

export interface Author {
  label: string;
  initials: string;
  shape: "round" | "square";
}

export function resolveAuthor(actor: Actor, titles: ReadonlyMap<string, string>): Author {
  const name =
    actor.kind === "session"
      ? (titles.get(actor.id) ?? actor.origin?.session_title ?? shortSessionId(actor.id))
      : actor.id;
  const label =
    actor.kind === "session" && actor.owner !== undefined ? `${name} (for ${actor.owner})` : name;
  const initials = name
    .split(/\s+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((part) => part[0]?.toUpperCase())
    .join("");

  return { initials, label, shape: actor.kind === "session" ? "square" : "round" };
}
