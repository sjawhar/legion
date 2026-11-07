import { ASK_TURNS } from "@legion/contracts";

import type { Actor, Ask, AskTurn, Comment } from "../../api/types";

/** `value` as a turn, or `undefined` where this dashboard knows no such word: a turn reaches the
 *  browser as the string the route table declares, and `ASK_TURNS` is the closed list that string
 *  is one of. A row whose turn is a word this build does not know is read as a row carrying no
 *  turn at all, which is what every surface already draws for one. */
export function askTurn(value: string | undefined): AskTurn | undefined {
  return ASK_TURNS.find((known) => known === value);
}

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
