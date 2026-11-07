import type { Artifact, Ask, Comment } from "@legion/contracts";
import type { DispatchClient } from "../dispatch-http";
import { documentLabel, documentTopicOf, issueTopic, type OwnerTopic } from "../dispatch-owner";
import { ownerSubscribeLine } from "../dispatch-subscribe";
import type { ExecuteDispatchToolInput, ResolvedArtifact } from "./context";

export function documentTopic(artifact: Artifact): OwnerTopic {
  return documentTopicOf(artifact.project, artifact.slug);
}

export function resolvedTopic(resolved: ResolvedArtifact): OwnerTopic {
  if (resolved.owner.kind === "project") return documentTopic(resolved.artifact);
  if (resolved.issue === undefined) throw new Error("issue document is missing its issue");
  return issueTopic(resolved.issue.key);
}

/**
 * What a write says when the session follows none of `owner`'s events: ` (not subscribed to …)`,
 * with the `envoy_subscribe` line that would. Empty when one of the session's subscriptions covers
 * the owner, and when they could not be read (`subscriptions` unset or answering null), since
 * nothing then says what the session receives.
 */
export async function notSubscribed(
  owner: OwnerTopic,
  subscriptions: ExecuteDispatchToolInput["subscriptions"]
): Promise<string> {
  if (ownerSubscribeLine(owner, await subscriptions?.()) === null) return "";
  return ` (not subscribed to ${owner.label}; envoy_subscribe ${owner.topic} for every event on it)`;
}

/** What an ask write says the session follows, with the line that subscribes it to every event on
 *  `owner` when no subscription of its covers the owner (`ownerSubscribeLine`). */
export async function followsAsk(
  owner: OwnerTopic,
  subscriptions: ExecuteDispatchToolInput["subscriptions"]
): Promise<string> {
  const subscribe = ownerSubscribeLine(owner, await subscriptions?.());
  return `You follow this ask: its answer and replies reach you directly.${subscribe === null ? "" : ` ${subscribe}`}`;
}
export function documentResultDetails(artifact: Artifact): Record<string, unknown> {
  return {
    project: artifact.project,
    artifact: artifact.id,
    document: documentLabel(artifact.project, artifact.slug),
  };
}

export function writeResultDetails(
  resolved: ResolvedArtifact,
  fields: Record<string, unknown>
): Record<string, unknown> {
  if (resolved.owner.kind === "project") {
    return { ...documentResultDetails(resolved.artifact), ...fields };
  }
  if (resolved.issue === undefined) throw new Error("issue document is missing its issue");
  return { issue: resolved.issue.key, ...fields };
}

/** Owner and ask ids for a result about one ask; no claim about following. */
export async function askOwnerDetails(
  client: DispatchClient,
  ask: Pick<Ask, "id" | "issue_key" | "artifact_id">,
  artifact?: Artifact
) {
  if (ask.issue_key !== null) {
    return { issue: ask.issue_key, ask: ask.id };
  }
  if (ask.artifact_id === undefined || ask.artifact_id === null) {
    throw new Error("document ask is missing its artifact ID");
  }
  const owner = artifact ?? (await client.getArtifact(ask.artifact_id));
  return { ...documentResultDetails(owner), ask: ask.id };
}

/** Owner label and details for a result about one comment; the artifact is fetched when not supplied. */
export async function commentOwnerResult(
  client: DispatchClient,
  comment: Pick<Comment, "id" | "issue_key" | "artifact_id">,
  artifact?: Artifact
): Promise<{ readonly label: string; readonly details: Record<string, unknown> }> {
  if (comment.issue_key !== null) {
    return { label: comment.issue_key, details: { issue: comment.issue_key, comment: comment.id } };
  }
  if (comment.artifact_id === undefined || comment.artifact_id === null) {
    throw new Error("document comment is missing its artifact ID");
  }
  const owner = artifact ?? (await client.getArtifact(comment.artifact_id));
  return {
    label: documentLabel(owner.project, owner.slug),
    details: { ...documentResultDetails(owner), comment: comment.id },
  };
}

/**
 * Details for a write that made the calling session a follower of the ask (opened it,
 * requested approval through it, followed it): `follows.ask` tells the host to say so once.
 * Editing or resolving an ask records no follower, so those results use askOwnerDetails.
 */
export async function followedAskDetails(
  client: DispatchClient,
  ask: Pick<Ask, "id" | "issue_key" | "artifact_id">,
  artifact?: Artifact
) {
  return { ...(await askOwnerDetails(client, ask, artifact)), follows: { ask: ask.id } };
}
