// AskGate's decision record: every rule decision, published to a Dispatch project document of the
// session's own, so that a standing session holding a Dispatch token and nothing else — no access
// to the machine the gate ran on — can count the decisions each AskGate session made. The gate's
// `advisor-gate` entry goes into the session's own Oh My Pi transcript on local disk, which no
// other session can read.
//
// What it records: every gated call whose rules check ran, which is every entry carrying a
// `ruleOutcome` — the refusals, the overrides, and the denominator of the error rate. A call with
// no rules check in it (no new ask, an unreadable command, a rebuttal, the breaker, a session with
// no model role) records nothing.
//
// What it never carries: no ask text, no quote, no reason, no reviewed arguments. `DecisionRecord`
// is the whole of it, which is why a document of these needs no privacy decision before it ships.
//
// Where: one document per session, `askgate-decisions-<session id>.md`, in the project
// OMP_ASKGATE_RECORD_PROJECT names, else the one LEGION_PROJECT names, else nowhere — with
// neither set the gate makes no Dispatch call at all and the call costs nothing. The session's
// first decision uploads the document with its header paragraph and that first record; every
// later one appends a fenced block through `docEdit`, because posting the same name twice gives
// Dispatch a second document rather than a new version of the first. The document's id is held in
// memory for the session, so the name is never posted twice.
//
// What is lost, and is left visible in the reader's output rather than hidden: a session killed
// between a decision and its upload loses that one record; a session that never reached a gated
// call writes no document at all; and a write that fails or outlives its stop loses its record
// too, leaving `recordError` on the gate's entry for whoever can read that transcript.
import type { CreateArtifactInput, EditArtifactInput } from "@legion/contracts";
import pkg from "../../package.json";

/** The record's own stop: a write that has not answered by then is given up, and never retried. */
export const RECORD_TIMEOUT_MS = 5_000;

/** The plugin that wrote the record, as the record names it. */
const PLUGIN = `@sjawhar/pi-envoy@${pkg.version}`;

/** Where the session ran: a Legion role's pod, or a person's own machine. */
export type RecordSource = "pod" | "machine";

/**
 * What the record needs of a Dispatch client, which is the two calls it makes. Declared
 * structurally, over the real input types, so the host's own `DispatchClient` satisfies it and a
 * test drives it with a plain object.
 */
export interface RecordClient {
  projectArtifact(
    project: string,
    input: CreateArtifactInput
  ): Promise<{ artifact: { id: string } }>;
  docEdit(id: string, input: EditArtifactInput): Promise<unknown>;
}

export interface RecordDeps {
  env: Record<string, string | undefined>;
  now: () => number;
  /**
   * The session's Dispatch as the directory given configures it, every request ending with the
   * signal given; undefined when the session has no Dispatch configuration.
   */
  dispatchClient: (
    directory: string,
    signal: AbortSignal
  ) => { readonly client: RecordClient } | undefined;
}

/**
 * The fields of the gate's `advisor-gate` entry that the record carries: a subset of its
 * `GateEntry`, declared structurally so this module stays free of the core.
 */
export interface RecordedDecision {
  tool: string;
  decision: string;
  outcome: string;
  verdictMode: string;
  target?: string;
  rule?: string;
  /** Set exactly on a call whose rules check ran, which is the call this module records. */
  ruleOutcome?: string;
  ruleError?: string;
  ruleTextMissing?: true;
  ruleAsksCut?: number;
  ruleLatencyMs?: number;
  charterLatencyMs?: number;
}

/** One record of a decision document: its wire shape, in the key order the line is written in. */
export interface DecisionRecord {
  /** RFC 3339, UTC. */
  time: string;
  session: string;
  source: RecordSource;
  project: string;
  tool: string;
  target: string | null;
  rule: string | null;
  ruleOutcome: string;
  ruleError: string | null;
  ruleTextMissing: boolean;
  ruleAsksCut: number;
  decision: string;
  outcome: string;
  verdictMode: string;
  ruleLatencyMs: number | null;
  charterLatencyMs: number | null;
  plugin: string;
}

export interface RecordRequest {
  entry: RecordedDecision;
  /** The session the gate ran in: its document's name, and the record's `session`. */
  sessionId: string;
  /** The directory whose Dispatch configuration the write uses: the session's own. */
  directory: string;
  /** The record's stop, armed by the caller at `RECORD_TIMEOUT_MS`. */
  signal: AbortSignal;
}

/**
 * Records one decision. Answers the text for the entry's `recordError` when the write failed or
 * was given up, and undefined when it recorded or when there was nothing to record.
 */
export type DecisionRecorder = (request: RecordRequest) => Promise<string | undefined>;

