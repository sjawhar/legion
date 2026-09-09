import { expect, test } from "bun:test";

import { buildIssuePath } from "../refs/routes";

import { canSubmitComposer, composerReferences } from "./Composer";

test("composer turns typed dispatch and pasted same-origin links into reference chips", () => {
  expect(
    composerReferences(
      "See dispatch://CORE-1/spec and https://dispatch.test/issues/CORE-1/artifact/design",
      "https://dispatch.test"
    )
  ).toEqual([
    {
      href: buildIssuePath({ key: "CORE-1", kind: "spec" }),
      reference: "dispatch://CORE-1/spec",
    },
    {
      href: buildIssuePath({ key: "CORE-1", kind: "artifact", slug: "design" }),
      reference: "dispatch://CORE-1/artifact/design",
    },
  ]);
});

test("composer emits canonical browser routes for versioned dispatch references", () => {
  expect(composerReferences("dispatch://CORE-1/artifact/design@v3")).toEqual([
    {
      href: buildIssuePath({ key: "CORE-1", kind: "artifact", slug: "design", version: 3 }),
      reference: "dispatch://CORE-1/artifact/design@v3",
    },
  ]);
});

test("composer holds Save until every image upload settles", () => {
  expect(canSubmitComposer("comment", "See image", "", false, 1)).toBe(false);
  expect(canSubmitComposer("comment", "See image", "", false, 0)).toBe(true);
});
