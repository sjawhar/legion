/**
 * The document text read `web/index.html` started before any of this bundle existed.
 *
 * A reader who opens a document is waiting for its bytes, and the one thing that read needs is
 * the address - `/issues/{key}/artifacts/{slug}` names the document as Dispatch's own route
 * does. `main.tsx` asks for it in the entry module's body (`features/doc/open-document-early.ts`),
 * which is as early as any module can, and that is still after every static import of the entry
 * has evaluated: on a phone at 4x CPU throttling the entry's own evaluation is one task of about
 * a second, and the read went out about 1.4 s into the load (dispatch://INFRA-341, R11). The
 * bytes can be asked for while the browser is still parsing the HTML, so an inline script in
 * `web/index.html` asks for them there and leaves the answer here.
 *
 * What it leaves is a `fetch` in flight, not an answer: the page's own client takes it in place
 * of the request it would have made (`createApiClient`), so the read is the client's in every
 * other way - the same deadline on Dispatch's silence, which the client arms over this read's
 * own abort, the same refusals, the same reachability. The HTML carries no credentials or
 * headers of its own because the client's `GET`s carry none either, only `credentials:
 * "same-origin"`, which is what makes the two requests the same request.
 *
 * It is taken once and by exact path. The HTML's rule for which address opens a document is a
 * strict subset of `parseIssuePath`'s - the same issue key, a narrower slug, and a `?v=` it
 * refuses unless the router would read it - and `main.tsx` opens the page's first address in the
 * entry's own first task, so this read is taken there or never made at all. A read nothing takes,
 * one Dispatch refuses and one that never arrives all cost the page nothing: the client falls
 * back to the request it makes today.
 */

/** What `web/index.html` left for the page: the API path it asked for, the answer it is waiting
 *  on, and the abort that cancels it, which the client's deadline takes over. */
export interface EarlyDocumentRead {
  readonly abort: (reason?: unknown) => void;
  readonly path: string;
  readonly response: Promise<Response>;
}

declare global {
  interface Window {
    __dispatchEarlyDocumentRead?: EarlyDocumentRead;
  }
}

/** The early read's answer where `input` and `init` name the request it was made for, and
 *  `undefined` everywhere else; never twice, so a second read of the same document is its own.
 *  The caller's signal - the client's deadline and whatever the caller added to it - aborts the
 *  early request as it aborts one of the client's own. */
export function takeEarlyDocumentRead(
  input: RequestInfo | URL,
  init?: RequestInit
): Promise<Response> | undefined {
  const early = window.__dispatchEarlyDocumentRead;
  if (
    early === undefined ||
    typeof input !== "string" ||
    input !== early.path ||
    (init?.method ?? "GET").toUpperCase() !== "GET"
  ) {
    return undefined;
  }
  window.__dispatchEarlyDocumentRead = undefined;
  const signal = init?.signal ?? undefined;
  if (signal !== undefined) {
    if (signal.aborted) {
      early.abort(signal.reason);
    } else {
      signal.addEventListener("abort", () => early.abort(signal.reason), { once: true });
    }
  }
  return early.response;
}
