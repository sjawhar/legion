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
// neither set the gate makes no Dispatch call at all and the call costs nothing. The document is
// written once and appended to ever after: the create carries a header paragraph and no record,
// and every record, the session's first included, is a fenced block appended through `docEdit`.
// That is the one rule that keeps a record safe, because Dispatch stores an upload of a name it
// already holds as a new version of that same document (`findArtifactByName`,
// `packages/envoy/internal/dispatch/api/artifacts.go`), which the reader reads in place of
// everything the versions before it held: a second create would erase every record appended since
// the first. The document's id is held in memory for the session; holding none — a process that
// never created this session's document, or one whose create was cut — the recorder reads the
// document by its name (`getProjectArtifact`, which Dispatch resolves by filename, one request of
// a few bytes) and creates one only where that read answers 404. One session's records are
// written one at a time, each started once the one before it has settled; each carries the `time`
// the gate made it, which is what the reader counts and sorts by, since a write given up on this
// side may still be running on Dispatch's while the next one is sent.
//
// Why one create is one create, however many attempts and processes make it: the create is sent
// under an `Idempotency-Key` of the document's own — `askgate-doc.` and the sha256 of the project
// and the name — and its body is fixed for the session, the header alone, with no time, no plugin
// version and no record in it, so every attempt of it is the same request to Dispatch. Dispatch
// replays a key that finished, answers `409 KEY_IN_FLIGHT` while its first run lives (the client
// asks again under the key), `409 KEY_COMMITTED` where it landed and lost its answer, and
// `422 KEY_REUSED` for a different body (`keyedWrites`,
// `packages/envoy/internal/dispatch/api/request_keys.go`); the last two send this side back to the
// read by name and then to the append, and nothing creates a second time. A key belongs to the
// login that sent it (`requestKeyLogin`), and every Legion pod writes under the shared agent
// token, which is one login for all of them, so two pods serving one session share these keys;
// Dispatch keeps a key 24 h (`IdempotencyKeyTTL`), which outlives the 10 minutes a create's run
// can hold it (`keyedHandlerLimit`). Each append carries a key of its own, the sha256 of the
// session and the record's id, so a late append is replayed rather than written twice, and the
// same record sent again with another body gets `422 KEY_REUSED` and is not written.
//
// How long a write runs: until Dispatch has answered it or the session's shutdown cuts it. The
// client asks again under the call's key (`DISPATCH_WRITE_RETRY`, 30 s,
// `packages/envoy-client/src/dispatch-http.ts`), so a slow Dispatch is waited out rather than given
// up at a stop of the recorder's own.
//
// What is lost, and is left visible in a count rather than hidden: a session killed between a
// decision and its write loses that one record; a session that never reached a gated call writes
// no document at all; a write that fails or that the session's shutdown cuts is unconfirmed, and
// one given up that way may have landed all the same, so a record its caller could not confirm
// can still be in the document, where the reader counts it once by its `id`. What no longer
// happens: a create given up here and committed on Dispatch after a later record wrote cannot
// erase that record, since every create of this document is the one keyed create. The caller says
// where a loss goes: both the core, for a gated call, and the host, for an unmarked write, append
// an `advisor-gate-record` entry — the host's named by the record's own id, since one tool call
// can log several unmarked writes — and `scripts/askgate-report.ts` counts each as a decision
// record the session could not confirm.
import { createHash } from "node:crypto";
import {
  type CreateArtifactInput,
  type EditArtifactInput,
  IDEMPOTENCY_KEY_REUSED,
} from "@legion/contracts";
import { DispatchServiceError, DispatchWriteLandedError } from "@legion/envoy-client/dispatch-http";
import pkg from "../../package.json";

/**
 * How long the records in flight have from the start of the session's shutdown before the cut
 * gives them up. It falls a margin before the core's own wait for the session's handlers and their
 * records (`SHUTDOWN_WAIT_MS`, 1.5 s, askgate-core.ts), so the `advisor-gate-record` entry of a
 * record that could not land is appended while the handler still holds the session open.
 */
export const RECORD_CUT_DELAY_MS = 1_250;

/** The `recordError` of a record the session's shutdown cut. */
export const RECORD_SHUT_DOWN = "the session shut down before Dispatch answered the record";

/**
 * What every decision document's name starts with, the session's own id after it: the one name the
 * upload writes and a reader of a project's documents finds them by.
 */
export const DECISION_DOCUMENT_PREFIX = "askgate-decisions-";

/**
 * What the `Idempotency-Key` of a session document's create starts with, and what one record's
 * append starts with: the sha256 of what each is of follows (`intentKey`). Both fit
 * `IDEMPOTENCY_KEY_PATTERN` — letters, digits, `.`, `_`, `:` and `-`, 128 at most — at 76
 * characters.
 */
