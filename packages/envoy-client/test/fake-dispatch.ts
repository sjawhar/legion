// One stand-in Dispatch for every test that needs one, in place of the copy each package grew of
// its own (INFRA-1414). Two entry points share one route table and one request recorder:
//
//   * `fakeDispatchFetch` — a `fetch`-shaped handler for an injected `fetchImpl` or a
//     `globalThis.fetch` override, recording every call;
//   * `startFakeDispatch` — a real loopback server over that same handler, with the `/ws/live`
//     upgrade the live socket opens.
//
// Two stand-ins of their own sit beside them, each for one call a package makes of Dispatch and
// neither over the route table:
//
//   * `fakeRecordClient` — the in-memory document store AskGate's decision record is written to,
//     with Dispatch's idempotency-key rules;
//   * `fakeLatestHandoff` — the one call `findHandoffReport` makes of Dispatch.
//
// `RecordClient` and `LatestHandoff` live in packages that depend on this one, so the shapes they
// need are declared here structurally rather than imported back.
import {
  type CreateArtifactInput,
  type EditArtifactInput,
  IDEMPOTENCY_KEY_COMMITTED,
  IDEMPOTENCY_KEY_IN_FLIGHT,
  IDEMPOTENCY_KEY_REUSED,
} from "@legion/contracts";
import type { ServerWebSocket } from "bun";
import {
  DispatchServiceError,
  DispatchWriteLandedError,
  DispatchWriteUnsettledError,
} from "../src/dispatch-http";

/** The path the live socket is opened on, the one route `startFakeDispatch` upgrades. */
const LIVE_PATH = "/ws/live";

let bunResponse: Promise<typeof Response> | undefined;

/**
 * Bun's own `Response`, which is not `globalThis.Response` everywhere: packages/dispatch's test
 * preload (bunfig.toml:2) registers happy-dom's globals, and `Bun.serve` does not take happy-dom's
 * `Response` as an answer. Bun's own fetch still answers with Bun's, so one data URL names the
 * constructor. Read once, on the first answer this module builds.
 */
function nativeResponse(): Promise<typeof Response> {
  bunResponse ??= Bun.fetch("data:,").then((answer) => answer.constructor as typeof Response);
  return bunResponse;
}

/** One request a fake Dispatch was given. */
export interface FakeDispatchCall {
  /** The URL asked for. */
  readonly url: URL;
  /** The method, upper case; `GET` where the caller named none. */
  readonly method: string;
  /** The request's headers. */
  readonly headers: Headers;
  /**
   * The request's body as text, empty where it carried none. `fakeDispatchFetch` reads a string
   * and a `URLSearchParams`; it records any other body as empty too.
   */
  readonly body: string;
  /** The signal the caller passed, which a test reads to see whether its request was aborted. */
  readonly signal: AbortSignal | undefined;
}

/** Each route's body, by pathname, answered as JSON; a pathname no route names is a 404. */
export type FakeDispatchRoutes = Readonly<Record<string, unknown>>;

/** What a caller may change about the requests a fake Dispatch answers. */
export interface FakeDispatchOptions {
  /**
   * One catch-all for every request, consulted before `routes`. Its throw is the caller's: the
   * wrapper never catches, so a handler that throws `TypeError("Unable to connect")` reaches the
   * code under test as a dead connection does.
   */
  readonly handler?: (call: FakeDispatchCall) => Response | Promise<Response>;
  /**
   * Each route's body by pathname, answered as JSON with 200. A pathname no route names is
   * `404 {code:"NOT_FOUND", error:<pathname>}`, as Dispatch's own router answers one.
   */
  readonly routes?: FakeDispatchRoutes;
}

/** A `fetch` that answers as a fake Dispatch, and every call it was given. */
export interface FakeDispatchFetch {
  /** Every call, in order. */
  readonly calls: readonly FakeDispatchCall[];
  /** Each call's `pathname + search`, in order: the shape most assertions compare. */
  readonly paths: readonly string[];
  /** The handler itself, for a `fetchImpl` option or a `globalThis.fetch` override. */
  readonly fetchImpl: typeof fetch;
}

/** What `fetch` takes as its first argument, named without the DOM lib's `RequestInfo`: a
 *  package that compiles this file need not hold it. */
type FetchInput = string | URL | Request;

/** `body` as text: the string bodies these fakes are given, and "" for anything else. */
function bodyText(body: RequestInit["body"]): string {
  if (typeof body === "string") return body;
  if (body instanceof URLSearchParams) return body.toString();
  return "";
}

