import { expect, test } from "bun:test";

import { embedHighlight } from "./highlight";

test("embedHighlight wraps a unique quote in a proof comment span and refuses ambiguity", () => {
  expect(
    embedHighlight("The quick brown fox", { by: "user:alice", id: "c-1", quote: "brown" })
  ).toBe(
    'The quick <span data-proof="comment" data-id="c-1" data-by="user:alice">brown</span> fox'
  );
  expect(embedHighlight("o o", { by: "u", id: "c", quote: "o" })).toBeUndefined();
  expect(embedHighlight("text", { by: "u", id: "c", quote: "gone" })).toBeUndefined();
  expect(embedHighlight("x", { by: 'a"b', id: "c", quote: "x" })).toBe(
    '<span data-proof="comment" data-id="c" data-by="a&quot;b">x</span>'
  );
});
