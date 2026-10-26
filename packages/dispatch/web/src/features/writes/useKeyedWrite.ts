import {
  type DefaultError,
  type UseMutationOptions,
  type UseMutationResult,
  useMutation,
} from "@tanstack/react-query";
import { useCallback, useRef, useState } from "react";

import type { WriteBounds } from "../../api/client";
import { newSendKey } from "../../lib/send-key";
import { retriesUnderItsKey } from "./keyed-write";
import { plainWrite, useWrites } from "./write-store";

/** A write a surface makes for the reader: `useMutation`'s options, but for `mutationFn`, which
 *  the write path makes from `write` and `label`. Its data is Dispatch's answer, or `null` for a
 *  write Dispatch made and has no answer to give for (`409 KEY_COMMITTED`, or a replay of an answer
 *  it did not keep): a landing all the same, which `onSuccess` handles - a create form lets go of
 *  its draft and says what was made (`LandedWithoutAnswer`) rather than opening it. */
export interface KeyedWriteOptions<TData, TVariables, TContext>
  extends Omit<
    UseMutationOptions<TData | null, DefaultError, TVariables, TContext>,
    "mutationFn" | "networkMode"
  > {
  /** What the shell's pending-writes indicator names this write, in the reader's words. */
  readonly label: (variables: TVariables) => string;
  /** One attempt of the write, under the bounds the store gives it: its key, its login and its
   *  deadline. */
  readonly write: (variables: TVariables, bounds: WriteBounds) => Promise<TData>;
  /** Whether this surface shows a refusal itself (a form the reader is in front of): the
   *  indicator then shows the write only while it is out. A function decides it per write. */
  readonly inline?: boolean | ((variables: TVariables) => boolean);
  /** What the write is ordered on: writes to one target - one issue's fields - go out in press
   *  order, so an older one never lands after a newer one. */
  readonly target?: (variables: TVariables) => string | undefined;
  /** An upload's file, kept across a reload with the write. */
  readonly file?: (variables: TVariables) => File | undefined;
  /** Whether the write ends the person's session (a sign-out): a lapse of it lets the write go
   *  rather than holding it for the next sign-in (`WriteSpec.endsWithSession`). */
  readonly endsWithSession?: boolean;
}

/** Whether `a` and `b` are the same variables: one value, or two that serialise alike (a form
 *  built afresh at each submit); variables that do not serialise are only themselves. */
function sameVariables(a: unknown, b: unknown): boolean {
  if (Object.is(a, b)) return true;
  try {
    return JSON.stringify(a) === JSON.stringify(b);
  } catch {
    return false;
  }
}

/** A press Dispatch made and had no answer to give for: its variables, and when it landed. */
export interface UnansweredLanding<TVariables> {
  readonly variables: TVariables;
  readonly at: number;
}

/** `useMutation`'s result, with the one "made, but Dispatch lost its answer" state every create
 *  form shows (`LandedWithoutAnswer`): the last press that landed with `null`, until the next
 *  press starts or the form lets it go (`forgetUnanswered`, on an edit). */
export type KeyedWriteResult<TData, TVariables, TContext> = UseMutationResult<
  TData | null,
  DefaultError,
  TVariables,
  TContext
> & {
  readonly unanswered: UnansweredLanding<TVariables> | undefined;
  readonly forgetUnanswered: () => void;
};

/**
 * The one way a surface that is not a composer writes to Dispatch: each `mutate` is one press,
 * made under a key of its own and tried under it until Dispatch decides it, shown by the shell's
 * pending-writes indicator while it is out and, refused, with Retry and Discard (`Writes.run`).
 * The mutation settles with Dispatch's answer - `null` for a landing without one - its refusal, or
 * `SendStoppedError` once the reader stops it, so the surface keeps its optimistic state until
 * then and rolls it back on anything but a landing. A `mutate` of the same variables a press the
 * reader stopped was made with is that press made again, under its key - Dispatch may have taken
 * it, and answers the key with what it did - while one after Dispatch's own refusal is a new
 * press, since Dispatch would answer the old key with that refusal. It runs whether or not the
 * browser says it is online (`networkMode: "always"`): the write's own tries wait out a network
 * that is down, and a mutation TanStack paused offline would make none, never go late and never
 * offer Stop trying. A press that lands with no answer is kept as `unanswered` for the surface
 * to say so, until the next press or `forgetUnanswered`.
 */
export function useKeyedWrite<TData, TVariables = void, TContext = unknown>({
  endsWithSession,
  file,
  inline,
  label,
  target,
  write,
  ...options
}: KeyedWriteOptions<TData, TVariables, TContext>): KeyedWriteResult<TData, TVariables, TContext> {
  const writes = useWrites();
  /** The last press the reader stopped, which a Retry of the same variables makes again. */
  const stopped = useRef<{ readonly key: string; readonly variables: TVariables } | undefined>(
    undefined
  );
  const [unanswered, setUnanswered] = useState<UnansweredLanding<TVariables> | undefined>(
    undefined
  );
  const forgetUnanswered = useCallback(() => setUnanswered(undefined), []);
  const mutation = useMutation({
    ...options,
    mutationFn: (variables: TVariables) => {
      const again = stopped.current;
      stopped.current = undefined;
      setUnanswered(undefined);
      const key =
        again !== undefined && sameVariables(again.variables, variables) ? again.key : newSendKey();
      return writes
        .run({
          detail: null,
          endsWithSession,
          file: file?.(variables),
          inline: typeof inline === "function" ? inline(variables) : inline,
          key,
          kind: plainWrite,
          label: label(variables),
          presentation: "indicator",
          target: target?.(variables),
          write: (bounds) => write(variables, bounds),
        })
        .then(
          (answer) => {
            if (answer === null) setUnanswered({ at: Date.now(), variables });
            return answer;
          },
          (error: unknown) => {
            if (retriesUnderItsKey(error instanceof Error ? error : undefined)) {
              stopped.current = { key, variables };
            }
            throw error;
          }
        );
    },
    networkMode: "always",
  });
  return { ...mutation, forgetUnanswered, unanswered };
}
