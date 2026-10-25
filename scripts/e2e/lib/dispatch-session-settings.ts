// The driver of scripts/e2e/dispatch-session-settings.sh (INFRA-195): it owns every Oh My Pi
// process of the run, each in `--mode rpc` as a Legion pane runs it, and one held-open Chromium
// page signed in to Dispatch as alice, and runs the checks in order. Each check prints
// `== <name>`, what it observed, and `ok <name>`, and keeps a screenshot of the page in
// <evidence>/checks; the first failure prints `FAIL <name>: <why>` and exits 1.
//
//   bun scripts/e2e/lib/dispatch-session-settings.ts run <rig.json>
//     the checks; rig.json is the setup the shell script wrote (no secret is in it: the provider
//     keys reach each Oh My Pi from this process's environment, and nothing else)
//   bun scripts/e2e/lib/dispatch-session-settings.ts db-create <name>
//     creates database <name> in LEGION_TEST_PG_DSN's server and prints a DSN naming it
//   bun scripts/e2e/lib/dispatch-session-settings.ts db-drop <name>
//     drops database <name> there, with whatever is still connected to it
//   bun scripts/e2e/lib/dispatch-session-settings.ts free-port
//     prints a loopback TCP port nothing listens on
//
// What the page shows is never its own oracle. The session's model is RPC `get_state`'s; the
// models it lists are `get_available_models`'; its thinking level and the levels it offers are the
// `Thinking:` and `Available:` lines an `/effort` prompt prints (RPC `command_output`); turns and
// switches are its session file's entries; and who sent a switch is Dispatch's stored message.
//
// Playwright is resolved from packages/dispatch, the package that owns the browser suite, and
// launched on the Chromium the rig names: a pod has no Playwright browser of its own (INFRA-42).
import { appendFileSync, cpSync, existsSync, readdirSync, readFileSync } from "node:fs";
import { createRequire } from "node:module";
import type { FileSink, Subprocess } from "bun";
import { connect } from "nats";

const [command, ...args] = process.argv.slice(2);

function refuse(message: string): never {
  process.stderr.write(`dispatch-session-settings: ${message}\n`);
  process.exit(2);
}

// ---- the run's setup ------------------------------------------------------------------------------

interface Rig {
  readonly work: string;
  readonly evidence: string;
  readonly omp: string;
  readonly chromium: string;
  readonly dispatchUrl: string;
  readonly envoyUrl: string;
  readonly natsUrl: string;
  /** The two models the caller's keys serve: the profile's default and fallback target first. */
  readonly models: readonly [string, string];
  readonly plugin: string;
  readonly releasedPlugin: string;
  readonly home: string;
  readonly agentDir: string;
  readonly sessionCwd: string;
  readonly tmp: string;
  readonly envoyTokenFile: string;
  readonly dispatchTokenFile: string;
  readonly pidFile: string;
  /** The profile's default thinking level (config.yml `defaultThinkingLevel`). */
  readonly defaultLevel: string;
  /** A catalog model of a provider the run holds no key for. */
  readonly unkeyedModel: string;
}

const PERSON = "alice";
const OTHER_PERSON = "bob";
const BROKEN_MODEL = "rig-broken/broken-model";
const RESTART_NOTICE =
  "This session does not report its model and thinking level. Restart it on a newer pi-envoy to show and switch them here.";
const NO_ANSWER =
  "No answer came from the session within 30 seconds. The header shows the model and thinking level the session last reported.";
/** A1's bound: a change made anywhere but the header shows within this long. */
const SHOWN_WITHIN_MS = 20_000;

// ---- checks ---------------------------------------------------------------------------------------

class CheckFailure extends Error {}

let current = "setup";
let checkNumber = 0;
let checkStarted = Date.now();

function begin(name: string): void {
  checkNumber += 1;
  current = name;
  checkStarted = Date.now();
  process.stdout.write(`== ${name}\n`);
}
function stage(name: string): void {
  current = name;
  process.stdout.write(`-- ${name}\n`);
}
function note(text: string): void {
  process.stdout.write(`   ${text}\n`);
}
function pass(): void {
  process.stdout.write(`ok ${current} (${Math.round((Date.now() - checkStarted) / 1000)}s)\n`);
}
function fail(message: string): never {
  throw new CheckFailure(message);
}
function expect(condition: boolean, message: string): void {
  if (!condition) fail(message);
}

const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

/** Polls `probe` every quarter second until it answers something other than undefined, for at
 *  most `limitMs`; the failure names what it waited for and the last thing it saw. */
async function until<T>(
  what: string,
  limitMs: number,
  probe: () => Promise<T | undefined> | T | undefined,
  last?: () => string
): Promise<T> {
  const deadline = Date.now() + limitMs;
  for (;;) {
    const value = await probe();
    if (value !== undefined) return value;
    if (Date.now() > deadline) {
      fail(
        `timed out after ${limitMs / 1000}s waiting for ${what}${last ? `; last: ${last()}` : ""}`
      );
    }
    await sleep(250);
  }
}

// ---- Oh My Pi in --mode rpc -----------------------------------------------------------------------

interface RpcEvent {
  readonly type?: string;
  readonly id?: string;
  readonly [key: string]: unknown;
}

interface SessionState {
  readonly model: string | undefined;
  readonly isStreaming: boolean;
  readonly isSettled: boolean;
  readonly sessionFile: string;
  readonly sessionId: string;
}

/** What an `/effort` prompt printed: the level on its `Thinking:` line and the levels on its
 *  `Available:` line, or, for a model with no adjustable thinking, its one line. */
type Effort =
  | { readonly level: string; readonly available: readonly string[] }
  | { readonly none: string };

function effortText(effort: Effort): string {
  return "none" in effort ? effort.none : `${effort.level} of [${effort.available.join(", ")}]`;
}
function effortLevel(effort: Effort): string {
  return "none" in effort ? "no adjustable thinking" : effort.level;
}

const decoder = new TextDecoder();

/** One Oh My Pi process in `--mode rpc`: its requests by id, its `command_output` lines, and every
 *  event it printed, kept in the evidence directory. */
class Omp {
  readonly label: string;
  readonly pid: number;
  readonly #proc: Subprocess;
  readonly #pending = new Map<string, (event: RpcEvent) => void>();
  readonly #outputs: string[] = [];
  readonly #results = new Map<string, RpcEvent>();
  readonly #log: string;
  #ready = false;
  #exited = false;
  #next = 0;
  #lock: Promise<unknown> = Promise.resolve();
  /** The session file `get_state` last named, kept as evidence when the run ends. */
  sessionFile: string | undefined;

