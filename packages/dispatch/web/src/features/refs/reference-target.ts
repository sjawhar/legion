import { type QueryClient, queryOptions, useQuery } from "@tanstack/react-query";

import { api, artifactVersionPath } from "../../api/client";
import { primarySpec } from "../../api/issue-cache";
import type {
  Artifact,
  ArtifactText,
  ArtifactVersionContent,
  AskRead,
  CommentRead,
  GraphReferences,
  IssueDetails,
  MessageRead,
} from "../../api/types";
import { useMarkdownHeadline } from "./markdown-engine";
import {
  buildDispatchReference,
  buildReferencePath,
  type DispatchReferenceRoute,
  type IssueRoute,
  isProjectRoute,
  referencedArtifact,
  referenceTargetKind,
  routeBlock,
} from "./routes";

/**
 * The queries behind a `dispatch://` reference's target, shared by every surface that resolves
 * one: the inline `RefLink` inside a rendered body, the `Unfurl` card under a bare-reference
 * body, and the `RefPreview` hover card. One module, so they all draw from one fetch path and
 * one cache key per record.
 */

export interface ReferenceTarget {
  readonly title: string | undefined;
  /** What the target says, as its author wrote it (Markdown): a message's body, a document's
   *  text. `undefined` while loading or for a target with nothing to say. */
  readonly description: string | undefined;
  /** The bytes route of the picture a reference to an image artifact names - the version it pins,
   *  else the latest - for a thumbnail beside the title; undefined for anything else. */
  readonly picture: string | undefined;
}

/** How much of a target's own text becomes a reference link's title: an ask's question, a
 *  comment's body. Long enough to recognise, short enough for a link inside a sentence. */
const TITLE_HEADLINE_MAX = 60;

const issueQuery = (key: string | undefined) =>
  queryOptions({
    queryKey: ["issue", key],
    queryFn: () => api.getIssue(key ?? ""),
  });

/** One issue message and its replies, as the Conversation and the hover card read it. */
export const messageQuery = (key: string | undefined, id: string | undefined) =>
  queryOptions({
    queryKey: ["issue", key, "message", id],
    queryFn: () => api.getMessage(key ?? "", id ?? ""),
  });

const projectArtifactQuery = (project: string | undefined, slug: string | undefined) =>
  queryOptions({
    queryKey: ["project", project, "artifacts", slug],
    queryFn: () => api.getProjectArtifact(project ?? "", slug ?? ""),
  });

/** An artifact of an agent's conversation, by its session and slug. */
export const agentArtifactQuery = (session: string | undefined, slug: string | undefined) =>
  queryOptions({
    queryKey: ["agent-artifacts", session, slug],
    queryFn: () => api.getAgentArtifact(session ?? "", slug ?? ""),
  });

/** A document's live text, or one immutable version of it when the reference pins a version. */
export const artifactTextQuery = (id: string | undefined, version: number | undefined) =>
  queryOptions<ArtifactText | ArtifactVersionContent>({
    queryKey: ["artifact", id, version ?? "text"],
    queryFn: () =>
      version === undefined
        ? api.getArtifactText(id ?? "")
        : api.getArtifactVersion(id ?? "", version),
  });

/** One ask with its thread, followers, and edits. */
export const askQuery = (id: string | undefined) =>
  queryOptions({
    queryKey: ["ask", id],
    queryFn: () => api.getAsk(id ?? ""),
  });

/** One comment with its replies. */
export const commentQuery = (id: string | undefined) =>
  queryOptions({
    queryKey: ["comment", id],
    queryFn: () => api.getComment(id ?? ""),
  });

/** `GET /api/v1/references?to=<reference>`: the node a reference names and the edges pointing at
 *  it, read by the Referenced by panel and, for a block, by the reference's title and card. */
export const referencesQuery = (reference: string | undefined) =>
  queryOptions({
    queryKey: ["references", "to", reference] as const,
    queryFn: () => api.getReferences(reference ?? ""),
  });

/** The reference's target records, each present once its query resolved. `artifact` is the
 * document a document/artifact reference names (never the owning issue's primary spec, unless the
 * reference names a block of it); `markdown` is that document's text at the referenced version,
 * read for no block reference; `block` is a block reference's graph read, whose node carries the
 * block's whole text and, for a pinned reference, its pin. */
export interface ReferenceData {
  readonly issue: IssueDetails | undefined;
  readonly artifact: Artifact | undefined;
  readonly markdown: string | undefined;
  readonly ask: AskRead | undefined;
  readonly comment: CommentRead | undefined;
  readonly message: MessageRead | undefined;
  readonly block: GraphReferences | undefined;
}

/** Warms every query the hover card will read for route, so a card mounted after the hover
 * delay renders populated instead of in its loading form: `useReferenceData`'s first hop, then
 * the document text behind a document/artifact reference once its artifact id is known, and —
 * for an issue reference — the issue's primary spec text, which only the card's issue view reads
 * (`useReferenceTarget` never fetches it). A block reference warms its graph read and the record
 * that names its document, and no document text. Every key comes from the query builders above. */
