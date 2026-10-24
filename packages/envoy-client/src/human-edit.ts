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
 * The diff a person's edit carries, as the fenced `diff` block that follows the card's TOON
 * block, then the cut note when Dispatch cut it; empty when the payload holds no diff. A fence
 * longer than any run of backticks in the diff keeps the block closed whatever the document holds.
 */
export function humanEditDiffBlock(diff: string | undefined, cut: boolean | undefined): string {
  if (diff === undefined || diff === "") return cut === true ? HUMAN_EDIT_DIFF_CUT_NOTE : "";
  const longestRun = Math.max(0, ...(diff.match(/`+/g) ?? []).map((run) => run.length));
  const fence = "`".repeat(Math.max(3, longestRun + 1));
  const body = diff.endsWith("\n") ? diff : `${diff}\n`;
  return `${fence}diff\n${body}${fence}${cut === true ? `\n${HUMAN_EDIT_DIFF_CUT_NOTE}` : ""}`;
}