  private constructor(rig: Rig, label: string, argv: readonly string[]) {
    this.label = label;
    this.#log = `${rig.evidence}/logs/omp-${label}.rpc.jsonl`;
    const env: Record<string, string> = {
      DISPATCH_TOKEN_FILE: rig.dispatchTokenFile,
      DISPATCH_URL: rig.dispatchUrl,
      ENVOY_MACHINE_ID: "legion-e2e-session-settings",
      ENVOY_NATS_URL: rig.natsUrl,
      ENVOY_TOKEN_FILE: rig.envoyTokenFile,
      ENVOY_URL: rig.envoyUrl,
      HOME: rig.home,
      PATH: process.env.PATH ?? "/usr/bin:/bin",
      PI_CODING_AGENT_DIR: rig.agentDir,
      TMPDIR: rig.tmp,
    };
    for (const key of ["ANTHROPIC_API_KEY", "OPENAI_API_KEY"]) {
      const value = process.env[key];
      if (value !== undefined && value !== "") env[key] = value;
    }
    this.#proc = Bun.spawn([rig.omp, ...argv], {
      cwd: rig.sessionCwd,
      env,
      stderr: Bun.file(`${rig.evidence}/logs/omp-${label}.stderr.log`),
      stdin: "pipe",
      stdout: "pipe",
    });
    this.pid = this.#proc.pid;
    appendFileSync(rig.pidFile, `${this.pid}\n`);
    void this.#proc.exited.then(() => {
      this.#exited = true;
    });
    void this.#read();
  }

  /** Starts Oh My Pi with `extension` loaded (none for a bare session) and `extra` arguments, and
   *  waits for its `ready` line. */
  static async start(
    rig: Rig,
    label: string,
    extension: string | null,
    extra: readonly string[] = []
  ): Promise<Omp> {
    const argv = [...(extension === null ? [] : ["--extension", extension]), "--mode", "rpc"];
    const omp = new Omp(rig, label, [...argv, ...extra]);
    await until(`Oh My Pi ${label} (pid ${omp.pid}) to print ready`, 120_000, () =>
      omp.#ready ? true : omp.#exited ? fail(`Oh My Pi ${label} exited at startup`) : undefined
    );
    return omp;
  }

  async #read(): Promise<void> {
    let buffer = "";
    const stdout = this.#proc.stdout as ReadableStream<Uint8Array>;
    for await (const chunk of stdout) {
      buffer += decoder.decode(chunk, { stream: true });
      for (let end = buffer.indexOf("\n"); end >= 0; end = buffer.indexOf("\n")) {
        const line = buffer.slice(0, end);
        buffer = buffer.slice(end + 1);
        this.#take(line);
      }
    }
  }

  #take(line: string): void {
    let event: RpcEvent;
    try {
      event = JSON.parse(line) as RpcEvent;
    } catch {
      return;
    }
    if (event.type !== "message_update" && event.type !== "tool_execution_update") {
      appendFileSync(this.#log, `${line.length > 4000 ? `${line.slice(0, 4000)}…` : line}\n`);
    }
    if (event.type === "ready") this.#ready = true;
    if (event.type === "command_output" && typeof event.text === "string") {
      this.#outputs.push(event.text);
    }
    if (event.type === "prompt_result" && typeof event.id === "string") {
      this.#results.set(event.id, event);
    }
    if (event.type === "extension_ui_request" && typeof event.id === "string") {
      // A dialog an extension opens would hold its caller until answered; nobody sits at this
      // session, so it is answered as a person who closed it.
      if (["select", "confirm", "input", "editor"].includes(String(event.method))) {
        this.#write({ cancelled: true, id: event.id, type: "extension_ui_response" });
      }
    }
    if (event.type === "response" && typeof event.id === "string") {
      this.#pending.get(event.id)?.(event);
      this.#pending.delete(event.id);
    }
  }

  #write(value: object): void {
    const stdin = this.#proc.stdin as FileSink;
    stdin.write(`${JSON.stringify(value)}\n`);
    stdin.flush();
  }

  /** Sends one command and answers its response's `data`; a refused command fails the check. */
  async request(commandBody: Record<string, unknown>, limitMs = 120_000): Promise<unknown> {
    this.#next += 1;
    const id = `rig-${this.#next}`;
    const answer = new Promise<RpcEvent>((resolve) => this.#pending.set(id, resolve));
    this.#write({ ...commandBody, id });
    const response = await Promise.race([answer, sleep(limitMs).then(() => undefined)]);
    if (response === undefined) {
      fail(`Oh My Pi ${this.label} did not answer ${commandBody.type} within ${limitMs / 1000}s`);
    }
    if (response.success !== true) {
      fail(`Oh My Pi ${this.label} refused ${commandBody.type}: ${String(response.error)}`);
    }
    return response.data;
  }

  async state(): Promise<SessionState> {
    const data = (await this.request({ type: "get_state" })) as {
      model?: { provider?: string; id?: string } | null;
      isStreaming: boolean;
      isSettled: boolean;
      sessionFile: string;
      sessionId: string;
    };
    const model =
      data.model?.provider && data.model.id ? `${data.model.provider}/${data.model.id}` : undefined;
    this.sessionFile = data.sessionFile;
    return {
      isSettled: data.isSettled,
      isStreaming: data.isStreaming,
      model,
      sessionFile: data.sessionFile,
      sessionId: data.sessionId,
    };
  }

  async models(): Promise<string[]> {
    const data = (await this.request({ type: "get_available_models" })) as {
      models: { provider: string; id: string }[];
    };
    return data.models.map((model) => `${model.provider}/${model.id}`);
  }

  /** Runs `text` as typed at the session's terminal and answers what the built-in printed. One at
   *  a time, so each answer's lines are its own. */
  async typed(text: string): Promise<string[]> {
    const run = this.#lock.then(async () => {
      const from = this.#outputs.length;
      await this.request({ message: text, type: "prompt" });
      return this.#outputs.slice(from);
    });
    this.#lock = run.catch(() => undefined);
    return run;
  }

  async effort(): Promise<Effort> {
    const printed = (await this.typed("/effort")).join("\n");
    const thinking = /^Thinking: (.+)$/m.exec(printed);
    const available = /^Available: (.+)$/m.exec(printed);
    if (thinking?.[1] !== undefined && available?.[1] !== undefined) {
      return { available: available[1].split(", "), level: thinking[1] };
    }
    if (/has no adjustable thinking level/.test(printed)) return { none: printed.trim() };
    return fail(`/effort printed "${printed}" in ${this.label}`);
  }

  /** Starts a prompt the model answers and answers its id; `settled` waits for its result. */
  async prompt(text: string): Promise<string> {
    this.#next += 1;
    const id = `rig-prompt-${this.#next}`;
    const answer = new Promise<RpcEvent>((resolve) => this.#pending.set(id, resolve));
    this.#write({ id, message: text, type: "prompt" });
    const response = await answer;
    if (response.success !== true)
      fail(`Oh My Pi ${this.label} refused the prompt: ${String(response.error)}`);
    return id;
  }

  async settled(id: string, limitMs: number): Promise<RpcEvent> {
    return until(`the prompt ${id} of ${this.label} to finish`, limitMs, () =>
      this.#results.get(id)
    );
  }

  signal(name: "SIGSTOP" | "SIGCONT"): void {
    process.kill(this.pid, name);
  }

  get exited(): boolean {
    return this.#exited;
  }

  /** Ends the process as a closed terminal ends it, and waits for it to go. */
  async stop(): Promise<void> {
    if (this.#exited) return;
    process.kill(this.pid, "SIGCONT");
    process.kill(this.pid, "SIGTERM");
    const gone = await Promise.race([
      this.#proc.exited.then(() => true),
      sleep(20_000).then(() => false),
    ]);
    if (!gone) {
      process.kill(this.pid, "SIGKILL");
      await this.#proc.exited;
    }
  }
}

// ---- the session file -----------------------------------------------------------------------------

interface Entry {
  readonly type?: string;
  readonly timestamp?: string;
  readonly customType?: string;
  readonly model?: string;
  readonly role?: string;
  readonly data?: { readonly message_id?: string };
  readonly content?: unknown;
  readonly message?: {
    readonly role?: string;
    readonly provider?: string;
    readonly model?: string;
    readonly stopReason?: string;
    readonly errorMessage?: string;
    readonly tag?: string;
    /** When the host started the message, in milliseconds. */
    readonly timestamp?: number;
    readonly content?: unknown;
  };
}

function entries(file: string): Entry[] {
  if (!existsSync(file)) return [];
  return readFileSync(file, "utf8")
    .split("\n")
    .flatMap((line) => {
      if (line.trim() === "") return [];
      try {
        return [JSON.parse(line) as Entry];
      } catch {
        return [];
      }
    });
}

function text(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return JSON.stringify(content ?? "");
  return content
    .map((part: { type?: string; text?: string }) =>
      part.type === "text" ? (part.text ?? "") : ""
    )
    .join("\n");
}

const userMessages = (file: string) =>
  entries(file).filter((entry) => entry.type === "message" && entry.message?.role === "user");
const assistantMessages = (file: string) =>
  entries(file).filter((entry) => entry.type === "message" && entry.message?.role === "assistant");
const turnModel = (entry: Entry) => `${entry.message?.provider}/${entry.message?.model}`;
/** Envoy cards holding `needle`: what pi-envoy delivers for every frame that is not a turn. */
const cards = (file: string, needle: string) =>
  entries(file).filter(
    (entry) =>
      entry.type === "custom_message" &&
      entry.customType === "envoy-message" &&
      text(entry.content).includes(needle)
  );
/** The attempts pi-envoy recorded as handled, by Dispatch message id. */
const handledAttempts = (file: string) =>
  entries(file).flatMap((entry) =>
    entry.type === "custom" && entry.customType === "envoy-dispatch-handled-attempt"
      ? [entry.data?.message_id ?? ""]
      : []
  );

// ---- Dispatch, the listener and the bus -----------------------------------------------------------

interface StoredMessage {
  readonly id: string;
  readonly body: string;
  readonly author: { readonly kind: string; readonly id: string };
  readonly created_at: string;
  readonly in_reply_to: string | null;
  readonly deliveries: readonly {
    readonly attempt: number;
    readonly state: string;
    readonly error: string | null;
    readonly accepted_as: string | null;
  }[];
}
interface StoredRead {
  readonly message: StoredMessage;
  readonly replies: readonly StoredMessage[];
}

class Services {
  readonly #rig: Rig;
  constructor(rig: Rig) {
    this.#rig = rig;
  }

  async #json(url: string, init: RequestInit): Promise<{ status: number; body: unknown }> {
    const response = await fetch(url, { ...init, signal: AbortSignal.timeout(30_000) });
    const raw = await response.text();
    let body: unknown = raw;
    try {
      body = JSON.parse(raw);
    } catch {
      // A body that is not JSON is kept as its text.
    }
    return { body, status: response.status };
  }

  /** What `login` reads of the session's stored thread, as the page reads it. */
  async thread(session: string, login = PERSON): Promise<StoredRead[]> {
    const { body, status } = await this.#json(
      `${this.#rig.dispatchUrl}/api/v1/agents/${encodeURIComponent(session)}/messages`,
      { headers: { "X-Dispatch-User": login } }
    );
    expect(
      status === 200,
      `GET the stored thread of ${session} answered ${status}: ${JSON.stringify(body)}`
    );
    return body as StoredRead[];
  }

  /** A person's Send to the session, as the composer and the header send it. */
  async send(session: string, body: string, login: string): Promise<StoredMessage> {
    const answer = await this.#json(
      `${this.#rig.dispatchUrl}/api/v1/agents/${encodeURIComponent(session)}/messages`,
      {
        body: JSON.stringify({ body, delivery: "steer" }),
        headers: { "Content-Type": "application/json", "X-Dispatch-User": login },
        method: "POST",
      }
    );
    expect(
      answer.status === 201,
      `${login}'s Send of "${body}" answered ${answer.status}: ${JSON.stringify(answer.body)}`
    );
    return answer.body as StoredMessage;
  }

  /** `path` posted with the session's own Dispatch bearer, as an agent would post it. */
  async #asAgent(path: string, body: object): Promise<{ status: number; body: unknown }> {
    const token = readFileSync(this.#rig.dispatchTokenFile, "utf8").trim();
    return this.#json(`${this.#rig.dispatchUrl}${path}`, {
      body: JSON.stringify(body),
      headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
      method: "POST",
    });
  }

  /** The same Send with the session's own Dispatch bearer, as an agent would make it. */
  sendAsAgent(session: string, body: string): Promise<{ status: number; body: unknown }> {
    return this.#asAgent(`/api/v1/agents/${encodeURIComponent(session)}/messages`, {
      body,
      delivery: "steer",
    });
  }

  /** The session's accept of an attempt as its user's turn, asked as the session asks it. */
  accept(id: string, attempt: number, session: string): Promise<{ status: number; body: unknown }> {
    return this.#asAgent(`/api/v1/messages/${id}/deliveries/${attempt}/accept`, {
      actor: { id: session, kind: "session" },
    });
  }

  async #listener(path: string, body?: object): Promise<unknown> {
    const token = readFileSync(this.#rig.envoyTokenFile, "utf8").trim();
    const answer = await this.#json(`${this.#rig.envoyUrl}${path}`, {
      headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
      ...(body === undefined ? {} : { body: JSON.stringify(body), method: "POST" }),
    });
    expect(
      answer.status === 200,
      `the listener answered ${path} with ${answer.status}: ${JSON.stringify(answer.body)}`
    );
    return answer.body;
  }

  async registered(session: string, since = 0): Promise<boolean> {
    const sessions = (await this.#listener("/v1/sessions")) as {
      session_id: string;
      last_seen: number;
    }[];
    return sessions.some((row) => row.session_id === session && row.last_seen >= since);
  }

  /** Registers an Envoy client of the run's own, as any session on the bus registers. */
  async registerClient(session: string): Promise<void> {
    await this.#listener("/v1/interests/subscribe", {
      dir: this.#rig.work,
      self_subscribed: true,
      session_id: session,
      title: "INFRA-195 rig sender",
      topics: [`notifications.agent.${session}`],
    });
  }

  /** An Envoy message from `from` to the session, as one session sends another. */
  async envoyMessage(from: string, to: string, message: string, key: string): Promise<void> {
    await this.#listener("/v1/messages/send", {
      idempotency_key: key,
      message,
      source: "agent",
      source_session: from,
      target_session: to,
    });
  }

  /** A Dispatch-shaped frame naming `messageId` at attempt 1 as alice's steer on no issue,
   *  carrying `body`: what any holder of the listener token can write
   *  (dispatch-user-turns.sh's forged_frame), sent to the session the listener lists. */
  async forgeDispatchFrame(
    session: string,
    messageId: string,
    body: string,
    key: string
  ): Promise<void> {
    const payload = JSON.stringify({
      delivery: { attempt: 1, mode: "steer" },
      event: {
        actor: { id: PERSON, kind: "user" },
        issue_key: null,
        payload: {
          author: { id: PERSON, kind: "user" },
          body,
          created_at: new Date().toISOString(),
          deliveries: [],
          id: messageId,
          in_reply_to: null,
          issue_key: null,
          target: `session:${session}`,
        },
        type: "message.created",
      },
    });
    await this.#listener("/v1/messages/send", {
      idempotency_key: key,
      message: body,
      payload,
      source: "dispatch",
      target_session: session,
    });
  }

  /** Publishes `frame` on the session's frames subject as a bare bus client. */
  async publishFrame(session: string, frame: object): Promise<void> {
    const nc = await connect({
      name: "legion-e2e-session-settings",
      servers: this.#rig.natsUrl,
      timeout: 10_000,
    });
    try {
      nc.publish(`agentstream.${session}.frames`, new TextEncoder().encode(JSON.stringify(frame)));
      await nc.flush();
    } finally {
      await nc.close();
    }
  }
}

