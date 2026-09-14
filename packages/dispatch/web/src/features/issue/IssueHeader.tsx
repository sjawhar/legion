import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type KeyboardEvent, type ReactNode, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import { mergeIssue } from "../../api/issue-cache";
import type { Artifact, Issue, IssueDetails, UserIssueState, UserState } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  badgeBlocking,
  badgeHigh,
  badgeLow,
  badgeMed,
  badgePrimary,
  bgTransparent,
  borderDefault,
  borderStrongHover,
  borderTransparent,
  calloutWarningBg,
  calloutWarningBorder,
  calloutWarningText,
  card,
  dangerText,
  focusBorder,
  focusVisibleRing,
  inputClasses,
  linkHoverText,
  linkText,
  primaryButtonBg,
  primaryButtonEnabledHoverBg,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  statusPill,
  statusPillDot,
  surfaceMutedStrongBg,
  textMutedHoverToSecondary,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryHoverToPrimary,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { ApprovalChip } from "../doc/ApprovalChip";
import { waitingOnYou } from "../inbox/BlockedOnYou";
import { openIssueStatuses, statusLabel } from "../project/board-model";
import { buildIssuePath } from "../refs/routes";
import { GitHubLink } from "./GitHubLink";
import { IssueLabels } from "./IssueLabels";
import { SubscribedAgents } from "./SubscribedAgents";
import { useIssueDrafts } from "./useIssueDrafts";

const priorityPills = [badgeBlocking, badgeHigh, badgeMed, badgeLow] as const;
const closedIssueStatuses = [...openIssueStatuses, "done"] as const;

