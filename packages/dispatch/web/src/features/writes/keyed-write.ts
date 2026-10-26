import { IDEMPOTENCY_KEY_TTL_MS, retriesUnderKey } from "@legion/contracts";

import {
  AnswerNotKeptError,
  ApiError,
  isKeyCommitted,
  isKeyInFlight,
  isUnauthorized,
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
 *  verdict, up to `MAX_BACKOFF_MS`; it starts over only once Dispatch can be reached again after
 *  it could not (`reachability.ts`), never because Dispatch answered something else meanwhile. */
export const FIRST_BACKOFF_MS = 1_000;
export const MAX_BACKOFF_MS = 30_000;

/**
 * What a Retry under a write's own key promises, and where that promise ends: Dispatch keeps a key
 * `IDEMPOTENCY_KEY_TTL_MS` from its first request, so a Retry later than that is a new write to
 * Dispatch, which may make it a second time. `made` is what the write does once it lands
 * (`delivered`, `saved`).
 */
export function sameKeyRetryNote(made: string): string {
  const hours = IDEMPOTENCY_KEY_TTL_MS / 3_600_000;
  return `Retry asks again under the same key, so it is ${made} at most once if you retry within ${hours} hours of the first try. After that Dispatch no longer knows the key: a Retry is a new write and may be ${made} twice.`;
}

/** A write that stopped trying before Dispatch confirmed it: the reader pressed Stop trying. Not
 *  Dispatch's refusal: the write may have reached it, and a Retry under the same key is answered
 *  with what that first one did rather than done a second time - within the key's lifetime
 *  (`sameKeyRetryNote`). */
export class SendStoppedError extends Error {
  constructor(
    message = `Dispatch has not confirmed this message. ${sameKeyRetryNote("delivered")}`
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
      `Not sent: this page was closed before Dispatch confirmed it. ${sameKeyRetryNote("delivered")}`
    );
    this.name = "SendInterruptedError";
  }
}

/** A write the reader discarded while it waited its turn behind another, before any attempt of it
 *  went out: it was never sent, so nothing of it reached Dispatch. */
export class WriteDiscardedError extends Error {
  constructor() {
    super("Discarded before it was sent.");
    this.name = "WriteDiscardedError";
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
 *  own, which ends the write at once. A `401` is no verdict either: the person's session lapsed,
 *  Dispatch ran nothing and keeps nothing under the key, and the write waits for them to sign in
 *  again (`Writes.signedIn`), asked again meanwhile in case the page is still signed in. */
function retryAfter(
  error: unknown,
  backoff: number
): { readonly ms: number; readonly wakes: boolean; readonly grows: boolean } | undefined {
  if (
    error instanceof NetworkError ||
    error instanceof RequestTimeoutError ||
    error instanceof UnreadableAnswerError
  ) {
    return { grows: true, ms: backoff, wakes: true };
  }
  if (isUnauthorized(error)) return { grows: true, ms: backoff, wakes: false };
  if (!(error instanceof ApiError) || !retriesUnderKey(error.status, error.code)) return undefined;
  // Dispatch said when to come back: the first request under the key is still running, or it is
  // turning requests away for now. Nothing the page learns elsewhere changes that, and the wait
  // is Dispatch's, not the backoff, which stays where it was.
  if ((error.status === 429 && error.retryAfterMs !== undefined) || isKeyInFlight(error)) {
    return { grows: false, ms: error.retryAfterMs ?? FIRST_BACKOFF_MS, wakes: false };
  }
  return { grows: true, ms: backoff, wakes: true };
}

/**
 * Waits `ms`, and says whether it waited for a reason to try again: `false` once `signal`
 * aborts, at once. A wait that `wakes` ends early - but never before `FIRST_BACKOFF_MS` of it,
 * so a write is not asked again in a tight loop - when the page learns Dispatch can be reached
 * again after it could not: the browser came back online, the live socket reopened after it was
 * lost, or Dispatch answered a request after one failed to reach it (`reachability.ts`).
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
 *  attempt's request as it goes out; `uploading` is told how much of an upload's body each attempt
 *  has sent; `failed` is told each attempt that ended without Dispatch's verdict; `unauthorized`
 *  is told each attempt Dispatch answers `401`, the person's session having lapsed. */
export interface KeyedWriteOptions {
  readonly failed?: () => void;
  readonly login?: string;
  readonly sent?: (request: WriteRequest) => void;
  readonly signal?: AbortSignal;
  readonly unauthorized?: () => void;
  readonly uploading?: (loaded: number, total: number) => void;
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
 * not JSON (`UnreadableAnswerError`), the person's session lapsed (`401`, told to `unauthorized`),
 * or Dispatch answered a 5xx, a 408, a 429 or `409 KEY_IN_FLIGHT` (`retriesUnderKey`) - is made
 * again after 1 s, 2 s, 4 s … never more than 30 s apart, or after Dispatch's `Retry-After`. Only
 * Dispatch becoming reachable again after it could not be reached - a transport failure or a
 * timeout, then an answer to any request; the browser coming back online; the live socket
 * reopening after it was lost - starts that backoff over and ends a wait early: an answer while it
 * was reachable all along resets nothing. However many attempts reach Dispatch, the key makes it
 * act once. Resolves with the 2xx,
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
  { failed, login, sent, signal, unauthorized, uploading }: KeyedWriteOptions = {}
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
  // Whether Dispatch became reachable again since the backoff last grew - after a transport
  // failure or a timeout, the browser's `online`, the live socket reopening: the backoff starts
  // over. An answer while it was reachable all along (another route's 2xx, this route's 5xx)
  // changes nothing, so a route that keeps failing backs off to its cap.
  let regained = false;
  const stopListening = onReachable(() => {
    regained = true;
  });
  try {
    for (;;) {
      try {
        return await attempt({
          deadlineMs: KEYED_SEND_ATTEMPT_MS,
          key,
          login,
          progress: uploading,
          sent: told,
        });
      } catch (error) {
        if (isKeyCommitted(error) || error instanceof AnswerNotKeptError) return null;
        const gone = error instanceof ApiError && error.status === 404;
        if (gone && unanswered && first?.method === "DELETE") return null;
        if (regained) {
          regained = false;
          backoff = FIRST_BACKOFF_MS;
        }
        const wait = retryAfter(error, backoff);
        if (wait === undefined) throw error;
        if (isUnauthorized(error)) unauthorized?.();
        failed?.();
        unanswered ||= !(error instanceof ApiError) || error.status >= 500;
        if (!(await pause(wait.ms, wait.wakes, signal))) throw new SendStoppedError();
        if (wait.grows) backoff = Math.min(backoff * 2, MAX_BACKOFF_MS);
      }
    }
  } finally {
    stopListening();
  }
}
