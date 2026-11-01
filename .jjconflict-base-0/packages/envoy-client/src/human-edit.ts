/** The fields of an `artifact.human_edited` payload its one-line head names. */
export interface HumanEditHeadFields {
  readonly name?: string | undefined;
  readonly from_version?: number | undefined;
  readonly to_version?: number | undefined;
  readonly editors?: readonly string[] | undefined;
}

/**
 * How a person's edit of a document reads as one line, in the Envoy card and in an issue's event
 * log alike: `spec.md v4 to v7, edited by person@example.com`, each editor named, in the order the
 * payload lists them.
 */
export function humanEditHead(edit: HumanEditHeadFields): string {
  const editors = edit.editors ?? [];
  const by = editors.length === 0 ? "" : `, edited by ${editors.join(", ")}`;
  return `${edit.name ?? "document"} v${edit.from_version ?? "?"} to v${edit.to_version ?? "?"}${by}`;
}

/** The note a card carries after a diff that Dispatch cut at its limit. */
export const HUMAN_EDIT_DIFF_CUT_NOTE = "(diff cut at 4,096 bytes)";

/**
 * The most of a diff a card shows, the same 4,096 bytes Dispatch cuts at (editNoticeDiffBytes).
 * Dispatch's own notices arrive within it; this bounds what a card renders whatever reaches the
 * client, since one oversized payload would otherwise fill a session's turn.
 */
export const HUMAN_EDIT_DIFF_MAX = 4096;

/**
 * The diff a person's edit carries, as the fenced `diff` block that follows the card's TOON
 * block, then the cut note when Dispatch cut it or this cut it; empty when the payload holds no
 * diff. A diff longer than HUMAN_EDIT_DIFF_MAX is cut at its last line end within that many
 * bytes, as Dispatch cuts its own (text.UnifiedDiffCut), and carries the note. A fence longer
 * than any run of backticks in the diff keeps the block closed whatever the document holds.
 */
export function humanEditDiffBlock(diff: string | undefined, cut: boolean | undefined): string {
  if (diff === undefined || diff === "") return cut === true ? HUMAN_EDIT_DIFF_CUT_NOTE : "";
  const bytes = new TextEncoder();
  let shown = diff;
  let note = cut === true;
  if (bytes.encode(shown).length > HUMAN_EDIT_DIFF_MAX) {
    let kept = "";
    let size = 0;
    for (const line of shown.split("\n").slice(0, -1)) {
      size += bytes.encode(`${line}\n`).length;
      if (size > HUMAN_EDIT_DIFF_MAX) break;
      kept += `${line}\n`;
    }
    shown = kept;
    note = true;
  }
  if (shown === "") return HUMAN_EDIT_DIFF_CUT_NOTE;
  const longestRun = Math.max(0, ...(shown.match(/`+/g) ?? []).map((run) => run.length));
  const fence = "`".repeat(Math.max(3, longestRun + 1));
  const body = shown.endsWith("\n") ? shown : `${shown}\n`;
  return `${fence}diff\n${body}${fence}${note ? `\n${HUMAN_EDIT_DIFF_CUT_NOTE}` : ""}`;
}
