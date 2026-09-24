import { execFile } from "node:child_process";
import { HANDOFF_PHASES } from "@legion/contracts";
import { messageFor } from "@legion/envoy-client/errors";
import type { PiZod, ToolResult } from "../pi-types";
import { requiredEnvironment } from "./classify";
import { writeGrantFile } from "./grant-file";

/**
 * The handoff actions of the `legion` tool: a worker's `legion handoff write|read|message|complete`
 * as tool calls rather than shell text, so the extension knows each one's outcome without reading
 * a bash command (LEGION-208 Stage 4b, task 4b.15). Each action runs the same command underneath —
 * `legion` found on the pane's PATH, which is the daemon's own CLI: the `<state_dir>/bin/legion`
 * launcher on tmux, the image's binary in a pod — in the issue workspace, so both daemons' CLIs
 * keep their contracts and neither daemon changes. `legion gh` and `legion credential` stay shell
 * commands: git and gh call them.
 */

/** Each handoff action and the fields it accepts, beside `op`. */
export const HANDOFF_OPERATION_FIELDS = {
  handoff_write: ["phase", "data"],
  handoff_read: ["phase"],
  handoff_message: ["sender", "recipient", "body"],
  handoff_complete: ["summary", "verdict", "ready"],
} as const satisfies Readonly<Record<string, readonly string[]>>;

export type HandoffOperation = keyof typeof HANDOFF_OPERATION_FIELDS;

export const HANDOFF_OPERATIONS = Object.keys(HANDOFF_OPERATION_FIELDS) as HandoffOperation[];

export function isHandoffOperation(operation: string): operation is HandoffOperation {
  return Object.hasOwn(HANDOFF_OPERATION_FIELDS, operation);
}

/** The tool-schema fields the handoff actions add, built from the host's zod. */
export function handoffSchemaFields(z: PiZod): Readonly<Record<string, unknown>> {
  return {
    phase: z.enum(HANDOFF_PHASES).optional(),
    data: z.unknown().optional(),
    sender: z.enum(HANDOFF_PHASES).optional(),
    recipient: z.enum(HANDOFF_PHASES).optional(),
    body: z.string().optional(),
    summary: z.string().optional(),
    verdict: z.enum(["pass", "fail"]).optional(),
    ready: z.boolean().optional(),
  };
}

/** The handoff actions' part of the tool description. */
export const HANDOFF_DESCRIPTION =
  "Handoff actions (phase workers and sub-architects): handoff_write writes this phase's handoff " +
  "(`phase`, and `data`: the phase-specific fields as a JSON object) to `.legion/<phase>.json` in " +
  "the issue workspace; handoff_read returns the handoffs (every phase, or `phase`); " +
  "handoff_message leaves a message for another phase (`sender`, `recipient`, `body`); " +
  "handoff_complete reports this phase complete to the daemon (`summary`: two sentences for the " +
  "architect; `verdict` pass|fail when your role's instructions require one; `ready: true` for " +
  "the merger's READY). Each returns the command's output; a failed action changed nothing.";

/** The bash tool's default timeout, which bounded these commands when they ran through bash. */
const COMMAND_TIMEOUT_MS = 300_000;

function required(parameters: Record<string, unknown>, operation: string, name: string): string {
  const value = parameters[name];
  if (typeof value !== "string" || value.trim() === "") {
    throw new Error(`${operation} requires ${name}`);
  }
  return value;
}

