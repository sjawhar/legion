import { commandHelp, commandsHelp, type ParsedCommand, parseCommand } from "./dispatch-command";
import { type ActiveDispatchConfig, activeDispatchConfig } from "./dispatch-config";
import type { DispatchHost } from "./dispatch-cwd";
import { executeDispatchTool } from "./dispatch-execute";
import {
  loadSessionMemory,
  pruneSessions,
  readSessionTitle,
  recordOutcome,
  sessionDirectory,
  sessionStateRoot,
  writeLongOutput,
  writePicture,
} from "./dispatch-session-state";
import { dispatchFollowNotice } from "./dispatch-subscribe";
import { messageFor } from "./errors";
import { ToolInputError } from "./tool-input-errors";

export interface DispatchCliIo {
  /** Where the text the model reads goes: results, refusals and notices alike. */
  stdout(text: string): void;
  /** A file's text; `-` is all of stdin. */
  readText(path: string): string;
  readonly cwd: string;
  readonly fetchImpl?: typeof fetch;
}

/** Exit codes: written or read, refused or failed, and a usage error. */
const OK = 0;
const REFUSED = 1;
const USAGE = 2;

const HOSTS: Record<string, DispatchHost> = { omp: "omp", claude: "claude", opencode: "opencode" };

/** Claude Code reads back at most 30,000 characters of a command's output by default
 *  (`BASH_MAX_OUTPUT_LENGTH`). The whole output stays under this, with room to spare, every line
 *  the CLI adds counted (`fitClaudeOutput`). */
const CLAUDE_OUTPUT_MAX = 25_000;

/** What the CLI prints when the call reached Dispatch but its local record could not be written. */
const STATE_WRITE_FAILED = "dispatch: Dispatch took the call, but this session's state";

function stateWriteFailed(problem: string): string {
  return `${STATE_WRITE_FAILED} could not be written: ${problem}`;
}

/**
 * The output, as lines, at most `limit` characters. Within the limit it is the result text, the
 * picture lines, then the `kept` lines (the follow notice and the CLI's own notes) as given. Past
 * it, the text and every picture line are written whole through `writeFull`, and the output names
 * that file: the result text is cut first, then picture lines are dropped from the end with a line
 * saying how many, while the kept lines stay whole. Only when the kept lines alone pass the limit
 * is the whole output, naming the file first, cut to it.
 */
export function fitClaudeOutput(
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
  const tail = (shown: number): string => {
    const left = pictures.length - shown;
    const dropped =
      left === 0
        ? []
        : [`(${left} more picture line${left === 1 ? "" : "s"} left out: see the full result)`];
    return [marker, ...pictures.slice(0, shown), ...dropped, ...kept].join("\n");
  };
  let shown = pictures.length;
  // The text needs at least the newline before the marker.
  while (shown > 0 && tail(shown).length + 1 > limit) shown -= 1;
  const after = tail(shown);
  const room = limit - after.length - 1;
  return room >= 0 ? `${text.slice(0, room)}\n${after}` : after.slice(0, limit);
}

/**
 * One `dispatch` command: parses `argv` into a Dispatch tool's arguments, runs that tool once as
 * the host session the environment names, and prints what the model reads. The host plugin sets
 * `DISPATCH_HOST` and the session's id in the agent's shell (`CLAUDE_CODE_SESSION_ID` under
 * Claude Code, `DISPATCH_SESSION_ID` elsewhere); an empty variable counts as unset. Memory a
 * long-lived host kept across calls lives in the session's state directory.
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

  let parsed: ParsedCommand;
  try {
    parsed = parseCommand(argv, io);
  } catch (error) {
    print(`dispatch: ${messageFor(error)}`);
    return USAGE;
  }
  if (parsed.kind === "help") {
    print(parsed.tool === undefined ? commandsHelp() : commandHelp(parsed.tool));
    return OK;
  }
  if (parsed.kind === "refused") {
    if (parsed.tool === undefined) {
      print([`dispatch: ${parsed.problems.join("\n")}`, "Run dispatch --help."].join("\n"));
      return USAGE;
    }
    print(new ToolInputError(parsed.tool, parsed.problems).message);
    return REFUSED;
  }

  const hostName = env.DISPATCH_HOST;
  const host = hostName === undefined ? undefined : HOSTS[hostName];
  if (host === undefined) {
    print(
      hostName === undefined
        ? "dispatch: DISPATCH_HOST is not set. The host plugin sets it in an agent's shell; to run dispatch by hand, set DISPATCH_HOST and DISPATCH_SESSION_ID yourself."
        : `dispatch: DISPATCH_HOST is ${JSON.stringify(hostName)}, not omp, claude or opencode.`
    );
    return USAGE;
  }
  const sessionVariable = host === "claude" ? "CLAUDE_CODE_SESSION_ID" : "DISPATCH_SESSION_ID";
  const sessionId = env[sessionVariable];
  if (sessionId === undefined) {
    print(
      `dispatch: ${sessionVariable} is not set, so this call has no session to act as. The host plugin sets it in an agent's shell.`
    );
    return USAGE;
  }
  let dir: string;
  try {
    dir = sessionDirectory(env, sessionId);
  } catch (error) {
    print(messageFor(error));
    return USAGE;
  }

  if (parsed.dryRun) {
    print(JSON.stringify(parsed.args, null, 2));
    return OK;
  }

  let config: ActiveDispatchConfig | null;
  try {
    config = activeDispatchConfig(env, { cwd: io.cwd });
  } catch (error) {
    print(`dispatch: ${messageFor(error)}`);
    return USAGE;
  }
  if (config === null) {
    print(
      "dispatch: Dispatch is not configured (DISPATCH_URL with DISPATCH_TOKEN or DISPATCH_TOKEN_FILE, or envoy.json)"
    );
    return USAGE;
  }

  let follows: Set<string>;
  try {
    pruneSessions(sessionStateRoot(env), Date.now());
    follows = loadSessionMemory(dir, sessionId);
  } catch (error) {
    print(`dispatch: ${messageFor(error)}`);
    return USAGE;
  }
  const { tool, args } = parsed;
  let text: string;
  const pictures: string[] = [];
  const notes: string[] = [];
  const problems: string[] = [];
  let entry: { tool: string; details?: unknown; error?: string };
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
    });
    text = result.text;
    try {
      for (const image of result.images ?? []) {
        const picture = writePicture(dir, image);
        pictures.push(`- picture: ${picture.path} (${image.mimeType}, ${picture.bytes} bytes)`);
      }
    } catch (error) {
      problems.push(messageFor(error));
    }
    const notice = dispatchFollowNotice(result.details);
    if (notice !== null && !follows.has(notice.ask)) {
      follows.add(notice.ask);
      notes.push(notice.text);
    }
    entry = { tool, details: result.details };
    code = OK;
  } catch (error) {
    text = messageFor(error);
    entry = { tool, error: text };
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
    host === "claude"
      ? fitClaudeOutput(text, pictures, kept, CLAUDE_OUTPUT_MAX, writeFull)
      : [text, ...pictures, ...kept].join("\n")
  );
  return code;
}
