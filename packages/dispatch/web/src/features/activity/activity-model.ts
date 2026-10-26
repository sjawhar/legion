import type { Actor, Event } from "../../api/types";
import { type AgentPart, askResolutionParts } from "../refs/AgentName";

export function dateKey(iso: string): string {
  const date = new Date(iso);
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, "0")}-${String(date.getDate()).padStart(2, "0")}`;
}

function localDate(key: string): Date {
  const [year, month, day] = key.split("-").map(Number);
  return new Date(year ?? 1970, (month ?? 1) - 1, day ?? 1);
}

function dayLabel(date: string, today: string): string {
  if (date === today) return "Today";

  const yesterday = localDate(today);
  yesterday.setDate(yesterday.getDate() - 1);
  if (date === dateKey(yesterday.toISOString())) return "Yesterday";

  const sameYear = date.slice(0, 4) === today.slice(0, 4);
  return new Intl.DateTimeFormat(undefined, {
    day: "numeric",
    month: "long",
    weekday: "long",
    ...(sameYear ? {} : { year: "numeric" }),
  }).format(localDate(date));
}

/** Whether two actors are the same writer: one session id, or one human login. */
function sameActor(left: Actor, right: Actor): boolean {
  return left.kind === right.kind && left.id === right.id;
}

/** One line of the Activity tab, as `AgentPart`s: an agent it names keeps its identity until
 *  `ActivityLine` draws it through `AgentName`. */
export function activityDescription(event: Event, previousStatus?: string): AgentPart[] {
  const description = describe(event, previousStatus);
  return typeof description === "string" ? [description] : description;
}

function describe(event: Event, previousStatus: string | undefined): string | AgentPart[] {
  switch (event.type) {
    case "project.created":
      return `created project ${event.payload.key}`;
    case "project.updated":
      return `updated project ${event.payload.key}`;
    case "settings.repo_project.updated":
      return `${event.payload.deleted ? "removed" : "updated"} repository mapping ${event.payload.mapping.repo}`;
    case "settings.project_default_assignee.updated":
      return event.payload.assignee === null
        ? `cleared the default assignee of ${event.payload.project}`
        : `set the default assignee of ${event.payload.project} to ${event.payload.assignee}`;
    case "settings.architecture_source.updated":
      return `${event.payload.deleted ? "removed" : "updated"} architecture source ${event.payload.source.repo}`;
    case "architecture.synced":
      return `synced the architecture model at ${event.payload.commit.slice(0, 12)}`;
    case "architecture.sync_failed":
      return `failed to sync the architecture model: ${event.payload.error}`;
    case "user_state.updated":
      return "updated user state";
    case "user_agent_state.updated":
      return "updated agent conversation state";
    case "user_thread_state.updated":
      return "read a thread";
    case "issue.created":
      return "created the issue";
    case "issue.updated":
      return previousStatus === "done" && event.payload.status !== "done"
        ? `reopened the issue into ${event.payload.status}`
        : "updated the issue";
    case "issue.closed":
      return "closed the issue";
    case "issue.claimed":
      return event.payload.previous_claim === undefined
        ? "claimed the issue"
        : ["took the claim from ", { actor: event.payload.previous_claim.actor }];
    case "issue.released": {
      // A release always carries the claim it cleared (the server appends the event only with a
      // claim in hand), so the only question is whose it was: naming the releaser's own claim
      // back at them reads as somebody else's.
      if (event.payload.reason === "closed") {
        return "released the claim with the close";
      }
      const released = event.payload.previous_claim.actor;
      return sameActor(released, event.actor)
        ? "released the claim"
        : ["released ", { actor: released }, "'s claim"];
    }
    case "issue.deployed":
      return `deployed ${event.payload.repo}#${event.payload.number} to production (run ${event.payload.run_id})`;
    case "artifact.created":
      return `added ${event.payload.artifact.name}`;
    case "artifact.version":
      return `saved ${event.payload.name} v${event.payload.version.number}`;
    case "comment.created":
      if (event.payload.ask_id !== null) {
        return `replied to “${event.payload.ask_question ?? "an ask"}”`;
      }
      if (event.payload.reply_to !== null) return "replied to a comment";
      return `commented on ${event.payload.artifact_name}: “${event.payload.anchor?.quote ?? ""}”`;
    case "comment.answered":
      return "answered a comment";
    case "comment.delivery":
      return `delivered a comment mention to ${event.payload.target}`;
    case "comment.resolved":
      return `resolved a comment on ${event.payload.artifact_name}`;
    case "comment.anchor_refreshed":
      return event.payload.anchor?.orphaned
        ? `made a comment on ${event.payload.artifact_name} lose its quote`
        : `re-anchored a comment on ${event.payload.artifact_name} after an edit`;
    case "comment.reopened":
      return `reopened a comment on ${event.payload.artifact_name}`;
    case "artifact.approved":
      return `approved ${event.payload.name} v${event.payload.version}`;
    case "artifact.changes_requested":
      return `requested changes on ${event.payload.name} v${event.payload.version}`;
    case "comment.edited":
      return `edited a comment on ${event.payload.artifact_name}`;
    case "suggestion.accepted":
      return `accepted a suggestion on ${event.payload.artifact_name}`;
    case "suggestion.rejected":
      return `rejected a suggestion on ${event.payload.artifact_name}`;
    case "ask.resolved":
      return askResolutionParts(event.payload.resolution);
    case "ask.opened":
      return `asked “${event.payload.question}”`;
    case "ask.anchor_refreshed":
      return event.payload.anchor?.orphaned
        ? `made an ask lose its quote: “${event.payload.question}”`
        : `re-anchored an ask after an edit: “${event.payload.question}”`;
    case "ask.edited":
      return `edited the question "${event.payload.question}"`;
    case "ask.handed_back":
      return `handed “${event.payload.question}” back for approval`;
    case "ask.answered":
      return event.payload.previous_answer === undefined
        ? `answered “${event.payload.question}”`
        : `changed the answer to “${event.payload.question}”`;
    case "message.created":
      return "sent a message";
    case "message.delivery":
      return "delivered a message";
    case "message.accepted":
      return "took a message as its own turn";
    case "message.answered":
      return "answered a message";
    case "child.status":
      return `moved ${event.payload.child_key} from ${event.payload.from} to ${event.payload.to}`;
    case "child.added":
      return `added child ${event.payload.child_key}`;
    case "child.removed":
      return `removed child ${event.payload.child_key}`;
    case "subscription.remove_requested":
      return [
        "requested unsubscribe for ",
        { sessionId: event.payload.session_id, storedTitle: undefined },
      ];
    case "subscription.removed":
      return [
        "unsubscribed ",
        { sessionId: event.payload.session_id, storedTitle: undefined },
        " from notifications",
      ];
    case "ask.follower_added":
      return event.actor.kind === "session" && event.actor.id === event.payload.session_id
        ? "followed an ask"
        : [
            "added ",
            { sessionId: event.payload.session_id, storedTitle: undefined },
            " to an ask's followers",
          ];
    case "ask.follower_removed":
      return event.actor.kind === "session" && event.actor.id === event.payload.session_id
        ? "unfollowed an ask"
        : [
            "removed ",
            { sessionId: event.payload.session_id, storedTitle: undefined },
            " from an ask's followers",
          ];
    case "block.repaired":
      return `repaired server-owned state on block ${event.payload.block_id}`;
    case "block.invalid":
      return `marked decision ${event.payload.block_id} malformed: ${event.payload.reason}`;
  }
}

