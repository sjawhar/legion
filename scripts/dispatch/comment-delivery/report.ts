import { readFile } from "node:fs/promises";
import { basename } from "node:path";
import { parseArgs } from "node:util";
import { RECEIPT_TIMEOUT_CAUSE } from "../../../packages/contracts/src/dispatch-api";

// Turns the three files of the comment delivery audit (INFRA-596) into a document for people:
// comments.csv from packages/envoy/internal/dispatch/deliveryaudit/comments.sql, and receipts.tsv
// and receipts-counts.txt from `tq receipts` (INFRA-608, theorem-labs/agent-infra
// transcript-store). scripts/dispatch/README.md ("Comment delivery") gives the run. Input that
// does not agree with itself is refused (exit 1, every problem on stderr, nothing on stdout),
// never counted.

/** comments.csv's columns, in order: the contract in deliveryaudit/README.md. */
export const COMMENT_COLUMNS = [
  "comment_id",
  "event_id",
  "owner",
  "created_at",
  "published_at",
  "author",
  "shape",
  "reopened",
  "target",
  "owed_reasons",
  "addressed",
  "mention_session_id",
  "mention_envelope_ids",
  "mention_btw_reply_id",
  "mention_modes",
  "mention_state",
  "mention_error",
  "route_used",
  "claim_used",
  "route_source",
] as const;
/** receipts.tsv's columns, in order: INFRA-608's output contract. */
export const RECEIPT_COLUMNS = [
  "comment_id",
  "event_id",
  "target",
  "listed",
  "checked_session",
  "status",
  "unknown_reason",
  "evidence",
  "received_by",
  "first_received_at",
  "first_receipt",
  "target_started_at",
  "target_ended_at",
] as const;

/** The columns every row of one comment repeats. */
const COMMENT_LEVEL_COLUMNS = [
  "event_id",
  "owner",
  "created_at",
  "published_at",
  "author",
  "shape",
  "reopened",
  "route_used",
  "claim_used",
  "route_source",
] as const;
const SHAPES: Readonly<Record<string, true>> = {
  ask_thread: true,
  reply: true,
  anchored: true,
  conversation: true,
};
const MENTION_STATES: Readonly<Record<string, true>> = {
  "": true,
  pending: true,
  sent: true,
  failed: true,
};
const MENTION_MODES: Readonly<Record<string, true>> = { btw: true, aside: true, steer: true };
const STATUSES = ["received", "not_received", "unknown"] as const;
const UNKNOWN_REASONS = ["session_not_in_store", "role", "btw"] as const;
/** Why a target was owed a comment, in the order a `not-addressed` cause names them. */
const OWED_REASONS = ["claimant", "route", "follower", "asker", "mention"] as const;

type OwedReason = (typeof OWED_REASONS)[number];
export type Status = (typeof STATUSES)[number];
export type UnknownReason = (typeof UNKNOWN_REASONS)[number];
type Row<T extends readonly string[]> = { readonly [K in T[number]]: string } & {
  readonly line: number;
};
export type CommentRow = Row<typeof COMMENT_COLUMNS>;
export type ReceiptRow = Row<typeof RECEIPT_COLUMNS>;

export class ReportRefusal extends Error {
  readonly problems: readonly string[];
  constructor(problems: readonly string[]) {
    super(`the report refuses its input:\n${problems.map((problem) => `- ${problem}`).join("\n")}`);
    this.name = "ReportRefusal";
    this.problems = problems;
  }
}

// ---------------------------------------------------------------- reading the files

interface Record_ {
  readonly fields: string[];
  readonly line: number;
}

/** RFC 4180 records, as psql --csv writes them: a quoted field may hold commas, `""` and
 *  newlines. `line` is the line the record starts on. */
