import { retriesUnderKey } from "@legion/contracts";

import {
  AnswerNotKeptError,
  ApiError,
  isKeyCommitted,
  isKeyInFlight,
  NetworkError,
  RequestTimeoutError,
  UnreadableAnswerError,
  type WriteBounds,
  type WriteRequest,
} from "../../api/client";
import { onReachable } from "../../api/reachability";

/**
 * How long a write may go unanswered before the page says so: past it the write is late, still
 * being tried under its key, and the reader may stop it. A composer also lets go of what it holds
 * the reader to - a host's Back, Escape or Collapse thread - at this point, since a request the
 * server never answers would hold them for good.
 */
export const SEND_DEADLINE_MS = 30_000;

/** How long one attempt of a write may go silent - no piece of its body sent, no piece of its
 *  answer received - before it is given up and made again under the same key. A write that keeps
 *  moving, a large upload on a slow line, is never cut off; Dispatch finishes a write whose
 *  attempt was given up, so the attempt after it is answered `409 KEY_IN_FLIGHT`, then with the
 *  first's answer. */
export const KEYED_SEND_ATTEMPT_MS = 15_000;

/** The wait before a write's first retry, doubling after each attempt that ends without Dispatch's
 *  verdict, up to `MAX_BACKOFF_MS`; any answer from Dispatch starts it over. */
export const FIRST_BACKOFF_MS = 1_000;
export const MAX_BACKOFF_MS = 30_000;

/** A write that stopped trying before Dispatch confirmed it: the reader pressed Stop trying. Not
 *  Dispatch's refusal: the write may have reached it, and a Retry under the same key is answered
 *  with what that first one did rather than done a second time. */
export class SendStoppedError extends Error {
  constructor(
    message = "Dispatch has not confirmed this message. Retry sends it again under the same key, so it is delivered at most once."
  ) {
    super(message);
    this.name = "SendStoppedError";
  }
}

/** A write a reload interrupted longer ago than `KEPT_SEND_RESUME_MS` (`write-storage.ts`), which
 *  the page does not make again on its own: the reader may no longer mean it. It stopped as a
 *  stopped write does, so it offers what a stopped write offers - a Retry under the same key, and
 *  Discard. */
export class SendInterruptedError extends SendStoppedError {
  constructor() {
    super(
      "Not sent: this page was closed before Dispatch confirmed it. Retry sends it under the same key, so it is delivered at most once."
    );
    this.name = "SendInterruptedError";
  }
}

/**
 * Whether a Retry of a write `refusal` ended goes under the write's own key. A refusal Dispatch
 * gave - any 4xx it answered - is its verdict on that key, which it would replay to the same key
 * for a day, so a Retry is the person's new action under a key of its own. A write that ended
 * without a verdict - stopped, or interrupted by a reload - may have landed, so its Retry asks
 * again under the same key, and Dispatch answers it with what the first one did.
 */
export function retriesUnderItsKey(refusal: Error | undefined): boolean {
  return refusal instanceof SendStoppedError;
}

/** What ended one attempt without Dispatch's verdict, and so makes the write again under its key:
 *  how long to wait first, and whether the page learning Dispatch can be reached again cuts that
 *  wait short. `undefined` for an answer that is Dispatch's verdict, or an error of the page's
 *  own, which ends the write at once. */
function retryAfter(
  error: unknown,
  backoff: number
): { readonly ms: number; readonly wakes: boolean } | undefined {
  if (
    error instanceof NetworkError ||
    error instanceof RequestTimeoutError ||
    error instanceof UnreadableAnswerError
  ) {
    return { ms: backoff, wakes: true };
  }
  if (!(error instanceof ApiError) || !retriesUnderKey(error.status, error.code)) return undefined;
  // Dispatch said when to come back: the first request under the key is still running, or it is
  // turning requests away for now. Nothing the page learns elsewhere changes that.
  if ((error.status === 429 && error.retryAfterMs !== undefined) || isKeyInFlight(error)) {
    return { ms: error.retryAfterMs ?? FIRST_BACKOFF_MS, wakes: false };
  }
  return { ms: backoff, wakes: true };
}

/**
 * Waits `ms`, and says whether it waited for a reason to try again: `false` once `signal`
 * aborts, at once. A wait that `wakes` ends early - but never before `FIRST_BACKOFF_MS` of it,
 * so an error answered alongside a busy page's reads is not asked again in a tight loop - when
 * the page learns Dispatch can be reached: it answered any request, the browser came back
 * online, the live socket opened (`reachability.ts`).
 */
