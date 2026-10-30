// The `--dispatch-project` reader: a Dispatch project's decision documents, read and counted. The
// session-file report's tests, and the command line's, are in `askgate-report.test.ts`.
import { describe, expect, test } from "bun:test";
import {
  computeDispatchReport,
  type DispatchReport,
  dispatchReportLines,
  type ReportClient,
  readDecisionDocuments,
} from "./askgate-report-dispatch";

/** The targets of the records below, each as the gate writes one. */
const ASK_1 = "dispatch_ask issue=ACME-1";
const EDIT_2 = "dispatch_doc_edit issue=ACME-2 artifact=spec";
const SPEC = "dispatch_issue project=ACME";
const COMMENT_7 = "dispatch_comment issue=ACME-7";
const at = (second: number) => `2026-10-22T10:00:${String(second).padStart(2, "0")}.000Z`;
const SINCE = "2026-10-20T00:00:00Z";

const PROJECT = "ACME";
const POD_SESSION = "ses-pod";
const MACHINE_SESSION = "ses-machine";

/** One decision record's line, in the wire shape `extensions/askgate/decision-record.ts` writes. */
function decision(e: {
  time: string;
  session: string;
  /** What tells the record from the session's others; its own time unless a test repeats one. */
  id?: string;
  source: "pod" | "machine";
  target?: string;
  rule?: string;
  outcome?: string;
  /** Null is an override's own: a call the agent sent anyway, which no rules check judged. */
  ruleOutcome?: string | null;
  ruleError?: string;
  /** How many of the call's new asks the rules request did not carry whole. */
  ruleAsksCut?: number;
  /** The call was judged on part of its asks: its upload's stored version could not be read. */
  ruleAsksPartial?: boolean;
  /** The checks' mode: `block` unless the record says otherwise. */
  verdictMode?: "warn" | "block";
  /** The asks an unmarked write carried, as the CLI counted them from its own text. */
  asksCarried?: number;
}): string {
  return JSON.stringify({
    time: e.time,
    session: e.session,
    id: e.id ?? `call-${e.time}`,
    source: e.source,
    project: PROJECT,
    tool: "bash",
    target: e.target ?? null,
    rule: e.rule ?? null,
    ruleOutcome:
      e.ruleOutcome === undefined ? (e.rule === undefined ? "none" : "match") : e.ruleOutcome,
    ruleError: e.ruleError ?? null,
    ruleTextMissing: false,
    ruleAsksCut: e.ruleAsksCut ?? 0,
    ruleAsksPartial: e.ruleAsksPartial ?? false,
    decision: e.rule === undefined ? "allow" : "revise",
    outcome: e.outcome ?? "verdict",
    verdictMode: e.verdictMode ?? "block",
    asksCarried: e.asksCarried ?? 0,
    ruleLatencyMs: 1200,
    charterLatencyMs: null,
    plugin: "@sjawhar/pi-envoy@0.0.0",
  });
}

/**
 * A decision document as the recorder writes one (`extensions/askgate/decision-record.ts`): the
 * header its create carries, which holds no record of its own, and then one block appended per
 * record, the session's first included.
 */
function decisionDocument(session: string, lines: readonly string[]): string {
  const header =
    `AskGate rule decisions of session ${session}. One fenced block per decision follows, ` +
    "each holding one JSON record.\n";
  return lines.reduce((text, line) => `${text}\n\`\`\`json\n${line}\n\`\`\`\n`, header);
}

/** A document of the project: its markdown, or null for one whose read Dispatch refuses. */
interface ProjectDocument {
  name: string;
  markdown: string | null;
}

/** A Dispatch project holding documents, as the reader's two calls see it. */
function dispatchProject(documents: readonly ProjectDocument[]): ReportClient {
  return {
    listProjectArtifacts: async (key: string) => {
      expect(key).toBe(PROJECT);
      return documents.map((held, index) => ({
        id: `art-${index}`,
        name: held.name,
        slug: held.name.replace(/\.md$/, ""),
      }));
    },
    docRead: async (id: string) => {
      const held = documents[Number(id.slice("art-".length))];
      if (held === undefined || held.markdown === null) {
        throw new Error(`artifact ${id} cannot be read`);
      }
      return { markdown: held.markdown };
    },
  };
}

/** Every count of one source at zero: the fixtures below name only what they change. */
const NO_DECISIONS = {
  calls: 0,
  refusals: 0,
  byRule: { no_change: 0, untried_access: 0 },
  overrides: 0,
  errors: 0,
  cut: 0,
  partial: 0,
  unjudged: 0,
  unmarkedBlock: 0,
  unmarkedWarn: 0,
  unmarkedAsks: 0,
};