function csvRecords(text: string, file: string, problems: string[]): Record_[] {
  const records: Record_[] = [];
  let fields: string[] = [];
  let field = "";
  let quoted = false;
  let line = 1;
  let start = 1;
  let atFieldStart = true;
  for (let index = 0; index < text.length; index++) {
    const char = text[index];
    if (quoted) {
      if (char === '"' && text[index + 1] === '"') {
        field += '"';
        index++;
      } else if (char === '"') {
        quoted = false;
      } else {
        if (char === "\n") line++;
        field += char;
      }
      continue;
    }
    if (char === '"' && atFieldStart) {
      quoted = true;
      atFieldStart = false;
    } else if (char === ",") {
      fields.push(field);
      field = "";
      atFieldStart = true;
    } else if (char === "\n" || (char === "\r" && text[index + 1] === "\n")) {
      if (char === "\r") index++;
      fields.push(field);
      records.push({ fields, line: start });
      fields = [];
      field = "";
      atFieldStart = true;
      line++;
      start = line;
    } else {
      field += char;
      atFieldStart = false;
    }
  }
  if (quoted) problems.push(`${file}: a quoted field that starts on line ${start} never ends`);
  if (field !== "" || fields.length > 0) {
    fields.push(field);
    records.push({ fields, line: start });
  }
  return records;
}

const TSV_ESCAPES: Readonly<Record<string, string>> = {
  "\\": "\\",
  t: "\t",
  n: "\n",
  r: "\r",
  "0": "\0",
  b: "\b",
  f: "\f",
  "'": "'",
};

/** TSV with a header: one record a line, fields split by tabs, backslash escapes. A NULL
 *  (`\N`) reads as empty. */
function tsvRecords(text: string): Record_[] {
  const lines = text.split("\n");
  if (lines.at(-1) === "") lines.pop();
  return lines.map((raw, index) => ({
    line: index + 1,
    fields: raw
      .split("\t")
      .map((field) =>
        field === "\\N"
          ? ""
          : field.replace(/\\(.)/g, (whole, char: string) => TSV_ESCAPES[char] ?? whole)
      ),
  }));
}

function table<T extends readonly string[]>(
  records: readonly Record_[],
  columns: T,
  file: string,
  separator: string,
  problems: string[]
): Row<T>[] {
  const [header, ...body] = records;
  if (header === undefined || header.fields.join(separator) !== columns.join(separator)) {
    problems.push(
      `${file}: the header is "${header?.fields.join(separator) ?? ""}", not "${columns.join(separator)}"`
    );
    return [];
  }
  const rows: Row<T>[] = [];
  for (const record of body) {
    if (record.fields.length !== columns.length) {
      problems.push(
        `${file} line ${record.line}: ${record.fields.length} fields, not ${columns.length}`
      );
      continue;
    }
    const row: Record<string, string | number> = { line: record.line };
    for (const [index, column] of columns.entries()) row[column] = record.fields[index] ?? "";
    rows.push(row as Row<T>);
  }
  return rows;
}

export function parseCommentsCsv(text: string, problems: string[]): CommentRow[] {
  const records = csvRecords(text, "comments.csv", problems);
  return table(records, COMMENT_COLUMNS, "comments.csv", ",", problems);
}

export function parseReceiptsTsv(text: string, problems: string[]): ReceiptRow[] {
  return table(tsvRecords(text), RECEIPT_COLUMNS, "receipts.tsv", "\t", problems);
}

/** receipts-counts.txt's keys, one line each, in order: INFRA-608's output contract. */
export const COUNT_KEYS = [
  "from",
  "to",
  "listed_pairs",
  "received",
  "not_received",
  "unknown",
  "unknown_session_not_in_store",
  "unknown_role",
  "unknown_btw",
  "unlisted_rows",
  "comments",
  "comments_with_receipt",
  "comments_without_receipt",
  "empty_target_pairs",
  "rows_without_id",
] as const;
type CountKey = (typeof COUNT_KEYS)[number];
type NumberKey = Exclude<CountKey, "from" | "to">;

/** receipts-counts.txt: what `tq receipts` counted, by key. */
export type ReceiptCounts = { readonly from: string; readonly to: string } & {
  readonly [K in NumberKey]: number;
};

/** receipts-counts.txt: exactly one `<key>\t<value>` line for each of COUNT_KEYS, in order, and
 *  a newline at the end. `from` and `to` are `YYYY-MM-DDTHH:MM:SSZ`; every other value is a plain
 *  integer. Undefined, with a problem for each line that is not so. */
