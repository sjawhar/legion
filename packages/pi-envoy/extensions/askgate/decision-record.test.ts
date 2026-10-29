import { describe, expect, jest, test } from "bun:test";
import pkg from "../../package.json";
import type { GateEntry } from "./askgate-core";
import {
  createDecisionRecorder,
  RECORD_SHUT_DOWN,
  RECORD_TIMEOUT_MS,
  type RecordClient,
  type RecordDeps,
  type RecordedDecision,
} from "./decision-record";

/** The clock every record below is stamped by. */
const NOW = Date.parse("2026-10-25T09:15:30.250Z");
const SESSION = "01a15300-0000-7000-8000-00000000beef";
const DOCUMENT = `askgate-decisions-${SESSION}.md`;

/**
 * A refused call's entry, as the gate builds it: the rules check matched, and the entry also holds
 * the ask's own words, the quote and the reason, none of which the record may carry.
 */
const REFUSAL: GateEntry = {
  advisor: "AskGate",
  tool: "dispatch_ask",
  path: "xd://dispatch_ask",
  toolCallId: "t1",
  decision: "revise",
  outcome: "verdict",
  verdictMode: "block",
  latencyMs: 9300,
  revisesForKey: 0,
  argsDigest: "a9f1c0",
  target: "dispatch_ask issue=X-1",
  rule: "no_change",
  ruleAsk: 1,
  ruleQuote: "keep the cache path as it is",
  ruleOutcome: "match",
  ruleLatencyMs: 4100,
  charterLatencyMs: 5200,
  reason: "a rule refuses a new ask in it",
  deliveredReason: "AskGate did not send this call: a rule refuses a new ask in it.",
  reviewedText: '{"question":"Should I change the cache path? Recommendation: keep it."}',
  model: "anthropic/claude-fable-5-1",
};

/**
 * An override's entry, as the gate builds one: a call the agent sent anyway with a rebuttal of its
 * own, or the third attempt on one target, answered before either model call — so no rules check
 * judged it, and the line the agent wrote is on the entry, which the record may no more carry than
 * an ask's words.
 */
const OVERRIDE: GateEntry = {
  advisor: "AskGate",
  tool: "dispatch_ask",
  path: "xd://dispatch_ask",
  toolCallId: "t3",
  decision: "allow",
  outcome: "rebuttal",
  verdictMode: "block",
  latencyMs: 2,
  revisesForKey: 2,
  argsDigest: "a9f1c0",
  target: "dispatch_ask issue=X-1",
  rebuttal: "the cache path moves with this change, so the ask stands",
};

/**
 * An unmarked write's entry, as envoy.ts builds one from the `dispatch` CLI's ledger line: the
 * write reached the CLI outside the form the gate reads, so no model call judged it, and the
 * CLI's own count of the asks it carries is all the record holds of what it said.
 */
const UNMARKED: RecordedDecision = {
  tool: "dispatch_ask",
  decision: "revise",
  outcome: "unmarked",
  verdictMode: "block",
  asksCarried: 1,
};

/**
 * The Dispatch a record is written to: what each call was given, and what it answers. The
 * documents it holds are the uploads it answered, by name, since an upload of a name it already
 * holds stores a new version of that document rather than a second one.
 */
function fakeDispatch(answers: Partial<RecordClient> = {}, id = "doc-1") {
  const uploads: Array<{ project: string; name: string; content: string; actor?: unknown }> = [];
  const edits: Array<{
    id: string;
    op: string;
    markdown: string;
    after?: string;
    actor?: unknown;
  }> = [];
  /** Every read of the project's documents: the project it asked for. */
  const lists: string[] = [];
  const stored = new Map<string, string>();
  const client: RecordClient = {
    listProjectArtifacts: async (project) => {
      lists.push(project);
      if (answers.listProjectArtifacts) return await answers.listProjectArtifacts(project);
      return [...stored].map(([name, document]) => ({ id: document, name }));
    },
    projectArtifact: async (project, input) => {
      uploads.push({
        project,
        name: input.name,
        content: "content" in input ? input.content : "",
        actor: input.actor,
      });
      const answer = answers.projectArtifact
        ? await answers.projectArtifact(project, input)
        : { artifact: { id } };
      // Held only once the upload answered: an upload that rejects leaves Dispatch as it was.
      stored.set(input.name, answer.artifact.id);
      return answer;
    },
    docEdit: async (document, input) => {
      for (const op of input.ops)
        edits.push({
          id: document,
          op: op.op,
          markdown: op.markdown ?? "",
          after: op.after,
          actor: input.actor,
        });
      return answers.docEdit
        ? await answers.docEdit(document, input)
        : { applied: input.ops.length };
    },
  };
  return { uploads, edits, lists, stored, client };
}

