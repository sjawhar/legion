import type { Actor, Ask, Comment } from "../../api/types";

/** Who holds an open ask's turn: `"you"` when the human does, else the agent holding it - the
 *  agent whose progress note is the newest reply, or the asker. The card draws it as "Waiting on
 *  you" / "Waiting on <agent>". Null when the ask is closed or the read did not carry
 *  `waiting_on`. */
export function askTurnHolder(
  ask: Pick<Ask, "author" | "state" | "waiting_on">,
  lastReply: Pick<Comment, "author"> | undefined
): "you" | Actor | null {
  if (ask.state !== "open" || ask.waiting_on === undefined) return null;
  if (ask.waiting_on === "human") return "you";
  return lastReply?.author.kind === "session" ? lastReply.author : ask.author;
}
