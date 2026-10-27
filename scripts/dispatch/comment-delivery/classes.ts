import { RECEIPT_TIMEOUT_CAUSE } from "../../../packages/contracts/src/dispatch-api";
import {
  COUNT_KEYS,
  type Comment,
  type CommentDraft,
  type CommentRow,
  type CountKey,
  commentsFrom,
  compare,
  type NumberKey,
  type Pair,
  parseCommentsCsv,
  parseCounts,
  parseReceiptsTsv,
  type Receipt,
  type ReceiptCounts,
  ReportRefusal,
  receiptsFrom,
  type Status,
  type UnknownReason,
} from "./inputs.ts";
import { join } from "./join.ts";

// The comment delivery audit's verdicts (INFRA-596): the causes and their fixes, each pair's
// verdict, each comment's class, the counts, and the checks that refuse input that does not add up.

/** The problems with receipts-counts.txt: a sum that its own numbers do not make, a number that
 *  is not what the report counts from receipts.tsv and comments.csv, or a rows_without_id that is
 *  not 0. Each problem names its line. */
function countProblems(
  counts: ReceiptCounts,
  rows: readonly CommentRow[],
  comments: readonly CommentDraft[],
  receipts: readonly Receipt[]
): string[] {
  const line = (key: CountKey) => `receipts-counts.txt line ${COUNT_KEYS.indexOf(key) + 1}`;
  const problems: string[] = [];
  const sums: readonly [NumberKey, readonly NumberKey[]][] = [
    ["listed_pairs", ["received", "not_received", "unknown"]],
    ["unknown", ["unknown_session_not_in_store", "unknown_role", "unknown_btw"]],
    ["comments", ["comments_with_receipt", "comments_without_receipt"]],
  ];
  for (const [total, parts] of sums) {
    const sum = parts.reduce((value, part) => value + counts[part], 0);
    if (sum !== counts[total]) {
      problems.push(
        `${line(total)}: ${total} is ${counts[total]}, but ` +
          `${parts.map((part) => `${part} ${counts[part]}`).join(" + ")} is ${sum}`
      );
    }
  }
  // A comments.csv row with no target has no receipts.tsv row; every other row has one listed row.
  if (counts.listed_pairs !== rows.length - counts.empty_target_pairs) {
    problems.push(
      `${line("listed_pairs")}: listed_pairs is ${counts.listed_pairs}, but comments.csv's ` +
        `${rows.length} rows - empty_target_pairs ${counts.empty_target_pairs} is ` +
        `${rows.length - counts.empty_target_pairs}`
    );
  }
  const listed = receipts.filter((receipt) => receipt.listed);
  const status = (value: Status) => listed.filter((receipt) => receipt.status === value).length;
  const reason = (value: UnknownReason) =>
    listed.filter((receipt) => receipt.unknownReason === value).length;
  // A comment with a receipt has a received row, listed or unlisted, of either evidence.
  const withReceipt = new Set(
    receipts.filter((receipt) => receipt.status === "received").map((receipt) => receipt.commentId)
  ).size;
  const counted: Readonly<Record<Exclude<NumberKey, "rows_without_id">, number>> = {
    listed_pairs: listed.length,
    received: status("received"),
    not_received: status("not_received"),
    unknown: status("unknown"),
    unknown_session_not_in_store: reason("session_not_in_store"),
    unknown_role: reason("role"),
    unknown_btw: reason("btw"),
    unlisted_rows: receipts.length - listed.length,
    comments: comments.length,
    comments_with_receipt: withReceipt,
    comments_without_receipt: comments.length - withReceipt,
    empty_target_pairs: rows.filter((row) => row.target === "").length,
  };
  for (const [key, number] of Object.entries(counted) as [NumberKey, number][]) {
    if (counts[key] !== number) {
      problems.push(
        `${line(key)}: ${key} is ${counts[key]}, but receipts.tsv and comments.csv give ${number}`
      );
    }
  }
  if (counts.rows_without_id !== 0) {
    problems.push(
      `${line("rows_without_id")}: rows_without_id is ${counts.rows_without_id}: tq receipts ` +
        "read no event id, envelope id or comment id from that many of a person's " +
        "comment.created rows, so receipts.tsv can miss receipts"
    );
  }
  return problems;
}

