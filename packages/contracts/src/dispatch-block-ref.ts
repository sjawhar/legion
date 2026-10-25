/**
 * The `#b-` fragment of a block reference: every UTF-8 byte of the block id percent-encoded in
 * upper-case hex except ASCII letters, digits and `-`, so no character a reference ends at or the
 * grammar trims (`'`, `.`, `!`, `*`, `_`, `~`, `)`), and no `@` a pin (`@v<N>` after the
 * fragment) could be read from, is ever written bare. Readers accept any percent-encoding; every
 * writer writes this one. Go's twin is `text.EncodeBlockID`; both are tested against
 * DISPATCH_BLOCK_ID_ENCODINGS, whose JSON copy is
 * `packages/envoy/internal/dispatch/text/testdata/dispatch-block-id-encodings.json`.
 */
export function encodeBlockId(id: string): string {
  let encoded = "";
  for (const byte of new TextEncoder().encode(id)) {
    const bare =
      (byte >= 0x30 && byte <= 0x39) ||
      (byte >= 0x41 && byte <= 0x5a) ||
      (byte >= 0x61 && byte <= 0x7a) ||
      byte === 0x2d;
    encoded += bare
      ? String.fromCharCode(byte)
      : `%${byte.toString(16).toUpperCase().padStart(2, "0")}`;
  }
  return encoded;
}

export const DISPATCH_BLOCK_ID_ENCODINGS: readonly { id: string; encoded: string }[] = [
  { id: "ship-it", encoded: "ship-it" },
  { id: "q:1", encoded: "q%3A1" },
  { id: "décision:1", encoded: "d%C3%A9cision%3A1" },
  { id: "it's", encoded: "it%27s" },
  { id: "a.b_c~d!e*f(g)", encoded: "a%2Eb%5Fc%7Ed%21e%2Af%28g%29" },
  { id: "x y#z%", encoded: "x%20y%23z%25" },
  { id: "it@v2", encoded: "it%40v2" },
  { id: "📊", encoded: "%F0%9F%93%8A" },
];
