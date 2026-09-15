import { type QueryClient, queryOptions, useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

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
import {
  borderDefault,
  cardHoverBorder,
  linkText,
  surfaceMutedBg,
  textSecondaryOnSurface,
} from "../../theme/classes";

import { type ComposerReference, composerReferences } from "../margin/Composer";
import {
  type DispatchReferenceRoute,
  isProjectRoute,
  parseDispatchReference,
  referenceTargetKind,
} from "./routes";

interface UnfurlProps {
  body: string;
}

interface GitHubIssue {
  body?: string | null;
  title?: string;
}

export interface ReferenceTarget {
  readonly title: string | undefined;
  readonly description: string | undefined;
}

export function excerpt(markdown: string | null | undefined, max = 160): string | undefined {
  const text = markdown?.replace(/\s+/g, " ").trim();
  return text === undefined || text.length === 0 ? undefined : text.slice(0, max);
}

function truncate(text: string, max: number): string | undefined {
  const trimmed = text.trim();
  if (trimmed === "") {
    return undefined;
  }
  return trimmed.length > max ? `${trimmed.slice(0, max)}…` : trimmed;
}

export function firstLine(text: string): string | undefined {
  const trimmed = text.trim();
  if (trimmed === "") {
    return undefined;
  }
  const newline = trimmed.indexOf("\n");
  return newline === -1 ? trimmed : trimmed.slice(0, newline).trim();
}

const issueQuery = (key: string | undefined) =>
  queryOptions({
    queryKey: ["issue", key],
    queryFn: () => api.getIssue(key ?? ""),
  });

const messageQuery = (key: string | undefined, id: string | undefined) =>
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

const askQuery = (id: string | undefined) =>
  queryOptions({
    queryKey: ["ask", id],
    queryFn: () => api.getAsk(id ?? ""),
  });

const commentQuery = (id: string | undefined) =>
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
 * artifact name (a document reference) or the issue title.
 */
export function useReferenceTarget(route: DispatchReferenceRoute | undefined): ReferenceTarget {
  const { artifact, ask, comment, issue, markdown, message } = useReferenceData(route);
  const kind = route === undefined ? undefined : referenceTargetKind(route);
  const title =
    kind === "message"
      ? message === undefined
        ? undefined
        : `${message.message.author.kind} ${message.message.author.id}`
      : kind === "ask"
        ? ask === undefined
          ? undefined
          : truncate(ask.ask.question, 60)
        : kind === "comment"
          ? comment === undefined
            ? undefined
            : firstLine(comment.comment.body)
          : (artifact?.name ?? issue?.title);
  const description =
    kind === "message"
      ? message === undefined
        ? undefined
        : firstLine(message.message.body)
      : (excerpt(markdown) ?? (artifact === undefined ? issue?.status : undefined));

  return { title, description };
}

function DispatchUnfurl({ reference }: { reference: ComposerReference }): ReactNode {
  const route = parseDispatchReference(reference.reference);
  const { title, description } = useReferenceTarget(route);

  return (
    <a
      className={`block rounded-lg border px-3 py-2 text-sm ${surfaceMutedBg} ${borderDefault} ${cardHoverBorder}`}
      href={reference.href}
    >
      <span className={`block font-medium ${linkText}`}>{title ?? reference.reference}</span>
      {description === undefined ? null : (
        <span className={`mt-1 block ${textSecondaryOnSurface}`}>{description}</span>
      )}
    </a>
  );
}

function githubReference(value: string): string | undefined {
  try {
    const url = new URL(value);
    if (url.hostname !== "github.com") {
      return undefined;
    }
    const match = url.pathname.match(/^\/([^/]+)\/([^/]+)\/(?:issues|pull)\/(\d+)$/);
    return match === null ? undefined : `/repos/${match[1]}/${match[2]}/issues/${match[3]}`;
  } catch {
    return undefined;
  }
}

function GitHubUnfurl({ href, path }: { href: string; path: string }): ReactNode {
  const issue = useQuery({
    queryKey: ["github", path],
    queryFn: async () => (await api.githubRest(path)).json() as Promise<GitHubIssue>,
  });
  return (
    <a
      className={`block rounded-lg border px-3 py-2 text-sm ${surfaceMutedBg} ${borderDefault} ${cardHoverBorder}`}
      href={href}
      rel="noreferrer"
      target="_blank"
    >
      <span className={`block font-medium ${linkText}`}>{issue.data?.title ?? href}</span>
      {excerpt(issue.data?.body) === undefined ? null : (
        <span className={`mt-1 block ${textSecondaryOnSurface}`}>{excerpt(issue.data?.body)}</span>
      )}
    </a>
  );
}

const bareReferenceBodyPattern = /^(?:(?:dispatch:\/\/|https?:\/\/)\S+\s*)+$/;

/**
 * Whether body is nothing but one or more reference URLs (optionally whitespace-separated) with
 * no surrounding prose. `MarkdownBody` now renders a reference inline with its resolved title, so
 * a caller that already renders body through `MarkdownBody` should keep the `Unfurl` card only for
 * a bare-reference body — otherwise the card duplicates the inline link.
 */
export function isBareReferenceBody(body: string): boolean {
  return bareReferenceBodyPattern.test(body.trim());
}

export function Unfurl({ body }: UnfurlProps): ReactNode {
  const dispatch = composerReferences(body);
  const github = [...body.matchAll(/https:\/\/github\.com\/[^\s<>"]+/g)]
    .map((match) => {
      const href = match[0].replace(/[),.;:!?]+$/, "");
      return { href, path: githubReference(href) };
    })
    .filter(
      (reference): reference is { href: string; path: string } => reference.path !== undefined
    );

  if (dispatch.length === 0 && github.length === 0) {
    return null;
  }
  return (
    <section aria-label="References" className="mt-3 space-y-2">
      {dispatch.map((reference) => (
        <DispatchUnfurl key={reference.reference} reference={reference} />
      ))}
      {github.map((reference) => (
        <GitHubUnfurl href={reference.href} key={reference.href} path={reference.path} />
      ))}
    </section>
  );
}