/** One recorder, over one fake Dispatch, with the directory and signal of every client it asked for. */
function harness(
  opts: {
    env?: Record<string, string | undefined>;
    client?: RecordClient;
    /** No Dispatch configuration for the directory, as a session without one has. */
    unconfigured?: boolean;
  } = {}
) {
  const asked: Array<{ directory: string; signal: AbortSignal }> = [];
  const deps: RecordDeps = {
    env: opts.env ?? { OMP_ASKGATE_RECORD_PROJECT: "INFRA" },
    now: () => NOW,
    dispatchClient: (directory, signal) => {
      asked.push({ directory, signal });
      if (opts.unconfigured) return undefined;
      return { client: opts.client ?? fakeDispatch().client };
    },
  };
  const recorder = createDecisionRecorder(deps);
  return {
    asked,
    recorder,
    write: (entry: GateEntry | RecordedDecision, sessionId = SESSION) =>
      recorder.record({ entry, sessionId, directory: "/work" }),
  };
}

/** The JSON records a document's markdown holds, in order. */
const recordsIn = (markdown: string) =>
  [...markdown.matchAll(/```json\n(.*)\n```/g)].map(
    ([, line]) => JSON.parse(line as string) as Record<string, unknown>
  );

/** The record's keys, in the order the line is written in: every record carries these and no more. */
const RECORD_KEYS = [
  "time",
  "session",
  "source",
  "project",
  "tool",
  "target",
  "rule",
  "ruleOutcome",
  "ruleError",
  "ruleTextMissing",
  "ruleAsksCut",
  "ruleAsksPartial",
  "decision",
  "outcome",
  "verdictMode",
  "asksCarried",
  "ruleLatencyMs",
  "charterLatencyMs",
  "plugin",
];

