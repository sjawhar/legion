import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type KeyboardEvent, type ReactNode, useEffect, useState } from "react";
import { Link, useLocation } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type {
  AuthenticatedUser,
  Comment,
  Event,
  ExternalLink,
  Issue,
  UserIssueState,
  UserState,
} from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { DocEditor } from "../doc/DocEditor";
import { DocView } from "../doc/DocView";
import { actorLabel } from "../refs/actor";
import { buildIssuePath, type DispatchRoute, parseIssuePath } from "../refs/routes";
import { NotFoundPage } from "../shell/NotFoundPage";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { BoardStrip } from "./BoardStrip";
import { ChildrenTab } from "./ChildrenTab";
import { type IssueTab, IssueTabs } from "./IssueTabs";
import { LogTab } from "./LogTab";

const issueStatuses = [
  "triage",
  "icebox",
  "backlog",
  "todo",
  "in_progress",
  "testing",
  "needs_review",
  "retro",
  "done",
];
const routePattern = /^(role:[a-z0-9-]+|session:[0-9a-f-]{16,})$/;

function activeSessions(events: Event[]): Extract<Event["actor"], { kind: "session" }>[] {
  const sessions = new Map<string, Extract<Event["actor"], { kind: "session" }>>();
  for (const event of events) {
    if (event.actor.kind === "session") {
      sessions.set(event.actor.id, event.actor);
    }
  }
  return [...sessions.values()];
}

function stateForIssue(state: UserState | undefined, issueKey: string): UserIssueState {
  return state?.[issueKey] ?? { dismissed: [], last_read_seq: 0, pinned: false };
}

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

function GitHubLink({ link }: { link: ExternalLink }): ReactNode {
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
      <span className="text-sm text-slate-600" title="Unsafe external link">
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
        className="text-sm text-sky-700 underline hover:text-sky-900"
        href={href}
        title={unavailable ? "GitHub details are unavailable for this sign-in." : undefined}
      >
        {link.url}
      </a>
    );
  }
  if (githubReference.data === undefined) {
    return (
      <a className="text-sm text-sky-700 underline" href={href}>
        {link.url}
      </a>
    );
  }
  const state =
    isPullRequest && githubReference.data.merged ? "merged" : githubReference.data.state;
  return (
    <a
      className="inline-flex items-center gap-2 text-sm text-sky-700 underline hover:text-sky-900"
      href={href}
    >
      <span>{githubReference.data.title}</span>
      <span className="rounded-full bg-slate-100 px-2 py-0.5 text-xs text-slate-700">{state}</span>
      {isPullRequest && checks.data !== undefined ? (
        <span className="rounded-full bg-slate-100 px-2 py-0.5 text-xs text-slate-700">
          checks: {checks.data}
        </span>
      ) : null}
    </a>
  );
}

