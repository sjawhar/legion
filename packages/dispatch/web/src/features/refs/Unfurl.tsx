import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import type { ArtifactText, ArtifactVersionContent } from "../../api/types";

import { type ComposerReference, composerReferences } from "../margin/Composer";
import { parseDispatchReference } from "./routes";

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
  const key = route?.key;
  const version = route?.kind === "artifact" ? route.version : undefined;
  const issue = useQuery({
    enabled: key !== undefined,
    queryKey: ["issue", key],
    queryFn: () => api.getIssue(key ?? ""),
  });
  const artifact = issue.data?.artifacts?.find(
    (candidate) => route?.kind === "artifact" && candidate.slug === route.slug
  );
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

  return (
    <a
      className="block rounded-lg border border-slate-200 bg-slate-50 px-3 py-2 text-sm hover:border-sky-400"
      href={reference.href}
    >
      <span className="block font-medium text-sky-800">{title ?? reference.reference}</span>
      {description === undefined ? null : (
        <span className="mt-1 block text-slate-600">{description}</span>
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
      className="block rounded-lg border border-slate-200 bg-slate-50 px-3 py-2 text-sm hover:border-sky-400"
      href={href}
      rel="noreferrer"
      target="_blank"
    >
      <span className="block font-medium text-sky-800">{issue.data?.title ?? href}</span>
      {excerpt(issue.data?.body) === undefined ? null : (
        <span className="mt-1 block text-slate-600">{excerpt(issue.data?.body)}</span>
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
