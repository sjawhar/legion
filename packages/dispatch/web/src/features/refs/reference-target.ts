import { type QueryClient, queryOptions, useQuery } from "@tanstack/react-query";

import { api, artifactVersionPath } from "../../api/client";
import { primarySpec } from "../../api/issue-cache";
import type {
  Artifact,
  ArtifactText,
  ArtifactVersionContent,
  AskRead,
  CommentRead,
  IssueReferenceSummary,
  IssueReferenceSummaryDocument,
  MessageRead,
} from "../../api/types";
import { useMarkdownHeadline } from "./markdown-engine";
import { type DispatchReferenceRoute, referencedArtifact, referenceTargetKind } from "./routes";

/**
 * The queries behind a `dispatch://` reference's target, shared by every surface that resolves
 * one: the inline `RefLink` inside a rendered body, the `Unfurl` card under a bare-reference
 * body, and the `RefPreview` hover card. One module, so they all draw from one fetch path and
 * one cache key per record.
 *
 * A reference to an issue or a document reads what it shows through a summary read, never the
 * full issue: an issue and its documents share `issueSummaryQuery`, a project document has
 * `documentSummaryQuery`. A page can link a hundred of them, so a summary is fetched again only by
 * an event that can change it (`staleTime: Infinity`, no refetch on window focus); the event
 * refresh rules name these keys. An ask, comment or message reference reads its own record alone.
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

/** What a reference to an issue, or to one of its documents, shows: the issue's title and status
 *  and each document's name, kind and latest version (`GET /issues/{key}/summary`). Fetched again
 *  only by an event that can change it. */
export const issueSummaryQuery = (key: string | undefined) =>
  queryOptions({
    queryKey: ["issue-summary", key],
    queryFn: () => api.getIssueSummary(key ?? ""),
    staleTime: Number.POSITIVE_INFINITY,
    refetchOnWindowFocus: false,
  });

/** What a reference to a project document shows: its name, kind and latest version
 *  (`GET /projects/{key}/artifacts/{slug}/summary`). Fetched again only by an event that can
 *  change it. */
export const documentSummaryQuery = (project: string | undefined, slug: string | undefined) =>
  queryOptions({
    queryKey: ["doc-summary", project, slug],
    queryFn: () => api.getProjectDocumentSummary(project ?? "", slug ?? ""),
    staleTime: Number.POSITIVE_INFINITY,
    refetchOnWindowFocus: false,
  });

/** The full issue, under the key the issue page reads it by. Of the reference surfaces only the
 *  hover card's issue view reads it (priority, labels, open asks, spec), while it is shown. */
export const issueQuery = (key: string | undefined) =>
  queryOptions({
    queryKey: ["issue", key],
    queryFn: () => api.getIssue(key ?? ""),
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

/** The reference's target records, each present once its query resolved. `issue` is the issue
 * summary behind an issue or issue-document reference; `artifact` is the document a
 * document/artifact reference names (never the owning issue's primary spec); `markdown` is that
 * document's text at the referenced version, read only when the caller asked for it. */
export interface ReferenceData {
  readonly issue: IssueReferenceSummary | undefined;
  readonly artifact: ReferenceArtifact | undefined;
  readonly markdown: string | undefined;
  readonly ask: AskRead | undefined;
  readonly comment: CommentRead | undefined;
  readonly message: MessageRead | undefined;
}

export interface ReferenceReadOptions {
  /** Whether to read the named document's text into `markdown`. The Unfurl card and the hover
   *  card show it; an inline link shows only a title and reads none. */
  readonly text: boolean;
}

/** Warms every query the hover card will read for route, so a card mounted after the hover
 * delay renders populated instead of in its loading form: an issue's full read and its primary
 * spec text (the card's issue view, which no link reads); a document's summary, then its text once
 * its id is known; an ask's, comment's or message's own read. Every key comes from the query
 * builders above. */
export function prefetchReference(queryClient: QueryClient, route: DispatchReferenceRoute): void {
  const warmText = (
    artifact: Pick<ReferenceArtifact, "id" | "kind"> | undefined,
    version: number | undefined
  ) => {
    if (artifact?.kind === "doc") {
      void queryClient.prefetchQuery(artifactTextQuery(artifact.id, version));
    }
  };
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
  const issueRoute =
    route === undefined || route.kind === "agent-artifact" || route.kind === "document"
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
  // An ask or comment on a project document shows only its own record.
  const document = route?.kind === "document" && route.item === undefined ? route : undefined;
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
  };
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
 * it loads or after. A reference to an image artifact, of an issue, a project or an agent's
 * conversation, also answers its picture: the bytes route of the version it pins, else of the
 * latest.
 */
export function useReferenceTarget(
  route: DispatchReferenceRoute | undefined,
  options: ReferenceReadOptions
): ReferenceTarget {
  const { artifact, ask, comment, issue, markdown, message } = useReferenceData(route, options);
  const kind = route === undefined ? undefined : referenceTargetKind(route);
  const authored =
    kind === "ask" ? ask?.ask.question : kind === "comment" ? comment?.comment.body : undefined;
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
  if (kind === "ask" || kind === "comment") {
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
