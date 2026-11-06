import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { PinButton } from "../../components/PinButton";
import { badgePrimary, linkHoverText, linkText, textMutedOnSurface } from "../../theme/classes";
import { CopyRefButton } from "../refs/CopyRefButton";
import { buildIssuePath } from "../refs/routes";
import { type IssueAction, IssueActionsMenu } from "./IssueActionsMenu";
import { authorText, pageChrome } from "./page-structure";

/**
 * Row one of the identity block: where the issue sits, what it is called, and what a reader does
 * to the issue itself. The parent is a breadcrumb rather than a field, because a reader reads it
 * to go there; the control that sets it is in the actions menu beside the pin.
 */
export function IssueIdentityRow({
  actions,
  issueKey,
  onPin,
  parent,
  pinSaving,
  pinned,
}: {
  actions: readonly IssueAction[];
  issueKey: string;
  onPin: () => void;
  /** The issue this one sits under, drawn as the way back up to it. */
  parent: string | null;
  pinSaving: boolean;
  pinned: boolean;
}): ReactNode {
  return (
    <div className="flex min-w-0 flex-wrap items-center gap-1">
      {parent === null ? null : (
        <span className={`flex shrink-0 items-center gap-1 text-sm ${textMutedOnSurface}`}>
          <Link
            className={`dispatch-inline-link shrink-0 underline ${linkText} ${linkHoverText}`}
            to={buildIssuePath({ key: parent, kind: "issue" })}
            {...authorText}
          >
            {parent}
          </Link>
          <span aria-hidden="true" {...pageChrome}>
            ›
          </span>
        </span>
      )}
      <p
        className={`rounded-md px-2.5 py-1.5 font-mono text-sm font-semibold ${badgePrimary.bg} ${badgePrimary.text}`}
        {...authorText}
      >
        {issueKey}
      </p>
      <CopyRefButton
        primary={{ label: `issue key ${issueKey}`, value: issueKey }}
        route={{ key: issueKey, kind: "issue" }}
      />
      <PinButton
        disabled={pinSaving}
        label={pinned ? "Unpin issue" : "Pin issue"}
        onClick={onPin}
        pinned={pinned}
      />
      <span className="ml-auto flex items-center gap-1">
        <IssueActionsMenu actions={actions} />
      </span>
    </div>
  );
}
