import type { Event } from "../../api/types";
import { describeAskResolution } from "../refs/actor";

export function eventDescription(event: Event): string {
  switch (event.type) {
    case "ask.resolved":
      return describeAskResolution(event.payload.resolution);
    case "ask.answered":
      return `Ask answered: ${event.payload.question}`;
    case "ask.edited":
      return `Ask edited: ${event.payload.question}`;
    case "comment.resolved":
      return "Comment resolved";
    case "comment.reopened":
      return "Comment reopened";
    case "comment.edited":
      return "Comment edited";
    case "message.created":
      return event.payload.body;
    case "ask.opened":
      return `Ask opened: ${event.payload.question}`;
    case "comment.created":
      return event.payload.body;
    case "issue.created":
      return "Issue created";
    case "issue.updated":
      return "Issue updated";
    case "issue.closed":
      return "Issue closed";
    case "artifact.created":
      return "Artifact created";
    case "artifact.approved":
      return `Approved v${event.payload.version}`;
    case "artifact.changes_requested":
      return `Changes requested on v${event.payload.version}`;
    case "artifact.version":
      return "Artifact version saved";
    case "suggestion.accepted":
      return "Suggestion accepted";
    case "suggestion.rejected":
      return "Suggestion rejected";
    case "child.status":
      return "Child status changed";
    case "subscription.removed":
      return `Unsubscribed ${event.payload.session_id}`;
  }
  return event satisfies never;
}
