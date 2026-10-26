import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { ApiError, api } from "../../api/client";
import type { ExternalLink } from "../../api/types";
import { TruncatedText } from "../../components/TruncatedText";
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

interface CheckRun {
  app?: { id: number } | null;
  check_suite?: { id: number } | null;
  conclusion: string | null;
  id: number;
  name: string;
  status: string;
}

interface WorkflowRun {
  check_suite_id: number;
  event: string;
  name: string;
}

const pageSize = 100;

/** Every item of a paged GitHub list, read a page of `pageSize` at a time until a short page. */
async function readEveryPage<T>(path: string, field: string): Promise<T[]> {
  const items: T[] = [];
  for (let page = 1; ; page += 1) {
    const separator = path.includes("?") ? "&" : "?";
    const response = await api.githubRest(`${path}${separator}per_page=${pageSize}&page=${page}`);
    const pageItems = ((await response.json()) as Record<string, unknown>)[field];
    if (!Array.isArray(pageItems)) {
      throw new Error(`GitHub answered ${path} with no ${field} list`);
    }
    items.push(...(pageItems as T[]));
    if (pageItems.length < pageSize) return items;
  }
}

/**
 * The check runs GitHub's merge reads: for each check, only its newest run (the highest id). A
 * check is its name, workflow name and triggering event when its check suite is a workflow run's,
 * as `gh pr checks` keys it, so a run a newer run of the same event replaced no longer counts
 * while a failed run of one event still blocks beside a passing run of another; any other check
 * run (an external App's) is its name and App.
 */
function newestCheckRuns(checkRuns: CheckRun[], workflowRuns: WorkflowRun[]): CheckRun[] {
  const workflowBySuite = new Map(workflowRuns.map((run) => [run.check_suite_id, run]));
  const newest = new Map<string, CheckRun>();
  for (const checkRun of checkRuns) {
    const workflow = checkRun.check_suite
      ? workflowBySuite.get(checkRun.check_suite.id)
      : undefined;
    const key = JSON.stringify(
      workflow === undefined
        ? ["app", checkRun.name, checkRun.app?.id ?? null]
        : ["workflow", checkRun.name, workflow.name, workflow.event]
    );
    const kept = newest.get(key);
    if (kept === undefined || checkRun.id > kept.id) newest.set(key, checkRun);
  }
  return [...newest.values()];
}

/**
 * One external link on the issue header's details line. A GitHub issue or pull request is
 * shown as its number and title (the repository while the title is unknown — loading, or no
 * GitHub App installation for the repository), never as the raw address, so it fits the line on a
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
      const repositoryPath = `repos/${match?.[1]}/${match?.[2]}`;
      const [allCheckRuns, workflowRuns] = await Promise.all([
        readEveryPage<CheckRun>(`${repositoryPath}/commits/${headSHA}/check-runs`, "check_runs"),
        readEveryPage<WorkflowRun>(
          `${repositoryPath}/actions/runs?head_sha=${headSHA}`,
          "workflow_runs"
        ),
      ]);
      const checkRuns = newestCheckRuns(allCheckRuns, workflowRuns);
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
        <TruncatedText>{link.url}</TruncatedText>
      </a>
    );
  }
  const repository = `${match[1]}/${match[2]}`;
  const number = `#${match[4]}`;
  // The reference (title, state) and the checks are separate lookups: a failed check-runs or
  // workflow-runs read only drops the checks pill and never the title and state already loaded.
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
          ? "GitHub details are unavailable for this repository."
          : reference === undefined
            ? link.url
            : `${repository}${number}: ${reference.title}`
      }
    >
      <span className="shrink-0 font-medium">{number}</span>
      <TruncatedText className="max-w-[18ch] sm:max-w-[32ch]">
        {reference === undefined ? repository : reference.title}
      </TruncatedText>
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