export type ClassName = "delivered" | "not-delivered" | "unknown";

export interface Verdict {
  readonly class: ClassName;
  /** The cause of a not-delivered verdict; empty otherwise. */
  readonly cause: string;
  /** Why a verdict is unknown: the unknown_reason of each unknown pair. */
  readonly why?: string;
  /** What a delivered pair rests on: `transcript` or `btw_reply:<reply id>`. */
  readonly evidence?: string;
}

const DELIVERED: Verdict = { class: "delivered", cause: "" };

function notDelivered(cause: string): Verdict {
  return { class: "not-delivered", cause };
}

/** What a cause means and what fixes it. A `not-addressed:<a>+<b>` cause is read as its parts,
 *  `not-addressed:<a>` and `not-addressed:<b>`. */
export interface CauseEntry {
  readonly meaning: string;
  readonly fix: string;
}

const FAN_OUT_TEST = "packages/envoy/internal/dispatch/outbox/publisher_comment_recipients_test.go";
const FAN_OUT_FIX = `The Dispatch outbox fan-out fix in this pull request. Its test is ${FAN_OUT_TEST}.`;
const LAST_HOP_FIX =
  "INFRA-594: sessions take every event addressed to them, also when it was published while " +
  "they were down.";

/** Every cause, in the order the report gives the first one that applies, and its fix. */
export const CAUSES: Readonly<Record<string, CauseEntry>> = {
  "never-published": {
    meaning: "Dispatch's outbox never published the comment's event.",
    fix:
      "No fix yet. The Dispatch outbox (packages/envoy/internal/dispatch/outbox) must find why " +
      "it did not publish the event.",
  },
  "no-target": {
    meaning: "No session was owed the comment, and Dispatch sent it to no session or role.",
    fix:
      "In this pull request: a reply in a thread on a document goes to every session in the " +
      "thread and to the document's session authors, and a comment on an issue with no route " +
      "and no session claim goes to the session authors of the issue's primary document. A " +
      "Legion issue before admission, and an issue that nobody routes or claims and whose " +
      "document's author has stopped (INFRA-49), still reach no session; the findings give each " +
      "comment's cause.",
  },
  "not-addressed:claimant": {
    meaning: "Dispatch did not send the comment to the issue's claimant session.",
    fix: FAN_OUT_FIX,
  },
  "not-addressed:route": {
    meaning:
      "Dispatch did not send the comment to the session or role that the issue's route names.",
    fix:
      "No fix yet. The outbox sends each comment to the issue's route (publishRoute); a route " +
      "it did not reach needs its own fix.",
  },
  "not-addressed:follower": {
    meaning: "Dispatch did not send the reply to a session that followed the ask.",
    fix:
      "No fix yet. The outbox sends an ask's replies to its followers; a follower it did not " +
      "reach needs its own fix.",
  },
  "not-addressed:asker": {
    meaning: "Dispatch did not send the reply to the session that opened the ask.",
    fix:
      "No fix yet. The outbox sends an ask's replies to its asker; an asker it did not reach " +
      "needs its own fix.",
  },
  "not-addressed:mention": {
    meaning:
      "The comment mentions the session or role, and Dispatch has no sent or failed delivery of " +
      "the mention.",
    fix: "No fix yet. A mention that Dispatch did not try to deliver needs its own fix.",
  },
  "mention-failed:session-not-live": {
    meaning: "Dispatch tried to deliver the mention, and the session was not live.",
    fix: LAST_HOP_FIX,
  },
  "mention-failed:receipt-timeout": {
    meaning:
      "Dispatch tried to deliver the mention, and the listener did not answer in time. The " +
      "mention can have arrived.",
    fix: "No fix yet. A mention delivery that times out needs its own fix.",
  },
  "mention-failed:other": {
    meaning:
      "Dispatch tried to deliver the mention, and it failed with another error. The pair rows " +
      "give the error.",
    fix: "No fix yet. Each such error needs its own fix.",
  },
  "not-running": {
    meaning:
      "Dispatch sent the comment to the session, and the transcript store's recorded start and " +
      "end of the session's file do not include the time Dispatch published it.",
    fix: LAST_HOP_FIX,
  },
  "running-no-row": {
    meaning:
      "Dispatch sent the comment to the session within the store's recorded start and end of " +
      "the session's file, and the transcript has no record of it. The process can still have " +
      "been down inside that span; the loss is in the last hop or in the plugin.",
    fix: LAST_HOP_FIX,
  },
};

