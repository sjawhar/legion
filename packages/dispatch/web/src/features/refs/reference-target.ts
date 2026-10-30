import { type QueryClient, type QueryStatus, queryOptions, useQuery } from "@tanstack/react-query";

import { ApiError, api, artifactVersionPath } from "../../api/client";
import { primarySpec } from "../../api/issue-cache";
import { issueQuery } from "../../api/queries";
import type {
  Artifact,
  ArtifactText,
  ArtifactVersionContent,
  AskRead,
  CommentRead,
  GraphReferences,
  IssueReferenceSummary,
  IssueReferenceSummaryDocument,
  MessageRead,
} from "../../api/types";
import { itemOfBlock } from "./block-reference";
import { useMarkdownHeadline } from "./markdown-engine";
import {
  buildDispatchReference,
  buildReferencePath,
  type DispatchReferenceRoute,
  type IssueRoute,
  referencedArtifact,
} from "./routes";
import { referenceTargetKind } from "./target-id";

/**
 * The queries behind a `dispatch://` reference's target, shared by every surface that resolves
 * one: the inline `RefLink` inside a rendered body, the `Unfurl` card under a bare-reference
 * body, and the `RefPreview` hover card. One module, so they all draw from one fetch path and
 * one cache key per record.
 *
 * A reference to an issue or a document reads what it shows through a summary read, never the
 * full issue: an issue and its documents share `issueSummaryQuery`, a project document has
 * `documentSummaryQuery`. A reference to a block of a document reads the block's graph read
 * (`blockQuery`), which carries the block's text. A page can link a hundred of them, so a healthy
 * summary or block read is fetched again only by an event that can change it (`staleTime:
 * Infinity`); the event refresh rules name these keys. Only a read in error is fetched again on
 * window focus (`refetchFailedOnFocus`). An ask, comment or message reference reads its own
 * record alone.
 */

/**
 * Window focus fetches a summary or block read again only while it is in error, so a read that
 * failed (a 503 past its retries) recovers on the open page while a healthy one stays quiet. It
 * is fetched even over the data it holds, which an event's refetch that failed leaves fresh by
 * age (`staleTime: Infinity`).
 */
const refetchFailedOnFocus = (query: { readonly state: { readonly status: QueryStatus } }) =>
  query.state.status === "error" ? "always" : false;

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

/** What a reference to an issue, or to one of its documents, shows: the issue's title and status
 *  and each document's name, kind, latest version and whether it is the spec
 *  (`GET /issues/{key}/summary`). Fetched again only by an event that can change it, or on focus
 *  after it failed. */
export const issueSummaryQuery = (key: string | undefined) =>
  queryOptions({
    queryKey: ["issue-summary", key],
    queryFn: () => api.getIssueSummary(key ?? ""),
    staleTime: Number.POSITIVE_INFINITY,
    refetchOnWindowFocus: refetchFailedOnFocus,
  });

/** What a reference to a project document shows: its name, kind and latest version
 *  (`GET /projects/{key}/artifacts/{slug}/summary`). Fetched again only by an event that can
 *  change it, or on focus after it failed. */
export const documentSummaryQuery = (project: string | undefined, slug: string | undefined) =>
  queryOptions({
    queryKey: ["doc-summary", project, slug],
    queryFn: () => api.getProjectDocumentSummary(project ?? "", slug ?? ""),
    staleTime: Number.POSITIVE_INFINITY,
    refetchOnWindowFocus: refetchFailedOnFocus,
  });

/** One issue message and its replies, as the Conversation's landing, a reference and the hover
 *  card read it. Outside the `["issue"]` prefix, so a refresh of issue details leaves it alone. */
