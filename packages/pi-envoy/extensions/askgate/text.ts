// The text AskGate's two checks share: how untrusted text is escaped into a prompt, how any text
// is cut to a byte budget, how a prompt template is filled, how a model's answer gives up its last
// line of JSON, and the one transform for text handed back to the agent. A leaf module: it imports
// nothing of the gate, so `askgate-core.ts` and `rules-check.ts` both read from it.

/** Room kept for a clip marker (` … [elided N bytes]`, at most 32 bytes). */
const CLIP_MARKER_BYTES = 32;
/** How much of a reason the agent is handed, after the tag transform. */
export const GATE_REASON_MAX_BYTES = 2 * 1024;

export const byteLength = (text: string) => Buffer.byteLength(text, "utf8");

/** `text` cut to at most `max` UTF-8 bytes, marker included. */
export function clipBytes(text: string, max: number): string {
  const size = byteLength(text);
  if (size <= max) return text;
  // A cut inside a multi-byte character decodes to U+FFFD; drop it.
  const head = Buffer.from(text, "utf8")
    .subarray(0, Math.max(0, max - CLIP_MARKER_BYTES))
    .toString("utf8")
    .replace(/\uFFFD$/, "");
  return `${head} … [elided ${size - byteLength(head)} bytes]`;
}

/** The middle clip's marker; `N` becomes the bytes it stands for. */
const MIDDLE_MARKER = " … [elided N bytes] … ";
/** The longest entity escaping writes, `&quot;`: how far back a cut can find the `&` it splits. */
const ENTITY_MAX_BYTES = 6;

/**
 * Where the entity a cut before byte `at` of escaped text would split begins, or undefined when
 * the cut splits none. Escaping turns every `&` of the text into an entity, and every entity it
 * writes is an `&`, lowercase letters and a `;`, so an `&` followed only by lowercase letters up
 * to the cut is an entity left open by it.
 */
function splitEntity(bytes: Buffer, at: number): number | undefined {
  for (let index = at - 1; index >= Math.max(0, at - ENTITY_MAX_BYTES); index--) {
    if (bytes[index] === 0x26) return index;
    if (bytes[index] < 0x61 || bytes[index] > 0x7a) return undefined;
  }
  return undefined;
}

/** A cut at or before byte `end` of escaped text that splits no character and no entity. */
function headEnd(bytes: Buffer, end: number): number {
  let at = end;
  while (at > 0 && (bytes[at] & 0xc0) === 0x80) at--;
  return splitEntity(bytes, at) ?? at;
}

/**
 * Escaped `text` cut to its first `max` bytes, and back to the start of a character or an entity
 * the cut would split, so the head ends on neither a U+FFFD nor a bare `&amp`.
 */
export const escapedHead = (text: string, max: number) => {
  const bytes = Buffer.from(text, "utf8");
  return bytes.subarray(0, headEnd(bytes, Math.min(max, bytes.length))).toString("utf8");
};

/**
 * Escaped `text` cut to at most `max` UTF-8 bytes in its middle: a head and a tail of about equal
 * size around a marker naming the bytes elided. An ask's text is clipped this way, never by
 * `clipBytes`, because the rules check reads both its ends: `untried_access` reads the start,
 * where an ask makes its request of the person, and `no_change` the end, where the ask's own
 * recommendation comes. Neither cut splits a character or an entity: the head ends before one,
 * the tail starts after it, so a clip of escaped text can never leave a `&lt` a tag could use.
 */
export function clipMiddle(text: string, max: number): string {
  const bytes = Buffer.from(text, "utf8");
  if (bytes.length <= max) return text;
  // The marker's room, its count written at the most digits it can take.
  const marker = byteLength(MIDDLE_MARKER) - 1 + String(bytes.length).length;
  const keep = Math.max(0, max - marker);
  const end = headEnd(bytes, Math.floor(keep / 2));
  let start = bytes.length - Math.ceil(keep / 2);
  while (start < bytes.length && (bytes[start] & 0xc0) === 0x80) start++;
  // A tail starting inside an entity starts after its `;` instead.
  if (splitEntity(bytes, start) !== undefined) {
    const close = bytes.indexOf(0x3b, start);
    start = close === -1 ? bytes.length : close + 1;
  }
  const head = bytes.subarray(0, end).toString("utf8");
  const tail = bytes.subarray(start).toString("utf8");
  return `${head}${MIDDLE_MARKER.replace("N", String(start - end))}${tail}`;
}

export const escapeXml = (text: string) =>
  text.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");

/** Untrusted text escaped, then cut to `max` bytes: no text of the agent's can close or open a tag. */
export const escaped = (text: string, max = Number.POSITIVE_INFINITY) =>
  clipBytes(escapeXml(text), max);

/** `{{name}}` placeholders replaced in one pass, values pasted verbatim (never re-scanned, never read as `$` patterns); a placeholder with no value throws. */
export function renderTemplate(template: string, values: Record<string, string>): string {
  return template.replace(/\{\{(\w+)\}\}/g, (_, name: string) => {
    if (!Object.hasOwn(values, name))
      throw new Error(`AskGate prompt template names {{${name}}}, which has no value`);
    return values[name];
  });
}

/**
 * The JSON object on the answer's last line (a closing code fence after it is ignored), which both
 * checks answer with; undefined when that line is not JSON or is not an object.
 */
export function lastLineJson(text: string): Record<string, unknown> | undefined {
  const lines = text
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "");
  while (lines.length > 0 && lines[lines.length - 1].startsWith("```")) lines.pop();
  let parsed: unknown;
  try {
    parsed = JSON.parse(lines.at(-1) ?? "");
  } catch {
    return undefined;
  }
  return typeof parsed === "object" && parsed !== null && !Array.isArray(parsed)
    ? (parsed as Record<string, unknown>)
    : undefined;
}

/**
 * The one transform for text handed to the agent (a model's reason, the agent's own rebuttal): verbatim,
 * except that a `<` which could start a tag (before a letter, `/`, `!` or `?`) becomes `&lt;`, then capped
 * at 2 KiB. The two defects are disjoint. A frame closed or a harness tag opened needs a tag-opening `<`,
 * and this rule stops it. Full escaping also turned `&&`, `->` and a `> ` quote line into entities, so a fix
 * sent back verbatim posted `&gt;` and a check for the quote failed; those now pass through unchanged. Text
 * rendered into the gate's own prompt is escaped in full instead (`escaped`). Nothing between here and
 * delivery decodes entities: warn joins the text into a developer message as-is, block's Error message
 * becomes the tool result as-is.
 */
export const agentFacing = (text: string) =>
  clipBytes(text.replace(/<(?=[A-Za-z/!?])/g, "&lt;"), GATE_REASON_MAX_BYTES);
