import { describe, expect, test } from "bun:test";
import pkg from "../../package.json";
import type { GateEntry } from "./askgate-core";
import {
  createDecisionRecorder,
  RECORD_TIMEOUT_MS,
  type RecordClient,
  type RecordDeps,
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

/** The Dispatch a record is written to: what each call was given, and what it answers. */
function fakeDispatch(answers: Partial<RecordClient> = {}, id = "doc-1") {
  const uploads: Array<{ project: string; name: string; content: string }> = [];
  const edits: Array<{ id: string; op: string; markdown: string; after?: string }> = [];
  const client: RecordClient = {
    projectArtifact: async (project, input) => {
      uploads.push({
        project,
        name: input.name,
        content: "content" in input ? input.content : "",
      });
      return answers.projectArtifact
        ? await answers.projectArtifact(project, input)
        : { artifact: { id } };
    },
    docEdit: async (document, input) => {
      for (const op of input.ops)
        edits.push({ id: document, op: op.op, markdown: op.markdown ?? "", after: op.after });
      return answers.docEdit
        ? await answers.docEdit(document, input)
        : { applied: input.ops.length };
    },
  };
  return { uploads, edits, client };
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
  const record = createDecisionRecorder(deps);
  return {
    asked,
    write: (entry: GateEntry, signal = new AbortController().signal, sessionId = SESSION) =>
      record({ entry, sessionId, directory: "/work", signal }),
  };
}

/** The JSON records a document's markdown holds, in order. */
const recordsIn = (markdown: string) =>
  [...markdown.matchAll(/```json\n(.*)\n```/g)].map(
    ([, line]) => JSON.parse(line as string) as Record<string, unknown>
  );

describe("the AskGate decision record", () => {
  test("the session's first decision uploads its document, and the second appends to that document", async () => {
    const dispatch = fakeDispatch({}, "doc-7");
    const g = harness({ client: dispatch.client });

    expect(await g.write(REFUSAL)).toBeUndefined();
    expect(dispatch.uploads).toHaveLength(1);
    expect(dispatch.uploads[0]?.project).toBe("INFRA");
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
      },
    ]);
    expect(recordsIn(dispatch.edits[0]?.markdown ?? "")).toHaveLength(1);
  });

  test("two sessions write two documents, each named for its own session", async () => {
    const dispatch = fakeDispatch();
    const g = harness({ client: dispatch.client });
    await g.write(REFUSAL);
    await g.write(REFUSAL, new AbortController().signal, "sess-other");
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

    expect(Object.keys(record)).toEqual([
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
      "decision",
      "outcome",
      "verdictMode",
      "ruleLatencyMs",
      "charterLatencyMs",
      "plugin",
    ]);
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
      decision: "revise",
      outcome: "verdict",
      verdictMode: "block",
      ruleLatencyMs: 4100,
      charterLatencyMs: 5200,
      plugin: `@sjawhar/pi-envoy@${pkg.version}`,
    });
    for (const key of ["ruleAsk", "ruleQuote", "reason", "reviewedText"])
      expect(record).not.toHaveProperty(key);
    // Neither the ask's words nor the reason reach the document, header and all.
    expect(dispatch.uploads[0]?.content).not.toContain("cache path");
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

  test("the project falls back to LEGION_PROJECT, and with neither set nothing reaches Dispatch", async () => {
    const own = fakeDispatch();
    await harness({
      env: { OMP_ASKGATE_RECORD_PROJECT: "INFRA", LEGION_PROJECT: "LEGION" },
      client: own.client,
    }).write(REFUSAL);
    expect(recordsIn(own.uploads[0]?.content ?? "")[0]).toMatchObject({ project: "INFRA" });

    const pod = fakeDispatch();
    await harness({ env: { LEGION_PROJECT: "LEGION" }, client: pod.client }).write(REFUSAL);
    expect(recordsIn(pod.uploads[0]?.content ?? "")[0]).toMatchObject({ project: "LEGION" });

    const none = fakeDispatch();
    for (const env of [{}, { OMP_ASKGATE_RECORD_PROJECT: "", LEGION_PROJECT: " " }]) {
      const g = harness({ env, client: none.client });
      expect(await g.write(REFUSAL)).toBeUndefined();
      // No project is no record: the gate does not even ask for a Dispatch client.
      expect(g.asked).toEqual([]);
    }
    expect(none.uploads).toEqual([]);
  });

  test("a call whose rules check never ran writes nothing", async () => {
    const dispatch = fakeDispatch();
    const g = harness({ client: dispatch.client });
    expect(
      await g.write({ ...REFUSAL, decision: "allow", rule: undefined, ruleOutcome: undefined })
    ).toBeUndefined();
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

  test("a write that never answers is given up at the record's stop", async () => {
    const hangs = Promise.withResolvers<{ artifact: { id: string } }>();
    const dispatch = fakeDispatch({ projectArtifact: () => hangs.promise });
    const stop = new AbortController();
    const answered = harness({ client: dispatch.client }).write(REFUSAL, stop.signal);
    stop.abort();
    expect(await answered).toBe(
      `Dispatch did not answer the record within ${RECORD_TIMEOUT_MS} ms`
    );
    expect(dispatch.uploads).toHaveLength(1);
    hangs.resolve({ artifact: { id: "doc-1" } });
  });

  test("a stop already closed gives the write up without waiting for it", async () => {
    const hangs = Promise.withResolvers<{ artifact: { id: string } }>();
    const dispatch = fakeDispatch({ projectArtifact: () => hangs.promise });
    const stop = new AbortController();
    stop.abort();
    expect(await harness({ client: dispatch.client }).write(REFUSAL, stop.signal)).toBe(
      `Dispatch did not answer the record within ${RECORD_TIMEOUT_MS} ms`
    );
    hangs.resolve({ artifact: { id: "doc-1" } });
  });
});