export function prefetchReference(queryClient: QueryClient, route: DispatchReferenceRoute): void {
  if (route.kind === "agent-artifact") {
    void queryClient.prefetchQuery(agentArtifactQuery(route.session, route.slug));
    return;
  }
  if (route.kind === "message") {
    void queryClient.prefetchQuery(messageQuery(route.key, route.id));
    return;
  }
  if (routeBlock(route) !== undefined) {
    void queryClient.prefetchQuery(referencesQuery(buildDispatchReference(route)));
    if (route.kind === "document") {
      void queryClient.prefetchQuery(projectArtifactQuery(route.project, route.slug));
    } else {
      void queryClient.prefetchQuery(issueQuery(route.key));
    }
    return;
  }
  if (route.kind === "ask") {
    void queryClient.prefetchQuery(askQuery(route.id));
  } else if (route.kind === "comment") {
    void queryClient.prefetchQuery(commentQuery(route.id));
  }
  if (route.kind === "document") {
    if (route.item?.kind === "ask") {
      void queryClient.prefetchQuery(askQuery(route.item.id));
    } else if (route.item?.kind === "comment") {
      void queryClient.prefetchQuery(commentQuery(route.item.id));
    }
    void queryClient.prefetchQuery(projectArtifactQuery(route.project, route.slug)).then(() => {
      const artifact = queryClient.getQueryData(
        projectArtifactQuery(route.project, route.slug).queryKey
      );
      if (artifact?.kind === "doc") {
        void queryClient.prefetchQuery(artifactTextQuery(artifact.id, route.version));
      }
    });
    return;
  }
  void queryClient.prefetchQuery(issueQuery(route.key)).then(() => {
    const issue = queryClient.getQueryData(issueQuery(route.key).queryKey);
    const artifact =
      route.kind === "artifact"
        ? issue?.artifacts.find((candidate) => candidate.slug === route.slug)
        : primarySpec(issue);
    if (artifact?.kind === "doc") {
      void queryClient.prefetchQuery(
        artifactTextQuery(artifact.id, route.kind === "artifact" ? route.version : undefined)
      );
    }
  });
}

export function useReferenceData(route: DispatchReferenceRoute | undefined): ReferenceData {
  const issueKey =
    route === undefined || route.kind === "agent-artifact" || isProjectRoute(route)
      ? undefined
      : route.key;
  const document = route?.kind === "document" ? route : undefined;
  const blockReference =
    route !== undefined && routeBlock(route) !== undefined
      ? buildDispatchReference(route)
      : undefined;
  const agentArtifact = route?.kind === "agent-artifact" ? route : undefined;
  const message = route?.kind === "message" ? route : undefined;
  const version =
    route?.kind === "artifact" || route?.kind === "document" || route?.kind === "agent-artifact"
      ? route.version
      : undefined;
  const askId =
    route?.kind === "ask"
      ? route.id
      : route?.kind === "document" && route.item?.kind === "ask"
        ? route.item.id
        : undefined;
  const commentId =
    route?.kind === "comment"
      ? route.id
      : route?.kind === "document" && route.item?.kind === "comment"
        ? route.item.id
        : undefined;
  const issue = useQuery({
    ...issueQuery(issueKey),
    enabled: issueKey !== undefined && message === undefined,
  });
  const messageQueryResult = useQuery({
    ...messageQuery(message?.key, message?.id),
    enabled: message !== undefined,
  });
  const projectArtifact = useQuery({
    ...projectArtifactQuery(document?.project, document?.slug),
    enabled: document !== undefined,
  });
  const sessionArtifact = useQuery({
    ...agentArtifactQuery(agentArtifact?.session, agentArtifact?.slug),
    enabled: agentArtifact !== undefined,
  });
  const block = useQuery({
    ...referencesQuery(blockReference),
    enabled: blockReference !== undefined,
  });
  let artifact: Artifact | undefined;
  switch (route?.kind) {
    case "document":
      artifact = projectArtifact.data;
      break;
    case "agent-artifact":
      artifact = sessionArtifact.data;
      break;
    case "spec":
      artifact = blockReference === undefined ? undefined : primarySpec(issue.data);
      break;
    case "artifact":
      artifact = issue.data?.artifacts.find((candidate) => candidate.slug === route.slug);
      break;
  }
  const text = useQuery({
    ...artifactTextQuery(artifact?.id, version),
    enabled: artifact?.kind === "doc" && blockReference === undefined,
  });
  const ask = useQuery({ ...askQuery(askId), enabled: askId !== undefined });
  const comment = useQuery({ ...commentQuery(commentId), enabled: commentId !== undefined });
  return {
    issue: issue.data,
    artifact,
    markdown: text.data !== undefined && "markdown" in text.data ? text.data.markdown : undefined,
    ask: ask.data,
    comment: comment.data,
    message: messageQueryResult.data,
    block: block.data,
  };
}

