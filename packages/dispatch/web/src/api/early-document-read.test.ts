import { afterEach, expect, test } from "bun:test";

import { takeEarlyDocumentRead } from "./early-document-read";

const PATH = "/api/v1/issues/CORE-1/artifacts/spec-md/text";

/** What `web/index.html` leaves on the window: the path it asked for, the answer it waits on,
 *  and the abort the page's deadline takes over. */
function earlyRead(path = PATH): { aborted: unknown[]; response: Promise<Response> } {
  const aborted: unknown[] = [];
  const response = Promise.resolve(Response.json({ text: "# A document" }));
  window.__dispatchEarlyDocumentRead = {
    abort: (reason) => aborted.push(reason),
    path,
    response,
  };
  return { aborted, response };
}

afterEach(() => {
  window.__dispatchEarlyDocumentRead = undefined;
});

test("the read is taken by exact path, and only once", () => {
  const { response } = earlyRead();
  expect(takeEarlyDocumentRead(PATH, {})).toBe(response);
  expect(takeEarlyDocumentRead(PATH, {})).toBeUndefined();
});

test("nothing but the request it was made for takes it", () => {
  const { response } = earlyRead();
  expect(takeEarlyDocumentRead("/api/v1/artifacts/an-id/text", {})).toBeUndefined();
  expect(takeEarlyDocumentRead(`${PATH}?v=2`, {})).toBeUndefined();
  expect(takeEarlyDocumentRead(new URL(`https://dispatch.test${PATH}`), {})).toBeUndefined();
  expect(takeEarlyDocumentRead(PATH, { method: "POST" })).toBeUndefined();
  expect(takeEarlyDocumentRead(PATH, {})).toBe(response);
});

test("an address that started no early read takes nothing", () => {
  expect(takeEarlyDocumentRead(PATH, {})).toBeUndefined();
});

test("the caller's deadline aborts the early request, whenever it fires", () => {
  const first = earlyRead();
  const running = new AbortController();
  takeEarlyDocumentRead(PATH, { signal: running.signal });
  expect(first.aborted).toEqual([]);
  running.abort("too slow");
  expect(first.aborted).toEqual(["too slow"]);

  const second = earlyRead();
  const already = new AbortController();
  already.abort("gone");
  takeEarlyDocumentRead(PATH, { signal: already.signal });
  expect(second.aborted).toEqual(["gone"]);
});
