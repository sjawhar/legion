import { hasControlCharacter } from "./dispatch-href";
import { referenceTargetFits, utf8Bytes } from "./dispatch-text-rules";
import { PROJECT_KEY_PATTERN } from "./dispatch-tools";

const encoder = new TextEncoder();

/**
 * The `#b-` fragment of a block reference: every UTF-8 byte of the block id percent-encoded in
 * upper-case hex except ASCII letters, digits and `-`, so no character a reference ends at or the
 * grammar trims (`'`, `.`, `!`, `*`, `_`, `~`, `)`), and no `@` a pin (`@v<N>` after the
 * fragment) could be read from, is ever written bare. Readers accept any percent-encoding; every
 * writer writes this one. Go's twin is `text.EncodeBlockID`; both are tested against
 * DISPATCH_BLOCK_ID_ENCODINGS, whose JSON copy is
 * `packages/envoy/internal/dispatch/text/testdata/dispatch-block-id-encodings.json`. Each code
 * point is encoded on its own: Bun 1.3's `encode` writes a non-ASCII Latin-1 character of a string
 * past 1024 characters as a NUL, and an id has no length bound.
 */
export function encodeBlockId(id: string): string {
  let encoded = "";
  for (const character of id) {
    for (const byte of encoder.encode(character)) {
      const bare =
        (byte >= 0x30 && byte <= 0x39) ||
        (byte >= 0x41 && byte <= 0x5a) ||
        (byte >= 0x61 && byte <= 0x7a) ||
        byte === 0x2d;
      encoded += bare
        ? String.fromCharCode(byte)
        : `%${byte.toString(16).toUpperCase().padStart(2, "0")}`;
    }
  }
  return encoded;
}

/**
 * The most UTF-8 bytes (`utf8Bytes`) a block id, decoded, may hold for text to cite its block. The
 * reference graph keys a block by its id in Postgres B-tree keys, which hold at most 2704 bytes,
 * so the server makes no graph node of a block carrying a longer one, and text naming one cites
 * nothing: a reader of text (the dashboard's composer and links, as the server's `text.Extract`)
 * holds a block id to it. It decides only what the graph keys: such a block is written and read as
 * any other, and a request naming it, which `decodeBlockId` and `parseBlockFragment` read whatever
 * its length, finds no node. Go's twin is `text.MaxBlockIDBytes`.
 */
export const MAX_BLOCK_ID_BYTES = 256;

/**
 * The block id a `#b-` fragment (`encoded`, after the `b-`) names, of any length, or undefined when
 * it names none: empty, holding a raw `@` (a pin is no part of the id; `encodeBlockId` writes `@`
 * as `%40`), not decoding, or decoding to an empty id or one holding a control character. Any
 * valid percent-encoding is read, as the server's `text` package reads it. A reader of text holds
 * the id to MAX_BLOCK_ID_BYTES as well.
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
  return id === "" || hasControlCharacter(id) ? undefined : id;
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

/**
 * A block reference as `parseCanonicalBlockReference` reads it: the block's document - `issue`
 * and the document's slug `document` (`spec` for the issue's primary document), or `project` and
 * its document's slug - the block id `block`, decoded, and the pinned version's decimal digits
 * `pin`, when the reference is pinned.
 */
export interface DispatchBlockReference {
  readonly issue?: string;
  readonly project?: string;
  readonly document: string;
  readonly block: string;
  readonly pin?: string;
}

// Go's `text` grammars: a project key (PROJECT_KEY_PATTERN, unanchored), an issue key and a
// document slug.
const PROJECT_KEY = PROJECT_KEY_PATTERN.source.slice(1, -1);
const ISSUE_KEY = `${PROJECT_KEY}-[1-9][0-9]*`;
const DOCUMENT_SLUG = "[a-z0-9]+(?:-[a-z0-9]+)*";
const ISSUE_KEY_ONLY = new RegExp(`^${ISSUE_KEY}$`);
const BLOCK_REFERENCE = new RegExp(
  `^dispatch://(?:(${ISSUE_KEY})/spec|(${ISSUE_KEY}|${PROJECT_KEY})/artifact/(${DOCUMENT_SLUG}))#b-(.*)$`,
  "s"
);

/**
 * A block reference as every writer writes one: `dispatch://<ISSUE>/spec` for a block of an
 * issue's primary document, `dispatch://<ISSUE or PROJECT>/artifact/<slug>` for any other document
 * of an issue or a project (a project's document whose slug is `spec` included), then `#b-` and
 * the id as `encodeBlockId` writes it, then `@v<pin>` when it is pinned. Go's twin is
 * `text.FormatBlockRef`.
 */
