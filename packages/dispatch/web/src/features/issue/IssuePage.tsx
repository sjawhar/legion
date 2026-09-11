import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  type FormEvent,
  type KeyboardEvent,
  type ReactNode,
  useCallback,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { Link, Navigate, useLocation, useNavigate } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import { mergeIssue } from "../../api/issue-cache";
import type {
  Artifact,
  Event,
  ExternalLink,
  Issue,
  UserIssueState,
  UserState,
} from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  badgeLow,
  bgTransparent,
  borderDefault,
  borderStrongHover,
  borderTransparent,
  calloutWarningBg,
  calloutWarningBorder,
  calloutWarningText,
  dangerText,
  focusBorder,
  focusVisibleRing,
  inputClasses,
  linkHoverText,
  linkText,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  surfaceMutedDisabledBg,
  surfaceMutedStrongBg,
  textMutedHoverToSecondary,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryHoverToPrimary,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { ArtifactDocument } from "../artifacts/ArtifactDocument";
import { ArtifactRoutePanel } from "../artifacts/ArtifactRoutePanel";
import { ConversationTab } from "../conversation/ConversationTab";
import { shortSessionId } from "../conversation/conversation-model";
import { useAgents } from "../conversation/useAgents";
import { ConnectionDot } from "../doc/ConnectionDot";
import type { DocumentToolbar } from "../doc/ProofDocument";
import {
  buildIssuePath,
  type IssueRoute,
  type IssueTab,
  isLegacyLogPath,
  isPrimaryDocumentArtifactRoute,
  issueTabForRoute,
  parseIssuePath,
} from "../refs/routes";
import { firstHighlightTerm } from "../search/search-model";
import { NotFoundPage } from "../shell/NotFoundPage";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { ChildrenTab } from "./ChildrenTab";
import { IssueTabs } from "./IssueTabs";
import { useIssueDrafts } from "./useIssueDrafts";

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

