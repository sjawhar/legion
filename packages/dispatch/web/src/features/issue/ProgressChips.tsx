import type { ReactNode } from "react";

import type { IssueProgress, ProgressCount } from "../../api/types";
import { pillClassName } from "../../components/Pill";
import { progressBarFill, surfaceMutedStrongBg, textSecondaryOnSurface } from "../../theme/classes";

/** What a count means, for the chip's `title`. */
function countTitle(count: ProgressCount, noun: "tasks" | "children"): string {
  const what =
    noun === "tasks"
      ? "task-list items in the spec (`- [ ]` and `- [x]`, nested lists included) are checked"
      : "child issues are done";
  return `${count.done} of ${count.total} ${what}.`;
}

/**
 * An issue's progress wherever the issue is shown, counted as GitHub counts an issue's: its spec's
 * task-list items and its direct children, each done of total, and each only when the server
 * counted one - `null` is a spec with no task list, an issue with no child - so an issue with
 * neither renders nothing and a list of plain issues stays quiet. A List row and a Board card show
 * each as a chip; the issue header passes `bar` and gets each with a thin bar filled to done/total
 * beside its count, since the header has the room and a reader glancing at it wants the shape
 * before the number.
 */
export function ProgressChips({
  progress,
  bar = false,
}: {
  progress: IssueProgress;
  bar?: boolean;
}): ReactNode {
  const counts: { noun: "tasks" | "children"; count: ProgressCount }[] = [];
  if (progress.tasks !== null) counts.push({ noun: "tasks", count: progress.tasks });
  if (progress.children !== null) counts.push({ noun: "children", count: progress.children });
  if (counts.length === 0) {
    return null;
  }
  return (
    <>
      {counts.map(({ noun, count }) =>
        bar ? (
          <span
            className={`inline-flex shrink-0 items-center gap-2 text-xs ${textSecondaryOnSurface}`}
            data-testid={`issue-progress-${noun}`}
            key={noun}
            title={countTitle(count, noun)}
          >
            <span
              aria-hidden="true"
              className={`h-1.5 w-16 overflow-hidden rounded-full ${surfaceMutedStrongBg}`}
            >
              <span
                className={`block h-full rounded-full ${progressBarFill}`}
                style={{ width: `${count.total === 0 ? 0 : (100 * count.done) / count.total}%` }}
              />
            </span>
            <span className="whitespace-nowrap">{`${count.done}/${count.total} ${noun}`}</span>
          </span>
        ) : (
          <span
            className={pillClassName("label")}
            data-testid={`issue-progress-${noun}`}
            key={noun}
            title={countTitle(count, noun)}
          >
            {`${count.done}/${count.total} ${noun}`}
          </span>
        )
      )}
    </>
  );
}
