/**
 * How a reply into a stored exchange is delivered: the mode its own attempts already used.
 *
 * One rule, read by every surface that answers a message a session was sent — the Agents page's
 * row, and the Inbox's reading pane, where a reply to an Agent row must behave exactly as one
 * sent from that row.
 */

import type { Agent, Message, MessageDelivery, MessageRead } from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import type { ReplyTarget } from "../conversation/composer-model";
import { plainDelivery } from "../conversation/delivery";
import { buildIssuePath } from "../refs/routes";
import type { AgentReply } from "./AgentMessageComposer";

/** The most recent delivery attempt across a root message and every reply under it. */
export function newestAttempt(
  items: readonly Pick<Message, "deliveries">[]
): MessageDelivery | undefined {
  let last: MessageDelivery | undefined;
  for (const attempt of items.flatMap((item) => item.deliveries)) {
    if (last === undefined || compareTimestamps(attempt.created_at, last.created_at) >= 0) {
      last = attempt;
    }
  }
  return last;
}

/** The delivery a reply into this exchange inherits: the agent, in the mode of the exchange's
 *  most recent attempt across the root and every delivered reply. Where that is a Send the agent
 *  does not take, or there is none (a reply to the agent's own message), it is a Send where the
 *  agent takes it, else a BTW where it takes that, else its plain mode - an Aside to an agent
 *  that takes neither. */
export function exchangeDelivery(
  agent: Agent,
  read: MessageRead
): NonNullable<ReplyTarget["thread"]> {
  const last = newestAttempt([read.message, ...read.replies]);
  const canSteer = agent.capabilities.includes("steer");
  const fallback = canSteer
    ? "steer"
    : agent.capabilities.includes("btw")
      ? "btw"
      : plainDelivery([agent.capabilities]);
  return {
    delivery:
      last === undefined || (last.delivery === "steer" && !canSteer) ? fallback : last.delivery,
    target: `session:${agent.session_id}`,
  };
}

/** What a reply to `node` in `read`'s exchange is addressed as: the quoted message, the
 *  conversation it belongs to (an issue, or none), and the delivery above. */
export function agentReplyTo(agent: Agent, read: MessageRead, node: Message): AgentReply {
  return {
    issueKey: read.message.issue_key,
    target: {
      actor: node.author,
      excerpt: node.body,
      id: node.id,
      parentKind: "message",
      thread: exchangeDelivery(agent, read),
      ...(node.issue_key === null
        ? {}
        : { to: buildIssuePath({ id: node.id, key: node.issue_key, kind: "message" }) }),
    },
  };
}
