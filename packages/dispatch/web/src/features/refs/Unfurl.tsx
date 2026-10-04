import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import {
  borderDefault,
  cardHoverBorder,
  linkText,
  surfaceMutedBg,
  textSecondaryOnSurface,
} from "../../theme/classes";

import { MarkdownPreview } from "./MarkdownPreview";
import { useReferenceTarget } from "./reference-target";
import {
  type ComposerReference,
  composerReferences,
  parseDispatchReference,
  referenceSpans,
} from "./routes";

interface UnfurlProps {
  body: string;
}

interface GitHubIssue {
  body?: string | null;
  title?: string;
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
        <MarkdownPreview
          className={`mt-1 ${textSecondaryOnSurface}`}
          lines={2}
          markdown={description}
        />
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
      {issue.data?.body === undefined ||
      issue.data.body === null ||
      issue.data.body.trim() === "" ? null : (
        <MarkdownPreview
          className={`mt-1 ${textSecondaryOnSurface}`}
          lines={2}
          markdown={issue.data.body}
        />
      )}
    </a>
  );
}

// Each word of the body a reference, with nothing between words but whitespace. A word is read
// once: `\S+` and `\s+` share no character, so no body splits into words more than one way.
const bareReferenceBodyPattern = /^(?:dispatch|https?):\/\/\S+(?:\s+(?:dispatch|https?):\/\/\S+)*$/;

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
  const github = referenceSpans(body, "https://github.com/").flatMap(({ value }) => {
    const path = githubReference(value);
    return path === undefined ? [] : [{ href: value, path }];
  });

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
