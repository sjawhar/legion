import type { ReactNode } from "react";

import type { IssueProgress, ProgressCount } from "../../api/types";
import { pillClassName } from "../../components/Pill";
import { useTooltips } from "../../components/tooltips";
import { progressBarFill, surfaceMutedStrongBg, textSecondaryOnSurface } from "../../theme/classes";
import { authorText, pageChrome } from "./page-structure";
import { PROGRESS_NOUNS } from "./page-words";

/** The noun a count is written with: `1/1 task`, `3/7 tasks`, `0/1 child`, `2/5 children`. The
 *  words are the page's own, so they are declared where every other word of it is. */
function countNoun(total: number, noun: "tasks" | "children"): string {
  if (noun === "tasks") return total === 1 ? PROGRESS_NOUNS.task : PROGRESS_NOUNS.tasks;
  return total === 1 ? PROGRESS_NOUNS.child : PROGRESS_NOUNS.children;
}

/** What a count means, for the chip's `title`: plain text, since a title renders no markdown.
 *  The issue page draws no tooltips (`components/tooltips.tsx`); its children section spells
 *  both counts out with their labels instead (`IssueChildren.tsx`). */
function countTitle(count: ProgressCount, noun: "tasks" | "children"): string {
  const one = count.total === 1;
  const what =
    noun === "tasks"
      ? `task-list item${one ? "" : "s"} in the spec (- [ ] and - [x], nested lists included) ${one ? "is" : "are"} checked`
      : `child issue${one ? "" : "s"} ${one ? "is" : "are"} done`;
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
  /** Absent from an answer an older server gives during a deploy's overlap, which shows nothing. */
  progress?: IssueProgress;
  bar?: boolean;
}): ReactNode {
  const tooltips = useTooltips();
  if (progress === undefined) {
    return null;
  }
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
            title={tooltips ? countTitle(count, noun) : undefined}
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
            <span className="whitespace-nowrap" {...pageChrome}>
              <span {...authorText}>{`${count.done}/${count.total}`}</span>{" "}
              {countNoun(count.total, noun)}
            </span>
          </span>
        ) : (
          <span
            className={pillClassName("label")}
            data-testid={`issue-progress-${noun}`}
            key={noun}
            title={tooltips ? countTitle(count, noun) : undefined}
            {...pageChrome}
          >
            <span {...authorText}>{`${count.done}/${count.total}`}</span>{" "}
            {countNoun(count.total, noun)}
          </span>
        )
      )}
    </>
  );
}
