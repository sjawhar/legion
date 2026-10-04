import { type QueryClient, queryOptions, useQuery } from "@tanstack/react-query";

import { api } from "../../api/client";
import { primarySpec } from "../../api/issue-cache";
import type {
  Artifact,
  ArtifactText,
  ArtifactVersionContent,
  AskRead,
  CommentRead,
  IssueDetails,
  MessageRead,
} from "../../api/types";
import { useMarkdownHeadline } from "./markdown-engine";
import { type DispatchReferenceRoute, isProjectRoute, referenceTargetKind } from "./routes";

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

/** The reference's target records, each present once its query resolved. `artifact` is the
 * document a document/artifact reference names (never the owning issue's primary spec);
 * `markdown` is that document's text at the referenced version. */
export interface ReferenceData {
  readonly issue: IssueDetails | undefined;
  readonly artifact: Artifact | undefined;
  readonly markdown: string | undefined;
  readonly ask: AskRead | undefined;
  readonly comment: CommentRead | undefined;
  readonly message: MessageRead | undefined;
}

/** Warms every query the hover card will read for route, so a card mounted after the hover
 * delay renders populated instead of in its loading form: `useReferenceData`'s first hop, then
 * the document text behind a document/artifact reference once its artifact id is known, and —
 * for an issue reference — the issue's primary spec text, which only the card's issue view reads
 * (`useReferenceTarget` never fetches it). Every key comes from the query builders above. */
export function prefetchReference(queryClient: QueryClient, route: DispatchReferenceRoute): void {
  if (route.kind === "message") {
    void queryClient.prefetchQuery(messageQuery(route.key, route.id));
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
  const issueKey = route === undefined || isProjectRoute(route) ? undefined : route.key;
  const document = route?.kind === "document" ? route : undefined;
  const message = route?.kind === "message" ? route : undefined;
  const version =
    route?.kind === "artifact" || route?.kind === "document" ? route.version : undefined;
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
  const artifact =
    document === undefined
      ? issue.data?.artifacts.find(
          (candidate) => route?.kind === "artifact" && candidate.slug === route.slug
        )
      : projectArtifact.data;
  const text = useQuery({
    ...artifactTextQuery(artifact?.id, version),
    enabled: artifact?.kind === "doc",
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
 * (whether issue-scoped or nested under a project document's `item`) resolves to its own
 * question/first line rather than the owning issue's title; everything else falls back to the
 * artifact name (a document reference) or the issue title. A title drawn from text its author
 * wrote (the question, the comment) is that Markdown projected to plain words and cut
 * (`useMarkdownHeadline`): a link's text can hold no formatting, so a `**Blocking:**` question
 * titles its link `Blocking: …`, never `**Blocking:** …`. The description is the target's own
 * Markdown, for the caller to render formatted: a message's body, a document's text, and an
 * ask's or comment's text where the title had to cut it (a title that holds the whole text
 * needs no second copy under it); an issue with no document falls back to its status.
 */
export function useReferenceTarget(route: DispatchReferenceRoute | undefined): ReferenceTarget {
  const { artifact, ask, comment, issue, markdown, message } = useReferenceData(route);
  const kind = route === undefined ? undefined : referenceTargetKind(route);
  const authored =
    kind === "ask" ? ask?.ask.question : kind === "comment" ? comment?.comment.body : undefined;
  const headline = useMarkdownHeadline(authored, TITLE_HEADLINE_MAX);
  const title =
    kind === "message"
      ? message === undefined
        ? undefined
        : `${message.message.author.kind} ${message.message.author.id}`
      : kind === "ask" || kind === "comment"
        ? headline
        : (artifact?.name ?? issue?.title);
  const description =
    kind === "message"
      ? message?.message.body
      : authored !== undefined
        ? headline?.endsWith("…") === true
          ? authored
          : undefined
        : markdown !== undefined && markdown.trim() !== ""
          ? markdown
          : artifact === undefined
            ? issue?.status
            : undefined;

  return { title, description };
}