/** What `fetch(input, init)` asked for, as this module records it. */
function callOf(input: FetchInput, init?: RequestInit): FakeDispatchCall {
  const request = input instanceof Request ? input : undefined;
  return {
    body: bodyText(init?.body),
    headers: new Headers(init?.headers ?? request?.headers),
    method: (init?.method ?? request?.method ?? "GET").toUpperCase(),
    signal: init?.signal ?? request?.signal ?? undefined,
    url: new URL(request?.url ?? String(input)),
  };
}

/** `call`'s answer: the catch-all, else the route table, else Dispatch's own no-route 404. */
async function answerFor(options: FakeDispatchOptions, call: FakeDispatchCall): Promise<Response> {
  if (options.handler !== undefined) return await options.handler(call);
  const native = await nativeResponse();
  const body = options.routes?.[call.url.pathname];
  return body === undefined
    ? native.json({ code: "NOT_FOUND", error: call.url.pathname }, { status: 404 })
    : native.json(body);
}

/**
 * A fake Dispatch as a `fetch`: every call recorded, every answer the options' own. No request
 * leaves the process, so the URLs a test names need not resolve.
 */
export function fakeDispatchFetch(options: FakeDispatchOptions = {}): FakeDispatchFetch {
  const calls: FakeDispatchCall[] = [];
  const paths: string[] = [];
  const fetchImpl = (async (input: FetchInput, init?: RequestInit) => {
    const call = callOf(input, init);
    calls.push(call);
    paths.push(call.url.pathname + call.url.search);
    return await answerFor(options, call);
  }) as typeof fetch;
  return { calls, fetchImpl, paths };
}

/** What a caller may change about the fake Dispatch it serves. */
export interface FakeDispatchServerOptions<Frame> extends FakeDispatchOptions {
  /**
   * The bearer the live socket must carry. With it set, an upgrade whose `Authorization` is not
   * `Bearer <token>` is answered `401 {code:"UNAUTHORIZED"}` rather than upgraded; with it unset
   * every upgrade is taken.
   */
  readonly token?: string;
  /**
   * Answers one frame the client sent, on the socket it came in on. `nth` counts from 1 the
   * frames of that same `type`, so a script can answer the first subscribe and ignore the
   * reconnect's.
   */
  readonly live?: (socket: ServerWebSocket<undefined>, frame: Frame, nth: number) => void;
  /** Runs after every recorded frame, whatever `live` did: what wakes a test waiting on one. */
  readonly onFrame?: () => void;
}

/** A fake Dispatch listening on a loopback port, and what its client asked of it. */
export interface FakeDispatchServer<Frame> {
  /** Its base URL, `http://127.0.0.1:<port>`. */
  readonly url: string;
  /** Its live socket's URL, `ws://127.0.0.1:<port>/ws/live`. */
  readonly wsUrl: string;
  /** The loopback port it listens on. */
  readonly port: number;
  /** Every URL read over HTTP, in order; an upgrade is no read. */
  readonly reads: readonly URL[];
  /** The `Authorization` header of every upgrade asked for, null where it carried none. */
  readonly upgrades: readonly (string | null)[];
  /** The `Cookie` header of every upgrade asked for, null where it carried none. */
  readonly cookies: readonly (string | null)[];
  /** Every frame the client sent, in order, parsed. */
  readonly received: readonly Frame[];
  /** Stops the server, closing the connections still open. */
  stop(): void;
}

/**
 * A fake Dispatch on a loopback port of its own: the options' routes over HTTP, and the live
 * socket at `/ws/live`, which a `live` script answers. Every caller stops it when its test ends.
 */
