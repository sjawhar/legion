import type { Page, Request, Response, Route } from "@playwright/test";

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
 *  the page cut off itself has none, so this lists only what reached the page. */
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