export function parseCounts(text: string, problems: string[]): ReceiptCounts | undefined {
  const found: string[] = [];
  if (!text.endsWith("\n")) problems.push("receipts-counts.txt: the last line has no newline");
  const lines = text === "" ? [] : text.replace(/\n$/, "").split("\n");
  for (let index = 0; index < Math.max(lines.length, COUNT_KEYS.length); index++) {
    const where = `receipts-counts.txt line ${index + 1}`;
    const key = COUNT_KEYS[index];
    const line = lines[index];
    if (key === undefined) {
      problems.push(`${where}: "${line}" is after the last key, rows_without_id`);
      continue;
    }
    if (line === undefined) {
      problems.push(`${where}: missing; the "${key}" line comes here`);
      continue;
    }
    const tab = line.indexOf("\t");
    const [given, value] =
      tab === -1 ? [line, undefined] : [line.slice(0, tab), line.slice(tab + 1)];
    if (given !== key) {
      problems.push(`${where}: the key is "${given}", not "${key}"`);
    } else if (value === undefined) {
      problems.push(`${where}: "${line}" has no tab and value`);
    } else if (key === "from" || key === "to") {
      if (/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$/.test(value) && instant(value) !== undefined) {
        found.push(value);
      } else {
        problems.push(`${where}: ${key} "${value}" is not a time YYYY-MM-DDTHH:MM:SSZ`);
      }
    } else if (/^(0|[1-9]\d*)$/.test(value) && Number.isSafeInteger(Number(value))) {
      found.push(value);
    } else {
      problems.push(`${where}: ${key} "${value}" is not a plain integer`);
    }
  }
  if (found.length !== COUNT_KEYS.length) return undefined;
  const [from = "", to = "", ...numbers] = found;
  return {
    from,
    to,
    ...Object.fromEntries(COUNT_KEYS.slice(2).map((key, index) => [key, Number(numbers[index])])),
  } as ReceiptCounts;
}

/** A UTC time as microseconds since the epoch, or undefined when it is not `YYYY-MM-DD[T ]
 *  HH:MM:SS[.fraction][Z]`. A time with no `Z` is read as UTC. */
function instant(value: string): number | undefined {
  const match = /^(\d{4})-(\d\d)-(\d\d)[T ](\d\d):(\d\d):(\d\d)(?:\.(\d{1,9}))?Z?$/.exec(value);
  if (match === null) return undefined;
  const [, year, month, day, hour, minute, second, fraction = ""] = match;
  const milliseconds = Date.UTC(
    Number(year),
    Number(month) - 1,
    Number(day),
    Number(hour),
    Number(minute),
    Number(second)
  );
  return milliseconds * 1000 + Number(fraction.padEnd(6, "0").slice(0, 6));
}

function compare(left: string, right: string): number {
  return left < right ? -1 : left > right ? 1 : 0;
}

/** A comma-joined list of comments.csv, one element per mention attempt; empty for none. */
function attempts(value: string): string[] {
  return value === "" ? [] : value.split(",");
}

// ---------------------------------------------------------------- the model

/** One row of receipts.tsv. */
export interface Receipt {
  readonly line: number;
  readonly commentId: string;
  readonly eventId: string;
  readonly target: string;
  readonly listed: boolean;
  readonly checkedSession: string;
  readonly status: Status;
  readonly unknownReason: UnknownReason | "";
  readonly evidence: string;
  readonly receivedBy: string;
  readonly firstReceivedAt: string;
  readonly firstReceipt: string;
  readonly targetStartedAt: string;
  readonly targetEndedAt: string;
  readonly startedUs: number | undefined;
  /** Undefined while the session is still running, or when the store does not hold it. */
  readonly endedUs: number | undefined;
}

/** One (comment, target) pair of comments.csv, with its listed receipts.tsv row. */
export interface Pair {
  readonly target: string;
  /** Why it was owed the comment, in OWED_REASONS order; empty when only addressed. */
  readonly reasons: readonly OwedReason[];
  readonly addressed: boolean;
  readonly mentionSessionId: string;
  /** One element per attempt, by attempt; an element is empty for an attempt with none. */
  readonly mentionEnvelopeIds: readonly string[];
  readonly mentionBtwReplyId: string;
  /** One element per attempt, by attempt, in the order of mentionEnvelopeIds. */
  readonly mentionModes: readonly string[];
  readonly mentionState: string;
  readonly mentionError: string;
  readonly receipt: Receipt;
}

