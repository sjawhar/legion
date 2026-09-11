import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import type { ArtifactText, ArtifactVersionContent } from "../../api/types";
import {
  borderDefault,
  cardHoverBorder,
  linkText,
  surfaceMutedBg,
  textSecondaryOnSurface,
} from "../../theme/classes";

import { type ComposerReference, composerReferences } from "../margin/Composer";
import { isProjectRoute, parseDispatchReference } from "./routes";

interface UnfurlProps {
  body: string;
}

interface GitHubIssue {
  body?: string | null;
  title?: string;
}

function excerpt(markdown: string | null | undefined): string | undefined {
  const text = markdown?.replace(/\s+/g, " ").trim();
  return text === undefined || text.length === 0 ? undefined : text.slice(0, 160);
}

function DispatchUnfurl({ reference }: { reference: ComposerReference }): ReactNode {
  const route = parseDispatchReference(reference.reference);
  const issueKey = route === undefined || isProjectRoute(route) ? undefined : route.key;
  const document = route?.kind === "document" ? route : undefined;
  const message = route?.kind === "message" ? route : undefined;
  const version =
    route?.kind === "artifact" || route?.kind === "document" ? route.version : undefined;
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
    queryKey: ["project", document?.project, "artifact", document?.slug],
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
  const title = artifact === undefined ? issue.data?.title : artifact.name;
  const markdown =
    text.data !== undefined && "markdown" in text.data ? text.data.markdown : undefined;
  const description =
    excerpt(markdown) ?? (artifact === undefined ? issue.data?.status : undefined);

  if (message !== undefined) {
    const firstLine = messageQuery.data?.message.body.split("\n")[0];
    return (
      <a
        className={`block rounded-lg border px-3 py-2 text-sm ${surfaceMutedBg} ${borderDefault} ${cardHoverBorder}`}
        href={reference.href}
      >
        <span className={`block font-medium ${linkText}`}>
          {messageQuery.data === undefined
            ? reference.reference
            : `${messageQuery.data.message.author.kind} ${messageQuery.data.message.author.id}`}
        </span>
        {firstLine === undefined ? null : (
          <span className={`mt-1 block ${textSecondaryOnSurface}`}>{firstLine}</span>
        )}
      </a>
    );
  }

  if (document !== undefined) {
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
