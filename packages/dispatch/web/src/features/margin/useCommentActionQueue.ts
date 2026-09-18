import { useMutation } from "@tanstack/react-query";
import { useRef, useState } from "react";

import { api } from "../../api/client";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";

export type CommentAction = "accept" | "reject" | "resolve" | "reopen";

export interface CommentActionInput {
  id: string;
  kind: CommentAction;
}

interface CommentActionQueueOptions<TContext> {
  onError?(error: Error, input: CommentActionInput, context: TContext | undefined): void;
  onMutate?(input: CommentActionInput): TContext | Promise<TContext>;
  onSuccess?(): void;
}

function submitCommentAction({ id, kind }: CommentActionInput) {
  if (kind === "accept") return api.acceptComment(id);
  if (kind === "reject") return api.rejectComment(id);
  if (kind === "reopen") return api.reopenComment(id);
  return api.resolveComment(id);
}

/**
 * Serializes comment state changes everywhere comments render. A second click on the same action
 * is ignored; a click on another thread is retained until the current request settles. A failure
 * clears that pending work so the reader can correct or retry the failed action explicitly.
 */
export function useCommentActionQueue<TContext = undefined>({
  onError,
  onMutate,
  onSuccess,
}: CommentActionQueueOptions<TContext>) {
  const actionGuard = useSubmitGuard();
  const inFlight = useRef<CommentActionInput | undefined>(undefined);
  const queued = useRef(new Map<string, CommentAction>());
  const [queuedIds, setQueuedIds] = useState<ReadonlySet<string>>(() => new Set());
  const syncQueuedIds = () => setQueuedIds(new Set(queued.current.keys()));
  const action = useMutation<unknown, Error, CommentActionInput, TContext>({
    mutationFn: submitCommentAction,
    onError,
    onMutate,
    onSettled: (_data, error) => {
      actionGuard.release();
      inFlight.current = undefined;
      if (error !== null) {
        queued.current.clear();
        syncQueuedIds();
        return;
      }
      onSuccess?.();
      const next = queued.current.entries().next();
      if (next.done) return;
      const [id, kind] = next.value;
      queued.current.delete(id);
      syncQueuedIds();
      start({ id, kind });
    },
  });
  function start(input: CommentActionInput): boolean {
    return actionGuard.guard(() => {
      inFlight.current = input;
      action.mutate(input);
    });
  }

  const activeId = action.isPending ? action.variables?.id : undefined;
  const pendingActionIds =
    activeId === undefined ? queuedIds : new Set<string>([activeId, ...queuedIds]);

  return {
    actionErrorId: action.isError ? action.variables?.id : undefined,
    mutateItem: (input: CommentActionInput) => {
      if (start(input)) return;
      const current = inFlight.current;
      if (current !== undefined && current.id === input.id && current.kind === input.kind) return;
      queued.current.set(input.id, input.kind);
      syncQueuedIds();
    },
    pendingActionIds,
    retryItem: () => {
      if (action.variables !== undefined) start(action.variables);
    },
  };
}