export function formatBlockReference(reference: DispatchBlockReference): string {
  const document =
    reference.issue !== undefined && reference.document === "spec"
      ? `${reference.issue}/spec`
      : `${reference.issue ?? reference.project}/artifact/${reference.document}`;
  const pin = reference.pin === undefined ? "" : `@v${reference.pin}`;
  return `dispatch://${document}#b-${encodeBlockId(reference.block)}${pin}`;
}

/**
 * The block `value` names when it is one block reference written exactly as
 * `formatBlockReference` writes it - the one way a typed block's `block[]` attribute holds a
 * pointer - or undefined. Its document part is an issue's primary document
 * (`dispatch://<ISSUE>/spec`) or a document of an issue or a project
 * (`dispatch://<ISSUE or PROJECT>/artifact/<slug>`), its fragment one `parseBlockFragment` reads,
 * its id at most MAX_BLOCK_ID_BYTES and its target id at most MAX_REFERENCE_TARGET_BYTES, as text
 * cites a block; and `formatBlockReference` must write it back as `value`, which refuses what a
 * reader alone accepts: lower-case or needless percent-encoding, and an issue's primary document
 * spelled `artifact/spec`. Go's twin is `text.CanonicalBlockRef`.
 */
export function parseCanonicalBlockReference(value: string): DispatchBlockReference | undefined {
  const match = BLOCK_REFERENCE.exec(value);
  if (match === null) {
    return undefined;
  }
  const [, specIssue, owner, slug, fragment] = match;
  const parsed = parseBlockFragment(fragment ?? "");
  if (parsed === undefined || utf8Bytes(parsed.block) > MAX_BLOCK_ID_BYTES) {
    return undefined;
  }
  const key = specIssue ?? owner ?? "";
  const document = specIssue === undefined ? (slug ?? "") : "spec";
  if (!referenceTargetFits(`${key}/${document}#${parsed.block}`)) {
    return undefined;
  }
  const reference: DispatchBlockReference = {
    ...(ISSUE_KEY_ONLY.test(key) ? { issue: key } : { project: key }),
    document,
    ...parsed,
  };
  return formatBlockReference(reference) === value ? reference : undefined;
}

/**
 * The pointers a typed block's `block[]` attribute holds, as markdown writes it: the empty string
 * for none, else canonical block references (`parseCanonicalBlockReference`) joined by exactly one
 * space, with none leading or trailing and none repeated. Undefined for any other value, which is
 * refused, never normalised, so the editor and the server never disagree on one
 * (`blockPointerValueProblem` says why). Go's twin is `pmdoc.ParseBlockPointerValue`; both are
 * tested against DISPATCH_BLOCK_POINTER_VALUES, whose JSON copy is
 * `packages/envoy/internal/dispatch/pmdoc/testdata/block-pointer-values.json`.
 */
export function parseBlockPointerValue(value: string): string[] | undefined {
  if (blockPointerValueProblem(value) !== undefined) {
    return undefined;
  }
  return value === "" ? [] : value.split(" ");
}

/**
 * Why `value` is not a `block[]` attribute as markdown writes it (`parseBlockPointerValue`),
 * following the wording of Go's `pmdoc.ParseBlockPointerValue` (the value quoted with
 * `JSON.stringify` where Go uses `%q`, which escapes a control or non-printing character
 * differently), or undefined when it is one.
 */
export function blockPointerValueProblem(value: string): string | undefined {
  if (value === "") {
    return undefined;
  }
  if (value.startsWith(" ") || value.endsWith(" ") || value.includes("  ")) {
    return `${JSON.stringify(value)} must be block references separated by exactly one space, with none leading or trailing`;
  }
  return blockPointersProblem(value.split(" "));
}

/**
 * Why `values` are not the pointers of a `block[]` attribute - one is not a canonical block
 * reference (`parseCanonicalBlockReference`), or one is named twice - following the wording of
 * Go's `pmdoc.validateBlockPointerValues`, quoted as above, or undefined when they are.
 */
export function blockPointersProblem(values: readonly string[]): string | undefined {
  const seen = new Set<string>();
  for (const value of values) {
    if (parseCanonicalBlockReference(value) === undefined) {
      return `${JSON.stringify(value)} is not a block reference as Dispatch writes one: dispatch://<ISSUE>/spec#b-<id>, or dispatch://<ISSUE or PROJECT>/artifact/<slug>#b-<id>, the id percent-encoded in upper-case hex except ASCII letters, digits and -, optionally pinned @v<N>`;
    }
    if (seen.has(value)) {
      return `names ${JSON.stringify(value)} twice`;
    }
    seen.add(value);
  }
  return undefined;
}

const atIdBound = `dispatch://CORE-1/spec#b-${"x".repeat(MAX_BLOCK_ID_BYTES)}`;