const DOCUMENT_KEY_PREFIX = "askgate-doc";
const RECORD_KEY_PREFIX = "askgate-rec";

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
   * The project's document of that name, which the recorder reads to find the session's own: one
   * request of a few bytes, where reading every document of the project took seconds and
   * megabytes under load (#2161, reviewer round 16). Dispatch resolves the reference by slug and
   * then by filename, and answers 404 where the project holds neither.
   */
  getProjectArtifact(project: string, name: string): Promise<{ readonly id: string }>;
  /**
   * The create of the session's document, under the key given: the caller's own, so every attempt
   * and every process creating this document is one intent to Dispatch.
   */
  projectArtifact(
    project: string,
    input: CreateArtifactInput,
    key: string
  ): Promise<{ artifact: { id: string } }>;
  /** One record appended, under the key given: that record's own, so a repeat is replayed. */
  docEdit(id: string, input: EditArtifactInput, key: string): Promise<unknown>;
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
  /**
   * What tells this record from every other of the session, so a reader counts a record Dispatch
   * stored twice once: the gated call's own tool call id, and for an unmarked write the id of the
   * CLI ledger line's place in its tool call, since one call can log several.
   */
  id: string;
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
  /** The record's `id`: what tells it from the session's others. */
  id: string;
}

/**
 * Writes a session's decisions to its Dispatch document, one write of a session at a time. The
 * gate's own records and the host's records of unmarked writes go through one of these, so a
 * session's decisions reach one document.
 */