function IssueHeader({
  isClosed,
  issue,
  state,
}: {
  isClosed: boolean;
  issue: Issue;
  state: UserIssueState;
}): ReactNode {
  const queryClient = useQueryClient();
  const [title, setTitle] = useState(issue.title);
  const [route, setRoute] = useState(issue.route ?? "");
  const [routeDirty, setRouteDirty] = useState(false);
  const events = useQuery({
    queryKey: ["events", issue.key, "active-sessions"],
    queryFn: () => api.getIssueEvents(issue.key, { limit: 200, order: "desc" }),
  });
  const sessions = activeSessions(events.data ?? []);
  const updateIssue = useMutation({
    mutationFn: (input: Partial<Pick<Issue, "title" | "status" | "route">>) =>
      api.patchIssue(issue.key, input),
    onSuccess: (next) => {
      queryClient.setQueryData(["issue", issue.key], next);
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
    },
  });
  const updateState = useMutation({
    mutationFn: (pinned: boolean) => api.putIssueState(issue.key, { pinned }),
    onMutate: (pinned) => {
      queryClient.setQueryData<UserState>(["user-state"], (current) => ({
        ...current,
        [issue.key]: { ...state, pinned },
      }));
    },
    onError: () => {
      void queryClient.invalidateQueries({ queryKey: ["user-state"] });
    },
  });

  useEffect(() => setTitle(issue.title), [issue.title]);
  useEffect(() => {
    if (!routeDirty) {
      setRoute(issue.route ?? "");
    }
  }, [issue.route, routeDirty]);

  const saveTitle = () => {
    const next = title.trim();
    if (!isClosed && next !== "" && next !== issue.title) {
      updateIssue.mutate({ title: next });
    } else {
      setTitle(issue.title);
    }
  };
  const saveTitleOnEnter = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === "Enter") {
      event.currentTarget.blur();
    }
  };
  const saveRoute = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!isClosed && (route === "" || routePattern.test(route))) {
      updateIssue.mutate({ route }, { onSuccess: () => setRouteDirty(false) });
    }
  };
  const routeIsValid = route === "" || routePattern.test(route);

  return (
    <header className="mb-6 border-b border-slate-200 pb-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="text-sm font-semibold text-sky-700">{issue.key}</p>
        <button
          className="rounded-lg border border-slate-300 px-3 py-2 text-sm font-medium text-slate-700 hover:border-sky-500"
          disabled={updateState.isPending}
          onClick={() => updateState.mutate(!state.pinned)}
          type="button"
        >
          {state.pinned ? "Unpin issue" : "Pin issue"}
        </button>
      </div>
      <input
        aria-label="Issue title"
        className="mt-2 w-full rounded-lg border border-transparent bg-transparent px-2 py-1 text-2xl font-semibold text-slate-950 outline-none hover:border-slate-300 focus:border-sky-500"
        onBlur={saveTitle}
        onChange={(event) => setTitle(event.target.value)}
        onKeyDown={saveTitleOnEnter}
        disabled={isClosed}
        value={title}
      />
      <div className="mt-4 flex flex-wrap items-center gap-3">
        <label className="text-sm font-medium text-slate-700">
          Status
          <select
            className="ml-2 rounded border border-slate-300 bg-white px-2 py-1 font-normal"
            onChange={(event) => updateIssue.mutate({ status: event.target.value })}
            value={issue.status}
          >
            {issueStatuses.map((status) => (
              <option key={status} value={status}>
                {status}
              </option>
            ))}
          </select>
        </label>
        {issue.labels.map((label) => (
          <span
            className="rounded-full bg-slate-200 px-2 py-1 text-xs font-medium text-slate-700"
            key={label}
          >
            {label}
          </span>
        ))}
        {issue.parent === null ? null : (
          <Link
            className="text-sm text-sky-700 underline hover:text-sky-900"
            to={buildIssuePath({ key: issue.parent, kind: "issue" })}
          >
            Parent: {issue.parent}
          </Link>
        )}
      </div>
      <form className="mt-4 flex flex-wrap items-start gap-2" onSubmit={saveRoute}>
        <label className="text-sm font-medium text-slate-700" htmlFor="issue-route">
          Route
        </label>
        <input
          aria-describedby="issue-route-help"
          className="min-w-64 rounded border px-2 py-1 text-sm outline-none focus:border-sky-500"
          id="issue-route"
          onChange={(event) => {
            setRouteDirty(true);
            setRoute(event.target.value);
          }}
          placeholder="role:legion-controller-core"
          disabled={isClosed}
          value={route}
        />
        <button
          className="rounded border border-slate-300 px-2 py-1 text-sm font-medium text-slate-700 disabled:cursor-not-allowed disabled:text-slate-400"
          disabled={isClosed || !routeIsValid || updateIssue.isPending}
          type="submit"
        >
          Save route
        </button>
        <span className={routeIsValid ? "sr-only" : "text-sm text-rose-700"} id="issue-route-help">
          Route must be role:[a-z0-9-]+ or session:[0-9a-f-]{`{16,}`}.
        </span>
      </form>
      {issue.external_links.length === 0 ? null : (
        <div className="mt-4 flex flex-wrap gap-x-4 gap-y-2">
          {issue.external_links.map((link) => (
            <GitHubLink key={link.url} link={link} />
          ))}
        </div>
      )}
      {sessions.length === 0 ? null : (
        <section aria-label="Active sessions" className="mt-4">
          <h2 className="text-sm font-semibold text-slate-700">Active sessions</h2>
          <ul className="mt-2 flex flex-wrap gap-2">
            {sessions.map((session) => {
              const tmuxTarget = session.origin?.tmux;
              return (
                <li
                  className="flex items-center gap-2 rounded-full bg-slate-100 px-3 py-1 text-sm text-slate-700"
                  key={session.id}
                >
                  <span title={session.id}>{actorLabel(session)}</span>
                  {tmuxTarget === undefined ? null : (
                    <button
                      className="font-medium text-sky-700 hover:text-sky-900"
                      onClick={() => {
                        void navigator.clipboard?.writeText(tmuxTarget);
                      }}
                      type="button"
                    >
                      Copy tmux target
                    </button>
                  )}
                </li>
              );
            })}
          </ul>
        </section>
      )}
      {updateIssue.isError ? (
        <p className="mt-3 text-sm text-rose-700">Could not update this issue.</p>
      ) : null}
    </header>
  );
}

