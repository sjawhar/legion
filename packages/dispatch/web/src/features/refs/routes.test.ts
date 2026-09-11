import { expect, test } from "bun:test";

import {
  buildDispatchReference,
  buildIssuePath,
  isLegacyLogPath,
  issueTabForRoute,
  parseDispatchReference,
  parseIssuePath,
} from "./routes";

function browserPath(path: string): string {
  const url = new URL(path, "https://dispatch.test/");
  return `${url.pathname}${url.search}`;
}

test("artifact references normalize to the plural browser route", () => {
  const route = parseDispatchReference("dispatch://CORE-1/artifact/design@v3");

  expect(route).toEqual({ key: "CORE-1", kind: "artifact", slug: "design", version: 3 });
  expect(route === undefined ? undefined : buildIssuePath(route)).toBe(
    browserPath("issues/CORE-1/artifacts/design?v=3")
  );
});

test("dispatch references reject plural artifact paths", () => {
  expect(parseDispatchReference("dispatch://CORE-1/artifacts/design")).toBeUndefined();
});

test("artifact route parser accepts the plural and singular browser paths", () => {
  expect(parseIssuePath(browserPath("issues/CORE-1/artifacts/design"), "?v=3")).toEqual({
    key: "CORE-1",
    kind: "artifact",
    slug: "design",
    version: 3,
  });
  expect(parseIssuePath(browserPath("issues/CORE-1/artifact/design"), "")).toEqual({
    key: "CORE-1",
    kind: "artifact",
    slug: "design",
  });
});

test("artifacts tab has its own issue route", () => {
  expect(parseIssuePath(browserPath("issues/CORE-1/artifacts"))).toEqual({
    key: "CORE-1",
    kind: "artifacts",
  });
  expect(buildIssuePath({ key: "CORE-1", kind: "artifacts" })).toBe("/issues/CORE-1/artifacts");
});

test("issue routes select Spec and artifact routes select Artifacts unless primary", () => {
  expect(
    issueTabForRoute(
      { key: "CORE-1", kind: "artifact", slug: "diagram" },
      { kind: "image", primary: false }
    )
  ).toBe("artifacts");
  expect(
    issueTabForRoute(
      { key: "CORE-1", kind: "artifact", slug: "spec" },
      { kind: "doc", primary: true }
    )
  ).toBe("spec");
  expect(issueTabForRoute({ key: "CORE-1", kind: "issue" }, undefined)).toBe("spec");
  expect(issueTabForRoute({ id: "ask-1", key: "CORE-1", kind: "ask" }, undefined)).toBe(
    "conversation"
  );
  expect(issueTabForRoute({ key: "CORE-1", kind: "spec" }, undefined)).toBe("spec");
  expect(issueTabForRoute({ key: "CORE-1", kind: "conversation" }, undefined)).toBe("conversation");
  expect(issueTabForRoute({ key: "CORE-1", kind: "children" }, undefined)).toBe("children");
  expect(issueTabForRoute({ key: "CORE-1", kind: "artifacts" }, undefined)).toBe("artifacts");
  expect(
    issueTabForRoute(
      { key: "CORE-1", kind: "artifact", slug: "diagram", version: 2 },
      { kind: "image", primary: false }
    )
  ).toBe("artifacts");
});

test("reference builders preserve dispatch version syntax while emitting canonical browser paths", () => {
  const route = { key: "CORE-1", kind: "artifact" as const, slug: "design", version: 3 };

  expect(buildDispatchReference(route)).toBe("dispatch://CORE-1/artifact/design@v3");
  expect(buildIssuePath(route)).toBe(browserPath("issues/CORE-1/artifacts/design?v=3"));
});

test("the conversation owns the /conversation path and still answers the retired /log path", () => {
  expect(parseIssuePath("/issues/CORE-1/conversation")).toEqual({
    key: "CORE-1",
    kind: "conversation",
  });
  expect(parseIssuePath("/issues/CORE-1/log")).toEqual({ key: "CORE-1", kind: "conversation" });
  expect(buildIssuePath({ key: "CORE-1", kind: "conversation" })).toBe(
    "/issues/CORE-1/conversation"
  );
  expect(isLegacyLogPath("/issues/CORE-1/log")).toBe(true);
  expect(isLegacyLogPath("/issues/CORE-1/log/")).toBe(true);
  expect(isLegacyLogPath("/issues/CORE-1/conversation")).toBe(false);
  expect(isLegacyLogPath("/issues/CORE-1/artifacts/log")).toBe(false);
});

test("agent references keep the dispatch://KEY/log grammar for the conversation", () => {
  expect(parseDispatchReference("dispatch://CORE-1/log")).toEqual({
    key: "CORE-1",
    kind: "conversation",
  });
  expect(parseDispatchReference("dispatch://CORE-1/conversation")).toEqual({
    key: "CORE-1",
    kind: "conversation",
  });
  expect(buildDispatchReference({ key: "CORE-1", kind: "conversation" })).toBe(
    "dispatch://CORE-1/log"
  );
});
