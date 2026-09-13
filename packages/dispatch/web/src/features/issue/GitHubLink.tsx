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
      <span className={`text-sm ${textMutedOnSurface}`} title="Unsafe external link">
        {link.url}
      </span>
    );
  }
  if (match === null || githubReference.isError || checks.isError) {
    const unavailable =
      (githubReference.error instanceof ApiError &&
        githubReference.error.code === "GITHUB_TOKEN_UNAVAILABLE") ||
      (checks.error instanceof ApiError && checks.error.code === "GITHUB_TOKEN_UNAVAILABLE");
    return (
      <a
        className={`text-sm underline ${linkText} ${linkHoverText}`}
        href={href}
        title={unavailable ? "GitHub details are unavailable for this sign-in." : undefined}
      >
        {link.url}
      </a>
    );
  }
  if (githubReference.data === undefined) {
    return (
      <a className={`text-sm underline ${linkText}`} href={href}>
        {link.url}
      </a>
    );
  }
  const state =
    isPullRequest && githubReference.data.merged ? "merged" : githubReference.data.state;
  return (
    <a
      className={`inline-flex items-center gap-2 text-sm underline ${linkText} ${linkHoverText}`}
      href={href}
    >
      <span>{githubReference.data.title}</span>
      <span className={`rounded-full px-2 py-0.5 text-xs ${badgeLow.bg} ${badgeLow.text}`}>
        {state}
      </span>
      {isPullRequest && checks.data !== undefined ? (
        <span className={`rounded-full px-2 py-0.5 text-xs ${badgeLow.bg} ${badgeLow.text}`}>
          checks: {checks.data}
        </span>
      ) : null}
    </a>
  );
}