export interface Comment {
  readonly id: string;
  readonly eventId: string;
  readonly owner: string;
  readonly createdAt: string;
  readonly publishedAt: string;
  readonly publishedUs: number | undefined;
  readonly author: string;
  readonly shape: string;
  /** Sorted by target. Empty for a comment with no target. */
  readonly pairs: readonly Pair[];
}

type PairRow = Omit<Pair, "receipt"> & { readonly line: number };
interface CommentDraft extends Omit<Comment, "pairs"> {
  readonly pairs: readonly PairRow[];
}

function isOwedReason(value: string): value is OwedReason {
  return (OWED_REASONS as readonly string[]).includes(value);
}

function commentsFrom(rows: readonly CommentRow[], problems: string[]): CommentDraft[] {
  const byId = new Map<string, CommentRow[]>();
  for (const row of rows) {
    const where = `comments.csv line ${row.line} (comment ${row.comment_id})`;
    if (row.comment_id === "") problems.push(`comments.csv line ${row.line}: no comment_id`);
    if (SHAPES[row.shape] !== true) problems.push(`${where}: shape "${row.shape}"`);
    if (row.reopened !== "true" && row.reopened !== "false") {
      problems.push(`${where}: reopened "${row.reopened}"`);
    }
    if (row.addressed !== "true" && row.addressed !== "false") {
      problems.push(`${where}: addressed "${row.addressed}"`);
    }
    if (instant(row.created_at) === undefined) {
      problems.push(`${where}: created_at "${row.created_at}"`);
    }
    if (row.published_at !== "" && instant(row.published_at) === undefined) {
      problems.push(`${where}: published_at "${row.published_at}"`);
    }
    if (MENTION_STATES[row.mention_state] !== true) {
      problems.push(`${where}: mention_state "${row.mention_state}"`);
    }
    const modes = attempts(row.mention_modes);
    if (!modes.every((mode) => MENTION_MODES[mode] === true)) {
      problems.push(`${where}: mention_modes "${row.mention_modes}"`);
    }
    if ((row.mention_state === "") !== (modes.length === 0)) {
      problems.push(
        `${where}: mention_state "${row.mention_state}" with mention_modes "${row.mention_modes}"`
      );
    }
    // An attempt with no envelope id is an empty element, so both lists have one element per
    // attempt; a single attempt with none is the empty string.
    const envelopes =
      modes.length === 0 ? attempts(row.mention_envelope_ids) : row.mention_envelope_ids.split(",");
    if (envelopes.length !== modes.length) {
      problems.push(
        `${where}: mention_envelope_ids "${row.mention_envelope_ids}" has ${envelopes.length} ` +
          `attempts, mention_modes "${row.mention_modes}" ${modes.length}`
      );
    }
    if (row.mention_btw_reply_id !== "" && !modes.includes("btw")) {
      problems.push(`${where}: mention_btw_reply_id with no btw attempt`);
    }
    if (row.target === "") {
      if (row.owed_reasons !== "" || row.addressed !== "false" || modes.length > 0) {
        problems.push(`${where}: a row with no target names a reason, an address or a mention`);
      }
    } else {
      if (!/^(session|role):./.test(row.target)) problems.push(`${where}: target "${row.target}"`);
      const reasons = row.owed_reasons.split(";");
      const known = row.owed_reasons === "addressed-only" || reasons.every(isOwedReason);
      if (!known || row.owed_reasons === "") {
        problems.push(`${where}: owed_reasons "${row.owed_reasons}"`);
      }
      if (row.owed_reasons === "addressed-only" && row.addressed !== "true") {
        problems.push(`${where}: addressed-only but not addressed`);
      }
    }
    const group = byId.get(row.comment_id);
    if (group === undefined) byId.set(row.comment_id, [row]);
    else group.push(row);
  }
  const comments: CommentDraft[] = [];
  for (const [id, group] of byId) {
    const [first] = group;
    if (first === undefined) continue;
    const seen = new Map<string, number>();
    for (const row of group) {
      const earlier = seen.get(row.target);
      if (earlier !== undefined) {
        problems.push(
          `comments.csv lines ${earlier} and ${row.line}: comment ${id} and target "${row.target}" twice`
        );
      }
      seen.set(row.target, row.line);
      for (const column of COMMENT_LEVEL_COLUMNS) {
        if (row[column] !== first[column]) {
          problems.push(
            `comments.csv lines ${first.line} and ${row.line}: comment ${id} has two values of ${column}`
          );
        }
      }
    }
    if (group.length > 1 && seen.has("")) {
      problems.push(`comments.csv: comment ${id} has a row with no target beside rows with one`);
    }
    const pairs = group
      .filter((row) => row.target !== "")
      .map(
        (row): PairRow => ({
          line: row.line,
          target: row.target,
          reasons: OWED_REASONS.filter((reason) => row.owed_reasons.split(";").includes(reason)),
          addressed: row.addressed === "true",
          mentionSessionId: row.mention_session_id,
          mentionEnvelopeIds: row.mention_modes === "" ? [] : row.mention_envelope_ids.split(","),
          mentionBtwReplyId: row.mention_btw_reply_id,
          mentionModes: attempts(row.mention_modes),
          mentionState: row.mention_state,
          mentionError: row.mention_error,
        })
      )
      .sort((left, right) => compare(left.target, right.target));
    comments.push({
      id,
      eventId: first.event_id,
      owner: first.owner,
      createdAt: first.created_at,
      publishedAt: first.published_at,
      publishedUs: instant(first.published_at),
      author: first.author,
      shape: first.shape,
      pairs,
    });
  }
  return comments.sort(
    (left, right) => compare(left.createdAt, right.createdAt) || compare(left.id, right.id)
  );
}