export function startFakeDispatch<Frame = unknown>(
  options: FakeDispatchServerOptions<Frame> = {}
): FakeDispatchServer<Frame> {
  const reads: URL[] = [];
  const upgrades: (string | null)[] = [];
  const cookies: (string | null)[] = [];
  const received: Frame[] = [];
  /** How many frames of each `type` the client has sent, which is `live`'s `nth`. */
  const counts = new Map<string, number>();

  /** The answer to a request that is no upgrade: recorded, then answered from the options. */
  const read = async (request: Request, url: URL): Promise<Response> => {
    const call: FakeDispatchCall = {
      body: await request.text(),
      headers: request.headers,
      method: request.method,
      signal: request.signal,
      url,
    };
    reads.push(url);
    return await answerFor(options, call);
  };

  const server = Bun.serve({
    // Not async: `server.upgrade` must run before this handler awaits anything.
    fetch(request, srv) {
      const url = new URL(request.url);
      if (url.pathname !== LIVE_PATH) return read(request, url);
      const authorization = request.headers.get("authorization");
      upgrades.push(authorization);
      cookies.push(request.headers.get("cookie"));
      if (options.token !== undefined && authorization !== `Bearer ${options.token}`) {
        return nativeResponse().then((native) =>
          native.json({ code: "UNAUTHORIZED", error: "invalid bearer token" }, { status: 401 })
        );
      }
      if (srv.upgrade(request)) return undefined;
      return nativeResponse().then((native) => new native("upgrade failed", { status: 400 }));
    },
    hostname: "127.0.0.1",
    port: 0,
    websocket: {
      message(socket, raw) {
        // The client's own frame, which the assertions hold to the contract.
        const parsed: unknown = JSON.parse(String(raw));
        const frame = parsed as Frame;
        received.push(frame);
        const type =
          typeof parsed === "object" && parsed !== null && "type" in parsed
            ? String(parsed.type)
            : "";
        const nth = (counts.get(type) ?? 0) + 1;
        counts.set(type, nth);
        options.live?.(socket, frame, nth);
        options.onFrame?.();
      },
    },
  });
  // `Server.port` is undefined for a unix socket alone, which `port: 0` above never asks for.
  const { port } = server;
  if (port === undefined) throw new Error("the fake Dispatch bound no port");

  return {
    cookies,
    port,
    reads,
    received,
    stop() {
      server.stop(true);
    },
    upgrades,
    url: `http://127.0.0.1:${port}`,
    wsUrl: `ws://127.0.0.1:${port}${LIVE_PATH}`,
  };
}

/**
 * What a decision record needs of a Dispatch client, which is the three calls it makes. Declared
 * here structurally, over the real input types: the client's own interface is pi-envoy's
 * `RecordClient`, and pi-envoy depends on this package, so importing it back would invert that.
 */
export interface FakeRecordClient {
  /** The project's document of that name, 404 where the project holds none. */
  getProjectArtifact(project: string, name: string): Promise<{ readonly id: string }>;
  /** The create of the session's document, under the key given. */
  projectArtifact(
    project: string,
    input: CreateArtifactInput,
    key: string
  ): Promise<{ artifact: { id: string } }>;
  /** One record appended, under the key given. */
  docEdit(id: string, input: EditArtifactInput, key: string): Promise<unknown>;
}