/** A mention delivery's error, as the rule of its `mention-failed` cause. */
export function mentionErrorClass(error: string): "session-not-live" | "receipt-timeout" | "other" {
  if (/^no live session\b|session not found|not live/i.test(error)) return "session-not-live";
  if (error === RECEIPT_TIMEOUT_CAUSE) return "receipt-timeout";
  return "other";
}

/** A cause's parts: `not-addressed:<a>+<b>` is `not-addressed:<a>` and `not-addressed:<b>`;
 *  any other cause is its one part. */
function causeParts(cause: string): string[] {
  return cause.startsWith("not-addressed:")
    ? cause
        .slice("not-addressed:".length)
        .split("+")
        .map((rule) => `not-addressed:${rule}`)
    : [cause];
}

const CAUSE_RANKS = new Map(Object.keys(CAUSES).map((cause, index) => [cause, index]));

/** Orders causes as the report checks them: by the CAUSES place of each part in turn, a part
 *  with no entry after every part with one. */
export function compareCauses(left: string, right: string): number {
  const rank = (part: string) => CAUSE_RANKS.get(part) ?? CAUSE_RANKS.size;
  const a = causeParts(left).map(rank);
  const b = causeParts(right).map(rank);
  for (let index = 0; index < Math.max(a.length, b.length); index++) {
    const difference = (a[index] ?? -1) - (b[index] ?? -1);
    if (difference !== 0) return difference;
  }
  return compare(left, right);
}

/** What `causes` says of a cause, or undefined when a part of it has no entry. */
export function describeCause(
  cause: string,
  causes: Readonly<Record<string, CauseEntry>>
): CauseEntry | undefined {
  const entries = causeParts(cause).map((part) => causes[part]);
  if (entries.some((entry) => entry === undefined)) return undefined;
  const found = entries as CauseEntry[];
  return {
    meaning: found.map((entry) => entry.meaning).join(" "),
    fix: [...new Set(found.map((entry) => entry.fix))].join(" "),
  };
}

/** One pair, judged alone from its receipts.tsv status. A not_received pair gets the first cause
 *  that applies: never-published; not-addressed, when it was owed, the outbox did not send it,
 *  and no mention attempt was sent or failed; mention-failed; not-running, when published_at is
 *  outside the checked session's times; else running-no-row. */
function judgePair(comment: Comment, pair: Pair): Verdict {
  const { receipt } = pair;
  if (receipt.status === "received") return { ...DELIVERED, evidence: receipt.evidence };
  if (receipt.status === "unknown")
    return { class: "unknown", cause: "", why: receipt.unknownReason };
  if (comment.publishedAt === "") return notDelivered("never-published");
  const sent = pair.addressed || pair.mentionState === "sent";
  if (pair.reasons.length > 0 && !sent && pair.mentionState !== "failed") {
    return notDelivered(`not-addressed:${pair.reasons.join("+")}`);
  }
  if (pair.mentionState === "failed") {
    return notDelivered(`mention-failed:${mentionErrorClass(pair.mentionError)}`);
  }
  const at = comment.publishedUs ?? 0;
  const running =
    receipt.startedUs !== undefined &&
    receipt.startedUs <= at &&
    (receipt.endedUs === undefined || at <= receipt.endedUs);
  return notDelivered(running ? "running-no-row" : "not-running");
}

/** What one comment's class rule reads. */
export interface CommentFacts {
  readonly comment: Comment;
  /** Every receipts.tsv row of the comment, listed or not. */
  readonly receipts: readonly Receipt[];
  readonly pairs: readonly { readonly pair: Pair; readonly verdict: Verdict }[];
}