function pause(ms: number, wakes: boolean, signal: AbortSignal | undefined): Promise<boolean> {
  if (signal?.aborted === true) return Promise.resolve(false);
  const { promise, resolve } = Promise.withResolvers<boolean>();
  const startedAt = Date.now();
  let timer = setTimeout(() => end(true), ms);
  function end(tryAgain: boolean): void {
    clearTimeout(timer);
    stopWaking();
    signal?.removeEventListener("abort", stop);
    resolve(tryAgain);
  }
  function stop(): void {
    end(false);
  }
  const stopWaking = wakes
    ? onReachable(() => {
        clearTimeout(timer);
        const floor = Math.min(ms, FIRST_BACKOFF_MS) - (Date.now() - startedAt);
        timer = setTimeout(() => end(true), Math.max(0, floor));
      })
    : () => {};
  signal?.addEventListener("abort", stop, { once: true });
  return promise;
}

/** How one keyed write is tried: `login` is the person who pressed it, sent on every attempt so
 *  Dispatch refuses it under anyone else's session; `signal` stops the trying; `sent` is told each
 *  attempt's request as it goes out. */
export interface KeyedWriteOptions {
  readonly login?: string;
  readonly sent?: (request: WriteRequest) => void;
  readonly signal?: AbortSignal;
}

/** A write whose attempts made more than one kind of request under one key: a page bug, since
 *  Dispatch would refuse the second as `422 KEY_REUSED`, and a queue keeping one request per write
 *  could make only one of them again after a reload. */
export class OneRequestPerWriteError extends Error {
  constructor(first: WriteRequest, next: WriteRequest) {
    super(
      `one write makes one request: ${first.method} ${first.path}, then ${next.method} ${next.path}`
    );
    this.name = "OneRequestPerWriteError";
  }
}

/**
 * Makes `attempt` under `key`, the person's action's name, until Dispatch decides it. Each attempt
 * is held to `KEYED_SEND_ATTEMPT_MS` of silence, and one that ends without Dispatch's verdict - the
 * connection failed (`NetworkError`), the attempt went silent (`RequestTimeoutError`), a 2xx was
 * not JSON (`UnreadableAnswerError`), or Dispatch answered a 5xx, a 408, a 429 or `409
 * KEY_IN_FLIGHT` (`retriesUnderKey`) - is made again after 1 s, 2 s, 4 s … never more than 30 s
 * apart, or after Dispatch's `Retry-After`; any answer from Dispatch to any request starts that
 * backoff over, and ends a wait early, as does the browser coming back online or the live socket
 * opening. However many attempts reach Dispatch, the key makes it act once. Resolves with the 2xx,
 * or `null` for a write that landed with no answer to give - `409 KEY_COMMITTED`, or a replay
 * that did not keep the first answer - and for a `DELETE` an earlier attempt of which may have
 * landed and which Dispatch now answers `404` (the thing is gone either way). Rejects with
 * anything else at once: a 4xx, its refusal; an error of the page's own. Every attempt must make
 * the same request (`OneRequestPerWriteError`). `signal` stops the trying between attempts - a
 * wait ends at once, an attempt in flight is let finish, since it may already have reached
 * Dispatch and its answer is still the write's - and the write then rejects with
 * `SendStoppedError`.
 */
export async function writeUnderKey<T>(
  attempt: (bounds: WriteBounds) => Promise<T>,
  key: string,
  { login, sent, signal }: KeyedWriteOptions = {}
): Promise<T | null> {
  let backoff = FIRST_BACKOFF_MS;
  let first: WriteRequest | undefined;
  // Whether an attempt went out that the page has no answer to: a resent `DELETE` then finds the
  // thing already gone.
  let unanswered = false;
  const told = (request: WriteRequest) => {
    if (first === undefined) first = request;
    else if (request.method !== first.method || request.path !== first.path) {
      throw new OneRequestPerWriteError(first, request);
    }
    sent?.(request);
  };
  // Whether Dispatch answered any request since the backoff last grew: it is reachable, and the
  // backoff starts over.
  let answered = false;
  const stopListening = onReachable((how) => {
    if (how === "answer") answered = true;
  });
  try {
    for (;;) {
      try {
        return await attempt({ deadlineMs: KEYED_SEND_ATTEMPT_MS, key, login, sent: told });
      } catch (error) {
        if (isKeyCommitted(error) || error instanceof AnswerNotKeptError) return null;
        const gone = error instanceof ApiError && error.status === 404;
        if (gone && unanswered && first?.method === "DELETE") return null;
        if (answered || (error instanceof ApiError && error.status < 500)) {
          answered = false;
          backoff = FIRST_BACKOFF_MS;
        }
        const wait = retryAfter(error, backoff);
        if (wait === undefined) throw error;
        unanswered ||= !(error instanceof ApiError) || error.status >= 500;
        if (!(await pause(wait.ms, wait.wakes, signal))) throw new SendStoppedError();
        backoff = Math.min(backoff * 2, MAX_BACKOFF_MS);
      }
    }
  } finally {
    stopListening();
  }
}
