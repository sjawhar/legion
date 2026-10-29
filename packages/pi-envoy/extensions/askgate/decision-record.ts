// AskGate's decision record: every rule decision, published to a Dispatch project document of the
// session's own, so that a standing session holding a Dispatch token and nothing else — no access
// to the machine the gate ran on — can count the decisions each AskGate session made. The gate's
// `advisor-gate` entry goes into the session's own Oh My Pi transcript on local disk, which no
// other session can read.
//
// What it records: every gated call whose rules check ran, which is every entry carrying a
// `ruleOutcome` — the refusals, the denominator of the error rate, and an upload that went out
// with none of its asks judged (`unjudged`) — every call the agent
// sent anyway, whose `outcome` is `rebuttal` or `breaker`: the gate answers those before any model
// call, so no rules check judged them and their `ruleOutcome` is null, and the record's `target`
// is what pairs each with the refusal it overrode — and every write that reached the `dispatch`
// CLI outside the form the gate reads, whose `outcome` is `unmarked`, where the write carries at
// least one ask for a person (`asksCarried`). A call that is none of those (no new ask, an
// unreadable command, a session with no model role) records nothing.
//
// What it never carries: no ask text, no quote, no reason, no reviewed arguments. `DecisionRecord`
// is the whole of it, which is why a document of these needs no privacy decision before it ships.
//
// Where: one document per session, `askgate-decisions-<session id>.md`, in the project
// OMP_ASKGATE_RECORD_PROJECT names, else the one LEGION_PROJECT names, else nowhere — with
// neither set the gate makes no Dispatch call at all and the call costs nothing. The session's
// first decision uploads the document with its header paragraph and that first record; every
// later one appends a fenced block through `docEdit`, because Dispatch stores a second upload of
// one name as a new version of that same document (`findArtifactByName`,
// `packages/envoy/internal/dispatch/api/artifacts.go`), which the reader reads in place of
// everything the first upload held. The document's id is held in memory for the session; holding
// none — a process that never uploaded this session's document, or one whose upload was cut —
// the recorder finds the document among the project's by its name and appends to it, and uploads
// only when the project holds none. One session's records are written one at a time, each after
// the one before it has settled, so they land in the order the gate made them.
//
// What is lost, and is left visible in the reader's output rather than hidden: a session killed
// between a decision and its write loses that one record; a session that never reached a gated
// call writes no document at all; a write that fails, that outlives its stop or that the
// session's shutdown cuts loses its record, and one given up that way may have landed all the
// same, so a record its caller calls lost can still be in the document; and two processes that
// write one session's first record at the same moment each find no document and upload one,
// where the later upload's version replaces the record the earlier one carried. The caller says
// where a loss goes: the core appends an `advisor-gate-record` entry for the gated call it
// belongs to, and the host leaves `recordError` on the `unmarked` entry it writes.
import type { CreateArtifactInput, EditArtifactInput } from "@legion/contracts";
import pkg from "../../package.json";

/**
 * The record's own stop, armed when the recorder takes a decision it records: a write that has not
 * answered by then is given up, and never retried.
 */
export const RECORD_TIMEOUT_MS = 5_000;

/** The `recordError` of a record the session's shutdown cut short, before its own stop fell. */
export const RECORD_SHUT_DOWN = "the session shut down before Dispatch answered the record";

/**
 * What every decision document's name starts with, the session's own id after it: the one name the
 * upload writes and a reader of a project's documents finds them by.
 */
export const DECISION_DOCUMENT_PREFIX = "askgate-decisions-";

/** The plugin that wrote the record, as the record names it: the manifest's own name and version. */
const PLUGIN = `${pkg.name}@${pkg.version}`;

/** Where the session ran: a Legion role's pod, or a person's own machine. */
export type RecordSource = "pod" | "machine";

/**
 * What the record needs of a Dispatch client, which is the three calls it makes. Declared
 * structurally, over the real input types, so the host's own `DispatchClient` satisfies it and a
 * test drives it with a plain object.
 */
