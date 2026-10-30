// The `--dispatch-project` reader of `askgate-report.ts`: the decision documents AskGate publishes
// to a Dispatch project, which is all a standing session holding a Dispatch token and nothing else
// — no access to the machine a gated session ran on — can read. The session-file report is the
// other program, in `askgate-report.ts`, which owns the command line of both; what they count with
// is in `askgate-report-common.ts`.
import { activeDispatchSettings } from "@legion/envoy-client/dispatch-config";
import { DispatchClient } from "@legion/envoy-client/dispatch-http";
import { sessionConfigFile, sessionDispatchSettings } from "@legion/envoy-client/session-settings";
import {
  DECISION_DOCUMENT_PREFIX,
  type DecisionRecord,
} from "../extensions/askgate/decision-record";
import {
  RULE_FAILURE_OUTCOMES,
  RULE_NAMES,
  type RuleOutcome,
} from "../extensions/askgate/rules-check";
import { isObject, num, overrides, share, str, strOrNull, tally } from "./askgate-report-common";

/**
 * The fields of one decision record the report counts: a subset of the record
 * `extensions/askgate/decision-record.ts` writes, taken from its own type, so a change to that
 * wire shape is a type error here rather than a silently empty count.
 */
export type ReportRecord = Pick<
  DecisionRecord,
  | "time"
  | "session"
  | "id"
  | "source"
  | "target"
  | "rule"
  | "ruleOutcome"
  | "ruleError"
  | "ruleAsksCut"
  | "ruleAsksPartial"
  | "outcome"
  | "verdictMode"
  | "asksCarried"
>;

/** What the reader reads of a project's artifact: the two names it matches, and the id it reads. */
export interface ReportArtifact {
  readonly id: string;
  readonly name: string;
  readonly slug: string;
}

/**
 * What the reader needs of a Dispatch client, which is the two calls it makes. Declared
 * structurally, over the fields it reads, so the host's own `DispatchClient` satisfies it and a
 * test drives it with a plain object.
 */
export interface ReportClient {
  listProjectArtifacts(project: string): Promise<readonly ReportArtifact[]>;
  docRead(id: string): Promise<{ readonly markdown: string }>;
}

/** One fenced line as a decision record, or null when it is not one. */
export function parseRecordLine(line: string): ReportRecord | null {
  let value: unknown;
  try {
    value = JSON.parse(line);
  } catch {
    return null;
  }
  if (!isObject(value)) return null;
  // The four fields every count rests on. A line missing one of them belongs in no count at all,
  // so it is reported as a line that would not parse rather than counted as a decision.
  const time = str(value.time);
  const session = str(value.session);
  const source = value.source;
  const ruleOutcome = value.ruleOutcome;
  if (!Number.isFinite(Date.parse(time)) || session === "") return null;
  if (source !== "pod" && source !== "machine") return null;
  // Null on an override — the record of a call the agent sent anyway, which no rules check judged
  // — and on an unmarked write, which no gate read; a string on a rules call. A line holding
  // anything else there is no record at all.
  if (ruleOutcome !== null && typeof ruleOutcome !== "string") return null;
  return {
    time,
    session,
    // A record written before the gate gave each one an id holds none, and an empty id names
    // nothing, so two of those are never taken for one write stored twice.
    id: str(value.id),
    source,
    target: strOrNull(value.target),
    rule: strOrNull(value.rule),
    ruleOutcome,
    ruleError: strOrNull(value.ruleError),
    // A record written before the gate counted the asks holds none, which is no ask cut.
    ruleAsksCut: num(value.ruleAsksCut) ?? 0,
    // A record written before the gate told the two apart holds nothing here, which is a call
    // judged on all of its asks.
    ruleAsksPartial: value.ruleAsksPartial === true,
    outcome: str(value.outcome),
    verdictMode: str(value.verdictMode),
    // Only an `unmarked` record carries asks the gate never read; every other holds 0.
    asksCarried: num(value.asksCarried) ?? 0,
  };
}

