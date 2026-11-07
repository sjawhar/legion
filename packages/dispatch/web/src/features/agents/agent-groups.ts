import type { Agent } from "../../api/types";
import { compareTimestamps, isTimestamp } from "../../lib/timestamps";

/** Dispatch activity this recent is a session someone is working with right now. */
const ACTIVE_WITHIN_MS = 2 * 60_000;

/** Past this much silence from the registry the session is Quiet. It is the ten minutes the
 *  Agents page has always folded `Inactive` on, which it takes from here, so one session does
 *  not sit in two groups' worth of rules. */
export const QUIET_AFTER_MS = 10 * 60_000;

/**
 * Which of the four groups a session's row belongs in.
 *
 * `quiet` is not today's `Inactive` fold: that one holds every session unseen for ten minutes
 * whatever it is owed, while a session with an open ask for the viewer is `needs-you` however
 * long it has been silent.
 */
export type AgentGroup = "needs-you" | "active" | "idle" | "quiet";

/**
 * The group one session's row belongs in, from that row alone.
 *
 * The four tests overlap, so the answer is the first that matches, in this order: an ask whose
 * turn is the viewer's or a reply they have not read (`needs-you`); Dispatch activity inside two
 * minutes (`active`); otherwise heard from by the registry inside ten minutes (`idle`, which is
 * where a session Dispatch has never heard from sits, since its `last_activity` is null); and a
 * session unseen for ten minutes (`quiet`).
 *
 * `asksForYou` is this session's count from the viewer's inbox tally and `unreadReplies` its
 * unread replies to them; `now` is an argument rather than `Date.now()` so a caller's clock
 * decides and a test is deterministic. A pin lifts no row out of its group here: a pinned
 * session is in the group its own row earns.
 */
export function agentGroup(
  agent: Agent,
  asksForYou: number,
  unreadReplies: number,
  now: number
): AgentGroup {
  if (asksForYou > 0 || unreadReplies > 0) {
    return "needs-you";
  }
  const active =
    agent.last_activity !== null &&
    // A timestamp the comparison would throw on is read as no activity rather than taking a
    // whole list of rows down with it.
    isTimestamp(agent.last_activity) &&
    compareTimestamps(agent.last_activity, new Date(now - ACTIVE_WITHIN_MS).toISOString()) >= 0;
  if (active) {
    return "active";
  }
  return now - agent.last_seen < QUIET_AFTER_MS ? "idle" : "quiet";
}