export interface RecordClient {
  /**
   * The project's documents, which the recorder reads to find the session's own by name: the one
   * read that tells it whether an upload would make the document or replace it.
   */
  listProjectArtifacts(
    project: string
  ): Promise<readonly { readonly id: string; readonly name: string }[]>;
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
  /** Set exactly on a call whose rules check ran; unset on an override, which none judged. */
  ruleOutcome?: string;
  ruleError?: string;
  ruleTextMissing?: true;
  ruleAsksCut?: number;
  ruleAsksPartial?: true;
  ruleLatencyMs?: number;
  charterLatencyMs?: number;
  /**
   * How many asks for a person an unmarked write carries, counted by the `dispatch` CLI from the
   * write's own text; 0 or unset on every call the gate itself judged.
   */
  asksCarried?: number;
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
  /** The rules check's outcome, and null on an override: the call no rules check judged. */
  ruleOutcome: string | null;
  ruleError: string | null;
  ruleTextMissing: boolean;
  ruleAsksCut: number;
  /**
   * The call was judged on part of its asks, or on none with `ruleOutcome` `unjudged`: an upload
   * whose stored version could not be read.
   */
  ruleAsksPartial: boolean;
  decision: string;
  outcome: string;
  verdictMode: string;
  /**
   * How many asks for a person the write carries, on an `unmarked` record; 0 on every other,
   * whose asks the gate itself read.
   */
  asksCarried: number;
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
}

/**
 * Writes a session's decisions to its Dispatch document, one write of a session at a time, each
 * under a stop of its own. The gate's own records and the host's records of unmarked writes go
 * through one of these, so a session's decisions reach one document.
 */
export interface DecisionRecorder {
  /**
   * Records one decision, under a stop of its own at `RECORD_TIMEOUT_MS`, armed only for a
   * decision there is something to record of. Answers the text for the entry's `recordError` when
   * the write failed, outlived its stop or was cut, and undefined when it recorded or when there
   * was nothing to record.
   */
  record(request: RecordRequest): Promise<string | undefined>;
  /**
   * Gives up every record in flight with `RECORD_SHUT_DOWN`, and gives up every later one as it
   * starts: the session is shutting down, and no record may hold it open past that.
   */
  cut(): void;
  /** Settles once every record in flight has answered its caller: what a shutdown waits for. */
  settled(): Promise<void>;
}

/** A variable that is set and holds something, or nothing; the three settings below read alike. */
function nonEmpty(value: string | undefined): string | undefined {
  const trimmed = value?.trim();
  return trimmed === undefined || trimmed === "" ? undefined : trimmed;
}

