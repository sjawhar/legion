import { IDEMPOTENCY_KEY_TTL_MS, retriesUnderKey } from "@legion/contracts";

import {
  AnswerNotKeptError,
  ApiError,
  busyRetryAfterMs,
  goesUnderKey,
  isKeyCommitted,
  isKeyInFlight,
  isUnauthorized,
  NetworkError,
  RequestTimeoutError,
  UnreadableAnswerError,
  type WriteBounds,
  type WriteMethod,
  type WriteRequest,
} from "../../api/client";
import { onReachable, type Reachable, routeOf } from "../../api/reachability";
import { keyMintedAt } from "../../lib/send-key";

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
 *  verdict, up to `MAX_BACKOFF_MS`; it starts over only once the page learns Dispatch can be
 *  reached again on the write's own route (`reachability.ts`), never because Dispatch answered
 *  something else meanwhile. */
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

/**
 * What a Retry promises once Dispatch has forgotten the write's key: the key has reached the
 * `IDEMPOTENCY_KEY_TTL_MS` Dispatch keeps one for, so Dispatch holds nothing under it and a Retry
 * is a new write, done a second time if the first one landed. `made` is what the write does once
 * it lands (`delivered`, `saved`). The words say "or more" because `keyExpired` turns true at the
 * lifetime itself, not past it: at exactly that hour the key is spent and "more than" would say
 * something the code does not.
 */
export function expiredRetryNote(made: string): string {
  const hours = IDEMPOTENCY_KEY_TTL_MS / 3_600_000;
  return `Its key is ${hours} hours old or more, so Dispatch no longer holds it: a Retry is a new write, which can be ${made} a second time if the first one was.`;
}

/**
 * Whether Dispatch has forgotten `key` by `now`: the key was minted `IDEMPOTENCY_KEY_TTL_MS` ago
 * or longer, so a request under it is a new write to Dispatch rather than a repeat of the first.
 *
 * False for a key that carries no mint time (`keyMintedAt`) - every key minted before the release
 * that mints v7 keys. Those keys keep today's behaviour: Dispatch forgets them, and the page
 * cycles them within one key lifetime of a deploy, where calling them spent would mint a new key
 * for a write Dispatch may already have done - the duplicate this check exists to prevent.
 */
export function keyExpired(key: string, now: number): boolean {
  const minted = keyMintedAt(key);
  return minted !== undefined && now - minted >= IDEMPOTENCY_KEY_TTL_MS;
}

/**
 * Whether `key` is spent by `now`: a request has already gone out under it, that request is one
 * that goes under its key (`goesUnderKey`), and Dispatch has forgotten the key (`keyExpired`).
 * Another request under a spent key would be a second write of the person's action rather than
 * a repeat of the first, which is why no attempt may go out under one.
 *
 * `carried` is what the key has carried: the method of an attempt that went out under it, or
 * `null` where none has. A key that has carried nothing is never spent however old it is -
 * Dispatch has never seen it, so there is nothing under it a request now would repeat - and a
 * key whose request was a `PUT` or a `DELETE` is never spent either, since neither sends a key
 * and making one again is the same change however late.
 *
 * This is the one place that question is answered, and both askers are told the same fact: the
 * store keeps what each key has carried (`Writes` `#carried`, kept with the write as
 * `KeptWrite.carried` and read back after a reload). The retry loop asks it before each attempt
 * (`writeUnderKey`, from what it is told the key has carried and from its own attempts) and a
 * kept refusal asks it of the same record (`refusalFrom`). Neither decides on its own, so the
 * two cannot answer the same write differently.
 */