// The pod session: a refusal a rebuttal on the same target overrode, a refusal whose request did
// not carry every ask whole, and a rules call that errored.
const POD_DOCUMENT = decisionDocument(POD_SESSION, [
  decision({ time: at(1), session: POD_SESSION, source: "pod", rule: "no_change", target: ASK_1 }),
  decision({
    time: at(2),
    session: POD_SESSION,
    source: "pod",
    target: ASK_1,
    outcome: "rebuttal",
    ruleOutcome: null,
  }),
  decision({
    time: at(3),
    session: POD_SESSION,
    source: "pod",
    rule: "untried_access",
    target: EDIT_2,
    ruleAsksCut: 2,
  }),
  decision({
    time: at(4),
    session: POD_SESSION,
    source: "pod",
    target: SPEC,
    ruleOutcome: "error",
    ruleError: "502 from the provider",
  }),
]);

// The machine session: one refusal, a rules call that timed out with no error text of its own,
// and one allowed call whose request clipped an ask.
const MACHINE_DOCUMENT = decisionDocument(MACHINE_SESSION, [
  decision({
    time: at(1),
    session: MACHINE_SESSION,
    source: "machine",
    rule: "no_change",
    target: COMMENT_7,
  }),
  decision({
    time: at(2),
    session: MACHINE_SESSION,
    source: "machine",
    target: SPEC,
    ruleOutcome: "timeout",
  }),
  decision({
    time: at(3),
    session: MACHINE_SESSION,
    source: "machine",
    target: SPEC,
    ruleAsksCut: 1,
  }),
]);

/** The day's project: the two sessions' documents, and one document that is not a decision one. */
const DAY: ProjectDocument[] = [
  { name: `askgate-decisions-${POD_SESSION}.md`, markdown: POD_DOCUMENT },
  { name: "spec.md", markdown: "The project's specification, which holds no decision." },
  { name: `askgate-decisions-${MACHINE_SESSION}.md`, markdown: MACHINE_DOCUMENT },
];

const DAY_REPORT: DispatchReport = {
  project: PROJECT,
  since: SINCE,
  documents: 2,
  unreadableDocuments: 0,
  sessions: 2,
  unparsedLines: 0,
  duplicateRecords: 0,
  total: {
    ...NO_DECISIONS,
    calls: 6,
    refusals: 3,
    byRule: { no_change: 2, untried_access: 1 },
    overrides: 1,
    errors: 2,
    cut: 2,
  },
  pod: {
    ...NO_DECISIONS,
    calls: 3,
    refusals: 2,
    byRule: { no_change: 1, untried_access: 1 },
    overrides: 1,
    errors: 1,
    cut: 1,
  },
  machine: {
    ...NO_DECISIONS,
    calls: 3,
    refusals: 1,
    byRule: { no_change: 1, untried_access: 0 },
    errors: 1,
    cut: 1,
  },
};

async function reportOf(documents: readonly ProjectDocument[], since: string) {
  const read = await readDecisionDocuments(dispatchProject(documents), PROJECT, Date.parse(since));
  return { read, report: computeDispatchReport(read, PROJECT, since) };
}