/** The records of one decision document, and how many of its fenced lines were not records. */
export function parseDecisionDocument(markdown: string): {
  records: ReportRecord[];
  unparsed: number;
} {
  const records: ReportRecord[] = [];
  let unparsed = 0;
  let inJson = false;
  for (const line of markdown.split("\n")) {
    const text = line.trim();
    if (text.startsWith("```")) {
      // A fence either closes the block it is in or opens one, and only a json block holds records.
      inJson = !inJson && text.slice(3).trim() === "json";
      continue;
    }
    if (!inJson || text === "") continue;
    const record = parseRecordLine(text);
    if (record === null) unparsed += 1;
    else records.push(record);
  }
  return { records, unparsed };
}

/** What a project's decision documents held, with the losses the read leaves visible. */
export interface DocumentRead {
  records: ReportRecord[];
  /** Documents whose markdown was read. */
  documents: number;
  /** Documents the read refused: their decisions are missing from every count below. */
  unreadable: number;
  /** Fenced lines that were not records: a decision half-written, or something else entirely. */
  unparsedLines: number;
  /**
   * Records dropped as a second copy of one already kept: the same session and the same `id`. A
   * write the gate gave up on and Dispatch ran all the same, then wrote again, is one decision.
   */
  duplicates: number;
}

/** How many decision documents are read at once, as `scripts/dispatch/ask-census.ts` reads issues. */
const DOCUMENT_CONCURRENCY = 8;

/**
 * Reads every decision document of the project, keeping the records at or after since (epoch ms),
 * `DOCUMENT_CONCURRENCY` of them in flight: a fortnight holds one document per session, each one
 * round trip, and read one at a time they are the whole run. A document that cannot be read is
 * counted rather than thrown: one session's loss leaves the rest of the day's counts standing,
 * and the count says how much of the day is missing.
 */
export async function readDecisionDocuments(
  client: ReportClient,
  project: string,
  since: number
): Promise<DocumentRead> {
  const read: DocumentRead = {
    records: [],
    documents: 0,
    unreadable: 0,
    unparsedLines: 0,
    duplicates: 0,
  };
  const artifacts = (await client.listProjectArtifacts(project)).filter(
    (artifact) =>
      artifact.name.startsWith(DECISION_DOCUMENT_PREFIX) ||
      artifact.slug.startsWith(DECISION_DOCUMENT_PREFIX)
  );
  // Each document's text, or undefined when its read failed, kept in the artifacts' own order, so
  // the counts and the records below read as one serial pass does.
  const markdowns: (string | undefined)[] = [];
  let next = 0;
  const reader = async (): Promise<void> => {
    while (next < artifacts.length) {
      const index = next++;
      try {
        markdowns[index] = (await client.docRead((artifacts[index] as ReportArtifact).id)).markdown;
      } catch {
        markdowns[index] = undefined;
      }
    }
  };
  await Promise.all(
    Array.from({ length: Math.min(DOCUMENT_CONCURRENCY, artifacts.length) }, reader)
  );
  // One decision per (session, id): a record Dispatch stored twice — a write given up here that
  // landed there all the same, and a later one under a key of its own — would otherwise count
  // twice. A record carrying no id is from before the gate wrote one and names no write, so each
  // of those is kept.
  const seen = new Set<string>();
  for (const markdown of markdowns) {
    if (markdown === undefined) {
      read.unreadable += 1;
      continue;
    }
    read.documents += 1;
    const { records, unparsed } = parseDecisionDocument(markdown);
    read.unparsedLines += unparsed;
    for (const record of records) {
      if (Date.parse(record.time) < since) continue;
      const key = `${record.session}\n${record.id}`;
      if (record.id !== "" && seen.has(key)) {
        read.duplicates += 1;
        continue;
      }
      seen.add(key);
      read.records.push(record);
    }
  }
  return read;
}

