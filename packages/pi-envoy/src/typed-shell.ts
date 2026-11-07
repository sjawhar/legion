import { Buffer } from "node:buffer";
import { chmodSync, closeSync, mkdtempSync, openSync, readSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { messageFor } from "@legion/envoy-client/errors";

/**
 * The `!command` a person can send one Oh My Pi session from Dispatch (R4 of dispatch://INFRA-812).
 * A Send or an Aside whose first character is `!` is no prompt: the session runs the rest of it
 * under `/bin/sh -c` in its own working directory and answers with what it printed. The mark is
 * one `!` and the rest is the command whatever it starts with, so `!!printf hi` runs `!printf hi`
 * and is answered with that shell's own failure.
 *
 * This module runs the command and writes the three texts it becomes; which messages reach it, and
 * where its texts go, is `extensions/envoy.ts`'s (`runTypedInput`). A line typed at the session's
 * own terminal never reaches here, so it keeps the host's own shell mode.
 */

/** Characters of a command's output each of the three texts carries; past it the text is cut and
 *  says so. */
export const SHELL_OUTPUT_MAX = 8_000;

/** Bytes of a command's output read back from its file. Four times the character cut, so the read
 *  always holds more characters than the cut keeps, whatever the encoding costs, and a command
 *  that prints without end costs the session this much memory and no more. */
const SHELL_OUTPUT_BYTES = 4 * SHELL_OUTPUT_MAX;

const NOTHING_PRINTED = "The command printed nothing.";

/** What the agent is told before the command and its output: it is the person's command, not a
 *  request of its own. */
const SHELL_COMMAND_HEADING =
  "A person ran this shell command in this session from Dispatch. Nothing is asked of you; this is the command and what it printed.";

/** What a person's `!command` did: the command without its mark, what it printed (both streams, in
 *  the order the shell wrote them, cut at `SHELL_OUTPUT_MAX`), and how it ended when it did not
 *  end well. */
export interface ShellCommandResult {
  readonly command: string;
  readonly output: string;
  readonly cut: boolean;
  /** One sentence naming the exit status of a command that failed, was killed or was stopped;
   *  undefined for one that exited 0. */
  readonly status: string | undefined;
}

/** The command a person's message says to run, or undefined for a message that is no command: the
 *  body whose first character is `!`, without that one character. */
export function shellCommandFrom(body: string): string | undefined {
  return body.startsWith("!") ? body.slice(1) : undefined;
}

/** The shell this module starts: no standard input, and both its output streams on one file
 *  descriptor. */
type ShellSubprocess = Bun.Subprocess<"ignore", number, number>;

/** Kills every process the command started, which its own process group holds. */
function killGroup(pid: number): void {
  try {
    process.kill(-pid, "SIGKILL");
  } catch {
    // The group is already gone.
  }
}

/** The first `SHELL_OUTPUT_BYTES` of the file the command printed to. */
function readOutput(path: string): string {
  const fd = openSync(path, "r");
  try {
    const buffer = Buffer.alloc(SHELL_OUTPUT_BYTES);
    const read = readSync(fd, buffer, 0, buffer.length, 0);
    return new TextDecoder().decode(buffer.subarray(0, read));
  } finally {
    closeSync(fd);
  }
}

/**
 * Runs one `!command` under `/bin/sh -c` in `cwd` with this session's own environment, and answers
 * with what it printed and how it ended. It is stopped after `limitMs` with what it printed kept,
 * and runs in a process group of its own, so nothing it left running outlives it or holds the
 * session.
 *
 * Both streams go to a file in a private directory rather than to a pipe: a pipe would be held
 * open by any process the command left running, and a command that prints without end would cost
 * the session the memory of everything it printed. Only the first `SHELL_OUTPUT_BYTES` are read
 * back, which is always more characters than the cut keeps.
 */
export async function runShellCommand(
  command: string,
  options: { readonly cwd: string; readonly limitMs: number }
): Promise<ShellCommandResult> {
  const directory = mkdtempSync(join(tmpdir(), "envoy-shell-"));
  try {
    chmodSync(directory, 0o700);
    const path = join(directory, "output");
    const fd = openSync(path, "wx", 0o600);
    let child: ShellSubprocess;
    try {
      child = Bun.spawn(["/bin/sh", "-c", command], {
        cwd: options.cwd,
        detached: true,
        env: process.env,
        stderr: fd,
        stdin: "ignore",
        stdout: fd,
      });
    } catch (error) {
      return {
        command,
        cut: false,
        output: "",
        status: `The command could not start: ${messageFor(error)}.`,
      };
    } finally {
      closeSync(fd);
    }
    let stopped = false;
    const limit = setTimeout(() => {
      stopped = true;
      child.kill("SIGKILL");
      killGroup(child.pid);
    }, options.limitMs);
    try {
      await child.exited;
    } finally {
      clearTimeout(limit);
      // Whatever the outcome, nothing the command started outlives it.
      killGroup(child.pid);
    }
    const printed = readOutput(path);
    const cut = printed.length > SHELL_OUTPUT_MAX;
    return {
      command,
      cut,
      output: (cut ? printed.slice(0, SHELL_OUTPUT_MAX) : printed).trimEnd(),
      status: statusOf(child, stopped, options.limitMs),
    };
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
}

/** How a command ended, as the one sentence the three texts carry, or undefined for one that
 *  exited 0. A command this session stopped says so rather than naming the signal that stopped
 *  it, which is the session's own doing and not the command's. */
function statusOf(child: ShellSubprocess, stopped: boolean, limitMs: number): string | undefined {
  if (stopped) return `The command was stopped after ${limitMs} ms.`;
  if (child.signalCode !== null) return `The command was killed by ${child.signalCode}.`;
  if (child.exitCode !== 0) return `The command ended with exit status ${child.exitCode}.`;
  return undefined;
}

/**
 * The person's line, what the command printed and how it ended: the one text the live view, the
 * reply on Dispatch and the agent's transcript all carry. A command that printed nothing says so,
 * so every `!` message has an answer.
 */
export function shellCommandReport(result: ShellCommandResult): string {
  const parts = [`!${result.command}`, result.output === "" ? NOTHING_PRINTED : result.output];
  if (result.cut) parts.push(`The output was cut at ${SHELL_OUTPUT_MAX} characters.`);
  if (result.status !== undefined) parts.push(result.status);
  return parts.join("\n\n");
}

/** The report as the agent reads it in its transcript, under the line that says whose command it
 *  was. */
export function shellCommandMessage(report: string): string {
  return `${SHELL_COMMAND_HEADING}\n\n${report}`;
}

/** The answer a `!` gets while this session still runs one: the line, and the command it waits
 *  on. One command runs for a session at a time, and a second is refused rather than queued. */
export function shellCommandBusyReport(command: string, running: string): string {
  return `!${command}\n\nThe command was not run: this session is still running !${running}.`;
}
