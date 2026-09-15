import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useCallback, useState } from "react";

import { ApiError, api } from "../../api/client";
import type { IssueSummary, UpdateIssueInput } from "../../api/types";
import { type IssueStatus, moveIssue } from "./board-model";

export const staleBoardMessage =
  "The board changed while you were moving this card - refreshed, try again.";

/**
 * The one path every board move takes - pointer, touch and keyboard drops alike. `moveCard`
 * places the card optimistically, sends the PATCH naming the visible neighbours, and refetches
 * the project list once the server has answered. Moves on one board are serialised through a
 * single mutation scope, so a second drop computes its neighbours from the first drop's
 * optimistic state and never overtakes it on the wire. Any failure rolls the board back,
 * refetches it, and says so: a stale rank (`RANK_INPUT`, the neighbours moved under the drag)
 * gets `staleBoardMessage`; anything else the server's message.
 */
export function useBoardMoves(project: string): {
  error: string | undefined;
  moveCard: (key: string, targetStatus: IssueStatus, insertionIndex: number) => Promise<void>;
} {
  const queryClient = useQueryClient();
  const [error, setError] = useState<string>();
  const patch = useMutation({
    mutationFn: ({ input, key }: { input: UpdateIssueInput; key: string }) =>
      api.patchIssue(key, input),
    scope: { id: `board:${project}` },
  });
  const { mutateAsync } = patch;
  const moveCard = useCallback(
    async (key: string, targetStatus: IssueStatus, insertionIndex: number) => {
      const queryKey = ["issues", "project", project] as const;
      const previous = queryClient.getQueryData<IssueSummary[]>(queryKey);
      if (previous === undefined) {
        return;
      }
      const moved = moveIssue(previous, key, targetStatus, insertionIndex);
      if (moved === undefined) {
        return;
      }
      queryClient.setQueryData(queryKey, moved.issues);
      try {
        await mutateAsync({ input: moved.input, key });
        setError(undefined);
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
    [mutateAsync, project, queryClient]
  );
  return { error, moveCard };
}
