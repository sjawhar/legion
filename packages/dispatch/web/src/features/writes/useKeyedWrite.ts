import {
  type DefaultError,
  type UseMutationOptions,
  type UseMutationResult,
  useMutation,
} from "@tanstack/react-query";

import type { WriteBounds } from "../../api/client";
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
 * then and rolls it back on anything but a landing. It runs whether or not the browser says it is
 * online (`networkMode: "always"`): the write's own tries wait out a network that is down, and a
 * mutation TanStack paused offline would make none, never go late and never offer Stop trying.
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
  return useMutation({
    ...options,
    mutationFn: (variables: TVariables) =>
      writes.run({
        inline: typeof inline === "function" ? inline(variables) : inline,
        label: label(variables),
        write: (bounds) => write(variables, bounds),
      }),
    networkMode: "always",
  });
}
