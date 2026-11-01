import type { ReactNode } from "react";

import type { IssuePriority } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { badgeLow, priorityBadge } from "../../theme/classes";
import { badgeSelectBadge, badgeSelectOverlay, badgeSelectWrapper } from "./badge-select";
import { type IssuePriorityWrite, useIssuePriority } from "./useIssuePriority";

const priorityOptions: readonly { label: string; value: IssuePriority | null }[] = [
  { label: "Unset", value: null },
  { label: "P0", value: 0 },
  { label: "P1", value: 1 },
  { label: "P2", value: 2 },
  { label: "P3", value: 3 },
];

/**
 * The priority editor for a surface that owns no write of its own - a list row, a board card,
 * an Inbox row - where this control is the only thing that writes that issue's priority.
 */
export function PriorityControl({
  disabled,
  issueKey,
  priority,
}: {
  disabled?: boolean;
  issueKey: string;
  priority: IssuePriority | null;
}): ReactNode {
  const write = useIssuePriority(issueKey);
  return (
    <PriorityEditor disabled={disabled} issueKey={issueKey} priority={priority} write={write} />
  );
}

/**
 * The single priority editor: shows the issue's priority as the `P0`–`P3` badge (a muted
 * "Priority" tag when unset) and, on click, tap, Enter or Space, opens a native `<select>` to
 * pick P0–P3 or Unset — the same picker on desktop and iPhone. The select is laid invisibly over
 * the badge so the tap target stays 44 px (32 px from `md`) while the badge keeps its size, and
 * the wrapper draws the keyboard focus ring. `write` goes through `useIssuePriority`, so every
 * surface gets the same optimistic update, rollback and list refetch. The select stays enabled
 * while a save is in flight: a disabled control drops the focus it holds and leaves the tab
 * order, and a pick made meanwhile is queued behind the save rather than refused. A press on the
 * select stays on the select: a board card is a drag activator, and a held tap on the badge must
 * open the picker, not lift the card. The select is labelled `Priority of <KEY>`; the board's `p`
 * reaches it through the focused card.
 *
 * The host passes the write when it has other ways to set the same priority: the issue page
 * creates one for the page and shares it with the keyboard's `0`–`3` and the palette's Set
 * priority rows, so one refusal is reported once, here, by the control the reader is looking at.
 */
export function PriorityEditor({
  disabled = false,
  issueKey,
  priority,
  write,
}: {
  disabled?: boolean;
  issueKey: string;
  priority: IssuePriority | null;
  write: IssuePriorityWrite;
}): ReactNode {
  const tone = priority === null ? badgeLow : priorityBadge[priority];
  return (
    <>
      <span className={`${badgeSelectWrapper} ${disabled ? "opacity-50" : ""}`}>
        <span aria-hidden="true" className={`${badgeSelectBadge} ${tone.bg} ${tone.text}`}>
          {priority === null ? "Priority" : `P${priority}`}
        </span>
        <select
          aria-label={`Priority of ${issueKey}`}
          className={badgeSelectOverlay}
          disabled={disabled}
          onMouseDown={(event) => event.stopPropagation()}
          onPointerDown={(event) => event.stopPropagation()}
          onTouchStart={(event) => event.stopPropagation()}
          onChange={(event) => {
            const { value } = event.target;
            write.submit(value === "" ? null : (Number(value) as IssuePriority));
          }}
          value={priority ?? ""}
        >
          {priorityOptions.map((option) => (
            <option key={option.label} value={option.value ?? ""}>
              {option.label}
            </option>
          ))}
        </select>
      </span>
      {write.failed ? (
        <QueryError
          message={`Could not update the priority of ${issueKey}.`}
          onRetry={write.retry}
          retrying={write.pending}
        />
      ) : null}
    </>
  );
}
