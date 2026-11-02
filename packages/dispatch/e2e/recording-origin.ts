/**
 * A server on a second loopback origin that records every request it is asked for, which is how
 * the `view` block's sandbox rows (dispatch://INFRA-682, A4) tell "the page could not reach the
 * network" from "the page reached it and nothing noticed". A row starts one of its own, drives a
 * page that tries to reach it, and reads the record.
 *
 * It listens on `127.0.0.1` and a port the OS chooses, as `e2e/broker-approval.e2e.ts`'s sibling
 * page does, and its record is the shape `e2e/round-robin-proxy.ts` keeps: one row per request,
 * in the order they arrived. Plain `127.0.0.1` needs no resolver rule, so every engine of these
 * rows reaches it; the `dispatch-e2e.test` origin is Chromium's alone (its
 * `--host-resolver-rules`) and is no trustworthy origin, which is why no view draws on it.
 *
 * A row starts one per test and closes it in a `finally`, so one row's requests are never read
 * as another's.
 */
import { createServer, type Server } from "node:http";

/** One request the recording origin answered. `path` carries the query, as the proxy's log does. */
export interface RecordedRequest {
  readonly method: string;
  readonly path: string;
}

export interface RecordingOrigin {
  /** `http://127.0.0.1:<port>`, for a page, a frame or a fetch to name. */
  readonly origin: string;
  /** Every request answered since the last `reset`, in the order they arrived. */
  requests(): readonly RecordedRequest[];
  /** Those requests' paths, which is what most assertions read. */
  paths(): readonly string[];
  /** Answers `path` with this HTML from now on. A page whose own body must name this origin
   *  cannot be written before the port is known, so it is put here rather than at the start. */
  serve(path: string, html: string): void;
  reset(): void;
  close(): Promise<void>;
}

/** A 1x1 PNG, for a path that names a picture: a page that loads an image from this origin must
 *  get a picture back rather than text, or the engine's `error` event would be the type's and
 *  not the policy's. */
const ONE_PIXEL_PNG = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScL0eAAAAABJRU5ErkJggg==",
  "base64"
);

/**
 * Starts one recording origin. `pages` names the paths that answer HTML - a fixture page the row
 * opens outside Dispatch, or the address a page moves its frame to - and `serve` adds one once
 * the origin is known. Every other path answers plain text, or a picture when it ends `.png`,
 * and every request is recorded before it is answered, whatever the path.
 */
export async function startRecordingOrigin(
  pages: Readonly<Record<string, string>> = {}
): Promise<RecordingOrigin> {
  const served = new Map(Object.entries(pages));
  let recorded: RecordedRequest[] = [];
  const server: Server = createServer((incoming, answer) => {
    const path = incoming.url ?? "";
    recorded.push({ method: incoming.method ?? "", path });
    // A POST's body is read and dropped: an unread body leaves the socket half-spoken and the
    // browser waiting on a form it submitted.
    incoming.resume();
    const page = served.get(path.split("?")[0] ?? path);
    if (page !== undefined) {
      answer.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
      answer.end(page);
      return;
    }
    if (path.split("?")[0]?.endsWith(".png") === true) {
      answer.writeHead(200, { "Content-Type": "image/png" });
      answer.end(ONE_PIXEL_PNG);
      return;
    }
    answer.writeHead(200, { "Content-Type": "text/plain; charset=utf-8" });
    answer.end("recorded");
  });
  const listening = Promise.withResolvers<void>();
  server.listen(0, "127.0.0.1", listening.resolve);
  await listening.promise;
  const address = server.address();
  if (address === null || typeof address === "string") {
    throw new Error(`the recording origin listens on no TCP port: ${String(address)}`);
  }
  return {
    close: () => {
      const closed = Promise.withResolvers<void>();
      // A browser holds its connections open, and `close` waits for every one of them.
      server.closeAllConnections();
      server.close((error) => (error === undefined ? closed.resolve() : closed.reject(error)));
      return closed.promise;
    },
    origin: `http://127.0.0.1:${address.port}`,
    paths: () => recorded.map((request) => request.path),
    requests: () => recorded,
    serve: (path, html) => {
      served.set(path, html);
    },
    reset: () => {
      recorded = [];
    },
  };
}
