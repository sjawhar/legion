import { afterEach, beforeEach, expect, test } from "bun:test";
import type { Window as HappyDOMWindow } from "happy-dom";

import { writeReachingNetwork, writesThatReachedNetwork } from "./network-guard";

// happy-dom's page starts at `about:blank`, where `XMLHttpRequest.open` refuses a relative url
// outright, so the upload's row puts the page on an origin as the dashboard is served from one.
const testWindow = window as unknown as HappyDOMWindow;
let pageBefore = "about:blank";

beforeEach(() => {
  pageBefore = window.location.href;
  testWindow.happyDOM.setURL("https://dispatch.test/issues/CORE-1/artifacts");
});

afterEach(() => {
  testWindow.happyDOM.setURL(pageBefore);
});

// W59. A unit test has no Dispatch, so a write that reaches the network was answered by nothing
// the test set up. A read is let through: hundreds of tests leave incidental reads unanswered.
test("a write is caught by its method, and a read is let through", () => {
  expect(writeReachingNetwork("POST", "/api/v1/issues")).toBe("POST /api/v1/issues");
  expect(writeReachingNetwork("patch", "/api/v1/issues/CORE-1")).toBe(
    "PATCH /api/v1/issues/CORE-1"
  );
  expect(writeReachingNetwork("DELETE", "/api/v1/issues/CORE-1")).toBe(
    "DELETE /api/v1/issues/CORE-1"
  );
  expect(writeReachingNetwork("GET", "/api/v1/issues")).toBeUndefined();
  expect(writeReachingNetwork("HEAD", "/api/v1/issues")).toBeUndefined();
});

// W59. The page sends an upload through `XMLHttpRequest` (`client.ts`, for its progress events),
// and happy-dom's `XMLHttpRequest` has a `Fetch` of its own rather than `globalThis.fetch`: the
// guard the preload installs must catch it there too, or an upload a test leaves retrying reaches
// the network unreported.
test("an upload's XMLHttpRequest write is refused and reported", () => {
  const request = new XMLHttpRequest();
  request.open("POST", "/api/v1/issues/CORE-1/artifacts");
  expect(() => request.send(new FormData())).toThrow(
    "a unit test's write reached the network: POST /api/v1/issues/CORE-1/artifacts"
  );
  // The preload's `afterEach` reports what reached the network, so a write still retrying after a
  // file's last test fails that file rather than passing unseen. This test takes its own back, so
  // nothing of it is left for that report.
  expect(writesThatReachedNetwork()).toEqual(["POST /api/v1/issues/CORE-1/artifacts"]);
  expect(writesThatReachedNetwork()).toEqual([]);
});

test("a write through fetch is refused and reported", async () => {
  await expect(fetch("/api/v1/issues", { body: "{}", method: "POST" })).rejects.toThrow(
    "a unit test's write reached the network: POST /api/v1/issues"
  );
  expect(writesThatReachedNetwork()).toEqual(["POST /api/v1/issues"]);
});
