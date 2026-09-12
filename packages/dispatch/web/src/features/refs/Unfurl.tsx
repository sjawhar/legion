import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import { queryKeys } from "../../api/query-keys";
import type { ArtifactText, ArtifactVersionContent } from "../../api/types";
import {
  borderDefault,
  cardHoverBorder,
  linkText,
  surfaceMutedBg,
  textSecondaryOnSurface,
} from "../../theme/classes";

import { type ComposerReference, composerReferences } from "../margin/Composer";
import { type DispatchReferenceRoute, isProjectRoute, parseDispatchReference } from "./routes";

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

function excerpt(markdown: string | null | undefined): string | undefined {
  const text = markdown?.replace(/\s+/g, " ").trim();
  return text === undefined || text.length === 0 ? undefined : text.slice(0, 160);
}

function truncate(text: string, max: number): string | undefined {
  const trimmed = text.trim();
  if (trimmed === "") {
    return undefined;
  }
  return trimmed.length > max ? `${trimmed.slice(0, max)}…` : trimmed;
}

function firstLine(text: string): string | undefined {
  const trimmed = text.trim();
  if (trimmed === "") {
    return undefined;
  }
  const newline = trimmed.indexOf("\n");
  return newline === -1 ? trimmed : trimmed.slice(0, newline).trim();
}

/**
 * Every query behind a resolved reference title, shared by the Unfurl card and the inline
 * `RefLink` markdown/document rendering so both draw from one fetch path. An ask or comment
 * (whether issue-scoped or nested under a project document's `item`) resolves to its own
 * question/first line rather than the owning issue's title; everything else falls back to the
 * artifact name (a document reference) or the issue title.
 */
export function useReferenceTarget(route: DispatchReferenceRoute | undefined): ReferenceTarget {
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
    enabled: issueKey !== undefined && message === undefined,
    queryKey: ["issue", issueKey],
    queryFn: () => api.getIssue(issueKey ?? ""),
  });
  const messageQuery = useQuery({
    enabled: message !== undefined,
    queryKey: ["issue", message?.key, "message", message?.id],
    queryFn: () => api.getMessage(message?.key ?? "", message?.id ?? ""),
  });
  const projectArtifact = useQuery({
    enabled: document !== undefined,
    queryKey: queryKeys.projectArtifact(document?.project, document?.slug),
    queryFn: () => api.getProjectArtifact(document?.project ?? "", document?.slug ?? ""),
  });
  const artifact =
    document === undefined
      ? issue.data?.artifacts.find(
          (candidate) => route?.kind === "artifact" && candidate.slug === route.slug
        )
      : projectArtifact.data;
  const text = useQuery<ArtifactText | ArtifactVersionContent>({
    enabled: artifact?.kind === "doc",
    queryKey: ["artifact", artifact?.id, version ?? "text"],
    queryFn: () =>
      version === undefined
        ? api.getArtifactText(artifact?.id ?? "")
        : api.getArtifactVersion(artifact?.id ?? "", version),
  });
  const ask = useQuery({
    enabled: askId !== undefined,
    queryKey: queryKeys.ask(askId),
    queryFn: () => api.getAsk(askId ?? ""),
  });
  const comment = useQuery({
    enabled: commentId !== undefined,
    queryKey: queryKeys.comment(commentId),
    queryFn: () => api.getComment(commentId ?? ""),
  });
  const markdown =
    text.data !== undefined && "markdown" in text.data ? text.data.markdown : undefined;
  const title =
    message !== undefined
      ? messageQuery.data === undefined
        ? undefined
        : `${messageQuery.data.message.author.kind} ${messageQuery.data.message.author.id}`
      : askId !== undefined
        ? ask.data === undefined
          ? undefined
          : truncate(ask.data.ask.question, 60)
        : commentId !== undefined
          ? comment.data === undefined
            ? undefined
            : firstLine(comment.data.comment.body)
          : artifact === undefined
            ? issue.data?.title
            : artifact.name;
  const description =
    message !== undefined
      ? messageQuery.data === undefined
        ? undefined
        : firstLine(messageQuery.data.message.body)
      : (excerpt(markdown) ?? (artifact === undefined ? issue.data?.status : undefined));

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
