import { expect, test } from "bun:test";

import { dispatchRefFromUrl } from "../dispatch-execute";
import { type ParsedDispatchRef, parseDispatchRef } from "../dispatch-owner";

test("a block of an issue's spec names the block, decoded, and the document it is in", () => {
  expect(parseDispatchRef("dispatch://CORE-1/spec#b-d%C3%A9cision%3A1")).toEqual({
    owner: { kind: "issue", issue: "CORE-1" },
    kind: "block",
    id: "spec",
    block: "décision:1",
    document: "dispatch://CORE-1/spec",
  });
});

test("a block of an issue's document and of a project's document", () => {
  expect(parseDispatchRef("dispatch://CORE-1/artifact/notes-md#b-ship%5Fit")).toEqual({
    owner: { kind: "issue", issue: "CORE-1" },
    kind: "block",
    id: "notes-md",
    block: "ship_it",
    document: "dispatch://CORE-1/artifact/notes-md",
  });
  expect(parseDispatchRef("dispatch://CORE/artifact/handbook-md#b-x")).toEqual({
    owner: { kind: "project", project: "CORE" },
    kind: "block",
    id: "handbook-md",
    artifact: "handbook-md",
    block: "x",
    document: "dispatch://CORE/artifact/handbook-md",
  });
});

test("a block pinned to a version of its document carries the pin; an encoded @ is the id's", () => {
  expect(parseDispatchRef("dispatch://CORE-1/spec#b-ship-it@v2")).toEqual({
    owner: { kind: "issue", issue: "CORE-1" },
    kind: "block",
    id: "spec",
    block: "ship-it",
    document: "dispatch://CORE-1/spec",
    pin: "2",
  });
  expect(parseDispatchRef("dispatch://CORE/artifact/handbook-md#b-d%C3%A9cision%3A1@v12")).toEqual({
    owner: { kind: "project", project: "CORE" },
    kind: "block",
    id: "handbook-md",
    artifact: "handbook-md",
    block: "décision:1",
    document: "dispatch://CORE/artifact/handbook-md",
    pin: "12",
  });
  expect(parseDispatchRef("dispatch://CORE-1/spec#b-it%40v2")).toEqual({
    owner: { kind: "issue", issue: "CORE-1" },
    kind: "block",
    id: "spec",
    block: "it@v2",
    document: "dispatch://CORE-1/spec",
  });
});

test.each([
  // No id, or one that does not decode or decodes to a control character.
  "dispatch://CORE-1/spec#b-",
  "dispatch://CORE-1/spec#b-%ZZ",
  "dispatch://CORE-1/spec#b-%00",
  "dispatch://CORE-1/spec#b-@v2",
  // A raw @ in the fragment that is no well-formed pin.
  "dispatch://CORE-1/spec#b-ship-it@v",
  "dispatch://CORE-1/spec#b-ship-it@v0",
  "dispatch://CORE-1/spec#b-ship-it@v01",
  "dispatch://CORE-1/spec#b-ship-it@vx",
  "dispatch://CORE-1/spec#b-a@b",
  "dispatch://CORE-1/spec#b-ship-it@v2@v3",
  // A pin past the largest the server's Go `int` holds.
  "dispatch://CORE-1/spec#b-ship-it@v9223372036854775808",
  // A versioned document: a version's stored text holds no block ids.
  "dispatch://CORE-1/artifact/spec@v2#b-x",
  "dispatch://CORE/artifact/handbook-md@v2#b-x",
  // Nothing but a document holds blocks.
  "dispatch://CORE-1#b-x",
  "dispatch://CORE-1/log#b-x",
  "dispatch://CORE-1/comment/cccccccc-0000-4000-8000-000000000042#b-x",
  "dispatch://CORE/artifact/handbook-md/ask/aaaaaaaa-0000-4000-8000-000000000042#b-x",
])("%s names nothing", (ref) => {
  expect(parseDispatchRef(ref)).toBeNull();
});

// `dispatch read --ref` is a read request, so a reference names its target whatever the length of
// its target id: the server's bound decides only what text indexes. A document whose slug is 2683
// characters, which a new upload's reference key still holds, parses, and so does a block of it,
// for an issue's document and a project's alike.
test.each([
  ["CORE-1", { kind: "issue", issue: "CORE-1" }],
  ["CORE", { kind: "project", project: "CORE" }],
] as const)("a long-named %s document and its block parse", (owner, parsedOwner) => {
  const slug = "a".repeat(2683);
  const document = `dispatch://${owner}/artifact/${slug}`;
  const project = parsedOwner.kind === "project" ? { artifact: slug } : {};
  expect(parseDispatchRef(document)).toEqual({ owner: parsedOwner, kind: "artifact", id: slug });
  expect(parseDispatchRef(`${document}#b-%C3%A9`)).toEqual({
    owner: parsedOwner,
    kind: "block",
    id: slug,
    block: "é",
    document,
    ...project,
  });
});

// A block id past the 256 bytes the server's graph keys a block by is read too: the bound decides
// only what text cites, and the server answers a read of such a block with the 404 any block it
// holds no node for gets. Its page's link is the same reference.
test("a block id of any length parses, pinned or not, from its reference and its page", () => {
  const id = `${"a".repeat(3200)}é`;
  const reference = `dispatch://CORE-1/spec#b-${"a".repeat(3200)}%C3%A9`;
  const block: ParsedDispatchRef = {
    owner: { kind: "issue", issue: "CORE-1" },
    kind: "block",
    id: "spec",
    block: id,
    document: "dispatch://CORE-1/spec",
  };
  expect(parseDispatchRef(reference)).toEqual(block);
  expect(parseDispatchRef(`${reference}@v2`)).toEqual({ ...block, pin: "2" });
  const server = "http://dispatch.test";
  expect(
    dispatchRefFromUrl(`${server}/issues/CORE-1/spec#b-${"a".repeat(3200)}%c3%a9`, server)
  ).toBe(reference);
});

// So does an issue whose number is long, and an item whose id is.
test.each([
  ["an issue", "dispatch://CORE-1", "0"],
  ["an issue's comment", "dispatch://CORE-1/comment/", "a"],
  ["an issue's message", "dispatch://CORE-1/message/", "a"],
  ["an ask on a project's document", "dispatch://CORE/artifact/plan-md/ask/", "a"],
  ["an item id counted decoded", "dispatch://CORE-1/comment/", "%C3%A9"],
] as const)("%s of any length parses", (_name, prefix, character) => {
  expect(parseDispatchRef(`${prefix}${character.repeat(3000)}`)).not.toBeNull();
});