function receiptsFrom(rows: readonly ReceiptRow[], problems: string[]): Receipt[] {
  // INFRA-608 sorts the rows by comment_id, then listed (true first), then target.
  const unsorted = rows.find((row, index) => {
    const before = rows[index - 1];
    if (before === undefined) return false;
    const order =
      compare(before.comment_id, row.comment_id) ||
      Number(before.listed !== "true") - Number(row.listed !== "true") ||
      compare(before.target, row.target);
    return order > 0;
  });
  if (unsorted !== undefined) {
    problems.push(
      `receipts.tsv line ${unsorted.line} (comment ${unsorted.comment_id}): out of order: the ` +
        "rows are not sorted by comment_id, then listed (true first), then target"
    );
  }
  const receipts: Receipt[] = [];
  for (const row of rows) {
    const where = `receipts.tsv line ${row.line} (comment ${row.comment_id})`;
    const status = (STATUSES as readonly string[]).includes(row.status)
      ? (row.status as Status)
      : undefined;
    const reason = (UNKNOWN_REASONS as readonly string[]).includes(row.unknown_reason)
      ? (row.unknown_reason as UnknownReason)
      : row.unknown_reason === ""
        ? ""
        : undefined;
    if (row.comment_id === "") problems.push(`receipts.tsv line ${row.line}: no comment_id`);
    if (row.listed !== "true" && row.listed !== "false") {
      problems.push(`${where}: listed "${row.listed}"`);
    }
    if (status === undefined) problems.push(`${where}: status "${row.status}"`);
    if (reason === undefined) problems.push(`${where}: unknown_reason "${row.unknown_reason}"`);
    if (!/^(transcript|btw_reply:.+)?$/.test(row.evidence)) {
      problems.push(`${where}: evidence "${row.evidence}"`);
    }
    if ((status === "unknown") !== (row.unknown_reason !== "")) {
      problems.push(`${where}: status "${row.status}" with unknown_reason "${row.unknown_reason}"`);
    }
    if ((status === "received") !== (row.evidence !== "")) {
      problems.push(`${where}: status "${row.status}" with evidence "${row.evidence}"`);
    }
    if (row.listed === "false" && !/^session:./.test(row.target)) {
      problems.push(`${where}: an unlisted row with target "${row.target}", not session:<id>`);
    }
    if (status === "not_received" && row.target_started_at === "") {
      problems.push(`${where}: not_received with no target_started_at`);
    }
    for (const column of ["first_received_at", "target_started_at", "target_ended_at"] as const) {
      if (row[column] !== "" && !/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$/.test(row[column])) {
        problems.push(`${where}: ${column} "${row[column]}" is not YYYY-MM-DDTHH:MM:SS.mmmZ`);
      }
    }
    const startedUs = row.target_started_at === "" ? undefined : instant(row.target_started_at);
    const endedUs = row.target_ended_at === "" ? undefined : instant(row.target_ended_at);
    if (status === undefined || reason === undefined) continue;
    receipts.push({
      line: row.line,
      commentId: row.comment_id,
      eventId: row.event_id,
      target: row.target,
      listed: row.listed === "true",
      checkedSession: row.checked_session,
      status,
      unknownReason: reason,
      evidence: row.evidence,
      receivedBy: row.received_by,
      firstReceivedAt: row.first_received_at,
      firstReceipt: row.first_receipt,
      targetStartedAt: row.target_started_at,
      targetEndedAt: row.target_ended_at,
      startedUs,
      endedUs,
    });
  }
  return receipts;
}

