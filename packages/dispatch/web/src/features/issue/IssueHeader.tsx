import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type KeyboardEvent, type ReactNode, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import { mergeIssue } from "../../api/issue-cache";
import type { Artifact, Issue, IssueDetails, UserIssueState, UserState } from "../../api/types";
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
  textMutedHoverToSecondary,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryHoverToPrimary,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { ApprovalChip } from "../doc/ApprovalChip";
import { ConnectionDot } from "../doc/ConnectionDot";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { selectableStatusesFor } from "../project/board-model";
import { buildIssuePath } from "../refs/routes";
import { GitHubLink } from "./GitHubLink";
import { IssueLabels } from "./IssueLabels";
import { SubscribedAgents } from "./SubscribedAgents";
import { useIssueDrafts } from "./useIssueDrafts";

export function IssueHeader({
  documentArtifact,
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
  documentArtifact: Artifact | undefined;
  isClosed: boolean;
  issue: IssueDetails;
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
  const subscribers = useQuery({
    queryKey: ["subscribers", issue.key],
    queryFn: () => api.getIssueSubscribers(issue.key),
  });
  const subscriberList = subscribers.isError ? [] : (subscribers.data ?? []);
  const unsubscribe = useMutation({
    mutationFn: (sessionId: string) => api.unsubscribeIssueSession(issue.key, sessionId),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["subscribers", issue.key] });
    },
  });
  const updateIssue = useMutation({
    mutationFn: (input: Partial<Pick<Issue, "priority" | "route" | "status" | "title">>) =>
      api.patchIssue(issue.key, input),
    onMutate: async (input) => {
      await queryClient.cancelQueries({ queryKey: ["issue", issue.key] });
      const previous = queryClient.getQueryData<IssueDetails>(["issue", issue.key]);
      const priority = input.priority;
      if (priority !== undefined) {
        queryClient.setQueryData<IssueDetails>(["issue", issue.key], (current) =>
          current === undefined ? undefined : { ...current, priority }
        );
      }
      return { previous };
    },
    onError: (_error, _input, context) => {
      if (context?.previous !== undefined) {
        queryClient.setQueryData<IssueDetails>(["issue", issue.key], context.previous);
      }
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
  const labelsMutation = useMutation({
    mutationFn: (labels: string[]) => api.patchIssue(issue.key, { labels }),
    onSuccess: (next) => {
      mergeIssue(queryClient, next);
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
    },
  });
  const drafts = useIssueDrafts(issue, updateIssue);
  const statusSaving = updateIssue.isPending && updateIssue.variables?.status !== undefined;
  const pendingStatus = statusSaving ? updateIssue.variables?.status : undefined;
  const selectableStatuses = selectableStatusesFor(issue.status);
  const staleOpenAsk = issue.open_asks.find(
    (ask) => Date.parse(ask.created_at) < Date.now() - 60 * 60 * 1000
  );
  const waitingOn =
    staleOpenAsk === undefined
      ? null
      : staleOpenAsk.author.kind === "session" && staleOpenAsk.author.owner !== undefined
        ? staleOpenAsk.author.owner
        : issue.created_by.kind === "user"
          ? issue.created_by.id
          : "you";
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
      {isClosed ? (
        <div
          className={`mb-4 flex flex-wrap items-center justify-between gap-3 rounded-lg p-3 text-sm ${calloutWarningBorder} ${calloutWarningBg} ${calloutWarningText}`}
        >
          <span>This issue is closed.</span>
          <button
            aria-label="Reopen issue"
            className={`min-h-11 rounded-lg px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
            disabled={updateIssue.isPending}
            onClick={() => drafts.requestStatusSubmit("backlog")}
            title="Reopen into Backlog"
            type="button"
          >
            {pendingStatus === "backlog" ? "Reopening…" : "Reopen"}
          </button>
        </div>
      ) : null}
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
            disabled={isClosed || updateIssue.isPending}
            onChange={(event) => drafts.requestStatusSubmit(event.target.value)}
            value={issue.status}
          >
            {selectableStatuses.map((status) => (
              <option key={status} value={status}>
                {status}
              </option>
            ))}
          </select>
        </label>
        <label
          className={`flex shrink-0 items-center gap-1 text-sm font-medium ${textSecondaryOnCanvas}`}
        >
          Priority
          <select
            className={`rounded px-2 py-1 font-normal disabled:cursor-not-allowed ${inputClasses(false)} ${surfaceMutedDisabledBg} ${secondaryButtonDisabledText}`}
            disabled={isClosed || updateIssue.isPending}
            onChange={(event) =>
              drafts.requestPrioritySubmit(
                event.target.value === "" ? null : (Number(event.target.value) as 0 | 1 | 2 | 3)
              )
            }
            value={issue.priority ?? ""}
          >
            <option value="">—</option>
            <option value="0">P0</option>
            <option value="1">P1</option>
            <option value="2">P2</option>
            <option value="3">P3</option>
          </select>
        </label>
        {isClosed ? null : (
          <button
            aria-label="Close issue"
            className={`min-h-11 shrink-0 rounded-lg px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
            disabled={updateIssue.isPending}
            onClick={() => drafts.requestStatusSubmit("done")}
            type="button"
          >
            {pendingStatus === "done" ? "Closing…" : "Close issue"}
          </button>
        )}
        {waitingOn === null ? null : (
          <span className={`rounded-full px-2 py-0.5 text-xs ${badgeLow.bg} ${badgeLow.text}`}>
            Waiting on {waitingOn}
          </span>
        )}
        {showDocumentControls && documentArtifact !== undefined ? (
          <ApprovalChip artifact={documentArtifact} variant="header" />
        ) : null}
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
            title={drafts.route === "" ? undefined : `Messages default to ${drafts.route}`}
            type="button"
          >
            {`Messages default to ${drafts.route === "" ? "no route" : drafts.route}`}
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
          {pendingStatus === "done"
            ? "Closing…"
            : pendingStatus === "backlog" && isClosed
              ? "Reopening…"
              : "Saving…"}
        </span>
      ) : null}
      <IssueLabels
        disabled={isClosed}
        labels={issue.labels}
        onSave={(labels) => labelsMutation.mutateAsync(labels)}
        project={issue.project}
        saveError={labelsMutation.isError}
        saving={labelsMutation.isPending}
      />
      {issue.parent === null ? null : (
        <div className="mt-2">
          <Link
            className={`text-sm underline ${linkText} ${linkHoverText}`}
            to={buildIssuePath({ key: issue.parent, kind: "issue" })}
          >
            Parent: {issue.parent}
          </Link>
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
      <SubscribedAgents
        onUnsubscribe={(sessionId) => unsubscribe.mutate(sessionId)}
        ownerLabel={issue.key}
        subscribers={subscriberList}
      />
      {subscribers.isError ? (
        <QueryError
          message="Subscribed agents unavailable — Envoy listener unreachable."
          onRetry={() => subscribers.refetch()}
          retrying={subscribers.isFetching}
        />
      ) : null}
      {unsubscribe.isError ? (
        <QueryError
          message="Could not unsubscribe this agent."
          onRetry={() => unsubscribe.mutate(unsubscribe.variables as string)}
          retrying={unsubscribe.isPending}
        />
      ) : null}
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
export function stateForIssue(state: UserState | undefined, issueKey: string): UserIssueState {
  return state?.[issueKey] ?? { dismissed: [], last_read_seq: 0, pinned: false };
}
