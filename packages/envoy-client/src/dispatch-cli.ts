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
 *  (`BASH_MAX_OUTPUT_LENGTH`). The whole output stays under this, with room to spare: past it,
 *  the result text is shortened and the full text is written to a file the agent opens, while the
 *  picture lines and the follow notice after it are kept whole. */
const CLAUDE_OUTPUT_MAX = 25_000;

/** What the CLI prints when the call reached Dispatch but its local record could not be written. */
const STATE_WRITE_FAILED = "dispatch: Dispatch took the call, but this session's state";

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
  let lines: string[];
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
    const after: string[] = [];
    let stateProblem: string | undefined;
    try {
      for (const image of result.images ?? []) {
        const picture = writePicture(dir, image);
        after.push(`- picture: ${picture.path} (${image.mimeType}, ${picture.bytes} bytes)`);
      }
    } catch (error) {
      stateProblem = messageFor(error);
    }
    const notice = dispatchFollowNotice(result.details);
    if (notice !== null && !follows.has(notice.ask)) {
      follows.add(notice.ask);
      after.push(notice.text);
    }
    let text = result.text;
    const tail = after.map((line) => `\n${line}`).join("");
    if (host === "claude" && text.length + tail.length > CLAUDE_OUTPUT_MAX) {
      try {
        const path = writeLongOutput(dir, text);
        const marker = `\n(the full result, ${text.length} characters: ${path})`;
        text = `${text.slice(0, Math.max(0, CLAUDE_OUTPUT_MAX - tail.length - marker.length))}${marker}`;
      } catch (error) {
        stateProblem ??= messageFor(error);
      }
    }
    lines = [`${text}${tail}`];
    if (stateProblem !== undefined) {
      lines.push(`${STATE_WRITE_FAILED} could not be written: ${stateProblem}`);
    }
    entry = { tool, details: result.details };
    code = OK;
  } catch (error) {
    const message = messageFor(error);
    lines = [message];
    entry = { tool, error: message };
    code = REFUSED;
  }
  try {
    recordOutcome(dir, sessionId, follows, entry);
  } catch (error) {
    lines.push(`${STATE_WRITE_FAILED} could not be written: ${messageFor(error)}`);
  }
  print(lines.join("\n"));
  return code;
}
