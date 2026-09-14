import type { ReactNode, Ref, SyntheticEvent } from "react";

import type { IssuePriority } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { badgeLow, hasFocusVisibleRing, priorityBadge } from "../../theme/classes";
import { useIssuePriority } from "./useIssuePriority";

const priorityOptions: readonly { label: string; value: IssuePriority | null }[] = [
  { label: "Unset", value: null },
  { label: "P0", value: 0 },
  { label: "P1", value: 1 },
  { label: "P2", value: 2 },
  { label: "P3", value: 3 },
];

function stopEvent(event: SyntheticEvent): void {
  event.stopPropagation();
}

/**
 * The single priority editor: shows the issue's priority as the `P0`–`P3` badge (a muted
 * "Priority" tag when unset) and, on click, tap, Enter or Space, opens a native `<select>` to
 * pick P0–P3 or Unset — the same picker on desktop and iPhone. The select is laid invisibly over
 * the badge so the tap target stays 44 px (32 px from `md`) while the badge keeps its size, and
 * the wrapper draws the keyboard focus ring. The write goes through `useIssuePriority`, so every
 * surface gets the same optimistic update, rollback and list refetch. `ref` reaches the select so
 * a keyboard shortcut can focus it; `stopPropagation` keeps pointer and key events from a
 * clickable or draggable parent.
 */
export function PriorityControl({
  disabled = false,
  issueKey,
  priority,
  ref,
  stopPropagation = false,
}: {
  disabled?: boolean;
  issueKey: string;
  priority: IssuePriority | null;
  ref?: Ref<HTMLSelectElement>;
  stopPropagation?: boolean;
}): ReactNode {
  const write = useIssuePriority(issueKey);
  const tone = priority === null ? badgeLow : priorityBadge[priority];
  const stop = stopPropagation ? stopEvent : undefined;
  return (
    <>
      <span
        className={`relative inline-flex min-h-11 shrink-0 items-center rounded-sm md:min-h-8 has-focus-visible:ring-2 ${hasFocusVisibleRing} ${
          disabled ? "opacity-50" : ""
        }`}
      >
        <span
          aria-hidden="true"
          className={`inline-flex items-center rounded-sm px-1.5 py-0.5 text-xs font-semibold ${tone.bg} ${tone.text}`}
        >
          {priority === null ? "Priority" : `P${priority}`}
        </span>
        <select
          aria-label={`Priority of ${issueKey}`}
          className="absolute inset-0 size-full cursor-pointer appearance-none opacity-0 disabled:cursor-not-allowed"
          disabled={disabled || write.pending}
          onChange={(event) => {
            const { value } = event.target;
            write.submit(value === "" ? null : (Number(value) as IssuePriority));
          }}
          onClick={stop}
          onKeyDown={stop}
          onPointerDown={stop}
          onTouchStart={stop}
          ref={ref}
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
