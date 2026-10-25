import type { Page, Request } from "@playwright/test";

/** One write the page made: the JSON body it sent and the `Idempotency-Key` header it sent it
 *  under, which names the person's one action, so every attempt at that action carries the same
 *  key and two actions carry two. */
export interface KeyedWrite<T> {
  readonly input: T;
  readonly key: string | undefined;
}

/** The body and the key of `request`, a write the page made. Playwright lower-cases every header
 *  name it reports. */
export function keyedWrite<T>(request: Request): KeyedWrite<T> {
  return { input: request.postDataJSON() as T, key: request.headers()["idempotency-key"] };
}

/** Every `method` request the page makes to `path` from here on, in order, as body and key - a
 *  route that fulfils or aborts a request itself still lists it, since the request event fires
 *  before any route answers. */
export function watchWrites<T>(page: Page, method: string, path: string): KeyedWrite<T>[] {
  const writes: KeyedWrite<T>[] = [];
  page.on("request", (request) => {
    if (request.method() === method && new URL(request.url()).pathname === path) {
      writes.push(keyedWrite<T>(request));
    }
  });
  return writes;
}
