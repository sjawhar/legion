import { askgateState, consumeMarker, gatedWrite } from "./askgate-marker";
import {
  commandHelp,
  commandName,
  commandsHelp,
  type ParsedCommand,
  parseCommand,
} from "./dispatch-command";
import type { ActiveDispatchConfig } from "./dispatch-config";
import type { DispatchHost } from "./dispatch-cwd";
import { checkDispatchCall, dispatchServerUrls, executeDispatchTool } from "./dispatch-execute";
import {
  appendResult,
  loadSessionMemory,
  pruneSessions,
  type ResultInput,
  readSessionTitle,
  recordOutcome,
  sessionDirectory,
  sessionStateRoot,
  writeLongOutput,
  writePicture,
} from "./dispatch-session-state";
import { followedAsk, followNoticeText } from "./dispatch-subscribe";
import { messageFor } from "./errors";
import {
  handedConfigFile,
  sessionConfigFile,
  sessionDispatchSettings,
  sessionEnvoySettings,
} from "./session-settings";
import { ToolInputError } from "./tool-input-errors";
import { createEnvoyClient, EnvoyApiError } from "./transport";

export interface DispatchCliIo {
  /** Where the text the model reads goes: results, refusals and notices alike. */
  stdout(text: string): void;
  /** Where a result's warnings go (`DispatchToolResult.warnings`), one line each, beside it. */
  stderr(text: string): void;
  /** A file's text; `-` is all of stdin. */
  readText(path: string): string;
  readonly cwd: string;
  readonly fetchImpl?: typeof fetch;
}

/** Exit codes: written or read, refused or failed, and a usage error. */
const OK = 0;
const REFUSED = 1;
const USAGE = 2;

const HOSTS = ["omp", "claude", "opencode"] as const satisfies readonly DispatchHost[];

/** Claude Code reads back at most 30,000 characters of a command's output by default
 *  (`BASH_MAX_OUTPUT_LENGTH`). The whole output stays under this, with room to spare, every line
 *  the CLI adds counted (`fitOutput`). */
const CLAUDE_OUTPUT_MAX = 25_000;

/** What the CLI prints when the call reached Dispatch but its local record could not be written. */
const STATE_WRITE_FAILED = "dispatch: Dispatch took the call, but this session's state";

function stateWriteFailed(problem: string): string {
  return `${STATE_WRITE_FAILED} could not be written: ${problem}`;
}

/** How a gated write gets AskGate's marker, as a refusal or a warn-mode warning tells the agent. */
const LITERAL_FORM =
  "AskGate reviews a Dispatch write, and lets it post, only when it is sent as one bash call of literal words: `dispatch <command> <words…>`, with at most one `cd <path> &&` before it, one `2>&1` after it and one here-document opened with a quoted delimiter (`<<'EOF'`). A pipe, a loop, a variable, xargs, a script, an alias, or a process a program or an eval cell starts carries no marker.";

/** A line that refuses a call before it reaches Dispatch, each at the start of the line:
 *  `dispatch <command> was not called: …` (exit 1, `ToolInputError` and the refusals
 *  `executeDispatchTool` makes before a write), or a usage refusal (exit 2), which is `dispatch: `
 *  and its reason. The CLI's other `dispatch: ` lines refuse nothing: a result's warning
 *  (`dispatch: warning: `), the note that the session's state was not written
 *  (STATE_WRITE_FAILED), and the notice of a file that is not read, printed beside a result. */
const NOT_CALLED = /^dispatch ([a-z][a-z-]*) was not called: /;
const NOT_A_REFUSAL = [
  "dispatch: warning: ",
  STATE_WRITE_FAILED,
  // readEnvoyConfig's warnings: the OpenCode-era `.opencode/envoy.json` beside a checkout.
  /^dispatch: \S+ is not read(?:: | for this profile| under | and is on another filesystem )/,
];

export interface CliRefusal {
  /** The refusal's first line. */
  readonly line: string;
  /** The command a `was not called` line names; null for a usage refusal, which names none. */
  readonly command: string | null;
}

/**
 * Each refusal the dispatch CLI printed in `text`, by its first line: what a reader of the CLI's
 * output (an agent's transcript, where a pipe can hide the exit) needs to tell a refused call from
 * one Dispatch answered. A line counts only at the start of a line of `text`.
 */
export function cliRefusals(text: string): CliRefusal[] {
  const found: CliRefusal[] = [];
  for (const line of text.split("\n")) {
    const command = NOT_CALLED.exec(line)?.[1];
    if (command !== undefined) {
      found.push({ line, command });
      continue;
    }
    if (!line.startsWith("dispatch: ")) continue;
    if (NOT_A_REFUSAL.some((ok) => (typeof ok === "string" ? line.startsWith(ok) : ok.test(line))))
      continue;
    found.push({ line, command: null });
  }
  return found;
}