/** Pairs each comments.csv pair with its one listed receipts.tsv row; refuses a pair with none,
 *  a listed row with no pair, a row whose comment, listed and target repeat, and a row whose event
 *  is not its comment's. */
function join(
  drafts: readonly CommentDraft[],
  receipts: readonly Receipt[],
  problems: string[]
): Comment[] {
  const byComment = new Map(drafts.map((comment) => [comment.id, comment]));
  const listed = new Map<string, Receipt>();
  const unlisted = new Map<string, Receipt>();
  for (const receipt of receipts) {
    const comment = byComment.get(receipt.commentId);
    if (comment === undefined) {
      problems.push(
        `receipts.tsv line ${receipt.line}: comment ${receipt.commentId} is not in comments.csv`
      );
      continue;
    }
    if (receipt.eventId !== comment.eventId) {
      problems.push(
        `receipts.tsv line ${receipt.line}: comment ${receipt.commentId} with event ` +
          `"${receipt.eventId}", but comments.csv gives event "${comment.eventId}"`
      );
    }
    const [seen, what] = receipt.listed ? [listed, "target"] : [unlisted, "unlisted target"];
    const key = `${receipt.commentId}\t${receipt.target}`;
    const earlier = seen.get(key);
    if (earlier !== undefined) {
      problems.push(
        `receipts.tsv lines ${earlier.line} and ${receipt.line}: comment ${receipt.commentId} and ${what} "${receipt.target}" twice`
      );
    }
    seen.set(key, receipt);
  }
  const paired = new Set<string>();
  const comments: Comment[] = [];
  for (const draft of drafts) {
    const pairs: Pair[] = [];
    for (const { line, ...pair } of draft.pairs) {
      const key = `${draft.id}\t${pair.target}`;
      const receipt = listed.get(key);
      if (receipt === undefined) {
        problems.push(
          `comments.csv line ${line}: comment ${draft.id} and target ${pair.target} have no listed row in receipts.tsv`
        );
        continue;
      }
      paired.add(key);
      pairs.push({ ...pair, receipt });
    }
    comments.push({ ...draft, pairs });
  }
  for (const [key, receipt] of listed) {
    if (!paired.has(key) && byComment.has(receipt.commentId)) {
      problems.push(
        `receipts.tsv line ${receipt.line}: comment ${receipt.commentId} and target ` +
          `"${receipt.target}" is a listed row that comments.csv does not hold`
      );
    }
  }
  return comments;
}

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

// ---------------------------------------------------------------- classes and causes

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
      "The Dispatch fan-out fix in this pull request: a comment on an issue with no route goes " +
      `to the issue's claimant session. Its test is ${FAN_OUT_TEST}.`,
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
      "Dispatch sent the comment to the session, and the session was not running when Dispatch " +
      "published it: the transcript store's times for the session do not include that time.",
    fix: LAST_HOP_FIX,
  },
  "running-no-row": {
    meaning:
      "Dispatch sent the comment to the session while the session was running, and the " +
      "session's transcript has no record of it. The loss is in the last hop or in the plugin.",
    fix: LAST_HOP_FIX,
  },
};

const CAUSE_CATEGORIES = [
  "never-published",
  "no-target",
  "not-addressed",
  "mention-failed",
  "not-running",
  "running-no-row",
] as const;
const MENTION_ERROR_CLASSES = ["session-not-live", "receipt-timeout", "other"] as const;

/** A mention delivery's error, as one of MENTION_ERROR_CLASSES. */
export function mentionErrorClass(error: string): (typeof MENTION_ERROR_CLASSES)[number] {
  if (/^no live session\b|session not found|not live/i.test(error)) return "session-not-live";
  if (error === RECEIPT_TIMEOUT_CAUSE) return "receipt-timeout";
  return "other";
}

