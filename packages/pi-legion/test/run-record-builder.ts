import { chmod, mkdir, readdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";

/** One invocation of the fake record builder: its process, its process group, and its argv after
 * the script (the command, then its arguments). */
export interface BuilderCall {
  readonly pid: number;
  readonly pgrp: number;
  readonly argv: readonly string[];
}

/** A stand-in for the deployment's record builder: `RUN_RECORD_PYTHON` is `/bin/sh` and
 * `RUN_RECORD_BUILDER` this script, which records each invocation and exits `exitCode`. */
export interface FakeBuilder {
  readonly interpreter: string;
  readonly script: string;
  /** The invocations once at least `count` have recorded themselves, oldest first; throws after
   * `timeoutMs` without them. */
  readonly calls: (count: number, timeoutMs?: number) => Promise<BuilderCall[]>;
  /** As `calls`, once every invocation recorded has also exited. */
  readonly settled: (count: number, timeoutMs?: number) => Promise<BuilderCall[]>;
  /** Whether the invocation `pid` has exited (its sleep, if any, over). */
  readonly finished: (pid: number) => Promise<boolean>;
}

/** Writes the fake builder into `directory`. With `sleepSeconds` it records itself first, then
 * sleeps before it exits, so a test can see the hook return while the builder still runs. */
export async function fakeRunRecordBuilder(
  directory: string,
  options: { readonly exitCode?: number; readonly sleepSeconds?: number } = {}
): Promise<FakeBuilder> {
  const callsDir = path.join(directory, "calls");
  await mkdir(callsDir, { recursive: true });
  const script = path.join(directory, "builder.sh");
  const sleep = options.sleepSeconds === undefined ? "" : `sleep ${options.sleepSeconds}\n`;
  await writeFile(
    script,
    [
      "#!/bin/sh",
      `dir='${callsDir}'`,
      'name="$(date +%s%N)-$$"',
      "{",
      '  echo "pid $$"',
      "  echo \"pgrp $(cut -d ' ' -f 5 /proc/$$/stat)\"",
      '  for arg in "$@"; do echo "arg $arg"; done',
      '} > "$dir/$name.tmp"',
      'mv "$dir/$name.tmp" "$dir/$name.call"',
      `${sleep}touch "$dir/$$.done"`,
      `exit ${options.exitCode ?? 0}`,
      "",
    ].join("\n")
  );
  await chmod(script, 0o755);

  const readCalls = async (): Promise<BuilderCall[]> => {
    const names = (await readdir(callsDir)).filter((name) => name.endsWith(".call"));
    // Each name starts with its invocation's start time in nanoseconds.
    const started = (name: string): bigint => BigInt(name.split("-")[0] ?? "0");
    names.sort((a, b) => (started(a) < started(b) ? -1 : 1));
    return Promise.all(
      names.map(async (name) => {
        const lines = (await readFile(path.join(callsDir, name), "utf8")).split("\n");
        const field = (prefix: string): string =>
          lines.find((line) => line.startsWith(`${prefix} `))?.slice(prefix.length + 1) ?? "";
        return {
          pid: Number(field("pid")),
          pgrp: Number(field("pgrp").trim()),
          argv: lines.filter((line) => line.startsWith("arg ")).map((line) => line.slice(4)),
        };
      })
    );
  };

  // The builder is a separate process the hook never waits for, so nothing in this process
  // signals its progress: its files are polled until a deadline.
  const poll = async (
    count: number,
    timeoutMs: number,
    ready: (calls: BuilderCall[], done: readonly string[]) => boolean
  ): Promise<BuilderCall[]> => {
    const deadline = Date.now() + timeoutMs;
    for (;;) {
      const calls = await readCalls();
      const done = (await readdir(callsDir)).filter((name) => name.endsWith(".done"));
      if (calls.length >= count && ready(calls, done)) return calls;
      if (Date.now() > deadline) {
        throw new Error(
          `the fake builder recorded ${calls.length} calls (${done.length} exited), not ${count}`
        );
      }
      await Bun.sleep(20);
    }
  };

  return {
    interpreter: "/bin/sh",
    script,
    calls: (count, timeoutMs = 4_000) => poll(count, timeoutMs, () => true),
    settled: (count, timeoutMs = 4_000) =>
      poll(count, timeoutMs, (calls, done) =>
        calls.every((call) => done.includes(`${call.pid}.done`))
      ),
    finished: async (pid) => (await readdir(callsDir)).includes(`${pid}.done`),
  };
}

/** The value after `flag` in a builder argv, or undefined when the flag is absent. */
export function flagValue(argv: readonly string[], flag: string): string | undefined {
  const index = argv.indexOf(flag);
  return index === -1 ? undefined : argv[index + 1];
}