export function keySpent(carried: WriteMethod | null, key: string, now: number): boolean {
  return carried !== null && goesUnderKey(carried) && keyExpired(key, now);
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

/** A write whose key Dispatch no longer holds (`keyExpired`): it was minted at least
 *  `IDEMPOTENCY_KEY_TTL_MS` ago, so no attempt under it would be a repeat of the first - Dispatch
 *  would run it as a new write. It ends as a stopped write does, so it offers what a stopped
 *  write offers - Retry and Discard - and that Retry goes under a new key
 *  (`retriesUnderItsKey`). */
export class SendExpiredError extends SendStoppedError {
  constructor() {
    super(`Dispatch has not confirmed this message. ${expiredRetryNote("delivered")}`);
    this.name = "SendExpiredError";
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
 * again under the same key, and Dispatch answers it with what the first one did. A write whose
 * key ran out (`SendExpiredError`) is the one of those that does not: Dispatch has forgotten the
 * key, so a request under it is a new write rather than a repeat of the first, and the Retry goes
 * under a new key, as it would after Dispatch's own verdict.
 */
export function retriesUnderItsKey(refusal: Error | undefined): boolean {
  return refusal instanceof SendStoppedError && !(refusal instanceof SendExpiredError);
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
  // Dispatch said when to come back: the first request under the key is still running, it is
  // turning requests away for now (`429`), or it was too busy for this one (`503 DATABASE_BUSY`,
  // `503 REQUEST_TIMEOUT`, `busyRetryAfterMs`). Nothing the page learns elsewhere changes that,
  // and the wait is Dispatch's, not the backoff, which stays where it was. Any other `503` keeps
  // the backoff.
  if (
    (error.status === 429 && error.retryAfterMs !== undefined) ||
    busyRetryAfterMs(error) !== undefined ||
    isKeyInFlight(error)
  ) {
    return { grows: false, ms: error.retryAfterMs ?? FIRST_BACKOFF_MS, wakes: false };
  }
  return { grows: true, ms: backoff, wakes: true };
}

/**
 * Waits `ms`, and says whether it waited for a reason to try again: `false` once `signal`
 * aborts, at once. A wait that `wakes` ends early - but never before `FIRST_BACKOFF_MS` of it,
 * so a write is not asked again in a tight loop - when `regained` says the page learned Dispatch
 * can be reached again on the write's route: the browser came back online, the live socket
 * reopened after it was lost, or Dispatch answered on that route after a request there failed to
 * reach it (`reachability.ts`).
 */
function pause(
  ms: number,
  wakes: boolean,
  signal: AbortSignal | undefined,
  regained: (how: Reachable, route: string | undefined) => boolean
): Promise<boolean> {
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
    ? onReachable((how, route) => {
        if (!regained(how, route)) return;
        clearTimeout(timer);
        const floor = Math.min(ms, FIRST_BACKOFF_MS) - (Date.now() - startedAt);
        timer = setTimeout(() => end(true), Math.max(0, floor));
      })
    : () => {};
  signal?.addEventListener("abort", stop, { once: true });
  return promise;
}

/** How one keyed write is tried: `login` is the person who pressed it, sent on every attempt so
 *  Dispatch refuses it under anyone else's session; `carried` is the method of the request that
 *  has already gone out under this write's key, which whoever holds the page's writes knows and
 *  this loop cannot (`Writes`), and is required so that every caller says which it means:
 *  `null` is the answer that the key has carried nothing, not one reached by leaving it out.
 *  `planned` is the request the write was kept as before any attempt of it went out, which
 *  every attempt must make; `signal` stops the trying; `sent` is told each attempt's request as
 *  it goes out; `uploading` is told how much of an upload's body each attempt has sent;
 *  `failed` is told each attempt that ended without Dispatch's verdict; `unauthorized` is told
 *  each attempt Dispatch answers `401`, and answers whether the page's sign-in check, read
 *  again, still names the person who pressed it. */
export interface KeyedWriteOptions {
  readonly carried: WriteMethod | null;
  readonly failed?: () => void;
  readonly login?: string;
  readonly planned?: WriteRequest;
  readonly sent?: (request: WriteRequest) => void;
  readonly signal?: AbortSignal;
  readonly unauthorized?: () => Promise<boolean>;
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
 * again after 1 s, 2 s, 4 s … never more than 30 s apart, or after Dispatch's `Retry-After`. A
 * `401` is a lapse only while the page's sign-in check does not contradict it: a second `401` in a
 * row, each after the check said the person is still signed in, is Dispatch's verdict and refuses
 * the write (the first may be a session that rotated between the two). Only the page learning
 * Dispatch can be reached again on the write's own route - the browser coming
 * back online, the live socket reopening after it was lost, or an answer on this write's route
 * after a request there failed at the connection or went silent - starts that backoff over and ends
 * a wait early: the write's own transport failure followed by another route's answer resets
 * nothing, nor does an answer while it was reachable all along. However many attempts reach
 * Dispatch, the key makes it act once. Resolves with the 2xx, or `null` for a write that landed
 * with no answer to give - `409 KEY_COMMITTED`, or a replay that did not keep the first answer -
 * and for a `DELETE` an earlier attempt of which may have landed and which Dispatch now answers
 * `404` (the thing is gone either way). Rejects with anything else at once: a 4xx, its refusal; an
 * error of the page's own. Every attempt must make the same request, the one the write was
 * `planned` as if it was (`OneRequestPerWriteError`). `signal` stops the trying between attempts -
 * a wait ends at once, an attempt in flight is let finish, since it may already have reached
 * Dispatch and its answer is still the write's - and the write then rejects with
 * `SendStoppedError`. A write whose key is spent (`keySpent`) ends with `SendExpiredError`
 * before the attempt that would go under it - its first, or the one after a wait - since that
 * attempt would be a new write to Dispatch rather than a repeat of the first. The key is spent
 * by what it has carried: `carried`, the method that went out under it before this call, and
 * then the request of this call's own first attempt, told as it goes out. A key that has
 * carried nothing stops nothing, however old it is, and neither does one a `PUT` or a `DELETE`
 * went under, since those send no key at all.
 */
export async function writeUnderKey<T>(
  attempt: (bounds: WriteBounds) => Promise<T>,
  key: string,
  { carried, failed, login, planned, sent, signal, unauthorized, uploading }: KeyedWriteOptions
): Promise<T | null> {
  let backoff = FIRST_BACKOFF_MS;
  let first: WriteRequest | undefined = planned;
  // What has gone out under this write's key: what the caller knows went out before this call,
  // and then this call's own attempts, each told as it goes out. A request told here may not
  // have reached Dispatch - the connection can fail after it leaves - which is the point: from
  // then on the key may name something there, and its age decides the write.
  let underKey: WriteMethod | null = carried;
  // Whether an attempt went out that the page has no answer to: a resent `DELETE` then finds the
  // thing already gone.
  let unanswered = false;
  const told = (request: WriteRequest) => {
    if (first === undefined) first = request;
    else if (request.method !== first.method || request.path !== first.path) {
      throw new OneRequestPerWriteError(first, request);
    }
    underKey = request.method;
    sent?.(request);
  };
  // Whether Dispatch became reachable again on this write's route - its method and path - since
  // the backoff last grew: the browser's `online`, the live socket reopening, an answer on the
  // write's own route after a request there could not reach it: the backoff starts over. Another
  // route's answer says nothing of this one - a read of the same path included - and an answer
  // while it was reachable all along (this route's 5xx) changes nothing, so a route that keeps
  // failing backs off to its cap.
  const ownRoute = (how: Reachable, route: string | undefined) =>
    how !== "answer" || (first !== undefined && route === routeOf(first.method, first.path));
  let regained = false;
  const stopListening = onReachable((how, route) => {
    if (ownRoute(how, route)) regained = true;
  });
  // Whether the last attempt was answered `401` although the sign-in check, read again, still
  // named the person: a second such answer is Dispatch's refusal, not a lapse.
  let unauthorizedWhileSignedIn = false;
  try {
    for (;;) {
      // Dispatch keeps a key a day from its first request: once it has forgotten a key something
      // has already gone out under, another request under it would be a second write of the
      // person's action rather than a repeat (`keySpent`).
      if (keySpent(underKey, key, Date.now())) throw new SendExpiredError();
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
        if (isUnauthorized(error)) {
          const stillSignedIn = (await unauthorized?.()) === true;
          if (stillSignedIn && unauthorizedWhileSignedIn) throw error;
          unauthorizedWhileSignedIn = stillSignedIn;
        } else {
          unauthorizedWhileSignedIn = false;
        }
        failed?.();
        unanswered ||= !(error instanceof ApiError) || error.status >= 500;
        if (!(await pause(wait.ms, wait.wakes, signal, ownRoute))) throw new SendStoppedError();
        if (wait.grows) backoff = Math.min(backoff * 2, MAX_BACKOFF_MS);
      }
    }
  } finally {
    stopListening();
  }
}
