import type { Page, Request, Response, Route } from "@playwright/test";

declare global {
  interface Window {
    __dispatchUploadAttempts?: UploadAttempt[];
  }
}

/** One write the page made: the JSON body it sent and the `Idempotency-Key` header it sent it
 *  under, which names the person's one action, so every attempt at that action carries the same
 *  key and two actions carry two. */
export interface KeyedWrite<T> {
  readonly input: T;
  readonly key: string | undefined;
}

/** Says whether a body the page sent is the `T` a row reads it as. */
export type BodyShape<T> = (body: unknown) => body is T;

/** The body and the key of `request`, a write the page made. Playwright lower-cases every header
 *  name it reports. A row that reads the body's fields passes the `shape` it reads, and a body
 *  of another shape fails the row there, naming it; without one the body is `unknown`, enough for
 *  `toEqual`. */
export function keyedWrite(request: Request): KeyedWrite<unknown>;
export function keyedWrite<T>(request: Request, shape: BodyShape<T>): KeyedWrite<T>;
export function keyedWrite<T>(request: Request, shape?: BodyShape<T>): KeyedWrite<unknown> {
  const input: unknown = request.postDataJSON();
  if (shape !== undefined && !shape(input)) {
    throw new Error(
      `${request.method()} ${new URL(request.url()).pathname} sent a body of another shape: ${JSON.stringify(input)}`
    );
  }
  return { input, key: request.headers()["idempotency-key"] };
}

/** Every `method` request the page makes to `path` from here on, in order, as body and key - a
 *  route that fulfils or aborts a request itself still lists it, since the request event fires
 *  before any route answers. */
export function watchWrites(page: Page, method: string, path: string): KeyedWrite<unknown>[];
export function watchWrites<T>(
  page: Page,
  method: string,
  path: string,
  shape: BodyShape<T>
): KeyedWrite<T>[];
export function watchWrites<T>(
  page: Page,
  method: string,
  path: string,
  shape?: BodyShape<T>
): KeyedWrite<unknown>[] {
  const writes: KeyedWrite<unknown>[] = [];
  page.on("request", (request) => {
    if (request.method() === method && new URL(request.url()).pathname === path) {
      writes.push(shape === undefined ? keyedWrite(request) : keyedWrite(request, shape));
    }
  });
  return writes;
}

/** The first answer the page gets to a `method` request to `path` that Dispatch replayed from a
 *  key it already answered (`Idempotency-Replayed: ?1`). */
export function replayed(page: Page, method: string, path: string): Promise<Response> {
  return page.waitForResponse(
    (response) =>
      response.request().method() === method &&
      new URL(response.url()).pathname === path &&
      response.headers()["idempotency-replayed"] === "?1"
  );
}

/** How long each turn of `runClockUntil` waits in real time, and how far it runs the page's own
 *  clock while a write is out. One number read in both places: that is what makes the crawling
 *  pace real speed rather than near it, and a change to one is a change to both. */
const REAL_STEP_MS = 100;

/** Runs `page`'s own clock forward in `stepMs` pieces, `REAL_STEP_MS` of real time apart,
 *  until `landing` settles, and answers what it settled with. A write between two attempts is
 *  waiting on a page `setTimeout` (`pause`, `web/src/features/writes/keyed-write.ts`) armed only
 *  once the attempt before it ended, which is after the row saw that attempt, so the clock is
 *  driven until the write moves rather than jumped once. `runFor` rather than `fastForward`:
 *  it fires a repeating timer every period it covers, where `fastForward` fires each timer once.
 *  The page's clock must be installed before it loaded (`page.clock.install()`, which leaves the
 *  clock running at real speed), so the write loop's timers are the ones driven here.
 *
 *  While a write of the page's is out the clock runs at real speed instead of at eleven times
 *  it, and it never stops: the crawling step and the wait between turns are the one
 *  `REAL_STEP_MS`, which is what ties the two. An attempt the page has heard nothing of for
 *  `KEYED_SEND_ATTEMPT_MS` of the page's own time is given up and made again under its key
 *  (`writeUnderKey`), and at the ordinary pace this loop spends `stepMs` of page time for each
 *  `REAL_STEP_MS` of real time — 1.1 s for every 0.1 s. Driven at that pace over an attempt in
 *  flight it reaches that 15 s in under a second and a half of a loaded lane's real time,
 *  abandoning the very attempt the row waits for and raising one more POST than the row counts.
 *  At the crawling pace the same 15 s costs 15 s of real time, which no answer of this harness
 *  takes. The clock crawls rather than stopping because a write that never ends must not hold
 *  the backoff of the write after it: a stopped clock fires no timer at all, and the row then
 *  waits out its whole budget for a retry that was never armed. Reads (`GET`) are left out of
 *  the reckoning: none of them is a keyed attempt, and the page holds some of them open for as
 *  long as it lives. */
