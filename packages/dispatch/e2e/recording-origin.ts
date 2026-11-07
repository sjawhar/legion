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
  /** Answers `path` with this HTML and never ends the response. The browser commits the
   *  document and parses what arrived, so its inline script runs, and its `load` event never
   *  fires because the body never ends - a frame that moved away without finishing
   *  (dispatch://INFRA-682, R5). The document's own response carries that rather than a
   *  subresource it names: whether an engine fetches a trailing `<img>` at all is the engine's
   *  business, and WebKit was measured not fetching one, so a row that hung on a picture proved
   *  nothing there. The connection is closed with the origin. */
  serveUnfinished(path: string, html: string): void;
  /** Records every request for `path` from now on and answers none of them, holding the
   *  connection open. Nothing is sent, so a navigation to such a path never commits. The
   *  connection is closed with the origin. */
  hang(path: string): void;
  /** How many responses for `path` this origin has sent and not ended, which is the server's own
   *  record that a document is still loading. A row asserts this rather than a subresource the
   *  document names, because whether an engine fetches one is the engine's business. */
  openResponses(path: string): number;
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
 * and every request is recorded before it is answered, whatever the path - but a path given to
 * `hang`, which is recorded and never answered at all.
 */
export async function startRecordingOrigin(
  pages: Readonly<Record<string, string>> = {}
): Promise<RecordingOrigin> {
  const served = new Map(Object.entries(pages));
  const unfinished = new Map<string, string>();
  const hanging = new Set<string>();
  const open = new Map<string, number>();
  let recorded: RecordedRequest[] = [];
  const server: Server = createServer((incoming, answer) => {
    const path = incoming.url ?? "";
    recorded.push({ method: incoming.method ?? "", path });
    // A POST's body is read and dropped: an unread body leaves the socket half-spoken and the
    // browser waiting on a form it submitted.
    incoming.resume();
    const named = path.split("?")[0] ?? path;
    if (hanging.has(named)) {
      // Recorded and never answered: the socket stays open until the origin closes, so whatever
      // waits on this request waits for the whole row.
      return;
    }
    const held = unfinished.get(named);
    if (held !== undefined) {
      // The head and the body go, and the response never ends. A browser commits the document
      // and parses what arrived, so the inline script runs; `load` waits on a body that never
      // closes. Chunked, so no Content-Length promises a length the body never reaches, and
      // flushed, so the bytes leave before the row looks. The socket closes with the origin.
      answer.writeHead(200, {
        "Content-Type": "text/html; charset=utf-8",
        "Transfer-Encoding": "chunked",
      });
      answer.write(held);
      answer.flushHeaders();
      open.set(named, (open.get(named) ?? 0) + 1);
      // Should the engine give up on it, the count follows, so the row reads what is true now
      // rather than what was true once.
      answer.on("close", () => {
        open.set(named, Math.max(0, (open.get(named) ?? 0) - 1));
      });
      return;
    }
    const page = served.get(named);
    if (page !== undefined) {
      answer.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
      answer.end(page);
      return;
    }
    if (named.endsWith(".png")) {
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
    serveUnfinished: (path, html) => {
      unfinished.set(path, html);
    },
    openResponses: (path) => open.get(path) ?? 0,
    hang: (path) => {
      hanging.add(path);
    },
    reset: () => {
      recorded = [];
    },
  };
}
