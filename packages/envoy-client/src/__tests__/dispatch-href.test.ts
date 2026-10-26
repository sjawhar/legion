import { expect, test } from "bun:test";
import { DISPATCH_HREF_REFERENCES } from "@legion/contracts";

import { dispatchRefFromUrl } from "../dispatch-execute";
import { parseDispatchRef } from "../dispatch-owner";

// The agent-side half of the shared golden table: `dispatch_search` prints these hrefs, and an
// agent pastes one back as a ref, so every row has to resolve to the reference it names. The
// SPA's `features/refs/dispatch-href.test.ts` walks the same table for the browser parser.
const server = "http://dispatch.test";

test.each(
  DISPATCH_HREF_REFERENCES.map((row) => [row.href, row.ref] as const)
)("%s resolves to %s", (href, ref) => {
  expect(dispatchRefFromUrl(`${server}${href}`, server)).toBe(ref);
});

test("a dashboard URL from another server is not a reference", () => {
  expect(dispatchRefFromUrl("http://elsewhere.test/issues/CORE-1", server)).toBeUndefined();
});

// The Activity tab's own page and reference, and the Conversation's unchanged one: a reference
// written before the Activity tab existed still names the conversation.
test("the issue's activity and conversation pages keep their own references", () => {
  expect(dispatchRefFromUrl(`${server}/issues/CORE-1/activity`, server)).toBe(
    "dispatch://CORE-1/activity"
  );
  expect(dispatchRefFromUrl(`${server}/issues/CORE-1/conversation`, server)).toBe(
    "dispatch://CORE-1/log"
  );
});

test("an issue document URL carrying both item parameters is refused", () => {
  expect(
    dispatchRefFromUrl(`${server}/issues/CORE-1/spec?comment=c1&ask=a1`, server)
  ).toBeUndefined();
});

// A block's link on a live document page names the block, its id written as `encodeBlockId`
// writes it; a version page, an item in the query, or a fragment that names no block leaves it.
test.each([
  ["/issues/CORE-1/spec#b-ship_it", "dispatch://CORE-1/spec#b-ship%5Fit"],
  ["/issues/CORE-1/artifacts/notes-md?v=2#b-ship-it", "dispatch://CORE-1/artifact/notes-md@v2"],
  ["/issues/CORE-1/spec?v=2#b-ship-it", "dispatch://CORE-1/artifact/spec@v2"],
  [
    "/projects/CORE/documents/handbook-md?version=3#b-ship-it",
    "dispatch://CORE/artifact/handbook-md@v3",
  ],
  [
    "/issues/CORE-1/spec?comment=44444444-4444-4444-8444-444444444444#b-ship-it",
    "dispatch://CORE-1/comment/44444444-4444-4444-8444-444444444444",
  ],
  // A pin is written only as a dispatch:// reference, so a raw @ here names no block.
  ["/issues/CORE-1/spec#b-ship-it@v2", "dispatch://CORE-1/spec"],
  ["/issues/CORE-1/spec#b-%ZZ", "dispatch://CORE-1/spec"],
  ["/issues/CORE-1/spec#b-", "dispatch://CORE-1/spec"],
])("%s resolves to %s", (href, ref) => {
  expect(dispatchRefFromUrl(`${server}${href}`, server)).toBe(ref);
});

// A dashboard URL an agent passes is a read request, so it names what its page shows whatever the
// length of its document's name: the page of a document whose slug is 2683 characters, which a
// new upload's reference key still holds, is that document, a block on it is the block, and
// `parseDispatchRef` reads each, for an issue's document and a project's alike.
test.each([
  ["CORE-1", "/issues/CORE-1/artifacts/", "dispatch://CORE-1/artifact/"],
  ["CORE", "/projects/CORE/documents/", "dispatch://CORE/artifact/"],
])("a long-named %s document's page is its reference", (_owner, page, document) => {
  const slug = "a".repeat(2683);
  const whole = dispatchRefFromUrl(`${server}${page}${slug}`, server);
  expect(whole).toBe(`${document}${slug}`);
  expect(parseDispatchRef(whole ?? "")).toMatchObject({ kind: "artifact", id: slug });
  // `é` is two bytes, written %C3%A9.
  const block = dispatchRefFromUrl(`${server}${page}${slug}#b-%C3%A9`, server);
  expect(block).toBe(`${document}${slug}#b-%C3%A9`);
  expect(parseDispatchRef(block ?? "")).toMatchObject({ kind: "block", id: slug, block: "é" });
});