/** How long a result waits on the listener for the session's subscriptions, after the write
 *  itself has landed: one attempt, no retry, since the read is best effort. */
const SUBSCRIPTIONS_TIMEOUT_MS = 1_500;

/**
 * The output, at most `limit` characters (unbounded for a host that shows all of it). Within the
 * limit it is the result text, the picture lines, then the `kept` lines (the follow notice and the
 * CLI's own notes) as given. Past it, the text and every picture line are written whole through
 * `writeFull`, and the output names that file: the result text is cut first, then picture lines are
 * dropped from the end with a line saying how many, while the kept lines stay whole. Only when the
 * kept lines alone pass the limit is the whole output, naming the file first, cut to it.
 */
export function fitOutput(
  text: string,
  pictures: readonly string[],
  kept: readonly string[],
  limit: number,
  writeFull: (full: string) => string
): string {
  const whole = [text, ...pictures, ...kept].join("\n");
  if (whole.length <= limit) return whole;
  const full = [text, ...pictures].join("\n");
  const marker = `(the full result, ${full.length} characters: ${writeFull(full)})`;
  const droppedLine = (left: number): string =>
    `(${left} more picture line${left === 1 ? "" : "s"} left out: see the full result)`;
  // The tail's length for each number of picture lines shown, from the parts' lengths alone, one
  // newline between parts; the text needs at least the newline before the marker.
  const keptLength = kept.reduce((sum, line) => sum + line.length + 1, 0);
  let pictureLength = pictures.reduce((sum, line) => sum + line.length + 1, 0);
  let shown = pictures.length;
  const tailLength = (): number => {
    const left = pictures.length - shown;
    return (
      marker.length + pictureLength + (left === 0 ? 0 : droppedLine(left).length + 1) + keptLength
    );
  };
  while (shown > 0 && tailLength() + 1 > limit) {
    shown -= 1;
    pictureLength -= (pictures[shown]?.length ?? 0) + 1;
  }
  const left = pictures.length - shown;
  const after = [
    marker,
    ...pictures.slice(0, shown),
    ...(left === 0 ? [] : [droppedLine(left)]),
    ...kept,
  ].join("\n");
  const room = limit - after.length - 1;
  return room >= 0 ? `${text.slice(0, room)}\n${after}` : after.slice(0, limit);
}

/**
 * One `dispatch` command: parses `argv` into a Dispatch tool's arguments, runs that tool once as
 * the host session the environment names, and prints what the model reads. The host plugin sets
 * `DISPATCH_HOST`, `ENVOY_CONFIG_FILE` (the envoy.json it chose) and the session's id in the
 * agent's shell (`CLAUDE_CODE_SESSION_ID` under Claude Code, `DISPATCH_SESSION_ID` elsewhere); an
 * empty variable counts as unset. With `DISPATCH_HOST` unset, `CLAUDE_CODE_CHILD_SESSION=1`, which
 * Claude Code sets in every Bash and hook process it runs, is host `claude`: a Claude Code
 * subagent's Bash can lack what the bridge's `SessionStart` hook writes to `CLAUDE_ENV_FILE`
 * (anthropics/claude-code#46696).
 *
 * The envoy.json it reads is the one its session's plugin chose: with `DISPATCH_HOST` set, the file
 * `ENVOY_CONFIG_FILE` names (each plugin sets the two together), else its host's own rule
 * (`envoyConfigPath(host)`); with `DISPATCH_HOST` unset and `CLAUDE_CODE_CHILD_SESSION=1`, Claude
 * Code's rule. Recomputing the file from a shell's other variables would follow ones a launcher or
 * an outer session left there. Memory a long-lived host kept across calls lives in the session's
 * state directory.
 */