describe("the AskGate decision record", () => {
  test("the session's first decision uploads its document, and the second appends to that document", async () => {
    const dispatch = fakeDispatch({}, "doc-7");
    const g = harness({ client: dispatch.client });

    expect(await g.write(REFUSAL)).toBeUndefined();
    expect(dispatch.uploads).toHaveLength(1);
    expect(dispatch.uploads[0]?.project).toBe("INFRA");
    // A bearer caller that names no session of its own is refused ACTOR_KIND by Dispatch.
    expect(dispatch.uploads[0]?.actor).toEqual({ kind: "session", id: SESSION });
    expect(dispatch.uploads[0]?.name).toBe(DOCUMENT);
    const content = dispatch.uploads[0]?.content ?? "";
    expect(content).toStartWith(`AskGate rule decisions of session ${SESSION}, made on `);
    expect(content).toContain("(source `machine`)");
    expect(recordsIn(content)).toHaveLength(1);
    expect(recordsIn(content)[0]).toMatchObject({ session: SESSION, rule: "no_change" });

    expect(await g.write({ ...REFUSAL, toolCallId: "t2", rule: undefined })).toBeUndefined();
    expect(dispatch.uploads).toHaveLength(1);
    expect(dispatch.edits).toEqual([
      {
        id: "doc-7",
        op: "insert",
        after: "end",
        markdown: expect.stringContaining('"rule":null') as unknown as string,
        actor: { kind: "session", id: SESSION },
      },
    ]);
    expect(recordsIn(dispatch.edits[0]?.markdown ?? "")).toHaveLength(1);
  });

  test("two sessions write two documents, each named for its own session", async () => {
    const dispatch = fakeDispatch();
    const g = harness({ client: dispatch.client });
    await g.write(REFUSAL);
    await g.write(REFUSAL, "sess-other");
    expect(dispatch.uploads.map((upload) => upload.name)).toEqual([
      DOCUMENT,
      "askgate-decisions-sess-other.md",
    ]);
    expect(dispatch.edits).toEqual([]);
  });

  test("the record holds exactly the contract's keys, and no ask text, quote or reason", async () => {
    const dispatch = fakeDispatch();
    await harness({ client: dispatch.client }).write(REFUSAL);
    const record = recordsIn(dispatch.uploads[0]?.content ?? "")[0] as Record<string, unknown>;

    expect(Object.keys(record)).toEqual(RECORD_KEYS);
    expect(record).toEqual({
      time: "2026-10-25T09:15:30.250Z",
      session: SESSION,
      source: "machine",
      project: "INFRA",
      tool: "dispatch_ask",
      target: "dispatch_ask issue=X-1",
      rule: "no_change",
      ruleOutcome: "match",
      ruleError: null,
      ruleTextMissing: false,
      ruleAsksCut: 0,
      ruleAsksPartial: false,
      decision: "revise",
      outcome: "verdict",
      verdictMode: "block",
      asksCarried: 0,
      ruleLatencyMs: 4100,
      charterLatencyMs: 5200,
      plugin: `@sjawhar/pi-envoy@${pkg.version}`,
    });
    for (const key of ["ruleAsk", "ruleQuote", "reason", "reviewedText"])
      expect(record).not.toHaveProperty(key);
    // Neither the ask's words nor the reason reach the document, header and all.
    expect(dispatch.uploads[0]?.content).not.toContain("cache path");
  });

  test("the record's plugin names the manifest's own package, not a literal that outlives a rename", async () => {
    const dispatch = fakeDispatch();
    await harness({ client: dispatch.client }).write(REFUSAL);
    const record = recordsIn(dispatch.uploads[0]?.content ?? "")[0] as Record<string, unknown>;

    // Both halves come from package.json, so a renamed package cannot leave records naming the old
    // name; the test above pins what that renders to for the manifest as it stands.
    expect(record.plugin).toBe(`${pkg.name}@${pkg.version}`);
  });

  test("a rules call that failed records its outcome, its error and what it had to leave out", async () => {
    const dispatch = fakeDispatch();
    await harness({ client: dispatch.client }).write({
      ...REFUSAL,
      decision: "allow",
      outcome: "verdict",
      rule: undefined,
      ruleOutcome: "error",
      ruleError: "provider 529",
      ruleTextMissing: true,
      ruleAsksCut: 3,
      ruleLatencyMs: undefined,
    });
    expect(recordsIn(dispatch.uploads[0]?.content ?? "")[0]).toMatchObject({
      rule: null,
      ruleOutcome: "error",
      ruleError: "provider 529",
      ruleTextMissing: true,
      ruleAsksCut: 3,
      decision: "allow",
      ruleLatencyMs: null,
      charterLatencyMs: 5200,
    });
  });

  test("an upload judged on part of its asks records that, and no missing text", async () => {
    const dispatch = fakeDispatch();
    await harness({ client: dispatch.client }).write({
      ...REFUSAL,
      tool: "dispatch_artifact",
      target: "dispatch_artifact issue=X-1",
      decision: "allow",
      rule: undefined,
      ruleOutcome: "none",
      ruleAsksPartial: true,
    });
    expect(recordsIn(dispatch.uploads[0]?.content ?? "")[0]).toMatchObject({
      tool: "dispatch_artifact",
      ruleOutcome: "none",
      ruleAsksPartial: true,
      ruleTextMissing: false,
    });
  });

  test("an upload whose rules check judged none of its asks is recorded, as unjudged", async () => {
    // No model call was made, so there is no rule and no error; the record is the only place a
    // standing session learns the call went out unjudged.
    const dispatch = fakeDispatch();
    await harness({ client: dispatch.client }).write({
      ...REFUSAL,
      tool: "dispatch_artifact",
      target: "dispatch_artifact issue=X-1",
      decision: "allow",
      rule: undefined,
      ruleOutcome: "unjudged",
      ruleAsksPartial: true,
    });
    expect(recordsIn(dispatch.uploads[0]?.content ?? "")).toMatchObject([
      {
        tool: "dispatch_artifact",
        rule: null,
        ruleOutcome: "unjudged",
        ruleError: null,
        ruleAsksPartial: true,
        decision: "allow",
      },
    ]);
  });

  test("source is pod with LEGION_ISSUE set, and machine without it", async () => {
    for (const [env, source] of [
      [{ OMP_ASKGATE_RECORD_PROJECT: "INFRA", LEGION_ISSUE: "INFRA-168" }, "pod"],
      [{ OMP_ASKGATE_RECORD_PROJECT: "INFRA", LEGION_ISSUE: "  " }, "machine"],
      [{ OMP_ASKGATE_RECORD_PROJECT: "INFRA" }, "machine"],
    ] as const) {
      const dispatch = fakeDispatch();
      await harness({ env, client: dispatch.client }).write(REFUSAL);
      const content = dispatch.uploads[0]?.content ?? "";
      expect(recordsIn(content)[0]).toMatchObject({ source });
      expect(content).toContain(`(source \`${source}\`)`);
    }
  });

  test("the project falls back to LEGION_PROJECT, upper-cased as a project key is", async () => {
    const own = fakeDispatch();
    await harness({
      env: { OMP_ASKGATE_RECORD_PROJECT: "INFRA", LEGION_PROJECT: "legion" },
      client: own.client,
    }).write(REFUSAL);
    expect(own.uploads[0]?.project).toBe("INFRA");
    expect(recordsIn(own.uploads[0]?.content ?? "")[0]).toMatchObject({ project: "INFRA" });

    // A pod's LEGION_PROJECT is the daemon's lower-cased project name, and Dispatch answers 404
    // to every spelling of a project key but the uppercase one.
    const pod = fakeDispatch();
    await harness({ env: { LEGION_PROJECT: "legion" }, client: pod.client }).write(REFUSAL);
    expect(pod.uploads[0]?.project).toBe("LEGION");
    expect(recordsIn(pod.uploads[0]?.content ?? "")[0]).toMatchObject({ project: "LEGION" });
  });

  test("with neither project set nothing reaches Dispatch", async () => {
    const none = fakeDispatch();
    for (const env of [{}, { OMP_ASKGATE_RECORD_PROJECT: "", LEGION_PROJECT: " " }]) {
      const g = harness({ env, client: none.client });
      expect(await g.write(REFUSAL)).toBeUndefined();
      // No project is no record: the gate does not even ask for a Dispatch client.
      expect(g.asked).toEqual([]);
    }
    expect(none.uploads).toEqual([]);
  });

  test("a rebuttal and the breaker each record the override, with no rules outcome of its own", async () => {
    for (const outcome of ["rebuttal", "breaker"] as const) {
      const dispatch = fakeDispatch();
      const g = harness({ client: dispatch.client });
      expect(await g.write(REFUSAL)).toBeUndefined();
      expect(await g.write({ ...OVERRIDE, outcome })).toBeUndefined();
      // Appended to the refusal's own document, carrying the refusal's target: that target, in
      // that session, is the whole of what pairs the override with what it overrode.
      expect(dispatch.uploads).toHaveLength(1);
      const record = recordsIn(dispatch.edits[0]?.markdown ?? "")[0] as Record<string, unknown>;
      expect(Object.keys(record)).toEqual(RECORD_KEYS);
      expect(record).toMatchObject({
        target: "dispatch_ask issue=X-1",
        rule: null,
        ruleOutcome: null,
        ruleError: null,
        ruleTextMissing: false,
        ruleAsksCut: 0,
        decision: "allow",
        outcome,
        ruleLatencyMs: null,
        charterLatencyMs: null,
      });
      // Neither the ask's words nor the line the agent wrote to override the refusal travels with
      // the record; both name the cache path, and the document holds it nowhere.
      expect(dispatch.edits[0]?.markdown).not.toContain("cache path");
      for (const key of ["rebuttal", "reason", "ruleQuote"]) expect(record).not.toHaveProperty(key);
    }
  });

  test("a call that is neither rules-checked nor an override writes nothing", async () => {
    const dispatch = fakeDispatch();
    const g = harness({ client: dispatch.client });
    for (const outcome of ["verdict", "halted", "skipped"] as const)
      expect(
        await g.write({
          ...REFUSAL,
          decision: "allow",
          outcome,
          rule: undefined,
          ruleOutcome: undefined,
        })
      ).toBeUndefined();
    expect(g.asked).toEqual([]);
    expect(dispatch.uploads).toEqual([]);
    expect(dispatch.edits).toEqual([]);
  });

  // A write that reached the CLI outside the form the gate reads is the one decision no model
  // call stands behind, and the asks it carries are the ones a person is left waiting on.
  test("an unmarked write carrying an ask is recorded, with its count and no rules outcome", async () => {
    const dispatch = fakeDispatch();
    const g = harness({ client: dispatch.client });
    expect(
      await g.write({ ...UNMARKED, asksCarried: 2, verdictMode: "warn", decision: "allow" })
    ).toBeUndefined();
    const record = recordsIn(dispatch.uploads[0]?.content ?? "")[0] as Record<string, unknown>;
    expect(Object.keys(record)).toEqual(RECORD_KEYS);
    expect(record).toMatchObject({
      tool: "dispatch_ask",
      target: null,
      rule: null,
      ruleOutcome: null,
      decision: "allow",
      outcome: "unmarked",
      verdictMode: "warn",
      asksCarried: 2,
      ruleLatencyMs: null,
      charterLatencyMs: null,
    });
  });

  test("an unmarked write carrying no ask writes nothing: no person waits on it", async () => {
    const dispatch = fakeDispatch();
    const g = harness({ client: dispatch.client });
    expect(await g.write({ ...UNMARKED, asksCarried: 0 })).toBeUndefined();
    expect(await g.write({ ...UNMARKED, asksCarried: undefined })).toBeUndefined();
    expect(g.asked).toEqual([]);
    expect(dispatch.uploads).toEqual([]);
    expect(dispatch.edits).toEqual([]);
  });

  test("a session with no Dispatch configuration answers why, and writes nothing", async () => {
    const g = harness({ unconfigured: true });
    expect(await g.write(REFUSAL)).toBe("the session has no Dispatch configuration");
    expect(g.asked).toMatchObject([{ directory: "/work" }]);
  });

  test("a write that rejects answers its error, and the session's next decision uploads again", async () => {
    const dispatch = fakeDispatch({
      projectArtifact: () => Promise.reject(new Error("dispatch: 503 Service Unavailable")),
    });
    const g = harness({ client: dispatch.client });
    expect(await g.write(REFUSAL)).toBe("dispatch: 503 Service Unavailable");
    expect(dispatch.edits).toEqual([]);
    // The failed upload is held for nothing: its record is lost and the next one uploads again.
    expect(await g.write(REFUSAL)).toBe("dispatch: 503 Service Unavailable");
    expect(dispatch.uploads).toHaveLength(2);
  });

  test("an append that rejects answers its error and the document is kept", async () => {
    const dispatch = fakeDispatch({ docEdit: () => Promise.reject(new Error("dispatch: 409")) });
    const g = harness({ client: dispatch.client });
    expect(await g.write(REFUSAL)).toBeUndefined();
    expect(await g.write(REFUSAL)).toBe("dispatch: 409");
    expect(await g.write(REFUSAL)).toBe("dispatch: 409");
    expect(dispatch.uploads).toHaveLength(1);
    expect(dispatch.edits).toHaveLength(2);
  });

  test("a write that never answers is given up at the record's own stop", async () => {
    // The recorder arms that stop itself, so the test runs the clock to it rather than waiting.
    jest.useFakeTimers();
    try {
      const hangs = Promise.withResolvers<{ artifact: { id: string } }>();
      const dispatch = fakeDispatch({ projectArtifact: () => hangs.promise });
      const answered = harness({ client: dispatch.client }).write(REFUSAL);
      for (let i = 0; i < 20; i++) await Promise.resolve();
      jest.advanceTimersByTime(RECORD_TIMEOUT_MS);
      expect(await answered).toBe(
        `Dispatch did not answer the record within ${RECORD_TIMEOUT_MS} ms`
      );
      expect(dispatch.uploads).toHaveLength(1);
      hangs.resolve({ artifact: { id: "doc-1" } });
    } finally {
      jest.useRealTimers();
    }
  });

  // #2161, reviewer round 15: the core's shutdown and the host's each cut the records in flight,
  // and only the core's said so; an unmarked write's record cut 52 ms in answered that Dispatch
  // had not answered within 5,000 ms. The recorder owns the cut now, and reports it itself.
  test("a record the session's shutdown cuts says so, not that it ran to its own stop", async () => {
    const hangs = Promise.withResolvers<{ artifact: { id: string } }>();
    const dispatch = fakeDispatch({ projectArtifact: () => hangs.promise });
    const g = harness({ client: dispatch.client });
    const answered = g.write({ ...UNMARKED, asksCarried: 2 });
    for (let i = 0; i < 20; i++) await Promise.resolve();
    g.recorder.cut();
    expect(await answered).toBe(RECORD_SHUT_DOWN);
    // The request behind it is ended with the record, and the write is never retried.
    expect(g.asked[0]?.signal.aborted).toBe(true);
    expect(dispatch.uploads).toHaveLength(1);
    hangs.resolve({ artifact: { id: "doc-1" } });
  });

  test("after the cut a record is given up before it asks Dispatch for anything, and settled answers", async () => {
    const dispatch = fakeDispatch();
    const g = harness({ client: dispatch.client });
    g.recorder.cut();
    expect(await g.write(REFUSAL)).toBe(RECORD_SHUT_DOWN);
    expect(g.asked).toEqual([]);
    expect(dispatch.uploads).toEqual([]);
    // Nothing is pending, so the shutdown's wait for the records ends at once.
    await g.recorder.settled();
  });

  test("settled waits for the record in flight, and answers once the cut has given it up", async () => {
    const hangs = Promise.withResolvers<{ artifact: { id: string } }>();
    const dispatch = fakeDispatch({ projectArtifact: () => hangs.promise });
    const g = harness({ client: dispatch.client });
    const answered = g.write(REFUSAL);
    for (let i = 0; i < 20; i++) await Promise.resolve();
    let settled = false;
    const waiting = g.recorder.settled().then(() => {
      settled = true;
    });
    for (let i = 0; i < 20; i++) await Promise.resolve();
    expect(settled).toBe(false);
    g.recorder.cut();
    await waiting;
    expect(settled).toBe(true);
    expect(await answered).toBe(RECORD_SHUT_DOWN);
    hangs.resolve({ artifact: { id: "doc-1" } });
  });

  // #2161, reviewer round 15: a second upload of a session's document replaces the records the
  // first one holds (Dispatch versions a project artifact by name), and a fresh process serving
  // the session held no id for it, so it uploaded again.
  test("a recorder holding no id for the session appends to the document the project already holds", async () => {
    const dispatch = fakeDispatch({}, "doc-held");
    expect(await harness({ client: dispatch.client }).write(REFUSAL)).toBeUndefined();
    expect(dispatch.uploads).toHaveLength(1);

    // A second process serving the same session: it holds no document id of its own.
    const next = harness({ client: dispatch.client });
    expect(await next.write({ ...REFUSAL, toolCallId: "t2" })).toBeUndefined();
    expect(dispatch.uploads).toHaveLength(1);
    expect(dispatch.edits).toMatchObject([{ id: "doc-held", op: "insert", after: "end" }]);
    // The document it found, held from here on: its next record reads the project no second time.
    expect(await next.write({ ...REFUSAL, toolCallId: "t3" })).toBeUndefined();
    expect(dispatch.lists).toEqual(["INFRA", "INFRA"]);
    expect(dispatch.edits).toHaveLength(2);
  });

  test("a document of another session's own is no match: the name is the session's", async () => {
    const dispatch = fakeDispatch({
      listProjectArtifacts: async () => [
        { id: "doc-other", name: "askgate-decisions-sess-other.md" },
      ],
    });
    expect(await harness({ client: dispatch.client }).write(REFUSAL)).toBeUndefined();
    expect(dispatch.uploads).toMatchObject([{ name: DOCUMENT }]);
    expect(dispatch.edits).toEqual([]);
  });

  // #2161, reviewer round 15: the appends of one session ran concurrently, and landed in the
  // order Dispatch answered them: "third" reached the document before "second".
  test("the records of one session land in the order they were made, however slowly Dispatch answers", async () => {
    /** Each append as Dispatch finished it, in the order it finished them. */
    const landed: string[] = [];
    /** One Dispatch answer per append, which this test gives in the reverse of the call order. */
    const answering = [
      Promise.withResolvers<void>(),
      Promise.withResolvers<void>(),
      Promise.withResolvers<void>(),
    ];
    let edits = 0;
    const dispatch = fakeDispatch({
      docEdit: async (_id, input) => {
        await answering[edits++]?.promise;
        for (const op of input.ops) landed.push(op.markdown ?? "");
        return { applied: input.ops.length };
      },
    });
    const g = harness({ client: dispatch.client });
    await g.write(REFUSAL);
    // Started back to back, as the gate starts them: each as its own handler returns.
    const answers = Promise.all([
      g.write({ ...REFUSAL, target: "first" }),
      g.write({ ...REFUSAL, target: "second" }),
      g.write({ ...REFUSAL, target: "third" }),
    ]);
    // Answered last first: the three are in flight together unless the recorder holds them back.
    for (const answer of [...answering].reverse()) {
      answer.resolve();
      for (let i = 0; i < 20; i++) await Promise.resolve();
    }
    expect(await answers).toEqual([undefined, undefined, undefined]);
    expect(landed.map((markdown) => recordsIn(markdown)[0]?.target)).toEqual([
      "first",
      "second",
      "third",
    ]);
  });
});