export function createDecisionRecorder(deps: RecordDeps): DecisionRecorder {
  /**
   * Each session's document, once this process knows its id: a session it serves again appends to
   * the document it already holds, and reads nothing of the project to find it.
   */
  const documents = new Map<string, string>();
  /**
   * The write each session's next record waits for: one write of a session at a time, so the
   * document's upload has answered before anything appends to it and the appends reach Dispatch
   * in the order the gate made them. This promise never rejects.
   */
  const writing = new Map<string, Promise<void>>();
  /** Every record whose caller is still waiting: what `settled` waits for. */
  const pending = new Set<Promise<void>>();
  /** The one call that ends each record in flight, with the reason its caller is told. */
  const stops = new Set<(why: string) => void>();
  /** Set by `cut`: the session is shutting down, so a record that starts after it is given up at once. */
  let cutShort = false;

  const record = async ({
    entry,
    sessionId,
    directory,
  }: RecordRequest): Promise<string | undefined> => {
    // The calls whose rules check ran, the calls the agent sent anyway — a rebuttal or the
    // breaker, which the gate answers with no check of its own — and the writes that reached the
    // CLI outside the form the gate reads and carry an ask for a person, are the decisions to
    // count; a call that is none of them is none. An unmarked write carrying no ask is a write
    // no person waits on, and the count of those is the session's own business.
    const ruleOutcome = entry.ruleOutcome ?? null;
    const override = entry.outcome === "rebuttal" || entry.outcome === "breaker";
    const asksCarried = entry.asksCarried ?? 0;
    const unmarked = entry.outcome === "unmarked" && asksCarried > 0;
    if (ruleOutcome === null && !override && !unmarked) return undefined;
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
      ruleAsksPartial: entry.ruleAsksPartial === true,
      decision: entry.decision,
      outcome: entry.outcome,
      verdictMode: entry.verdictMode,
      asksCarried,
      ruleLatencyMs: entry.ruleLatencyMs ?? null,
      charterLatencyMs: entry.charterLatencyMs ?? null,
      plugin: PLUGIN,
    };
    const block = `\`\`\`json\n${JSON.stringify(record)}\n\`\`\``;
    const name = `${DECISION_DOCUMENT_PREFIX}${sessionId}.md`;
    // Past the cut the session is sealing: this record is given up before it asks Dispatch for
    // anything, and arms no stop of its own, since what it would hold the session open for is
    // already past.
    if (cutShort) return RECORD_SHUT_DOWN;
    // The record's own stop, armed here and nowhere earlier: an entry the lines above record
    // nothing of leaves no timer behind. The abort ends the request behind it, and the first
    // reason given — this stop, or the session's shutdown — is the one the caller is told.
    const stop = new AbortController();
    const { promise: stopped, resolve: giveUp } = Promise.withResolvers<string>();
    const end = (why: string) => {
      if (stop.signal.aborted) return;
      // The caller's answer is given before the abort, so the reason it reads is this one and
      // never whatever the aborted request rejects with.
      giveUp(why);
      stop.abort(why);
    };
    stops.add(end);
    const timer = setTimeout(
      () => end(`Dispatch did not answer the record within ${RECORD_TIMEOUT_MS} ms`),
      RECORD_TIMEOUT_MS
    );
    // Inside the write, so a Dispatch configuration that refuses to resolve fails as any other
    // write does: the record is lost and the entry says why.
    const write = async (): Promise<string | undefined> => {
      const dispatch = deps.dispatchClient(directory, stop.signal);
      if (dispatch === undefined) return "the session has no Dispatch configuration";
      const client = dispatch.client;
      // A bearer caller names its own session or Dispatch refuses the write (ACTOR_KIND, "bearer
      // callers require actor.kind session"); the `dispatch` command names it the same way.
      const actor = { kind: "session", id: sessionId } as const;
      // The id this process holds for the session, else the document the project already holds
      // under that name. An upload of a name Dispatch knows stores a new version of that same
      // document, and the reader reads the latest version alone, so a process holding no id reads
      // the project's documents before it writes: a document a cut upload left behind, or one the
      // process that served this session before wrote, takes this record rather than losing its
      // own. A read that fails is this record's failure, as a failed write is.
      const held =
        documents.get(sessionId) ??
        (await client.listProjectArtifacts(project)).find((artifact) => artifact.name === name)?.id;
      if (held !== undefined) {
        documents.set(sessionId, held);
        await client.docEdit(held, {
          ops: [{ op: "insert", markdown: block, after: "end" }],
          actor,
        });
        return undefined;
      }
      const where = source === "pod" ? "a Legion role's pod" : "a person's machine";
      const created = await client.projectArtifact(project, {
        name,
        content:
          `AskGate rule decisions of session ${sessionId}, made on ${where} (source ` +
          `\`${source}\`). One fenced block per decision follows, each holding one JSON ` +
          `record.\n\n${block}\n`,
        actor,
      });
      // Held once Dispatch has answered it, so the session's next record appends to this document
      // and reads nothing to find it. An upload that fails or is given up holds nothing: the next
      // record looks the document up by name, and uploads again only where none landed.
      documents.set(sessionId, created.artifact.id);
      return undefined;
    };
    // This session's records, one at a time: the next starts once this write has settled, however
    // it settled, so neither a failure nor a stop leaves the session's later records unwritten.
    const queued = writing.get(sessionId);
    const mine = queued === undefined ? write() : queued.then(write, write);
    writing.set(
      sessionId,
      mine.then(
        () => undefined,
        () => undefined
      )
    );
    // The caller's own wait, which `settled` holds the shutdown for: it ends at the write, at this
    // record's stop or at the cut, whichever comes first, and never after.
    const { promise: answered, resolve: answer } = Promise.withResolvers<void>();
    pending.add(answered);
    try {
      // The write is never waited on past the stop, and never retried.
      return await Promise.race([mine, stopped]);
    } catch (error) {
      // A write that fails costs the gated call nothing: the entry carries why, and that is all.
      const message = error instanceof Error ? error.message : String(error);
      return message === "" ? "the Dispatch write failed without a message" : message;
    } finally {
      clearTimeout(timer);
      stops.delete(end);
      pending.delete(answered);
      answer();
    }
  };

  return {
    record,
    cut: () => {
      cutShort = true;
      for (const end of [...stops]) end(RECORD_SHUT_DOWN);
    },
    settled: async () => {
      // A record that starts while this waits is waited for as well; past the cut each of those is
      // given up as it starts, so the wait ends with the records the session had.
      while (pending.size > 0) await Promise.allSettled([...pending]);
    },
  };
}