/** The day's counts over one set of records: one source's, or every source together. */
export interface DecisionCounts {
  /**
   * The rules calls: the records a rules check judged, which is the error rate's denominator. An
   * override is a record of its own and none of them, so overriding a refusal never moves it, and
   * so is an unmarked write, which no gate read.
   */
  calls: number;
  /** Records with a rule: the calls the rules check refused. */
  refusals: number;
  /** The refusals by rule, the rules the gate ships named even at zero. */
  byRule: Record<string, number>;
  /** Refusals a later record on the same target, in the same session, overrode. */
  overrides: number;
  /** Rules calls that carried an error text, timed out, or gave no verdict. */
  errors: number;
  /**
   * Rules calls whose request did not carry every new ask whole (`ruleAsksCut`): the check judged
   * clipped text, so what it did not read could match no rule.
   */
  cut: number;
  /**
   * Rules calls judged on part of their asks (`ruleAsksPartial`): an upload whose stored version
   * the gate could not read, so the asks that version may already hold went unjudged.
   */
  partial: number;
  /**
   * Rules calls that judged none of their asks (`ruleOutcome` `unjudged`), a part of `partial`:
   * the upload went out with no rule check of the asks it may add, and no model was asked.
   */
  unjudged: number;
  /**
   * Writes that reached the `dispatch` CLI outside the form the gate reads and carried an ask for
   * a person (`outcome` `unmarked`), in block mode: the CLI refused each, so it did not post.
   */
  unmarkedBlock: number;
  /** The same in warn mode: each posted, with no check of the asks it carries. */
  unmarkedWarn: number;
  /**
   * The asks those unmarked writes carried, summed. The CLI counts them from the write's own
   * text, so a re-upload of ask blocks a stored version already holds counts them as carried.
   */
  unmarkedAsks: number;
}

/** One day's decisions over one Dispatch project, as both output modes print them. */
export interface DispatchReport {
  project: string;
  /** `--since`, as given. */
  since: string;
  documents: number;
  unreadableDocuments: number;
  /** Sessions the kept records name: the sessions that made a decision in the day. */
  sessions: number;
  unparsedLines: number;
  /** Records dropped as a second copy of one already counted: the same session and the same id. */
  duplicateRecords: number;
  total: DecisionCounts;
  pod: DecisionCounts;
  machine: DecisionCounts;
}

/** The counts of one source's records; `overridden` holds the day's overridden refusals. */
function countDecisions(
  records: readonly ReportRecord[],
  overridden: ReadonlySet<ReportRecord>
): DecisionCounts {
  const refusals = records.filter((record) => record.rule !== null);
  const calls = records.filter((record) => record.ruleOutcome !== null);
  // An unmarked write never reached a gate: it carries no rule and no rules outcome, so it is in
  // no count above, and it is counted here alone.
  const unmarked = records.filter((record) => record.outcome === "unmarked");
  return {
    calls: calls.length,
    refusals: refusals.length,
    byRule: Object.fromEntries(
      tally(
        refusals.map((record) => record.rule as string),
        RULE_NAMES
      ).map((count) => [count.name, count.count])
    ),
    overrides: refusals.filter((record) => overridden.has(record)).length,
    errors: calls.filter(
      (record) =>
        record.ruleError !== null ||
        RULE_FAILURE_OUTCOMES.includes(record.ruleOutcome as RuleOutcome)
    ).length,
    cut: calls.filter((record) => record.ruleAsksCut > 0).length,
    partial: calls.filter((record) => record.ruleAsksPartial).length,
    unjudged: calls.filter((record) => record.ruleOutcome === "unjudged").length,
    unmarkedBlock: unmarked.filter((record) => record.verdictMode === "block").length,
    unmarkedWarn: unmarked.filter((record) => record.verdictMode === "warn").length,
    unmarkedAsks: unmarked.reduce((sum, record) => sum + record.asksCarried, 0),
  };
}

export function computeDispatchReport(
  read: DocumentRead,
  project: string,
  since: string
): DispatchReport {
  // The walk runs over the whole day, so a refusal and the rebuttal that overrode it meet even
  // where one source's records are counted on their own; each override lands on its refusal's.
  const overridden = new Set(
    overrides(read.records, (record) => record.time).map(({ refusal }) => refusal)
  );
  const of = (source: ReportRecord["source"]): readonly ReportRecord[] =>
    read.records.filter((record) => record.source === source);
  return {
    project,
    since,
    documents: read.documents,
    unreadableDocuments: read.unreadable,
    sessions: new Set(read.records.map((record) => record.session)).size,
    unparsedLines: read.unparsedLines,
    duplicateRecords: read.duplicates,
    total: countDecisions(read.records, overridden),
    pod: countDecisions(of("pod"), overridden),
    machine: countDecisions(of("machine"), overridden),
  };
}

