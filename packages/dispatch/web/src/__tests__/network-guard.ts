/**
 * The unit-test write guard, installed by the preload (`setup.ts`).
 *
 * A unit test has no Dispatch: a write that reaches the network was answered by nothing the test
 * set up, so whatever the page made of its failure - a write retried, a read mark dropped - passed
 * unchecked. Each one fails its test with its method and path, and one still retrying after a
 * file's last test is reported there (`writesThatReachedNetwork`). Reads still go to the network
 * as before: hundreds of tests leave incidental reads unanswered, which this guard does not yet
 * cover.
 *
 * Both ways the page makes a request are guarded. `fetch` carries every write but one; an upload
 * goes through `XMLHttpRequest` (`client.ts`, for its progress events), and happy-dom's
 * `XMLHttpRequest` uses a `Fetch` of its own rather than `globalThis.fetch`, so the two need
 * guarding apart.
 */

/** Every write a test let reach the network since the last report, each `<METHOD> <path>`. */
const reached: string[] = [];

/** The route a request would reach Dispatch on when it is a write - anything but a read - and
 *  nothing when it is a read, which the guard lets through. */
export function writeReachingNetwork(method: string, url: string): string | undefined {
  const verb = method.toUpperCase();
  return verb === "GET" || verb === "HEAD" ? undefined : `${verb} ${url}`;
}

/** Every write that reached the network since this was last called, and forgets them: the
 *  preload's `afterEach` fails the test with them, so a write still retrying after a file's last
 *  test is reported rather than lost with the test runner's exit. */
export function writesThatReachedNetwork(): string[] {
  return reached.splice(0);
}

/** Guards `globalThis.fetch` and `XMLHttpRequest.prototype.send` against a test's writes. A test
 *  that answers requests itself (`spyOn(globalThis, "fetch")`) replaces the first for its own
 *  run. */
export function guardWritesToNetwork(): void {
  const networkFetch = globalThis.fetch;
  const guardedFetch = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const method = init?.method ?? (input instanceof Request ? input.method : "GET");
    const path = input instanceof Request ? input.url : String(input);
    const write = writeReachingNetwork(method, path);
    if (write === undefined) return networkFetch(input, init);
    reached.push(write);
    throw new TypeError(`a unit test's write reached the network: ${write}`);
  };
  // `typeof fetch` also names Bun's `preconnect`, which nothing in a unit test calls.
  globalThis.fetch = Object.assign(guardedFetch, { preconnect: networkFetch.preconnect });

  // An `XMLHttpRequest` names its method and url at `open`, and no API gives them back, so the
  // guard keeps each request's own until it is sent.
  const requested = new WeakMap<XMLHttpRequest, { method: string; url: string }>();
  const open = XMLHttpRequest.prototype.open;
  XMLHttpRequest.prototype.open = function guardedOpen(
    this: XMLHttpRequest,
    method: string,
    url: string | URL,
    ...rest: unknown[]
  ) {
    requested.set(this, { method, url: String(url) });
    return (open as (this: XMLHttpRequest, ...args: unknown[]) => void).call(
      this,
      method,
      url,
      ...rest
    );
  } as typeof XMLHttpRequest.prototype.open;
  const send = XMLHttpRequest.prototype.send;
  XMLHttpRequest.prototype.send = function guardedSend(
    this: XMLHttpRequest,
    body?: Document | XMLHttpRequestBodyInit | null
  ) {
    const request = requested.get(this);
    const write =
      request === undefined ? undefined : writeReachingNetwork(request.method, request.url);
    if (write === undefined) {
      send.call(this, body);
      return;
    }
    reached.push(write);
    throw new TypeError(`a unit test's write reached the network: ${write}`);
  };
}