/** A class a comment can fall in. The report checks CLASS_RULES in order and gives a comment the
 *  verdict of the first rule that applies, so a rule need not exclude the comments of the rules
 *  before it; the report refuses a comment that no rule applies to. */
export interface ClassRule {
  readonly name: string;
  applies(facts: CommentFacts): boolean;
  verdict(facts: CommentFacts): Verdict;
}

function received(facts: CommentFacts): boolean {
  return facts.receipts.some((receipt) => receipt.status === "received");
}

function unknownPairs(facts: CommentFacts): readonly { pair: Pair; verdict: Verdict }[] {
  return facts.pairs.filter(({ verdict }) => verdict.class === "unknown");
}

/** The first cause, in CAUSES order, of the comment's pairs. */
function primaryCause(facts: CommentFacts): string {
  const causes = facts.pairs
    .map(({ verdict }) => verdict.cause)
    .filter((cause) => cause !== "")
    .sort(compareCauses);
  return causes[0] ?? "";
}

export const CLASS_RULES: readonly ClassRule[] = [
  { name: "delivered", applies: received, verdict: () => DELIVERED },
  {
    name: "never-published",
    applies: (facts) => facts.comment.publishedAt === "",
    verdict: () => notDelivered("never-published"),
  },
  {
    name: "no-target",
    applies: (facts) => facts.pairs.length === 0,
    verdict: () => notDelivered("no-target"),
  },
  {
    name: "unknown",
    applies: (facts) => unknownPairs(facts).length > 0,
    verdict: (facts) => ({
      class: "unknown",
      cause: "",
      why: unknownPairs(facts)
        .map(({ pair, verdict }) => `${pair.target}: ${verdict.why ?? ""}`)
        .join("; "),
    }),
  },
  // Every pair left is not_received.
  {
    name: "not delivered, with a cause",
    applies: () => true,
    verdict: (facts) => notDelivered(primaryCause(facts)),
  },
];

export interface Judged extends CommentFacts {
  readonly verdict: Verdict;
}

export interface Counts {
  readonly total: number;
  readonly delivered: number;
  readonly notDelivered: number;
  readonly unknown: number;
  /** Every cause of CAUSES (zero included) and every combined cause the input produced. */
  readonly causes: ReadonlyMap<string, number>;
}

function countVerdicts(
  verdicts: readonly Verdict[],
  causes: Readonly<Record<string, CauseEntry>>
): Counts {
  const byCause = new Map<string, number>(Object.keys(causes).map((cause) => [cause, 0]));
  for (const verdict of verdicts) {
    if (verdict.class === "not-delivered") {
      byCause.set(verdict.cause, (byCause.get(verdict.cause) ?? 0) + 1);
    }
  }
  return {
    total: verdicts.length,
    delivered: verdicts.filter((verdict) => verdict.class === "delivered").length,
    notDelivered: verdicts.filter((verdict) => verdict.class === "not-delivered").length,
    unknown: verdicts.filter((verdict) => verdict.class === "unknown").length,
    causes: new Map([...byCause].sort(([left], [right]) => compareCauses(left, right))),
  };
}

/** `Total T = delivered D + not delivered N (cause n, ...) + unknown U`. */
export function identityLine(counts: Counts): string {
  const causes = [...counts.causes].map(([cause, count]) => `${cause} ${count}`).join(", ");
  return (
    `Total ${counts.total} = delivered ${counts.delivered} + not delivered ` +
    `${counts.notDelivered} (${causes}) + unknown ${counts.unknown}`
  );
}

/** The problems with an identity that does not add up; empty when it does. */
export function identityProblems(counts: Counts, level: string): string[] {
  const causes = [...counts.causes.values()].reduce((sum, count) => sum + count, 0);
  const problems: string[] = [];
  if (counts.delivered + counts.notDelivered + counts.unknown !== counts.total) {
    problems.push(
      `${level}: the identity fails: ${identityLine(counts)}, and ${counts.delivered} + ` +
        `${counts.notDelivered} + ${counts.unknown} is not ${counts.total}`
    );
  }
  if (causes !== counts.notDelivered) {
    problems.push(
      `${level}: the identity fails: the causes add up to ${causes}, not ${counts.notDelivered}`
    );
  }
  return problems;
}