export function IssueHeader({
  documentArtifact,
  isClosed,
  issue,
  state,
}: {
  documentArtifact: Artifact | undefined;
  isClosed: boolean;
  issue: IssueDetails;
  state: UserIssueState;
}): ReactNode {
  const queryClient = useQueryClient();
  const [editingTitle, setEditingTitle] = useState(false);
  const [routeEditing, setRouteEditing] = useState(false);
  const [subscribersOpen, setSubscribersOpen] = useState(false);
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
  const selectableStatuses = isClosed ? closedIssueStatuses : openIssueStatuses;
  const openAsks = issue.open_asks.filter((ask) => ask.state === "open");
  const waitingForHuman = waitingOnYou(openAsks);
  const waitingOnAgents = openAsks.length - waitingForHuman.length;
  const whoseTurn =
    openAsks.length === 0
      ? null
      : waitingForHuman.length > 0
        ? `Waiting on you (${waitingForHuman.length})`
        : `Waiting on agents (${waitingOnAgents})`;
  const approvalRequestOpen = openAsks.some((ask) => ask.kind === "approval");
  const showApprovalActions =
    documentArtifact?.approval !== undefined &&
    (documentArtifact.approval.state !== "draft" || approvalRequestOpen);
  const priorityPill = issue.priority === null ? badgeLow : priorityPills[issue.priority];
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
  const routeLabel = `Messages default to ${drafts.route === "" ? "no route" : drafts.route}`;

  return (
    <header className={`mb-3 rounded-xl border p-3 ${card}`} data-testid="issue-header">
      {isClosed ? (
        <div
          className={`mb-2 flex flex-wrap items-center justify-between gap-2 rounded-lg p-3 text-sm ${calloutWarningBorder} ${calloutWarningBg} ${calloutWarningText}`}
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
      <div className="flex flex-wrap items-center gap-2">
        <div className="flex min-w-[12rem] flex-1 flex-col gap-2 sm:flex-row sm:items-start xl:basis-full 2xl:basis-auto">
          <p
            className={`self-start shrink-0 rounded-md px-2.5 py-1.5 font-mono text-sm font-semibold ${badgePrimary.bg} ${badgePrimary.text}`}
          >
            {issue.key}
          </p>
          <div className="flex min-w-0 flex-1 items-start gap-2">
            {editingTitle ? (
              <input
                aria-describedby={drafts.titleError === null ? undefined : "issue-title-help"}
                aria-label="Issue title"
                className={`min-w-0 flex-1 rounded-lg border px-2 py-1 text-xl font-semibold outline-none md:py-0 ${borderTransparent} ${bgTransparent} ${textPrimaryOnSurface} ${borderStrongHover} ${focusBorder}`}
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
                className={`min-w-0 flex-1 break-words rounded-lg border px-2 py-1 text-xl font-semibold md:py-0 ${borderTransparent} ${textPrimaryOnSurface} line-clamp-2 ${
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
            <button
              aria-label={state.pinned ? "Unpin issue" : "Pin issue"}
              aria-pressed={state.pinned}
              className={`flex min-h-11 min-w-11 shrink-0 items-center justify-center rounded-lg md:min-h-8 md:min-w-8 ${
                state.pinned
                  ? `${primaryButtonBg} ${primaryButtonEnabledHoverBg}`
                  : `${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`
              }`}
              disabled={updateState.isPending}
              onClick={() => pinGuard.guard(() => updateState.mutate(!state.pinned))}
              title={state.pinned ? "Unpin issue" : "Pin issue"}
              type="button"
            >
              <svg aria-hidden="true" className="h-4 w-4" fill="none" viewBox="0 0 24 24">
                <path
                  d="M8 4h8l-1 6 3 3H6l3-3-1-6Zm4 9v7"
                  fill={state.pinned ? "currentColor" : "none"}
                  stroke="currentColor"
                  strokeWidth="1.8"
                />
              </svg>
            </button>
          </div>
        </div>
        <div
          className="flex w-full shrink-0 flex-wrap items-center gap-2 md:w-auto xl:contents 2xl:flex"
          data-testid="issue-state-actions"
        >
          <label
            className={`flex min-h-11 shrink-0 items-center gap-2 rounded-full border px-2 text-sm font-semibold uppercase tracking-wide sm:px-3 md:min-h-8 xl:px-2 ${borderDefault} ${statusPill.bg} ${statusPill.text}`}
          >
            <span
              aria-hidden="true"
              className={`hidden h-2 w-2 rounded-full sm:block ${statusPillDot}`}
            />
            <select
              aria-label="Status"
              className={`min-w-0 appearance-none font-inherit outline-none disabled:cursor-not-allowed disabled:opacity-50 ${bgTransparent} ${statusPill.text}`}
              disabled={isClosed || updateIssue.isPending}
              onChange={(event) => drafts.requestStatusSubmit(event.target.value)}
              value={issue.status}
            >
              {selectableStatuses.map((status) => (
                <option key={status} value={status}>
                  {statusLabel(status)}
                </option>
              ))}
            </select>
          </label>
          <select
            aria-label="Priority"
            className={`min-h-11 shrink-0 rounded-full border px-2 py-2 text-sm font-semibold outline-none disabled:cursor-not-allowed disabled:opacity-50 sm:px-3 md:min-h-8 md:py-1 xl:px-2 ${borderDefault} ${priorityPill.bg} ${priorityPill.text}`}
            disabled={isClosed || updateIssue.isPending}
            onChange={(event) =>
              drafts.requestPrioritySubmit(
                event.target.value === "" ? null : (Number(event.target.value) as 0 | 1 | 2 | 3)
              )
            }
            value={issue.priority ?? ""}
          >
            <option value="">Priority —</option>
            <option value="0">P0</option>
            <option value="1">P1</option>
            <option value="2">P2</option>
            <option value="3">P3</option>
          </select>
          {documentArtifact === undefined ? null : (
            <ApprovalChip
              artifact={documentArtifact}
              layout="contents"
              showActions={showApprovalActions}
              variant="header"
            />
          )}
          {isClosed ? null : (
            <button
              aria-label="Close issue"
              className={`min-h-11 shrink-0 rounded-lg px-2 py-2 text-sm font-medium sm:px-3 md:min-h-8 md:py-1 xl:px-2 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
              disabled={updateIssue.isPending}
              onClick={() => drafts.requestStatusSubmit("done")}
              type="button"
            >
              {pendingStatus === "done" ? (
                "Closing…"
              ) : (
                <>
                  <span className="sm:hidden">Close</span>
                  <span className="hidden sm:inline">Close issue</span>
                </>
              )}
            </button>
          )}
        </div>
        <div
          className="flex min-w-0 flex-wrap items-center gap-2 md:[&_button]:min-h-7 md:[&_button]:py-0 xl:contents 2xl:flex"
          data-testid="issue-metadata-rail"
        >
          <div
            className={`flex shrink-0 items-center gap-2 text-sm xl:order-1 2xl:order-none ${textSecondaryOnSurface}`}
          >
            <span className="font-medium">Labels:</span>
            <IssueLabels
              disabled={isClosed}
              labels={issue.labels}
              onSave={(labels) => labelsMutation.mutateAsync(labels)}
              project={issue.project}
              saveError={labelsMutation.isError}
              saving={labelsMutation.isPending}
              variant="rail"
            />
          </div>
          <div
            className={`flex w-full shrink-0 flex-wrap items-center gap-2 text-sm md:w-auto xl:order-1 2xl:order-none ${textSecondaryOnSurface}`}
          >
            <span className="font-medium">Route:</span>
            {routeEditing ? (
              <form
                className="flex w-full flex-wrap items-center gap-2 md:w-auto md:flex-nowrap"
                onSubmit={saveRoute}
              >
                <label className="sr-only" htmlFor="issue-route">
                  Route
                </label>
                <input
                  aria-describedby="issue-route-help"
                  className={`w-full rounded px-2 py-1 text-sm outline-none md:w-64 ${inputClasses(true)}`}
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
                  className={`min-h-11 rounded px-2 py-1 text-sm font-medium disabled:cursor-not-allowed ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonDisabledText}`}
                  disabled={isClosed || !drafts.routeIsValid || updateIssue.isPending}
                  type="submit"
                >
                  Save route
                </button>
                <button
                  className={`min-h-11 rounded border px-2 py-1 text-sm font-medium ${borderTransparent} ${textMutedHoverToSecondary}`}
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
                aria-label={routeLabel}
                className={`inline-flex min-h-11 max-w-[14ch] shrink-0 items-center truncate rounded-full px-3 py-2 text-left text-sm outline-none focus-visible:ring-2 xl:px-2 ${surfaceMutedStrongBg} ${textSecondaryOnSurface} ${textSecondaryHoverToPrimary} ${focusVisibleRing}`}
                disabled={isClosed}
                onClick={() => setRouteEditing(true)}
                title={drafts.route === "" ? undefined : drafts.route}
                type="button"
              >
                {drafts.route === "" ? "No route" : drafts.route}
              </button>
            )}
          </div>
          <button
            aria-expanded={subscribersOpen}
            className={`flex min-h-11 shrink-0 items-center gap-2 rounded-lg px-2 py-1 text-sm font-medium ${textSecondaryHoverToPrimary}`}
            onClick={() => setSubscribersOpen((open) => !open)}
            type="button"
          >
            <span>Subscribers:</span>
            <span>{subscriberList.length}</span>
          </button>
          {issue.parent === null ? null : (
            <Link
              className={`shrink-0 text-sm underline ${linkText} ${linkHoverText}`}
              to={buildIssuePath({ key: issue.parent, kind: "issue" })}
            >
              Parent: {issue.parent}
            </Link>
          )}
          {issue.external_links.map((link) => (
            <div className="shrink-0" key={link.url}>
              <GitHubLink link={link} />
            </div>
          ))}
          {whoseTurn === null ? null : (
            <span
              className={`ml-auto shrink-0 rounded-full border px-3 py-1 text-xs font-semibold xl:hidden 2xl:ml-auto 2xl:inline-flex ${calloutWarningBorder} ${calloutWarningBg} ${calloutWarningText}`}
            >
              {whoseTurn}
            </span>
          )}
        </div>
      </div>
      {drafts.titleError === null ? null : (
        <p className={`mt-1 text-sm ${dangerText}`} id="issue-title-help">
          {drafts.titleError}
        </p>
      )}
      {statusSaving ? (
        <span className={`mt-1 block text-xs ${textMutedOnSurface}`} role="status">
          {pendingStatus === "done"
            ? "Closing…"
            : pendingStatus === "backlog" && isClosed
              ? "Reopening…"
              : "Saving…"}
        </span>
      ) : null}
      {subscribersOpen ? (
        <SubscribedAgents
          onUnsubscribe={(sessionId) => unsubscribe.mutate(sessionId)}
          ownerLabel={issue.key}
          subscribers={subscriberList}
        />
      ) : null}
      {subscribers.isError ? (
        <QueryError
          message="Subscribed agents unavailable — Envoy listener unreachable."
          onRetry={() => subscribers.refetch()}
          retrying={subscribers.isFetching}
        />
      ) : null}
      {updateState.isError ? (
        <QueryError
          message="Could not save pin status."
          onRetry={() => pinGuard.retryLast(updateState)}
          retrying={updateState.isPending}
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