export async function runClockUntil<T>(
  page: Page,
  landing: Promise<T>,
  stepMs = 1_100
): Promise<T> {
  // The writes of the page's that are out, by request: an attempt that went out before this was
  // called ends while it runs, and a tally would count that ending as one write fewer than none.
  const out = new Set<Request>();
  const went = (request: Request) => {
    if (request.method() !== "GET") out.add(request);
  };
  const ended = (request: Request) => {
    out.delete(request);
  };
  page.on("request", went);
  page.on("requestfailed", ended);
  page.on("requestfinished", ended);
  let waiting = true;
  const landed = landing.finally(() => {
    waiting = false;
  });
  const quiet = landed.catch(() => undefined);
  try {
    while (waiting) {
      await page.clock.runFor(out.size === 0 ? stepMs : Math.min(stepMs, REAL_STEP_MS));
      const margin = Promise.withResolvers<void>();
      setTimeout(margin.resolve, REAL_STEP_MS);
      await Promise.race([quiet, margin.promise]);
    }
  } finally {
    page.off("request", went);
    page.off("requestfailed", ended);
    page.off("requestfinished", ended);
  }
  return landed;
}

/** How many writes the tab keeps in its storage for a reload to resume (`dispatch.write:<key>`),
 *  leaving out read marks: a mark is a quiet write, kept as an entry of its own until Dispatch
 *  answers it, which a row about the writes the reader made does not count. */
export function keptWrites(page: Page): Promise<number> {
  return page.evaluate(
    () =>
      Object.entries(window.sessionStorage).filter(([name, value]) => {
        if (!name.startsWith("dispatch.write:")) return false;
        try {
          const kept: unknown = JSON.parse(value);
          return !(
            typeof kept === "object" &&
            kept !== null &&
            "presentation" in kept &&
            kept.presentation === "quiet"
          );
        } catch {
          return true;
        }
      }).length
  );
}

/** One answer the page got to a write, as a row reads it: the key its request went under, its
 *  status, its error code, and the two headers a keyed answer may carry. */
export interface KeyedAnswer {
  readonly key: string | undefined;
  readonly status: number;
  readonly code?: string;
  readonly retryAfter?: string;
  readonly replayed?: string;
}

/** Every answer the page gets from here on to a `method` request to `path`, in order. An attempt
 *  the page cut off itself has none, so this lists only what reached the page. An answer is listed
 *  once its body has been read, after the response event, so a row that has just awaited the
 *  response (`waitForResponse`, `replayed`) polls the list (`expect.poll`) rather than reading it
 *  once. */
export function watchAnswers(page: Page, method: string, path: string): KeyedAnswer[] {
  const answers: KeyedAnswer[] = [];
  page.on("response", async (response) => {
    if (response.request().method() !== method || new URL(response.url()).pathname !== path) {
      return;
    }
    const body: unknown = await response.json().catch(() => null);
    answers.push({
      code:
        body !== null && typeof body === "object" && "code" in body ? String(body.code) : undefined,
      key: response.request().headers()["idempotency-key"],
      replayed: response.headers()["idempotency-replayed"],
      retryAfter: response.headers()["retry-after"],
      status: response.status(),
    });
  });
  return answers;
}

