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

test("an issue document URL carrying both item parameters is refused", () => {
  expect(
    dispatchRefFromUrl(`${server}/issues/CORE-1/spec?comment=c1&ask=a1`, server)
  ).toBeUndefined();
});
