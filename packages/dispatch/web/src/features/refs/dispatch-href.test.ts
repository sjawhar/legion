import { expect, test } from "bun:test";
import { DISPATCH_HREF_REFERENCES } from "@legion/contracts";

import { referenceRouteFromHref } from "./RefLink";
import { parseDispatchReference } from "./routes";

// The SPA's half of the shared golden table: every dashboard href Dispatch emits must name the
// same thing to the URL parser as its `dispatch://` reference does to the reference parser.
// `envoy-client`'s `dispatch-execute.test.ts` walks the same table for the agent-side parser, so
// a new href shape cannot reach one parser and not the other.
test.each(
  DISPATCH_HREF_REFERENCES.map((row) => [row.href, row.ref] as const)
)("%s names the same reference as %s", (href, ref) => {
  expect(referenceRouteFromHref(href, "http://dispatch.test")).toEqual(parseDispatchReference(ref));
});

test("an issue document href carrying both item parameters names nothing", () => {
  expect(
    referenceRouteFromHref("/issues/CORE-1/spec?comment=c1&ask=a1", "http://dispatch.test")
  ).toBeUndefined();
});
