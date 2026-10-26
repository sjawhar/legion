import { expect, test } from "bun:test";

import {
  buildDispatchReference,
  buildInboxPath,
  buildIssuePath,
  buildProjectPath,
  buildReferencePath,
  documentItemPath,
  documentRoute,
  issueTabForRoute,
  itemRoute,
  parseAgentArtifactPath,
  parseDispatchReference,
  parseInboxSearch,
  parseIssuePath,
  parseNodeReference,
  parseProjectPath,
  pictureReference,
  referenceRouteFromHref,
  referenceTargetKind,
  routeHasMargin,
  routeProjectOf,
  shortForm,
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

// The target bound holds text alone: a long-named document's reference is no citation in text,
// but the graph returns it as a node's own reference, and the dashboard opens and previews that
// node by it, a block of it too.
test("a long-named document's own reference is its node, though text past the bound cites nothing", () => {
  const slug = "a".repeat(2683);
  const document = `dispatch://CORE-1/artifact/${slug}`;
  expect(parseDispatchReference(document)).toBeUndefined();
  expect(parseNodeReference(document)).toEqual({ key: "CORE-1", kind: "artifact", slug });
  expect(parseNodeReference(`${document}#b-p1`)).toEqual({
    block: "p1",
    key: "CORE-1",
    kind: "artifact",
    slug,
  });
});

test("dispatch references reject plural artifact paths", () => {
  expect(parseDispatchReference("dispatch://CORE-1/artifacts/design")).toBeUndefined();
});

test("a route's project is a project path's key or the prefix of an issue path's key", () => {
  expect(routeProjectOf("/projects/CORE/issues")).toBe("CORE");
  expect(routeProjectOf("/projects/OPS2")).toBe("OPS2");
  expect(routeProjectOf("/issues/CORE-12/spec")).toBe("CORE");
  expect(routeProjectOf("/issues/AB9-3")).toBe("AB9");
  expect(routeProjectOf("/agents")).toBeUndefined();
  expect(routeProjectOf("/")).toBeUndefined();
});

test("an item's document path names the document it belongs to, never the issue's spec by default", () => {
  const primary = {
    issue_key: "CORE-1",
    kind: "doc" as const,
    primary: true,
    project: "CORE",
    slug: "spec",
  };
  const secondary = { ...primary, primary: false, slug: "expert-message-v4-md" };
  const projectDocument = { ...primary, issue_key: null, primary: false, slug: "handbook-md" };

  expect(documentItemPath(primary, { id: "c1", kind: "comment" })).toBe(
    "/issues/CORE-1/spec?comment=c1"
  );
  expect(documentItemPath(secondary, { id: "c2", kind: "comment" })).toBe(
    "/issues/CORE-1/artifacts/expert-message-v4-md?comment=c2"
  );
  expect(documentItemPath(secondary, { id: "a1", kind: "ask" })).toBe(
    "/issues/CORE-1/artifacts/expert-message-v4-md?ask=a1"
  );
  // A typed ask block is reached by its block fragment, which is how a decision block focuses.
  expect(documentItemPath(secondary, { blockID: "ship-it", id: "a1", kind: "ask" })).toBe(
    "/issues/CORE-1/artifacts/expert-message-v4-md#b-ship-it"
  );
  expect(documentItemPath(projectDocument, { id: "c3", kind: "comment" })).toBe(
    "/projects/CORE/documents/handbook-md?comment=c3"
  );
});

test("every block link writes its id as encodeBlockId does, so `_` is %5F", () => {
  const notes = {
    issue_key: "CORE-1",
    kind: "doc" as const,
    primary: false,
    project: "CORE",
    slug: "notes-md",
  };
  expect(documentItemPath(notes, { blockID: "ship_it", id: "a1", kind: "ask" })).toBe(
    "/issues/CORE-1/artifacts/notes-md#b-ship%5Fit"
  );
  const block = { block: "ship_it", key: "CORE-1", kind: "spec" as const };
  expect(buildDispatchReference(block)).toBe("dispatch://CORE-1/spec#b-ship%5Fit");
  expect(buildReferencePath(block)).toBe("/issues/CORE-1/spec#b-ship%5Fit");
  // Read back with any percent-encoding, the reference and the page name the same block.
  expect(parseDispatchReference("dispatch://CORE-1/spec#b-ship_it")).toEqual(block);
  expect(
    referenceRouteFromHref("/issues/CORE-1/spec#b-ship%5Fit", "https://dispatch.test")
  ).toEqual(block);
});

test("a pinned block reference round-trips with its pin, and opens the version it pins", () => {
  const pinned = parseDispatchReference("dispatch://CORE-1/artifact/notes-md#b-ship%5Fit@v2");
  expect(pinned).toEqual({
    block: "ship_it",
    key: "CORE-1",
    kind: "artifact",
    pin: "2",
    slug: "notes-md",
  });
  if (pinned === undefined) throw new Error("the pinned reference did not parse");
  expect(buildDispatchReference(pinned)).toBe("dispatch://CORE-1/artifact/notes-md#b-ship%5Fit@v2");
  expect(referenceTargetKind(pinned)).toBe("block");
  expect(shortForm(pinned)).toBe("CORE-1 notes-md block v2");
  // A version's stored text holds no block ids, so a pinned block opens the version itself.
  expect(buildReferencePath(pinned)).toBe("/issues/CORE-1/artifacts/notes-md?v=2");

  const document = parseDispatchReference("dispatch://CORE/artifact/handbook-md#b-p1@v7");
  if (document === undefined) throw new Error("the pinned project reference did not parse");
  expect(buildDispatchReference(document)).toBe("dispatch://CORE/artifact/handbook-md#b-p1@v7");
  expect(buildReferencePath(document)).toBe("/projects/CORE/documents/handbook-md?version=7");

  // A spec keeps its versions under its own slug: with it, the version; without it, the live spec
  // at the block.
  const spec = parseDispatchReference("dispatch://CORE-1/spec#b-p1@v3");
  if (spec === undefined) throw new Error("the pinned spec reference did not parse");
  expect(buildDispatchReference(spec)).toBe("dispatch://CORE-1/spec#b-p1@v3");
  expect(buildReferencePath(spec, "core-1-md")).toBe("/issues/CORE-1/artifacts/core-1-md?v=3");
  expect(buildReferencePath(spec)).toBe("/issues/CORE-1/spec#b-p1");

  // Unpinned, a block opens the live document at the block.
  const live = parseDispatchReference("dispatch://CORE-1/spec#b-p1");
  if (live === undefined) throw new Error("the block reference did not parse");
  expect(buildReferencePath(live, "core-1-md")).toBe("/issues/CORE-1/spec#b-p1");
  expect(referenceTargetKind(live)).toBe("block");
});

test("a pin past what a number holds exactly is written back digit for digit, as the server reads it", () => {
  // 2^63 - 1, the largest pin the server's Go `int` holds; as a number it would print 2^63.
  const reference = "dispatch://CORE-1/spec#b-p1@v9223372036854775807";
  const pinned = parseDispatchReference(reference);
  if (pinned === undefined) throw new Error("the largest pin did not parse");
  expect(buildDispatchReference(pinned)).toBe(reference);
  expect(shortForm(pinned)).toBe("CORE-1 spec block v9223372036854775807");
  expect(buildReferencePath(pinned, "core-1-md")).toBe(
    "/issues/CORE-1/artifacts/core-1-md?v=9223372036854775807"
  );
  const document = parseDispatchReference(
    "dispatch://CORE/artifact/handbook-md#b-p1@v9223372036854775807"
  );
  if (document === undefined) throw new Error("the largest project pin did not parse");
  expect(buildReferencePath(document)).toBe(
    "/projects/CORE/documents/handbook-md?version=9223372036854775807"
  );
  // One more names nothing, nor does a pin a number would print as `1e+21`.
  expect(
    parseDispatchReference("dispatch://CORE-1/spec#b-p1@v9223372036854775808")
  ).toBeUndefined();
  expect(
    parseDispatchReference("dispatch://CORE-1/spec#b-p1@v1000000000000000000000")
  ).toBeUndefined();
});

test("a block names nothing on a versioned document, an item, an issue or a malformed pin", () => {
  for (const reference of [
    "dispatch://CORE-1/artifact/notes-md@v2#b-p1",
    "dispatch://CORE/artifact/handbook-md/comment/c1#b-p1",
    "dispatch://CORE-1#b-p1",
    "dispatch://CORE-1/comment/c1#b-p1",
    "dispatch://CORE-1/spec#b-p1@v",
    "dispatch://CORE-1/spec#b-p1@v0",
    "dispatch://CORE-1/spec#b-p1@v01",
    "dispatch://CORE-1/spec#b-a@b",
    "dispatch://CORE-1/spec#b-",
  ]) {
    expect(parseDispatchReference(reference)).toBeUndefined();
  }
  // A page's raw `@` is no pin, and a version page or an item page is the page whatever its
  // fragment.
  const origin = "https://dispatch.test";
  expect(referenceRouteFromHref("/issues/CORE-1/spec#b-p1@v2", origin)).toEqual({
    key: "CORE-1",
    kind: "spec",
  });
  expect(referenceRouteFromHref("/issues/CORE-1/artifacts/notes-md?v=2#b-p1", origin)).toEqual({
    key: "CORE-1",
    kind: "artifact",
    slug: "notes-md",
    version: 2,
  });
  expect(referenceRouteFromHref("/issues/CORE-1/spec?comment=c1#b-p1", origin)).toEqual({
    id: "c1",
    key: "CORE-1",
    kind: "comment",
  });
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
  expect(issueTabForRoute({ key: "CORE-1", kind: "activity" }, undefined)).toBe("activity");
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

test("the activity tab owns its path, its reference and its short form", () => {
  const route = { key: "CORE-1", kind: "activity" as const };

  expect(parseDispatchReference("dispatch://CORE-1/activity")).toEqual(route);
  expect(buildDispatchReference(route)).toBe("dispatch://CORE-1/activity");
  expect(shortForm(route)).toBe("CORE-1 activity");
  expect(parseIssuePath("/issues/CORE-1/activity")).toEqual(route);
  expect(buildIssuePath(route)).toBe(browserPath("issues/CORE-1/activity"));
  expect(
    referenceRouteFromHref("https://dispatch.test/issues/CORE-1/activity", "https://dispatch.test")
  ).toEqual(route);
});

test("parses and builds project, documents, and document routes with version and item query", () => {
  expect(parseProjectPath("/projects/CORE")).toEqual({ kind: "project", project: "CORE" });
  // The filter strip's URL state (`?label=`, `?q=`, `?needs-you=`, `?unread=`) never changes
  // what route a project path is.
  expect(
    parseProjectPath("/projects/CORE", "?label=x&label=y&q=ship&needs-you=1&unread=1")
  ).toEqual({ kind: "project", project: "CORE" });
  // The Issues tab and its filters live at /issues; the Architecture pane at /architecture
  // (its `?component=` state is not part of the route either).
  expect(parseProjectPath("/projects/CORE/issues", "?label=x&status=todo")).toEqual({
    kind: "issues",
    project: "CORE",
  });
  expect(parseProjectPath("/projects/CORE/architecture", "?component=web")).toEqual({
    kind: "architecture",
    project: "CORE",
  });
  expect(buildProjectPath({ kind: "issues", project: "CORE" })).toBe("/projects/CORE/issues");
  expect(buildProjectPath({ kind: "architecture", project: "CORE" })).toBe(
    "/projects/CORE/architecture"
  );
  expect(parseProjectPath("/projects/CORE/architecture/web")).toBeUndefined();
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

test("an agent conversation's artifact round-trips through its reference, its page and a dashboard link", () => {
  const pinned = {
    kind: "agent-artifact" as const,
    session: "ses-1",
    slug: "shot-png",
    version: 2,
  };
  expect(parseDispatchReference("dispatch://agent/ses-1/artifact/shot-png@v2")).toEqual(pinned);
  expect(buildDispatchReference(pinned)).toBe("dispatch://agent/ses-1/artifact/shot-png@v2");
  expect(buildReferencePath(pinned)).toBe("/agents/ses-1/artifacts/shot-png?v=2");
  expect(parseAgentArtifactPath("/agents/ses-1/artifacts/shot-png", "?v=2")).toEqual(pinned);
  expect(
    referenceRouteFromHref(
      "https://dispatch.test/agents/ses-1/artifacts/shot-png?v=2",
      "https://dispatch.test"
    )
  ).toEqual(pinned);
  expect(referenceTargetKind(pinned)).toBe("document");
  // Until its title resolves, a link to it reads as the session's short id and the slug.
  expect(shortForm({ ...pinned, session: "0123456789abcdef" })).toBe(
    "agent/session:01234567…/shot-png"
  );

  const latest = { kind: "agent-artifact" as const, session: "ses-1", slug: "shot-png" };
  expect(parseDispatchReference("dispatch://agent/ses-1/artifact/shot-png")).toEqual(latest);
  expect(buildDispatchReference(latest)).toBe("dispatch://agent/ses-1/artifact/shot-png");
  expect(buildReferencePath(latest)).toBe("/agents/ses-1/artifacts/shot-png");

  // A reference names the session as written; its page path encodes it, and reading the page
  // decodes it back to the same id.
  const written = parseDispatchReference("dispatch://agent/ses%2F1/artifact/shot-png");
  expect(written).toEqual({ kind: "agent-artifact", session: "ses%2F1", slug: "shot-png" });
  if (written === undefined) throw new Error("the written id did not parse");
  expect(buildReferencePath(written)).toBe("/agents/ses%252F1/artifacts/shot-png");
  expect(parseAgentArtifactPath("/agents/ses%252F1/artifacts/shot-png")).toEqual({
    kind: "agent-artifact",
    session: "ses%2F1",
    slug: "shot-png",
  });
});

test("an agent artifact names a session that holds no slash, query, fragment or space", () => {
  for (const reference of [
    "dispatch://agent/a/b/artifact/shot-png",
    "dispatch://agent/a#b/artifact/shot-png",
    "dispatch://agent/a?b/artifact/shot-png",
    "dispatch://agent/a\u00a0b/artifact/shot-png",
    "dispatch://agent/a\ufeffb/artifact/shot-png",
    "dispatch://agent//artifact/shot-png",
    "dispatch://agent/ses-1/artifact/Shot-png",
    "dispatch://agent/ses-1/artifact/shot-png@v0",
    "dispatch://agent/ses-1",
  ]) {
    expect(parseDispatchReference(reference)).toBeUndefined();
  }
  for (const path of ["/agents/a%2Fb/artifacts/shot-png", "/agents/a%20b/artifacts/shot-png"]) {
    expect(parseAgentArtifactPath(path)).toBeUndefined();
  }
  expect(parseAgentArtifactPath("/agents/ses-1/artifacts/shot-png", "?v=0")).toBeUndefined();
});

test("an artifact of an agent's conversation is referenced under its session", () => {
  const picture = {
    issue_key: null,
    kind: "image" as const,
    primary: false,
    project: "",
    session_id: "ses-1",
    slug: "shot-png",
  };
  expect(buildDispatchReference(documentRoute(picture))).toBe(
    "dispatch://agent/ses-1/artifact/shot-png"
  );
  expect(buildDispatchReference(documentRoute(picture, 3))).toBe(
    "dispatch://agent/ses-1/artifact/shot-png@v3"
  );
});

test("a picture is a Dispatch reference only when its source parses as one, captioned by its alt or the artifact's slug", () => {
  // Another website, a `dispatch://` string the grammar refuses, and a picture with no source at
  // all are no reference: the caller draws them as it draws any picture.
  expect(
    pictureReference({ alt: "shot.png", src: "https://example.test/shot.png" })
  ).toBeUndefined();
  expect(pictureReference({ src: "dispatch://not a reference" })).toBeUndefined();
  expect(pictureReference({})).toBeUndefined();

  // A reference to something other than an artifact names no artifact; one to an artifact without
  // a version names it unpinned. Both keep the source as written.
  expect(pictureReference({ src: "dispatch://CORE-1/ask/a1" })).toEqual({
    caption: "",
    named: undefined,
    route: { id: "a1", key: "CORE-1", kind: "ask" },
    source: "dispatch://CORE-1/ask/a1",
  });
  expect(pictureReference({ src: "dispatch://CORE-1/artifact/notes-md" })).toEqual({
    caption: "notes-md",
    named: { owner: { issue: "CORE-1" }, slug: "notes-md", version: undefined },
    route: { key: "CORE-1", kind: "artifact", slug: "notes-md" },
    source: "dispatch://CORE-1/artifact/notes-md",
  });

  // The caption is the trimmed alt, or the slug when the alt is blank.
  const pinned = {
    named: { owner: { issue: "CORE-1" }, slug: "shot-png", version: 1 },
    route: { key: "CORE-1", kind: "artifact" as const, slug: "shot-png", version: 1 },
    source: "dispatch://CORE-1/artifact/shot-png@v1",
  };
  expect(pictureReference({ alt: "shot.png", src: pinned.source })).toEqual({
    ...pinned,
    caption: "shot.png",
  });
  expect(pictureReference({ alt: "  ", src: pinned.source })).toEqual({
    ...pinned,
    caption: "shot-png",
  });
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

test("inbox assignee filters round-trip as repeated query parameters, after the other filters", () => {
  const route = { assignees: ["ana@example.com", "bo@example.com", "unassigned"] };
  const path = buildInboxPath(route);
  expect(path).toBe("/?assignee=ana%40example.com&assignee=bo%40example.com&assignee=unassigned");
  expect(parseInboxSearch(path.slice(1))).toEqual(route);
  expect(buildInboxPath({ agent: "s1", assignees: ["unassigned"], section: "needs-you" })).toBe(
    "/?agent=s1&section=needs-you&assignee=unassigned"
  );
  expect(parseInboxSearch("?agent=s1&section=needs-you&assignee=unassigned")).toEqual({
    agent: "s1",
    assignees: ["unassigned"],
    section: "needs-you",
  });
});

// `everyone` is how the cleared filter survives a reload: an absent `assignee` is the default,
// the reader's own work, so clearing the filter has to be written down.
test("the cleared inbox filter is `assignee=everyone` and an empty one leaves the field absent", () => {
  expect(buildInboxPath({ assignees: [] })).toBe("/?assignee=everyone");
  expect(parseInboxSearch("?assignee=everyone")).toEqual({ assignees: [] });
  expect(parseInboxSearch("?assignee=everyone&assignee=+")).toEqual({ assignees: [] });
  expect(parseInboxSearch("?assignee=everyone&assignee=ana%40example.com")).toEqual({
    assignees: ["ana@example.com"],
  });
  expect(parseInboxSearch("?assignee=")).toEqual({});
  expect(parseInboxSearch("?view=mine&section=needs-you")).toEqual({ section: "needs-you" });
});

test("inbox assignees fold case and spaces, drop duplicates, keep order, and keep unknowns", () => {
  expect(parseInboxSearch("?assignee=+Ana%40Example.com+&assignee=ana%40example.com")).toEqual({
    assignees: ["ana@example.com"],
  });
  expect(
    parseInboxSearch(
      "?assignee=bo%40example.com&assignee=ana%40example.com&assignee=bo%40example.com"
    )
  ).toEqual({ assignees: ["bo@example.com", "ana@example.com"] });
  // Nothing refuses an email the page does not list: the split is client-side, so it just
  // matches no row.
  expect(parseInboxSearch("?assignee=Nobody%40Example.com")).toEqual({
    assignees: ["nobody@example.com"],
  });
});

// The shell reserves its right gutter from this, and `features/margin/Margin.tsx` renders from
// it, so the two cannot disagree: an 80 px gutter beside a rail that is not there is exactly
// what two separate answers produced.
test("routeHasMargin is true for issue and project-document routes and false for the rest", () => {
  const withMargin = [
    "/issues/CORE-1",
    "/issues/CORE-1/conversation",
    "/issues/CORE-1/children",
    "/issues/CORE-1/artifacts",
    "/issues/CORE-1/artifacts/notes",
    "/projects/CORE/documents/design-language",
  ];
  const withoutMargin = [
    "/",
    "/agents",
    "/agents/session-1/live",
    "/settings",
    "/projects/CORE",
    "/projects/CORE/issues",
    "/projects/CORE/documents",
    "/projects/CORE/architecture",
    "/nope",
  ];
  for (const pathname of withMargin) {
    expect([pathname, routeHasMargin(pathname)]).toEqual([pathname, true]);
  }
  for (const pathname of withoutMargin) {
    expect([pathname, routeHasMargin(pathname)]).toEqual([pathname, false]);
  }
});