/** The text a block reference shows: the text the pinned version held for a pinned reference, the
 *  block's current text otherwise. */
export function blockText(node: GraphReferences["node"]): string | undefined {
  return node.pin === undefined ? node.text : node.pin.text;
}

/**
 * Every query behind a resolved reference title, shared by the Unfurl card and the inline
 * `RefLink` markdown/document rendering so both draw from one fetch path. An ask or comment
 * (whether issue-scoped or nested under a project document's `item`) resolves to its own text
 * rather than the owning issue's title or document; everything else falls back to the artifact
 * name (a document reference) or the issue title. A title drawn from text its author wrote (the
 * question, the comment) is that Markdown projected to plain words and cut
 * (`useMarkdownHeadline`): a link's text can hold no formatting, so a `**Blocking:**` question
 * titles its link `Blocking: …`, never `**Blocking:** …`. The description is the target's own
 * Markdown, for the caller to render formatted: a message's body, a document's text, and an
 * ask's or comment's text where the title had to cut it (a title that holds the whole text
 * needs no second copy under it); an issue with no document falls back to its status. An ask or
 * comment never borrows its issue's status or its document's text, while it loads or after. A
 * block of a document resolves to the headline of its own text, as an ask or comment does: the
 * text the pinned version held for a pinned reference, its current text otherwise. A
 * reference to an image artifact, of an issue, a project or an agent's conversation, also
 * answers its picture: the bytes route of the version it pins, else of the latest.
 */
export function useReferenceTarget(route: DispatchReferenceRoute | undefined): ReferenceTarget {
  const { artifact, ask, block, comment, issue, markdown, message } = useReferenceData(route);
  const kind = route === undefined ? undefined : referenceTargetKind(route);
  let authored: string | undefined;
  switch (kind) {
    case "ask":
      authored = ask?.ask.question;
      break;
    case "comment":
      authored = comment?.comment.body;
      break;
    case "block":
      authored = block === undefined ? undefined : blockText(block.node);
      break;
  }
  const headline = useMarkdownHeadline(authored, TITLE_HEADLINE_MAX);

  if (kind === "message") {
    return {
      title:
        message === undefined
          ? undefined
          : `${message.message.author.kind} ${message.message.author.id}`,
      description: message?.message.body,
      picture: undefined,
    };
  }
  if (kind === "ask" || kind === "comment" || kind === "block") {
    return {
      title: headline?.text,
      description: headline?.cut === true ? authored : undefined,
      picture: undefined,
    };
  }
  const text = markdown !== undefined && markdown.trim() !== "" ? markdown : undefined;
  const named = route === undefined ? undefined : referencedArtifact(route);
  const pictureVersion =
    named?.version ?? artifact?.versions.reduce((latest, item) => Math.max(latest, item.number), 0);
  return {
    title: artifact?.name ?? issue?.title,
    description: text ?? (artifact === undefined ? issue?.status : undefined),
    picture:
      named === undefined || artifact?.kind !== "image" || !pictureVersion
        ? undefined
        : artifactVersionPath(named.owner, named.slug, pictureVersion),
  };
}

/** Whether a reference's path waits on its issue: a block of an issue's spec pinned to a version
 *  opens that version under the spec's own slug, which only the issue names. */
export function needsSpecSlug(
  route: DispatchReferenceRoute
): route is Extract<IssueRoute, { kind: "spec" }> & { pin: string } {
  return route.kind === "spec" && route.pin !== undefined;
}

/** The SPA path a reference opens (`buildReferencePath`). A block of an issue's spec pinned to a
 *  version opens that version under the spec's own slug, so its path reads the issue (the query
 *  the reference's title and card already share); until the issue is read it is the live spec at
 *  the block. No other reference's path reads anything. */
export function useReferencePath(route: DispatchReferenceRoute): string;
export function useReferencePath(route: DispatchReferenceRoute | undefined): string | undefined;
export function useReferencePath(route: DispatchReferenceRoute | undefined): string | undefined {
  const specKey = route !== undefined && needsSpecSlug(route) ? route.key : undefined;
  const issue = useQuery({ ...issueQuery(specKey), enabled: specKey !== undefined });
  return route === undefined ? undefined : buildReferencePath(route, primarySpec(issue.data)?.slug);
}

/** `useReferencePath`'s answer for a click outside a render: it waits for the issue a pinned block
 *  of a spec needs, and opens the live spec at the block when the issue cannot be read, as the
 *  hook's path does before it is. */
export async function fetchReferencePath(
  queryClient: QueryClient,
  route: DispatchReferenceRoute
): Promise<string> {
  if (!needsSpecSlug(route)) {
    return buildReferencePath(route);
  }
  const issue = await queryClient.fetchQuery(issueQuery(route.key)).catch(() => undefined);
  return buildReferencePath(route, primarySpec(issue)?.slug);
}
