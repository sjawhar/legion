import type { Ask, Comment } from "../../api/types";
import { actorLabel } from "../refs/actor";

/** The status line on an open ask card: "Waiting on you" when the human holds the turn, else
 *  "Waiting on <agent>" naming who holds it - the agent whose progress note is the newest reply,
 *  or the asker. Null when the ask is closed or the read did not carry `waiting_on`. */
export function askTurnLabel(
  ask: Pick<Ask, "author" | "state" | "waiting_on">,
  lastReply: Pick<Comment, "author"> | undefined
): string | null {
  if (ask.state !== "open" || ask.waiting_on === undefined) return null;
  if (ask.waiting_on === "human") return "Waiting on you";
  const holder = lastReply?.author.kind === "session" ? lastReply.author : ask.author;
  return `Waiting on ${actorLabel(holder)}`;
}