/** A variable that is set and holds something, or nothing; the three settings below read alike. */
function nonEmpty(value: string | undefined): string | undefined {
  const trimmed = value?.trim();
  return trimmed === undefined || trimmed === "" ? undefined : trimmed;
}

export function createDecisionRecorder(deps: RecordDeps): DecisionRecorder {
  /**
   * Each session's document: the upload in flight, or the id it answered. One entry per session
   * this process serves, so a session the host returns to appends to the document it already made.
   */
  const documents = new Map<string, Promise<string>>();

  return async ({ entry, sessionId, directory, signal }) => {
    // Exactly the calls whose rules check ran; a call without one is no decision to count.
    const ruleOutcome = entry.ruleOutcome;
    if (ruleOutcome === undefined) return undefined;
    // The session's own setting, else the Legion pod's project — a pod has no envoy.json, so its
    // environment is the only setting it can carry — else no record at all. Upper-cased, because a
    // Dispatch project key is `^[A-Z][A-Z0-9]{1,9}$` and every other spelling is a 404, while a
    // pod's LEGION_PROJECT is the daemon's lower-cased project name.
    const project = (
      nonEmpty(deps.env.OMP_ASKGATE_RECORD_PROJECT) ?? nonEmpty(deps.env.LEGION_PROJECT)
    )?.toUpperCase();
    if (project === undefined) return undefined;
    // A Legion role's session is the one the daemon gives an issue to work, so LEGION_ISSUE is the
    // test that tells a pod's decisions from those made on a person's machine; the counts split on it.
    const source: RecordSource = nonEmpty(deps.env.LEGION_ISSUE) === undefined ? "machine" : "pod";
    const record: DecisionRecord = {
      time: new Date(deps.now()).toISOString(),
      session: sessionId,
      source,
      project,
      tool: entry.tool,
      target: entry.target ?? null,
      rule: entry.rule ?? null,
      ruleOutcome,
      ruleError: entry.ruleError ?? null,
      ruleTextMissing: entry.ruleTextMissing === true,
      ruleAsksCut: entry.ruleAsksCut ?? 0,
      decision: entry.decision,
      outcome: entry.outcome,
      verdictMode: entry.verdictMode,
      ruleLatencyMs: entry.ruleLatencyMs ?? null,
      charterLatencyMs: entry.charterLatencyMs ?? null,
      plugin: PLUGIN,
    };
    const block = `\`\`\`json\n${JSON.stringify(record)}\n\`\`\``;
    const held = documents.get(sessionId);
    // Inside the write, so a Dispatch configuration that refuses to resolve fails as any other
    // write does: the record is lost and the entry says why.
    const write = async (): Promise<string | undefined> => {
      const dispatch = deps.dispatchClient(directory, signal);
      if (dispatch === undefined) return "the session has no Dispatch configuration";
      // A bearer caller names its own session or Dispatch refuses the write (ACTOR_KIND, "bearer
      // callers require actor.kind session"); the `dispatch` command names it the same way.
      const actor = { kind: "session", id: sessionId } as const;
      if (held !== undefined) {
        await dispatch.client.docEdit(await held, {
          ops: [{ op: "insert", markdown: block, after: "end" }],
          actor,
        });
        return undefined;
      }
      const where = source === "pod" ? "a Legion role's pod" : "a person's machine";
      const created = dispatch.client
        .projectArtifact(project, {
          name: `askgate-decisions-${sessionId}.md`,
          content:
            `AskGate rule decisions of session ${sessionId}, made on ${where} (source ` +
            `\`${source}\`). One fenced block per decision follows, each holding one JSON ` +
            `record.\n\n${block}\n`,
          actor,
        })
        .then((answer) => answer.artifact.id);
      // Held before it answers, so a second decision of the same session appends to this document
      // rather than posting its name again, which Dispatch answers with a second document.
      documents.set(sessionId, created);
      // An upload that fails holds nothing, and one given up fails too, since the stop aborts the
      // request behind it: that record is lost, and the session's next decision uploads again.
      created.catch(() => {
        if (documents.get(sessionId) === created) documents.delete(sessionId);
      });
      await created;
      return undefined;
    };
    // The write is never waited on past the stop the caller armed, and never retried.
    const { promise: stopped, resolve: giveUp } = Promise.withResolvers<string>();
    const given = `Dispatch did not answer the record within ${RECORD_TIMEOUT_MS} ms`;
    if (signal.aborted) giveUp(given);
    else signal.addEventListener("abort", () => giveUp(given), { once: true });
    try {
      return await Promise.race([write(), stopped]);
    } catch (error) {
      // A write that fails costs the gated call nothing: the entry carries why, and that is all.
      const message = error instanceof Error ? error.message : String(error);
      return message === "" ? "the Dispatch write failed without a message" : message;
    }
  };
}