function bySource(report: DispatchReport, count: (counts: DecisionCounts) => number): string {
  return `${count(report.total)} (pod ${count(report.pod)}, machine ${count(report.machine)})`;
}

export function formatDispatchReport(report: DispatchReport): string[] {
  return [
    "A rules call is one decision record whose rules check ran; every count below but the overrides and the unmarked writes is over those alone.",
    "An override is another record a document holds: a refused call the agent sent again, which no rules check judged and which no rules call count holds.",
    "An unmarked write is the third: a write that reached the dispatch CLI outside the form the gate reads, carrying at least one ask for a person. No check judged it, so it is in no rules call count and in no override count.",
    "A rules refusal is a rules call with a rule. It was overridden when the next record on its target, in its session, is a rebuttal or a breaker.",
    "A document that could not be read, and a line that is not a record, are counted below: the decisions they held are missing from every other count.",
    "A duplicate record is a second copy of one decision, which Dispatch stored where a write given up here landed there all the same: it is counted below and in nothing else.",
    "A rules call with asks cut is one whose request did not carry every new ask of the call whole: the check judged clipped text.",
    "A rules call judged on part of its asks is an upload whose stored version the gate could not read: it judged the asks no stored version can hold and left the rest alone.",
    "A rules call that judged none of its asks is one of those whose every ask carries an id the stored version may hold: no model was asked, and the upload went out unjudged.",
    "The asks an unmarked write carried are counted from the write's own text, so a re-upload of ask blocks the stored version already holds counts them as carried.",
    `project: ${report.project}`,
    `since: ${report.since}`,
    `documents: ${report.documents}`,
    `unreadable documents: ${report.unreadableDocuments}`,
    `sessions: ${report.sessions}`,
    `unparsed lines: ${report.unparsedLines}`,
    `duplicate records: ${report.duplicateRecords}`,
    `rules calls: ${bySource(report, (c) => c.calls)}`,
    `refusals: ${bySource(report, (c) => c.refusals)}`,
    ...Object.keys(report.total.byRule).map(
      (rule) =>
        `rule ${rule}: ${report.total.byRule[rule] ?? 0} (pod ${report.pod.byRule[rule] ?? 0}, ` +
        `machine ${report.machine.byRule[rule] ?? 0})`
    ),
    `overrides: ${bySource(report, (c) => c.overrides)}`,
    `rules errors: ${bySource(report, (c) => c.errors)}`,
    `rules calls with asks cut: ${bySource(report, (c) => c.cut)}`,
    `rules calls judged on part of their asks: ${bySource(report, (c) => c.partial)}`,
    `rules calls that judged none of their asks: ${bySource(report, (c) => c.unjudged)}`,
    `unmarked writes refused in block mode: ${bySource(report, (c) => c.unmarkedBlock)}`,
    `unmarked writes posted in warn mode: ${bySource(report, (c) => c.unmarkedWarn)}`,
    `asks those unmarked writes carried: ${bySource(report, (c) => c.unmarkedAsks)}`,
    `rules error rate: ${share(report.total.errors, report.total.calls)} (pod ` +
      `${share(report.pod.errors, report.pod.calls)}, machine ` +
      `${share(report.machine.errors, report.machine.calls)})`,
  ];
}

/** What the Dispatch mode prints: with --json one object and nothing else, else the plain text. */
export function dispatchReportLines(report: DispatchReport, json: boolean): string[] {
  return json ? [JSON.stringify(report)] : formatDispatchReport(report);
}

/** The session's own Dispatch, as a standing session holding a token and nothing else resolves it. */
export function sessionDispatch(): DispatchClient {
  const { dispatch, warnings } = sessionDispatchSettings(
    sessionConfigFile("command", process.env),
    process.env,
    { cwd: process.cwd() }
  );
  // Said on stderr, so --json keeps stdout to the one object.
  for (const warning of warnings) console.error(`askgate-report: ${warning}`);
  const config = activeDispatchSettings(dispatch);
  if (config === null) {
    throw new Error(`Dispatch is not configured: ${dispatch.enabled ? "" : dispatch.notice}`);
  }
  return new DispatchClient(config.url, config.token, fetch);
}
