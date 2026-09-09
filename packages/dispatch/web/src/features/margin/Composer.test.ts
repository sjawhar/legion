import { expect, test } from "bun:test";

import { canSubmitComposer, composerReferences } from "./Composer";

test("composer turns typed dispatch and pasted same-origin links into reference chips", () => {
  expect(
    composerReferences(
      "See dispatch://CORE-1/spec and https://dispatch.test/issues/CORE-1/artifact/design",
      "https://dispatch.test"
    )
  ).toEqual([
    { href: "/issues/CORE-1/spec", reference: "dispatch://CORE-1/spec" },
    {
      href: "/issues/CORE-1/artifact/design",
      reference: "dispatch://CORE-1/artifact/design",
    },
  ]);
});

test("composer holds Save until every image upload settles", () => {
  expect(canSubmitComposer("comment", "See image", "", false, 1)).toBe(false);
  expect(canSubmitComposer("comment", "See image", "", false, 0)).toBe(true);
});
