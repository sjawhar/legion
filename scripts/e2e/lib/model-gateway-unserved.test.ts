import { afterAll, describe, expect, test } from "bun:test";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// model-gateway-unserved.sh over the call record the key-file route's key command writes: a call
// that finds its key file empty or gone is the route's one way to leave an agent without a key.
const lib = import.meta.dir;
const dir = mkdtempSync(join(tmpdir(), "model-gateway-unserved-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));
const operatorHome = join(dir, "operator-home");
mkdirSync(operatorHome);

let installs = 0;
// install writes a fresh profile's route over a fresh key file and returns where its record goes.
function install(keyContents = "sk-ant-test-key-00000000000000000000000") {
  const run = join(dir, `install-${++installs}`);
  const home = join(run, "omp-home");
  mkdirSync(join(home, ".omp"), { recursive: true });
  const dest = join(run, "model-gateway");
  const keyFile = join(run, "anthropic-key-file");
  writeFileSync(keyFile, `${keyContents}\n`, { mode: 0o600 });
  const result = Bun.spawnSync(
    [
      "bash",
      join(lib, "install-key-file-model-route.sh"),
      "--profile",
      "legion-e2e-test",
      "--home",
      home,
      "--dest",
      dest,
      "--key-dir",
      join(run, "model-gateway-key"),
      "--key-file",
      keyFile,
    ],
    { env: { PATH: process.env.PATH ?? "", HOME: operatorHome } }
  );
  return { run, dest, keyFile, result, keyCommand: result.stdout.toString().trim() };
}

interface Caller {
  // A Legion pane's identity, as the Go tmux runtime sets it on the pane's environment.
  role?: string;
  generation?: string;
}
// call runs the key command as Oh My Pi does, in a pane's own working directory.
async function call(keyCommand: string, cwd: string, caller: Caller = {}) {
  mkdirSync(cwd, { recursive: true });
  const child = Bun.spawn(["/bin/sh", "-c", `exec ${keyCommand}`], {
    cwd,
    env: {
      PATH: process.env.PATH ?? "",
      ...(caller.role === undefined ? {} : { LEGION_ROLE: caller.role }),
      ...(caller.generation === undefined ? {} : { LEGION_GENERATION: caller.generation }),
    },
    stdout: "pipe",
    stderr: "pipe",
  });
  const [code, stdout] = await Promise.all([child.exited, new Response(child.stdout).text()]);
  return { code, stdout: stdout.trim() };
}

function unserved(...args: string[]) {
  const result = Bun.spawnSync(["bash", join(lib, "model-gateway-unserved.sh"), ...args], {
    env: { PATH: process.env.PATH ?? "" },
  });
  return {
    code: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
  };
}

describe("the model route's call record", () => {
  test("tells apart the agents of one issue, which share its workspace, by their Legion pane", async () => {
    const { run, dest, keyFile, keyCommand } = install();
    // The record's own time format, UTC to the second.
    const since = new Date().toISOString().replace(/\.\d+Z$/, "Z");
    const workspace = join(run, "issue-workspace");
    // The tester's call finds the key file empty; the architect's in the same directory, after the
    // operator writes the key back, is served.
    writeFileSync(keyFile, "");
    expect(await call(keyCommand, workspace, { role: "tester", generation: "1" })).toEqual({
      code: 1,
      stdout: "",
    });
    writeFileSync(keyFile, "sk-ant-test-key-again\n");
    expect((await call(keyCommand, workspace, { role: "architect", generation: "2" })).code).toBe(
      0
    );

    const notes = unserved("--notes", dest, since, "tester-handoff");
    expect(notes.code).toBe(0);
    const listed = notes.stdout.split("\n").filter((line) => line.startsWith("  "));
    expect(listed).toHaveLength(1);
    expect(listed[0]).toContain(` tester/1 in ${workspace} (pid `);
    expect(listed[0]).toContain(`: failed: empty ${keyFile}`);
    expect(listed[0]).not.toContain("served again");
    const record = readFileSync(join(dest, "anthropic-key.calls"), "utf8");
    expect(record).toContain("\ttester/1\tfailed\t");
    expect(record).toContain("\tarchitect/2\tserved\t");

    // `legion controller start` sets LEGION_ROLE=controller and no generation; a harness sets
    // neither.
    await call(keyCommand, join(run, "controller"), { role: "controller" });
    await call(keyCommand, join(run, "harness"));
    const fields = readFileSync(join(dest, "anthropic-key.calls"), "utf8")
      .split("\n")
      .filter(Boolean)
      .map((l) => l.split("\t"));
    expect(fields.every((f) => f.length === 6 && f.every(Boolean))).toBe(true);
    expect(fields.at(-2)?.[3]).toBe("controller");
    expect(fields.at(-1)?.[3]).toBe("-");
  });

  test("lists for a failed check each agent still without a key, and each that recovered from a failure within 30 s of the check's start", () => {
    // The rules are the reader's over the record's own lines, so this writes them at chosen times.
    const dest = join(dir, "notes-window");
    mkdirSync(dest);
    const since = "2026-09-30T12:00:00Z";
    const line = (at: string, agent: string, outcome: string) =>
      `${at}\t42\t/ws\t${agent}\t${outcome}\t${outcome === "served" ? "read from /k" : "why"}\n`;
    writeFileSync(
      join(dest, "anthropic-key.calls"),
      [
        line("2026-09-30T11:50:00Z", "implementer/1", "failed"), // still without, long before
        line("2026-09-30T11:50:00Z", "reviewer/1", "failed"),
        line("2026-09-30T11:50:10Z", "reviewer/1", "served"), // recovered before the window
        line("2026-09-30T11:59:50Z", "planner/1", "failed"), // inside the 30 s window
        line("2026-09-30T12:00:02Z", "planner/1", "served"),
        line("2026-09-30T11:59:50Z", "merger/1", "failed"),
        line("2026-09-30T12:00:02Z", "merger/1", "served"),
        // A second failure after that service: its own first service is the one that counts.
        line("2026-09-30T12:00:10Z", "merger/1", "failed"),
        line("2026-09-30T12:00:15Z", "merger/1", "served"),
        line("2026-09-30T12:00:20Z", "merger/1", "served"),
        line("2026-09-30T12:00:05Z", "tester/1", "failed"), // still without, during the check
        line("2026-09-30T12:00:06Z", "architect/1", "served"),
      ].join("")
    );

    const notes = unserved("--notes", dest, since, "merge");
    expect(notes.code).toBe(0);
    expect(notes.stdout).toStartWith(
      "model-gateway-unserved: 4 agent(s) may have failed check merge for want of a model key: 2 still without one on their last call, and 2 served again after a failure from 2026-09-30T11:59:30Z on"
    );
    const listed = notes.stdout.split("\n").filter((l) => l.startsWith("  "));
    expect(listed).toEqual([
      "  2026-09-30T11:50:00Z implementer/1 in /ws (pid 42): failed: why (before check merge began)",
      "  2026-09-30T11:59:50Z planner/1 in /ws (pid 42): failed: why (before check merge began); served again at 2026-09-30T12:00:02Z",
      "  2026-09-30T12:00:05Z tester/1 in /ws (pid 42): failed: why",
      "  2026-09-30T12:00:10Z merger/1 in /ws (pid 42): failed: why; served again at 2026-09-30T12:00:15Z",
    ]);
    // No record yet, a malformed time, a line of the old five-field shape, and an outcome the key
    // command does not write.
    expect(unserved("--notes", join(dir, "not-installed"), since, "setup")).toMatchObject({
      code: 0,
      stdout: "",
    });
    expect(unserved("--notes", dest, "yesterday", "setup").code).toBe(2);
    writeFileSync(
      join(dest, "anthropic-key.calls"),
      "2026-09-30T12:00:05Z\t42\t/ws\tfailed\twhy\n"
    );
    expect(unserved("--notes", dest, since, "merge").code).toBe(1);
    writeFileSync(
      join(dest, "anthropic-key.calls"),
      line("2026-09-30T12:00:05Z", "tester/1", "timeout")
    );
    expect(unserved("--notes", dest, since, "merge").code).toBe(1);
  });

  test("serves the key with every whitespace character dropped, and records a key file gone or blank as failed", async () => {
    const { run, dest, keyFile, keyCommand } = install();
    writeFileSync(keyFile, " sk-ant-x \r\n");
    const raw = Bun.spawnSync(["/bin/sh", "-c", `exec ${keyCommand}`], {
      cwd: run,
      env: { PATH: process.env.PATH ?? "" },
    });
    expect([raw.exitCode, raw.stdout.toString()]).toEqual([0, "sk-ant-x\n"]);
    writeFileSync(keyFile, "\r\n");
    expect(
      (await call(keyCommand, join(run, "blank"), { role: "tester", generation: "1" })).code
    ).toBe(1);
    rmSync(keyFile);
    expect(
      (await call(keyCommand, join(run, "gone"), { role: "tester", generation: "2" })).code
    ).toBe(1);
    const record = readFileSync(join(dest, "anthropic-key.calls"), "utf8");
    expect(record).toContain(`\ttester/1\tfailed\tempty ${keyFile}\n`);
    expect(record).toContain(`\ttester/2\tfailed\tcould not read ${keyFile}\n`);
  });

  test("keeps the installer's preflight out of the record, and says why it got no key once", () => {
    const { run, dest, keyFile, result } = install("");
    expect(result.exitCode).toBe(1);
    expect(result.stderr.toString()).toContain(
      `the preflight got no key from --key-file ${keyFile}: anthropic-key key command: no key (failed): empty ${keyFile}`
    );
    // The preflight is no agent: a failed run's notes do not count it, nor repeat its reason.
    expect(existsSync(join(dest, "anthropic-key.calls"))).toBe(false);
    expect(existsSync(join(run, "model-gateway-key", "anthropic-key.preflight"))).toBe(false);
  });

  test("refuses any stage proof's evidence directory that is not empty, whether or not it reached its key command", () => {
    const evidence = join(dir, "evidence");
    expect(unserved("--fresh", evidence)).toMatchObject({ code: 0, stdout: "" });
    mkdirSync(evidence);
    expect(unserved("--fresh", evidence)).toMatchObject({ code: 0, stdout: "" });
    // An earlier Stage 4b run that stopped before its controller checkpoint left only these.
    writeFileSync(join(evidence, "transcript.log"), "== setup\n");
    const stopped = unserved("--fresh", evidence);
    expect(stopped.code).toBe(1);
    expect(stopped.stdout).toContain(`${evidence} is not an empty directory`);
    mkdirSync(join(evidence, "model-gateway"));
    expect(unserved("--fresh", evidence).code).toBe(1);
    // One it cannot list is refused too, never read as empty. At mode 0300 (write and search, no
    // read) a run it let through would append to the earlier run's transcript.log; at 000 it would
    // die at its first mkdir. Both need a non-root runner: root lists any directory.
    for (const mode of [0o300, 0o000]) {
      chmodSync(evidence, mode);
      try {
        const unlistable = unserved("--fresh", evidence);
        expect(unlistable.code).toBe(1);
        expect(unlistable.stdout).toContain(`${evidence} cannot be listed`);
        // ls's own error is the reason; the environment carries no LANG, so ls prints the C
        // locale's.
        expect(unlistable.stdout).toContain("Permission denied");
      } finally {
        chmodSync(evidence, 0o755);
      }
    }
  });

  test("a stage proof that refuses a reused evidence directory lists none of the earlier run's agents", async () => {
    // The earlier run's evidence directory: its key command, and a record whose last call failed.
    const { run: evidence, keyFile, keyCommand } = install();
    writeFileSync(keyFile, "");
    await call(keyCommand, join(evidence, "issue-workspace"), { role: "tester", generation: "1" });
    // Stage 3's teardown removes its containers; this run starts none, and needs no Docker.
    const stageBin = join(dir, "stage-bin");
    mkdirSync(stageBin, { recursive: true });
    writeFileSync(join(stageBin, "docker"), "#!/bin/sh\nexit 0\n", { mode: 0o755 });
    const stages: [string, Record<string, string>][] = [
      ["stage3-devbox-workflow.sh", { STAGE3_EVIDENCE_DIR: evidence }],
      [
        "stage3-4b13b-acceptance.sh",
        {
          ACCEPT_EVIDENCE_DIR: evidence,
          ACCEPT_PG_CONTAINER: "none",
          ACCEPT_PG_PORT: "1",
          ACCEPT_NATS_BIN: "/bin/false",
        },
      ],
    ];
    for (const [script, inputs] of stages) {
      const result = Bun.spawnSync(["bash", join(lib, "..", script)], {
        env: { PATH: `${stageBin}:${process.env.PATH}`, HOME: operatorHome, ...inputs },
      });
      const stderr = result.stderr.toString();
      const scratch = /the run's scratch workspace(?:, kept for review,)? is (\S+)/.exec(
        stderr
      )?.[1];
      if (scratch !== undefined) rmSync(scratch, { recursive: true, force: true });
      expect(result.exitCode).toBe(1);
      expect(stderr).toContain(`FAIL setup: ${evidence} is not an empty directory`);
      // The earlier run's tester could not have failed this run's setup, which never started one.
      expect(`${result.stdout}${stderr}`).not.toContain("may have failed check");
    }
  }, 120_000);
});
