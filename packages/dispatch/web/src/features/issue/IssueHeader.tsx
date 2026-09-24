import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  type FormEvent,
  type KeyboardEvent,
  type ReactNode,
  useEffect,
  useId,
  useRef,
  useState,
} from "react";
import { Link } from "react-router-dom";

import { api, apiErrorMessage } from "../../api/client";
import { mergeIssue } from "../../api/issue-cache";
import { userStateQuery } from "../../api/queries";
import type { Artifact, IssueDetails, UserIssueState, UserState } from "../../api/types";
import { PinButton } from "../../components/PinButton";
import { QueryError } from "../../components/QueryError";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
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
import { useAgents } from "../conversation/useAgents";
import { ApprovalChip } from "../doc/ApprovalChip";
import { waitingOnYou } from "../inbox/BlockedOnYou";
import { issueStatuses, openIssueStatuses, statusLabel } from "../project/board-model";
import { actorLabel } from "../refs/actor";
import { CopyRefButton } from "../refs/CopyRefButton";
import { ReferencedBy, ReferencedByToggle } from "../refs/ReferencedBy";
import { buildDispatchReference, buildIssuePath } from "../refs/routes";
import { AssigneeControl } from "./AssigneeControl";
import { ClaimChip } from "./ClaimChip";
import { GitHubLink } from "./GitHubLink";
import { IssueComponentsLine } from "./IssueComponentsLine";
import { IssueLabels } from "./IssueLabels";
import { PriorityControl } from "./PriorityControl";
import { stateForIssue } from "./pins";
import { SubscribedAgents } from "./SubscribedAgents";
import { type IssueUpdateInput, useIssueDrafts } from "./useIssueDrafts";