/** Dispatch's answer that the project holds no document of that name, as the client throws it. */
export function recordNotFound(name: string): DispatchServiceError {
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
export interface FakeKeyedRun {
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

/** One `projectArtifact` call, as the create was given it. */
export interface FakeRecordUpload {
  project: string;
  name: string;
  content: string;
  actor?: unknown;
  key?: string;
}

/** One operation of one `docEdit` call, as the append was given it. */
export interface FakeRecordEdit {
  id: string;
  op: string;
  markdown: string;
  after?: string;
  actor?: unknown;
  key?: string;
}

/** What a caller may change about the Dispatch a record is written to. */
export interface FakeRecordClientOptions {
  /**
   * Answers of the caller's own, by call: a call this names runs it in place of the store below,
   * after the call has been recorded. A client that rejects, hangs or answers another document is
   * written this way.
   */
  readonly answers?: Partial<FakeRecordClient>;
  /** The id Dispatch gives the document the create makes; `doc-1` by default. */
  readonly id?: string;
  /** The document `markdown()` reads when the caller names none: the caller's own by name. */
  readonly document?: string;
}

/** The Dispatch a record is written to, and what each call was given. */
export interface FakeRecord {
  /** Every create, in order. */
  readonly uploads: readonly FakeRecordUpload[];
  /** Every appended operation, in order. */
  readonly edits: readonly FakeRecordEdit[];
  /** Every read of a document by name: the name it asked for. */
  readonly reads: readonly string[];
  /** Every key Dispatch has been sent, and the one run each stands for. */
  readonly keys: ReadonlyMap<string, FakeKeyedRun>;
  /** Each document by name: its id, and the text it holds now. */
  readonly stored: Map<string, { id: string; markdown: string }>;
  /** The client itself, which a recorder is given. */
  readonly client: FakeRecordClient;
  /** The text the document of that name holds now; the options' own document by default. */
  markdown(name?: string): string;
  /** Dispatch takes the next create and leaves it running: its caller is given no definitive
   *  answer, as the call's deadline or the client's keyed window gives it. */
  hold(): void;
  /** Finishes the create Dispatch took and left running, as Dispatch commits one long after its
   *  client gave it up; `answerLost` drops its answer, so a repeat under its key is
   *  `409 KEY_COMMITTED`. */
  commitHeld(answerLost?: boolean): void;
}

/**
 * The Dispatch a record is written to: what each call was given, what it answered, and the text
 * each document holds. It follows the two rules this recorder turns on.
 *
 * An upload of a name it already holds stores a new version of that same document, which the
 * reader reads in place of every version before it, so the upload's text **replaces** what the
 * document held (`findArtifactByNameOrSlug`, `packages/envoy/internal/dispatch/api/artifacts.go`).
 *
 * A write under an `Idempotency-Key` is one intent, run once (`keyedWrites`,
 * `packages/envoy/internal/dispatch/api/request_keys.go`): a repeat while the first run lives is
 * `409 KEY_IN_FLIGHT`, which the client asks again under the key until that run finishes — the
 * wait here stands for those attempts — a repeat after it finished is replayed and runs nothing,
 * one whose run committed and lost its answer is `409 KEY_COMMITTED`, and a repeat carrying a
 * different body is `422 KEY_REUSED`. A write with no key is run as Dispatch always ran it.
 */
export function fakeRecordClient(options: FakeRecordClientOptions = {}): FakeRecord {
  const { answers = {}, document, id = "doc-1" } = options;
  const uploads: FakeRecordUpload[] = [];
  const edits: FakeRecordEdit[] = [];
  const reads: string[] = [];
  const stored = new Map<string, { id: string; markdown: string }>();
  const keys = new Map<string, FakeKeyedRun>();
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
    const mine: FakeKeyedRun = { digest, finished };
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

  const client: FakeRecordClient = {
    getProjectArtifact: async (project, name) => {
      reads.push(name);
      if (answers.getProjectArtifact) return await answers.getProjectArtifact(project, name);
      const held = stored.get(name);
      if (held === undefined) throw recordNotFound(name);
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
    docEdit: async (doc, input, key) => {
      for (const op of input.ops ?? [])
        edits.push({
          id: doc,
          op: op.op,
          markdown: op.markdown ?? "",
          after: op.after,
          actor: input.actor,
          key,
        });
      if (answers.docEdit) return await answers.docEdit(doc, input, key);
      return await keyed(key, `POST ${doc}/edits ${JSON.stringify(input.ops)}`, () => {
        const held = [...stored.values()].find((made) => made.id === doc);
        if (held === undefined) {
          throw new DispatchServiceError("ARTIFACT_NOT_FOUND", 404, `no document ${doc}`);
        }
        for (const op of input.ops ?? []) held.markdown += `\n${op.markdown ?? ""}\n`;
        return { applied: (input.ops ?? []).length };
      });
    },
  };

  return {
    client,
    commitHeld: (answerLost = false) => {
      const run = heldRuns.shift();
      if (run === undefined) throw new Error("no held run to commit");
      run(answerLost);
    },
    edits,
    hold: () => {
      holdCreates += 1;
    },
    keys,
    markdown: (name = document) => (name === undefined ? "" : (stored.get(name)?.markdown ?? "")),
    reads,
    stored,
    uploads,
  };
}

/**
 * The newest handoff record of an issue's phase, as pi-shared's `LatestHandoff` reads it.
 * Declared here structurally for the reason `FakeRecordClient` is: pi-shared depends on this
 * package.
 */
export type FakeLatestHandoff = (
  issue: string,
  word: string
) => Promise<{ readonly id: string; readonly version: number }>;

/** A Dispatch answering one thing for every phase's newest record, and every read it was asked. */
export interface FakeHandoffDispatch {
  /** `"<issue> <word>"` for every read, in order. */
  readonly reads: readonly string[];
  /** The call itself, which `findHandoffReport` is given. */
  readonly latestHandoff: FakeLatestHandoff;
}

/** A Dispatch answering `answer` for every phase's newest record, recording each read. */
export function fakeLatestHandoff(
  answer: () => Promise<{ readonly id: string; readonly version: number }>
): FakeHandoffDispatch {
  const reads: string[] = [];
  return {
    latestHandoff: async (issue, word) => {
      reads.push(`${issue} ${word}`);
      return await answer();
    },
    reads,
  };
}