function activeSessions(
  events: Event[],
  liveSessionTitles: ReadonlyMap<string, string>
): Extract<Event["actor"], { kind: "session" }>[] {
  const sessions = new Map<string, Extract<Event["actor"], { kind: "session" }>>();
  for (const event of events) {
    if (event.actor.kind === "session") {
      sessions.set(event.actor.id, event.actor);
    }
  }
  return [...sessions.values()].filter((session) => liveSessionTitles.has(session.id));
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
      <span className={`text-sm ${textMutedOnCanvas}`} title="Unsafe external link">
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

function IssueHeader({
  isClosed,
  issue,
  onShowDiffChange,
  onVersionChange,
  showDiff,
  showDocumentControls,
  state,
  toolbar,
  version,
}: {
  isClosed: boolean;
  issue: Issue;
  onShowDiffChange(next: boolean): void;
  onVersionChange(version: number | null): void;
  showDiff: boolean;
  showDocumentControls: boolean;
  state: UserIssueState;
  toolbar: DocumentToolbar | undefined;
  version: number | undefined;
}): ReactNode {
  const queryClient = useQueryClient();
  const [editingTitle, setEditingTitle] = useState(false);
  const [routeEditing, setRouteEditing] = useState(false);
  const events = useQuery({
    queryKey: ["events", issue.key, "active-sessions"],
    queryFn: () => api.getIssueEvents(issue.key, { limit: 200, order: "desc" }),
  });
  const hasSessionEvents = events.data?.some((event) => event.actor.kind === "session") ?? false;
  const { isError: liveSessionsError, titles: liveSessionTitles } = useAgents(hasSessionEvents);
  const sessions = liveSessionsError ? [] : activeSessions(events.data ?? [], liveSessionTitles);
  const updateIssue = useMutation({
    mutationFn: (input: Partial<Pick<Issue, "route" | "status" | "title">>) =>
      api.patchIssue(issue.key, input),
    onMutate: async () => {
      await queryClient.cancelQueries({ queryKey: ["issue", issue.key] });
    },
    onSettled: (_data, error) => {
      drafts.onIssueSettled(error);
    },
    onSuccess: (next, input) => {
      mergeIssue(queryClient, next);
      drafts.onIssueSuccess(next, input);
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
    },
  });
  const drafts = useIssueDrafts(issue, updateIssue);
  const statusSaving = updateIssue.isPending && updateIssue.variables?.status !== undefined;
  const pinGuard = useSubmitGuard();
  const updateState = useMutation({
    mutationFn: (pinned: boolean) => api.putIssueState(issue.key, { pinned }),
    onSettled: () => {
      pinGuard.release();
      void queryClient.invalidateQueries({ queryKey: ["user-state"] });
    },
    onMutate: async (pinned) => {
      await queryClient.cancelQueries({ queryKey: ["user-state"] });
      const previous = queryClient.getQueryData<UserState>(["user-state"]);
      queryClient.setQueryData<UserState>(["user-state"], (current) => ({
        ...current,
        [issue.key]: { ...state, pinned },
      }));
      return { previous };
    },
    onError: (_error, _pinned, context) => {
      queryClient.setQueryData<UserState>(["user-state"], (current) => ({
        ...current,
        [issue.key]: context?.previous?.[issue.key] ?? {
          dismissed: [],
          last_read_seq: 0,
          pinned: false,
        },
      }));
    },
    onSuccess: (next) => {
      queryClient.setQueryData<UserState>(["user-state"], (current) => ({
        ...current,
        [issue.key]: next,
      }));
    },
  });
  const saveTitleOnEnter = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === "Enter") {
      event.currentTarget.blur();
    }
  };
  const saveRoute = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (drafts.requestRouteSubmit()) {
      setRouteEditing(false);
    }
  };

  return (
    <header className={`mb-3 border-b pb-3 ${borderDefault}`}>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <p className={`shrink-0 text-sm font-semibold ${linkText}`}>{issue.key}</p>
        {editingTitle ? (
          <input
            aria-describedby={drafts.titleError === null ? undefined : "issue-title-help"}
            aria-label="Issue title"
            className={`min-w-40 flex-1 rounded-lg border px-2 py-1 text-lg font-semibold outline-none ${borderTransparent} ${bgTransparent} ${textPrimaryOnCanvas} ${borderStrongHover} ${focusBorder}`}
            disabled={isClosed}
            onBlur={() => {
              drafts.requestTitleSubmit();
              setEditingTitle(false);
            }}
            onChange={(event) => drafts.writeTitle(event.target.value)}
            onKeyDown={(event) => {
              if (event.key === "Escape") {
                drafts.discardTitle();
                setEditingTitle(false);
                return;
              }
              saveTitleOnEnter(event);
            }}
            ref={(node) => node?.focus()}
            value={drafts.title}
          />
        ) : (
          <h1
            className={`min-w-40 flex-1 truncate rounded-lg border px-2 py-1 text-lg font-semibold ${borderTransparent} ${textPrimaryOnCanvas} ${
              isClosed ? "" : `cursor-text ${borderStrongHover}`
            }`}
            onClick={() => {
              if (!isClosed) {
                setEditingTitle(true);
              }
            }}
            onFocus={() => {
              if (!isClosed) {
                setEditingTitle(true);
              }
            }}
            onKeyDown={(event) => {
              if (!isClosed && (event.key === "Enter" || event.key === " ")) {
                event.preventDefault();
                setEditingTitle(true);
              }
            }}
            tabIndex={isClosed ? -1 : 0}
            title={issue.title}
          >
            {drafts.title}
          </h1>
        )}
        <label
          className={`flex shrink-0 items-center gap-1 text-sm font-medium ${textSecondaryOnCanvas}`}
        >
          Status
          <select
            className={`rounded px-2 py-1 font-normal disabled:cursor-not-allowed ${inputClasses(false)} ${surfaceMutedDisabledBg} ${secondaryButtonDisabledText}`}
            disabled={updateIssue.isPending}
            onChange={(event) => drafts.requestStatusSubmit(event.target.value)}
            value={issue.status}
          >
            {issueStatuses.map((status) => (
              <option key={status} value={status}>
                {status}
              </option>
            ))}
          </select>
        </label>
        {showDocumentControls && toolbar !== undefined ? (
          <div className="flex min-w-0 flex-wrap items-center gap-2">
            <label
              className={`flex min-h-11 min-w-0 items-center gap-2 text-sm font-medium ${textSecondaryOnCanvas}`}
            >
              Version
              <select
                aria-label="Version"
                className={`min-h-11 min-w-0 max-w-56 truncate rounded border px-2 py-2 font-normal ${inputClasses(false)}`}
                onChange={(event) => {
                  onVersionChange(event.target.value === "" ? null : Number(event.target.value));
                }}
                value={version ?? ""}
              >
                <option value="">Current</option>
                {toolbar.versions.map((item) => (
                  <option key={item.number} value={item.number}>
                    Version {item.number}
                    {item.named && item.summary !== null ? ` — ${item.summary}` : ""}
                  </option>
                ))}
              </select>
            </label>
            <button
              className={`shrink-0 rounded-lg px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
              disabled={isClosed || toolbar.isNamingVersion}
              onClick={toolbar.requestNamedVersion}
              type="button"
            >
              Name version
            </button>
            {version === undefined ? null : (
              <button
                className={`shrink-0 rounded-lg px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
                onClick={() => onShowDiffChange(!showDiff)}
                type="button"
              >
                {showDiff ? "Show version" : "Diff vs current"}
              </button>
            )}
            <ConnectionDot connection={toolbar.connection} />
          </div>
        ) : null}
        {routeEditing ? (
          <form className="flex min-w-0 flex-1 flex-wrap items-center gap-2" onSubmit={saveRoute}>
            <label className="sr-only" htmlFor="issue-route">
              Route
            </label>
            <input
              aria-describedby="issue-route-help"
              className={`min-w-0 flex-1 rounded px-2 py-1 text-sm outline-none ${inputClasses(false)}`}
              disabled={isClosed}
              id="issue-route"
              onChange={(event) => drafts.writeRoute(event.target.value)}
              onKeyDown={(event) => {
                if (event.key === "Escape") {
                  drafts.discardRoute();
                  setRouteEditing(false);
                }
              }}
              placeholder="role:legion-controller-core"
              value={drafts.route}
            />
            <button
              className={`rounded px-2 py-1 text-sm font-medium disabled:cursor-not-allowed ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonDisabledText}`}
              disabled={isClosed || !drafts.routeIsValid || updateIssue.isPending}
              type="submit"
            >
              Save route
            </button>
            <button
              className={`rounded border px-2 py-1 text-sm font-medium ${borderTransparent} ${textMutedHoverToSecondary}`}
              onClick={() => {
                drafts.discardRoute();
                setRouteEditing(false);
              }}
              type="button"
            >
              Cancel
            </button>
            <span
              className={drafts.routeIsValid ? "sr-only" : `text-sm ${dangerText}`}
              id="issue-route-help"
            >
              Route must be role:[a-z0-9-]+ or session:[0-9a-f-]{`{16,}`}.
            </span>
          </form>
        ) : (
          <button
            className={`min-w-40 flex-1 truncate rounded px-2 py-1 text-left text-sm outline-none focus-visible:ring-2 ${textSecondaryHoverToPrimary} ${focusVisibleRing}`}
            disabled={isClosed}
            onClick={() => setRouteEditing(true)}
            title={drafts.route === "" ? undefined : `Messages also reach ${drafts.route}`}
            type="button"
          >
            {drafts.route === ""
              ? "No route — messages stay on the issue"
              : `Messages also reach ${drafts.route}`}
          </button>
        )}
        <button
          className={`shrink-0 rounded-lg px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
          disabled={updateState.isPending}
          onClick={() => pinGuard.guard(() => updateState.mutate(!state.pinned))}
          type="button"
        >
          {state.pinned ? "Unpin issue" : "Pin issue"}
        </button>
      </div>
      {drafts.titleError === null ? null : (
        <p className={`mt-1 text-sm ${dangerText}`} id="issue-title-help">
          {drafts.titleError}
        </p>
      )}
      {statusSaving ? (
        <span className={`mt-1 block text-xs ${textMutedOnCanvas}`} role="status">
          Saving…
        </span>
      ) : null}
      {issue.labels.length === 0 && issue.parent === null ? null : (
        <div className="mt-2 flex flex-wrap items-center gap-2">
          {issue.labels.map((label) => (
            <span
              className={`break-all rounded-full px-2 py-1 text-xs font-medium ${surfaceMutedStrongBg} ${textSecondaryOnCanvas}`}
              key={label}
            >
              {label}
            </span>
          ))}
          {issue.parent === null ? null : (
            <Link
              className={`text-sm underline ${linkText} ${linkHoverText}`}
              to={buildIssuePath({ key: issue.parent, kind: "issue" })}
            >
              Parent: {issue.parent}
            </Link>
          )}
        </div>
      )}
      {updateState.isError ? (
        <QueryError
          message="Could not save pin status."
          onRetry={() => pinGuard.retryLast(updateState)}
          retrying={updateState.isPending}
        />
      ) : null}
      {issue.external_links.length === 0 ? null : (
        <div className="mt-2 flex flex-wrap gap-x-4 gap-y-2">
          {issue.external_links.map((link) => (
            <GitHubLink key={link.url} link={link} />
          ))}
        </div>
      )}
      {sessions.length === 0 ? null : (
        <section aria-label="Active sessions" className="mt-2">
          <h2 className={`text-sm font-semibold ${textSecondaryOnCanvas}`}>Active sessions</h2>
          <ul className="mt-2 flex flex-wrap gap-2">
            {sessions.map((session) => {
              const tmuxTarget = session.origin?.tmux;
              const title = liveSessionTitles.get(session.id)?.trim();
              const label =
                title === "" || title === undefined ? shortSessionId(session.id) : title;
              return (
                <li
                  className={`flex items-center gap-2 rounded-full px-3 py-1 text-sm ${badgeLow.bg} ${badgeLow.text}`}
                  key={session.id}
                >
                  <span title={session.id}>{label}</span>
                  {tmuxTarget === undefined ? null : (
                    <button
                      className={`font-medium ${linkText} ${linkHoverText}`}
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
        <QueryError
          message="Could not update this issue."
          onRetry={drafts.retry}
          retrying={updateIssue.isPending}
        />
      ) : null}
    </header>
  );
}
export function IssuePage(): ReactNode {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  if (route === undefined) {
    return <NotFoundPage />;
  }
  if (isLegacyLogPath(pathname)) {
    return <Navigate replace to={`${buildIssuePath(route)}${search}`} />;
  }
  // key={route.key}: switching to a different issue remounts IssueDetail
  // fresh (discarding any unsaved local drafts); switching tabs within the
  // same issue keeps route.key unchanged, so it only re-renders.
  return <IssueDetail key={route.key} route={route} />;
}

function IssueDetail({ route }: { route: IssueRoute }): ReactNode {
  const { search } = useLocation();
  const artifactRoute = route.kind === "artifact" ? route : undefined;
  const navigate = useNavigate();
  const artifactRouteSlug = artifactRoute?.slug;
  const query = new URLSearchParams(search);
  const commentId = query.get("comment") ?? undefined;
  const askId = query.get("ask") ?? undefined;
  const highlightTerm = firstHighlightTerm(query.get("q") ?? "");
  const issue = useQuery({
    queryKey: ["issue", route.key],
    queryFn: () => api.getIssue(route.key),
  });
  const state = useQuery({ queryKey: ["user-state"], queryFn: () => api.getMyState() });
  const primaryArtifact = issue.data?.artifacts?.find(
    ({ id }) => id === issue.data?.primary_artifact_id
  );
  const selectedArtifact =
    artifactRouteSlug === undefined
      ? primaryArtifact
      : issue.data?.artifacts?.find(({ slug }) => slug === artifactRouteSlug);
  const isPrimaryArtifactRoute = isPrimaryDocumentArtifactRoute(route, selectedArtifact);
  const activeTab = issueTabForRoute(route, selectedArtifact);
  const conversationFocusItemId =
    route.kind === "ask" || route.kind === "comment" ? route.id : undefined;
  const panelScroll = useRef<Partial<Record<IssueTab, number>>>({});
  const [specShowDiff, setSpecShowDiff] = useState(false);
  const [specToolbar, setSpecToolbar] = useState<DocumentToolbar | undefined>(undefined);
  const [artifactShowDiff, setArtifactShowDiff] = useState(false);
  const [artifactToolbar, setArtifactToolbar] = useState<DocumentToolbar | undefined>(undefined);
  const handleSpecToolbarChange = useCallback((next: DocumentToolbar | undefined) => {
    setSpecToolbar(next);
    if (next === undefined) {
      setSpecShowDiff(false);
    }
  }, []);
  const handleArtifactToolbarChange = useCallback((next: DocumentToolbar | undefined) => {
    setArtifactToolbar(next);
    if (next === undefined) {
      setArtifactShowDiff(false);
    }
  }, []);

  useLayoutEffect(() => {
    const top = panelScroll.current[activeTab];
    if (top !== undefined) {
      window.scrollTo({ top });
    }
  }, [activeTab]);

  // Once a panel's tab has ever been active, keep rendering its content even
  // while hidden — that is what keeps the Spec document connected and the Conversation's read
  // observer alive across tab switches (see the `hidden` panels below). A panel the user has
  // never opened stays unmounted, so a fresh page load doesn't pay for panels it never shows.
  const [activatedTabs, setActivatedTabs] = useState<Record<IssueTab, boolean>>(() => ({
    artifacts: activeTab === "artifacts",
    children: activeTab === "children",
    conversation: activeTab === "conversation",
    spec: activeTab === "spec",
  }));
  if (!activatedTabs[activeTab]) {
    setActivatedTabs({ ...activatedTabs, [activeTab]: true });
  }

  useDocumentTitle(
    issue.data === undefined ? "Dispatch" : `${issue.data.key} · ${issue.data.title} · Dispatch`
  );

  if (issue.isPending || state.isPending) {
    return <p className={textMutedOnCanvas}>Loading issue…</p>;
  }
  if (issue.isError || state.isError) {
    const notFound = issue.error instanceof ApiError && issue.error.status === 404;
    return (
      <section>
        <h1 className={`text-xl font-semibold ${textPrimaryOnCanvas}`}>
          {notFound ? "Issue not found" : "Couldn't load this issue"}
        </h1>
        {notFound ? (
          <p className={`mt-2 text-sm ${textSecondaryOnCanvas}`}>
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
          className={`mt-4 inline-block text-sm font-medium underline ${linkText} ${linkHoverText}`}
          to="/"
        >
          Back to inbox
        </Link>
      </section>
    );
  }
  if (issue.data === undefined) {
    return <p className={dangerText}>Could not load this issue.</p>;
  }

  if (primaryArtifact === undefined) {
    return <p className={dangerText}>Could not load this issue&apos;s primary document.</p>;
  }

  const issueState = stateForIssue(state.data, issue.data.key);
  const isClosed = issue.data.closed_at !== null;
  const issueKey = issue.data.key;
  const selectDocumentVersion = (artifact: Artifact, version: number | null) => {
    navigate(
      buildIssuePath(
        version === null && artifact.primary
          ? { key: issue.data.key, kind: "spec" }
          : version === null
            ? { key: issue.data.key, kind: "artifact", slug: artifact.slug }
            : { key: issue.data.key, kind: "artifact", slug: artifact.slug, version }
      )
    );
  };

  return (
    <section>
      {isClosed ? (
        <p
          className={`mb-4 rounded-lg p-3 text-sm ${calloutWarningBorder} ${calloutWarningBg} ${calloutWarningText}`}
        >
          This issue is closed.
        </p>
      ) : null}
      <IssueHeader
        isClosed={isClosed}
        issue={issue.data}
        onShowDiffChange={setSpecShowDiff}
        onVersionChange={(version) => {
          setSpecShowDiff(false);
          selectDocumentVersion(primaryArtifact, version);
        }}
        showDiff={specShowDiff}
        showDocumentControls={activeTab === "spec"}
        state={issueState}
        toolbar={specToolbar}
        version={isPrimaryArtifactRoute ? artifactRoute?.version : undefined}
      />
      <IssueTabs
        activeTab={activeTab}
        issueKey={issueKey}
        onBeforeTabChange={(current) => {
          panelScroll.current[current] = window.scrollY;
        }}
      />
      <div
        aria-hidden={activeTab !== "spec"}
        aria-labelledby="issue-spec-tab"
        hidden={activeTab !== "spec"}
        id="issue-spec-panel"
        role="tabpanel"
      >
        {activatedTabs.spec && !(artifactRoute !== undefined && !isPrimaryArtifactRoute) ? (
          <ArtifactDocument
            artifact={primaryArtifact}
            askId={askId}
            commentId={commentId}
            highlightTerm={highlightTerm}
            isClosed={isClosed}
            onToolbarChange={handleSpecToolbarChange}
            owner={{ key: issueKey, kind: "issue" }}
            onVersionChange={(version) => {
              setSpecShowDiff(false);
              selectDocumentVersion(primaryArtifact, version);
            }}
            showDiff={specShowDiff}
            version={isPrimaryArtifactRoute ? artifactRoute?.version : undefined}
          />
        ) : null}
      </div>
      <div
        aria-hidden={activeTab !== "conversation"}
        aria-labelledby="issue-conversation-tab"
        hidden={activeTab !== "conversation"}
        id="issue-conversation-panel"
        role="tabpanel"
      >
        {activatedTabs.conversation ? (
          <ConversationTab
            focusItemId={conversationFocusItemId}
            isClosed={isClosed}
            issueKey={issueKey}
            state={state.data}
            visible={activeTab === "conversation"}
          />
        ) : null}
      </div>
      <div
        aria-hidden={activeTab !== "children"}
        aria-labelledby="issue-children-tab"
        hidden={activeTab !== "children"}
        id="issue-children-panel"
        role="tabpanel"
      >
        {activatedTabs.children ? <ChildrenTab issue={issue.data} /> : null}
      </div>
      <ArtifactRoutePanel
        active={activeTab === "artifacts"}
        artifact={selectedArtifact}
        artifactRoute={artifactRoute}
        isClosed={isClosed}
        isPrimaryArtifactRoute={isPrimaryArtifactRoute}
        mounted={activatedTabs.artifacts}
        onShowDiffChange={setArtifactShowDiff}
        route={route}
        showDiff={artifactShowDiff}
        toolbar={artifactToolbar}
      >
        {selectedArtifact?.kind === "doc" ? (
          <ArtifactDocument
            artifact={selectedArtifact}
            askId={askId}
            commentId={commentId}
            highlightTerm={highlightTerm}
            isClosed={isClosed}
            onToolbarChange={handleArtifactToolbarChange}
            owner={{ key: issueKey, kind: "issue" }}
            onVersionChange={(version) => {
              setArtifactShowDiff(false);
              selectDocumentVersion(selectedArtifact, version);
            }}
            showDiff={artifactShowDiff}
            version={artifactRoute?.version}
          />
        ) : null}
      </ArtifactRoutePanel>
    </section>
  );
}
