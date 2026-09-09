import { expect, test } from "bun:test";

import { composerReferences } from "./Composer";

test("composer turns typed dispatch and pasted same-origin links into reference chips", () => {
  expect(
    composerReferences(
      "See dispatch://CORE-1/spec and https://dispatch.test/issues/CORE-1/artifacts/design",
      "https://dispatch.test"
    )
  ).toEqual([
    { href: "/issues/CORE-1/spec", reference: "dispatch://CORE-1/spec" },
    {
      href: "/issues/CORE-1/artifacts/design",
      reference: "dispatch://CORE-1/artifact/design",
    },
  ]);
});
