import { type QueryClient, queryOptions, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo } from "react";

import { ApiError, api, artifactVersionPath, type ReferenceSource } from "../../api/client";
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
import { type MarkdownHeadline, useMarkdownHeadline } from "./markdown-engine";
import {
  type DispatchReferenceRoute,
  isProjectRoute,
  referencedArtifact,
  referenceTargetKind,
} from "./routes";

/**
 * The queries behind a `dispatch://` reference's target, shared by every surface that resolves
 * one: the inline `RefLink` inside a rendered body, the `Unfurl` card under a bare-reference
 * body, and the `RefPreview` hover card. One module, so they all draw from one fetch path and
 * one cache key per record.
 */

export interface ReferenceTarget {
  /** The target's title as one line of words, which is also its link's accessible name: an
   *  issue's key, a space and its title (`CORE-12 Fix the thing`); a document's name; an ask's,
   *  comment's or message's own text cut to a headline. `undefined` while loading and for a
   *  target that does not exist, where a surface shows the reference as its author wrote it. */
  readonly title: string | undefined;
  /** The issue key `title` begins with, which a surface draws in semibold as an issue list row
   *  does (`features/project/IssueList.tsx`); `undefined` for any other target. */
  readonly titleKey: string | undefined;
  /** What the target says, as its author wrote it (Markdown): a message's body, a document's
   *  text. `undefined` while loading or for a target with nothing to say. */
  readonly description: string | undefined;
  /** The bytes route of the picture a reference to an image artifact names - the version it pins,
   *  else the latest - for a thumbnail beside the title; undefined for anything else. */
  readonly picture: string | undefined;
}

/** How much of a target's own text becomes a reference link's title: an ask's question, a
 *  comment's or message's body. Long enough to recognise, short enough for a link inside a
 *  sentence. */
const TITLE_HEADLINE_MAX = 60;

/**
 * The retry rule of the queries a reference resolves through: the client's own (the app's shared
 * rule, `isRetryableQueryError` as `main.tsx` sets it), except that a 404 is final. A reference to
 * an issue, ask, comment or message that does not exist (`dispatch://CORE-999999`, a key-shaped
 * word such as `UTF-8` written bare) is asked about once, so a body full of such words costs one
 * request each and its written text shows at once. Set in each reference query's own options, so
 * a page that reads the same record keeps the shared rule.
 */
export function referenceRetry(
  queryClient: QueryClient
): (failureCount: number, error: Error) => boolean {
  const shared = queryClient.getDefaultOptions().queries?.retry;
  return (failureCount, error) => {
    if (error instanceof ApiError && error.status === 404) {
      return false;
    }
    if (typeof shared === "function") {
      return shared(failureCount, error);
    }
    // TanStack's own default, three retries in a browser, when the client sets none.
    return typeof shared === "number" ? failureCount < shared : (shared ?? failureCount < 3);
  };
}

/** Whether an issue query asks again on a remount, a refocus or a reconnect, as the app's defaults
 *  have it: not once it answered 404, which only the event stream's `issue.created` (`api/sse.ts`,
 *  which refetches every issue query) can change. */
function asksAgain(query: { state: { error: Error | null } }): boolean {
  return !(query.state.error instanceof ApiError && query.state.error.status === 404);
}

/** One issue with its documents, as the issue page reads it: a reference's title, and whether a
 *  key written bare names an issue that exists (`useRenderedMarkdown`). An issue that does not
 *  exist is asked about once (`asksAgain`, with `referenceRetry` in each reader's options), and a
 *  key written before its issue exists links once `issue.created` refetches it. */
