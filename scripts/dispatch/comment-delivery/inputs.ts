// The comment delivery audit's three input files (#2366), read and checked: comments.csv,
// receipts.tsv and receipts-counts.txt, as report.ts gives them. Every problem is pushed to the
// caller's list; a row that cannot be read makes no model value.

/** comments.csv's columns, in order: the contract in deliveryaudit/README.md. */
const COMMENT_COLUMNS = [
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
/** receipts.tsv's columns, in order: the output contract of `tq receipts`. */
const RECEIPT_COLUMNS = [
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
const SHAPES = ["ask_thread", "reply", "anchored", "conversation"] as const;
const MENTION_STATES = ["", "pending", "sent", "failed"] as const;
const MENTION_MODES = ["btw", "aside", "steer"] as const;
const STATUSES = ["received", "not_received", "unknown"] as const;
const UNKNOWN_REASONS = ["session_not_in_store", "role", "btw"] as const;
/** Why a target was owed a comment, in the order a `not-addressed` cause names them. */
const OWED_REASONS = ["claimant", "route", "follower", "asker", "mention"] as const;

type Shape = (typeof SHAPES)[number];
type MentionMode = (typeof MENTION_MODES)[number];
type OwedReason = (typeof OWED_REASONS)[number];
export type Status = (typeof STATUSES)[number];
export type UnknownReason = (typeof UNKNOWN_REASONS)[number];

function isOneOf<T extends string>(values: readonly T[], value: string): value is T {
  return (values as readonly string[]).includes(value);
}

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

interface RawRecord {
  readonly fields: string[];
  readonly line: number;
}

/** RFC 4180 records, as psql --csv writes them: a quoted field may hold commas, `""` and
 *  newlines. `line` is the line the record starts on. */
function csvRecords(text: string, file: string, problems: string[]): RawRecord[] {
  const records: RawRecord[] = [];
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
function tsvRecords(text: string): RawRecord[] {
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
  records: readonly RawRecord[],
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

/** receipts-counts.txt's keys, one line each, in order: `tq receipts`' output contract. */
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
export type CountKey = (typeof COUNT_KEYS)[number];
export type NumberKey = Exclude<CountKey, "from" | "to">;

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

export function compare(left: string, right: string): number {
  return left < right ? -1 : left > right ? 1 : 0;
}

/** A comma-joined list of comments.csv, one element per mention attempt; empty for none. */
function attempts(value: string): string[] {
  return value === "" ? [] : value.split(",");
}

/** A row's mention attempts: its modes, and its envelope ids, one element per attempt. An
 *  attempt with no envelope id is an empty element, so both lists have one element per attempt;
 *  a single attempt with none is the empty string. */
function mentionAttempts(row: CommentRow): { modes: string[]; envelopes: string[] } {
  const modes = attempts(row.mention_modes);
  const envelopes =
    modes.length === 0 ? attempts(row.mention_envelope_ids) : row.mention_envelope_ids.split(",");
  return { modes, envelopes };
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
  readonly mentionModes: readonly MentionMode[];
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
  readonly shape: Shape;
  /** Sorted by target. Empty for a comment with no target. */
  readonly pairs: readonly Pair[];
}

type PairRow = Omit<Pair, "receipt"> & { readonly line: number };
export interface CommentDraft extends Omit<Comment, "pairs"> {
  readonly pairs: readonly PairRow[];
}

export function commentsFrom(rows: readonly CommentRow[], problems: string[]): CommentDraft[] {
  const byId = new Map<string, CommentRow[]>();
  for (const row of rows) {
    const where = `comments.csv line ${row.line} (comment ${row.comment_id})`;
    if (row.comment_id === "") problems.push(`comments.csv line ${row.line}: no comment_id`);
    if (!isOneOf(SHAPES, row.shape)) problems.push(`${where}: shape "${row.shape}"`);
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
    if (!isOneOf(MENTION_STATES, row.mention_state)) {
      problems.push(`${where}: mention_state "${row.mention_state}"`);
    }
    const { modes, envelopes } = mentionAttempts(row);
    if (!modes.every((mode) => isOneOf(MENTION_MODES, mode))) {
      problems.push(`${where}: mention_modes "${row.mention_modes}"`);
    }
    if ((row.mention_state === "") !== (modes.length === 0)) {
      problems.push(
        `${where}: mention_state "${row.mention_state}" with mention_modes "${row.mention_modes}"`
      );
    }
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
      const known =
        row.owed_reasons === "addressed-only" ||
        reasons.every((reason) => isOneOf(OWED_REASONS, reason));
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
    // A shape or a mention mode that is none of the known ones is refused above, and makes no
    // comment or pair.
    if (!isOneOf(SHAPES, first.shape)) continue;
    const pairs = group
      .flatMap((row): PairRow[] => {
        const { modes, envelopes } = mentionAttempts(row);
        if (row.target === "" || !modes.every((mode) => isOneOf(MENTION_MODES, mode))) return [];
        const owed = row.owed_reasons.split(";");
        return [
          {
            line: row.line,
            target: row.target,
            reasons: OWED_REASONS.filter((reason) => owed.includes(reason)),
            addressed: row.addressed === "true",
            mentionSessionId: row.mention_session_id,
            mentionEnvelopeIds: envelopes,
            mentionBtwReplyId: row.mention_btw_reply_id,
            mentionModes: modes,
            mentionState: row.mention_state,
            mentionError: row.mention_error,
          },
        ];
      })
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

export function receiptsFrom(rows: readonly ReceiptRow[], problems: string[]): Receipt[] {
  // `tq receipts` sorts the rows by comment_id, then listed (true first), then target.
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
    const status = isOneOf(STATUSES, row.status) ? row.status : undefined;
    const reason = isOneOf(UNKNOWN_REASONS, row.unknown_reason)
      ? row.unknown_reason
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