/** One event of the issue's history as the Activity tab lists it, with the dividers that break
 *  the list into days and mark where the reader's watermark falls. */
export type ActivityItem =
  | {
      kind: "event";
      id: string;
      at: string;
      author: Actor;
      description: AgentPart[];
      event: Event;
      seq: number;
    }
  | { kind: "day-divider"; id: string; date: string; label: string }
  | { kind: "unread-divider"; id: "unread-divider" };

export interface ActivityInput {
  events: Event[];
  /** `user_issue_state.last_read_seq`; the Conversation is its only writer. */
  lastReadSeq: number;
  /** Local YYYY-MM-DD. */
  today: string;
}

/**
 * Every event of the issue, newest first, one line each: no grouping into turns and no hiding,
 * which is what separates this list from the Conversation's. A line's description is read in
 * sequence order, so `issue.updated` can say what status it reopened from.
 */
export function buildActivityItems({ events, lastReadSeq, today }: ActivityInput): ActivityItem[] {
  const lines: Extract<ActivityItem, { kind: "event" }>[] = [];
  let previousIssueStatus: string | undefined;

  for (const event of [...events].sort((left, right) => left.seq - right.seq)) {
    lines.push({
      kind: "event",
      id: `activity:${event.id}`,
      at: event.created_at,
      author: event.actor,
      description: activityDescription(event, previousIssueStatus),
      event,
      seq: event.seq,
    });
    if (
      event.type === "issue.created" ||
      event.type === "issue.updated" ||
      event.type === "issue.closed"
    ) {
      previousIssueStatus = event.payload.status;
    }
  }

  // The divider sits between the newest read line and the oldest unread one, so it is drawn only
  // when the loaded pages hold both.
  let unreadPlaced = !(
    lines.some((line) => line.seq <= lastReadSeq) && lines.some((line) => line.seq > lastReadSeq)
  );
  const items: ActivityItem[] = [];
  let currentDay: string | undefined;

  for (let index = lines.length - 1; index >= 0; index -= 1) {
    const line = lines[index] as Extract<ActivityItem, { kind: "event" }>;
    const day = dateKey(line.at);
    if (day !== currentDay) {
      currentDay = day;
      items.push({ kind: "day-divider", id: `day:${day}`, date: day, label: dayLabel(day, today) });
    }
    if (!unreadPlaced && line.seq <= lastReadSeq) {
      items.push({ kind: "unread-divider", id: "unread-divider" });
      unreadPlaced = true;
    }
    items.push(line);
  }

  return items;
}
