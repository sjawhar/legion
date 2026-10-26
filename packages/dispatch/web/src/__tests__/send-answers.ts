import { spyOn } from "bun:test";

import { api, type WriteBounds, type WriteRequest } from "../api/client";

/** What a test answers the writes made from their request alone with (`answerSends`). */
export interface SendAnswers {
  /** Has `fn` answer every attempt of `request`, and hands `request` back. */
  readonly answer: <R extends WriteRequest>(
    request: R,
    fn: (bounds: WriteBounds) => Promise<unknown>
  ) => R;
  /** Every request answered here, in order. */
  readonly calls: readonly WriteRequest[];
  /** Puts `api.send` back. */
  readonly restore: () => void;
}

/**
 * Answers the writes a test makes from their request alone (`WriteSpec.request`, every plain write
 * on a line): each attempt of such a write is `api.send(request)` with the very request it named,
 * so `answer(request, fn)` has `fn` answer every attempt of it, telling `sent` the request first as
 * the client does. A request the test named no answer for goes through the real `api.send`.
 */
export function answerSends(): SendAnswers {
  const answers = new WeakMap<WriteRequest, (bounds: WriteBounds) => Promise<unknown>>();
  const calls: WriteRequest[] = [];
  const real = api.send.bind(api);
  const spy = spyOn(api, "send").mockImplementation(((
    request: WriteRequest,
    bounds: WriteBounds
  ) => {
    const answer = answers.get(request);
    if (answer === undefined) return real(request, bounds);
    calls.push(request);
    bounds.sent?.(request);
    return answer(bounds);
  }) as typeof api.send);
  return {
    answer: (request, fn) => {
      answers.set(request, fn);
      return request;
    },
    calls,
    restore: () => spy.mockRestore(),
  };
}