/** The `legion` arguments for one action, its fields validated. */
function commandArguments(
  operation: HandoffOperation,
  parameters: Record<string, unknown>
): string[] {
  const accepted: readonly string[] = HANDOFF_OPERATION_FIELDS[operation];
  for (const name of Object.keys(parameters)) {
    if (name !== "op" && !accepted.includes(name)) {
      throw new Error(`${operation} does not accept field "${name}"`);
    }
  }
  switch (operation) {
    case "handoff_write": {
      const data = parameters.data;
      if (typeof data !== "object" || data === null || Array.isArray(data)) {
        throw new Error("handoff_write requires data: the phase's handoff fields as a JSON object");
      }
      const phase = required(parameters, operation, "phase");
      return ["handoff", "write", "--phase", phase, "--data", JSON.stringify(data)];
    }
    case "handoff_read":
      return parameters.phase === undefined
        ? ["handoff", "read"]
        : ["handoff", "read", "--phase", required(parameters, operation, "phase")];
    case "handoff_message":
      return [
        "handoff",
        "message",
        "--from",
        required(parameters, operation, "sender"),
        "--to",
        required(parameters, operation, "recipient"),
        "--body",
        required(parameters, operation, "body"),
      ];
    case "handoff_complete": {
      const command = ["handoff", "complete", "--summary", required(parameters, operation, "summary")];
      const { verdict, ready } = parameters;
      if (verdict !== undefined) {
        if (verdict !== "pass" && verdict !== "fail") {
          throw new Error("handoff_complete's verdict is pass or fail");
        }
        command.push("--verdict", verdict);
      }
      if (ready !== undefined && typeof ready !== "boolean") {
        throw new Error("handoff_complete's ready is true or false");
      }
      if (ready === true) command.push("--ready");
      return command;
    }
  }
}

/** Runs `legion <args>` in `cwd`; its exit code and combined output, or the reason it never ran
 * to an exit (not found, timed out, aborted). */
function runLegion(
  args: readonly string[],
  cwd: string,
  signal: AbortSignal | undefined
): Promise<{ readonly exitCode: number; readonly output: string }> {
  const { promise, resolve, reject } = Promise.withResolvers<{
    readonly exitCode: number;
    readonly output: string;
  }>();
  execFile(
    "legion",
    args,
    { cwd, env: process.env, signal, timeout: COMMAND_TIMEOUT_MS, maxBuffer: 16 * 1024 * 1024 },
    (error, stdout, stderr) => {
      const output = [stdout, stderr].filter((text) => text.trim() !== "").join("\n");
      if (error === null) {
        resolve({ exitCode: 0, output });
      } else if (typeof error.code === "number") {
        resolve({ exitCode: error.code, output });
      } else {
        reject(new Error(`legion ${args.slice(0, 2).join(" ")} did not run: ${messageFor(error)}`));
      }
    }
  );
  return promise;
}

/**
 * Runs one handoff action. `handoff_complete` redeems a grant: `mintGrant` mints a fresh one,
 * which is written to the pane's `LEGION_GRANT_FILE` exactly as the bash hook writes one before a
 * shell command, and the command reads it there. The result carries the command's output and its
 * exit code (`details.exitCode`); a non-zero exit is an error result.
 */
export async function runHandoffAction(input: {
  readonly operation: HandoffOperation;
  readonly parameters: Record<string, unknown>;
  readonly signal: AbortSignal | undefined;
  readonly mintGrant: () => Promise<string>;
}): Promise<ToolResult> {
  const args = commandArguments(input.operation, input.parameters);
  if (input.operation === "handoff_complete") {
    const grantFile = process.env.LEGION_GRANT_FILE;
    if (grantFile === undefined || grantFile.trim() === "") {
      throw new Error(
        "LEGION_GRANT_FILE is not set on this pane: the daemon that launched it predates this plugin; restart the daemon on the matching release"
      );
    }
    await writeGrantFile(grantFile, await input.mintGrant());
  }
  const { exitCode, output } = await runLegion(
    args,
    requiredEnvironment(process.env, "LEGION_WORKSPACE"),
    input.signal
  );
  const text = output === "" ? `legion ${args.slice(0, 2).join(" ")} exited ${exitCode}` : output;
  const details = { operation: input.operation, exitCode };
  return exitCode === 0
    ? { content: [{ type: "text", text }], details }
    : { content: [{ type: "text", text }], details, isError: true };
}
