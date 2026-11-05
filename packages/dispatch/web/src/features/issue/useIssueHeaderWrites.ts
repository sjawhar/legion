import { useQueryClient } from "@tanstack/react-query";

import { api, apiErrorCodeIn, apiErrorMessage, issuePatchRequest } from "../../api/client";
import { mergeIssue } from "../../api/issue-cache";
import { userStateQuery } from "../../api/queries";
import type { Issue, IssueDetails, UserIssueState, UserState } from "../../api/types";
import { type SubmitGuard, useSubmitGuard } from "../../hooks/useSubmitGuard";
import { type KeyedWriteResult, useKeyedWrite } from "../writes/useKeyedWrite";
import { stateForIssue } from "./pins";
import { type IssueDrafts, type IssueUpdateInput, useIssueDrafts } from "./useIssueDrafts";

/** A title Dispatch refuses the same way every time, so its refusal is shown with no Retry. */
const titleRefusals = new Set(["CAP_EXCEEDED", "ISSUE_CLOSED"]);

/** Whether `input` is the Status pill's pick alone: that control shows its write in the shell's
 *  indicator and rolls back on a refusal, where every other header field holds its draft and
 *  says why itself. */
export function statusPickOnly(input: IssueUpdateInput | undefined): boolean {
  return input !== undefined && Object.keys(input).length === 1 && input.status !== undefined;
}

/** What the shell's indicator names one header write of `key`. */
function issueUpdateLabel(key: string, input: IssueUpdateInput): string {
  const field = Object.keys(input)[0] ?? "field";
  return input.status === undefined ? `${key}: ${field} saved` : `${key} set to ${input.status}`;
}

/**
 * Every write the identity block makes, in one place: the issue's own fields (the title, the
 * status, the route, the parent), its labels, its pin and its claim's release, with the drafts
 * those fields hold, and the few facts each row reads back from a write in flight - which status
 * a pick is saving, a parent or a title Dispatch refused.
 *
 * The rows that draw them hold no write of their own, so a field moved from one row to another -
 * or into the panel - changes where a control is drawn and nothing about how it is saved. Each
 * write is one write of the page's one store (`features/writes/`), so every one of them is
 * keyed, shown in the shell's indicator and kept across a reload.
 */
export interface IssueHeaderWrites {
  /** The drafts of the title, the route and the parent, and the submits that save them. */
  readonly drafts: IssueDrafts;
  /** The issue's own fields: the title, the status, the route, the parent. */
  readonly updateIssue: KeyedWriteResult<Issue, IssueUpdateInput, unknown>;
  /** The labels, which the popover saves as one set when it closes. */
  readonly labels: KeyedWriteResult<Issue, string[], unknown>;
  /** The pin, applied to the person's state at once and rolled back on a refusal. */
  readonly pin: KeyedWriteResult<UserIssueState, boolean, PinRollback>;
  /** One press at a time on the pin, whose write is optimistic. */
  readonly pinGuard: SubmitGuard;
  readonly releaseClaim: KeyedWriteResult<Issue, void, unknown>;
  /** The status a pick is saving right now, which the pill and the closed line show. */
  readonly pendingStatus: string | undefined;
  readonly statusSaving: boolean;
  /** A parent Dispatch refused, with the reason it gave, drawn where the parent is edited. */
  readonly parentSaveFailed: boolean;
  readonly parentError: string;
  /** A title Dispatch refuses every time (too long, or the issue is closed), or null. */
  readonly titleRefusal: string | null;
}

/** What the pin's optimistic write puts back when Dispatch refuses it. */
interface PinRollback {
  readonly previous: UserState | undefined;
}

export function useIssueHeaderWrites(
  issue: IssueDetails,
  state: UserIssueState
): IssueHeaderWrites {
  const queryClient = useQueryClient();
  // Every write here names `write` first: its answer and its variables are what the callbacks
  // below read.
  const releaseClaim = useKeyedWrite({
    write: (_variables, bounds) => api.releaseIssueClaim(issue.key, bounds),
    inline: true,
    label: () => `Claim on ${issue.key} released`,
    onSuccess: (next) => {
      if (next !== null) mergeIssue(queryClient, next);
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
    },
  });
  // A title, route, parent or close the header shows refused itself, beside its draft; a status
  // pick is a control, so its refusal is the shell's (`PendingWritesIndicator`), and the pick
  // rolls back. Every field write goes in the issue's line (`issue:<key>`), so an older one still
  // being tried never lands after a newer one, whatever left the page meanwhile. A landing with no
  // answer to give is merged by reading the issue again.
  const updateIssue = useKeyedWrite({
    inline: (input) => !statusPickOnly(input),
    label: (input) => issueUpdateLabel(issue.key, input),
    target: () => `issue:${issue.key}`,
    request: (input: IssueUpdateInput) => issuePatchRequest(issue.key, input),
    onSettled: (_data, error) => {
      drafts.onIssueSettled(error);
    },
    onSuccess: (next, input) => {
      if (next === null) {
        void queryClient.invalidateQueries({ queryKey: ["issue", issue.key] });
      } else {
        mergeIssue(queryClient, next);
      }
      drafts.onIssueSuccess(next ?? issue, input);
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
    },
  });
  const labels = useKeyedWrite({
    label: () => `Labels of ${issue.key}`,
    target: () => `issue:${issue.key}`,
    request: (next: string[]) => issuePatchRequest(issue.key, { labels: next }),
    onSuccess: (next) => {
      if (next !== null) mergeIssue(queryClient, next);
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
    },
  });
  const drafts = useIssueDrafts(issue, updateIssue);
  const pinGuard = useSubmitGuard();
  const pin = useKeyedWrite({
    label: (pinned: boolean) => `${issue.key} ${pinned ? "pinned" : "unpinned"}`,
    write: (pinned: boolean, bounds) => api.putIssueState(issue.key, { pinned }, bounds),
    onSettled: () => {
      pinGuard.release();
      void queryClient.invalidateQueries({ queryKey: userStateQuery().queryKey });
      // The sidebar lists the pinned issues from their own read, which follows the pin however
      // its write settled: with an answer, landed with none (`null`), or refused.
      void queryClient.invalidateQueries({ exact: true, queryKey: ["issues", "pinned"] });
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
      if (next === null) return;
      queryClient.setQueryData<UserState>(userStateQuery().queryKey, (current) => ({
        ...current,
        [issue.key]: next,
      }));
    },
  });

  const statusSaving = updateIssue.isPending && updateIssue.variables?.status !== undefined;
  return {
    drafts,
    labels,
    // A refused parent save (PARENT_INPUT, ISSUE_CLOSED) renders the server's reason inline,
    // like a route validation error, instead of the generic update failure line.
    parentError: apiErrorMessage(updateIssue.error, "Could not save parent."),
    parentSaveFailed: updateIssue.isError && updateIssue.variables?.parent !== undefined,
    pendingStatus: statusSaving ? updateIssue.variables?.status : undefined,
    pin,
    pinGuard,
    releaseClaim,
    statusSaving,
    // A title Dispatch refused (CAP_EXCEEDED past ISSUE_TITLE_MAX, ISSUE_CLOSED) shows its reason
    // with no Retry, since the same title is refused every time; any other failure keeps the
    // generic line and its Retry.
    titleRefusal:
      updateIssue.isError &&
      updateIssue.variables?.title !== undefined &&
      apiErrorCodeIn(updateIssue.error, titleRefusals)
        ? updateIssue.error.message
        : null,
    updateIssue,
  };
}
