import { type FormEvent, type ReactNode, useEffect, useRef, useState } from "react";

import type { IssueDetails } from "../../api/types";
import {
  borderTransparent,
  dangerText,
  inputClasses,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonText,
  textMutedHoverToSecondary,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { HELP_LINES, PROPERTY_LABELS, pageChrome } from "./page-structure";
import type { IssueDrafts } from "./useIssueDrafts";
import type { IssueHeaderWrites } from "./useIssueHeaderWrites";

/**
 * A property of the issue the identity block opens an editor for, one editor at a time: the
 * three the reader types rather than picks. Every other property is edited by the control that
 * draws it, so it needs no editor of its own.
 */
export type IssueField = "blocked_by" | "parent" | "route";

/** The issues `value` names, in the order they were typed, with every repeat and blank dropped:
 *  what a reader types into the blocked-by editor as the model's own list. */
export function issueKeysIn(value: string): string[] {
  const keys = value
    .split(/[\s,]+/u)
    .map((key) => key.trim().toUpperCase())
    .filter((key) => key !== "");
  return [...new Set(keys)];
}

export function IssueFieldForms({
  field,
  isClosed,
  issue,
  onClose,
  writes,
}: {
  /** Which editor is open, and `null` while none is. */
  field: IssueField | null;
  isClosed: boolean;
  issue: IssueDetails;
  onClose: () => void;
  writes: IssueHeaderWrites;
}): ReactNode {
  if (field === "route") {
    return (
      <RouteForm
        drafts={writes.drafts}
        isClosed={isClosed}
        onClose={onClose}
        saving={writes.updateIssue.isPending}
      />
    );
  }
  if (field === "parent") {
    return (
      <ParentForm
        drafts={writes.drafts}
        isClosed={isClosed}
        issue={issue}
        onClose={onClose}
        saving={writes.updateIssue.isPending}
      />
    );
  }
  if (field === "blocked_by") {
    return <BlockedByForm issue={issue} onClose={onClose} writes={writes} />;
  }
  return null;
}

/** The buttons every one of these editors ends on: save, then cancel. */
function FormActions({
  cancel,
  saveLabel,
  saving,
}: {
  cancel: () => void;
  saveLabel: string;
  saving: boolean;
}): ReactNode {
  return (
    <>
      <button
        className={`min-h-11 rounded px-2 py-1 text-sm font-medium disabled:cursor-not-allowed md:min-h-8 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonDisabledText}`}
        disabled={saving}
        type="submit"
        {...pageChrome}
      >
        {saveLabel}
      </button>
      <button
        className={`min-h-11 rounded border px-2 py-1 text-sm font-medium md:min-h-8 ${borderTransparent} ${textMutedHoverToSecondary}`}
        onClick={cancel}
        type="button"
        {...pageChrome}
      >
        Cancel
      </button>
    </>
  );
}

/** Where the issue's messages go. The sentence that says what a route is and what it is not was
 *  a tooltip: it is drawn here, where a reader is deciding what to set (R11). */
function RouteForm({
  drafts,
  isClosed,
  onClose,
  saving,
}: {
  drafts: IssueDrafts;
  isClosed: boolean;
  onClose: () => void;
  saving: boolean;
}): ReactNode {
  const input = useRef<HTMLInputElement>(null);
  // Focus the input once when the editor opens: the trigger button unmounts, so without this
  // focus would drop to the body and a keyboard user would have to find the form again.
  useEffect(() => {
    input.current?.focus();
  }, []);
  const save = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (drafts.requestRouteSubmit()) {
      onClose();
    }
  };
  return (
    <form className="mt-2 flex flex-wrap items-center gap-2" onSubmit={save}>
      <label
        className={`text-sm font-medium ${textSecondaryOnSurface}`}
        htmlFor="issue-route"
        {...pageChrome}
      >
        {PROPERTY_LABELS.route}
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
            onClose();
          }
        }}
        placeholder="role:legion-controller-core"
        ref={input}
        value={drafts.route}
      />
      <FormActions
        cancel={() => {
          drafts.discardRoute();
          onClose();
        }}
        saveLabel="Save route"
        saving={isClosed || !drafts.routeIsValid || saving}
      />
      <span
        className={drafts.routeIsValid ? "sr-only" : `text-sm ${dangerText}`}
        id="issue-route-help"
      >
        A message route must be role:[a-z0-9-]+ or session:[0-9a-f-]{`{16,}`}.
      </span>
      <p
        className={`basis-full text-sm ${textMutedOnSurface}`}
        id="issue-route-hint"
        {...pageChrome}
      >
        {HELP_LINES.route}
      </p>
    </form>
  );
}

