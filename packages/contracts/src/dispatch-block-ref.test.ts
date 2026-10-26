import { expect, test } from "bun:test";

import {
  DISPATCH_BLOCK_ID_ENCODINGS,
  decodeBlockId,
  encodeBlockId,
  MAX_BLOCK_ID_BYTES,
  parseBlockFragment,
} from "./dispatch-block-ref";

// Ids at the bound and one byte past it, in UTF-8 bytes: an `é` is two, so the multi-byte pair
// is one a count of UTF-16 code units would take the wrong way.
const atBound = "x".repeat(MAX_BLOCK_ID_BYTES);
const pastBound = `${atBound}x`;
const multiByteAtBound = "é".repeat(MAX_BLOCK_ID_BYTES / 2);
const multiBytePastBound = `${multiByteAtBound}x`;

test.each([...DISPATCH_BLOCK_ID_ENCODINGS])("encodes block id %p", (row) => {
  expect(encodeBlockId(row.id)).toBe(row.encoded);
});

test("an encoded id decodes back to the id", () => {
  for (const row of DISPATCH_BLOCK_ID_ENCODINGS) {
    expect(decodeBlockId(encodeBlockId(row.id))).toBe(row.id);
  }
});

// No id: a raw `@` (`encodeBlockId` writes `@` as `%40`, so one is only ever a pin), an empty
// fragment, a percent-encoding that is not UTF-8, a control character, and an id longer than
// MAX_BLOCK_ID_BYTES.
test.each([
  ["ship-it@v2", undefined],
  ["", undefined],
  ["%E0%A4", undefined],
  ["%0A", undefined],
  ["%00", undefined],
  [encodeBlockId(atBound), atBound],
  [encodeBlockId(pastBound), undefined],
  [encodeBlockId(multiByteAtBound), multiByteAtBound],
  [encodeBlockId(multiBytePastBound), undefined],
])("decodes block id %p", (encoded, want) => {
  expect(decodeBlockId(encoded)).toBe(want);
});

// The `#b-` fragments of DISPATCH_TEXT_REFERENCES' block references, after the `b-`.
test.each([
  ["ship-it", { block: "ship-it" }],
  ["d%C3%A9cision%3A1", { block: "décision:1" }],
  ["d%c3%a9", { block: "dé" }],
  ["it%40v2", { block: "it@v2" }],
  ["ship-it@v2", { block: "ship-it", pin: 2 }],
  ["d%C3%A9cision%3A1@v12", { block: "décision:1", pin: 12 }],
  ["", undefined],
  ["%E0%A4", undefined],
  ["%0A", undefined],
  ["@v2", undefined],
  ["ship-it@v", undefined],
  ["ship-it@v0", undefined],
  ["ship-it@v01", undefined],
  ["ship-it@vx", undefined],
  ["a@b", undefined],
  ["a@v1@v2", undefined],
  [`${encodeBlockId(multiByteAtBound)}@v2`, { block: multiByteAtBound, pin: 2 }],
  [`${encodeBlockId(multiBytePastBound)}@v2`, undefined],
])("parses block fragment %p", (fragment, want) => {
  expect(parseBlockFragment(fragment)).toEqual(want);
});

test("the Go encoder's table is the same table", async () => {
  // `packages/envoy/internal/dispatch/text` cannot import TypeScript, so it walks a JSON copy.
  // Regenerate it from `packages/contracts` rather than editing it:
  //   bun -e 'import { DISPATCH_BLOCK_ID_ENCODINGS as t } from "./src/dispatch-block-ref.ts";
  //           await Bun.write("../envoy/internal/dispatch/text/testdata/dispatch-block-id-encodings.json",
  //             JSON.stringify(t, null, 2) + "\n")'
  const path = new URL(
    "../../envoy/internal/dispatch/text/testdata/dispatch-block-id-encodings.json",
    import.meta.url
  );
  expect(JSON.parse(await Bun.file(path).text())).toEqual([...DISPATCH_BLOCK_ID_ENCODINGS]);
});
