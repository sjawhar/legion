import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useCallback, useState } from "react";

import { ApiError, api } from "../../api/client";
import { boardMoveMutationKey } from "../../api/queries";
import type { IssueSummary, UpdateIssueInput } from "../../api/types";
import { type IssueStatus, moveIssue } from "./board-model";
import { projectIssuesQueryKey } from "./issue-filters";

export const staleBoardMessage =
  "The board changed while you were moving this card - refreshed, try again.";

const everyIssue = () => true;

/**
 * The one path every board move takes - pointer, touch and keyboard drops alike. `moveCard`
 * places the card optimistically, sends the PATCH naming the visible neighbours, and refetches
 * the project list once the server has answered. Moves on one board are serialised through a
 * single mutation scope, so a second drop computes its neighbours from the first drop's
 * optimistic state and never overtakes it on the wire. A list the server answered before the
 * move never lands over the optimistic placement: a fetch already on the wire is cancelled at the
 * optimistic write, and one that starts while a move is unanswered waits for the answer
 * (`projectIssuesQuery`, keyed on `boardMoveMutationKey`). Any failure rolls the board back,
 * refetches it, and says so: a stale rank (`RANK_INPUT`, the neighbours moved under the drag)
 * gets `staleBoardMessage`; anything else the server's message.
 *
 * With filters active, `labels` names the query the board actually renders (the same key the
 * List shares), so the optimistic write, rollback and refetch hit the list on screen; a
 * successful move also refetches the unfiltered list, whose rank changed too. `isVisible` is the
 * board's filter predicate: insertion indices count visible cards, the PATCH names visible
 * neighbours, and hidden issues keep their cache positions.
 */
export function useBoardMoves(
  project: string,
  labels: readonly string[] = [],
  isVisible: (issue: IssueSummary) => boolean = everyIssue
): {
  error: string | undefined;
  moveCard: (key: string, targetStatus: IssueStatus, insertionIndex: number) => Promise<void>;
} {
  const queryClient = useQueryClient();
  const [error, setError] = useState<string>();
  const patch = useMutation({
    mutationFn: ({ input, key }: { input: UpdateIssueInput; key: string }) =>
      api.patchIssue(key, input),
    mutationKey: boardMoveMutationKey(project),
    scope: { id: `board:${project}` },
  });
  const { mutateAsync } = patch;
  const moveCard = useCallback(
    async (key: string, targetStatus: IssueStatus, insertionIndex: number) => {
      const queryKey = projectIssuesQueryKey(project, labels);
      const previous = queryClient.getQueryData<IssueSummary[]>(queryKey);
      if (previous === undefined) {
        return;
      }
      const moved = moveIssue(previous, key, targetStatus, insertionIndex, isVisible);
      if (moved === undefined) {
        return;
      }
      // Not awaited: the cancel takes effect synchronously, and the write must land in this
      // tick so a second move computes from it. The refetch below restores what it drops.
      void queryClient.cancelQueries({ queryKey, exact: true });
      queryClient.setQueryData(queryKey, moved.issues);
      try {
        await mutateAsync({ input: moved.input, key });
        setError(undefined);
        if (labels.length > 0) {
          await queryClient.invalidateQueries({ queryKey: projectIssuesQueryKey(project, []) });
        }
      } catch (cause) {
        queryClient.setQueryData(queryKey, previous);
        setError(
          cause instanceof ApiError
            ? cause.code === "RANK_INPUT"
              ? staleBoardMessage
              : cause.message
            : "Could not move the card."
        );
      } finally {
        await queryClient.invalidateQueries({ queryKey });
      }
    },
    [isVisible, labels, mutateAsync, project, queryClient]
  );
  return { error, moveCard };
}