export const messageQuery = (key: string | undefined, id: string | undefined) =>
  queryOptions({
    queryKey: ["issue-message", key, id],
    queryFn: () => api.getMessage(key ?? "", id ?? ""),
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

/** A block reference's graph read, under the Referenced by panel's key: its node carries the
 *  block's whole text and, for a pinned reference, its pin. A link to a block reads it as a link
 *  to an issue reads its summary, fetched again only by an event that can change it (a version of
 *  the block's document, a citation of the block added or removed, an edit of the comment whose
 *  block it is, or a reply that names the block), or on focus after it failed. A read that answers
 *  that the block is gone shows none of the text it held before (`useReferenceData`). */
export const blockQuery = (reference: string | undefined) =>
  queryOptions({
    ...referencesQuery(reference),
    staleTime: Number.POSITIVE_INFINITY,
    refetchOnWindowFocus: refetchFailedOnFocus,
  });

/** The document a reference names, as much of it as a reference shows: an issue summary's
 *  document, a project document's summary, or an agent conversation's artifact cut to the same
 *  fields. `latest_version` is its greatest version number, null when it has none. */
export type ReferenceArtifact = Pick<
  IssueReferenceSummaryDocument,
  "id" | "kind" | "latest_version" | "name"
>;

/** An agent conversation's artifact as a `ReferenceArtifact`; its versions are in number order. */
function agentReferenceArtifact(artifact: Artifact): ReferenceArtifact {
  return {
    id: artifact.id,
    kind: artifact.kind,
    latest_version: artifact.versions.at(-1)?.number ?? null,
    name: artifact.name,
  };
}

/** The issue's spec among an issue summary's documents, or undefined until the summary is read. */
export function summarySpec(
  summary: IssueReferenceSummary | undefined
): IssueReferenceSummaryDocument | undefined {
  return summary?.documents.find((document) => document.primary);
}

/** The block a route names, as the reference its graph read is keyed by; undefined for a route
 *  that names no block. */
function blockReferenceOf(route: DispatchReferenceRoute | undefined): string | undefined {
  return route !== undefined && "block" in route && route.block !== undefined
    ? buildDispatchReference(route)
    : undefined;
}

/** The reference's target records, each present once its query resolved. `issue` is the issue
 * summary behind an issue or issue-document reference; `artifact` is the document a
 * document/artifact reference names (never the owning issue's primary spec); `markdown` is that
 * document's text at the referenced version, read only when the caller asked for it; `block` is a
 * block reference's graph read, whose node carries the block's whole text and, for a pinned
 * reference, its pin, undefined once the read answers that the block is gone. A block reference
 * reads that alone: its document's name is the hover card's own read (`useBlockDocument`). */
export interface ReferenceData {
  readonly issue: IssueReferenceSummary | undefined;
  readonly artifact: ReferenceArtifact | undefined;
  readonly markdown: string | undefined;
  readonly ask: AskRead | undefined;
  readonly comment: CommentRead | undefined;
  readonly message: MessageRead | undefined;
  readonly block: GraphReferences | undefined;
}

export interface ReferenceReadOptions {
  /** Whether to read the named document's text into `markdown`. The Unfurl card and the hover
   *  card show it; an inline link shows only a title and reads none. */
  readonly text: boolean;
}

/** Warms every query the hover card will read for route, so a card mounted after the hover
 * delay renders populated instead of in its loading form: an issue's full read and its primary
 * spec text (the card's issue view, which no link reads); a document's summary, then its text once
 * its id is known; a block's graph read and, for a block of a document, the summary that names its
 * document, and no document text (a block of a comment or message is its graph read alone, whose
 * node carries the block's text); an ask's, comment's or message's own read. Every key comes from
 * the query builders above. */
export function prefetchReference(queryClient: QueryClient, route: DispatchReferenceRoute): void {
  const warmText = (
    artifact: Pick<ReferenceArtifact, "id" | "kind"> | undefined,
    version: number | undefined
  ) => {
    if (artifact?.kind === "doc") {
      void queryClient.prefetchQuery(artifactTextQuery(artifact.id, version));
    }
  };
  const block = blockReferenceOf(route);
  if (block !== undefined) {
    void queryClient.prefetchQuery(blockQuery(block));
    if (itemOfBlock(route) !== undefined) {
      return;
    }
    if (route.kind === "document") {
      void queryClient.prefetchQuery(documentSummaryQuery(route.project, route.slug));
    } else if ("key" in route) {
      void queryClient.prefetchQuery(issueSummaryQuery(route.key));
    }
    return;
  }
  switch (route.kind) {
    case "agent-artifact": {
      const query = agentArtifactQuery(route.session, route.slug);
      void queryClient.prefetchQuery(query).then(() => {
        const artifact = queryClient.getQueryData(query.queryKey);
        warmText(
          artifact === undefined ? undefined : agentReferenceArtifact(artifact),
          route.version
        );
      });
      return;
    }
    case "message":
      void queryClient.prefetchQuery(messageQuery(route.key, route.id));
      return;
    case "ask":
      void queryClient.prefetchQuery(askQuery(route.id));
      return;
    case "comment":
      void queryClient.prefetchQuery(commentQuery(route.id));
      return;
    case "document": {
      if (route.item?.kind === "ask") {
        void queryClient.prefetchQuery(askQuery(route.item.id));
        return;
      }
      if (route.item?.kind === "comment") {
        void queryClient.prefetchQuery(commentQuery(route.item.id));
        return;
      }
      const query = documentSummaryQuery(route.project, route.slug);
      void queryClient
        .prefetchQuery(query)
        .then(() => warmText(queryClient.getQueryData(query.queryKey), route.version));
      return;
    }
    case "artifact": {
      const query = issueSummaryQuery(route.key);
      void queryClient.prefetchQuery(query).then(() => {
        const documents = queryClient.getQueryData(query.queryKey)?.documents;
        warmText(
          documents?.find((candidate) => candidate.slug === route.slug),
          route.version
        );
      });
      return;
    }
    default: {
      const query = issueQuery(route.key);
      void queryClient
        .prefetchQuery(query)
        .then(() => warmText(primarySpec(queryClient.getQueryData(query.queryKey)), undefined));
    }
  }
}

export function useReferenceData(
  route: DispatchReferenceRoute | undefined,
  options: ReferenceReadOptions
): ReferenceData {
  const blockReference = blockReferenceOf(route);
  // A block of a comment or message is read from its graph node alone, which carries its text:
  // no issue, document, ask, comment or message read behind it.
  const record = route !== undefined && itemOfBlock(route) !== undefined ? undefined : route;
  const issueRoute =
    route === undefined ||
    route.kind === "agent-artifact" ||
    route.kind === "document" ||
    blockReference !== undefined
      ? undefined
      : route;
  // An ask, comment or message under an issue shows only its own record.
  const summaryKey =
    issueRoute === undefined ||
    issueRoute.kind === "ask" ||
    issueRoute.kind === "comment" ||
    issueRoute.kind === "message"
      ? undefined
      : issueRoute.key;
  // An ask or comment on a project document shows only its own record, and a block its own text.
  const document =
    route?.kind === "document" && route.item === undefined && blockReference === undefined
      ? route
      : undefined;
  const agentArtifact = route?.kind === "agent-artifact" ? route : undefined;
  const message = record?.kind === "message" ? record : undefined;
  const version =
    route?.kind === "artifact" || route?.kind === "document" || route?.kind === "agent-artifact"
      ? route.version
      : undefined;
  const askId =
    record?.kind === "ask"
      ? record.id
      : record?.kind === "document" && record.item?.kind === "ask"
        ? record.item.id
        : undefined;
  const commentId =
    record?.kind === "comment"
      ? record.id
      : record?.kind === "document" && record.item?.kind === "comment"
        ? record.item.id
        : undefined;
  const issue = useQuery({
    ...issueSummaryQuery(summaryKey),
    enabled: summaryKey !== undefined,
  });
  const messageQueryResult = useQuery({
    ...messageQuery(message?.key, message?.id),
    enabled: message !== undefined,
  });
  const projectDocument = useQuery({
    ...documentSummaryQuery(document?.project, document?.slug),
    enabled: document !== undefined,
  });
  const sessionArtifact = useQuery({
    ...agentArtifactQuery(agentArtifact?.session, agentArtifact?.slug),
    enabled: agentArtifact !== undefined,
    select: agentReferenceArtifact,
  });
  const block = useQuery({
    ...blockQuery(blockReference),
    enabled: blockReference !== undefined,
  });
  const artifact: ReferenceArtifact | undefined =
    document !== undefined
      ? projectDocument.data
      : agentArtifact !== undefined
        ? sessionArtifact.data
        : issue.data?.documents.find(
            (candidate) => route?.kind === "artifact" && candidate.slug === route.slug
          );
  const text = useQuery({
    ...artifactTextQuery(artifact?.id, version),
    enabled: options.text && artifact?.kind === "doc",
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
    // TanStack keeps a query's last data through a failed refetch, so a link or card open when an
    // edit deletes its block would go on showing text its document or item no longer holds. The
    // read answers 404 `NOT_FOUND` for that block, and the link and card then read as a freshly
    // loaded page reads the same link. A pinned reference to a removed block answers 200 with the
    // pin's `removed` state and keeps its pinned text; any other failure (a dropped connection, a
    // 5xx) leaves the last data shown.
    block:
      block.error instanceof ApiError &&
      block.error.status === 404 &&
      block.error.code === "NOT_FOUND"
        ? undefined
        : block.data,
  };
}

/** The document a block reference names, as the hover card shows it: the spec or the issue
 *  document from the issue's summary, or the project document's summary. Undefined until read, and
 *  for a route that names no block of a document: a block of a comment or message names its item,
 *  never a document, so it reads no summary. */
export function useBlockDocument(
  route: DispatchReferenceRoute | undefined
): ReferenceArtifact | undefined {
  const named =
    route === undefined || blockReferenceOf(route) === undefined || itemOfBlock(route) !== undefined
      ? undefined
      : route;
  const project = named?.kind === "document" ? named : undefined;
  const issueKey =
    named !== undefined && named.kind !== "document" && "key" in named ? named.key : undefined;
  const issue = useQuery({ ...issueSummaryQuery(issueKey), enabled: issueKey !== undefined });
  const projectDocument = useQuery({
    ...documentSummaryQuery(project?.project, project?.slug),
    enabled: project !== undefined,
  });
  switch (named?.kind) {
    case "document":
      return projectDocument.data;
    case "spec":
      return summarySpec(issue.data);
    case "artifact":
      return issue.data?.documents.find((candidate) => candidate.slug === named.slug);
    default:
      return undefined;
  }
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
 * Markdown, for the caller to render formatted: a message's body, a document's text (read only
 * with `options.text`), and an ask's or comment's text where the title had to cut it (a title
 * that holds the whole text needs no second copy under it); an issue with no document falls back
 * to its status. An ask or comment never borrows its issue's status or its document's text, while
 * it loads or after. A block of a document resolves to the headline of its own text, as an ask or
 * comment does: the text the pinned version held for a pinned reference, its current text
 * otherwise. A reference to an image artifact, of an issue, a project or an agent's conversation,
 * also answers its picture: the bytes route of the version it pins, else of the latest.
 */
export function useReferenceTarget(
  route: DispatchReferenceRoute | undefined,
  options: ReferenceReadOptions
): ReferenceTarget {
  const { artifact, ask, block, comment, issue, markdown, message } = useReferenceData(
    route,
    options
  );
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
  const pictureVersion = named?.version ?? artifact?.latest_version;
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
): route is Extract<IssueRoute, { kind: "spec" }> & { block: { id: string; pin: string } } {
  return route.kind === "spec" && route.block?.pin !== undefined;
}

/** The SPA path a reference opens (`buildReferencePath`). A block of an issue's spec pinned to a
 *  version opens that version under the spec's own slug, so its path reads the issue's summary
 *  (the query an issue reference's title and the block's card already share); until it is read
 *  it is the live spec at the block. No other reference's path reads anything. */
export function useReferencePath(route: DispatchReferenceRoute): string;
export function useReferencePath(route: DispatchReferenceRoute | undefined): string | undefined;
export function useReferencePath(route: DispatchReferenceRoute | undefined): string | undefined {
  const specKey = route !== undefined && needsSpecSlug(route) ? route.key : undefined;
  const issue = useQuery({ ...issueSummaryQuery(specKey), enabled: specKey !== undefined });
  return route === undefined ? undefined : buildReferencePath(route, summarySpec(issue.data)?.slug);
}

/** `useReferencePath`'s answer for a click outside a render: it waits for the issue summary a
 *  pinned block of a spec needs, and opens the live spec at the block when the summary cannot be
 *  read, as the hook's path does before it is. */
export async function fetchReferencePath(
  queryClient: QueryClient,
  route: DispatchReferenceRoute
): Promise<string> {
  if (!needsSpecSlug(route)) {
    return buildReferencePath(route);
  }
  const issue = await queryClient.fetchQuery(issueSummaryQuery(route.key)).catch(() => undefined);
  return buildReferencePath(route, summarySpec(issue)?.slug);
}
