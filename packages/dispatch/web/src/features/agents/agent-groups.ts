/**
 * The four states a live session is in (INFRA-812, R7), as one rule two surfaces read: the
 * Agents page's groups and, on dispatch://INFRA-883, a machine's tmux panes. It takes a
 * session's row of `GET /api/v1/agents`, the Inbox tally of that session's open asks waiting on
 * the person, and the person's unread replies from it, and answers the row's state. It fetches
 * nothing and knows nothing of the page that draws it.
 */
import type { Agent } from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";

/** Dispatch activity inside this long is `active`. */
export const ACTIVE_WITHIN_MS = 2 * 60_000;

/** Nothing heard from the listener for this long is `quiet`. */
export const QUIET_AFTER_MS = 10 * 60_000;

/** The four states, in the order a person reads them, which is also the order their tests are
 *  tried in. */
export const AGENT_GROUPS = ["needs-you", "active", "idle", "quiet"] as const;

export type AgentGroup = (typeof AGENT_GROUPS)[number];

/** What each state is called wherever it is shown. */
export const AGENT_GROUP_LABELS: Record<AgentGroup, string> = {
  active: "Active",
  idle: "Idle",
  "needs-you": "Needs you",
  quiet: "Quiet",
};

/**
 * The state of one session: **Needs you** when an open ask of its own waits on the person or it
 * has replies they have not read; **Active** when Dispatch heard from it inside two minutes;
 * **Quiet** when the listener has not heard from it for ten; **Idle** otherwise, which holds
 * every session Dispatch itself has never heard from (`last_activity` null).
 *
 * The four tests overlap, and the first match wins in that order (INFRA-812's D2): a session
 * with an open ask that has been silent for an hour is Needs you, not Quiet, because a person
 * reads "who is waiting for me" first. So Quiet is not the same set as the Agents page's old
 * `Inactive` fold - a silent session with an open ask leaves it.
 *
 * Nothing here reads a pin (D3): a pinned session is in the group its own row puts it in.
 */
export function agentGroup(
  agent: Agent,
  needsYou: number,
  unreadReplies: number,
  now: number = Date.now()
): AgentGroup {
  if (needsYou > 0 || unreadReplies > 0) return "needs-you";
  // `last_activity` is the server's own timestamp, so the bound is written the way the server
  // writes one and read by `compareTimestamps`: `Date.parse` drops the microseconds it carries.
  const activeSince = new Date(now - ACTIVE_WITHIN_MS).toISOString();
  if (agent.last_activity !== null && compareTimestamps(agent.last_activity, activeSince) >= 0) {
    return "active";
  }
  return now - agent.last_seen >= QUIET_AFTER_MS ? "quiet" : "idle";
}
