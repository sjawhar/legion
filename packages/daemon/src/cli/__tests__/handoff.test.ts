import { afterEach, beforeEach, describe, expect, it, mock } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { handoffCommand } from "../index";

interface RunnableCommand {
  run?: (context: {
    args: Record<string, unknown>;
    rawArgs: Record<string, unknown>;
    cmd: unknown;
  }) => Promise<unknown> | unknown;
  subCommands?: Record<string, unknown>;
  args?: unknown;
}

interface StringArg {
  type: string;
  required?: boolean;
}

function createTempDir(): string {
  return fs.mkdtempSync(path.join(os.tmpdir(), "legion-cli-handoff-"));
}

function getSubCommand(command: unknown, name: string): RunnableCommand {
  const subCommands = (command as RunnableCommand).subCommands;
  if (!subCommands || !(name in subCommands)) {
    throw new Error(`Subcommand not found: ${name}`);
  }

  return subCommands[name] as RunnableCommand;
}

async function runCommand(command: unknown, args: Record<string, unknown>): Promise<void> {
  const run = (command as RunnableCommand).run;
  if (!run) {
    throw new Error("Command has no run handler");
  }

  const parsedArgs = { _: [], ...args };
  await run({ args: parsedArgs, rawArgs: parsedArgs, cmd: command });
}

async function resolveArgs(command: RunnableCommand): Promise<Record<string, unknown>> {
  if (!command.args) {
    throw new Error("Command has no args");
  }

  if (typeof command.args === "function") {
    return (await command.args()) as Record<string, unknown>;
  }

  return command.args as Record<string, unknown>;
}

describe("handoff command", () => {
  const originalCwd = process.cwd();
  const originalExit = process.exit;
  const originalLog = console.log;
  const originalError = console.error;
  let tempDir = "";
  let exitCode: number | undefined;

  beforeEach(() => {
    tempDir = createTempDir();
    process.chdir(tempDir);
    exitCode = undefined;
    process.exit = mock((code?: number) => {
      exitCode = code;
      return undefined as never;
    }) as typeof process.exit;
    console.log = mock(() => {}) as typeof console.log;
    console.error = mock(() => {}) as typeof console.error;
  });

  afterEach(() => {
    process.chdir(originalCwd);
    fs.rmSync(tempDir, { recursive: true, force: true });
    process.exit = originalExit;
    console.log = originalLog;
    console.error = originalError;
  });

  it("defines write/read/message subcommands and required args", async () => {
    const write = getSubCommand(handoffCommand, "write");
    const read = getSubCommand(handoffCommand, "read");
    const message = getSubCommand(handoffCommand, "message");

    const writeArgs = await resolveArgs(write);
    const readArgs = await resolveArgs(read);
    const messageArgs = await resolveArgs(message);

    expect((writeArgs.phase as StringArg).type).toBe("string");
    expect((writeArgs.phase as StringArg).required).toBe(true);
    expect((writeArgs.data as StringArg).type).toBe("string");
    expect((writeArgs.data as StringArg).required).toBe(true);

    expect((readArgs.phase as StringArg).type).toBe("string");
    expect((readArgs.phase as StringArg).required).toBeFalsy();

    expect((messageArgs.from as StringArg).type).toBe("string");
    expect((messageArgs.from as StringArg).required).toBe(true);
    expect((messageArgs.to as StringArg).type).toBe("string");
    expect((messageArgs.to as StringArg).required).toBe(true);
    expect((messageArgs.body as StringArg).type).toBe("string");
    expect((messageArgs.body as StringArg).required).toBe(true);
  });

  it("writes phase handoff JSON with auto-populated fields", async () => {
    const write = getSubCommand(handoffCommand, "write");
    await runCommand(write, { phase: "plan", data: '{"taskCount":5}' });

    const handoffPath = path.join(tempDir, ".legion", "plan.json");
    expect(fs.existsSync(handoffPath)).toBe(true);

    const payload = JSON.parse(fs.readFileSync(handoffPath, "utf-8")) as Record<string, unknown>;
    expect(payload.taskCount).toBe(5);
    expect(payload.schemaVersion).toBe(1);
    expect(payload.phase).toBe("plan");
    expect(typeof payload.completed).toBe("string");
    expect(exitCode).toBeUndefined();
  });

  it("reads a single phase handoff and prints JSON", async () => {
    const write = getSubCommand(handoffCommand, "write");
    const read = getSubCommand(handoffCommand, "read");

    await runCommand(write, { phase: "plan", data: '{"taskCount":5}' });
    await runCommand(read, { phase: "plan" });

    const calls = (console.log as ReturnType<typeof mock>).mock.calls;
    const output = calls[calls.length - 1]?.[0] as string;
    const parsed = JSON.parse(output) as Record<string, unknown>;

    expect(parsed.phase).toBe("plan");
    expect(parsed.taskCount).toBe(5);
  });

  it("reads all phases and prints a JSON object", async () => {
    const write = getSubCommand(handoffCommand, "write");
    const read = getSubCommand(handoffCommand, "read");

    await runCommand(write, { phase: "plan", data: '{"taskCount":5}' });
    await runCommand(write, { phase: "implement", data: '{"filesChanged":["a.ts"]}' });
    await runCommand(read, {});

    const calls = (console.log as ReturnType<typeof mock>).mock.calls;
    const output = calls[calls.length - 1]?.[0] as string;
    const parsed = JSON.parse(output) as Record<string, Record<string, unknown>>;

    expect(parsed.plan.phase).toBe("plan");
    expect(parsed.implement.phase).toBe("implement");
  });

  it("writes handoff messages", async () => {
    const message = getSubCommand(handoffCommand, "message");
    await runCommand(message, { from: "plan", to: "implement", body: "test" });

    const messagesDir = path.join(tempDir, ".legion", "messages");
    const files = fs.readdirSync(messagesDir);
    expect(files.length).toBe(1);
    expect(files[0]).toContain("-plan-to-implement.json");

    const payload = JSON.parse(
      fs.readFileSync(path.join(messagesDir, files[0] as string), "utf-8")
    ) as Record<string, unknown>;
    expect(payload.from).toBe("plan");
    expect(payload.to).toBe("implement");
    expect(payload.body).toBe("test");
    expect(typeof payload.timestamp).toBe("string");
  });

  it("exits non-zero for invalid phase", async () => {
    const write = getSubCommand(handoffCommand, "write");
    try {
      await runCommand(write, { phase: "invalid", data: "{}" });
    } catch {}

    expect(exitCode).toBe(1);
    const errors = (console.error as ReturnType<typeof mock>).mock.calls.flat();
    expect(errors.join("\n")).toContain("Invalid phase");
  });

  it("exits non-zero for invalid JSON data", async () => {
    const write = getSubCommand(handoffCommand, "write");
    try {
      await runCommand(write, { phase: "plan", data: "{" });
    } catch {}

    expect(exitCode).toBe(1);
    const errors = (console.error as ReturnType<typeof mock>).mock.calls.flat();
    expect(errors.join("\n")).toContain("Invalid JSON");
  });
});