/** Orders causes as the report checks them: by category, then by rule. */
function causeKey(cause: string): number[] {
  const [category = "", rule = ""] = cause.split(":", 2);
  const index = (CAUSE_CATEGORIES as readonly string[]).indexOf(category);
  const head = index === -1 ? CAUSE_CATEGORIES.length : index;
  if (category === "not-addressed") {
    return [head, ...rule.split("+").map((part) => OWED_REASONS.indexOf(part as OwedReason))];
  }
  if (category === "mention-failed") {
    return [head, MENTION_ERROR_CLASSES.indexOf(rule as (typeof MENTION_ERROR_CLASSES)[number])];
  }
  return [head];
}

export function compareCauses(left: string, right: string): number {
  const a = causeKey(left);
  const b = causeKey(right);
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
  const parts = cause.startsWith("not-addressed:")
    ? cause
        .slice("not-addressed:".length)
        .split("+")
        .map((rule) => `not-addressed:${rule}`)
    : [cause];
  const entries = parts.map((part) => causes[part]);
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
export function judgePair(comment: Comment, pair: Pair): Verdict {
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

/** A class a comment can fall in. Each rule includes the negation of every rule before it, so a
 *  comment falls in exactly one; the report refuses a comment that falls in none or in two. */
export interface ClassRule {
  readonly name: string;
  applies(facts: CommentFacts): boolean;
  verdict(facts: CommentFacts): Verdict;
}

function received(facts: CommentFacts): boolean {
  return facts.receipts.some((receipt) => receipt.status === "received");
}

function published(facts: CommentFacts): boolean {
  return facts.comment.publishedAt !== "";
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
    applies: (facts) => !received(facts) && !published(facts),
    verdict: () => notDelivered("never-published"),
  },
  {
    name: "no-target",
    applies: (facts) => !received(facts) && published(facts) && facts.pairs.length === 0,
    verdict: () => notDelivered("no-target"),
  },
  {
    name: "unknown",
    applies: (facts) =>
      !received(facts) &&
      published(facts) &&
      facts.pairs.length > 0 &&
      unknownPairs(facts).length > 0,
    verdict: (facts) => ({
      class: "unknown",
      cause: "",
      why: unknownPairs(facts)
        .map(({ pair, verdict }) => `${pair.target}: ${verdict.why ?? ""}`)
        .join("; "),
    }),
  },
  {
    name: "not delivered, with a cause",
    applies: (facts) =>
      !received(facts) &&
      published(facts) &&
      facts.pairs.length > 0 &&
      unknownPairs(facts).length === 0,
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

export function countVerdicts(
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
    const matched = rules.filter((rule) => rule.applies(facts));
    if (matched.length !== 1) {
      problems.push(
        matched.length === 0
          ? `comment ${comment.id} falls in no class`
          : `comment ${comment.id} falls in ${matched.length} classes: ${matched.map((rule) => rule.name).join(", ")}`
      );
      continue;
    }
    const verdict = (matched[0] as ClassRule).verdict(facts);
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

// ---------------------------------------------------------------- the document

function cell(value: string): string {
  return value === ""
    ? ""
    : value.replaceAll("\\", "\\\\").replaceAll("|", "\\|").replace(/\r?\n/g, " ");
}

function verdictText(verdict: Verdict): string {
  if (verdict.class === "delivered") {
    return verdict.evidence === undefined ? "delivered" : `delivered (${verdict.evidence})`;
  }
  if (verdict.class === "unknown") {
    return verdict.why === undefined ? "unknown" : `unknown (${verdict.why})`;
  }
  return "not delivered";
}

function causeText(verdict: Verdict): string {
  return verdict.cause === "" ? "" : `\`${verdict.cause}\``;
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

/** The markdown document. Deterministic: the same files give the same bytes. */
export function renderReport(
  report: Report,
  traces: readonly { readonly name: string; readonly text: string }[],
  causes: Readonly<Record<string, CauseEntry>> = CAUSES
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
    "`tq receipts` (INFRA-608, theorem-labs/agent-infra `transcript-store/`) gives each pair a " +
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
    "   - `not-running`: Dispatch sent the comment, and the session was not running when " +
      "Dispatch published it. The transcript store gives the session's start and end.",
    "   - `running-no-row`: Dispatch sent the comment to a running session, and the session's " +
      "transcript does not have it.",
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
    const entry = describeCause(cause, causes);
    out.push(
      `| \`${cause}\` | ${counts.causes.get(cause) ?? 0} | ${pairCounts.causes.get(cause) ?? 0} | ` +
        `${cell(entry?.meaning ?? "")} | ${cell(entry?.fix ?? "")} |`
    );
  }
  out.push("");
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
  for (const trace of traces) out.push(`### ${trace.name}`, "", fence(trace.text), "");
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
          const result = its.class === "not-delivered" ? its.cause : verdictText(its);
          const owed = pair.reasons.length === 0 ? "not owed, only sent" : pair.reasons.join(", ");
          return `${pair.target} (${owed}): ${result}`;
        })
        .join("<br>") || "(none)";
    out.push(
      `| ${comment.id} | ${cell(comment.owner)} | ${comment.createdAt} | ${cell(comment.author)} | ` +
        `${comment.shape} | ${cell(verdictText(verdict))} | ${causeText(verdict)} | ${cell(pairs)} |`
    );
  }
  out.push("");
  out.push("## Each pair", "");
  out.push(
    "| Comment | Target | Owed because | Sent by the outbox | Mention attempts | Checked session | Status | Class | Cause |",
    "| --- | --- | --- | --- | --- | --- | --- | --- | --- |"
  );
  for (const judged of report.comments) {
    const id = judged.comment.id;
    if (judged.pairs.length === 0) {
      out.push(
        `| ${id} | (none) |  |  |  |  |  | ${verdictText(judged.verdict)} | ${causeText(judged.verdict)} |`
      );
      continue;
    }
    for (const { pair, verdict } of judged.pairs) {
      out.push(
        `| ${id} | ${pair.target} | ` +
          `${pair.reasons.length === 0 ? "not owed, only sent" : pair.reasons.join(", ")} | ` +
          `${pair.addressed ? "yes" : "no"} | ${cell(mentionText(pair))} | ` +
          `${cell(pair.receipt.checkedSession)} | ${pair.receipt.status} | ` +
          `${cell(verdictText(verdict))} | ${causeText(verdict)} |`
      );
    }
  }
  out.push("");
  out.push("## Unlisted receipts", "");
  out.push(
    "Sessions that took a comment in and are not a pair's session or a session forked from " +
      `one: ${report.unlisted.length}. Each makes its comment delivered.`,
    ""
  );
  for (const receipt of report.unlisted) {
    out.push(
      `- Comment ${receipt.commentId}: ${receipt.target}, first at ` +
        `${receipt.firstReceivedAt}, ${receipt.firstReceipt}`
    );
  }
  return `${out.join("\n").trimEnd()}\n`;
}

// ---------------------------------------------------------------- the command

interface Options {
  readonly comments: string;
  readonly receipts: string;
  readonly counts: string;
  readonly traces: readonly string[];
}

function parseArguments(argv: readonly string[]): Options {
  const { values } = parseArgs({
    args: [...argv],
    options: {
      comments: { type: "string" },
      receipts: { type: "string" },
      counts: { type: "string" },
      trace: { type: "string", multiple: true },
    },
    strict: true,
  });
  const { comments, receipts, counts } = values;
  if (comments === undefined || receipts === undefined || counts === undefined) {
    throw new Error("--comments, --receipts and --counts are required");
  }
  return { comments, receipts, counts, traces: values.trace ?? [] };
}

export async function run(argv: readonly string[]): Promise<string> {
  const options = parseArguments(argv);
  const [comments, receipts, counts, ...traceTexts] = await Promise.all(
    [options.comments, options.receipts, options.counts, ...options.traces].map((path) =>
      readFile(path, "utf8")
    )
  );
  const traces = options.traces.map((path, index) => ({
    name: basename(path),
    text: traceTexts[index] ?? "",
  }));
  const report = judge({
    comments: comments ?? "",
    receipts: receipts ?? "",
    counts: counts ?? "",
  });
  return renderReport(report, traces);
}

if (import.meta.main) {
  try {
    process.stdout.write(await run(Bun.argv.slice(2)));
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`);
    process.exit(1);
  }
}