/** The message `body` that `login` sent the session after the run had seen `before`. */
async function newMessage(
  services: Services,
  session: string,
  body: string,
  before: ReadonlySet<string>
): Promise<StoredRead> {
  return until(`Dispatch to store "${body}" to ${session}`, 30_000, async () =>
    (await services.thread(session)).find(
      (read) => read.message.body === body && !before.has(read.message.id)
    )
  );
}
async function storedIds(services: Services, session: string): Promise<Set<string>> {
  return new Set((await services.thread(session)).map((read) => read.message.id));
}
/** The session's reply to stored message `id`, once there is one. */
async function sessionReply(
  services: Services,
  session: string,
  id: string,
  limitMs: number
): Promise<string> {
  return until(`the session's reply to message ${id}`, limitMs, async () => {
    const read = (await services.thread(session)).find((each) => each.message.id === id);
    return read?.replies.find((reply) => reply.author.kind === "session")?.body;
  });
}

// ---- the page -------------------------------------------------------------------------------------

interface Locator {
  selectOption(value: string): Promise<unknown>;
  fill(value: string): Promise<void>;
  click(options?: { timeout?: number }): Promise<void>;
  waitFor(options?: { state?: string; timeout?: number }): Promise<void>;
  getByRole(role: string, options: { name: string }): Locator;
  locator(selector: string): Locator;
}
interface Page {
  goto(url: string, options?: { waitUntil?: string }): Promise<unknown>;
  reload(options?: { waitUntil?: string }): Promise<unknown>;
  screenshot(options: { path: string; fullPage?: boolean }): Promise<unknown>;
  getByRole(role: string, options: { name: string }): Locator;
  getByTestId(id: string): Locator;
  evaluate<T, A>(fn: (arg: A) => T | Promise<T>, arg: A): Promise<T>;
  url(): string;
}

/** What the header shows: the model and thinking level, as the picker's value or the pill's
 *  text, whether each is a picker and what it offers, the switch line and the notice. */
interface Header {
  readonly model: string | null;
  readonly modelPicker: boolean;
  readonly modelOptions: readonly string[];
  readonly thinking: string | null;
  readonly thinkingPicker: boolean;
  readonly thinkingOptions: readonly string[];
  readonly pickersDisabled: boolean;
  readonly status: string | null;
  readonly notice: string | null;
}

function headerText(header: Header): string {
  return JSON.stringify({
    model: header.model,
    modelPicker: header.modelPicker,
    notice: header.notice,
    status: header.status,
    thinking: header.thinking,
    thinkingPicker: header.thinkingPicker,
  });
}

class Browser {
  readonly #rig: Rig;
  readonly page: Page;
  readonly #close: () => Promise<void>;
  #session = "";

  private constructor(rig: Rig, page: Page, close: () => Promise<void>) {
    this.#rig = rig;
    this.page = page;
    this.#close = close;
  }

  static async open(rig: Rig): Promise<Browser> {
    const require = createRequire(
      new URL("../../../packages/dispatch/package.json", import.meta.url)
    );
    const { chromium } = require("@playwright/test");
    const browser = await chromium.launch({ executablePath: rig.chromium });
    const context = await browser.newContext({
      extraHTTPHeaders: { "X-Dispatch-User": PERSON },
      viewport: { height: 1000, width: 1280 },
    });
    const page: Page = await context.newPage();
    return new Browser(rig, page, () => browser.close());
  }

  close(): Promise<void> {
    return this.#close();
  }

  get session(): string {
    return this.#session;
  }

  /** Opens the session's live view on the one page. */
  async live(session: string): Promise<void> {
    this.#session = session;
    await this.page.goto(`${this.#rig.dispatchUrl}/agents/${encodeURIComponent(session)}/live`, {
      waitUntil: "domcontentloaded",
    });
    await this.page.getByTestId("agent-thread").waitFor({ timeout: 60_000 });
  }

  async reload(): Promise<void> {
    await this.page.reload({ waitUntil: "domcontentloaded" });
    await this.page.getByTestId("agent-thread").waitFor({ timeout: 60_000 });
  }

  async header(): Promise<Header> {
    return this.page.evaluate(() => {
      const find = (id: string) => document.querySelector(`[data-testid="${id}"]`);
      const model = find("agent-model-picker") as HTMLSelectElement | null;
      const thinking = find("agent-thinking-picker") as HTMLSelectElement | null;
      const values = (select: HTMLSelectElement | null) =>
        select === null
          ? []
          : Array.from(select.options)
              .filter((option) => !option.disabled)
              .map((option) => option.value);
      const shownThinking =
        thinking === null
          ? (find("agent-session-thinking")?.textContent?.trim() ?? null)
          : thinking.value === ""
            ? "model default"
            : thinking.value;
      return {
        model:
          model === null ? (find("agent-session-model")?.textContent?.trim() ?? null) : model.value,
        modelOptions: values(model),
        modelPicker: model !== null,
        notice: find("agent-settings-notice")?.textContent?.trim() ?? null,
        pickersDisabled: model?.disabled === true || thinking?.disabled === true,
        status: find("agent-switch-status")?.textContent?.trim() ?? null,
        thinking: shownThinking,
        thinkingOptions: values(thinking),
        thinkingPicker: thinking !== null,
      };
    }, undefined);
  }

