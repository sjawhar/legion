import { hasControlCharacter } from "./dispatch-href";

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

/**
 * The most UTF-8 bytes a block id, decoded, may hold. The reference graph keys a block by its id
 * in Postgres B-tree keys, which hold at most 2704 bytes, so a reference naming a longer id names
 * nothing, the server makes no graph node of a block carrying one, and a write through the API
 * naming one is refused. Go's twin is `text.MaxBlockIDBytes`.
 */
export const MAX_BLOCK_ID_BYTES = 256;

/**
 * The block id a `#b-` fragment (`encoded`, after the `b-`) names, or undefined when it names
 * none: empty, holding a raw `@` (a pin is no part of the id; `encodeBlockId` writes `@` as `%40`),
 * not decoding, or decoding to an empty id, one holding a control character, or one longer than
 * MAX_BLOCK_ID_BYTES. Any valid percent-encoding is read, as the server's `text` package reads it.
 */
export function decodeBlockId(encoded: string): string | undefined {
  if (encoded === "" || encoded.includes("@")) {
    return undefined;
  }
  let id: string;
  try {
    id = decodeURIComponent(encoded);
  } catch {
    return undefined;
  }
  return id === "" ||
    hasControlCharacter(id) ||
    new TextEncoder().encode(id).length > MAX_BLOCK_ID_BYTES
    ? undefined
    : id;
}

/**
 * The largest version a block reference's pin (`@v<N>`) may name: Go's `int` on the 64-bit
 * platforms the server runs on, which `text.parseBlock` reads N into. A pin past it names nothing.
 */
const MAX_BLOCK_PIN = 9223372036854775807n;

/**
 * The block a `#b-` fragment (`encoded`, after the `b-`) names, and the version `pin` it pins when
 * it ends in `@v<N>`, as the server's `text.parseBlock` reads it: a raw `@` only as the start of
 * a well-formed pin (`N` a positive integer with no leading zero, at most MAX_BLOCK_PIN), and the
 * id before it as `decodeBlockId` reads it. Undefined for any other fragment. `pin` is N's decimal
 * digits as written, so a pin past the integers a JavaScript number holds exactly is carried and
 * written back unrounded.
 */
export function parseBlockFragment(encoded: string): { block: string; pin?: string } | undefined {
  const at = encoded.indexOf("@");
  const pin = at === -1 ? undefined : encoded.slice(at + 1).match(/^v([1-9][0-9]*)$/)?.[1];
  if (at !== -1 && (pin === undefined || BigInt(pin) > MAX_BLOCK_PIN)) {
    return undefined;
  }
  const block = decodeBlockId(at === -1 ? encoded : encoded.slice(0, at));
  if (block === undefined) {
    return undefined;
  }
  return pin === undefined ? { block } : { block, pin };
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
