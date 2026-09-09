import { expect, test } from "bun:test";

import {
  buildDispatchReference,
  buildIssuePath,
  parseDispatchReference,
  parseIssuePath,
} from "./routes";

test("artifact references normalize to the plural browser route", () => {
  const route = parseDispatchReference("dispatch://CORE-1/artifact/design@v3");

  expect(route).toEqual({ key: "CORE-1", kind: "artifact", slug: "design", version: 3 });
  expect(route === undefined ? undefined : buildIssuePath(route)).toBe(
    "/issues/CORE-1/artifacts/design?v=3"
  );
});

test("artifact route parser accepts the canonical and legacy browser forms", () => {
  expect(parseIssuePath("/issues/CORE-1/artifacts/design", "?v=3")).toEqual({
    key: "CORE-1",
    kind: "artifact",
    slug: "design",
    version: 3,
  });
  expect(parseIssuePath("/issues/CORE-1/artifact/design", "")).toEqual({
    key: "CORE-1",
    kind: "artifact",
    slug: "design",
  });
});

test("reference builders preserve dispatch version syntax while emitting canonical browser paths", () => {
  const route = { key: "CORE-1", kind: "artifact" as const, slug: "design", version: 3 };

  expect(buildDispatchReference(route)).toBe("dispatch://CORE-1/artifact/design@v3");
  expect(buildIssuePath(route)).toBe("/issues/CORE-1/artifacts/design?v=3");
});