  /** Waits until the header satisfies `wanted`; answers it and how long that took. */
  async headerUntil(
    what: string,
    limitMs: number,
    wanted: (header: Header) => boolean
  ): Promise<{ header: Header; tookMs: number }> {
    const from = Date.now();
    let last: Header | undefined;
    const header = await until(
      `the header to show ${what}`,
      limitMs,
      async () => {
        last = await this.header();
        return wanted(last) ? last : undefined;
      },
      () => (last === undefined ? "nothing" : headerText(last))
    );
    return { header, tookMs: Date.now() - from };
  }

  async pickModel(model: string): Promise<void> {
    await this.page.getByRole("combobox", { name: "Model" }).selectOption(model);
  }

  async pickThinking(level: string): Promise<void> {
    await this.page.getByRole("combobox", { name: "Thinking level" }).selectOption(level);
  }

  /** Types `body` in the composer and presses Send, as the person sends a message. */
  async send(body: string): Promise<void> {
    await this.page.getByRole("combobox", { name: "Delivery mode" }).selectOption("steer");
    const composer = this.page.getByTestId("agent-composer");
    await composer.locator("textarea").fill(body);
    await composer.getByRole("button", { name: "Send" }).click({ timeout: 60_000 });
  }

  /** The thread as the page shows it: the viewer's messages, everyone else's with the author
   *  the page names, the session's Dispatch replies, and each streamed turn's model label. */
  async thread(): Promise<{
    mine: string[];
    others: { author: string; text: string }[];
    replies: string[];
    turnModels: string[];
  }> {
    return this.page.evaluate(() => {
      const all = (id: string) => Array.from(document.querySelectorAll(`[data-testid="${id}"]`));
      return {
        mine: all("agent-message-user").map((node) => node.textContent?.trim() ?? ""),
        others: all("agent-message-other").map((node) => ({
          author: node.querySelector("p")?.textContent?.trim() ?? "",
          text: node.textContent?.trim() ?? "",
        })),
        replies: all("agent-dispatch-reply").map((node) => node.textContent?.trim() ?? ""),
        turnModels: all("agent-message-model").map((node) => node.textContent?.trim() ?? ""),
      };
    }, undefined);
  }

  /** The first replay the session's stream serves a page that opens, read over the page's own
   *  connection: its frames. */
  async replay(session: string): Promise<{ kind: string; seq: number; [key: string]: unknown }[]> {
    return this.page.evaluate(async (sessionID: string) => {
      const response = await fetch(`/api/v1/agents/${encodeURIComponent(sessionID)}/stream`);
      if (!response.ok || response.body === null) throw new Error(`stream: ${response.status}`);
      const reader = response.body.getReader();
      const decode = new TextDecoder();
      let buffer = "";
      try {
        for (;;) {
          const { done, value } = await reader.read();
          if (done) throw new Error("the stream ended before its replay");
          buffer += decode.decode(value, { stream: true });
          for (let end = buffer.indexOf("\n\n"); end >= 0; end = buffer.indexOf("\n\n")) {
            const lines = buffer.slice(0, end).split("\n");
            buffer = buffer.slice(end + 2);
            if (!lines.includes("event: replay")) continue;
            const data = lines
              .filter((line) => line.startsWith("data: "))
              .map((line) => line.slice("data: ".length))
              .join("\n");
            return JSON.parse(data).frames;
          }
        }
      } finally {
        await reader.cancel();
      }
    }, session);
  }

  async shot(name: string): Promise<void> {
    const file = `${this.#rig.evidence}/checks/${String(checkNumber).padStart(2, "0")}-${name}.png`;
    await this.page.screenshot({ fullPage: true, path: file }).catch(() => undefined);
  }
}

// ---- the run --------------------------------------------------------------------------------------

/** The concrete levels of an `/effort` answer: what it offers less `off` and `auto`. */
function concrete(effort: Effort): string[] {
  return "none" in effort
    ? []
    : effort.available.filter((level) => level !== "off" && level !== "auto");
}

