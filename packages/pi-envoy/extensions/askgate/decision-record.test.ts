import { describe, expect, jest, test } from "bun:test";
import { createHash } from "node:crypto";
import {
  IDEMPOTENCY_KEY_COMMITTED,
  IDEMPOTENCY_KEY_IN_FLIGHT,
  IDEMPOTENCY_KEY_PATTERN,
  IDEMPOTENCY_KEY_REUSED,
} from "@legion/contracts";
import {
  DispatchServiceError,
  DispatchWriteLandedError,
  DispatchWriteUnsettledError,
} from "@legion/envoy-client/dispatch-http";
import pkg from "../../package.json";
import type { GateEntry } from "./askgate-core";
import {
  createDecisionRecorder,
  RECORD_CUT_DELAY_MS,
  RECORD_SHUT_DOWN,
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

/** Dispatch's answer that the project holds no document of that name, as the client throws it. */
function notFound(name: string): DispatchServiceError {
  return new DispatchServiceError(
    "ARTIFACT_NOT_FOUND",
    404,
    `document "${name}" not found by slug; project documents: none`
  );
}

/** What the client throws when a write of its own ended with no definitive answer while Dispatch
 *  may still be running it: its keyed retry window ran out, or the call's deadline did. */
function unsettled(): DispatchWriteUnsettledError {
  return new DispatchWriteUnsettledError(
    new DispatchServiceError(
      IDEMPOTENCY_KEY_IN_FLIGHT,
      409,
      "an earlier request with this key is still running"
    ),
    5
  );
}

/** One key's one run, as Dispatch holds it. */
interface KeyedRun {
  /** The request the key was used for: a different one under it is `422 KEY_REUSED`. */
  digest: string;
  /** Settles when the run finishes on Dispatch, however long after its caller gave it up. */
  finished: Promise<void>;
  /** What the finished run answered, replayed to every later request under the key. */
  answer?: unknown;
  /** The run wrote and its answer is gone: every later request under the key is `409
   *  KEY_COMMITTED`. */
  committed?: boolean;
}

/**
 * The Dispatch a record is written to: what each call was given, what it answered, and the text
 * each document holds. It follows the two rules this recorder turns on.
 *
 * An upload of a name it already holds stores a new version of that same document, which the
 * reader reads in place of every version before it, so the upload's text **replaces** what the
 * document held (`findArtifactByName`, `packages/envoy/internal/dispatch/api/artifacts.go`).
 *
 * A write under an `Idempotency-Key` is one intent, run once (`keyedWrites`,
 * `packages/envoy/internal/dispatch/api/request_keys.go`): a repeat while the first run lives is
 * `409 KEY_IN_FLIGHT`, which the client asks again under the key until that run finishes — the
 * wait here stands for those attempts — a repeat after it finished is replayed and runs nothing,
 * one whose run committed and lost its answer is `409 KEY_COMMITTED`, and a repeat carrying a
 * different body is `422 KEY_REUSED`. A write with no key is run as Dispatch always ran it.
 */
function fakeDispatch(answers: Partial<RecordClient> = {}, id = "doc-1") {
  const uploads: Array<{
    project: string;
    name: string;
    content: string;
    actor?: unknown;
    key?: string;
  }> = [];
  const edits: Array<{
    id: string;
    op: string;
    markdown: string;
    after?: string;
    actor?: unknown;
    key?: string;
  }> = [];
  /** Every read of a document by name: the name it asked for. */
  const reads: string[] = [];
  /** Each document by name: its id, and the text it holds now. */
  const stored = new Map<string, { id: string; markdown: string }>();
  /** Every key Dispatch has been sent, and the one run each stands for. */
  const keys = new Map<string, KeyedRun>();
  /** The creates Dispatch took and has not finished: `commitHeld` finishes the first of them. */
  const heldRuns: Array<(answerLost: boolean) => void> = [];
  /** How many creates Dispatch takes and leaves running rather than finishes. */
  let holdCreates = 0;

  /** One write under its key, by the rules above; `hold` is Dispatch taking the run and leaving
   *  it going while its caller is given no definitive answer. */
  const keyed = async <T>(
    key: string | undefined,
    digest: string,
    run: () => T,
    hold = false
  ): Promise<T> => {
    const seen = key === undefined ? undefined : keys.get(key);
    if (seen !== undefined) {
      if (seen.digest !== digest) {
        throw new DispatchServiceError(
          IDEMPOTENCY_KEY_REUSED,
          422,
          "this key was used for another request"
        );
      }
      await seen.finished;
      if (seen.committed === true) {
        throw new DispatchWriteLandedError(
          IDEMPOTENCY_KEY_COMMITTED,
          409,
          "POST",
          "https://dispatch.test"
        );
      }
      // The first run's own answer, replayed: this fake stores it as it answered it.
      return seen.answer as T;
    }
    const { promise: finished, resolve: finish } = Promise.withResolvers<void>();
    const mine: KeyedRun = { digest, finished };
    if (key !== undefined) keys.set(key, mine);
    if (!hold) {
      const answer = run();
      mine.answer = answer;
      finish();
      return answer;
    }
    heldRuns.push((answerLost) => {
      mine.answer = run();
      mine.committed = answerLost;
      finish();
    });
    throw unsettled();
  };

  const client: RecordClient = {
    getProjectArtifact: async (project, name) => {
      reads.push(name);
      if (answers.getProjectArtifact) return await answers.getProjectArtifact(project, name);
      const held = stored.get(name);
      if (held === undefined) throw notFound(name);
      return { id: held.id };
    },
    projectArtifact: async (project, input, key) => {
      const content = "content" in input ? input.content : "";
      uploads.push({ project, name: input.name, content, actor: input.actor, key });
      if (answers.projectArtifact) return await answers.projectArtifact(project, input, key);
      const hold = holdCreates > 0;
      if (hold) holdCreates -= 1;
      return await keyed(
        key,
        `POST ${project}/artifacts ${input.name} ${content}`,
        () => {
          // The upload's text is the document's from here on: the name it carries is the document.
          const made = { id: stored.get(input.name)?.id ?? id, markdown: content };
          stored.set(input.name, made);
          return { artifact: { id: made.id } };
        },
        hold
      );
    },
    docEdit: async (document, input, key) => {
      for (const op of input.ops)
        edits.push({
          id: document,
          op: op.op,
          markdown: op.markdown ?? "",
          after: op.after,
          actor: input.actor,
          key,
        });
      if (answers.docEdit) return await answers.docEdit(document, input, key);
      return await keyed(key, `POST ${document}/edits ${JSON.stringify(input.ops)}`, () => {
        const held = [...stored.values()].find((doc) => doc.id === document);
        if (held === undefined) {
          throw new DispatchServiceError("ARTIFACT_NOT_FOUND", 404, `no document ${document}`);
        }
        for (const op of input.ops) held.markdown += `\n${op.markdown ?? ""}\n`;
        return { applied: input.ops.length };
      });
    },
  };
  return {
    uploads,
    edits,
    reads,
    keys,
    stored,
    client,
    /** The text the document of that name holds now. */
    markdown: (name = DOCUMENT) => stored.get(name)?.markdown ?? "",
    /** Dispatch takes the next create and leaves it running: its caller is given no definitive
     *  answer, as the call's deadline or the client's keyed window gives it. */
    hold: () => {
      holdCreates += 1;
    },
    /** Finishes the create Dispatch took and left running, as Dispatch commits one long after its
     *  client gave it up; `answerLost` drops its answer, so a repeat under its key is
     *  `409 KEY_COMMITTED`. */
    commitHeld: (answerLost = false) => {
      const run = heldRuns.shift();
      if (run === undefined) throw new Error("no held run to commit");
      run(answerLost);
    },
  };
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
    /** One record; `id` tells it from the session's others, as the gated call's own id does. */
    write: (entry: GateEntry | RecordedDecision, sessionId = SESSION, id = "t1") =>
      recorder.record({ entry, sessionId, directory: "/work", id }),
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
  "id",
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

/**
 * The bytes every create of this session's document carries: the header alone, the same for every
 * attempt and every process, which is what lets one key stand for them all.
 */
const HEADER =
  `AskGate rule decisions of session ${SESSION}. One fenced block per decision follows, ` +
  "each holding one JSON record.\n";

/** The key the create of this session's document carries, as the recorder derives it. */
const CREATE_KEY = `askgate-doc.${createHash("sha256").update(`INFRA\n${DOCUMENT}`).digest("hex")}`;

/** The key one record's append carries: the sha256 of the session and that record's own id. */
const appendKey = (id: string) =>
  `askgate-rec.${createHash("sha256").update(`${SESSION}\n${id}`).digest("hex")}`;

describe("the AskGate decision record", () => {
  test("the session's first decision creates its document and appends its record, and the second appends to that document", async () => {
    const dispatch = fakeDispatch({}, "doc-7");
    const g = harness({ client: dispatch.client });

    expect(await g.write(REFUSAL)).toBeUndefined();
    expect(dispatch.uploads).toHaveLength(1);
    expect(dispatch.uploads[0]?.project).toBe("INFRA");
    // A bearer caller that names no session of its own is refused ACTOR_KIND by Dispatch.
    expect(dispatch.uploads[0]?.actor).toEqual({ kind: "session", id: SESSION });
    expect(dispatch.uploads[0]?.name).toBe(DOCUMENT);
    // The create carries the header and no record: the records are the appends, every one of them.
    const content = dispatch.uploads[0]?.content ?? "";
    expect(content).toBe(HEADER);
    expect(recordsIn(content)).toEqual([]);
    expect(recordsIn(dispatch.markdown())).toMatchObject([{ session: SESSION, rule: "no_change" }]);

    expect(
      await g.write({ ...REFUSAL, toolCallId: "t2", rule: undefined }, SESSION, "t2")
    ).toBeUndefined();
    expect(dispatch.uploads).toHaveLength(1);
    // Each record is one insert at the end of the document, and nothing else is written.
    expect(dispatch.edits.map(({ id, op, after, actor }) => ({ id, op, after, actor }))).toEqual([
      { id: "doc-7", op: "insert", after: "end", actor: { kind: "session", id: SESSION } },
      { id: "doc-7", op: "insert", after: "end", actor: { kind: "session", id: SESSION } },
    ]);
    expect(dispatch.edits.map(({ markdown }) => recordsIn(markdown).length)).toEqual([1, 1]);
    expect(recordsIn(dispatch.markdown()).map((record) => record.rule)).toEqual([
      "no_change",
      null,
    ]);
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
    expect(dispatch.edits.map((edit) => edit.id)).toEqual(["doc-1", "doc-1"]);
  });

  test("the record holds exactly the contract's keys, and no ask text, quote or reason", async () => {
    const dispatch = fakeDispatch();
    await harness({ client: dispatch.client }).write(REFUSAL, SESSION, "t1");
    const record = recordsIn(dispatch.markdown())[0] as Record<string, unknown>;

    expect(Object.keys(record)).toEqual(RECORD_KEYS);
    expect(record).toEqual({
      time: "2026-10-25T09:15:30.250Z",
      session: SESSION,
      id: "t1",
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
    // Neither the ask's words nor the reason reach the document, its header and all.
    expect(dispatch.markdown()).not.toContain("cache path");
  });

  test("the record's plugin names the manifest's own package, not a literal that outlives a rename", async () => {
    const dispatch = fakeDispatch();
    await harness({ client: dispatch.client }).write(REFUSAL);
    const record = recordsIn(dispatch.markdown())[0] as Record<string, unknown>;

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
    expect(recordsIn(dispatch.markdown())[0]).toMatchObject({
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
    expect(recordsIn(dispatch.markdown())[0]).toMatchObject({
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
    expect(recordsIn(dispatch.markdown())).toMatchObject([
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
      expect(recordsIn(dispatch.markdown())[0]).toMatchObject({ source });
      // The record carries where the session ran; the create's header does not, so that a pod and
      // a machine serving one session create the same document, byte for byte, under one key.
      expect(dispatch.uploads[0]?.content).not.toContain("source");
    }
  });

  test("the project falls back to LEGION_PROJECT, upper-cased as a project key is", async () => {
    const own = fakeDispatch();
    await harness({
      env: { OMP_ASKGATE_RECORD_PROJECT: "INFRA", LEGION_PROJECT: "legion" },
      client: own.client,
    }).write(REFUSAL);
    expect(own.uploads[0]?.project).toBe("INFRA");
    expect(recordsIn(own.markdown())[0]).toMatchObject({ project: "INFRA" });

    // A pod's LEGION_PROJECT is the daemon's lower-cased project name, and Dispatch answers 404
    // to every spelling of a project key but the uppercase one.
    const pod = fakeDispatch();
    await harness({ env: { LEGION_PROJECT: "legion" }, client: pod.client }).write(REFUSAL);
    expect(pod.uploads[0]?.project).toBe("LEGION");
    expect(recordsIn(pod.markdown())[0]).toMatchObject({ project: "LEGION" });
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
      expect(await g.write({ ...OVERRIDE, outcome }, SESSION, "t3")).toBeUndefined();
      // Appended to the refusal's own document, carrying the refusal's target: that target, in
      // that session, is the whole of what pairs the override with what it overrode.
      expect(dispatch.uploads).toHaveLength(1);
      const record = recordsIn(dispatch.edits[1]?.markdown ?? "")[0] as Record<string, unknown>;
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
      expect(dispatch.edits[1]?.markdown).not.toContain("cache path");
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
    const record = recordsIn(dispatch.markdown())[0] as Record<string, unknown>;
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

  test("a create that rejects answers its error, and the session's next decision creates again under the same key", async () => {
    const dispatch = fakeDispatch({
      projectArtifact: () => Promise.reject(new Error("dispatch: 503 Service Unavailable")),
    });
    const g = harness({ client: dispatch.client });
    expect(await g.write(REFUSAL)).toBe("dispatch: 503 Service Unavailable");
    expect(dispatch.edits).toEqual([]);
    // The failed create is held for nothing: its record is lost and the next one creates again —
    // under the key the first one used, which is the whole of why a second create is safe.
    expect(await g.write(REFUSAL, SESSION, "t2")).toBe("dispatch: 503 Service Unavailable");
    expect(dispatch.uploads).toHaveLength(2);
    const keys = dispatch.uploads.map((upload) => upload.key);
    expect(keys[0]).toMatch(IDEMPOTENCY_KEY_PATTERN);
    expect(keys[1]).toBe(keys[0] as string);
  });

  test("an append that rejects answers its error and the document is kept", async () => {
    const dispatch = fakeDispatch({ docEdit: () => Promise.reject(new Error("dispatch: 409")) });
    const g = harness({ client: dispatch.client });
    expect(await g.write(REFUSAL, SESSION, "t1")).toBe("dispatch: 409");
    expect(await g.write(REFUSAL, SESSION, "t2")).toBe("dispatch: 409");
    expect(await g.write(REFUSAL, SESSION, "t3")).toBe("dispatch: 409");
    // The document the first record created is kept and appended to, never created again.
    expect(dispatch.uploads).toHaveLength(1);
    expect(dispatch.edits).toHaveLength(3);
    expect(recordsIn(dispatch.markdown())).toEqual([]);
  });

  // #2161, reviewer round 16: under load Dispatch answered 13 of 31 records after the recorder's
  // 5 s stop, and those decisions were in no count. A record now runs until Dispatch answers it
  // or the shutdown cuts it, and the create's attempts share the document's own key, so nothing
  // here creates it twice.
  test("a record Dispatch answers long after the old 5 s stop still lands, exactly once", async () => {
    jest.useFakeTimers();
    try {
      const answers = Promise.withResolvers<{ artifact: { id: string } }>();
      // A create Dispatch is slow to answer: it makes the document when it answers, as Dispatch
      // does, and the records after it append to that one.
      const dispatch = fakeDispatch({
        projectArtifact: async (_project, input) => {
          const answer = await answers.promise;
          dispatch.stored.set(input.name, { id: answer.artifact.id, markdown: "" });
          return answer;
        },
      });
      const g = harness({ client: dispatch.client });
      const answered = g.write(REFUSAL);
      for (let i = 0; i < 20; i++) await Promise.resolve();
      // Long past the stop the recorder used to arm, and past the longest pause the client's own
      // keyed retries take: nothing here gives the write up.
      jest.advanceTimersByTime(20_000);
      for (let i = 0; i < 20; i++) await Promise.resolve();
      expect(g.asked[0]?.signal.aborted).toBe(false);
      answers.resolve({ artifact: { id: "doc-late" } });
      expect(await answered).toBeUndefined();
      expect(dispatch.uploads).toHaveLength(1);
      // The session's next record appends to the document that create made, rather than making
      // a second one that would replace it.
      expect(await g.write({ ...REFUSAL, toolCallId: "t2" }, SESSION, "t2")).toBeUndefined();
      expect(dispatch.uploads).toHaveLength(1);
      expect(dispatch.edits).toMatchObject([{ id: "doc-late" }, { id: "doc-late" }]);
    } finally {
      jest.useRealTimers();
    }
  });

  // #2161, reviewer round 16: the stop was armed when a record was queued, not when its write
  // began, so three records behind a 3 s append shared one 5 s budget and the last two were given
  // up having never been sent. No record is on a budget of its own any more.
  test("records queued behind a slow write all land, however long the one before them took", async () => {
    jest.useFakeTimers();
    try {
      /** Each append's answer, given 3 s after the write started, as a slow Dispatch answers. */
      const slow = async (): Promise<{ applied: number }> => {
        await new Promise<void>((resolve) => setTimeout(resolve, 3_000));
        return { applied: 1 };
      };
      const dispatch = fakeDispatch({ docEdit: slow });
      const g = harness({ client: dispatch.client });
      // Four records back to back, the first of them the one whose create made the document: each
      // append is sent once the one before it has settled, however long that took.
      const answers = Promise.all([
        g.write({ ...REFUSAL, target: "zeroth" }, SESSION, "t0"),
        g.write({ ...REFUSAL, target: "first" }, SESSION, "t1"),
        g.write({ ...REFUSAL, target: "second" }, SESSION, "t2"),
        g.write({ ...REFUSAL, target: "third" }, SESSION, "t3"),
      ]);
      for (let elapsed = 0; elapsed < 16_000; elapsed += 500) {
        for (let i = 0; i < 20; i++) await Promise.resolve();
        jest.advanceTimersByTime(500);
      }
      expect(await answers).toEqual([undefined, undefined, undefined, undefined]);
      expect(dispatch.edits.map(({ markdown }) => recordsIn(markdown)[0]?.target)).toEqual([
        "zeroth",
        "first",
        "second",
        "third",
      ]);
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
  // the session held no id for it, so it uploaded again. Round 16: that lookup read every
  // document of the project — 4.5 MB and 2.1 s under load — and it reads this session's own by
  // name instead.
  test("a recorder holding no id for the session reads its document by name and appends to it", async () => {
    const dispatch = fakeDispatch({}, "doc-held");
    expect(await harness({ client: dispatch.client }).write(REFUSAL)).toBeUndefined();
    expect(dispatch.uploads).toHaveLength(1);
    // The create follows the read that found no document of that name, and nothing else is read.
    expect(dispatch.reads).toEqual([DOCUMENT]);

    // A second process serving the same session: it holds no document id of its own.
    const next = harness({ client: dispatch.client });
    expect(await next.write({ ...REFUSAL, toolCallId: "t2" }, SESSION, "t2")).toBeUndefined();
    expect(dispatch.uploads).toHaveLength(1);
    expect(dispatch.edits.map(({ id, op, after }) => ({ id, op, after }))).toEqual([
      { id: "doc-held", op: "insert", after: "end" },
      { id: "doc-held", op: "insert", after: "end" },
    ]);
    // The document it found, held from here on: its next record reads nothing to find it.
    expect(await next.write({ ...REFUSAL, toolCallId: "t3" }, SESSION, "t3")).toBeUndefined();
    expect(dispatch.reads).toEqual([DOCUMENT, DOCUMENT]);
    expect(dispatch.edits).toHaveLength(3);
    // Every record of the session is in its one document, the first included.
    expect(recordsIn(dispatch.markdown()).map((record) => record.id)).toEqual(["t1", "t2", "t3"]);
  });

  // #2161, reviewer round 17 (PRRT_kwDORFy7ds6x189x): the session's first record used to upload
  // the whole document with that record in it. An upload this side gave up on — the call's
  // deadline, the client's keyed retry window, or the shutdown cut — can commit on Dispatch after
  // it, and Dispatch takes an upload of a name it holds as a new version of that document, so
  // that late commit replaced every record written since: the document held ["t1"] alone, and
  // records 2 and 3 carried no `recordError`, so no report counted them. The document is created
  // once, under a key of its own, and every record is an append.
  test("a first create this side gave up on, committed late, cannot replace the records written since", async () => {
    const dispatch = fakeDispatch({}, "doc-late");
    const g = harness({ client: dispatch.client });
    // Dispatch takes record 1's create and runs on with it; this side is given no definitive
    // answer, so that record is counted as one the session could not confirm.
    dispatch.hold();
    expect(await g.write(REFUSAL, SESSION, "t1")).toContain("may or may not have taken effect");
    expect(recordsIn(dispatch.markdown())).toEqual([]);

    // Records 2 and 3, written while Dispatch is still running that create: record 2 finds no
    // document of that name and creates again — under the key the first create used, which is
    // what Dispatch is still running — and record 3 follows it.
    const second = g.write({ ...REFUSAL, target: "second" }, SESSION, "t2");
    const third = g.write({ ...REFUSAL, target: "third" }, SESSION, "t3");
    for (let i = 0; i < 40; i++) await Promise.resolve();
    expect(dispatch.uploads).toHaveLength(2);
    // That first create commits now, long after this side gave it up; the attempt asked again
    // under its key is answered with what it made.
    dispatch.commitHeld();
    expect(await Promise.all([second, third])).toEqual([undefined, undefined]);

    // Every record this side reported written is in the document — the late commit has nothing of
    // its own to write over them with — and the two creates are two attempts of one intent.
    expect(recordsIn(dispatch.markdown()).map((record) => record.id)).toEqual(["t2", "t3"]);
    expect(dispatch.markdown()).toStartWith(HEADER);
    expect(dispatch.uploads.map((upload) => upload.key)).toEqual([CREATE_KEY, CREATE_KEY]);
  });

  test("a recorder resumed in a new process, its create committed after it died, appends to that document", async () => {
    const dispatch = fakeDispatch({}, "doc-resumed");
    const first = harness({ client: dispatch.client });
    dispatch.hold();
    expect(await first.write(REFUSAL, SESSION, "t1")).toContain("may or may not have taken effect");
    // The process serving the session goes; its create commits on Dispatch after it.
    dispatch.commitHeld();

    const next = harness({ client: dispatch.client });
    expect(await next.write({ ...REFUSAL, target: "second" }, SESSION, "t2")).toBeUndefined();
    expect(dispatch.uploads).toHaveLength(1);
    expect(dispatch.reads).toEqual([DOCUMENT, DOCUMENT]);
    expect(recordsIn(dispatch.markdown()).map((record) => record.id)).toEqual(["t2"]);
  });

  // Both answers say the create's key already stands for a write Dispatch made: the document is
  // there to be read, and a second create under a key of its own is what would replace it.
  test("a create answered KEY_COMMITTED or KEY_REUSED reads the document by name and appends, creating nothing more", async () => {
    for (const refusal of [
      new DispatchWriteLandedError(IDEMPOTENCY_KEY_COMMITTED, 409, "POST", "https://dispatch.test"),
      new DispatchServiceError(
        IDEMPOTENCY_KEY_REUSED,
        422,
        "this key was used for another request"
      ),
    ]) {
      // Another run of this create — another process's, or another version of this plugin's —
      // made the document under the same key, so the read after the refusal finds it.
      let made = false;
      const dispatch = fakeDispatch({
        getProjectArtifact: async (_project, name) => {
          if (!made) throw notFound(name);
          return { id: "doc-committed" };
        },
        projectArtifact: () => {
          made = true;
          return Promise.reject(refusal);
        },
      });
      dispatch.stored.set(DOCUMENT, { id: "doc-committed", markdown: HEADER });
      const g = harness({ client: dispatch.client });
      expect(await g.write(REFUSAL, SESSION, "t1")).toBeUndefined();
      // The read that found nothing, the one create, and the read that found what it made.
      expect(dispatch.uploads).toHaveLength(1);
      expect(dispatch.reads).toEqual([DOCUMENT, DOCUMENT]);
      expect(dispatch.edits.map((edit) => ({ id: edit.id, key: edit.key }))).toEqual([
        { id: "doc-committed", key: appendKey("t1") },
      ]);
      expect(recordsIn(dispatch.markdown()).map((record) => record.id)).toEqual(["t1"]);
    }
  });

  test("two recorders create the session's document with the same bytes, under the same key", async () => {
    const pod = fakeDispatch();
    await harness({
      env: { OMP_ASKGATE_RECORD_PROJECT: "INFRA", LEGION_ISSUE: "INFRA-168" },
      client: pod.client,
    }).write(REFUSAL, SESSION, "t1");
    const machine = fakeDispatch();
    await harness({ client: machine.client }).write(REFUSAL, SESSION, "t2");

    // Nothing that varies between two runs is in the create: no record, no time, no version, and
    // not where the session ran, which the records carry themselves.
    expect(pod.uploads[0]?.content).toBe(HEADER);
    expect(machine.uploads[0]?.content).toBe(pod.uploads[0]?.content as string);
    expect([pod.uploads[0]?.key, machine.uploads[0]?.key]).toEqual([CREATE_KEY, CREATE_KEY]);
    expect(CREATE_KEY).toMatch(IDEMPOTENCY_KEY_PATTERN);
    expect(recordsIn(pod.markdown())[0]?.source).toBe("pod");
    expect(recordsIn(machine.markdown())[0]?.source).toBe("machine");
  });

  test("each record is appended under a key of its own, so a record written twice lands once", async () => {
    const dispatch = fakeDispatch();
    expect(
      await harness({ client: dispatch.client }).write(REFUSAL, SESSION, "t1")
    ).toBeUndefined();
    // A second process writing the record again: its first attempt was given up on this side,
    // not lost, and Dispatch replays it rather than appending a second copy.
    const next = harness({ client: dispatch.client });
    expect(await next.write(REFUSAL, SESSION, "t1")).toBeUndefined();
    expect(await next.write({ ...REFUSAL, target: "second" }, SESSION, "t2")).toBeUndefined();

    expect(dispatch.edits.map((edit) => edit.key)).toEqual([
      appendKey("t1"),
      appendKey("t1"),
      appendKey("t2"),
    ]);
    expect(appendKey("t1")).toMatch(IDEMPOTENCY_KEY_PATTERN);
    expect(recordsIn(dispatch.markdown()).map((record) => record.id)).toEqual(["t1", "t2"]);
  });

  test("a read that fails for anything but a 404 is the record's failure, and uploads nothing", async () => {
    const dispatch = fakeDispatch({
      getProjectArtifact: () =>
        Promise.reject(new DispatchServiceError("INTERNAL", 500, "dispatch: 500 Internal Error")),
    });
    // An upload here would store a new version of a document that exists, replacing every record
    // it holds, so a read that says nothing about the document is a loss of this one record alone.
    expect(await harness({ client: dispatch.client }).write(REFUSAL)).toBe(
      "dispatch: 500 Internal Error"
    );
    expect(dispatch.uploads).toEqual([]);
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
    // The record whose create makes the document, appended like every other; its append is
    // answered at once, so the three below are the ones the order is read from.
    const made = g.write({ ...REFUSAL, target: "zeroth" }, SESSION, "t0");
    answering[0]?.resolve();
    expect(await made).toBeUndefined();
    // Started back to back, as the gate starts them: each as its own handler returns.
    const answers = Promise.all([
      g.write({ ...REFUSAL, target: "first" }, SESSION, "t1"),
      g.write({ ...REFUSAL, target: "second" }, SESSION, "t2"),
      g.write({ ...REFUSAL, target: "third" }, SESSION, "t3"),
    ]);
    // Answered last first: the three are in flight together unless the recorder holds them back.
    for (const answer of [...answering.slice(1)].reverse()) {
      answer.resolve();
      for (let i = 0; i < 20; i++) await Promise.resolve();
    }
    expect(await answers).toEqual([undefined, undefined, undefined]);
    expect(landed.map((markdown) => recordsIn(markdown)[0]?.target)).toEqual([
      "zeroth",
      "first",
      "second",
      "third",
    ]);
  });

  // #2161, reviewer round 16: the cut had two owners, the core's handler and the host's, chosen
  // by whether the gate loaded. The recorder owns the delay and the cut, and `shutdown` is the
  // whole of what a session_shutdown handler does with the records.
  test("shutdown gives a record in flight the cut delay, then cuts it and answers", async () => {
    jest.useFakeTimers();
    try {
      const hangs = Promise.withResolvers<{ artifact: { id: string } }>();
      const dispatch = fakeDispatch({ projectArtifact: () => hangs.promise });
      const g = harness({ client: dispatch.client });
      const answered = g.write(REFUSAL);
      for (let i = 0; i < 20; i++) await Promise.resolve();
      let shutdown = false;
      const sealing = g.recorder.shutdown().then(() => {
        shutdown = true;
      });
      // Up to the delay the record is still running: nothing has given it up.
      jest.advanceTimersByTime(RECORD_CUT_DELAY_MS - 1);
      for (let i = 0; i < 20; i++) await Promise.resolve();
      expect(shutdown).toBe(false);
      expect(g.asked[0]?.signal.aborted).toBe(false);
      jest.advanceTimersByTime(1);
      await sealing;
      expect(shutdown).toBe(true);
      expect(await answered).toBe(RECORD_SHUT_DOWN);
      expect(g.asked[0]?.signal.aborted).toBe(true);
      hangs.resolve({ artifact: { id: "doc-1" } });
    } finally {
      jest.useRealTimers();
    }
  });

  test("shutdown answers at once with no record pending, and still cuts one started while the session seals", async () => {
    jest.useFakeTimers();
    try {
      const hangs = Promise.withResolvers<{ artifact: { id: string } }>();
      const dispatch = fakeDispatch({ projectArtifact: () => hangs.promise });
      const g = harness({ client: dispatch.client });
      await g.recorder.shutdown();
      // The gate the core abandons writes its record as its own handler returns, after this
      // handler has answered: the delay runs from the shutdown all the same.
      const answered = g.write(REFUSAL);
      for (let i = 0; i < 20; i++) await Promise.resolve();
      expect(dispatch.uploads).toHaveLength(1);
      jest.advanceTimersByTime(RECORD_CUT_DELAY_MS);
      expect(await answered).toBe(RECORD_SHUT_DOWN);
      // Past the cut, a record is given up before it asks Dispatch for anything.
      expect(await g.write(REFUSAL, SESSION, "t2")).toBe(RECORD_SHUT_DOWN);
      expect(dispatch.uploads).toHaveLength(1);
      hangs.resolve({ artifact: { id: "doc-1" } });
    } finally {
      jest.useRealTimers();
    }
  });
});
