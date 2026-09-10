import { useRef } from "react";

export interface RetryableMutation<Input> {
  mutate: (input: Input) => void;
  variables: Input | undefined;
}

export interface SubmitGuard {
  /** Runs `fn` unless a previous guarded call is still in flight; a disabled-button
   * re-render lags a same-tick double click or Retry press, so this gates on a ref that
   * updates synchronously instead of on a mutation's (React-state) `isPending`. */
  guard: (fn: () => void) => boolean;
  /** Clears the guard; call from the mutation's `onSettled`. */
  release: () => void;
  /** Repeats the mutation's latest input if a request is not already in flight. */
  retryLast: <Input>(mutation: RetryableMutation<Input>) => boolean;
}

export function useSubmitGuard(): SubmitGuard {
  const inFlight = useRef(false);
  const guard = (fn: () => void): boolean => {
    if (inFlight.current) {
      return false;
    }
    inFlight.current = true;
    fn();
    return true;
  };
  return {
    guard,
    release: () => {
      inFlight.current = false;
    },
    retryLast: <Input>({ mutate, variables }: RetryableMutation<Input>): boolean => {
      if (variables === undefined) {
        return false;
      }
      return guard(() => mutate(variables));
    },
  };
}
