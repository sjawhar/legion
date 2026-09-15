import { expect, test } from "bun:test";

import {
  buildDispatchReference,
  buildInboxPath,
  buildIssuePath,
  buildProjectPath,
  documentRoute,
  issueTabForRoute,
  itemRoute,
  parseDispatchReference,
  parseInboxSearch,
  parseIssuePath,
  parseProjectPath,
} from "./routes";

function browserPath(path: string): string {
  const url = new URL(path, "https://dispatch.test/");
  return `${url.pathname}${url.search}`;
}

test("artifact references normalize to the plural browser route", () => {
  const route = parseDispatchReference("dispatch://CORE-1/artifact/design@v3");

  expect(route).toEqual({ key: "CORE-1", kind: "artifact", slug: "design", version: 3 });
  if (route?.kind !== "artifact") {
    throw new Error("artifact reference did not parse as an artifact route");
  }
  expect(buildIssuePath(route)).toBe(browserPath("issues/CORE-1/artifacts/design?v=3"));
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

test("message routes round-trip through the dispatch:// reference and the browser path", () => {
  const route = { id: "message-1", key: "CORE-1", kind: "message" as const };

  expect(parseDispatchReference("dispatch://CORE-1/message/message-1")).toEqual(route);
  expect(buildDispatchReference(route)).toBe("dispatch://CORE-1/message/message-1");
  expect(parseIssuePath("/issues/CORE-1/messages/message-1")).toEqual(route);
  expect(buildIssuePath(route)).toBe(browserPath("issues/CORE-1/messages/message-1"));
});

test("ask routes round-trip through the dispatch:// reference and the browser path", () => {
  const route = { id: "ask-1", key: "CORE-1", kind: "ask" as const };

  expect(parseDispatchReference("dispatch://CORE-1/ask/ask-1")).toEqual(route);
  expect(buildDispatchReference(route)).toBe("dispatch://CORE-1/ask/ask-1");
  expect(parseIssuePath("/issues/CORE-1/asks/ask-1")).toEqual(route);
  expect(buildIssuePath(route)).toBe(browserPath("issues/CORE-1/asks/ask-1"));
  expect(issueTabForRoute(route, undefined)).toBe("conversation");
});

test("a document is referenced as the spec, an issue artifact, or a project document; a version pins the artifact form", () => {
  const spec = {
    issue_key: "CORE-1",
    kind: "doc" as const,
    primary: true,
    project: "CORE",
    slug: "spec",
  };
  expect(buildDispatchReference(documentRoute(spec))).toBe("dispatch://CORE-1/spec");
  expect(buildDispatchReference(documentRoute(spec, 3))).toBe("dispatch://CORE-1/artifact/spec@v3");
  expect(buildDispatchReference(documentRoute({ ...spec, primary: false, slug: "notes" }))).toBe(
    "dispatch://CORE-1/artifact/notes"
  );
  const projectDocument = { ...spec, issue_key: null, primary: false, slug: "design notes" };
  expect(buildDispatchReference(documentRoute(projectDocument))).toBe(
    "dispatch://CORE/artifact/design%20notes"
  );
  expect(buildDispatchReference(documentRoute(projectDocument, 2))).toBe(
    "dispatch://CORE/artifact/design%20notes@v2"
  );
});

test("an ask or comment is referenced under its issue, else under the named project document", () => {
  const document = { project: "CORE", slug: "design-notes" };
  expect(itemRoute("ask", { id: "ask-1", issue_key: "CORE-1" }, document)).toEqual({
    id: "ask-1",
    key: "CORE-1",
    kind: "ask",
  });
  expect(itemRoute("comment", { id: "c-1", issue_key: null }, document)).toEqual({
    item: { id: "c-1", kind: "comment" },
    kind: "document",
    project: "CORE",
    slug: "design-notes",
  });
  expect(itemRoute("ask", { id: "ask-1", issue_key: null }, undefined)).toBeUndefined();
});

test("the conversation owns the /conversation browser path", () => {
  expect(parseIssuePath("/issues/CORE-1/conversation")).toEqual({
    key: "CORE-1",
    kind: "conversation",
  });
  expect(buildIssuePath({ key: "CORE-1", kind: "conversation" })).toBe(
    "/issues/CORE-1/conversation"
  );
  expect(parseIssuePath("/issues/CORE-1/log")).toBeUndefined();
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

test("parses and builds project, documents, and document routes with version and item query", () => {
  expect(parseProjectPath("/projects/CORE")).toEqual({ kind: "project", project: "CORE" });
  // The filter strip's URL state (`?label=`, `?q=`, `?needs-you=`, `?unread=`) never changes
  // what route a project path is.
  expect(
    parseProjectPath("/projects/CORE", "?label=x&label=y&q=ship&needs-you=1&unread=1")
  ).toEqual({ kind: "project", project: "CORE" });
  expect(parseProjectPath("/projects/CORE/documents")).toEqual({
    kind: "documents",
    project: "CORE",
  });
  expect(
    parseProjectPath("/projects/CORE/documents/design-notes", "?version=3&comment=note-1")
  ).toEqual({
    item: { id: "note-1", kind: "comment" },
    kind: "document",
    project: "CORE",
    slug: "design-notes",
    version: 3,
  });
  expect(
    buildProjectPath({
      item: { id: "ask-1", kind: "ask" },
      kind: "document",
      project: "CORE",
      slug: "design-notes",
      version: 2,
    })
  ).toBe("/projects/CORE/documents/design-notes?version=2&ask=ask-1");
});

test("maps dispatch project artifact references onto the document path and back", () => {
  const route = parseDispatchReference("dispatch://CORE/artifact/design-notes@v3/comment/note-1");

  expect(route).toEqual({
    item: { id: "note-1", kind: "comment" },
    kind: "document",
    project: "CORE",
    slug: "design-notes",
    version: 3,
  });
  if (route?.kind !== "document") {
    throw new Error("project artifact reference did not parse as a document route");
  }
  expect(buildProjectPath(route)).toBe(
    "/projects/CORE/documents/design-notes?version=3&comment=note-1"
  );
  expect(buildDispatchReference(route)).toBe(
    "dispatch://CORE/artifact/design-notes@v3/comment/note-1"
  );
});

test("a dashed key is never a project route and a bare project is never an issue route", () => {
  expect(parseProjectPath("/projects/CORE-1")).toBeUndefined();
  expect(parseIssuePath("/issues/CORE")).toBeUndefined();
  expect(parseDispatchReference("dispatch://CORE")).toBeUndefined();
});

test("inbox agent filters round-trip through the query string and ignore unknown sections", () => {
  expect(buildInboxPath()).toBe("/");
  expect(buildInboxPath({ agent: "s 1", section: "needs-you" })).toBe(
    "/?agent=s+1&section=needs-you"
  );
  expect(parseInboxSearch("?agent=s+1&section=needs-you")).toEqual({
    agent: "s 1",
    section: "needs-you",
  });
  expect(parseInboxSearch("?agent=&section=later")).toEqual({});
});

test("inbox view round-trips through the query string and ignores unknown views", () => {
  expect(buildInboxPath({ view: "everyone" })).toBe("/?view=everyone");
  expect(buildInboxPath({ agent: "s1", view: "mine" })).toBe("/?agent=s1&view=mine");
  expect(parseInboxSearch("?view=everyone")).toEqual({ view: "everyone" });
  expect(parseInboxSearch("?view=mine&section=needs-you")).toEqual({
    section: "needs-you",
    view: "mine",
  });
  expect(parseInboxSearch("?view=theirs")).toEqual({});
});
