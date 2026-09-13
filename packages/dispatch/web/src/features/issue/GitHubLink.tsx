import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { ApiError, api } from "../../api/client";
import type { ExternalLink } from "../../api/types";
import { badgeLow, linkHoverText, linkText, textMutedOnSurface } from "../../theme/classes";

function safeExternalHref(value: string): string | undefined {
  try {
    const protocol = new URL(value).protocol;
    return protocol === "http:" || protocol === "https:" ? value : undefined;
  } catch {
    return undefined;
  }
}

interface GitHubReference {
  head?: { sha?: string };
  merged?: boolean;
  state: string;
  title: string;
}

/**
 * One external link on the issue header's details line. A GitHub issue or pull request is
 * shown as its number and title (the repository while the title is unknown — loading, or no
 * GitHub App credentials for this sign-in), never as the raw address, so it fits the line on a
 * phone; the title is truncated and carried in full by the link's tooltip.
 */
export function GitHubLink({ link }: { link: ExternalLink }): ReactNode {
  const href = safeExternalHref(link.url);
  const match =
    href?.match(/^https:\/\/github\.com\/([^/]+)\/([^/]+)\/(issues|pull)\/(\d+)\/?$/) ?? null;
  const isPullRequest = match?.[3] === "pull";
  const githubReference = useQuery({
    enabled: match !== null,
    queryKey: ["github-link", link.url],
    queryFn: async () => {
      const response = await api.githubRest(
        `repos/${match?.[1]}/${match?.[2]}/${isPullRequest ? "pulls" : "issues"}/${match?.[4]}`
      );
      return (await response.json()) as GitHubReference;
    },
    retry: false,
  });
  const headSHA = isPullRequest ? githubReference.data?.head?.sha : undefined;
  const checks = useQuery({
    enabled: headSHA !== undefined,
    queryKey: ["github-link-checks", link.url, headSHA],
    queryFn: async () => {
      const response = await api.githubRest(
        `repos/${match?.[1]}/${match?.[2]}/commits/${headSHA}/check-runs`
      );
      const { check_runs: checkRuns } = (await response.json()) as {
        check_runs: { conclusion: string | null; status: string }[];
      };
      if (
        checkRuns.some(({ conclusion }) =>
          ["action_required", "cancelled", "failure", "timed_out"].includes(conclusion ?? "")
        )
      ) {
        return "failure";
      }
      if (
        checkRuns.some(({ conclusion, status }) => conclusion === null || status !== "completed")
      ) {
        return "pending";
      }
      return "success";
    },
    retry: false,
  });

  if (href === undefined) {
    return (
      <span
        className={`inline-block max-w-[18ch] truncate text-sm sm:max-w-[32ch] ${textMutedOnSurface}`}
        title="Unsafe external link"
      >
        {link.url}
      </span>
    );
  }
  if (match === null) {
    return (
      <a
        className={`inline-block max-w-[18ch] truncate text-sm underline sm:max-w-[32ch] ${linkText} ${linkHoverText}`}
        href={href}
        title={link.url}
      >
        {link.url}
      </a>
    );
  }
  const repository = `${match[1]}/${match[2]}`;
  const number = `#${match[4]}`;
  // The reference (title, state) and the check-runs are separate lookups: a failed check-runs
  // read only drops the checks pill and never the title and state already loaded.
  const reference = githubReference.isError ? undefined : githubReference.data;
  const unavailable =
    reference === undefined &&
    githubReference.error instanceof ApiError &&
    githubReference.error.code === "GITHUB_TOKEN_UNAVAILABLE";
  const state =
    reference === undefined
      ? undefined
      : isPullRequest && reference.merged
        ? "merged"
        : reference.state;
  return (
    <a
      className={`inline-flex max-w-full items-center gap-2 text-sm underline ${linkText} ${linkHoverText}`}
      href={href}
      title={
        unavailable
          ? "GitHub details are unavailable for this sign-in."
          : reference === undefined
            ? link.url
            : `${repository}${number}: ${reference.title}`
      }
    >
      <span className="shrink-0 font-medium">{number}</span>
      <span className="max-w-[18ch] truncate sm:max-w-[32ch]">
        {reference === undefined ? repository : reference.title}
      </span>
      {state === undefined ? null : (
        <span
          className={`shrink-0 rounded-full px-2 py-0.5 text-xs ${badgeLow.bg} ${badgeLow.text}`}
        >
          {state}
        </span>
      )}
      {isPullRequest && !checks.isError && checks.data !== undefined ? (
        <span
          className={`shrink-0 rounded-full px-2 py-0.5 text-xs ${badgeLow.bg} ${badgeLow.text}`}
        >
          checks: {checks.data}
        </span>
      ) : null}
    </a>
  );
}