async function run(rigFile: string): Promise<void> {
  const rig = JSON.parse(readFileSync(rigFile, "utf8")) as Rig;
  const [A, B] = rig.models;
  const services = new Services(rig);
  const started: Omp[] = [];
  const startOmp = async (
    label: string,
    extension: string | null,
    extra: readonly string[] = []
  ) => {
    const omp = await Omp.start(rig, label, extension, extra);
    started.push(omp);
    return omp;
  };
  let browser: Browser | undefined;
  const runStarted = Date.now();

  /** Waits until the session `omp` runs is on the listener, seen at or after `since`. */
  const registers = async (omp: Omp, since = 0): Promise<string> => {
    const { sessionId } = await omp.state();
    await until(
      `session ${sessionId} (${omp.label}) to register with the listener`,
      120_000,
      async () => ((await services.registered(sessionId, since)) ? true : undefined)
    );
    return sessionId;
  };
  /** The model and level `omp` reports through RPC alone. */
  const oracle = async (omp: Omp) => {
    const state = await omp.state();
    const effort = await omp.effort();
    return { effort, model: state.model, state };
  };
  /** Waits until the session runs no turn and none is owed. */
  const idle = async (omp: Omp, limitMs = 240_000) =>
    until(`${omp.label} to settle`, limitMs, async () => {
      const state = await omp.state();
      return !state.isStreaming && state.isSettled ? state : undefined;
    });
  /** A pick from the header and the session's answer to it, shown on the page's switch line: the
   *  stored message it is, and the answer quoted. */
  const pickAndAnswer = async (
    session: string,
    kind: "model" | "thinking",
    value: string,
    wantAnswer: string
  ): Promise<{ stored: StoredRead; status: string; tookMs: number }> => {
    if (browser === undefined) fail("the page is not open");
    const before = await storedIds(services, session);
    const pickedAt = Date.now();
    if (kind === "model") await browser.pickModel(value);
    else await browser.pickThinking(value);
    // The switch is a stored message whose reply is the session's answer; only then is the
    // switch line read, so a line an earlier switch left cannot stand in for this one's.
    const body = kind === "model" ? `/model ${value}` : `/effort ${value}`;
    const stored = await newMessage(services, session, body, before);
    const reply = await sessionReply(services, session, stored.message.id, 60_000);
    expect(
      reply === wantAnswer,
      `the session answered "${body}" with "${reply}", want "${wantAnswer}"`
    );
    const want = `The session answered: ${wantAnswer}`;
    const { header } = await browser.headerUntil(
      `the switch line "${want}"`,
      SHOWN_WITHIN_MS,
      (h) => h.status === want
    );
    return { status: header.status ?? "", stored, tookMs: Date.now() - pickedAt };
  };

  try {
    // ---- the sessions --------------------------------------------------------------------------
    stage("preflight");
    const preflight = await startOmp("preflight", null);
    const available = await preflight.models();
    for (const model of [A, B, BROKEN_MODEL]) {
      expect(
        available.includes(model),
        `${model} is not among the ${available.length} models the session lists as available under the caller's keys`
      );
    }
    expect(
      !available.includes(rig.unkeyedModel),
      `${rig.unkeyedModel} is listed as available, so it cannot stand for a model whose key the session cannot read`
    );
    const lists: Record<string, Effort> = {};
    for (const model of [A, B]) {
      const [provider, ...id] = model.split("/");
      await preflight.request({ modelId: id.join("/"), provider, type: "set_model" });
      lists[model] = await preflight.effort();
      const prompt = await preflight.prompt("Reply with the one word: ready");
      await preflight.settled(prompt, 180_000);
      const answer = assistantMessages((await preflight.state()).sessionFile).at(-1);
      expect(
        answer !== undefined &&
          turnModel(answer) === model &&
          answer.message?.stopReason === "stop",
        `the one-word preflight prompt on ${model} did not answer: ${JSON.stringify(answer?.message ?? null).slice(0, 400)}`
      );
      note(
        `${model}: answered "${text(answer?.message?.content).trim()}"; /effort offers ${effortText(lists[model] as Effort)}`
      );
    }
    expect(
      JSON.stringify(concrete(lists[A] as Effort)) !== JSON.stringify(concrete(lists[B] as Effort)),
      `${A} and ${B} offer the same /effort levels, so a model pick cannot show the level changing`
    );
    await preflight.stop();
    stage("sessions");
    const main = await startOmp("main", rig.plugin);
    const other = await startOmp("other", rig.plugin);
    const S = await registers(main);
    const O = await registers(other);
    const otherStart = await oracle(other);
    note(
      `main session ${S} (pid ${main.pid}); other session ${O} (pid ${other.pid}) on ${otherStart.model}, ${effortText(otherStart.effort)}`
    );
    browser = await Browser.open(rig);
    await browser.live(S);
    const firstReplay = await browser.replay(S);
    expect(
      firstReplay.some((frame) => frame.kind === "commands"),
      `the first replay of ${S} has no commands frame, so this Oh My Pi (LEGION_TEST_OMP=${rig.omp}) cannot run typed input from Dispatch: ${JSON.stringify(firstReplay.map((frame) => frame.kind))}`
    );
    note(`first replay: ${firstReplay.map((frame) => frame.kind).join(", ")}`);
    const page = browser;
    /** The person's prompt from the composer to the open session `omp` runs, and the assistant
     *  turns the session ran for it, read from its session file once the run has settled: the
     *  turns after the user message pi-envoy tagged with the prompt's Dispatch id. */
    const promptFromPage = async (omp: Omp, body: string) => {
      const before = await storedIds(services, page.session);
      await page.send(body);
      const sent = await newMessage(services, page.session, body, before);
      const file = (await omp.state()).sessionFile;
      const turns = () => {
        const all = entries(file);
        const at = all.findIndex(
          (entry) =>
            entry.type === "message" &&
            entry.message?.role === "user" &&
            entry.message.tag === sent.message.id
        );
        return at < 0
          ? []
          : all
              .slice(at)
              .filter((entry) => entry.type === "message" && entry.message?.role === "assistant");
      };
      await until(`the turn on "${body}"`, 240_000, () =>
        turns().some((turn) => turn.message?.stopReason !== "toolUse") ? true : undefined
      );
      await idle(omp);
      return { id: sent.message.id, turns: turns() };
    };

    // ---- 1 --------------------------------------------------------------------------------------
    begin("header-before-any-turn");
    {
      const { effort, model } = await oracle(main);
      const listed = await main.models();
      expect(
        !("none" in effort),
        `/effort in ${S} reports no adjustable thinking: ${effortText(effort)}`
      );
      const level = effortLevel(effort);
      const { header } = await page.headerUntil(
        `${model} and ${level}`,
        60_000,
        (h) => h.model === model && h.thinking === level
      );
      await page.shot("header-before-any-turn");
      expect(
        header.modelPicker && header.thinkingPicker,
        `the header offers no picker: ${headerText(header)}`
      );
      expect(
        JSON.stringify([...header.modelOptions].sort()) === JSON.stringify([...listed].sort()),
        `the model picker offers ${header.modelOptions.length} models, get_available_models lists ${listed.length}; only in the picker: ${header.modelOptions.filter((m) => !listed.includes(m)).join(", ")}; only listed: ${listed.filter((m) => !header.modelOptions.includes(m)).join(", ")}`
      );
      const offered = "none" in effort ? [] : effort.available;
      expect(
        JSON.stringify(header.thinkingOptions) === JSON.stringify(offered),
        `the thinking picker offers [${header.thinkingOptions.join(", ")}], /effort offers [${offered.join(", ")}]`
      );
      const file = (await main.state()).sessionFile;
      expect(assistantMessages(file).length === 0, `${S} has run a turn already`);
      expect((await page.thread()).turnModels.length === 0, "the page shows a turn");
      note(`get_state ${model}; /effort ${effortText(effort)}; header ${headerText(header)}`);
      note(
        `the model picker offers the ${listed.length} models get_available_models lists; the thinking picker offers [${header.thinkingOptions.join(", ")}]; no turn has run`
      );
      pass();
    }

    // ---- 2 --------------------------------------------------------------------------------------
    const userCount = async () => userMessages((await main.state()).sessionFile).length;
    begin("terminal-set-model");
    {
      const users = await userCount();
      for (const model of [B, A]) {
        const [provider, ...id] = model.split("/");
        await main.request({ modelId: id.join("/"), provider, type: "set_model" });
        const { tookMs } = await page.headerUntil(model, SHOWN_WITHIN_MS, (h) => h.model === model);
        const state = await main.state();
        expect(state.model === model, `get_state reports ${state.model} after set_model ${model}`);
        note(
          `RPC set_model ${model}: get_state ${state.model}; the header showed it ${tookMs} ms after the RPC answer`
        );
      }
      await page.shot("terminal-set-model");
      expect((await userCount()) === users, "the session file gained a user message");
      note(`the session file holds ${users} user messages, as before`);
      pass();
    }
    begin("terminal-set-thinking");
    {
      const users = await userCount();
      const level = "medium";
      await main.request({ level, type: "set_thinking_level" });
      const { tookMs } = await page.headerUntil(
        level,
        SHOWN_WITHIN_MS,
        (h) => h.thinking === level
      );
      const effort = await main.effort();
      expect(
        effortLevel(effort) === level,
        `/effort reports ${effortText(effort)} after set_thinking_level ${level}`
      );
      expect((await userCount()) === users, "the session file gained a user message");
      await page.shot("terminal-set-thinking");
      note(
        `RPC set_thinking_level ${level}: /effort ${effortText(effort)}; the header showed it ${tookMs} ms after the RPC answer; no user message`
      );
      pass();
    }
    begin("terminal-typed-effort");
    {
      const users = await userCount();
      const level = "high";
      const printed = await main.typed(`/effort ${level}`);
      expect(
        printed.includes(`Thinking set to ${level}.`),
        `/effort ${level} printed ${JSON.stringify(printed)}`
      );
      const { tookMs } = await page.headerUntil(
        level,
        SHOWN_WITHIN_MS,
        (h) => h.thinking === level
      );
      const effort = await main.effort();
      expect(effortLevel(effort) === level, `/effort reports ${effortText(effort)}`);
      expect((await userCount()) === users, "the session file gained a user message");
      await page.shot("terminal-typed-effort");
      note(
        `RPC prompt "/effort ${level}" printed ${JSON.stringify(printed)}; the header showed ${level} ${tookMs} ms after the RPC answer; no user message`
      );
      pass();
    }

    // ---- 3 --------------------------------------------------------------------------------------
    begin("message-switch");
    {
      const before = await storedIds(services, S);
      const sentAt = Date.now();
      await page.send(`/model ${B}`);
      const { tookMs } = await page.headerUntil(B, SHOWN_WITHIN_MS, (h) => h.model === B);
      const stored = await newMessage(services, S, `/model ${B}`, before);
      const reply = await sessionReply(services, S, stored.message.id, 60_000);
      const state = await main.state();
      expect(state.model === B, `get_state reports ${state.model}`);
      await page.shot("message-switch");
      note(
        `the composer sent "/model ${B}" (message ${stored.message.id}); the header showed ${B} ${tookMs} ms after Send (${Date.now() - sentAt} ms to here); the session replied "${reply}"; get_state ${state.model}`
      );
      pass();
    }

    // ---- 4 --------------------------------------------------------------------------------------
    begin("picker-model");
    {
      const { status, stored, tookMs } = await pickAndAnswer(S, "model", A, `Model set to ${A}.`);
      expect(
        stored.message.author.kind === "user" && stored.message.author.id === PERSON,
        `Dispatch stored "/model ${A}" as ${JSON.stringify(stored.message.author)}, want ${PERSON}`
      );
      const reply =
        stored.replies.find((each) => each.author.kind === "session")?.body ??
        (await sessionReply(services, S, stored.message.id, 30_000));
      expect(reply === `Model set to ${A}.`, `the session replied "${reply}"`);
      const shown = await until(
        "the thread to show the switch and its answer",
        30_000,
        async () => {
          const thread = await page.thread();
          return thread.mine.includes(`/model ${A}`) &&
            thread.replies.some((each) => each.includes(`Model set to ${A}.`))
            ? thread
            : undefined;
        }
      );
      const { effort, model } = await oracle(main);
      expect(model === A, `get_state reports ${model}`);
      const { header } = await page.headerUntil(
        `${A} and ${effortLevel(effort)}`,
        SHOWN_WITHIN_MS,
        (h) => h.model === A && h.thinking === effortLevel(effort)
      );
      note(
        `picked ${A}: Dispatch stored "/model ${A}" by ${stored.message.author.kind} ${stored.message.author.id} (message ${stored.message.id}); the switch line read "${status}" ${tookMs} ms after the pick; the thread shows "/model ${A}" as ${PERSON}'s and the reply "${reply}"`
      );
      note(
        `get_state ${model}; /effort ${effortText(effort)}; header ${headerText(header)}; ${shown.mine.length} of ${PERSON}'s messages shown`
      );
      const body = "Reply with the one word: alpha";
      const prompt = await promptFromPage(main, body);
      expect(
        prompt.turns.length > 0 && prompt.turns.every((turn) => turnModel(turn) === A),
        `the prompt's turns ran on ${prompt.turns.map(turnModel).join(", ")}, want ${A}`
      );
      const labels = await until(`the page to label the turn ${A}`, 30_000, async () => {
        const thread = await page.thread();
        return thread.turnModels.at(-1) === A ? thread.turnModels : undefined;
      });
      await page.shot("picker-model");
      note(
        `the composer's "${body}" (message ${prompt.id}) ran ${prompt.turns.length} turn(s), each on ${A} in the session file; the page labels the turn ${labels.at(-1)}`
      );
      pass();
    }

    // ---- 5 --------------------------------------------------------------------------------------
    begin("picker-auto-kept");
    {
      const auto = await pickAndAnswer(S, "thinking", "auto", "Thinking set to auto.");
      await page.headerUntil("auto", SHOWN_WITHIN_MS, (h) => h.thinking === "auto");
      const model = await pickAndAnswer(S, "model", B, `Model set to ${B}.`);
      const { effort, model: running } = await oracle(main);
      expect(running === B, `get_state reports ${running}`);
      expect(
        effortLevel(effort) === "auto",
        `/effort reports ${effortText(effort)} after the model pick, want auto`
      );
      const { header } = await page.headerUntil(
        `${B} and auto`,
        SHOWN_WITHIN_MS,
        (h) => h.model === B && h.thinking === "auto"
      );
      await page.shot("picker-auto-kept");
      note(
        `picked auto ("${auto.status}"), then ${B} ("${model.status}"): get_state ${running}; /effort ${effortText(effort)}; header ${headerText(header)}`
      );
      pass();
    }

    // ---- 6 --------------------------------------------------------------------------------------
    begin("picker-level-after-model");
    {
      const onB = concrete(lists[B] as Effort);
      const onA = concrete(lists[A] as Effort);
      const level = onB.find((each) => !onA.includes(each)) ?? (onB.at(-1) as string);
      await pickAndAnswer(S, "thinking", level, `Thinking set to ${level}.`);
      await page.headerUntil(level, SHOWN_WITHIN_MS, (h) => h.thinking === level);
      const startState = await oracle(main);
      expect(
        startState.model === B && effortLevel(startState.effort) === level,
        `the main session starts at ${startState.model}, ${effortText(startState.effort)}`
      );
      // The twin: a second session of the same profile, brought to the same model and level
      // through RPC, then sent `/model <A>` as a person's message through Dispatch.
      const twin = await startOmp("twin", rig.plugin);
      const T = await registers(twin);
      const [provider, ...id] = B.split("/");
      await twin.request({ modelId: id.join("/"), provider, type: "set_model" });
      await twin.request({ level, type: "set_thinking_level" });
      const twinStart = await oracle(twin);
      expect(
        twinStart.model === B && effortLevel(twinStart.effort) === level,
        `the twin starts at ${twinStart.model}, ${effortText(twinStart.effort)}`
      );
      const sent = await services.send(T, `/model ${A}`, PERSON);
      const twinReply = await sessionReply(services, T, sent.id, 60_000);
      expect(twinReply === `Model set to ${A}.`, `the twin replied "${twinReply}"`);
      const twinAfter = await oracle(twin);
      const expected = effortLevel(twinAfter.effort);
      await twin.stop();
      const picked = await pickAndAnswer(S, "model", A, `Model set to ${A}.`);
      const after = await oracle(main);
      const { header } = await page.headerUntil(
        `${A} and ${expected}`,
        SHOWN_WITHIN_MS,
        (h) => h.model === A && h.thinking === expected
      );
      expect(
        after.model === A && effortLevel(after.effort) === expected,
        `the main session's /effort reports ${effortText(after.effort)} on ${after.model}, the twin's ${expected}`
      );
      await page.shot("picker-level-after-model");
      note(
        `both at ${B} and ${level}; the twin ${T}, sent "/model ${A}" as a message, then reports ${twinAfter.model}, ${effortText(twinAfter.effort)}`
      );
      note(
        `the main session, picked ${A} in the header ("${picked.status}"): get_state ${after.model}, /effort ${effortText(after.effort)}; header ${headerText(header)}`
      );
      pass();
    }

    // ---- 7 --------------------------------------------------------------------------------------
    begin("picker-effort");
    {
      const before = await main.effort();
      const level =
        concrete(before).find((each) => each !== effortLevel(before) && each === "medium") ??
        (concrete(before).find((each) => each !== effortLevel(before)) as string);
      const picked = await pickAndAnswer(S, "thinking", level, `Thinking set to ${level}.`);
      const effort = await main.effort();
      expect(effortLevel(effort) === level, `/effort reports ${effortText(effort)}, want ${level}`);
      const { header } = await page.headerUntil(
        level,
        SHOWN_WITHIN_MS,
        (h) => h.thinking === level
      );
      await page.shot("picker-effort");
      note(
        `picked ${level} from ${effortText(before)} ("${picked.status}" ${picked.tookMs} ms after the pick); /effort ${effortText(effort)}; header ${headerText(header)}`
      );
      pass();
    }

    // ---- 8 --------------------------------------------------------------------------------------
    begin("picker-during-turn");
    {
      const file = (await main.state()).sessionFile;
      const startAt = entries(file).length;
      const long =
        "Use your bash tool to run exactly this command in the foreground: sleep 45 . When it has finished, reply with the one word: done";
      const prompt = await main.prompt(long);
      await until("the long turn to be running its command", 120_000, async () => {
        const state = await main.state();
        const running = entries(file)
          .slice(startAt)
          .some((entry) => entry.type === "message" && entry.message?.role === "assistant");
        return state.isStreaming && running ? true : undefined;
      });
      const before = await storedIds(services, S);
      const pickedAt = Date.now();
      await page.pickModel(B);
      const want = `The session answered: Model set to ${B}.`;
      await page.headerUntil(`the switch line "${want}"`, 30_000, (h) => h.status === want);
      const answeredMs = Date.now() - pickedAt;
      const during = await main.state();
      expect(during.isStreaming, "the turn had ended by the time the session answered the switch");
      const stored = await newMessage(services, S, `/model ${B}`, before);
      note(
        `picked ${B} while the turn ran; the session answered "${want.slice("The session answered: ".length)}" ${answeredMs} ms later (message ${stored.message.id}), with the turn still running (get_state isStreaming ${during.isStreaming}, model ${during.model})`
      );
      await main.settled(prompt, 300_000);
      await idle(main);
      // The run's first request, which asked for the command, was on the old model; every
      // request the session started after it answered the switch (the run's continuation once the
      // command returned) is on the picked one.
      const answeredAt = pickedAt + answeredMs;
      const run = entries(file)
        .slice(startAt)
        .filter((entry) => entry.type === "message" && entry.message?.role === "assistant");
      const later = run.filter((entry) => (entry.message?.timestamp ?? 0) > answeredAt);
      note(
        `the long prompt's run: ${run.map((entry) => `${turnModel(entry)} (${entry.message?.stopReason}, started ${(entry.message?.timestamp ?? 0) > answeredAt ? "after" : "before"} the answer)`).join(", ")}`
      );
      expect(
        turnModel(run[0] as Entry) === A,
        `the running turn started on ${turnModel(run[0] as Entry)}, want ${A}`
      );
      expect(
        later.every((entry) => turnModel(entry) === B),
        `a request started after the switch's answer ran on ${later.map(turnModel).join(", ")}, want ${B}`
      );
      const body = "Reply with the one word: bravo";
      const next = await promptFromPage(main, body);
      expect(
        next.turns.length > 0 && next.turns.every((turn) => turnModel(turn) === B),
        `the next turn ran on ${next.turns.map(turnModel).join(", ")}, want ${B}`
      );
      await page.shot("picker-during-turn");
      note(`the next prompt, "${body}", ran on ${next.turns.map(turnModel).join(", ")}`);
      pass();
    }

    // ---- 9 --------------------------------------------------------------------------------------
    begin("picker-refusal");
    {
      const before = await oracle(main);
      const replay = await page.replay(S);
      const settings = replay.filter((frame) => frame.kind === "settings").at(-1);
      expect(
        settings?.reported === true,
        `the replay of ${S} carries no reported settings frame: ${JSON.stringify(settings ?? null)}`
      );
      const top = Math.max(...replay.map((frame) => frame.seq));
      const injected = {
        ...settings,
        models: [...(settings?.models as string[]), rig.unkeyedModel],
        seq: top + 1_000_000,
      };
      await services.publishFrame(S, injected);
      await page.headerUntil(`${rig.unkeyedModel} among the models`, SHOWN_WITHIN_MS, (h) =>
        h.modelOptions.includes(rig.unkeyedModel)
      );
      note(
        `a bare bus client published on agentstream.${S}.frames a reported settings frame at seq ${injected.seq} (the replay's highest is ${top}) listing the session's ${(settings?.models as string[]).length} models and ${rig.unkeyedModel}`
      );
      const want = `Failed to set model: No API key for ${rig.unkeyedModel}`;
      const picked = await pickAndAnswer(S, "model", rig.unkeyedModel, want);
      const after = await oracle(main);
      expect(after.model === before.model, `get_state reports ${after.model}, was ${before.model}`);
      expect(
        effortText(after.effort) === effortText(before.effort),
        `/effort reports ${effortText(after.effort)}, was ${effortText(before.effort)}`
      );
      const { header } = await page.headerUntil(
        `${before.model}`,
        5_000,
        (h) => h.model === before.model
      );
      await page.shot("picker-refusal");
      note(
        `picked ${rig.unkeyedModel}: the switch line read "${picked.status}"; get_state ${after.model} and /effort ${effortText(after.effort)}, as before; header ${headerText(header)}`
      );
      await page.reload();
      const listed = await main.models();
      const restored = await page.headerUntil(
        "the session's own list after a reload",
        60_000,
        (h) =>
          h.model === before.model &&
          !h.modelOptions.includes(rig.unkeyedModel) &&
          h.modelOptions.length === listed.length
      );
      note(
        `after a reload the header offers the session's own ${restored.header.modelOptions.length} models again`
      );
      pass();
    }

    // ---- 10 -------------------------------------------------------------------------------------
    begin("forged-switch-changes-nothing");
    {
      const before = await oracle(main);
      const forgedModel = before.model === B ? A : B;
      const forgedLevel = effortLevel(before.effort) === "high" ? "low" : "high";
      const file = before.state.sessionFile;
      const users = userMessages(file).length;
      const forgedId = crypto.randomUUID();
      const run = forgedId.slice(0, 8);
      await services.forgeDispatchFrame(S, forgedId, `/model ${forgedModel}`, `forged-${run}`);
      const sender = `rig-sender-${run}`;
      await services.registerClient(sender);
      await services.envoyMessage(sender, S, `/effort ${forgedLevel}`, `envoy-${run}`);
      await until("the forged frame and the Envoy message to arrive as cards", 120_000, () =>
        handledAttempts(file).includes(forgedId) &&
        cards(file, `/model ${forgedModel}`).length > 0 &&
        cards(file, `/effort ${forgedLevel}`).length > 0
          ? true
          : undefined
      );
      await idle(main);
      const after = await oracle(main);
      await page.shot("forged-switch-changes-nothing");
      note(`before: get_state ${before.model}, /effort ${effortText(before.effort)}`);
      note(
        `if either were applied: the model would read ${forgedModel} (not ${before.model}) or the level ${forgedLevel} (not ${effortLevel(before.effort)})`
      );
      note(
        `a frame forged with the listener token, a person-shaped steer naming message ${forgedId}, which Dispatch never stored, with body "/model ${forgedModel}": recorded as handled attempt ${forgedId} and delivered as a card`
      );
      note(
        `an Envoy message "/effort ${forgedLevel}" from the registered client ${sender}: delivered as a card`
      );
      note(`after: get_state ${after.model}, /effort ${effortText(after.effort)}`);
      expect(after.model === before.model, `the forged switch changed the model to ${after.model}`);
      expect(
        effortText(after.effort) === effortText(before.effort),
        `the Envoy message changed the thinking level to ${effortText(after.effort)}`
      );
      const raw = userMessages(file)
        .slice(users)
        .map((entry) => text(entry.message?.content));
      expect(
        !raw.some(
          (each) =>
            each.includes(`/model ${forgedModel}`) || each.includes(`/effort ${forgedLevel}`)
        ),
        `a forged switch became a user message: ${JSON.stringify(raw)}`
      );
      pass();
    }

    // ---- 11 -------------------------------------------------------------------------------------
    begin("agent-token-refused");
    {
      const before = await oracle(main);
      const stored = (await services.thread(S)).length;
      const target = before.model === B ? A : B;
      const answer = await services.sendAsAgent(S, `/model ${target}`);
      const code =
        (answer.body as { code?: string; error?: { code?: string } } | null)?.code ??
        (answer.body as { error?: { code?: string } } | null)?.error?.code;
      expect(
        answer.status === 403 && JSON.stringify(answer.body).includes("HUMAN_ONLY"),
        `the agent bearer's switch answered ${answer.status} ${JSON.stringify(answer.body)}`
      );
      await sleep(3_000);
      const after = await oracle(main);
      expect((await services.thread(S)).length === stored, "Dispatch stored the agent's switch");
      expect(
        after.model === before.model && effortText(after.effort) === effortText(before.effort),
        `the session changed: ${after.model}, ${effortText(after.effort)}`
      );
      await page.shot("agent-token-refused");
      note(
        `POST /api/v1/agents/${S}/messages "/model ${target}" with the session's Dispatch bearer: ${answer.status} ${code ?? ""} ${JSON.stringify(answer.body)}`
      );
      note(
        `Dispatch still stores ${stored} messages; get_state ${after.model}, /effort ${effortText(after.effort)}, as before`
      );
      pass();
    }

    // ---- 12 -------------------------------------------------------------------------------------
    begin("other-session-unchanged");
    {
      const now = await oracle(other);
      expect(
        now.model === otherStart.model && effortText(now.effort) === effortText(otherStart.effort),
        `the other session is at ${now.model}, ${effortText(now.effort)}, started at ${otherStart.model}, ${effortText(otherStart.effort)}`
      );
      const changes = entries(now.state.sessionFile).filter(
        (entry) => entry.type === "model_change" || entry.type === "thinking_level_change"
      );
      expect(
        changes.length <= 2,
        `the other session's file records ${changes.length} model or level changes`
      );
      await page.shot("other-session-unchanged");
      note(
        `the other session ${O}, started before every switch: get_state ${now.model}, /effort ${effortText(now.effort)}, as at its start; ${changes.length} change entries in its file (its start's own)`
      );
      pass();
    }

    // ---- 13 -------------------------------------------------------------------------------------
    begin("no-answer");
    {
      const before = await oracle(main);
      const shown = await page.header();
      const target = before.model === B ? A : B;
      const storedBefore = await storedIds(services, S);
      main.signal("SIGSTOP");
      note(`SIGSTOP Oh My Pi pid ${main.pid}`);
      const pickedAt = Date.now();
      await page.pickModel(target);
      const stored = await newMessage(services, S, `/model ${target}`, storedBefore);
      const attempt = stored.message.deliveries.at(-1);
      expect(
        attempt !== undefined && attempt.state !== "failed",
        `Dispatch's send failed: ${JSON.stringify(attempt ?? null)}`
      );
      const { header } = await page.headerUntil(
        "the no-answer line",
        45_000,
        (h) => h.status === NO_ANSWER
      );
      const waited = Date.now() - pickedAt;
      expect(
        header.model === shown.model && header.thinking === shown.thinking,
        `the header moved from ${headerText(shown)} to ${headerText(header)}`
      );
      expect(!header.pickersDisabled, "the pickers are still disabled after the no-answer line");
      await page.shot("no-answer");
      note(
        `picked ${target} (message ${stored.message.id}, attempt ${attempt?.attempt} ${attempt?.state}); ${waited} ms later the switch line read "${header.status}"; the header still shows ${header.model}, ${header.thinking}, pickers enabled`
      );
      const createdAt = Date.parse(stored.message.created_at);
      const wait = createdAt + 65_000 - Date.now();
      if (wait > 0) await sleep(wait);
      main.signal("SIGCONT");
      note(`SIGCONT ${Math.round((Date.now() - createdAt) / 1000)}s after the message was stored`);
      const file = before.state.sessionFile;
      await until("the late frame to arrive as a card", 120_000, () =>
        handledAttempts(file).includes(stored.message.id) &&
        cards(file, `/model ${target}`).length > 0
          ? true
          : undefined
      );
      // The session logs Dispatch's refusal of its accept by the refusal's own text, which
      // Dispatch writes only with ACCEPT_STALE (message_delivery.go); the same accept, asked again
      // with the session's bearer, answers the code itself.
      const logs = readdirSync(`${rig.home}/.omp/logs`).filter((name) =>
        name.endsWith(`.${main.pid}.log`)
      );
      const stale = await until("the session's log to record the refused accept", 30_000, () => {
        const lines = logs.flatMap((name) =>
          readFileSync(`${rig.home}/.omp/logs/${name}`, "utf8").split("\n")
        );
        return lines.find(
          (line) =>
            line.includes(stored.message.id) &&
            line.includes(`attempt ${attempt?.attempt} was requested more than a minute ago`)
        );
      });
      const again = await services.accept(stored.message.id, attempt?.attempt ?? 1, S);
      expect(
        again.status === 409 && JSON.stringify(again.body).includes('"ACCEPT_STALE"'),
        `the accept asked again answered ${again.status} ${JSON.stringify(again.body)}, want 409 ACCEPT_STALE`
      );
      await idle(main);
      const after = await oracle(main);
      expect(after.model === before.model, `get_state reports ${after.model}, was ${before.model}`);
      const delivered = (await services.thread(S)).find(
        (read) => read.message.id === stored.message.id
      );
      expect(
        delivered?.message.deliveries.every((each) => each.accepted_as === null) === true,
        `Dispatch records the late switch accepted: ${JSON.stringify(delivered?.message.deliveries)}`
      );
      note(`the session's log: ${stale.slice(0, 400)}`);
      note(`the same accept asked again: ${again.status} ${JSON.stringify(again.body)}`);
      note(
        `the frame arrived as a card and a handled attempt; get_state ${after.model}, /effort ${effortText(after.effort)}, as before; Dispatch records no acceptance`
      );
      pass();
    }

    // ---- 14 -------------------------------------------------------------------------------------
    begin("resume-keeps-switch");
    let resumed: Omp;
    {
      const before = await oracle(main);
      const file = before.state.sessionFile;
      await main.stop();
      note(`ended Oh My Pi pid ${main.pid} at ${before.model}, ${effortText(before.effort)}`);
      const relaunched = Date.now();
      resumed = await startOmp("resumed", rig.plugin, ["--resume", file]);
      const state = await resumed.state();
      expect(state.sessionId === S, `--resume opened session ${state.sessionId}, want ${S}`);
      await registers(resumed, relaunched);
      const after = await oracle(resumed);
      expect(
        after.model === before.model && effortText(after.effort) === effortText(before.effort),
        `the resumed session is at ${after.model}, ${effortText(after.effort)}, was ${before.model}, ${effortText(before.effort)}`
      );
      await page.reload();
      const { header } = await page.headerUntil(
        `${before.model} and ${effortLevel(before.effort)}`,
        60_000,
        (h) =>
          h.model === before.model && h.thinking === effortLevel(before.effort) && h.modelPicker
      );
      await page.shot("resume-keeps-switch");
      note(
        `relaunched with --resume ${file} (pid ${resumed.pid}): session ${state.sessionId}; get_state ${after.model}, /effort ${effortText(after.effort)}; the reloaded header ${headerText(header)}`
      );
      pass();
    }

    // ---- 15 -------------------------------------------------------------------------------------
    begin("new-session-defaults");
    {
      const fresh = await startOmp("new", rig.plugin);
      const N = await registers(fresh);
      const now = await oracle(fresh);
      expect(
        now.model === A && effortLevel(now.effort) === rig.defaultLevel,
        `the new session is at ${now.model}, ${effortText(now.effort)}, want the profile's ${A} and ${rig.defaultLevel}`
      );
      await page.live(N);
      const { header } = await page.headerUntil(
        `${A} and ${rig.defaultLevel}`,
        60_000,
        (h) => h.model === A && h.thinking === rig.defaultLevel
      );
      await page.shot("new-session-defaults");
      const switched = await oracle(resumed);
      note(
        `a new session ${N} of the same profile: get_state ${now.model}, /effort ${effortText(now.effort)}; its header ${headerText(header)}; the switched session stays at ${switched.model}, ${effortText(switched.effort)}`
      );
      await fresh.stop();
      await page.live(S);
      pass();
    }

    // ---- 16 -------------------------------------------------------------------------------------
    begin("fallback");
    {
      await page.headerUntil("the resumed session's pickers", 60_000, (h) => h.modelPicker);
      const picked = await pickAndAnswer(S, "model", BROKEN_MODEL, `Model set to ${BROKEN_MODEL}.`);
      await page.headerUntil(BROKEN_MODEL, SHOWN_WITHIN_MS, (h) => h.model === BROKEN_MODEL);
      const file = (await resumed.state()).sessionFile;
      const startAt = entries(file).length;
      const body = "Reply with the one word: charlie";
      const before = await storedIds(services, S);
      await page.send(body);
      await newMessage(services, S, body, before);
      const fallback = await until("a model_change with role fallback", 240_000, () =>
        entries(file)
          .slice(startAt)
          .find((entry) => entry.type === "model_change" && entry.role === "fallback")
      );
      const entryAt = Date.parse(fallback.timestamp ?? "");
      const usersAtFallback = userMessages(file).length;
      const { header } = await page.headerUntil(A, SHOWN_WITHIN_MS + 10_000, (h) => h.model === A);
      const shownAt = Date.now();
      expect(
        fallback.model === A,
        `the fallback moved the session to ${fallback.model}, want ${A}`
      );
      expect(
        shownAt - entryAt <= SHOWN_WITHIN_MS,
        `the header showed ${A} ${shownAt - entryAt} ms after the fallback entry`
      );
      expect(
        userMessages(file).length === usersAtFallback,
        "a user turn ran between the fallback and the header showing it"
      );
      await idle(resumed);
      // What each request of the prompt's run did, the retry on the fallback model included: the
      // check holds on the header, and the retry's own outcome is the host's, reported as seen.
      const turns = entries(file)
        .slice(startAt)
        .filter((entry) => entry.type === "message" && entry.message?.role === "assistant");
      await page.shot("fallback");
      note(
        `picked ${BROKEN_MODEL} ("${picked.status}"), then sent "${body}"; the run's requests: ${turns
          .map(
            (turn) =>
              `${turnModel(turn)} ${turn.message?.stopReason}${turn.message?.errorMessage ? ` (${turn.message.errorMessage.split("\n")[0]?.slice(0, 160)})` : ""}`
          )
          .join("; ")}`
      );
      expect(
        turnModel(turns[0] as Entry) === BROKEN_MODEL && turns[0]?.message?.stopReason === "error",
        `the prompt's first request ran on ${turnModel(turns[0] as Entry)}, want a failed one on ${BROKEN_MODEL}`
      );
      note(
        `the session file's model_change at ${fallback.timestamp}: role ${fallback.role}, model ${fallback.model}; the header showed ${header.model} ${shownAt - entryAt} ms after it, with no further user message`
      );
      pass();
    }

    // ---- 17 -------------------------------------------------------------------------------------
    begin("older-plugin-notice");
    {
      const older = await startOmp("released", rig.releasedPlugin);
      const R = await registers(older);
      const prompt = await older.prompt("Reply with the one word: delta");
      await older.settled(prompt, 180_000);
      const last = assistantMessages((await older.state()).sessionFile).at(-1);
      expect(last !== undefined, "the released plugin's session ran no turn");
      const lastModel = turnModel(last as Entry);
      await page.live(R);
      const { header } = await page.headerUntil(
        "the restart notice",
        60_000,
        (h) => h.notice === RESTART_NOTICE && h.model === lastModel
      );
      expect(
        !header.modelPicker && !header.thinkingPicker,
        `the older session's header offers a picker: ${headerText(header)}`
      );
      await page.shot("older-plugin-notice");
      const version = JSON.parse(
        readFileSync(`${rig.releasedPlugin}/package.json`, "utf8")
      ).version;
      note(
        `session ${R} on the released @sjawhar/pi-envoy ${version}: its last turn ran on ${lastModel}; header ${headerText(header)}`
      );
      await older.stop();
      await page.live(S);
      pass();
    }

    // ---- 18 -------------------------------------------------------------------------------------
    begin("attribution");
    {
      await page.headerUntil("the session's pickers", 60_000, (h) => h.modelPicker);
      const target = (await resumed.state()).model === B ? A : B;
      const sent = await services.send(S, `/model ${target}`, OTHER_PERSON);
      const reply = await sessionReply(services, S, sent.id, 60_000);
      expect(
        reply === `Model set to ${target}.`,
        `the session replied "${reply}" to ${OTHER_PERSON}'s switch`
      );
      const stored = (await services.thread(S)).find((read) => read.message.id === sent.id);
      expect(
        stored?.message.author.id === OTHER_PERSON && stored.message.author.kind === "user",
        `Dispatch stored the switch as ${JSON.stringify(stored?.message.author)}`
      );
      const shown = await until(
        `${PERSON}'s page to show ${OTHER_PERSON}'s switch`,
        30_000,
        async () =>
          (await page.thread()).others.find(
            (each) => each.author === OTHER_PERSON && each.text.includes(`/model ${target}`)
          )
      );
      const { header } = await page.headerUntil(target, SHOWN_WITHIN_MS, (h) => h.model === target);
      await page.shot("attribution");
      note(
        `${OTHER_PERSON} sent "/model ${target}" with X-Dispatch-User: ${OTHER_PERSON} (message ${sent.id}); Dispatch stored it as ${stored?.message.author.kind} ${stored?.message.author.id}; the session replied "${reply}"`
      );
      note(
        `${PERSON}'s page shows it under "${shown.author}": "${shown.text}"; header ${headerText(header)}`
      );
      pass();
    }

    process.stdout.write(
      `all ${checkNumber} checks passed in ${Math.round((Date.now() - runStarted) / 1000)}s\n`
    );
  } catch (error) {
    if (browser !== undefined) await browser.shot(`${current}-failed`);
    const message = error instanceof Error ? error.message : String(error);
    process.stderr.write(`FAIL ${current}: ${message}\n`);
    if (!(error instanceof CheckFailure) && error instanceof Error && error.stack) {
      process.stderr.write(`${error.stack}\n`);
    }
    process.exitCode = 1;
  } finally {
    // Each session's file and every Oh My Pi log go to the evidence before the work directory
    // is removed. Two sessions can share a file (a resumed one), so each is kept under its label.
    for (const omp of started) {
      await omp.stop().catch(() => undefined);
      if (omp.sessionFile !== undefined && existsSync(omp.sessionFile)) {
        await Bun.write(`${rig.evidence}/sessions/${omp.label}.jsonl`, Bun.file(omp.sessionFile));
      }
    }
    if (existsSync(`${rig.home}/.omp/logs`)) {
      cpSync(`${rig.home}/.omp/logs`, `${rig.evidence}/logs/omp`, { recursive: true });
    }
    await browser?.close().catch(() => undefined);
  }
}

