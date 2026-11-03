import { z } from "zod";
import type {
  AskAnsweredEventPayload,
  AskEditEventPayload,
  AskEventPayload,
  AskFollowerEventPayload,
  AskResolution,
} from "./dispatch-api-asks";
import type { CommentDeliveryEventPayload, CommentEventPayload } from "./dispatch-api-comments";
import type {
  Actor,
  ArchitectureSourceUpdatedEventPayload,
  ArchitectureSyncedEventPayload,
  ArchitectureSyncFailedEventPayload,
  DisplayTree,
  Project,
  ProjectDefaultAssigneeUpdatedEventPayload,
  RepoProjectUpdatedEventPayload,
} from "./dispatch-api-core";
import type {
  ArtifactCreatedEventPayload,
  ArtifactHumanEditedEventPayload,
  ArtifactReviewEventPayload,
  ArtifactVersionEventPayload,
  BlockInvalidEventPayload,
  BlockRepairedEventPayload,
  DecisionKeptEventPayload,
} from "./dispatch-api-documents";
import type {
  ChildEventPayload,
  ChildStatusEventPayload,
  IssueClaimEventPayload,
  IssueDeployedEventPayload,
  IssueEventPayload,
  IssueReleasedEventPayload,
} from "./dispatch-api-issues";
import type {
  MessageAcceptedEventPayload,
  MessageDeliveryEventPayload,
  MessageEventPayload,
} from "./dispatch-api-messages";
import type { SubscriptionRemovedEventPayload } from "./dispatch-api-sessions";
import type {
  UserAgentStateUpdatedEventPayload,
  UserStateUpdatedEventPayload,
  UserThreadStateUpdatedEventPayload,
} from "./dispatch-api-user";

/**
 * The event log: one entry per committed write, its type naming the payload it carries, and the
 * framing a reader of the bus validates before it looks at that payload.
 */

/**
 * One phase handoff without its body: what `GET /api/v1/issues/{key}/handoffs` lists, newest
 * first, and what `POST /api/v1/issues/{key}/handoffs/{phase}` answers. `phase` is a lowercase slug
 * (Legion writes `plan`, `implement`, `test` and `review`), `version` counts from 1 within the
 * issue and phase, and `author` is the session that wrote it.
 */
export interface HandoffSummary {
  readonly id: string;
  readonly issue_key: string;
  readonly phase: string;
  readonly version: number;
  readonly author: Actor;
  readonly created_at: string;
}

/**
 * One phase handoff with its body, as `GET /api/v1/issues/{key}/handoffs/{phase}/latest` and
 * `GET /api/v1/handoffs/{id}` answer it. `body` is the exact text the session wrote: one JSON
 * object of at most `DISPATCH_HANDOFF_BODY_MAX` bytes, never reformatted.
 */
export interface Handoff extends HandoffSummary {
  readonly body: string;
}

/**
 * `handoff.recorded`: a session recorded version `version` of the issue's `phase` handoff, the
 * record `handoff_id`. It never carries the body; a reader fetches the record by id. Appended in
 * the write's own transaction and published on the issue's subject as every issue event is.
 * Strict: the Go writer (`HandoffRecordedEventPayload` in
 * `packages/envoy/internal/dispatch/model`) emits exactly these three fields.
 */
export const HandoffRecordedPayloadSchema = z.strictObject({
  handoff_id: z.string(),
  phase: z.string(),
  version: z.number().int().positive(),
});

export type HandoffRecordedPayload = z.output<typeof HandoffRecordedPayloadSchema>;

/**
 * The payload of a `server.started` envelope: the build commit every live Dispatch server process
 * serves (a fleet any of whose processes carries no stamped commit is never announced) and when
 * the fleet was found serving it, by the database's clock, in UTC. The field is named `started_at`
 * because the Go writer and the Legion daemon carry that name. The Legion daemon compares the
 * commit with the merges of its open production checks. Strict: the Go writer emits exactly these
 * two fields.
 */
export const DispatchServerStartedPayloadSchema = z.strictObject({
  commit: z.string().min(1),
  started_at: z.iso.datetime(),
});

export type DispatchServerStartedPayload = z.output<typeof DispatchServerStartedPayloadSchema>;

interface DispatchEventBase {
  readonly id: number;
  readonly issue_key: string | null;
  readonly artifact_id?: string | null;
  readonly project?: string;
  readonly seq: number;
  readonly actor: Actor;
  /** Whether the event wakes the issue's route, claimant and authors (`events.Notify`), so an
   *  agent's own event is not a wake. A reader following an owner topic on the bus skips on the
   *  published copy's `quiet` instead (`PublishedDispatchEvent`). */
  readonly notify: boolean;
  readonly created_at: string;
  /** The display tree of every stored text the payload carries, keyed by that text's path in
   *  this envelope (`payload.body`, `payload.options.0.label`). It rides beside the payload, so
   *  each payload keeps the shape its readers know. Absent unless the request sent
   *  `Dispatch-Trees: 1`; present on every `/ws/live` event frame. */
  readonly trees?: Readonly<Record<string, DisplayTree>>;
}

