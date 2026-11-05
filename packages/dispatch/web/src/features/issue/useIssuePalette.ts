import type { IssuePriority } from "../../api/types";
import { useKeymap } from "../shell/keymap";
import type { IssueDrafts } from "./useIssueDrafts";
import type { IssuePriorityWrite } from "./useIssuePriority";

/**
 * The issue actions that are reachable from the command palette alone (`$mod+k`): closing and
 * reopening an issue are too consequential for a single key, and the priority has both the
 * palette and `0`-`3`. Each row runs the same handler and carries the same guard as the control
 * the identity block draws, so a row is offered exactly while that control would take a click.
 *
 * It registers into the `issue` scope `IssuePage` pushes; it never pushes one of its own.
 */
export function useIssuePalette({
  drafts,
  isClosed,
  priorityWrite,
  saving,
}: {
  drafts: Pick<IssueDrafts, "requestStatusSubmit">;
  isClosed: boolean;
  priorityWrite: IssuePriorityWrite;
  /** Whether a field write is in flight: a row offered while one is would submit a second. */
  saving: boolean;
}): void {
  useKeymap("issue", [
    {
      id: "close",
      keys: [],
      label: "Close issue",
      run: () => drafts.requestStatusSubmit("done"),
      // `disabled={saving}` on the control, and `isClosed` decides whether it is rendered at
      // all; a row offered while the write is in flight would submit a second one.
      when: () => !isClosed && !saving,
    },
    {
      id: "reopen",
      keys: [],
      label: "Reopen issue",
      run: () => drafts.requestStatusSubmit("backlog"),
      when: () => isClosed && !saving,
    },
    ...([0, 1, 2, 3] as const).map((level) => ({
      id: `set-p${level}`,
      keys: [],
      label: `Set priority P${level}`,
      run: () => priorityWrite.submit(level as IssuePriority),
      // The same condition as `PriorityEditor`'s `disabled`.
      when: () => !(isClosed || saving),
    })),
  ]);
}