/** Every value a `block[]` attribute may hold (`ok`) or not, and why. */
export const DISPATCH_BLOCK_POINTER_VALUES: readonly { value: string; ok: boolean; why: string }[] =
  [
    {
      value: "dispatch://CORE-1/spec#b-x",
      ok: true,
      why: "a block of an issue's primary document",
    },
    {
      value: "dispatch://CORE-1/artifact/notes-md#b-x",
      ok: true,
      why: "a block of an issue's document",
    },
    {
      value: "dispatch://CORE/artifact/handbook-md#b-x",
      ok: true,
      why: "a block of a project's document",
    },
    {
      value: "dispatch://CORE/artifact/spec#b-x",
      ok: true,
      why: "a project's document whose slug is spec",
    },
    { value: "dispatch://CORE-1/spec#b-d%C3%A9cision%3A1", ok: true, why: "a Unicode id" },
    { value: "dispatch://CORE-1/spec#b-x@v2", ok: true, why: "a pinned block" },
    { value: "dispatch://CORE-1/spec#b-x@v9223372036854775807", ok: true, why: "the largest pin" },
    {
      value: "dispatch://CORE-1/spec#b-x dispatch://CORE/artifact/handbook-md#b-y",
      ok: true,
      why: "two references separated by one space",
    },
    { value: "", ok: true, why: "no pointers" },
    { value: atIdBound, ok: true, why: "an id of exactly 256 bytes" },
    {
      value: "dispatch://CORE-1/artifact/spec#b-x",
      ok: false,
      why: "an issue's primary document is written /spec",
    },
    { value: "dispatch://CORE-1/spec", ok: false, why: "a document, not a block" },
    {
      value: "dispatch://agent/s1/artifact/x#b-y",
      ok: false,
      why: "an agent's document has no blocks to point at",
    },
    { value: "dispatch://CORE-1/spec#b-d%c3%a9", ok: false, why: "lower-case hex" },
    { value: "dispatch://CORE-1/spec#b-%41", ok: false, why: "an over-encoded ASCII letter" },
    { value: "dispatch://CORE-1/spec#b-x%2Dy", ok: false, why: "an over-encoded -" },
    { value: "dispatch://CORE-1/spec#b-%FF", ok: false, why: "invalid UTF-8" },
    { value: "dispatch://CORE-1/spec#b-%0A", ok: false, why: "a control character" },
    { value: "dispatch://CORE-1/spec#x", ok: false, why: "a fragment that is not b-" },
    { value: "dispatch://CORE-1/spec#c-x", ok: false, why: "a fragment that is not b-" },
    { value: "dispatch://CORE-1/spec#b-", ok: false, why: "an empty id" },
    { value: `${atIdBound}x`, ok: false, why: "an id over 256 bytes" },
    { value: "dispatch://CORE-1/artifact/notes-md@v2#b-x", ok: false, why: "a versioned document" },
    { value: "dispatch://CORE-1#b-x", ok: false, why: "an issue names no document" },
    {
      value: "dispatch://CORE-1/spec#b-x@v9223372036854775808",
      ok: false,
      why: "a pin past the largest",
    },
    { value: "dispatch://CORE-1/spec#b-x@v01", ok: false, why: "a pin with a leading zero" },
    { value: "dispatch://CORE-1/spec#b-x@v0", ok: false, why: "a pin of zero" },
    {
      value: "https://dispatch.example/issues/CORE-1/spec#b-x",
      ok: false,
      why: "a dashboard URL of a block",
    },
    { value: "dispatch://CORE-1/spec#b-x.", ok: false, why: "trailing punctuation" },
    { value: "dispatch://CORE-1/spec#b-x)", ok: false, why: "a trailing parenthesis" },
    {
      value: "dispatch://CORE-1/spec#b-x,dispatch://CORE-1/spec#b-y",
      ok: false,
      why: "references joined by a comma",
    },
    {
      value: "dispatch://CORE-1/spec#b-x dispatch://CORE-1/spec#b-x",
      ok: false,
      why: "a repeated reference",
    },
    {
      value: "dispatch://CORE-1/spec#b-x  dispatch://CORE-1/spec#b-y",
      ok: false,
      why: "a doubled space",
    },
    { value: " dispatch://CORE-1/spec#b-x", ok: false, why: "a leading space" },
    { value: "dispatch://CORE-1/spec#b-x ", ok: false, why: "a trailing space" },
    {
      value: "dispatch://CORE-1/spec#b-x\tdispatch://CORE-1/spec#b-y",
      ok: false,
      why: "a tab between references",
    },
    {
      value: "dispatch://CORE-1/spec#b-x\uFEFFdispatch://CORE-1/spec#b-y",
      ok: false,
      why: "U+FEFF between references",
    },
  ];
