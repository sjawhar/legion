// Oh My Pi in `--mode rpc`, as a Legion pane runs it, for the dispatch-session-settings driver:
// one process per `Omp`, its RPC requests by id, and what an `/effort` prompt prints.
import { appendFileSync } from "node:fs";
import type { FileSink, Subprocess } from "bun";
import { decoder, fail, type Rig, sleep, until } from "./session-settings-common";

export interface RpcEvent {
  readonly type?: string;
  readonly id?: string;
  readonly [key: string]: unknown;
}

export interface SessionState {
  readonly model: string | undefined;
  readonly isStreaming: boolean;
  readonly isSettled: boolean;
  readonly sessionFile: string;
  readonly sessionId: string;
}

/** What an `/effort` prompt printed: the level on its `Thinking:` line and the levels on its
 *  `Available:` line, or, for a model with no adjustable thinking, its one line. */
export type Effort =
  | { readonly level: string; readonly available: readonly string[] }
  | { readonly none: string };

export function effortText(effort: Effort): string {
  return "none" in effort ? effort.none : `${effort.level} of [${effort.available.join(", ")}]`;
}
export function effortLevel(effort: Effort): string {
  return "none" in effort ? "no adjustable thinking" : effort.level;
}
/** The concrete levels of an `/effort` answer: what it offers less `off` and `auto`. */
export function concrete(effort: Effort): string[] {
  return "none" in effort
    ? []
    : effort.available.filter((level) => level !== "off" && level !== "auto");
}

/** One Oh My Pi process in `--mode rpc`: its requests by id, its `command_output` lines, and every
 *  event it printed, kept in the evidence directory. */
export class Omp {
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

  private constructor(
    rig: Rig,
    keys: Readonly<Record<string, string>>,
    label: string,
    argv: readonly string[]
  ) {
    this.label = label;
    this.#log = `${rig.evidence}/logs/omp-${label}.rpc.jsonl`;
    const env: Record<string, string> = {
      ...keys,
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

  /** Starts Oh My Pi with the caller's provider `keys` in its environment, `extension` loaded
   *  (none for a bare session) and `extra` arguments, and waits for its `ready` line. */
  static async start(
    rig: Rig,
    keys: Readonly<Record<string, string>>,
    label: string,
    extension: string | null,
    extra: readonly string[] = []
  ): Promise<Omp> {
    const argv = [...(extension === null ? [] : ["--extension", extension]), "--mode", "rpc"];
    const omp = new Omp(rig, keys, label, [...argv, ...extra]);
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