function historicalHighlight(
  markdown: string,
  highlight: { from: number; to: number } | undefined,
  anchor: Comment["anchor"] | undefined
): { from: number; to: number } | undefined {
  if (highlight === undefined || anchor === null || anchor === undefined) {
    return highlight;
  }
  if (markdown.slice(highlight.from, highlight.to) === anchor.quote) {
    return highlight;
  }
  const from = markdown.indexOf(anchor.quote);
  return from === -1 || markdown.indexOf(anchor.quote, from + 1) !== -1
    ? highlight
    : { from, to: from + anchor.quote.length };
}

function ArtifactVersionView({
  artifactId,
  from,
  issueKey,
  to,
  version,
}: {
  artifactId: string;
  from: number | undefined;
  issueKey: string;
  to: number | undefined;
  version: number;
}): ReactNode {
  const content = useQuery({
    queryKey: ["artifact", artifactId, "version", version],
    queryFn: () => api.getArtifactVersion(artifactId, version),
  });
  const comments = useQuery({
    queryKey: ["comments", issueKey, artifactId],
    queryFn: () => api.listComments(issueKey, artifactId),
  });

  if (content.isPending) {
    return <p className="text-slate-500">Loading version…</p>;
  }
  if (content.isError || content.data === undefined || !("markdown" in content.data)) {
    return <p className="text-rose-700">Could not load this document version.</p>;
  }
  const anchor = comments.data?.find(
    (comment) =>
      comment.anchor?.artifact_id === artifactId &&
      comment.anchor.version === version &&
      comment.anchor.from === from &&
      comment.anchor.to === to
  )?.anchor;
  const highlight = historicalHighlight(
    content.data.markdown,
    from === undefined || to === undefined ? undefined : { from, to },
    anchor
  );
  return (
    <section aria-label={`Document version ${version}`} className="space-y-3">
      <h2 className="text-lg font-semibold text-slate-900">Version {version}</h2>
      <DocView highlight={highlight} markdown={content.data.markdown} />
    </section>
  );
}

export function IssuePage({ user }: { user: AuthenticatedUser }): ReactNode {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  if (route === undefined) {
    return <NotFoundPage />;
  }
  // key={route.key}: switching to a different issue remounts IssueDetail
  // fresh (discarding any unsaved local drafts); switching tabs within the
  // same issue keeps route.key unchanged, so it only re-renders.
  return <IssueDetail key={route.key} route={route} user={user} />;
}

