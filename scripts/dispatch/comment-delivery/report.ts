import { readFile } from "node:fs/promises";
import { basename } from "node:path";
import { parseArgs } from "node:util";
import { RECEIPT_TIMEOUT_CAUSE } from "../../../packages/contracts/src/dispatch-api";

// Turns the three files of the comment delivery audit (INFRA-596) into a document for people:
// comments.csv from packages/envoy/internal/dispatch/deliveryaudit/comments.sql, and received.tsv
// and sessions.tsv from received.sql and sessions.sql beside this file. scripts/dispatch/README.md
// ("Comment delivery") gives the run. Input that does not agree with itself is refused (exit 1,
// every problem on stderr, nothing on stdout), never counted.

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
  "target_kind",
  "owed_reasons",
  "addressed",
  "mention_state",
  "mention_error",
  "route_used",
  "claim_used",
  "route_source",
  "mention_envelope_id",
  "mention_delivery",
] as const;
export const RECEIVED_COLUMNS = [
  "session_id",
  "host",
  "ts",
  "raw_file",
  "raw_line",
  "envelope_id",
  "event_id",
  "comment_id",
] as const;
export const SESSION_COLUMNS = [
  "session_id",
  "host",
  "started_at",
  "ended_at",
  "parent_session",
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
const MENTION_DELIVERIES: Readonly<Record<string, true>> = {
  "": true,
  btw: true,
  aside: true,
  steer: true,
};
/** Why a target was owed a comment, in the order a `not-addressed` cause names them. */
const OWED_REASONS = ["claimant", "route", "follower", "asker", "mention"] as const;
const AGENT_PREFIX = "notifications.agent.";
const ROLE_PREFIX = "notifications.role.";

type OwedReason = (typeof OWED_REASONS)[number];
type Row<T extends readonly string[]> = { readonly [K in T[number]]: string } & {
  readonly line: number;
};
export type CommentRow = Row<typeof COMMENT_COLUMNS>;
export type ReceivedRow = Row<typeof RECEIVED_COLUMNS>;
export type SessionRow = Row<typeof SESSION_COLUMNS>;

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

/** ClickHouse's TSVWithNames: one record a line, fields split by tabs, backslash escapes. A
 *  NULL (`\N`) reads as empty: no column of these queries has one. */
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
  problems: string[]
): Row<T>[] {
  const [header, ...body] = records;
  if (header === undefined || header.fields.join(",") !== columns.join(",")) {
    problems.push(
      `${file}: the header is "${header?.fields.join(",") ?? ""}", not "${columns.join(",")}"`
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
  return table(
    csvRecords(text, "comments.csv", problems),
    COMMENT_COLUMNS,
    "comments.csv",
    problems
  );
}

export function parseReceivedTsv(text: string, problems: string[]): ReceivedRow[] {
  return table(tsvRecords(text), RECEIVED_COLUMNS, "received.tsv", problems);
}

export function parseSessionsTsv(text: string, problems: string[]): SessionRow[] {
  return table(tsvRecords(text), SESSION_COLUMNS, "sessions.tsv", problems);
}

/** A UTC time as microseconds since the epoch, or undefined when it is not `YYYY-MM-DDTHH:MM:SS
 *  [.fraction]Z`. */
function instant(value: string): number | undefined {
  const match = /^(\d{4})-(\d\d)-(\d\d)T(\d\d):(\d\d):(\d\d)(?:\.(\d{1,9}))?Z$/.exec(value);
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

/** A session id without the `@<host>:<path hash>` the store adds to a diverged copy. */
function baseSession(id: string): string {
  const at = id.indexOf("@");
  return at === -1 ? id : id.slice(0, at);
}

function compare(left: string, right: string): number {
  return left < right ? -1 : left > right ? 1 : 0;
}

// ---------------------------------------------------------------- the model

export interface Target {
  readonly target: string;
  readonly kind: "session" | "role";
  /** The session's id, for a session target. */
  readonly session: string;
  /** Why it was owed the comment, in OWED_REASONS order; empty when only addressed. */
  readonly reasons: readonly OwedReason[];
  readonly addressed: boolean;
  readonly mentionState: string;
  readonly mentionError: string;
  /** The envelope id the listener minted for the mention's newest attempt; empty when none. */
  readonly mentionEnvelopeId: string;
  /** That attempt's mode: btw, aside or steer; empty when the target has no mention attempt. */
  readonly mentionDelivery: string;
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
  /** Sorted by subject. Empty for a comment with no target. */
  readonly targets: readonly Target[];
}

export interface Receipt {
  readonly session: string;
  readonly host: string;
  readonly ts: string;
  readonly rawFile: string;
  readonly rawLine: string;
  readonly envelopeId: string;
  readonly eventId: string;
  readonly commentId: string;
}

export interface Session {
  readonly id: string;
  readonly startedUs: number;
  /** Undefined while the session's file is still being written. */
  readonly endedUs: number | undefined;
  readonly parents: readonly string[];
}

function commentsFrom(rows: readonly CommentRow[], problems: string[]): Comment[] {
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
    if (MENTION_DELIVERIES[row.mention_delivery] !== true) {
      problems.push(`${where}: mention_delivery "${row.mention_delivery}"`);
    }
    if ((row.mention_state === "") !== (row.mention_delivery === "")) {
      problems.push(
        `${where}: mention_state "${row.mention_state}" with mention_delivery "${row.mention_delivery}"`
      );
    }
    if (row.target === "") {
      if (row.target_kind !== "" || row.owed_reasons !== "" || row.addressed !== "false") {
        problems.push(`${where}: a row with no target names a kind, a reason or an address`);
      }
    } else {
      const kind = row.target.startsWith(AGENT_PREFIX)
        ? "session"
        : row.target.startsWith(ROLE_PREFIX)
          ? "role"
          : "";
      if (kind === "" || kind !== row.target_kind) {
        problems.push(`${where}: target ${row.target} with target_kind "${row.target_kind}"`);
      }
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
  const comments: Comment[] = [];
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
    const targets = group
      .filter((row) => row.target !== "")
      .map(
        (row): Target => ({
          target: row.target,
          kind: row.target_kind === "role" ? "role" : "session",
          session:
            row.target_kind === "session" ? baseSession(row.target.slice(AGENT_PREFIX.length)) : "",
          reasons: OWED_REASONS.filter((reason) => row.owed_reasons.split(";").includes(reason)),
          addressed: row.addressed === "true",
          mentionState: row.mention_state,
          mentionError: row.mention_error,
          mentionEnvelopeId: row.mention_envelope_id,
          mentionDelivery: row.mention_delivery,
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
      targets,
    });
  }
  return comments.sort(
    (left, right) => compare(left.createdAt, right.createdAt) || compare(left.id, right.id)
  );
}

function isOwedReason(value: string): value is OwedReason {
  return (OWED_REASONS as readonly string[]).includes(value);
}

function receiptsFrom(rows: readonly ReceivedRow[], problems: string[]): Receipt[] {
  const seen = new Map<string, number>();
  const receipts: Receipt[] = [];
  for (const row of rows) {
    const session = baseSession(row.session_id);
    const where = `received.tsv line ${row.line}`;
    const key = `${session}\t${row.raw_file}\t${row.raw_line}`;
    const earlier = seen.get(key);
    if (earlier !== undefined) {
      problems.push(
        `received.tsv lines ${earlier} and ${row.line}: session ${session}, ${row.raw_file}:${row.raw_line} twice`
      );
    }
    seen.set(key, row.line);
    if (session === "") problems.push(`${where}: no session_id`);
    if (instant(row.ts) === undefined) problems.push(`${where}: ts "${row.ts}"`);
    const dispatchId = /^dispatch-([0-9]+)$/.exec(row.envelope_id)?.[1] ?? "";
    if (row.envelope_id === "" || row.event_id !== dispatchId) {
      problems.push(
        `${where}: envelope_id "${row.envelope_id}" does not give event_id "${row.event_id}"`
      );
    }
    receipts.push({
      session,
      host: row.host,
      ts: row.ts,
      rawFile: row.raw_file,
      rawLine: row.raw_line,
      envelopeId: row.envelope_id,
      eventId: row.event_id,
      commentId: row.comment_id,
    });
  }
  return receipts;
}

/** One session per base id. The diverged copies of one session file (`<id>@<host>:<hash>`)
 *  are that one session, so they merge: the earliest start, the latest end (none when a copy
 *  has none), every parent. The same id twice is a duplicate. */
function sessionsFrom(rows: readonly SessionRow[], problems: string[]): Map<string, Session> {
  const lines = new Map<string, number>();
  const sessions = new Map<string, Session>();
  for (const row of rows) {
    const where = `sessions.tsv line ${row.line}`;
    const earlier = lines.get(row.session_id);
    if (earlier !== undefined) {
      problems.push(
        `sessions.tsv lines ${earlier} and ${row.line}: session ${row.session_id} twice`
      );
    }
    lines.set(row.session_id, row.line);
    const id = baseSession(row.session_id);
    const startedUs = instant(row.started_at);
    const endedUs = row.ended_at === "" ? undefined : instant(row.ended_at);
    if (id === "") problems.push(`${where}: no session_id`);
    if (startedUs === undefined) problems.push(`${where}: started_at "${row.started_at}"`);
    if (row.ended_at !== "" && endedUs === undefined) {
      problems.push(`${where}: ended_at "${row.ended_at}"`);
    }
    if (startedUs === undefined) continue;
    const parents = row.parent_session
      .split(";")
      .map(baseSession)
      .filter((parent) => parent !== "" && parent !== id);
    const other = sessions.get(id);
    sessions.set(
      id,
      other === undefined
        ? { id, startedUs, endedUs, parents: [...new Set(parents)].sort(compare) }
        : {
            id,
            startedUs: Math.min(other.startedUs, startedUs),
            endedUs:
              other.endedUs === undefined || endedUs === undefined
                ? undefined
                : Math.max(other.endedUs, endedUs),
            parents: [...new Set([...other.parents, ...parents])].sort(compare),
          }
    );
  }
  return sessions;
}

// ---------------------------------------------------------------- classes and causes

export type ClassName = "delivered" | "not-delivered" | "unknown";

export interface Verdict {
  readonly class: ClassName;
  /** The cause of a not-delivered verdict; empty otherwise. */
  readonly cause: string;
  /** Why a target is unknown: it is a role, or the store does not hold the session. */
  readonly why?: string;
  /** `btw-sent` on a delivered verdict that rests on Dispatch's record of a mention it sent as a
   *  BTW, not on a transcript row: the BTW side turn writes none. */
  readonly note?: "btw-sent";
}

const DELIVERED: Verdict = { class: "delivered", cause: "" };
const BTW_SENT: Verdict = { class: "delivered", cause: "", note: "btw-sent" };

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
    meaning: "Dispatch did not send the comment to the session that the issue's route names.",
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
      "The comment mentions the session, and Dispatch has no sent or failed delivery of the " +
      "mention to it.",
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
      "Dispatch tried to deliver the mention, and it failed with another error. The target " +
      "rows give the error.",
    fix: "No fix yet. Each such error needs its own fix.",
  },
  "not-running": {
    meaning:
      "Dispatch sent the comment to the session, and no copy, fork or resume of the session " +
      "was running when Dispatch published it.",
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

/** What the classification reads of the sessions and their receipts. */
export interface Evidence {
  readonly sessions: ReadonlyMap<string, Session>;
  /** A session and every session forked from it or that it was forked from, through any chain. */
  family(session: string): ReadonlySet<string>;
}

function evidence(sessions: ReadonlyMap<string, Session>): Evidence {
  const children = new Map<string, string[]>();
  for (const session of sessions.values()) {
    for (const parent of session.parents) {
      const list = children.get(parent);
      if (list === undefined) children.set(parent, [session.id]);
      else list.push(session.id);
    }
  }
  const families = new Map<string, ReadonlySet<string>>();
  return {
    sessions,
    family(session) {
      const cached = families.get(session);
      if (cached !== undefined) return cached;
      const family = new Set([session]);
      const walk = (next: (id: string) => readonly string[]): void => {
        const stack = [session];
        for (let id = stack.pop(); id !== undefined; id = stack.pop()) {
          for (const other of next(id)) {
            if (family.has(other)) continue;
            family.add(other);
            stack.push(other);
          }
        }
      };
      walk((id) => sessions.get(id)?.parents ?? []);
      walk((id) => children.get(id) ?? []);
      families.set(session, family);
      return family;
    },
  };
}

/** One target of a comment, judged alone: delivered when the session or one of its family has a
 *  receipt, else delivered on Dispatch's record (`btw-sent`) when a mention of the comment was
 *  sent to the session as a BTW, else never-published, else unknown (a role, or a session the
 *  store does not hold), else its one cause. */
export function judgeTarget(
  comment: Comment,
  target: Target,
  receipts: readonly Receipt[],
  facts: Evidence
): Verdict {
  const family = target.kind === "session" ? facts.family(target.session) : new Set<string>();
  if (receipts.some((receipt) => family.has(receipt.session))) return DELIVERED;
  const mentioned = target.reasons.includes("mention");
  // A BTW runs as a side turn, which writes no transcript row (packages/pi-envoy/extensions/
  // envoy.ts), so Dispatch's record of the sent mention is the only evidence there is.
  if (mentioned && target.mentionState === "sent" && target.mentionDelivery === "btw") {
    return BTW_SENT;
  }
  if (comment.publishedAt === "") return notDelivered("never-published");
  if (target.kind === "role") return { class: "unknown", cause: "", why: "a role" };
  if (!facts.sessions.has(target.session)) {
    return { class: "unknown", cause: "", why: "the store does not hold this session" };
  }
  if (!target.addressed && !(mentioned && target.mentionState === "sent")) {
    if (mentioned && target.mentionState === "failed") {
      return notDelivered(`mention-failed:${mentionErrorClass(target.mentionError)}`);
    }
    return notDelivered(`not-addressed:${target.reasons.join("+")}`);
  }
  const at = comment.publishedUs ?? 0;
  const running = [...family].some((id) => {
    const session = facts.sessions.get(id);
    return (
      session !== undefined &&
      session.startedUs <= at &&
      (session.endedUs === undefined || at <= session.endedUs)
    );
  });
  return notDelivered(running ? "running-no-row" : "not-running");
}

/** What one comment's class rule reads. */
export interface CommentFacts {
  readonly comment: Comment;
  /** Every receipt of the comment, by any session. */
  readonly receipts: readonly Receipt[];
  readonly targets: readonly { readonly target: Target; readonly verdict: Verdict }[];
}

/** A class a comment can fall in. Each rule includes the negation of every rule before it, so a
 *  comment falls in exactly one; the report refuses a comment that falls in none or in two. */
export interface ClassRule {
  readonly name: string;
  applies(facts: CommentFacts): boolean;
  verdict(facts: CommentFacts): Verdict;
}

function received(facts: CommentFacts): boolean {
  return facts.receipts.length > 0;
}

function heardByBtw(facts: CommentFacts): boolean {
  return facts.targets.some(({ verdict }) => verdict.note === "btw-sent");
}

/** No transcript has the comment, and Dispatch has no record of a BTW mention of it sent. */
function unconfirmed(facts: CommentFacts): boolean {
  return !received(facts) && !heardByBtw(facts);
}

function published(facts: CommentFacts): boolean {
  return facts.comment.publishedAt !== "";
}

function unheldTargets(facts: CommentFacts): readonly Target[] {
  return facts.targets
    .filter(({ verdict }) => verdict.class === "unknown")
    .map(({ target }) => target);
}

/** The first cause, in CAUSES order, of the comment's targets. */
function primaryCause(facts: CommentFacts): string {
  const causes = facts.targets
    .map(({ verdict }) => verdict.cause)
    .filter((cause) => cause !== "")
    .sort(compareCauses);
  return causes[0] ?? "";
}

export const CLASS_RULES: readonly ClassRule[] = [
  { name: "delivered", applies: received, verdict: () => DELIVERED },
  {
    name: "delivered on Dispatch's record of a BTW mention",
    applies: (facts) => !received(facts) && heardByBtw(facts),
    verdict: () => BTW_SENT,
  },
  {
    name: "never-published",
    applies: (facts) => unconfirmed(facts) && !published(facts),
    verdict: () => notDelivered("never-published"),
  },
  {
    name: "no-target",
    applies: (facts) => unconfirmed(facts) && published(facts) && facts.targets.length === 0,
    verdict: () => notDelivered("no-target"),
  },
  {
    name: "unknown",
    applies: (facts) =>
      unconfirmed(facts) &&
      published(facts) &&
      facts.targets.length > 0 &&
      unheldTargets(facts).length > 0,
    verdict: () => ({ class: "unknown", cause: "" }),
  },
  {
    name: "not delivered, with a cause",
    applies: (facts) =>
      unconfirmed(facts) &&
      published(facts) &&
      facts.targets.length > 0 &&
      unheldTargets(facts).length === 0,
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
  readonly received: string;
  readonly sessions: string;
}

export interface ReportOptions {
  readonly rules?: readonly ClassRule[];
  readonly causes?: Readonly<Record<string, CauseEntry>>;
}

export interface Report {
  readonly comments: readonly Judged[];
  readonly commentCounts: Counts;
  readonly targetCounts: Counts;
  /** Receipts that name a comment or event comments.csv does not list. */
  readonly unlisted: readonly Receipt[];
  /** Receipts that name no comment and no event, and whose envelope id is no target row's
   *  mention_envelope_id. */
  readonly untied: readonly Receipt[];
}

/** Reads and judges the three files, or throws ReportRefusal naming every problem. */
export function judge(input: ReportInput, options: ReportOptions = {}): Report {
  const rules = options.rules ?? CLASS_RULES;
  const causes = options.causes ?? CAUSES;
  const problems: string[] = [];
  const comments = commentsFrom(parseCommentsCsv(input.comments, problems), problems);
  const receipts = receiptsFrom(parseReceivedTsv(input.received, problems), problems);
  const sessions = sessionsFrom(parseSessionsTsv(input.sessions, problems), problems);
  if (problems.length > 0) throw new ReportRefusal(problems);

  const byId = new Map(comments.map((comment) => [comment.id, comment]));
  const byEvent = new Map<string, Comment>();
  for (const comment of comments) {
    if (comment.eventId === "") continue;
    const other = byEvent.get(comment.eventId);
    if (other !== undefined) {
      problems.push(
        `comments.csv: comments ${other.id} and ${comment.id} name one event ${comment.eventId}`
      );
    }
    byEvent.set(comment.eventId, comment);
  }
  // A mention's delivery of an ask-thread reply names neither the comment nor its event: only
  // the envelope id the listener minted, which Dispatch records on the mention's attempt.
  const byEnvelope = new Map<string, Comment>();
  for (const comment of comments) {
    for (const target of comment.targets) {
      if (target.mentionEnvelopeId === "") continue;
      const other = byEnvelope.get(target.mentionEnvelopeId);
      if (other !== undefined && other.id !== comment.id) {
        problems.push(
          `comments.csv: comments ${other.id} and ${comment.id} name one mention envelope ` +
            target.mentionEnvelopeId
        );
      }
      byEnvelope.set(target.mentionEnvelopeId, comment);
    }
  }
  const receiptsOf = new Map<string, Receipt[]>();
  const unlisted: Receipt[] = [];
  const untied: Receipt[] = [];
  for (const receipt of receipts) {
    const named = receipt.commentId === "" ? undefined : byId.get(receipt.commentId);
    const byItsEvent = receipt.eventId === "" ? undefined : byEvent.get(receipt.eventId);
    // A receipt that names both must name a pair comments.csv holds, whichever of them it lists.
    if (
      (named !== undefined && receipt.eventId !== "" && named.eventId !== receipt.eventId) ||
      (byItsEvent !== undefined && receipt.commentId !== "" && byItsEvent.id !== receipt.commentId)
    ) {
      problems.push(
        `received.tsv: ${receipt.rawFile}:${receipt.rawLine} names comment ${receipt.commentId} ` +
          `and event ${receipt.eventId}, which comments.csv does not pair`
      );
      continue;
    }
    const names = receipt.commentId !== "" || receipt.eventId !== "";
    const comment = named ?? byItsEvent ?? (names ? undefined : byEnvelope.get(receipt.envelopeId));
    if (comment !== undefined) {
      const list = receiptsOf.get(comment.id);
      if (list === undefined) receiptsOf.set(comment.id, [receipt]);
      else list.push(receipt);
    } else if (names) {
      unlisted.push(receipt);
    } else {
      untied.push(receipt);
    }
  }

  const facts = evidence(sessions);
  const judged: Judged[] = [];
  for (const comment of comments) {
    const its = receiptsOf.get(comment.id) ?? [];
    const commentFacts: CommentFacts = {
      comment,
      receipts: its,
      targets: comment.targets.map((target) => ({
        target,
        verdict: judgeTarget(comment, target, its, facts),
      })),
    };
    const matched = rules.filter((rule) => rule.applies(commentFacts));
    if (matched.length !== 1) {
      problems.push(
        matched.length === 0
          ? `comment ${comment.id} falls in no class`
          : `comment ${comment.id} falls in ${matched.length} classes: ${matched.map((rule) => rule.name).join(", ")}`
      );
      continue;
    }
    const verdict = (matched[0] as ClassRule).verdict(commentFacts);
    if (verdict.class === "not-delivered" && verdict.cause === "") {
      problems.push(`comment ${comment.id} is not delivered and has no cause`);
    }
    if (verdict.class === "unknown" && unheldTargets(commentFacts).length === 0) {
      problems.push(
        `comment ${comment.id} is unknown and names no role or session the store does not hold`
      );
    }
    judged.push({ ...commentFacts, verdict });
  }

  const commentVerdicts = judged.map((comment) => comment.verdict);
  const targetVerdicts = judged.flatMap((comment) =>
    comment.targets.length === 0
      ? [comment.verdict]
      : comment.targets.map((target) => target.verdict)
  );
  for (const verdict of [...commentVerdicts, ...targetVerdicts]) {
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
  const targetCounts = countVerdicts(targetVerdicts, causes);
  problems.push(...identityProblems(commentCounts, "comments"));
  problems.push(...identityProblems(targetCounts, "targets"));
  if (problems.length > 0) throw new ReportRefusal(problems);
  const byPlace = (left: Receipt, right: Receipt): number =>
    compare(left.session, right.session) ||
    compare(left.rawFile, right.rawFile) ||
    Number(left.rawLine) - Number(right.rawLine);
  return {
    comments: judged,
    commentCounts,
    targetCounts,
    unlisted: unlisted.sort(byPlace),
    untied: untied.sort(byPlace),
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
    return verdict.note === undefined ? "delivered" : `delivered (${verdict.note})`;
  }
  if (verdict.class === "unknown")
    return verdict.why === undefined ? "unknown" : `unknown (${verdict.why})`;
  return "not delivered";
}

function fence(text: string): string {
  const longest = Math.max(0, ...[...text.matchAll(/`+/g)].map((match) => match[0].length));
  const marks = "`".repeat(Math.max(3, longest + 1));
  return `${marks}text\n${text}${text.endsWith("\n") || text === "" ? "" : "\n"}${marks}`;
}

function receiptLine(receipt: Receipt): string {
  const names = [
    receipt.commentId === "" ? "" : `comment ${receipt.commentId}`,
    receipt.eventId === "" ? "" : `event ${receipt.eventId}`,
  ].filter((name) => name !== "");
  return (
    `- ${receipt.ts}, session ${receipt.session} on ${receipt.host}, ` +
    `${receipt.rawFile}:${receipt.rawLine}, envelope ${receipt.envelopeId}` +
    (names.length === 0 ? "" : `, ${names.join(", ")}`)
  );
}

/** The markdown document. Deterministic: the same files give the same bytes. */
export function renderReport(
  report: Report,
  traces: readonly { readonly name: string; readonly text: string }[],
  causes: Readonly<Record<string, CauseEntry>> = CAUSES
): string {
  const { commentCounts: counts, targetCounts } = report;
  const first = report.comments[0]?.comment.createdAt ?? "";
  const last = report.comments.at(-1)?.comment.createdAt ?? "";
  const out: string[] = [];
  out.push("# Did people's Dispatch comments reach the agent sessions?", "");
  out.push(identityLine(counts), "");
  const btwSent = report.comments.filter(({ verdict }) => verdict.note === "btw-sent").length;
  out.push(
    `- Comments that people wrote: ${counts.total}` +
      (counts.total === 0 ? "." : `, created from ${first} to ${last}.`),
    `- Delivered: ${counts.delivered}, of which btw-sent ${btwSent}: Dispatch's record of a ` +
      "mention it sent as a BTW, not a transcript row.",
    `- Not delivered: ${counts.notDelivered}.`,
    `- Unknown: ${counts.unknown}.`,
    "",
    "Each comment has one target or more: a session or role it was owed, or that Dispatch sent " +
      "it to. The same classes, applied to each target alone:",
    "",
    identityLine(targetCounts),
    ""
  );
  out.push("## What the classes mean", "");
  out.push(
    "The report puts each comment in one class. It checks the classes in this order, and it " +
      "uses the first class that applies.",
    "",
    "1. **Delivered.** The transcript of at least one agent session has the comment. A " +
      "session that follows the issue counts too. A transcript row names the comment's id or " +
      "its event's id. A mention's delivery of a reply on an ask names neither, so its row " +
      "counts when its envelope id is the envelope id that Dispatch recorded for the mention's " +
      "delivery.",
    "2. **Delivered: btw-sent.** No transcript has the comment, and Dispatch sent a mention of " +
      "the comment to a session as a BTW: the delivery has the state `sent`. A BTW runs as a " +
      "side turn, and the side turn adds nothing to the transcript. So this class is Dispatch's " +
      "record of the delivery, not a transcript row. A target is `btw-sent` on the same rule " +
      "when no transcript of its session has the comment.",
    "3. **Not delivered: never-published.** Dispatch's outbox never published the comment's " +
      "event.",
    "4. **Not delivered: no-target.** No session was owed the comment, and Dispatch sent it to " +
      "no session or role.",
    "5. **Unknown.** Dispatch sent the comment to a role, or a session that the transcript " +
      "store does not hold was owed it or got it. The transcript store holds only the Oh My Pi " +
      "sessions of the two dev boxes. It does not hold Claude Code sessions or the sessions " +
      "of Legion pods. So the report cannot tell if these targets got the comment. The row " +
      "names each such target.",
    "6. **Not delivered, with one cause.** The report gives the first cause that applies to " +
      "a target of the comment, in this order:",
    "   - `not-addressed:<rule>`: Dispatch did not send the comment to a session that was " +
      "owed it. The rule says why the session was owed it: claimant, route, follower, asker " +
      "or mention. A mention counts as sent when Dispatch's delivery of the mention has the " +
      "state `sent`.",
    "   - `mention-failed:<error>`: Dispatch tried to deliver a mention, and the delivery " +
      "failed. The error is `session-not-live`, `receipt-timeout` or `other`.",
    "   - `not-running`: Dispatch sent the comment, and the session was not running when " +
      "Dispatch published it.",
    "   - `running-no-row`: Dispatch sent the comment to a running session, and the session's " +
      "transcript does not have it.",
    "",
    "A session includes each session that was forked from it, and each session that it was " +
      "forked from, through any number of forks. A receipt by any of them is a receipt by the " +
      "session. The copies of one session file that the store keeps apart are one session.",
    ""
  );
  out.push("## Causes and their fixes", "");
  out.push(
    "| Cause | Comments | Targets | What it means | Fix |",
    "| --- | --- | --- | --- | --- |"
  );
  const allCauses = [...new Set([...counts.causes.keys(), ...targetCounts.causes.keys()])].sort(
    compareCauses
  );
  for (const cause of allCauses) {
    const entry = describeCause(cause, causes);
    out.push(
      `| \`${cause}\` | ${counts.causes.get(cause) ?? 0} | ${targetCounts.causes.get(cause) ?? 0} | ` +
        `${cell(entry?.meaning ?? "")} | ${cell(entry?.fix ?? "")} |`
    );
  }
  out.push("");
  out.push("## Traces", "");
  if (traces.length === 0) out.push("No trace was given.", "");
  for (const trace of traces) out.push(`### ${trace.name}`, "", fence(trace.text), "");
  out.push("## Each comment", "");
  out.push(
    "| Comment | On | Created | Author | Shape | Class | Cause | Targets |",
    "| --- | --- | --- | --- | --- | --- | --- | --- |"
  );
  for (const judged of report.comments) {
    const { comment, verdict } = judged;
    const targets =
      judged.targets
        .map(({ target, verdict: its }) => {
          const result = its.class === "not-delivered" ? its.cause : verdictText(its);
          const owed =
            target.reasons.length === 0 ? "not owed, only sent" : target.reasons.join(", ");
          return `${target.target} (${owed}): ${result}`;
        })
        .join("<br>") || "(none)";
    out.push(
      `| ${comment.id} | ${cell(comment.owner)} | ${comment.createdAt} | ${cell(comment.author)} | ` +
        `${comment.shape} | ${verdictText(verdict)} | ${verdict.cause === "" ? "" : `\`${verdict.cause}\``} | ` +
        `${cell(targets)} |`
    );
  }
  out.push("");
  out.push("## Each target of each comment", "");
  out.push(
    "| Comment | Target | Owed because | Sent by Dispatch | Mention delivery | Class | Cause |",
    "| --- | --- | --- | --- | --- | --- | --- |"
  );
  for (const judged of report.comments) {
    const id = judged.comment.id;
    if (judged.targets.length === 0) {
      out.push(
        `| ${id} | (none) |  |  |  | ${verdictText(judged.verdict)} | ` +
          `${judged.verdict.cause === "" ? "" : `\`${judged.verdict.cause}\``} |`
      );
      continue;
    }
    for (const { target, verdict } of judged.targets) {
      // `<state> (<mode>)`, then `: <error>` when the attempt has one; empty with no attempt.
      const mode = target.mentionDelivery === "" ? "" : ` (${target.mentionDelivery})`;
      const error = target.mentionError === "" ? "" : `: ${target.mentionError}`;
      out.push(
        `| ${id} | ${target.target} | ` +
          `${target.reasons.length === 0 ? "not owed, only sent" : target.reasons.join(", ")} | ` +
          `${target.addressed ? "yes" : "no"} | ` +
          `${cell(`${target.mentionState}${mode}${error}`)} | ` +
          `${verdictText(verdict)} | ` +
          `${verdict.cause === "" ? "" : `\`${verdict.cause}\``} |`
      );
    }
  }
  out.push("");
  out.push("## Receipts that the report did not count", "");
  out.push(
    `Receipts that name a comment or an event that comments.csv does not list: ` +
      `${report.unlisted.length}. Such a receipt is of a person's comment from outside the window.`,
    ""
  );
  for (const receipt of report.unlisted) out.push(receiptLine(receipt));
  if (report.unlisted.length > 0) out.push("");
  out.push(
    `Receipts that name no comment and no event, and whose envelope id is not an envelope id ` +
      `that comments.csv records for a mention's delivery: ${report.untied.length}. Such a ` +
      "receipt is a mention's delivery of a reply on an ask from outside the window, or of an " +
      "earlier attempt of the mention than its newest. comments.csv gives only the newest " +
      "attempt of each mention.",
    ""
  );
  for (const receipt of report.untied) out.push(receiptLine(receipt));
  if (report.untied.length > 0) out.push("");
  return `${out.join("\n").trimEnd()}\n`;
}

// ---------------------------------------------------------------- the command

interface Options {
  readonly comments: string;
  readonly received: string;
  readonly sessions: string;
  readonly traces: readonly string[];
}

function parseArguments(argv: readonly string[]): Options {
  const { values } = parseArgs({
    args: [...argv],
    options: {
      comments: { type: "string" },
      received: { type: "string" },
      sessions: { type: "string" },
      trace: { type: "string", multiple: true },
    },
    strict: true,
  });
  const { comments, received, sessions } = values;
  if (comments === undefined || received === undefined || sessions === undefined) {
    throw new Error("--comments, --received and --sessions are required");
  }
  return { comments, received, sessions, traces: values.trace ?? [] };
}

export async function run(argv: readonly string[]): Promise<string> {
  const options = parseArguments(argv);
  const [comments, received, sessions, ...traceTexts] = await Promise.all(
    [options.comments, options.received, options.sessions, ...options.traces].map((path) =>
      readFile(path, "utf8")
    )
  );
  const traces = options.traces.map((path, index) => ({
    name: basename(path),
    text: traceTexts[index] ?? "",
  }));
  const report = judge({
    comments: comments ?? "",
    received: received ?? "",
    sessions: sessions ?? "",
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
