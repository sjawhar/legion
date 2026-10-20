import { expect, test } from "bun:test";
import { DISPATCH_HREF_REFERENCES } from "@legion/contracts";

import { dispatchRefFromUrl } from "../dispatch-execute";

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
