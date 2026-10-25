import {
  ApiError,
  isKeyInFlight,
  RequestTimeoutError,
  type WriteBounds,
  type WriteRequest,
} from "../../api/client";

/**
 * How long a write may go unanswered before the page says so: past it the write is late, still
 * being tried under its key, and the reader may stop it. A composer also lets go of what it holds
 * the reader to - a host's Back, Escape or Collapse thread - at this point, since a request the
 * server never answers would hold them for good.
 */
export const SEND_DEADLINE_MS = 30_000;

/** How long one attempt of a write may go unanswered before it is given up and made again under
 *  the same key. */
export const KEYED_SEND_ATTEMPT_MS = 15_000;

/** The wait before a write's first retry, doubling after each attempt that ends without Dispatch's
 *  verdict, up to `MAX_BACKOFF_MS`. */
const FIRST_BACKOFF_MS = 1_000;
const MAX_BACKOFF_MS = 30_000;

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

/** Waits `ms`, and says whether it waited the whole of it: `false` once `signal` aborts, at once. */
function pause(ms: number, signal: AbortSignal | undefined): Promise<boolean> {
  if (signal?.aborted === true) return Promise.resolve(false);
  const { promise, resolve } = Promise.withResolvers<boolean>();
  const stop = () => {
    clearTimeout(timer);
    resolve(false);
  };
  const timer = setTimeout(() => {
    signal?.removeEventListener("abort", stop);
    resolve(true);
  }, ms);
  signal?.addEventListener("abort", stop, { once: true });
  return promise;
}

/** How one keyed write is tried: `signal` stops the trying, `sent` is told each attempt's request
 *  as it goes out, and `decided` says whether a 2xx is the write's outcome yet. */
export interface KeyedWriteOptions<T> {
  readonly decided?: (answer: T) => boolean;
  readonly sent?: (request: WriteRequest) => void;
  readonly signal?: AbortSignal;
}

/** Whether `error` ended an attempt without Dispatch's verdict, so the write is made again under
 *  its key: the fetch failed (a browser's `TypeError`), the attempt outran its deadline, Dispatch
 *  answered a 5xx or a 429, or it is still running the first request under the key
 *  (`409 KEY_IN_FLIGHT`). Anything else - a refusal, an answer it did not keep, an error of the
 *  page's own - ends the write at once. */
function undecided(error: unknown): boolean {
  if (error instanceof TypeError || error instanceof RequestTimeoutError) return true;
  return (
    error instanceof ApiError &&
    (error.status >= 500 || error.status === 429 || isKeyInFlight(error))
  );
}

/**
 * Makes `attempt` under `key`, the person's action's name, until Dispatch decides it. Each attempt
 * is held to `KEYED_SEND_ATTEMPT_MS`, and one that ends without Dispatch's verdict (`undecided`),
 * or with a 2xx `decided` says is not yet the outcome, is made again after 1 s, 2 s, 4 s … never
 * more than 30 s apart, so however many of them reach Dispatch, the key makes it act once.
 * Resolves with any other 2xx; rejects with anything else at once - a 4xx, its refusal;
 * `AnswerNotKeptError` for a write that ran but whose answer Dispatch did not keep; an error of
 * the page's own. `signal` stops the trying between attempts - a wait ends at once, an attempt in
 * flight is let finish, since it may already have reached Dispatch and its answer is still the
 * write's - and the write then rejects with `SendStoppedError`.
 */
export async function writeUnderKey<T>(
  attempt: (bounds: WriteBounds) => Promise<T>,
  key: string,
  { decided = () => true, sent, signal }: KeyedWriteOptions<T> = {}
): Promise<T> {
  let backoff = FIRST_BACKOFF_MS;
  for (;;) {
    try {
      const answer = await attempt({ deadlineMs: KEYED_SEND_ATTEMPT_MS, key, sent });
      if (decided(answer)) return answer;
    } catch (error) {
      if (!undecided(error)) throw error;
    }
    if (!(await pause(backoff, signal))) throw new SendStoppedError();
    backoff = Math.min(backoff * 2, MAX_BACKOFF_MS);
  }
}