describe("the Dispatch reader", () => {
  test("two sessions' documents, one pod and one machine, are read into one report", async () => {
    const { report } = await reportOf(DAY, SINCE);
    expect(report).toEqual(DAY_REPORT);
  });

  // #2161, reviewer round 17: the document's create carries its header and no record, and every
  // record is appended after it, the session's first included. A session whose records were all
  // lost leaves that header alone, which is no record and no unparsed line either.
  test("a document whose header holds no record is read, with the records appended after it", async () => {
    const session = "ses-appended";
    const records = [
      decision({ time: at(1), session, source: "pod", rule: "no_change", target: ASK_1 }),
      decision({ time: at(2), session, source: "pod", target: SPEC }),
    ];
    const { read } = await reportOf(
      [
        { name: `askgate-decisions-${session}.md`, markdown: decisionDocument(session, records) },
        { name: "askgate-decisions-ses-empty.md", markdown: decisionDocument("ses-empty", []) },
      ],
      SINCE
    );
    expect({
      records: read.records.map((record) => record.target),
      documents: read.documents,
      unparsedLines: read.unparsedLines,
    }).toEqual({ records: [ASK_1, SPEC], documents: 2, unparsedLines: 0 });
  });

  test("a record before --since is left out, and one at --since is counted", async () => {
    const documents = [
      {
        name: `askgate-decisions-${POD_SESSION}.md`,
        markdown: decisionDocument(POD_SESSION, [
          decision({
            time: at(0),
            session: POD_SESSION,
            source: "pod",
            rule: "no_change",
            target: ASK_1,
          }),
          decision({
            time: at(1),
            session: POD_SESSION,
            source: "pod",
            rule: "untried_access",
            target: EDIT_2,
          }),
        ]),
      },
    ];
    const { read, report } = await reportOf(documents, at(1));
    expect(read.records.map((record) => record.time)).toEqual([at(1)]);
    expect(report.total).toEqual({
      ...NO_DECISIONS,
      calls: 1,
      refusals: 1,
      byRule: { no_change: 0, untried_access: 1 },
    });
  });

  test("the documents are read eight at a time, their records kept in the project's order", async () => {
    const sessions = Array.from({ length: 20 }, (_, index) => `ses-${index}`);
    const project = dispatchProject(
      sessions.map((session) => ({
        name: `askgate-decisions-${session}.md`,
        markdown: decisionDocument(session, [
          decision({ time: at(1), session, source: "pod", rule: "no_change", target: ASK_1 }),
        ]),
      }))
    );
    let reading = 0;
    let mostReading = 0;
    const read = await readDecisionDocuments(
      {
        listProjectArtifacts: project.listProjectArtifacts,
        // Counted around the read's own await, which every reader reaches before the first of
        // them resumes: no clock, and the high-water mark is the bound itself.
        docRead: async (id) => {
          reading += 1;
          mostReading = Math.max(mostReading, reading);
          const document = await project.docRead(id);
          reading -= 1;
          return document;
        },
      },
      PROJECT,
      Date.parse(SINCE)
    );
    expect(mostReading).toBe(8);
    expect(read.documents).toBe(sessions.length);
    expect(read.records.map((record) => record.session)).toEqual(sessions);
  });

  test("a line that is not a record and a document the read refuses are counted, not fatal", async () => {
    const documents: ProjectDocument[] = [
      { name: `askgate-decisions-${POD_SESSION}.md`, markdown: POD_DOCUMENT },
      {
        name: `askgate-decisions-${MACHINE_SESSION}.md`,
        markdown: decisionDocument(MACHINE_SESSION, [
          '{"time": "2026-10-22T10:00:01.000Z", "session": "ses-machine"',
          decision({
            time: at(1),
            session: MACHINE_SESSION,
            source: "machine",
            rule: "no_change",
            target: COMMENT_7,
          }),
        ]),
      },
      { name: "askgate-decisions-ses-killed.md", markdown: null },
    ];
    const { report } = await reportOf(documents, SINCE);
    expect({
      documents: report.documents,
      unreadableDocuments: report.unreadableDocuments,
      unparsedLines: report.unparsedLines,
      sessions: report.sessions,
    }).toEqual({ documents: 2, unreadableDocuments: 1, unparsedLines: 1, sessions: 2 });
    expect(report.pod).toEqual(DAY_REPORT.pod);
    expect(report.machine).toEqual({
      ...NO_DECISIONS,
      calls: 1,
      refusals: 1,
      byRule: { no_change: 1, untried_access: 0 },
    });
  });

  // #2161, reviewer round 16: a record write given up on the session's side can have landed on
  // Dispatch's all the same, so one decision can be in the document twice. Each decision carries
  // an id of its own, and one (session, id) is one decision.
  test("one decision stored twice is counted once, and the second copy is counted as a duplicate", async () => {
    const twice = decision({
      time: at(1),
      session: POD_SESSION,
      id: "tool-call-1",
      source: "pod",
      rule: "no_change",
      target: ASK_1,
    });
    const documents: ProjectDocument[] = [
      {
        name: `askgate-decisions-${POD_SESSION}.md`,
        markdown: decisionDocument(POD_SESSION, [
          twice,
          twice,
          // Another call of the same session: its own id, so it is a decision of its own.
          decision({
            time: at(2),
            session: POD_SESSION,
            id: "tool-call-2",
            source: "pod",
            rule: "untried_access",
            target: EDIT_2,
          }),
        ]),
      },
      {
        // Another session's record of the same id: a tool call id is one session's own.
        name: `askgate-decisions-${MACHINE_SESSION}.md`,
        markdown: decisionDocument(MACHINE_SESSION, [
          decision({
            time: at(1),
            session: MACHINE_SESSION,
            id: "tool-call-1",
            source: "machine",
            rule: "no_change",
            target: COMMENT_7,
          }),
        ]),
      },
    ];
    const { read, report } = await reportOf(documents, SINCE);
    expect(read.duplicates).toBe(1);
    expect(read.records.map((record) => [record.session, record.id])).toEqual([
      [POD_SESSION, "tool-call-1"],
      [POD_SESSION, "tool-call-2"],
      [MACHINE_SESSION, "tool-call-1"],
    ]);
    expect(report.duplicateRecords).toBe(1);
    expect(report.total.refusals).toBe(3);
    expect(report.pod.refusals).toBe(2);
    expect(dispatchReportLines(report, false)).toContain("duplicate records: 1");
  });

  test("records written before the gate gave each an id are each counted, duplicates and all", async () => {
    // A document a released gate wrote before this change holds no id: two of its records are two
    // decisions, since nothing in them says otherwise.
    const old = JSON.stringify({
      ...JSON.parse(
        decision({ time: at(1), session: POD_SESSION, source: "pod", rule: "no_change" })
      ),
      id: undefined,
    });
    const { read, report } = await reportOf(
      [
        {
          name: `askgate-decisions-${POD_SESSION}.md`,
          markdown: decisionDocument(POD_SESSION, [old, old]),
        },
      ],
      SINCE
    );
    expect(read.records.map((record) => record.id)).toEqual(["", ""]);
    expect(read.duplicates).toBe(0);
    expect(report.total.refusals).toBe(2);
  });

  test("--json prints one object carrying the numbers the plain text prints", async () => {
    const { report } = await reportOf(DAY, SINCE);
    const json = dispatchReportLines(report, true);
    expect(json).toHaveLength(1);
    expect(JSON.parse(json[0] as string)).toEqual(DAY_REPORT);
    const text = dispatchReportLines(report, false);
    expect(text).toContain(`project: ${PROJECT}`);
    expect(text).toContain(`since: ${SINCE}`);
    expect(text).toContain("documents: 2");
    expect(text).toContain("unreadable documents: 0");
    expect(text).toContain("sessions: 2");
    expect(text).toContain("unparsed lines: 0");
    expect(text).toContain("duplicate records: 0");
    expect(text).toContain("rules calls: 6 (pod 3, machine 3)");
    expect(text).toContain("refusals: 3 (pod 2, machine 1)");
    expect(text).toContain("rule no_change: 2 (pod 1, machine 1)");
    expect(text).toContain("rule untried_access: 1 (pod 1, machine 0)");
    expect(text).toContain("overrides: 1 (pod 1, machine 0)");
    expect(text).toContain("rules errors: 2 (pod 1, machine 1)");
    expect(text).toContain("rules calls with asks cut: 2 (pod 1, machine 1)");
    expect(text).toContain("rules calls judged on part of their asks: 0 (pod 0, machine 0)");
    expect(text).toContain("rules calls that judged none of their asks: 0 (pod 0, machine 0)");
    expect(text).toContain("unmarked writes refused in block mode: 0 (pod 0, machine 0)");
    expect(text).toContain("unmarked writes posted in warn mode: 0 (pod 0, machine 0)");
    expect(text).toContain("asks those unmarked writes carried: 0 (pod 0, machine 0)");
    expect(text).toContain("rules error rate: 33.3% (pod 33.3%, machine 33.3%)");
  });

  test("the asks a request did not carry whole are counted from the records, by source", async () => {
    // Two pod calls whose request clipped an ask, one machine call whose request carried its asks
    // whole: what the rules check judged short of the call's own text is the count's whole point.
    const documents: ProjectDocument[] = [
      {
        name: `askgate-decisions-${POD_SESSION}.md`,
        markdown: decisionDocument(POD_SESSION, [
          decision({
            time: at(1),
            session: POD_SESSION,
            source: "pod",
            rule: "no_change",
            target: ASK_1,
            ruleAsksCut: 4,
          }),
          decision({
            time: at(2),
            session: POD_SESSION,
            source: "pod",
            target: SPEC,
            ruleAsksCut: 1,
          }),
        ]),
      },
      {
        name: `askgate-decisions-${MACHINE_SESSION}.md`,
        markdown: decisionDocument(MACHINE_SESSION, [
          decision({ time: at(1), session: MACHINE_SESSION, source: "machine", target: SPEC }),
        ]),
      },
    ];
    const { read, report } = await reportOf(documents, SINCE);
    // The count is of the calls that lost text, not of the asks they lost: five asks, two calls.
    expect(read.records.map((record) => record.ruleAsksCut)).toEqual([4, 1, 0]);
    expect([report.total.cut, report.pod.cut, report.machine.cut]).toEqual([2, 2, 0]);
    expect(dispatchReportLines(report, false)).toContain(
      "rules calls with asks cut: 2 (pod 2, machine 0)"
    );
    const json = JSON.parse(dispatchReportLines(report, true)[0] as string) as DispatchReport;
    expect([json.total.cut, json.pod.cut, json.machine.cut]).toEqual([2, 2, 0]);
  });

  test("the calls judged on part of their asks are counted from the records, by source", async () => {
    // One pod call and one machine call whose upload's stored version the gate could not read, so
    // each judged only the asks no stored version can hold; the pod's other call judged them all.
    const documents: ProjectDocument[] = [
      {
        name: `askgate-decisions-${POD_SESSION}.md`,
        markdown: decisionDocument(POD_SESSION, [
          decision({
            time: at(1),
            session: POD_SESSION,
            source: "pod",
            target: SPEC,
            ruleAsksPartial: true,
          }),
          decision({ time: at(2), session: POD_SESSION, source: "pod", target: ASK_1 }),
        ]),
      },
      {
        name: `askgate-decisions-${MACHINE_SESSION}.md`,
        markdown: decisionDocument(MACHINE_SESSION, [
          decision({
            time: at(1),
            session: MACHINE_SESSION,
            source: "machine",
            target: SPEC,
            ruleAsksPartial: true,
          }),
        ]),
      },
    ];
    const { read, report } = await reportOf(documents, SINCE);
    expect(read.records.map((record) => record.ruleAsksPartial)).toEqual([true, false, true]);
    expect([report.total.partial, report.pod.partial, report.machine.partial]).toEqual([2, 1, 1]);
    expect(dispatchReportLines(report, false)).toContain(
      "rules calls judged on part of their asks: 2 (pod 1, machine 1)"
    );
    const json = JSON.parse(dispatchReportLines(report, true)[0] as string) as DispatchReport;
    expect([json.total.partial, json.pod.partial, json.machine.partial]).toEqual([2, 1, 1]);
  });

  test("a call that judged none of its asks is a rules call, judged on part of its asks, and counted apart", async () => {
    // An upload whose stored version the gate could not read and whose every opener carries an id:
    // the record is written with no model call behind it. It is a decision, and not a clean one.
    const documents: ProjectDocument[] = [
      {
        name: `askgate-decisions-${POD_SESSION}.md`,
        markdown: decisionDocument(POD_SESSION, [
          decision({
            time: at(1),
            session: POD_SESSION,
            source: "pod",
            target: SPEC,
            ruleOutcome: "unjudged",
            ruleAsksPartial: true,
          }),
          decision({ time: at(2), session: POD_SESSION, source: "pod", target: ASK_1 }),
        ]),
      },
    ];
    const { read, report } = await reportOf(documents, SINCE);
    expect(read.records.map((record) => record.ruleOutcome)).toEqual(["unjudged", "none"]);
    expect(report.pod).toEqual({ ...NO_DECISIONS, calls: 2, partial: 1, unjudged: 1 });
    const text = dispatchReportLines(report, false);
    expect(text).toContain("rules calls: 2 (pod 2, machine 0)");
    expect(text).toContain("rules calls judged on part of their asks: 1 (pod 1, machine 0)");
    expect(text).toContain("rules calls that judged none of their asks: 1 (pod 1, machine 0)");
    const json = JSON.parse(dispatchReportLines(report, true)[0] as string) as DispatchReport;
    expect([json.total.unjudged, json.pod.unjudged, json.machine.unjudged]).toEqual([1, 1, 0]);
  });

  test("an override counts on the refusal it overrode and in none of the rules call counts", async () => {
    const markdown = decisionDocument(POD_SESSION, [
      decision({
        time: at(1),
        session: POD_SESSION,
        source: "pod",
        rule: "no_change",
        target: ASK_1,
      }),
      // The agent sent the refused call again with a rebuttal: the gate answered it before either
      // model call, so the record carries no rules outcome and only its target pairs the two.
      decision({
        time: at(2),
        session: POD_SESSION,
        source: "pod",
        target: ASK_1,
        outcome: "rebuttal",
        ruleOutcome: null,
      }),
      decision({
        time: at(3),
        session: POD_SESSION,
        source: "pod",
        target: SPEC,
        ruleOutcome: "error",
        ruleError: "502 from the provider",
      }),
    ]);
    const { read, report } = await reportOf(
      [{ name: `askgate-decisions-${POD_SESSION}.md`, markdown }],
      SINCE
    );
    // The override is a record like any other: three records read, none of them unparsed.
    expect(read.records).toHaveLength(3);
    expect(report.unparsedLines).toBe(0);
    expect(report.pod).toEqual({
      ...NO_DECISIONS,
      calls: 2,
      refusals: 1,
      byRule: { no_change: 1, untried_access: 0 },
      overrides: 1,
      errors: 1,
    });
    const text = dispatchReportLines(report, false);
    expect(text).toContain("rules calls: 2 (pod 2, machine 0)");
    expect(text).toContain("overrides: 1 (pod 1, machine 0)");
    expect(text).toContain("rules errors: 1 (pod 1, machine 0)");
    // One of the two rules calls errored. The override is in neither the count nor its divisor,
    // which a denominator of three records would have made 33.3%.
    expect(text).toContain("rules error rate: 50.0% (pod 50.0%, machine n/a)");
  });

  test("unmarked writes are counted on their own line, by mode, with the asks they carried", async () => {
    // A write that reached the CLI outside the form the gate reads: block refused it, warn posted
    // it with no check of its asks. No rules check judged either, and neither has a target, so
    // they are no rules call, no refusal and no override — only their own line counts them.
    const markdown = decisionDocument(POD_SESSION, [
      decision({
        time: at(1),
        session: POD_SESSION,
        source: "pod",
        outcome: "unmarked",
        ruleOutcome: null,
        verdictMode: "block",
        asksCarried: 1,
      }),
      decision({
        time: at(2),
        session: POD_SESSION,
        source: "pod",
        outcome: "unmarked",
        ruleOutcome: null,
        verdictMode: "warn",
        asksCarried: 2,
      }),
      decision({ time: at(3), session: POD_SESSION, source: "pod", target: ASK_1 }),
    ]);
    const machine = decisionDocument(MACHINE_SESSION, [
      decision({
        time: at(1),
        session: MACHINE_SESSION,
        source: "machine",
        outcome: "unmarked",
        ruleOutcome: null,
        verdictMode: "warn",
        asksCarried: 3,
      }),
    ]);
    const { read, report } = await reportOf(
      [
        { name: `askgate-decisions-${POD_SESSION}.md`, markdown },
        { name: `askgate-decisions-${MACHINE_SESSION}.md`, markdown: machine },
      ],
      SINCE
    );
    expect(read.records.map((record) => [record.outcome, record.asksCarried])).toEqual([
      ["unmarked", 1],
      ["unmarked", 2],
      ["verdict", 0],
      ["unmarked", 3],
    ]);
    expect(report.pod).toEqual({
      ...NO_DECISIONS,
      calls: 1,
      unmarkedBlock: 1,
      unmarkedWarn: 1,
      unmarkedAsks: 3,
    });
    expect(report.machine).toEqual({
      ...NO_DECISIONS,
      unmarkedWarn: 1,
      unmarkedAsks: 3,
    });
    const text = dispatchReportLines(report, false);
    expect(text).toContain("unmarked writes refused in block mode: 1 (pod 1, machine 0)");
    expect(text).toContain("unmarked writes posted in warn mode: 2 (pod 1, machine 1)");
    expect(text).toContain("asks those unmarked writes carried: 6 (pod 3, machine 3)");
    // The one rules call is the pod's third record: the unmarked writes move no other count.
    expect(text).toContain("rules calls: 1 (pod 1, machine 0)");
    expect(text).toContain("refusals: 0 (pod 0, machine 0)");
    expect(text).toContain("overrides: 0 (pod 0, machine 0)");
    expect(text).toContain("rules error rate: 0.0% (pod 0.0%, machine n/a)");
    const json = JSON.parse(dispatchReportLines(report, true)[0] as string) as DispatchReport;
    expect([json.total.unmarkedBlock, json.total.unmarkedWarn, json.total.unmarkedAsks]).toEqual([
      1, 2, 6,
    ]);
    expect(
      dispatchReportLines(report, false)
        .slice(0, text.indexOf(`project: ${PROJECT}`))
        .join(" ")
    ).toContain("counts them as carried");
  });
});