const routeHint =
  "New asks, comments, and messages on this issue wake this agent or role; replies inside a thread reach their participants directly. It is where messages go, not who is working the issue — that is the claim.";

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
  const routeInputRef = useRef<HTMLInputElement>(null);
  // Focus the route input once when editing opens: the trigger button unmounts, so without this
  // focus would drop to the body and a keyboard user would have to find the form again.
  useEffect(() => {
    if (routeEditing) {
      routeInputRef.current?.focus();
    }
  }, [routeEditing]);
  const [parentEditing, setParentEditing] = useState(false);
  const parentInputRef = useRef<HTMLInputElement>(null);
  // Same focus handoff as the route editor: the trigger button unmounts when the form opens.
  useEffect(() => {
    if (parentEditing) {
      parentInputRef.current?.focus();
    }
  }, [parentEditing]);
  const [subscribersOpen, setSubscribersOpen] = useState(false);
  const [referencesOpen, setReferencesOpen] = useState(false);
  const { titles: agentTitles } = useAgents(issue.created_by?.kind === "session");
  const openedBy = issue.created_by == null ? null : actorLabel(issue.created_by, agentTitles);
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
  const releaseClaim = useMutation({
    mutationFn: () => api.releaseIssueClaim(issue.key),
    onSuccess: (next) => {
      mergeIssue(queryClient, next);
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
    },
  });
  const updateIssue = useMutation({
    mutationFn: (input: IssueUpdateInput) => api.patchIssue(issue.key, input),
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
  const selectableStatuses = isClosed ? issueStatuses : openIssueStatuses;
  const openAsks = issue.open_asks.filter((ask) => ask.state === "open");
  // The detail carries waiting_on on every open ask; a response without it cannot say whose
  // turn it is, so the indicator stays off rather than shown wrong.
  const knowsWhoseTurn = openAsks.every((ask) => ask.waiting_on !== undefined);
  const waitingForHuman = waitingOnYou(openAsks);
  const waitingOnAgents = openAsks.length - waitingForHuman.length;
  const whoseTurn =
    openAsks.length === 0 || !knowsWhoseTurn
      ? null
      : waitingForHuman.length > 0
        ? `Waiting on you (${waitingForHuman.length})`
        : `Waiting on agents (${waitingOnAgents})`;
  const approvalRequestOpen = openAsks.some((ask) => ask.kind === "approval");
  const showApprovalActions =
    documentArtifact?.approval !== undefined &&
    (documentArtifact.approval.state !== "draft" || approvalRequestOpen);
  const pinGuard = useSubmitGuard();
  const updateState = useMutation({
    mutationFn: (pinned: boolean) => api.putIssueState(issue.key, { pinned }),
    onSettled: () => {
      pinGuard.release();
      void queryClient.invalidateQueries({ queryKey: userStateQuery().queryKey });
    },
    onMutate: async (pinned) => {
      await queryClient.cancelQueries({ queryKey: userStateQuery().queryKey });
      const previous = queryClient.getQueryData<UserState>(userStateQuery().queryKey);
      queryClient.setQueryData<UserState>(userStateQuery().queryKey, (current) => ({
        ...current,
        [issue.key]: { ...state, pinned },
      }));
      return { previous };
    },
    onError: (_error, _pinned, context) => {
      queryClient.setQueryData<UserState>(userStateQuery().queryKey, (current) => ({
        ...current,
        [issue.key]: stateForIssue(context?.previous, issue.key),
      }));
    },
    onSuccess: (next) => {
      queryClient.setQueryData<UserState>(userStateQuery().queryKey, (current) => ({
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
  const saveParent = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (drafts.requestParentSubmit()) {
      setParentEditing(false);
    }
  };
  // A refused parent save (PARENT_INPUT, ISSUE_CLOSED) renders the server's reason inline,
  // like a route validation error, instead of the generic update failure line.
  const parentSaveFailed = updateIssue.isError && updateIssue.variables?.parent !== undefined;
  const parentError = apiErrorMessage(updateIssue.error, "Could not save parent.");
  const routeLabel = `Messages default to ${drafts.route === "" ? "no route" : drafts.route}`;
  const issueReference = buildDispatchReference({ key: issue.key, kind: "issue" });
  const referencesPanelId = useId();
  // The title slot has one flex-basis whether it shows the heading or the editor: below 2xl the
  // title always takes its own row (basis-full) and the state controls and details line share
  // the row beneath it; from 2xl the slot is content-sized (basis-auto) so they join the title's
  // row when they fit. A basis that changed between view and edit mode would move every other
  // control on the blur a mousedown causes, and Chromium would deliver the mouseup — and so the
  // click — to a different element, swallowing the first click after a title edit.

  return (
    <header className={`mb-3 min-w-0 rounded-xl border p-3 ${card}`} data-testid="issue-header">
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
        <div className="flex min-w-0 grow basis-full flex-col gap-2 sm:flex-row sm:items-start 2xl:basis-auto">
          <div className="flex shrink-0 items-center gap-1 self-start">
            <p
              className={`rounded-md px-2.5 py-1.5 font-mono text-sm font-semibold ${badgePrimary.bg} ${badgePrimary.text}`}
            >
              {issue.key}
            </p>
            <CopyRefButton
              primary={{ label: `issue key ${issue.key}`, value: issue.key }}
              route={{ key: issue.key, kind: "issue" }}
            />
          </div>
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
            <PinButton
              disabled={updateState.isPending}
              label={state.pinned ? "Unpin issue" : "Pin issue"}
              onClick={() => pinGuard.guard(() => updateState.mutate(!state.pinned))}
              pinned={state.pinned}
            />
          </div>
        </div>
        <div
          className="flex w-full min-w-0 flex-wrap items-center gap-2 md:w-auto"
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
          <PriorityControl
            disabled={isClosed || updateIssue.isPending}
            issueKey={issue.key}
            priority={issue.priority}
          />
          <AssigneeControl
            assignee={issue.assignee}
            disabled={isClosed || updateIssue.isPending}
            issueKey={issue.key}
          />
          {documentArtifact === undefined ? null : (
            <ApprovalChip
              artifact={documentArtifact}
              layout="contents"
              showActions={showApprovalActions}
              variant="header"
            />
          )}
          {issue.claim === null ? null : (
            <>
              {/* A claim is state — who is working this, since when — so it sits with the
                  status, priority and assignee, and Release is an action beside Close. This
                  row wraps, so both stay visible and tappable at 390px; the metadata rail
                  below never wraps (#1211) and would clip them. */}
              <ClaimChip claim={issue.claim} />
              <button
                aria-label={`Release the claim on ${issue.key}`}
                className={`min-h-11 shrink-0 rounded-lg px-2 py-2 text-sm font-medium sm:px-3 md:min-h-8 md:py-1 xl:px-2 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
                disabled={releaseClaim.isPending}
                onClick={() => releaseClaim.mutate()}
                title="Release this claim so another agent can take the issue"
                type="button"
              >
                {releaseClaim.isPending ? "Releasing…" : "Release"}
              </button>
            </>
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
          className="flex min-w-0 flex-nowrap items-center gap-2 overflow-x-auto [scrollbar-width:thin] md:[&_button]:min-h-7 md:[&_button]:py-0"
          data-testid="issue-metadata-rail"
        >
          {whoseTurn === null ? null : (
            <span
              className={`inline-flex shrink-0 items-center rounded-full border px-3 py-1 text-xs font-semibold ${calloutWarningBorder} ${calloutWarningBg} ${calloutWarningText}`}
              data-testid="issue-whose-turn"
            >
              {whoseTurn}
            </span>
          )}
          {openedBy === null ? null : (
            <div className={`flex shrink-0 items-center gap-2 text-sm ${textSecondaryOnSurface}`}>
              <span className="font-medium">Opened by:</span>
              <span className="shrink-0 whitespace-nowrap" title={openedBy}>
                {openedBy}
              </span>
            </div>
          )}
          <div className={`flex shrink-0 items-center gap-2 text-sm ${textSecondaryOnSurface}`}>
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
          {routeEditing ? null : (
            <div className={`flex shrink-0 items-center gap-2 text-sm ${textSecondaryOnSurface}`}>
              <span aria-describedby="issue-route-hint" className="font-medium" title={routeHint}>
                Message route:
              </span>
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
            </div>
          )}
          <button
            aria-expanded={subscribersOpen}
            className={`flex min-h-11 shrink-0 items-center gap-2 rounded-lg px-2 py-1 text-sm font-medium ${textSecondaryHoverToPrimary}`}
            onClick={() => setSubscribersOpen((open) => !open)}
            type="button"
          >
            <span>Subscribers:</span>
            <span>{subscriberList.length}</span>
          </button>
          <ReferencedByToggle
            controls={referencesPanelId}
            count={issue.referenced_by_count}
            expanded={referencesOpen}
            onToggle={() => setReferencesOpen((open) => !open)}
          />
          {parentEditing ? null : (
            <div className={`flex shrink-0 items-center gap-2 text-sm ${textSecondaryOnSurface}`}>
              <span className="font-medium">Parent:</span>
              {issue.parent === null ? null : (
                <Link
                  className={`shrink-0 underline ${linkText} ${linkHoverText}`}
                  to={buildIssuePath({ key: issue.parent, kind: "issue" })}
                >
                  {issue.parent}
                </Link>
              )}
              <button
                aria-label={issue.parent === null ? "Set parent issue" : "Edit parent issue"}
                className={`inline-flex min-h-11 max-w-[14ch] shrink-0 items-center truncate rounded-full px-3 py-2 text-left text-sm outline-none focus-visible:ring-2 xl:px-2 ${surfaceMutedStrongBg} ${textSecondaryOnSurface} ${textSecondaryHoverToPrimary} ${focusVisibleRing}`}
                disabled={isClosed}
                onClick={() => setParentEditing(true)}
                type="button"
              >
                {issue.parent === null ? "None" : "Edit"}
              </button>
            </div>
          )}
          <IssueComponentsLine issue={issue} />
          {issue.external_links.map((link) => (
            <div className="flex shrink-0 items-center" key={link.url}>
              <GitHubLink link={link} />
            </div>
          ))}
        </div>
        {referencesOpen ? (
          <ReferencedBy
            className="mt-1 w-full"
            reference={issueReference}
            toggle={{ id: referencesPanelId }}
          />
        ) : null}
      </div>
      <span className="sr-only" id="issue-route-hint">
        {routeHint}
      </span>
      {routeEditing ? (
        <form className="mt-2 flex flex-wrap items-center gap-2" onSubmit={saveRoute}>
          <label
            aria-describedby="issue-route-hint"
            className={`text-sm font-medium ${textSecondaryOnSurface}`}
            htmlFor="issue-route"
            title={routeHint}
          >
            Message route:
          </label>
          <input
            aria-label="Message route"
            aria-describedby="issue-route-hint issue-route-help"
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
            ref={routeInputRef}
            value={drafts.route}
          />
          <button
            className={`min-h-11 rounded px-2 py-1 text-sm font-medium disabled:cursor-not-allowed md:min-h-8 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonDisabledText}`}
            disabled={isClosed || !drafts.routeIsValid || updateIssue.isPending}
            type="submit"
          >
            Save route
          </button>
          <button
            className={`min-h-11 rounded border px-2 py-1 text-sm font-medium md:min-h-8 ${borderTransparent} ${textMutedHoverToSecondary}`}
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
            A message route must be role:[a-z0-9-]+ or session:[0-9a-f-]{`{16,}`}.
          </span>
        </form>
      ) : null}
      {parentEditing ? (
        <form className="mt-2 flex flex-wrap items-center gap-2" onSubmit={saveParent}>
          <label className="sr-only" htmlFor="issue-parent">
            Parent
          </label>
          <span aria-hidden="true" className={`text-sm font-medium ${textSecondaryOnSurface}`}>
            Parent:
          </span>
          <input
            aria-describedby="issue-parent-help"
            className={`w-full rounded px-2 py-1 text-sm outline-none md:w-64 ${inputClasses(true)}`}
            disabled={isClosed}
            id="issue-parent"
            onChange={(event) => drafts.writeParent(event.target.value)}
            onKeyDown={(event) => {
              if (event.key === "Escape") {
                drafts.discardParent();
                setParentEditing(false);
              }
            }}
            placeholder={`${issue.project}-12`}
            ref={parentInputRef}
            value={drafts.parent}
          />
          <button
            className={`min-h-11 rounded px-2 py-1 text-sm font-medium disabled:cursor-not-allowed md:min-h-8 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonDisabledText}`}
            disabled={isClosed || updateIssue.isPending}
            type="submit"
          >
            Save parent
          </button>
          <button
            className={`min-h-11 rounded border px-2 py-1 text-sm font-medium md:min-h-8 ${borderTransparent} ${textMutedHoverToSecondary}`}
            onClick={() => {
              drafts.discardParent();
              setParentEditing(false);
            }}
            type="button"
          >
            Cancel
          </button>
          <span className="sr-only" id="issue-parent-help">
            Parent issue key in {issue.project}; leave empty to clear the parent.
          </span>
        </form>
      ) : null}
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
        parentSaveFailed ? (
          <p className={`mt-1 text-sm ${dangerText}`} id="issue-parent-error">
            {parentError}
          </p>
        ) : (
          <QueryError
            message="Could not update this issue."
            onRetry={drafts.retry}
            retrying={updateIssue.isPending}
          />
        )
      ) : null}
    </header>
  );
}
