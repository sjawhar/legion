import { expect, test } from "bun:test";
import { requestsThatReachedNetwork } from "./network";

// What the policy claims is that a request no handler answers fails the test that made it. A test
// that merely made one would leave this file red for good, so each test here makes the request,
// takes the report the preload would have failed it with (`requestsThatReachedNetwork` empties
// what it reports), and asserts that the report names that request - which is the same report,
// from the same recorder, on the same path.

test("a read no handler answers is reported with its method and path", async () => {
  const response = await fetch("/api/v1/issues/NO-HANDLER");
  // The frame is errored rather than performed: nothing leaves the process for `dispatch.test`.
  expect(response.ok).toBe(false);
  expect(requestsThatReachedNetwork()).toEqual(["GET /api/v1/issues/NO-HANDLER"]);
});

test("an upload no handler answers is reported with its method and path", async () => {
  const { promise, resolve } = Promise.withResolvers<void>();
  const upload = new XMLHttpRequest();
  upload.onload = () => resolve();
  upload.onerror = () => resolve();
  upload.open("POST", "/api/v1/issues/NO-HANDLER/artifacts");
  upload.send(new FormData());
  await promise;
  expect(requestsThatReachedNetwork()).toEqual(["POST /api/v1/issues/NO-HANDLER/artifacts"]);
});

test("a request a handler answers is not reported", async () => {
  // One of the shell's own routes, which `network.ts` answers for every page.
  const response = await fetch("/api/v1/agents");
  expect(response.status).toBe(200);
  expect(await response.json()).toEqual([]);
  expect(requestsThatReachedNetwork()).toEqual([]);
});