/** Lets the first `method` request matching `pattern` through to Dispatch and drops its answer,
 *  as a connection lost on the way back drops it; every later request goes through untouched.
 *  With `until`, the page is left waiting on the dropped answer until that settles (a reload
 *  cancels the request first), rather than told at once that it failed. Resolves with the status
 *  Dispatch answered the dropped request, as soon as Dispatch answers it. */
export async function dropFirstAnswer(
  page: Page,
  pattern: string,
  method: string,
  until?: Promise<unknown>
): Promise<number> {
  const { promise, resolve } = Promise.withResolvers<number>();
  let dropped = false;
  await page.route(pattern, async (route: Route) => {
    if (route.request().method() !== method || dropped) return route.fallback();
    dropped = true;
    resolve((await route.fetch()).status());
    await until;
    // A reload cancelled the request already; there is nothing left to answer.
    await route.abort("failed").catch(() => undefined);
  });
  return promise;
}

/** One attempt of an upload as the page's own `XMLHttpRequest` saw it: the key it went under and
 *  how it ended - `load`, with Dispatch's status and error code; `error`, the connection failed;
 *  `abort`, the page gave it up; `timeout`. `ended` is unset while it is still out. */
export interface UploadAttempt {
  readonly code?: string;
  readonly ended?: "abort" | "error" | "load" | "timeout";
  readonly key?: string;
  readonly status?: number;
}

/** Records, from before the page loads, every `XMLHttpRequest` it sends to `path` (the transport
 *  of its uploads, `xhrUpload`) as the request itself saw it, beside the page's own listeners:
 *  Playwright's network events report a request the engine handed the page an answer to and one
 *  whose connection failed in each engine's own terms, where the page reads only `load` or
 *  `error`. Returns a reader of what was recorded, in send order. */
export async function recordUploadAttempts(
  page: Page,
  path: string
): Promise<() => Promise<UploadAttempt[]>> {
  await page.addInitScript((watched: string) => {
    type Recorded = {
      code?: string;
      ended?: "abort" | "error" | "load" | "timeout";
      key?: string;
      status?: number;
    };
    const attempts: Recorded[] = [];
    window.__dispatchUploadAttempts = attempts;
    const paths = new WeakMap<XMLHttpRequest, string>();
    const keys = new WeakMap<XMLHttpRequest, string>();
    const { open, send, setRequestHeader } = XMLHttpRequest.prototype;
    XMLHttpRequest.prototype.open = function (
      this: XMLHttpRequest,
      ...args: [method: string, url: string | URL, ...rest: unknown[]]
    ) {
      paths.set(this, new URL(String(args[1]), window.location.href).pathname);
      Reflect.apply(open, this, args);
    };
    XMLHttpRequest.prototype.setRequestHeader = function (
      this: XMLHttpRequest,
      name: string,
      value: string
    ) {
      if (name.toLowerCase() === "idempotency-key") keys.set(this, value);
      return setRequestHeader.call(this, name, value);
    };
    XMLHttpRequest.prototype.send = function (
      this: XMLHttpRequest,
      body?: Document | XMLHttpRequestBodyInit | null
    ) {
      if (paths.get(this) === watched) {
        const attempt: Recorded = { key: keys.get(this) };
        attempts.push(attempt);
        this.addEventListener("load", () => {
          attempt.ended = "load";
          attempt.status = this.status;
          try {
            const answer: unknown = JSON.parse(this.responseText);
            if (typeof answer === "object" && answer !== null && "code" in answer) {
              attempt.code = String(answer.code);
            }
          } catch {
            // An answer that is not JSON carries no code.
          }
        });
        for (const ended of ["abort", "error", "timeout"] as const) {
          this.addEventListener(ended, () => {
            attempt.ended = ended;
          });
        }
      }
      return send.call(this, body);
    };
  }, path);
  return () => page.evaluate(() => window.__dispatchUploadAttempts ?? []);
}