export type DispatchEvent =
  | (DispatchEventBase & { readonly type: "project.created"; readonly payload: Project })
  | (DispatchEventBase & { readonly type: "project.updated"; readonly payload: Project })
  | (DispatchEventBase & {
      readonly type: "settings.repo_project.updated";
      readonly payload: RepoProjectUpdatedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "settings.project_default_assignee.updated";
      readonly payload: ProjectDefaultAssigneeUpdatedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "settings.architecture_source.updated";
      readonly payload: ArchitectureSourceUpdatedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "architecture.synced";
      readonly payload: ArchitectureSyncedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "architecture.sync_failed";
      readonly payload: ArchitectureSyncFailedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "user_state.updated";
      readonly payload: UserStateUpdatedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "user_agent_state.updated";
      readonly payload: UserAgentStateUpdatedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "user_thread_state.updated";
      readonly payload: UserThreadStateUpdatedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "decision.kept";
      readonly payload: DecisionKeptEventPayload;
    })
  | (DispatchEventBase & { readonly type: "issue.created"; readonly payload: IssueEventPayload })
  | (DispatchEventBase & { readonly type: "issue.updated"; readonly payload: IssueEventPayload })
  | (DispatchEventBase & { readonly type: "issue.closed"; readonly payload: IssueEventPayload })
  | (DispatchEventBase & {
      readonly type: "issue.claimed";
      readonly payload: IssueClaimEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "issue.released";
      readonly payload: IssueReleasedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "issue.deployed";
      readonly payload: IssueDeployedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "handoff.recorded";
      readonly payload: HandoffRecordedPayload;
    })
  | (DispatchEventBase & {
      readonly type: "artifact.created";
      readonly payload: ArtifactCreatedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "artifact.version";
      readonly payload: ArtifactVersionEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "artifact.human_edited";
      readonly payload: ArtifactHumanEditedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "artifact.approved";
      readonly payload: ArtifactReviewEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "artifact.changes_requested";
      readonly payload: ArtifactReviewEventPayload;
    })
  | (DispatchEventBase & { readonly type: "ask.opened"; readonly payload: AskEventPayload })
  | (DispatchEventBase & {
      readonly type: "ask.anchor_refreshed";
      readonly payload: AskEventPayload;
    })
  /** Every rewording: a PATCH edit, a block ask's new text, and an approval request's move to a new
   *  version or new summary. Each one stamps the ask's `edited_at`. */
  | (DispatchEventBase & {
      readonly type: "ask.edited";
      readonly payload: AskEditEventPayload;
    })
  /** An agent handed its approval request back to the human: `approval.requested_version` is the
   *  version it handed back. It rewords nothing and leaves `edited_at` as it was; a hand-back
   *  with a new summary is an `ask.edited` followed by this event. */
  | (DispatchEventBase & {
      readonly type: "ask.handed_back";
      readonly payload: AskEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "ask.answered";
      readonly payload: AskAnsweredEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "ask.resolved";
      readonly payload: AskEventPayload & {
        readonly state: "resolved";
        readonly resolution: AskResolution;
      };
    })
  | (DispatchEventBase & {
      readonly type: "block.repaired";
      readonly payload: BlockRepairedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "block.invalid";
      readonly payload: BlockInvalidEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "comment.created";
      readonly payload: CommentEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "comment.anchor_refreshed";
      readonly payload: CommentEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "comment.delivery";
      readonly payload: CommentDeliveryEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "comment.answered";
      readonly payload: CommentEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "comment.resolved";
      readonly payload: CommentEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "comment.reopened";
      readonly payload: CommentEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "comment.edited";
      readonly payload: CommentEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "suggestion.accepted";
      readonly payload: CommentEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "suggestion.rejected";
      readonly payload: CommentEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "message.created";
      readonly payload: MessageEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "message.delivery";
      readonly payload: MessageDeliveryEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "message.accepted";
      readonly payload: MessageAcceptedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "message.answered";
      readonly payload: MessageEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "child.status";
      readonly payload: ChildStatusEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "child.added";
      readonly payload: ChildEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "child.removed";
      readonly payload: ChildEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "subscription.remove_requested";
      readonly payload: SubscriptionRemovedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "subscription.removed";
      readonly payload: SubscriptionRemovedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "ask.follower_added";
      readonly payload: AskFollowerEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "ask.follower_removed";
      readonly payload: AskFollowerEventPayload;
    });

export type Event = DispatchEvent;
export type EventType = DispatchEvent["type"];

/**
 * One event as Dispatch's outbox publishes it to NATS: the event this API serves, plus `quiet`.
 *
 * `quiet` is whether the event carries nothing for a reader following its owner topic
 * (`events.Quiet` in envoy): a delivery receipt, an anchor refresh, settings or per-viewer
 * bookkeeping, a targeted message its session already had, an ask edit that only moved a version,
 * or a version nobody named. That is what such a reader skips on, where `notify` answers only
 * whether the event wakes the issue's route, claimant and authors - false for every event an agent
 * wrote, so a reader skipping on it drops the activity a session subscribed to the issue asked
 * for. It is optional because a frame published before Dispatch stated it carries none, and such a
 * frame is judged by `notify` instead. The API's own event shape carries no `quiet`: the outbox
 * adds it to the copy it publishes.
 */
export type PublishedDispatchEvent = DispatchEvent & { readonly quiet?: boolean };

/**
 * Tolerantly validates the Dispatch event framing delivered by Envoy. Payload
 * content is parsed per event type so malformed optional display fields do not
 * discard a valid event header.
 */
export const DispatchEventSchema = z.object({
  issue_key: z.string().nullable(),
  artifact_id: z.string().nullish(),
  project: z.string().optional(),
  type: z.string(),
  actor: z.object({ kind: z.string(), id: z.string().optional() }).passthrough(),
  payload: z.object({}).passthrough(),
});

export type InboundDispatchEvent = z.output<typeof DispatchEventSchema>;
