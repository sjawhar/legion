import { useCallback, useMemo, useRef } from "react";

export interface RetryableMutation<Input> {
  mutate: (input: Input) => void;
  variables: Input | undefined;
}

export interface SubmitGuard {
  /** Runs `fn` unless a previous guarded call is still in flight; a disabled-button
   * re-render lags a same-tick double click or Retry press, so this gates on a ref that
   * updates synchronously instead of on a mutation's (React-state) `isPending`. */
  guard: (fn: () => void) => boolean;
  /** Whether a guarded call is in flight. Unlike mutation state, this is current in the same task
   * that began the call, so a caller can hold every control before React has re-rendered it. */
  held: () => boolean;
  /** Clears the guard. A caller that allows one press per request calls it from the mutation's
   * `onSettled`; one that only drops a same-tick double press, letting the next press through
   * while the first is still out (the broadcast queue, `features/agents/BroadcastSends.tsx`),
   * calls it on the next task. */
  release: () => void;
  /** Repeats the mutation's latest input if a request is not already in flight. */
  retryLast: <Input>(mutation: RetryableMutation<Input>) => boolean;
}

export function useSubmitGuard(): SubmitGuard {
  const inFlight = useRef(false);
  const guard = useCallback((fn: () => void): boolean => {
    if (inFlight.current) {
      return false;
    }
    inFlight.current = true;
    fn();
    return true;
  }, []);
  const held = useCallback(() => inFlight.current, []);
  const release = useCallback(() => {
    inFlight.current = false;
  }, []);
  const retryLast = useCallback(
    <Input>({ mutate, variables }: RetryableMutation<Input>): boolean => {
      if (variables === undefined) {
        return false;
      }
      return guard(() => mutate(variables));
    },
    [guard]
  );
  return useMemo(() => ({ guard, held, release, retryLast }), [guard, held, release, retryLast]);
}
