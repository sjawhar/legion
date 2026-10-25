import {
  type DefaultError,
  type UseMutationOptions,
  type UseMutationResult,
  useMutation,
} from "@tanstack/react-query";
import { useLayoutEffect, useRef } from "react";

import { AnswerNotKeptError, type WriteBounds } from "../../api/client";
import { newSendKey } from "../../lib/send-key";
import { retriesUnderItsKey } from "./keyed-write";
import { usePendingWrites } from "./pending-writes";

/** A write a surface makes for the reader: `useMutation`'s options, but for `mutationFn`, which
 *  the write path makes from `write` and `label`. */
export interface KeyedWriteOptions<TData, TVariables, TContext>
  extends Omit<
    UseMutationOptions<TData, DefaultError, TVariables, TContext>,
    "mutationFn" | "networkMode"
  > {
  /** What the shell's pending-writes indicator names this write, in the reader's words. */
  readonly label: (variables: TVariables) => string;
  /** One attempt of the write, under the bounds the queue gives it: its key and its deadline. */
  readonly write: (variables: TVariables, bounds: WriteBounds) => Promise<TData>;
  /** Whether this surface shows a refusal itself (a form the reader is in front of): the
   *  indicator then shows the write only while it is out. A function decides it per write. */
  readonly inline?: boolean | ((variables: TVariables) => boolean);
}

/**
 * The one way a surface that is not a composer writes to Dispatch: each `mutate` is one press,
 * made under a key of its own and tried under it until Dispatch decides it, shown by the shell's
 * pending-writes indicator while it is out and, refused, with Retry and Discard
 * (`PendingWrites.run`). The mutation settles with Dispatch's answer, its refusal, or
 * `SendStoppedError` once the reader stops it, so the surface keeps its optimistic state until
 * then and rolls it back on anything but a landing. A `mutate` of the very variables a press the
 * reader stopped was made with is that press made again, under its key - Dispatch may have taken
 * it, and answers the key with what it did - while one after Dispatch's own refusal is a new
 * press, since Dispatch would answer the old key with that refusal. It runs whether or not the
 * browser says it is online (`networkMode: "always"`): the write's own tries wait out a network
 * that is down, and a mutation TanStack paused offline would make none, never go late and never
 * offer Stop trying.
 */
export function useKeyedWrite<TData, TVariables = void, TContext = unknown>({
  inline,
  label,
  write,
  ...options
}: KeyedWriteOptions<TData, TVariables, TContext>): UseMutationResult<
  TData,
  DefaultError,
  TVariables,
  TContext
> {
  const writes = usePendingWrites();
  /** The last press the reader stopped, which a Retry of the same variables makes again. */
  const stopped = useRef<{ readonly key: string; readonly variables: TVariables } | undefined>(
    undefined
  );
  const { onError, onSettled } = options;
  const mutation = useMutation({
    ...options,
    mutationFn: (variables: TVariables) => {
      const again = stopped.current;
      stopped.current = undefined;
      const key =
        again !== undefined && Object.is(again.variables, variables) ? again.key : newSendKey();
      return writes
        .run({
          inline: typeof inline === "function" ? inline(variables) : inline,
          key,
          label: label(variables),
          write: (bounds) => write(variables, bounds),
        })
        .catch((error: unknown) => {
          if (error instanceof Error && retriesUnderItsKey(error)) {
            stopped.current = { key, variables };
          }
          throw error;
        });
    },
    networkMode: "always",
    // A write that landed with no answer to give (`AnswerNotKeptError`) is no refusal: the queue
    // has read again everything it could have changed, so the surface rolls nothing back and
    // settles as on a landing, with no error.
    onError: (error, ...rest) =>
      error instanceof AnswerNotKeptError ? undefined : onError?.(error, ...rest),
    onSettled: (data, error, ...rest) =>
      onSettled?.(data, error instanceof AnswerNotKeptError ? null : error, ...rest),
  });
  // Nor does the surface show it as one: the mutation is reset before the browser paints it.
  const landedWithoutAnswer = mutation.error instanceof AnswerNotKeptError;
  const { reset } = mutation;
  useLayoutEffect(() => {
    if (landedWithoutAnswer) reset();
  }, [landedWithoutAnswer, reset]);
  return mutation;
}
