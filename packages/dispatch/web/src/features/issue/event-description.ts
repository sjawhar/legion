import type { Event } from "../../api/types";
import { describeAskResolution } from "../refs/actor";

export function eventDescription(event: Event): string {
  switch (event.type) {
    case "project.created":
      return `Project created: ${event.payload.name}`;
    case "project.updated":
      return `Project updated: ${event.payload.name}`;
    case "settings.repo_project.updated":
      return `${event.payload.deleted ? "Repository mapping removed" : "Repository mapping updated"}: ${event.payload.mapping.repo}`;
    case "settings.architecture_source.updated":
      return `${event.payload.deleted ? "Architecture source removed" : "Architecture source updated"}: ${event.payload.source.repo}`;
    case "architecture.synced":
      return `Architecture synced: ${event.payload.components} component${event.payload.components === 1 ? "" : "s"} at ${event.payload.commit.slice(0, 12)}`;
    case "architecture.sync_failed":
      return `Architecture sync failed: ${event.payload.error}`;
    case "user_state.updated":
      return "User state updated";
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
    case "message.answered":
      return event.payload.body;
    case "message.delivery":
      return `Message ${event.payload.state}: ${event.payload.delivery}`;
    case "ask.opened":
      return `Ask opened: ${event.payload.question}`;
    case "ask.anchor_refreshed":
      return event.payload.anchor?.orphaned
        ? "Ask lost its quote"
        : "Ask re-anchored after an edit";
    case "comment.created":
    case "comment.answered":
      return event.payload.body;
    case "comment.anchor_refreshed":
      return event.payload.anchor?.orphaned
        ? "Comment lost its quote"
        : "Comment re-anchored after an edit";
    case "comment.delivery":
      return `Comment ${event.payload.state}: ${event.payload.delivery}`;
    case "issue.created":
      return "Issue created";
    case "issue.updated":
      return "Issue updated";
    case "issue.closed":
      return "Issue closed";
    case "artifact.created":
      return `Added ${event.payload.artifact.name}`;
    case "artifact.approved":
      return `Approved v${event.payload.version}`;
    case "artifact.changes_requested":
      return `Changes requested on v${event.payload.version}`;
    case "artifact.version":
      return `Saved ${event.payload.name} v${event.payload.version.number}`;
    case "suggestion.accepted":
      return "Suggestion accepted";
    case "suggestion.rejected":
      return "Suggestion rejected";
    case "child.status":
      return "Child status changed";
    case "child.added":
      return `Added child ${event.payload.child_key}`;
    case "child.removed":
      return `Removed child ${event.payload.child_key}`;
    case "subscription.remove_requested":
      return `Unsubscribe requested for ${event.payload.session_id}`;
    case "subscription.removed":
      return `Unsubscribed ${event.payload.session_id}`;
    case "ask.follower_added":
      return `${event.payload.session_id} follows an ask`;
    case "ask.follower_removed":
      return `${event.payload.session_id} unfollowed an ask`;
    case "block.repaired":
      return `Repaired block ${event.payload.block_id}`;
    case "block.invalid":
      return `marked decision ${event.payload.block_id} malformed: ${event.payload.reason}`;
  }
  return event satisfies never;
}
