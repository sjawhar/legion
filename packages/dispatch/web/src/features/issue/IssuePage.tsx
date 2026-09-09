import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type KeyboardEvent, type ReactNode, useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type { Event, ExternalLink, Issue, UserIssueState, UserState } from "../../api/types";
import { BoardStrip } from "./BoardStrip";
import { ChildrenTab } from "./ChildrenTab";
import { LogTab } from "./LogTab";

const issueStatuses = [
  "triage",
  "icebox",
  "backlog",
  "todo",
  "in progress",
  "testing",
  "needs review",
  "retro",
  "done",
];
const routePattern = /^(role:[a-z0-9-]+|session:[0-9a-f-]{16,})$/;

type IssueTab = "log" | "children";

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

function GitHubLink({ link }: { link: ExternalLink }): ReactNode {
  const match = link.url.match(
    /^https:\/\/github\.com\/([^/]+)\/([^/]+)\/(?:issues|pull)\/(\d+)\/?$/
  );
  const githubIssue = useQuery({
    enabled: match !== null,
    queryKey: ["github-link", link.url],
    queryFn: async () => {
      const response = await api.githubRest(
        `repos/${match?.[1]}/${match?.[2]}/issues/${match?.[3]}`
      );
      return (await response.json()) as { state: string; title: string };
    },
    retry: false,
  });

  if (match === null || githubIssue.isError) {
    const unavailable =
      githubIssue.error instanceof ApiError &&
      githubIssue.error.code === "GITHUB_TOKEN_UNAVAILABLE";
    return (
      <a
        className="text-sm text-sky-700 underline hover:text-sky-900"
        href={link.url}
        title={unavailable ? "GitHub details are unavailable for this sign-in." : undefined}
      >
        {link.url}
      </a>
    );
  }
  if (githubIssue.data === undefined) {
    return (
      <a className="text-sm text-sky-700 underline" href={link.url}>
        {link.url}
      </a>
    );
  }
  return (
    <a
      className="inline-flex items-center gap-2 text-sm text-sky-700 underline hover:text-sky-900"
      href={link.url}
    >
      <span>{githubIssue.data.title}</span>
      <span className="rounded-full bg-slate-100 px-2 py-0.5 text-xs text-slate-700">
        {githubIssue.data.state}
      </span>
    </a>
  );
}

function IssueHeader({ issue, state }: { issue: Issue; state: UserIssueState }): ReactNode {
  const queryClient = useQueryClient();
  const [title, setTitle] = useState(issue.title);
  const [route, setRoute] = useState(issue.route ?? "");
  const events = useQuery({
    queryKey: ["events", issue.key, "active-sessions"],
    queryFn: () => api.getIssueEvents(issue.key, { limit: 200 }),
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
  useEffect(() => setRoute(issue.route ?? ""), [issue.route]);

  const saveTitle = () => {
    const next = title.trim();
    if (next !== "" && next !== issue.title) {
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
    if (route === "" || routePattern.test(route)) {
      updateIssue.mutate({ route });
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
            to={`/issues/${issue.parent}`}
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
          onChange={(event) => setRoute(event.target.value)}
          placeholder="role:legion-controller-core"
          value={route}
        />
        <button
          className="rounded border border-slate-300 px-2 py-1 text-sm font-medium text-slate-700 disabled:cursor-not-allowed disabled:text-slate-400"
          disabled={!routeIsValid || updateIssue.isPending}
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
                  <span>{session.id}</span>
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

export function IssuePage(): ReactNode {
  const { key } = useParams();
  const [tab, setTab] = useState<IssueTab>("log");
  const issue = useQuery({
    enabled: key !== undefined,
    queryKey: ["issue", key],
    queryFn: () => api.getIssue(key ?? ""),
  });
  const state = useQuery({ queryKey: ["user-state"], queryFn: () => api.getMyState() });

  if (issue.isPending || state.isPending) {
    return <p className="text-slate-500">Loading issue…</p>;
  }
  if (issue.isError || state.isError || issue.data === undefined || key === undefined) {
    return <p className="text-rose-700">Could not load this issue.</p>;
  }

  const issueState = stateForIssue(state.data, issue.data.key);
  return (
    <section>
      <IssueHeader issue={issue.data} state={issueState} />
      <BoardStrip issue={issue.data} state={issueState} />
      <div className="mb-4 flex gap-2 border-b border-slate-200" role="tablist">
        <button
          aria-controls="issue-log-panel"
          aria-selected={tab === "log"}
          className={
            tab === "log"
              ? "border-b-2 border-sky-600 px-3 py-2 text-sm font-semibold text-sky-700"
              : "px-3 py-2 text-sm text-slate-600"
          }
          id="issue-log-tab"
          onClick={() => setTab("log")}
          role="tab"
          type="button"
        >
          Log
        </button>
        <button
          aria-controls="issue-children-panel"
          aria-selected={tab === "children"}
          className={
            tab === "children"
              ? "border-b-2 border-sky-600 px-3 py-2 text-sm font-semibold text-sky-700"
              : "px-3 py-2 text-sm text-slate-600"
          }
          id="issue-children-tab"
          onClick={() => setTab("children")}
          role="tab"
          type="button"
        >
          Children
        </button>
      </div>
      <div
        aria-labelledby={tab === "log" ? "issue-log-tab" : "issue-children-tab"}
        id={tab === "log" ? "issue-log-panel" : "issue-children-panel"}
        role="tabpanel"
      >
        {tab === "log" ? (
          <LogTab issueKey={issue.data.key} state={state.data} />
        ) : (
          <ChildrenTab issue={issue.data} />
        )}
      </div>
    </section>
  );
}