function IssueDetail({
  route,
  user,
}: {
  route: DispatchRoute;
  user: AuthenticatedUser;
}): ReactNode {
  const { search } = useLocation();
  const artifactRoute = route.kind === "artifact" ? route : undefined;
  const artifactRouteSlug = artifactRoute?.slug;
  const isSpecRoute = route.kind === "spec";
  const query = new URLSearchParams(search);
  const from = Number(query.get("from"));
  const to = Number(query.get("to"));
  const highlight =
    Number.isInteger(from) && Number.isInteger(to) && from >= 0 && to > from
      ? { from, to }
      : undefined;
  const issue = useQuery({
    queryKey: ["issue", route.key],
    queryFn: () => api.getIssue(route.key),
  });
  const state = useQuery({ queryKey: ["user-state"], queryFn: () => api.getMyState() });

  useDocumentTitle(
    issue.data === undefined ? "Dispatch" : `${issue.data.key} · ${issue.data.title} · Dispatch`
  );

  if (issue.isPending || state.isPending) {
    return <p className="text-slate-500">Loading issue…</p>;
  }
  if (issue.isError || state.isError) {
    const notFound = issue.error instanceof ApiError && issue.error.status === 404;
    return (
      <section>
        <h1 className="text-xl font-semibold text-slate-950">
          {notFound ? "Issue not found" : "Couldn't load this issue"}
        </h1>
        {notFound ? (
          <p className="mt-2 text-sm text-slate-600">
            {route.key} doesn&apos;t exist, or you don&apos;t have access to it.
          </p>
        ) : (
          <div className="mt-2">
            <QueryError
              message="Couldn't load this issue."
              onRetry={() => {
                void issue.refetch();
                void state.refetch();
              }}
            />
          </div>
        )}
        <Link
          className="mt-4 inline-block text-sm font-medium text-sky-700 underline hover:text-sky-900"
          to="/"
        >
          Back to inbox
        </Link>
      </section>
    );
  }
  if (issue.data === undefined) {
    return <p className="text-rose-700">Could not load this issue.</p>;
  }

  const primaryArtifact = issue.data.artifacts?.find(
    ({ id }) => id === issue.data.primary_artifact_id
  );
  if (primaryArtifact === undefined) {
    return <p className="text-rose-700">Could not load this issue's primary document.</p>;
  }
  const selectedArtifact =
    artifactRouteSlug === undefined
      ? primaryArtifact
      : issue.data.artifacts?.find(({ slug }) => slug === artifactRouteSlug);
  if (selectedArtifact === undefined) {
    return <p className="text-rose-700">Could not load this document artifact.</p>;
  }

  const issueState = stateForIssue(state.data, issue.data.key);
  const activeTab: IssueTab =
    artifactRouteSlug !== undefined && selectedArtifact.kind !== "doc"
      ? "log"
      : route.kind === "children"
        ? "children"
        : route.kind === "log"
          ? "log"
          : artifactRoute !== undefined || isSpecRoute
            ? "spec"
            : // Bare `/issues/KEY` with no tab segment: Log is the historical default.
              "log";
  const isClosed = issue.data.closed_at !== null;
  const issueKey = issue.data.key;

  return (
    <section>
      {isClosed ? (
        <p className="mb-4 rounded-lg border border-amber-200 bg-amber-50 p-3 text-sm text-amber-950">
          This issue is closed.
        </p>
      ) : null}
      <IssueHeader isClosed={isClosed} issue={issue.data} state={issueState} />
      <BoardStrip issue={issue.data} state={issueState} />
      <IssueTabs activeTab={activeTab} issueKey={issueKey} />
      <div
        aria-labelledby="issue-spec-tab"
        hidden={activeTab !== "spec"}
        id="issue-spec-panel"
        role="tabpanel"
      >
        {activeTab === "spec" ? (
          artifactRoute?.version === undefined ? (
            <DocEditor
              artifact={selectedArtifact}
              isClosed={isClosed}
              key={selectedArtifact.id}
              user={user}
            />
          ) : (
            <ArtifactVersionView
              artifactId={selectedArtifact.id}
              from={highlight?.from}
              issueKey={issueKey}
              to={highlight?.to}
              version={artifactRoute.version}
            />
          )
        ) : null}
      </div>
      <div
        aria-labelledby="issue-log-tab"
        hidden={activeTab !== "log"}
        id="issue-log-panel"
        role="tabpanel"
      >
        {activeTab === "log" ? <LogTab issueKey={issueKey} state={state.data} /> : null}
      </div>
      <div
        aria-labelledby="issue-children-tab"
        hidden={activeTab !== "children"}
        id="issue-children-panel"
        role="tabpanel"
      >
        {activeTab === "children" ? <ChildrenTab issue={issue.data} /> : null}
      </div>
    </section>
  );
}
