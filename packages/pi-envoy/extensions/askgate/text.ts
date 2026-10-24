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