// ---- the database and a port ----------------------------------------------------------------------

function serverDsn(): URL {
  const dsn = process.env.LEGION_TEST_PG_DSN;
  if (dsn === undefined || dsn === "") refuse("LEGION_TEST_PG_DSN is required");
  return new URL(dsn);
}

function databaseName(name: string | undefined): string {
  if (name === undefined || !/^[a-z][a-z0-9_]*$/.test(name))
    refuse(`a database name of [a-z0-9_] is required, not ${name}`);
  return name;
}

async function createDatabase(): Promise<void> {
  const name = databaseName(args[0]);
  const server = serverDsn();
  const sql = new Bun.SQL(server.toString());
  try {
    await sql.unsafe(`create database "${name}"`);
  } finally {
    await sql.close();
  }
  const own = new URL(server.toString());
  own.pathname = `/${name}`;
  process.stdout.write(`${own.toString()}\n`);
}

async function dropDatabase(): Promise<void> {
  const name = databaseName(args[0]);
  const sql = new Bun.SQL(serverDsn().toString());
  try {
    await sql.unsafe(`drop database if exists "${name}" with (force)`);
  } finally {
    await sql.close();
  }
}

function freePort(): void {
  const server = Bun.listen({ hostname: "127.0.0.1", port: 0, socket: { data() {} } });
  process.stdout.write(`${server.port}\n`);
  server.stop(true);
}

if (command === "run") {
  if (args[0] === undefined) refuse("usage: run <rig.json>");
  await run(args[0]);
  // A request's or a stop's deadline timer would otherwise hold the process for minutes after the
  // last check.
  process.exit(process.exitCode ?? 0);
} else if (command === "db-create") await createDatabase();
else if (command === "db-drop") await dropDatabase();
else if (command === "free-port") freePort();
else refuse(`unknown command ${command ?? "(none)"}: run, db-create, db-drop or free-port`);