export interface ReportInput {
  readonly comments: string;
  readonly receipts: string;
  readonly counts: string;
}

export interface ReportOptions {
  readonly rules?: readonly ClassRule[];
  readonly causes?: Readonly<Record<string, CauseEntry>>;
}

export interface Report {
  readonly window: { readonly from: string; readonly to: string };
  readonly receiptCounts: ReceiptCounts;
  readonly comments: readonly Judged[];
  readonly commentCounts: Counts;
  readonly pairCounts: Counts;
  /** The listed=false rows: sessions that took a comment in and are no target, by comment. */
  readonly unlisted: readonly Receipt[];
}

/** Reads and judges the three files, or throws ReportRefusal naming every problem. */
export function judge(input: ReportInput, options: ReportOptions = {}): Report {
  const rules = options.rules ?? CLASS_RULES;
  const causes = options.causes ?? CAUSES;
  const problems: string[] = [];
  const rows = parseCommentsCsv(input.comments, problems);
  const drafts = commentsFrom(rows, problems);
  const receipts = receiptsFrom(parseReceiptsTsv(input.receipts, problems), problems);
  const receiptCounts = parseCounts(input.counts, problems);
  if (problems.length > 0 || receiptCounts === undefined) throw new ReportRefusal(problems);
  const comments = join(drafts, receipts, problems);
  problems.push(...countProblems(receiptCounts, rows, drafts, receipts));
  if (problems.length > 0) throw new ReportRefusal(problems);

  const receiptsOf = new Map<string, Receipt[]>();
  for (const receipt of receipts) {
    const list = receiptsOf.get(receipt.commentId);
    if (list === undefined) receiptsOf.set(receipt.commentId, [receipt]);
    else list.push(receipt);
  }
  const judged: Judged[] = [];
  for (const comment of comments) {
    const facts: CommentFacts = {
      comment,
      receipts: receiptsOf.get(comment.id) ?? [],
      pairs: comment.pairs.map((pair) => ({ pair, verdict: judgePair(comment, pair) })),
    };
    const rule = rules.find((candidate) => candidate.applies(facts));
    if (rule === undefined) {
      problems.push(`comment ${comment.id} falls in no class`);
      continue;
    }
    const verdict = rule.verdict(facts);
    if (verdict.class === "not-delivered" && verdict.cause === "") {
      problems.push(`comment ${comment.id} is not delivered and has no cause`);
    }
    judged.push({ ...facts, verdict });
  }

  const commentVerdicts = judged.map((comment) => comment.verdict);
  // A comment with no target counts once at the pair level too, with its own verdict.
  const pairVerdicts = judged.flatMap((comment) =>
    comment.pairs.length === 0 ? [comment.verdict] : comment.pairs.map(({ verdict }) => verdict)
  );
  for (const verdict of [...commentVerdicts, ...pairVerdicts]) {
    // A not-delivered verdict with no cause is refused above.
    if (
      verdict.class === "not-delivered" &&
      verdict.cause !== "" &&
      describeCause(verdict.cause, causes) === undefined
    ) {
      const problem = `the cause ${verdict.cause} has no fix in the cause-to-fix table`;
      if (!problems.includes(problem)) problems.push(problem);
    }
  }
  const commentCounts = countVerdicts(commentVerdicts, causes);
  const pairCounts = countVerdicts(pairVerdicts, causes);
  problems.push(...identityProblems(commentCounts, "comments"));
  problems.push(...identityProblems(pairCounts, "pairs"));
  if (problems.length > 0) throw new ReportRefusal(problems);
  const order = new Map(judged.map((comment, index) => [comment.comment.id, index]));
  return {
    window: { from: receiptCounts.from, to: receiptCounts.to },
    receiptCounts,
    comments: judged,
    commentCounts,
    pairCounts,
    unlisted: receipts
      .filter((receipt) => !receipt.listed)
      .sort(
        (left, right) =>
          (order.get(left.commentId) ?? 0) - (order.get(right.commentId) ?? 0) ||
          compare(left.target, right.target)
      ),
  };
}