export async function runDispatchCli(
  argv: readonly string[],
  rawEnv: Readonly<Record<string, string | undefined>>,
  io: DispatchCliIo
): Promise<number> {
  const print = (text: string): void => io.stdout(text.endsWith("\n") ? text : `${text}\n`);
  const env: Record<string, string> = {};
  for (const [name, value] of Object.entries(rawEnv)) {
    if (value !== undefined && value !== "") env[name] = value;
  }
  // A refusal before the call reaches Dispatch goes to the session's ledger too, whenever this
  // call knows its session, so a reader of the ledger (pi-envoy's run-end check, the
  // skill-scenario rig) sees a call whose output a pipe, `$(…)` or another program swallowed.
  // The host need not be known for the session to be: a Python `subprocess.run(["dispatch", …])`
  // inherits the session id without `DISPATCH_HOST`. A ledger that cannot be written changes
  // nothing here: the refusal is already printed, and the caller is exiting.
  const refuse = (
    text: string,
    code: number,
    tool?: string,
    askgate?: ResultInput["askgate"]
  ): number => {
    print(text);
    const sessionId = env.CLAUDE_CODE_SESSION_ID ?? env.DISPATCH_SESSION_ID;
    if (sessionId !== undefined) {
      try {
        appendResult(sessionDirectory(env, sessionId), {
          tool: tool ?? "dispatch",
          error: text.trimEnd(),
          ...(askgate === undefined ? {} : { askgate }),
        });
      } catch {
        // The session's directory is unusable; the printed refusal stands on its own.
      }
    }
    return code;
  };

  let parsed: ParsedCommand;
  try {
    parsed = parseCommand(argv, io);
  } catch (error) {
    return refuse(`dispatch: ${messageFor(error)}`, USAGE);
  }
  if (parsed.kind === "help") {
    print(parsed.tool === undefined ? commandsHelp() : commandHelp(parsed.tool));
    return OK;
  }
  if (parsed.kind === "refused") {
    return parsed.tool === undefined
      ? refuse(
          [`dispatch: ${parsed.problems.join("\n")}`, "Run dispatch --help."].join("\n"),
          USAGE
        )
      : refuse(new ToolInputError(parsed.tool, parsed.problems).message, REFUSED, parsed.tool);
  }

  const hostName =
    env.DISPATCH_HOST ?? (env.CLAUDE_CODE_CHILD_SESSION === "1" ? "claude" : undefined);
  const host = HOSTS.find((name) => name === hostName);
  if (host === undefined) {
    return refuse(
      hostName === undefined
        ? "dispatch: DISPATCH_HOST is not set. The host plugin sets it in an agent's shell; to run dispatch by hand, set DISPATCH_HOST and DISPATCH_SESSION_ID yourself."
        : `dispatch: DISPATCH_HOST is ${JSON.stringify(hostName)}, not omp, claude or opencode.`,
      USAGE,
      parsed.tool
    );
  }
  const sessionVariable = host === "claude" ? "CLAUDE_CODE_SESSION_ID" : "DISPATCH_SESSION_ID";
  const sessionId = env[sessionVariable];
  if (sessionId === undefined) {
    return refuse(
      `dispatch: ${sessionVariable} is not set, so this call has no session to act as. The host plugin sets it in an agent's shell.`,
      USAGE,
      parsed.tool
    );
  }
  let dir: string;
  try {
    dir = sessionDirectory(env, sessionId);
  } catch (error) {
    return refuse(messageFor(error), USAGE, parsed.tool);
  }

  // The file the session's plugin handed its shell (each plugin sets DISPATCH_HOST and
  // ENVOY_CONFIG_FILE together), else the host's own rule.
  const file =
    (env.DISPATCH_HOST === undefined ? null : handedConfigFile(env)) ??
    sessionConfigFile(host, env);
  if (parsed.dryRun) {
    // The call's own argument check, so the call refuses no arguments its dry run passed; what
    // the call needs besides (Dispatch's configuration, this session's state, Dispatch's answers)
    // a dry run does not read. It runs with Dispatch off too, reading a dashboard-URL ref only
    // against a server it knows, and its standard output is the JSON alone.
    const settings =
      file.path === null ? null : sessionDispatchSettings(file, env, { cwd: io.cwd }).dispatch;
    const { problems } = await checkDispatchCall({
      tool: parsed.tool,
      args: parsed.args,
      cwd: io.cwd,
      env,
      serverUrls: settings?.enabled === true ? dispatchServerUrls(settings) : [],
    });
    if (problems.length > 0) {
      return refuse(new ToolInputError(parsed.tool, problems).message, REFUSED, parsed.tool);
    }
    print(JSON.stringify(parsed.args, null, 2));
    return OK;
  }
  if (file.path === null) {
    return refuse(`dispatch: ${file.error}`, USAGE, parsed.tool);
  }
  const { dispatch: resolution, warnings } = sessionDispatchSettings(file, env, { cwd: io.cwd });
  // A checkout's OpenCode-era file, which is not read, is said on every call while it is there.
  for (const warning of warnings) print(`dispatch: ${warning}`);
  if (!resolution.enabled) {
    return refuse(
      resolution.error === null
        ? `dispatch: ${resolution.notice}`
        : `dispatch: dispatch config: ${resolution.error}`,
      USAGE,
      parsed.tool
    );
  }
  const config: ActiveDispatchConfig = resolution;

  // AskGate: a session a live Oh My Pi process records with its gate bound (`askgate.json` in
  // this directory) posts a gated write only with one marker the gate minted for this exact argv,
  // whatever ran the CLI. A session no live process records needs none.
  let unmarked: ResultInput["askgate"];
  if (gatedWrite(parsed.tool, parsed.args)) {
    const notCalled = `dispatch ${commandName(parsed.tool)} was not called:`;
    try {
      const gate = askgateState(dir);
      if (gate.kind === "broken") {
        return refuse(
          `${notCalled} AskGate's record for this session, ${gate.problem}, so whether AskGate reviewed this write cannot be told.`,
          REFUSED,
          parsed.tool,
          { mode: "block", why: gate.problem }
        );
      }
      if (gate.kind === "on") {
        const spent = consumeMarker(gate.stores, argv, parsed.args, Date.now());
        if (!spent.ok && gate.mode === "block") {
          return refuse(
            `${notCalled} AskGate is on for this session, and ${spent.why}. ${LITERAL_FORM} Send the write again in that form, as a bash call of its own.`,
            REFUSED,
            parsed.tool,
            { mode: "block", why: spent.why }
          );
        }
        if (!spent.ok) {
          unmarked = { mode: "warn", why: spent.why };
          print(
            `dispatch: warning: AskGate is on for this session in warn mode, and ${spent.why}, so this write posts unreviewed. ${LITERAL_FORM} Send the next one in that form.`
          );
        }
      }
    } catch (error) {
      return refuse(
        `${notCalled} AskGate's marker for this call could not be checked: ${messageFor(error)}.`,
        REFUSED,
        parsed.tool,
        { mode: "block", why: messageFor(error) }
      );
    }
  }

  // A result names an owner the session is not subscribed to, or the line that subscribes it,
  // only from the listener's record of the session's topics, read (Envoy's settings with it) once,
  // when a result first asks; with Envoy off, or no answer from it, a result says nothing about
  // subscriptions. A session the listener has no record of holds none. The write has landed by
  // the time a result asks, so nothing here may throw: any failure is null, never a refusal.
  let topics: Promise<readonly string[] | null> | undefined;
  const readTopics = async (): Promise<readonly string[] | null> => {
    try {
      const envoy = sessionEnvoySettings(file, env, { cwd: io.cwd, nats: false });
      if (!envoy.enabled) return null;
      const listener = createEnvoyClient({
        baseUrl: envoy.url,
        fetch: io.fetchImpl ?? fetch,
        token: envoy.token,
        timeoutMs: SUBSCRIPTIONS_TIMEOUT_MS,
        retry: false,
      });
      return (await listener.getInterest(sessionId)).topics;
    } catch (error) {
      return error instanceof EnvoyApiError && error.details.status === 404 ? [] : null;
    }
  };
  const subscriptions = (): Promise<readonly string[] | null> => {
    topics ??= readTopics();
    return topics;
  };

  let follows: Set<string>;
  try {
    pruneSessions(sessionStateRoot(env), Date.now());
    follows = loadSessionMemory(dir, sessionId);
  } catch (error) {
    return refuse(`dispatch: ${messageFor(error)}`, USAGE, parsed.tool);
  }
  const { tool, args } = parsed;
  let text: string;
  const pictures: string[] = [];
  const notes: string[] = [];
  const problems: string[] = [];
  let entry: ResultInput;
  let code: number;
  try {
    const result = await executeDispatchTool({
      tool,
      args,
      cwd: io.cwd,
      host,
      sessionId,
      sessionTitle: env.DISPATCH_SESSION_TITLE ?? readSessionTitle(dir),
      config,
      env,
      ...(io.fetchImpl === undefined ? {} : { fetchImpl: io.fetchImpl }),
      subscriptions,
    });
    text = result.text;
    for (const warning of result.warnings ?? []) {
      io.stderr(`${warning}\n`);
    }
    try {
      for (const image of result.images ?? []) {
        const picture = writePicture(dir, image);
        pictures.push(`- picture: ${picture.path} (${image.mimeType}, ${picture.bytes} bytes)`);
      }
    } catch (error) {
      problems.push(messageFor(error));
    }
    const followed = followedAsk(result.details);
    if (followed !== null && !follows.has(followed.ask)) {
      follows.add(followed.ask);
      notes.push(followNoticeText(followed, await subscriptions()));
    }
    entry = {
      tool,
      details: result.details,
      ...(unmarked === undefined ? {} : { askgate: unmarked }),
    };
    code = OK;
  } catch (error) {
    text = messageFor(error);
    entry = { tool, error: text, ...(unmarked === undefined ? {} : { askgate: unmarked }) };
    code = REFUSED;
  }
  try {
    recordOutcome(dir, sessionId, follows, entry);
  } catch (error) {
    problems.push(messageFor(error));
  }
  const kept = [...notes, ...problems.map(stateWriteFailed)];
  const writeFull = (full: string): string => {
    try {
      return writeLongOutput(dir, full);
    } catch (error) {
      return `not written, ${messageFor(error)}`;
    }
  };
  print(
    fitOutput(
      text,
      pictures,
      kept,
      host === "claude" ? CLAUDE_OUTPUT_MAX : Number.POSITIVE_INFINITY,
      writeFull
    )
  );
  return code;
}