export interface DecisionRecorder {
  /**
   * Records one decision. The write runs until Dispatch has answered it or the cut ends it.
   * Answers the text for the entry's `recordError` when the write failed or was cut, and
   * undefined when it recorded or when there was nothing to record.
   */
  record(request: RecordRequest): Promise<string | undefined>;
  /**
   * The session is shutting down: gives the records in flight `RECORD_CUT_DELAY_MS` to land, then
   * gives up those still running with `RECORD_SHUT_DOWN`, as it gives up every record started
   * after that as it starts, and answers once the records it waited for have answered their
   * callers. The one owner of the cut, whether or not the gate loaded: `settled` is all the core
   * waits on.
   */
  shutdown(): Promise<void>;
  /**
   * Gives up every record in flight with `RECORD_SHUT_DOWN`, and gives up every later one as it
   * starts: what `shutdown` does once its delay has run out.
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
   * document's upload has answered before anything appends to it, and each append is sent in the
   * order the gate made it. What a record given up on this side is doing on Dispatch's is not
   * this side's to know, so the order records land in is not claimed; each carries its own `time`.
   * This promise never rejects.
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
    id,
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
      id,
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
    // anything, since what it would hold the session open for is already past.
    if (cutShort) return RECORD_SHUT_DOWN;
    // What ends this record before Dispatch has answered it, and the only thing that does: the
    // session's shutdown, through `cut`. The abort ends the request behind it, and the caller is
    // told the reason given here rather than whatever the aborted request rejects with.
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
    // Inside the write, so a Dispatch configuration that refuses to resolve fails as any other
    // write does: the record is lost and the entry says why.
    const write = async (): Promise<string | undefined> => {
      const dispatch = deps.dispatchClient(directory, stop.signal);
      if (dispatch === undefined) return "the session has no Dispatch configuration";
      const client = dispatch.client;
      // A bearer caller names its own session or Dispatch refuses the write (ACTOR_KIND, "bearer
      // callers require actor.kind session"); the `dispatch` command names it the same way.
      const actor = { kind: "session", id: sessionId } as const;
      // The id this process holds for the session, else the document the project holds under that
      // name, else the one the create below makes: a document a cut create left behind, or one the
      // process that served this session before wrote, takes this record rather than losing its
      // own. A read that fails for any other reason than a 404 is this record's failure, as a
      // failed write is.
      const held =
        documents.get(sessionId) ?? (await document(client, project, name, sessionId, actor));
      // Held once Dispatch has answered for it, so the session's next record reads nothing to find
      // it. A create that fails or is given up holds nothing: the next record reads the document
      // by name, and creates again under the same key, which Dispatch replays rather than runs.
      documents.set(sessionId, held);
      // Every record, this session's first included, is appended: the document's text is written
      // once, by the create, so no write of this session can replace what another wrote. The key
      // is this record's own, so a late append is replayed rather than written twice, and the same
      // record sent again with another body is refused (`422 KEY_REUSED`), never written twice.
      await client.docEdit(
        held,
        { ops: [{ op: "insert", markdown: block, after: "end" }], actor },
        intentKey(RECORD_KEY_PREFIX, sessionId, id)
      );
      return undefined;
    };
    // This session's records, one at a time: the next starts once this write has settled, however
    // it settled.
    const queued = writing.get(sessionId);
    const mine = queued === undefined ? write() : queued.then(write);
    // What the next record of this session waits for: this write however it settled, so neither a
    // failure nor a cut leaves the session's later records unwritten. It never rejects, which is
    // why the wait above needs no rejection arm of its own.
    writing.set(
      sessionId,
      mine.then(
        () => undefined,
        () => undefined
      )
    );
    // The caller's own wait, which `settled` holds the shutdown for: it ends at the write or at
    // the cut, whichever comes first, and never after.
    const { promise: answered, resolve: answer } = Promise.withResolvers<void>();
    pending.add(answered);
    try {
      // The write is never waited on past the cut, and nothing here attempts it a second time:
      // the client's own attempts share one Idempotency-Key, and another from here would not.
      return await Promise.race([mine, stopped]);
    } catch (error) {
      // A write that fails costs the gated call nothing: the entry carries why, and that is all.
      const message = error instanceof Error ? error.message : String(error);
      return message === "" ? "the Dispatch write failed without a message" : message;
    } finally {
      stops.delete(end);
      pending.delete(answered);
      answer();
    }
  };

  const cut = (): void => {
    cutShort = true;
    for (const end of [...stops]) end(RECORD_SHUT_DOWN);
  };

  const settled = async (): Promise<void> => {
    // A record that starts while this waits is waited for as well; past the cut each of those is
    // given up as it starts, so the wait ends with the records the session had.
    while (pending.size > 0) await Promise.allSettled([...pending]);
  };

  return {
    record,
    cut,
    settled,
    shutdown: async () => {
      // The delay runs from here, the start of the session's shutdown, and the timer is left
      // armed when this answers early: a record the core starts while the session seals — the
      // gate it abandoned writes one as its handler returns — is cut by this same timer, which is
      // unreferenced so it holds no event loop open.
      const timer = setTimeout(cut, RECORD_CUT_DELAY_MS);
      timer.unref?.();
      await settled();
    },
  };
}

/**
 * The id of the project's document of that name, or undefined where it holds none. Dispatch's 404
 * is that answer; every other failure is the caller's to report, since a record written over a
 * document the read could not see would replace what that document holds.
 */
async function byName(
  client: RecordClient,
  project: string,
  name: string
): Promise<string | undefined> {
  try {
    return (await client.getProjectArtifact(project, name)).id;
  } catch (error) {
    if (error instanceof DispatchServiceError && error.fromDispatch && error.status === 404) {
      return undefined;
    }
    throw error;
  }
}

/**
 * The id of the session's document: the one the project holds under that name, else the one this
 * create makes. The create is sent under a key of the document's own, so the session's document is
 * created once however many attempts, records and processes reach for it. An answer that says the
 * create already landed — a replay Dispatch kept no answer for, `409 KEY_COMMITTED`, or
 * `422 KEY_REUSED` for a create another version of this plugin wrote under the same key — sends
 * this back to the read: the document is there to be appended to, and nothing uploads a second
 * time. A create Dispatch never answered is this record's failure, and the next record asks again
 * under the same key, which Dispatch replays, holds or refuses rather than running twice.
 */
async function document(
  client: RecordClient,
  project: string,
  name: string,
  sessionId: string,
  actor: { readonly kind: "session"; readonly id: string }
): Promise<string> {
  const held = await byName(client, project, name);
  if (held !== undefined) return held;
  // The create's body, fixed for the session: the header alone, with no record, no time and no
  // plugin version in it, so two attempts of it — two processes', or one process's own, a day
  // apart — are the same request under the same key, which Dispatch replays rather than refusing
  // as a key reused (`422 KEY_REUSED`).
  const content =
    `AskGate rule decisions of session ${sessionId}. One fenced block per decision follows, ` +
    "each holding one JSON record.\n";
  try {
    const created = await client.projectArtifact(
      project,
      { name, content, actor },
      intentKey(DOCUMENT_KEY_PREFIX, project, name)
    );
    return created.artifact.id;
  } catch (error) {
    if (!mayHaveCreated(error)) throw error;
  }
  const after = await byName(client, project, name);
  if (after !== undefined) return after;
  // Dispatch says this key's create landed, and the project holds no document of that name: it was
  // deleted, or renamed. Uploading again would need a key of its own, and a key of its own is what
  // replaces one document's records with another's, so this record is lost instead.
  throw new Error(
    `Dispatch holds the create of "${name}" under its key, and the project holds no document of ` +
      "that name"
  );
}

/**
 * Whether this failure says Dispatch may have made the write already, so the caller reads what it
 * wrote rather than sending it again: the write landed and its answer is gone
 * (`409 KEY_COMMITTED`, or a replay of an answer Dispatch kept none of), or the key was used for
 * a different request (`422 KEY_REUSED`), which only another writer under this same intent can be.
 */
function mayHaveCreated(error: unknown): boolean {
  if (error instanceof DispatchWriteLandedError) return true;
  return (
    error instanceof DispatchServiceError &&
    error.fromDispatch &&
    error.status === 422 &&
    error.code === IDEMPOTENCY_KEY_REUSED
  );
}

/**
 * One caller's `Idempotency-Key` for one intent: the prefix, and the sha256 of what the intent is
 * of, which is what keeps the key inside `IDEMPOTENCY_KEY_PATTERN` whatever a session id or a
 * document name holds.
 */
function intentKey(prefix: string, ...of: readonly string[]): string {
  return `${prefix}.${createHash("sha256").update(of.join("\n")).digest("hex")}`;
}