export const issueQuery = (key: string | undefined) =>
  queryOptions({
    queryKey: ["issue", key],
    queryFn: () => api.getIssue(key ?? ""),
    refetchOnReconnect: asksAgain,
    refetchOnWindowFocus: asksAgain,
    retryOnMount: asksAgain,
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

/**
 * The question map a `#id` in source's text resolves against: the document the text belongs to and
 * its question blocks, each to its ask (`GET /api/v1/references/questions`). Keyed under
 * `["reference-questions"]`, which the event stream refreshes only on the writes that can change a
 * document's question map or move a text to another document (`changesQuestionMap` in
 * `api/sse.ts`): a document's creation or version, which can add or retire a block, `ask.opened`,
 * which settlement writes as it makes a block's ask, an ask's edit, and an ask's or comment's anchor
 * refresh. A new message or comment leaves every mounted map alone.
 */
export const referenceQuestionsQuery = (source: ReferenceSource | undefined) =>
  queryOptions({
    queryKey: ["reference-questions", source?.kind, source?.id],
    queryFn: () => {
      if (source === undefined) {
        throw new Error("A question map needs the text it is read for.");
      }
      return api.getReferenceQuestions(source);
    },
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
  if (route.kind === "agent-artifact") {
    void queryClient.prefetchQuery(agentArtifactQuery(route.session, route.slug));
    return;
  }
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

/** Which record of each kind a reference reads, `undefined` for each it does not. */
interface ReferenceRecords {
  readonly issueKey: string | undefined;
  readonly document: Extract<DispatchReferenceRoute, { kind: "document" }> | undefined;
  readonly agentArtifact: Extract<DispatchReferenceRoute, { kind: "agent-artifact" }> | undefined;
  readonly message: Extract<DispatchReferenceRoute, { kind: "message" }> | undefined;
  readonly version: number | undefined;
  readonly askId: string | undefined;
  readonly commentId: string | undefined;
}

function referenceRecords(route: DispatchReferenceRoute | undefined): ReferenceRecords {
  return {
    issueKey:
      route === undefined || route.kind === "agent-artifact" || isProjectRoute(route)
        ? undefined
        : route.key,
    document: route?.kind === "document" ? route : undefined,
    agentArtifact: route?.kind === "agent-artifact" ? route : undefined,
    message: route?.kind === "message" ? route : undefined,
    version:
      route?.kind === "artifact" || route?.kind === "document" || route?.kind === "agent-artifact"
        ? route.version
        : undefined,
    askId:
      route?.kind === "ask"
        ? route.id
        : route?.kind === "document" && route.item?.kind === "ask"
          ? route.item.id
          : undefined,
    commentId:
      route?.kind === "comment"
        ? route.id
        : route?.kind === "document" && route.item?.kind === "comment"
          ? route.item.id
          : undefined,
  };
}

/** The artifact a reference names, from whichever owner's record holds it. */
function namedArtifact(
  route: DispatchReferenceRoute | undefined,
  records: ReferenceRecords,
  issue: IssueDetails | undefined,
  projectArtifact: Artifact | undefined,
  sessionArtifact: Artifact | undefined
): Artifact | undefined {
  if (records.document !== undefined) {
    return projectArtifact;
  }
  if (records.agentArtifact !== undefined) {
    return sessionArtifact;
  }
  return issue?.artifacts.find(
    (candidate) => route?.kind === "artifact" && candidate.slug === route.slug
  );
}

function documentMarkdown(text: ArtifactText | ArtifactVersionContent | undefined) {
  return text !== undefined && "markdown" in text ? text.markdown : undefined;
}

export function useReferenceData(route: DispatchReferenceRoute | undefined): ReferenceData {
  const records = referenceRecords(route);
  const { agentArtifact, askId, commentId, document, issueKey, message, version } = records;
  const retry = referenceRetry(useQueryClient());
  const issue = useQuery({
    ...issueQuery(issueKey),
    enabled: issueKey !== undefined && message === undefined,
    retry,
  });
  const messageQueryResult = useQuery({
    ...messageQuery(message?.key, message?.id),
    enabled: message !== undefined,
    retry,
  });
  const projectArtifact = useQuery({
    ...projectArtifactQuery(document?.project, document?.slug),
    enabled: document !== undefined,
  });
  const sessionArtifact = useQuery({
    ...agentArtifactQuery(agentArtifact?.session, agentArtifact?.slug),
    enabled: agentArtifact !== undefined,
  });
  const artifact = namedArtifact(
    route,
    records,
    issue.data,
    projectArtifact.data,
    sessionArtifact.data
  );
  const text = useQuery({
    ...artifactTextQuery(artifact?.id, version),
    enabled: artifact?.kind === "doc",
  });
  const ask = useQuery({
    ...askQuery(askId),
    enabled: askId !== undefined,
    retry,
  });
  const comment = useQuery({
    ...commentQuery(commentId),
    enabled: commentId !== undefined,
    retry,
  });
  return {
    issue: issue.data,
    artifact,
    markdown: documentMarkdown(text.data),
    ask: ask.data,
    comment: comment.data,
    message: messageQueryResult.data,
  };
}

/** The text a reference's title is a headline of: an ask's question, a comment's or a message's
 *  body; `undefined` for any other target and while it loads. */
function authoredText(route: DispatchReferenceRoute, data: ReferenceData): string | undefined {
  switch (referenceTargetKind(route)) {
    case "ask":
      return data.ask?.ask.question;
    case "comment":
      return data.comment?.comment.body;
    case "message":
      return data.message?.message.body;
    default:
      return undefined;
  }
}

/** The one title rule, given the target's records and the headline of its authored text. */
function referenceTargetOf(
  route: DispatchReferenceRoute,
  data: ReferenceData,
  headline: MarkdownHeadline | undefined
): ReferenceTarget {
  const kind = referenceTargetKind(route);
  if (kind === "ask" || kind === "comment" || kind === "message") {
    return {
      title: headline?.text,
      titleKey: undefined,
      description: headline?.cut === true ? authoredText(route, data) : undefined,
      picture: undefined,
    };
  }
  if (kind === "issue") {
    const key = data.issue === undefined || !("key" in route) ? undefined : route.key;
    return {
      title: key === undefined ? undefined : `${key} ${data.issue?.title}`,
      titleKey: key,
      description: data.issue?.status,
      picture: undefined,
    };
  }
  const { artifact, markdown } = data;
  const named = referencedArtifact(route);
  const pictureVersion =
    named?.version ?? artifact?.versions.reduce((latest, item) => Math.max(latest, item.number), 0);
  return {
    title: artifact?.name,
    titleKey: undefined,
    description: markdown !== undefined && markdown.trim() !== "" ? markdown : undefined,
    picture:
      named === undefined || artifact?.kind !== "image" || !pictureVersion
        ? undefined
        : artifactVersionPath(named.owner, named.slug, pictureVersion),
  };
}

const NO_TARGET: ReferenceTarget = {
  description: undefined,
  picture: undefined,
  title: undefined,
  titleKey: undefined,
};

/**
 * Every query behind a resolved reference title, shared by the Unfurl card, the inline `RefLink`
 * markdown/document rendering and the document editor's reference titles, so all draw from one
 * fetch path by one title rule. An issue reads as its key then its title (`titleKey` names the
 * key, as `features/project/IssueList.tsx` sets it in semibold); a document as its name; an ask,
 * comment or message (whether issue-scoped or nested under a project document's `item`) as its own
 * text rather than the owning issue's title or document. A title drawn from text its author wrote
 * (the question, the comment, the message) is that Markdown projected to plain words and cut
 * (`useMarkdownHeadline`): a link's text can hold no formatting, so a `**Blocking:**` question
 * titles its link `Blocking: …`, never `**Blocking:** …`. The description is the target's own
 * Markdown, for the caller to render formatted: a document's text, and an ask's, comment's or
 * message's text where the title had to cut it (a title that holds the whole text needs no second
 * copy under it); an issue's is its status. An ask, comment or message never borrows its issue's
 * status or its document's text, while it loads or after, and a target that does not exist (a 404,
 * final in these queries) has no title. A reference to an image artifact, of an issue, a project or
 * an agent's conversation, also answers its picture: the bytes route of the version it pins, else
 * of the latest.
 */
export function useReferenceTarget(route: DispatchReferenceRoute | undefined): ReferenceTarget {
  return useReferenceTargetOf(route, useReferenceData(route));
}

/** `useReferenceTarget` for a caller that already reads route's records (`useReferenceData`), so
 *  it subscribes to them once. */
export function useReferenceTargetOf(
  route: DispatchReferenceRoute | undefined,
  data: ReferenceData
): ReferenceTarget {
  const authored = route === undefined ? undefined : authoredText(route, data);
  const headline = useMarkdownHeadline(authored, TITLE_HEADLINE_MAX);
  return route === undefined ? NO_TARGET : referenceTargetOf(route, data, headline);
}

/** The question map of source's document (`referenceQuestionsQuery`) as each block id to its ask's
 *  `dispatch://` reference, asked only while enabled and empty until it answers; a 404 is final
 *  (`referenceRetry`). One value until the answer or enabled changes, so a reader may memoise on
 *  it. */
export function useQuestionMap(
  source: ReferenceSource | undefined,
  enabled: boolean
): ReadonlyMap<string, string> {
  const retry = referenceRetry(useQueryClient());
  const { data } = useQuery({ ...referenceQuestionsQuery(source), enabled, retry });
  const questions = data?.questions;
  return useMemo(
    () => new Map(enabled ? questions?.map((question) => [question.block_id, question.ask]) : []),
    [enabled, questions]
  );
}