/** The issue this one sits under. */
function ParentForm({
  drafts,
  isClosed,
  issue,
  onClose,
  saving,
}: {
  drafts: IssueDrafts;
  isClosed: boolean;
  issue: IssueDetails;
  onClose: () => void;
  saving: boolean;
}): ReactNode {
  const input = useRef<HTMLInputElement>(null);
  useEffect(() => {
    input.current?.focus();
  }, []);
  const save = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (drafts.requestParentSubmit()) {
      onClose();
    }
  };
  return (
    <form className="mt-2 flex flex-wrap items-center gap-2" onSubmit={save}>
      <label
        className={`text-sm font-medium ${textSecondaryOnSurface}`}
        htmlFor="issue-parent"
        {...pageChrome}
      >
        {PROPERTY_LABELS.parent}
      </label>
      <input
        aria-describedby="issue-parent-help"
        className={`w-full rounded px-2 py-1 text-sm outline-none md:w-64 ${inputClasses(true)}`}
        disabled={isClosed}
        id="issue-parent"
        onChange={(event) => drafts.writeParent(event.target.value)}
        onKeyDown={(event) => {
          if (event.key === "Escape") {
            drafts.discardParent();
            onClose();
          }
        }}
        placeholder={`${issue.project}-12`}
        ref={input}
        value={drafts.parent}
      />
      <FormActions
        cancel={() => {
          drafts.discardParent();
          onClose();
        }}
        saveLabel="Save parent"
        saving={isClosed || saving}
      />
      <span className="sr-only" id="issue-parent-help">
        Parent issue key in {issue.project}; leave empty to clear the parent.
      </span>
    </form>
  );
}

/** The issues this one waits on, which the children section reads on a child's own row. */
function BlockedByForm({
  issue,
  onClose,
  writes,
}: {
  issue: IssueDetails;
  onClose: () => void;
  writes: IssueHeaderWrites;
}): ReactNode {
  const input = useRef<HTMLInputElement>(null);
  const [draft, setDraft] = useState(issue.blocked_by.join(" "));
  useEffect(() => {
    input.current?.focus();
  }, []);
  const isClosed = issue.closed_at !== null;
  const save = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    writes.updateIssue.mutate({ blocked_by: issueKeysIn(draft) });
    onClose();
  };
  return (
    <form className="mt-2 flex flex-wrap items-center gap-2" onSubmit={save}>
      <label
        className={`text-sm font-medium ${textSecondaryOnSurface}`}
        htmlFor="issue-blocked-by"
        {...pageChrome}
      >
        {PROPERTY_LABELS.blocked_by}
      </label>
      <input
        aria-describedby="issue-blocked-by-help"
        className={`w-full rounded px-2 py-1 text-sm outline-none md:w-64 ${inputClasses(true)}`}
        disabled={isClosed}
        id="issue-blocked-by"
        onChange={(event) => setDraft(event.target.value)}
        onKeyDown={(event) => {
          if (event.key === "Escape") {
            onClose();
          }
        }}
        placeholder={`${issue.project}-12 ${issue.project}-13`}
        ref={input}
        value={draft}
      />
      <FormActions
        cancel={onClose}
        saveLabel="Save blocked by"
        saving={isClosed || writes.updateIssue.isPending}
      />
      <span className="sr-only" id="issue-blocked-by-help">
        Issue keys in {issue.project}, separated by spaces; leave empty to clear them.
      </span>
    </form>
  );
}
