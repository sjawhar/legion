import {
  compareCauses,
  describeCause,
  identityLine,
  type Report,
  type Verdict,
} from "./classes.ts";
import type { Pair } from "./inputs.ts";

// The comment delivery audit's two outputs (#2366): the Markdown document and the TSV file of
// its pairs. Both are deterministic: the same report gives the same bytes.

function cell(value: string): string {
  return value === ""
    ? ""
    : value.replaceAll("\\", "\\\\").replaceAll("|", "\\|").replace(/\r?\n/g, " ");
}

/** A value as a CommonMark code span, so that a Markdown editor reads no link, e-mail address or
 *  emphasis in it. The fence is the shortest run of backticks that is not a run in the value. A
 *  space goes inside each fence when the value starts or ends with a backtick, or starts and ends
 *  with a space: CommonMark takes one such space off each end. A line ending is a space, as in any
 *  code span. Empty for an empty value. */
function code(value: string): string {
  if (value === "") return "";
  const text = value.replace(/\r\n?|\n/g, " ");
  const runs = new Set([...text.matchAll(/`+/g)].map((match) => match[0].length));
  let length = 1;
  while (runs.has(length)) length += 1;
  const marks = "`".repeat(length);
  const padded =
    /^`|`$/.test(text) || (text.startsWith(" ") && text.endsWith(" ") && /[^ ]/.test(text));
  const pad = padded ? " " : "";
  return `${marks}${pad}${text}${pad}${marks}`;
}

/** A value as a code span in a table cell: each `|` as `\|`, which GFM reads as `|` also inside a
 *  code span. (A backslash of the value just before a `|` cannot be kept.) */
function cellCode(value: string): string {
  return code(value).replaceAll("|", "\\|");
}

/** An id as the document shows it: bare when it is only hex digits and dashes, as a uuid is, and
 *  so cannot be read as a link; else as `span` writes it. */
function idText(value: string, span: (value: string) => string): string {
  return /^[0-9A-Fa-f-]+$/.test(value) ? value : span(value);
}

/** The class, with the evidence of a delivered verdict or the why of an unknown one as `span`
 *  writes it: as it is for the pairs file, a code span for the document. */
function verdictText(verdict: Verdict, span = (value: string) => value): string {
  if (verdict.class === "delivered") {
    return verdict.evidence === undefined ? "delivered" : `delivered (${span(verdict.evidence)})`;
  }
  if (verdict.class === "unknown") {
    return verdict.why === undefined ? "unknown" : `unknown (${span(verdict.why)})`;
  }
  return "not delivered";
}

function fence(text: string): string {
  const longest = Math.max(0, ...[...text.matchAll(/`+/g)].map((match) => match[0].length));
  const marks = "`".repeat(Math.max(3, longest + 1));
  return `${marks}text\n${text}${text.endsWith("\n") || text === "" ? "" : "\n"}${marks}`;
}

/** `<n> <mode> <envelope id or "no envelope">` for each attempt, then the newest attempt's state
 *  and error. Empty when no mention named the target. */
function mentionText(pair: Pair): string {
  if (pair.mentionModes.length === 0) return "";
  const each = pair.mentionModes.map(
    (mode, index) => `${index + 1} ${mode} ${pair.mentionEnvelopeIds[index] || "no envelope"}`
  );
  const error = pair.mentionError === "" ? "" : `: ${pair.mentionError}`;
  return `${each.join(", ")}; newest ${pair.mentionState}${error}`;
}

/** Why the pair was owed the comment, for the document. */
function owedText(pair: Pair): string {
  return pair.reasons.length === 0 ? "not owed, only sent" : pair.reasons.join(", ");
}

/** The columns of the pairs file, in order. */
const PAIR_COLUMNS = [
  "comment_id",
  "target",
  "owed_because",
  "sent_by_the_outbox",
  "mention_attempts",
  "checked_session",
  "status",
  "class",
  "cause",
] as const;

/** A value as one TSV field: a backslash, a tab, a carriage return and a newline as `\\`, `\t`,
 *  `\r` and `\n`. */
function tsvField(value: string): string {
  return value.replace(
    /[\\\t\r\n]/g,
    (char) => ({ "\\": "\\\\", "\t": "\\t", "\r": "\\r", "\n": "\\n" })[char] ?? char
  );
}

/** The pairs file: a header, then one row for each pair in the order of the comments, and a
 *  comment with no pair as one row with an empty target. Deterministic: the same files give the
 *  same bytes. */
export function renderPairs(report: Report): { readonly text: string; readonly rows: number } {
  const rows: (readonly string[])[] = [];
  for (const judged of report.comments) {
    const id = judged.comment.id;
    if (judged.pairs.length === 0) {
      rows.push([id, "", "", "", "", "", "", verdictText(judged.verdict), judged.verdict.cause]);
      continue;
    }
    for (const { pair, verdict } of judged.pairs) {
      rows.push([
        id,
        pair.target,
        owedText(pair),
        pair.addressed ? "yes" : "no",
        mentionText(pair),
        pair.receipt.checkedSession,
        pair.receipt.status,
        verdictText(verdict),
        verdict.cause,
      ]);
    }
  }
  const lines = [PAIR_COLUMNS, ...rows].map((fields) => fields.map(tsvField).join("\t"));
  return { text: `${lines.join("\n")}\n`, rows: rows.length };
}

/** A file named on the command line: its name and its text. */
export interface NamedText {
  readonly name: string;
  readonly text: string;
}

/** The pairs file as the document names it: its base name and its number of rows. */
interface PairsFile {
  readonly name: string;
  readonly rows: number;
}

/** The markdown document. Deterministic: the same files give the same bytes. */
export function renderReport(
  report: Report,
  findings: readonly NamedText[],
  traces: readonly NamedText[],
  pairs: PairsFile
): string {
  const { commentCounts: counts, pairCounts, receiptCounts: tq } = report;
  const out: string[] = [];
  out.push("# Did people's Dispatch comments reach the agent sessions?", "");
  out.push(identityLine(counts), "");
  out.push(
    `- Window: comments that people created from ${report.window.from} (included) to ` +
      `${report.window.to} (not included), UTC.`,
    `- Comments: ${counts.total}.`,
    `- Delivered: ${counts.delivered}.`,
    `- Not delivered: ${counts.notDelivered}.`,
    `- Unknown: ${counts.unknown}.`,
    "",
    "Each comment has one pair or more: a session or role that it was owed, or that Dispatch " +
      "sent it to. The same classes, applied to each pair alone:",
    "",
    identityLine(pairCounts),
    ""
  );
  out.push("## What the classes mean", "");
  out.push(
    "`tq receipts` (theorem-labs/agent-infra `transcript-store/`) gives each pair a " +
      "status: `received`, `not_received` or `unknown`. It reads the transcripts of the pair's " +
      "session and of each session forked from it. This report gives each comment one class. It " +
      "checks the classes in this order, and it uses the first class that applies.",
    "",
    "1. **Delivered.** A session took the comment in: a pair has the status `received`, or " +
      "another session's transcript has the comment (an unlisted receipt). A pair is `received` " +
      "when a transcript has the comment (`transcript`), or when Dispatch has the answer of a BTW " +
      "side turn to the comment (`btw_reply:<id>`).",
    "2. **Not delivered: never-published.** Dispatch's outbox never published the comment's " +
      "event.",
    "3. **Not delivered: no-target.** No session was owed the comment, and Dispatch sent it to " +
      "no session or role.",
    "4. **Unknown.** A pair has the status `unknown`. The comment row names each such pair and " +
      "its reason.",
    "5. **Not delivered, with one cause.** Every pair has the status `not_received`. The report " +
      "gives the first cause that applies to a pair of the comment, in this order:",
    "   - `not-addressed:<rule>`: the pair was owed the comment, the outbox did not send it to " +
      "the pair, and no delivery of a mention to the pair was sent or failed. The rule says why " +
      "the pair was owed it: claimant, route, follower, asker or mention, joined with `+`.",
    "   - `mention-failed:<error>`: Dispatch tried to deliver a mention, and its newest attempt " +
      "failed. The error is `session-not-live`, `receipt-timeout` or `other`.",
    "   - `not-running`: Dispatch sent the comment to the session, and the transcript store's " +
      "recorded start and end of the session's file do not include the time Dispatch published it.",
    "   - `running-no-row`: Dispatch sent the comment to the session within the store's recorded " +
      "start and end of the session's file, and the transcript does not have it. The process can " +
      "still have been down inside that span.",
    "",
    "A pair is judged alone with the same rules: `received` is delivered, `unknown` is unknown, " +
      "and `not_received` gets its one cause.",
    ""
  );
  out.push("## What unknown means", "");
  out.push(
    "The report cannot tell whether an unknown pair got the comment. Its reason is one of these:",
    "",
    "- `session_not_in_store`: the transcript store does not hold the session. The store holds " +
      "only the Oh My Pi sessions of the two dev boxes. It does not hold Claude Code sessions or " +
      "the sessions of Legion pods.",
    "- `role`: the pair is a role, and no mention of the role resolved to a session, so there is " +
      "no session whose transcript to read.",
    "- `btw`: Dispatch delivered a mention as a BTW, and has no answer from the side turn. A BTW " +
      "side turn adds nothing to the transcript.",
    ""
  );
  out.push("## Causes and their fixes", "");
  out.push("| Cause | Comments | Pairs | What it means | Fix |", "| --- | --- | --- | --- | --- |");
  const allCauses = [...new Set([...counts.causes.keys(), ...pairCounts.causes.keys()])].sort(
    compareCauses
  );
  for (const cause of allCauses) {
    const entry = describeCause(cause);
    out.push(
      `| ${cellCode(cause)} | ${counts.causes.get(cause) ?? 0} | ${pairCounts.causes.get(cause) ?? 0} | ` +
        `${cell(entry.meaning)} | ${cell(entry.fix)} |`
    );
  }
  out.push("");
  out.push("## Findings", "");
  if (findings.length === 0) out.push("No findings file was given.", "");
  // Each findings file is Markdown, put in as it is; only its last newline gives way to the
  // blank line that ends it.
  for (const finding of findings) out.push(finding.text.replace(/\n$/, ""), "");
  out.push("## What tq receipts counted", "");
  out.push(
    `- Listed pairs: ${tq.listed_pairs} = received ${tq.received} + not_received ` +
      `${tq.not_received} + unknown ${tq.unknown} (session_not_in_store ` +
      `${tq.unknown_session_not_in_store}, role ${tq.unknown_role}, btw ${tq.unknown_btw}).`,
    `- Pairs with no target (a comments.csv row with no target, which has no receipts.tsv row): ` +
      `${tq.empty_target_pairs}.`,
    `- Unlisted receipt rows: ${tq.unlisted_rows}.`,
    `- Comments: ${tq.comments} = with a receipt ${tq.comments_with_receipt} + without ` +
      `${tq.comments_without_receipt}. A comment with a receipt has a row with the status ` +
      "`received` in receipts.tsv, listed or unlisted.",
    `- A person's comment rows from which no id was read: ${tq.rows_without_id}.`,
    "",
    "The report counted the same numbers from receipts.tsv and comments.csv.",
    ""
  );
  out.push("## Traces", "");
  if (traces.length === 0) out.push("No trace was given.", "");
  for (const trace of traces) out.push(`### ${code(trace.name)}`, "", fence(trace.text), "");
  out.push("## Each comment", "");
  out.push(
    "| Comment | On | Created | Author | Shape | Class | Cause | Pairs |",
    "| --- | --- | --- | --- | --- | --- | --- | --- |"
  );
  for (const judged of report.comments) {
    const { comment, verdict } = judged;
    const pairs =
      judged.pairs
        .map(({ pair, verdict: its }) => {
          const result =
            its.class === "not-delivered" ? cell(its.cause) : verdictText(its, cellCode);
          return `${cellCode(pair.target)} (${owedText(pair)}): ${result}`;
        })
        .join("<br>") || "(none)";
    out.push(
      `| ${idText(comment.id, cellCode)} | ${cellCode(comment.owner)} | ${comment.createdAt} | ` +
        `${cellCode(comment.author)} | ${comment.shape} | ${verdictText(verdict, cellCode)} | ` +
        `${cellCode(verdict.cause)} | ${pairs} |`
    );
  }
  out.push("");
  out.push("## Each pair", "");
  out.push(
    `The rows of the pairs are in the TSV file ${code(pairs.name)}, beside this document: ` +
      `${pairs.rows} rows after a header line, one for each pair, in the order of the comments ` +
      "above. Its columns are " +
      `${PAIR_COLUMNS.slice(0, -1)
        .map((column) => `\`${column}\``)
        .join(", ")} and \`${PAIR_COLUMNS.at(-1)}\`.`
  );
  out.push("");
  out.push("## Unlisted receipts", "");
  out.push(
    "Sessions that took a comment in and are not a pair's session or a session forked from " +
      `one: ${report.unlisted.length}. Each makes its comment delivered.`,
    ""
  );
  for (const receipt of report.unlisted) {
    out.push(
      `- Comment ${idText(receipt.commentId, code)}: ${code(receipt.target)}, first at ` +
        `${receipt.firstReceivedAt}, ${code(receipt.firstReceipt)}`
    );
  }
  return `${out.join("\n").trimEnd()}\n`;
}
