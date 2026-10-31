import { afterAll, describe, expect, test } from "bun:test";
import {
  existsSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  readlinkSync,
  rmSync,
  statSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// The box-login route's own installer, against a fake operator home in a temp directory: never the
// real ~/.omp. Its one outside call is `mise ls --global --json`, faked here by a `mise` first on
// PATH that prints the listing the test names and records the HOME and directory it ran under.
const lib = import.meta.dir;
const dir = mkdtempSync(join(tmpdir(), "install-box-login-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));
// The caller's HOME, which the installer refuses as --home; distinct from the fake operator home.
const callerHome = join(dir, "caller-home");
mkdirSync(callerHome);
const fixed = "18.12.2-sami.20261026-004924";
// Stand-in bytes for the operator's database: the installer must link to it and never change it.
const dbBytes = "not a database: a stand-in the installer must link to and never touch\n";

let installs = 0;
interface Setup {
  /** The version the fake operator's global mise config resolves github:sjawhar/oh-my-pi to. */
  version?: string;
  /** Whether the fake operator home holds .omp/agent/agent.db. */
  operatorDb?: boolean;
  /** Prepares the run's home before the installer runs. */
  prepare?: (agent: string, home: string, run: string) => void;
  /** The --profile value. */
  profile?: string;
}
function install({
  version = fixed,
  operatorDb = true,
  prepare,
  profile = "legion-e2e-test",
}: Setup = {}) {
  const run = join(dir, `install-${++installs}`);
  const home = join(run, "omp-home");
  mkdirSync(join(home, ".omp"), { recursive: true });
  const operatorHome = join(run, "operator-home");
  mkdirSync(join(operatorHome, ".omp", "agent"), { recursive: true });
  const db = join(operatorHome, ".omp", "agent", "agent.db");
  if (operatorDb) writeFileSync(db, dbBytes, { mode: 0o600 });
  const agent = join(home, ".omp", "profiles", "legion-e2e-test", "agent");
  prepare?.(agent, home, run);
  const bin = join(run, "bin");
  mkdirSync(bin);
  const listing = join(run, "listing.json");
  writeFileSync(
    listing,
    JSON.stringify({
      "github:sjawhar/oh-my-pi": [{ version, requested_version: version, active: true }],
      "npm:other": [{ version: "1.0.0", active: true }],
    })
  );
  const seen = join(run, "mise-saw");
  writeFileSync(
    join(bin, "mise"),
    `#!/usr/bin/env bash\nprintf 'args=%s\\nhome=%s\\npwd=%s\\n' "$*" "$HOME" "$PWD" >${JSON.stringify(seen)}\ncat ${JSON.stringify(listing)}\n`,
    { mode: 0o755 }
  );
  const dest = join(run, "model-gateway");
  const result = Bun.spawnSync(
    [
      "bash",
      join(lib, "install-box-login-route.sh"),
      "--profile",
      profile,
      "--home",
      home,
      "--dest",
      dest,
      "--operator-home",
      operatorHome,
    ],
    { env: { PATH: `${bin}:${process.env.PATH ?? ""}`, HOME: callerHome } }
  );
  return {
    agent,
    home,
    run,
    db,
    dest,
    operatorHome,
    seen,
    code: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
  };
}

describe("the box-login model route installer", () => {
  test("links the profile's agent.db to the operator's, and prints the operator's build", () => {
    const r = install();
    expect(r.stderr).toContain("routes anthropic/claude-opus-4-8");
    expect(r.code).toBe(0);
    expect(r.stdout).toBe(`github:sjawhar/oh-my-pi@${fixed}\n`);
    const link = join(r.agent, "agent.db");
    expect(lstatSync(link).isSymbolicLink()).toBe(true);
    expect(readlinkSync(link)).toBe(r.db);
    // The operator's database is linked to, never copied, read into anything or changed.
    expect(readFileSync(r.db, "utf8")).toBe(dbBytes);
    expect(readFileSync(join(r.dest, "box-login.route"), "utf8")).toBe(
      [
        "route\tbox-login: the operator's own Oh My Pi login, never a key",
        `link\t${link}`,
        `database\t${r.db}`,
        `omp\tgithub:sjawhar/oh-my-pi@${fixed}`,
        "",
      ].join("\n")
    );
  });

  test("reads the operator's global pin from / under the operator's home, so no project config answers", () => {
    const r = install();
    expect(r.code).toBe(0);
    expect(readFileSync(r.seen, "utf8")).toBe(
      `args=ls --global --json\nhome=${r.operatorHome}\npwd=/\n`
    );
  });

  test("config.yml pins anthropic, disables the other providers, turns memory off and overrides no key", () => {
    const r = install();
    expect(r.code).toBe(0);
    const config = readFileSync(join(r.agent, "config.yml"), "utf8");
    expect(config).toContain("enabledModels:\n  - anthropic/claude-opus-4-8\n");
    for (const provider of [
      "amazon-bedrock",
      "bedrock-mantle",
      "google",
      "ollama",
      "llama.cpp",
      "lm-studio",
    ]) {
      expect(config).toContain(`\n  - ${provider}\n`);
    }
    expect(config).toContain('memory:\n  backend: "off"\n');
    for (const role of ["default", "smol", "slow", "task", "review", "oracle", "deep"]) {
      expect(config).toContain(`\n  ${role}: anthropic/claude-opus-4-8\n`);
    }
    expect(config).not.toMatch(/apiKey|X-Api-Key|!exec/);
    // No models.yml: Anthropic keeps Oh My Pi's own endpoint and the stored login.
    expect(existsSync(join(r.agent, "models.yml"))).toBe(false);
  });

  test("refuses a box whose operator has no agent.db, and writes nothing", () => {
    const r = install({ operatorDb: false });
    expect(r.code).toBe(1);
    expect(r.stderr).toContain(
      "agent.db is not a regular file: the operator has no Oh My Pi login on this box to share"
    );
    expect(existsSync(r.agent)).toBe(false);
  });

  test("refuses a profile whose agent.db is already a regular file, and leaves it as it was", () => {
    const r = install({
      prepare: (agent) => {
        mkdirSync(agent, { recursive: true });
        writeFileSync(join(agent, "agent.db"), "the profile's own\n");
      },
    });
    expect(r.code).toBe(2);
    expect(r.stderr).toContain("agent.db exists and is not the symlink to");
    expect(readFileSync(join(r.agent, "agent.db"), "utf8")).toBe("the profile's own\n");
    expect(existsSync(join(r.agent, "config.yml"))).toBe(false);
  });

  test("refuses an operator build older than the refresh-race fix", () => {
    const r = install({ version: "18.12.2-sami.20261025-144204" });
    expect(r.code).toBe(1);
    expect(r.stderr).toContain(
      "is 18.12.2-sami.20261025-144204, older than 18.12.2-sami.20261026-004924, the first build with the refresh-race fix"
    );
    expect(existsSync(r.agent)).toBe(false);
  });

  test("takes a build stamped after the fix", () => {
    const r = install({ version: "18.12.2-sami.20261101-000000" });
    expect(r.code).toBe(0);
    expect(r.stdout).toBe("github:sjawhar/oh-my-pi@18.12.2-sami.20261101-000000\n");
  });

  test.each([
    ["an unparseable", "latest", "not <base>-sami.YYYYMMDD-HHMMSS"],
    ["another base's", "18.13.0-sami.20261101-000000", "base 18.13.0; only base 18.12.2 is known"],
  ])("refuses %s pin rather than guessing", (_, version, message) => {
    const r = install({ version });
    expect(r.code).toBe(1);
    expect(r.stderr).toContain(message);
    expect(existsSync(r.agent)).toBe(false);
  });

  test.each([
    "../../escape",
    "x/../../../escape",
    "nested/name",
    ".hidden",
    "a..b",
  ])("refuses the profile name %p, which is not one plain path segment, and writes nothing", (profile) => {
    const r = install({ profile });
    expect(r.code).toBe(2);
    expect(r.stderr).toContain(`--profile '${profile}' is not one plain name`);
    expect(existsSync(join(r.home, "escape"))).toBe(false);
    expect(existsSync(join(r.run, "escape"))).toBe(false);
    expect(existsSync(join(r.home, ".omp", "profiles"))).toBe(false);
  });

  test("refuses a profiles directory that is a symlink out of the run's home, and writes nothing there", () => {
    const r = install({
      prepare: (_agent, home, run) => {
        mkdirSync(join(run, "elsewhere"));
        symlinkSync(join(run, "elsewhere"), join(home, ".omp", "profiles"));
      },
    });
    expect(r.code).toBe(2);
    expect(r.stderr).toContain(
      `resolves to ${join(r.run, "elsewhere", "legion-e2e-test", "agent")}`
    );
    expect(readdirSync(join(r.run, "elsewhere"))).toEqual([]);
  });

  test.each([
    "--profile",
    "--home",
    "--dest",
    "--operator-home",
  ])("refuses %s with no value as an argument error (exit 2), not an unbound variable", (flag) => {
    const result = Bun.spawnSync(["bash", join(lib, "install-box-login-route.sh"), flag], {
      env: { PATH: process.env.PATH ?? "", HOME: callerHome },
    });
    expect(result.exitCode).toBe(2);
    expect(result.stderr.toString()).toContain(`${flag} needs a value`);
  });
});

// A shell with lib/box-login.sh sourced, the lock moved to FILE when one is given.
const helpers = (file: string | undefined, body: string) =>
  `. ${JSON.stringify(join(lib, "box-login.sh"))}\n${file === undefined ? "" : `box_login_lock_file=${JSON.stringify(file)}\n`}${body}`;

describe("the box-login lock", () => {
  test("is one fixed file per uid under /tmp, whatever XDG_RUNTIME_DIR or TMPDIR says", () => {
    const uid = Bun.spawnSync(["id", "-u"]).stdout.toString().trim();
    for (const env of [
      { XDG_RUNTIME_DIR: join(dir, "xdg-a"), TMPDIR: join(dir, "tmp-a") },
      { XDG_RUNTIME_DIR: join(dir, "xdg-b") },
      {},
    ]) {
      const r = Bun.spawnSync(["bash", "-c", helpers(undefined, 'echo "$box_login_lock_file"')], {
        env: { PATH: process.env.PATH ?? "", ...env },
      });
      expect(r.stdout.toString()).toBe(`/tmp/legion-e2e-box-login.${uid}.lock\n`);
    }
  });

  test("refuses a second holder under another XDG_RUNTIME_DIR, naming the first, and is free again once the first is gone", async () => {
    const file = join(dir, "test-box-login.lock");
    const holder = Bun.spawn(
      ["bash", "-c", helpers(file, "take_box_login_lock && echo locked && exec sleep 60")],
      { stdout: "pipe", env: { PATH: process.env.PATH ?? "", XDG_RUNTIME_DIR: join(dir, "xdg-a") } }
    );
    try {
      const first = await holder.stdout.getReader().read();
      expect(new TextDecoder().decode(first.value)).toBe("locked\n");
      const second = Bun.spawnSync(["bash", "-c", helpers(file, "take_box_login_lock")], {
        env: { PATH: process.env.PATH ?? "", XDG_RUNTIME_DIR: join(dir, "xdg-b") },
      });
      expect(second.exitCode).toBe(1);
      expect(second.stderr.toString()).toContain(
        `another box-login proof holds ${file}: pid ${holder.pid} (`
      );
      expect(second.stderr.toString()).toContain("one box-login proof runs at a time");
    } finally {
      holder.kill();
      await holder.exited;
    }
    const third = Bun.spawnSync([
      "bash",
      "-c",
      helpers(file, "take_box_login_lock && echo locked"),
    ]);
    expect(third.exitCode).toBe(0);
    expect(third.stdout.toString()).toBe("locked\n");
  });
});

// The stage proofs themselves, run only as far as their first lines: a refused run must leave no
// work or evidence directory. The lock is the real fixed file; the test holds it for a moment (or
// finds a real proof holding it, which refuses the stages the same way).
describe("a box-login stage proof refused at its first lines", () => {
  const e2e = join(lib, "..");
  const uid = Bun.spawnSync(["id", "-u"]).stdout.toString().trim();
  const runDirs = (pid: number) =>
    readdirSync("/tmp").filter(
      (name) => name.startsWith(`legion-e2e2.${pid}.`) || name.startsWith(`legion-e2e3.${pid}.`)
    );
  const stage = (script: string, env: Record<string, string>) => {
    const evidence = join(dir, `evidence-${script}-${Object.keys(env).join("-")}`);
    const child = Bun.spawnSync(["bash", join(e2e, script)], {
      env: {
        PATH: process.env.PATH ?? "",
        HOME: callerHome,
        LEGION_E2E_MODEL_ROUTE: "box-login",
        STAGE3_EVIDENCE_DIR: evidence,
        ...env,
      },
    });
    return {
      code: child.exitCode,
      stderr: child.stderr.toString(),
      left: [...runDirs(child.pid), ...(existsSync(evidence) ? [evidence] : [])],
    };
  };
  const scripts = ["stage2-tmux-supervision.sh", "stage3-devbox-workflow.sh"];

  test.each(scripts)("%s takes the lock before it makes any run state", async (script) => {
    const holder = Bun.spawn(
      [
        "bash",
        "-c",
        `exec 9>>/tmp/legion-e2e-box-login.${uid}.lock; flock -n 9 || true; echo ready; exec sleep 60`,
      ],
      { stdout: "pipe" }
    );
    try {
      await holder.stdout.getReader().read();
      const r = stage(script, {});
      expect(r.stderr).toContain(
        "FAIL setup: another box-login proof is running on this box (the holder is named above)"
      );
      expect(r.code).toBe(1);
      expect(r.left).toEqual([]);
    } finally {
      holder.kill();
      await holder.exited;
    }
  });

  test.each(scripts)("%s refuses LEGION_OMP_PATH under box-login, naming it", (script) => {
    const r = stage(script, { LEGION_OMP_PATH: "/opt/other/omp" });
    expect(r.stderr).toContain(
      "FAIL setup: LEGION_OMP_PATH is set (/opt/other/omp); under LEGION_E2E_MODEL_ROUTE=box-login"
    );
    expect(r.code).toBe(1);
    expect(r.left).toEqual([]);
  });
});

describe("a box-login run's teardown", () => {
  test("removes the profile's link and leaves the operator's database as it was", () => {
    const r = install();
    expect(r.code).toBe(0);
    const before = statSync(r.db);
    // What cleanup runs: the link's removal, then the workspace's (a passed run removes it whole).
    const teardown = Bun.spawnSync([
      "bash",
      "-c",
      helpers(
        undefined,
        `unlink_box_login_db ${JSON.stringify(r.agent)} && rm -rf ${JSON.stringify(r.home)}`
      ),
    ]);
    expect(teardown.exitCode).toBe(0);
    expect(existsSync(r.home)).toBe(false);
    const after = statSync(r.db);
    expect([after.ino, after.mode, after.size]).toEqual([before.ino, before.mode, before.size]);
    expect(readFileSync(r.db, "utf8")).toBe(dbBytes);
  });

  test("unlinks the link alone: a kept workspace keeps the rest of the profile", () => {
    const r = install();
    const link = join(r.agent, "agent.db");
    Bun.spawnSync([
      "bash",
      "-c",
      helpers(undefined, `unlink_box_login_db ${JSON.stringify(r.agent)}`),
    ]);
    expect(() => lstatSync(link)).toThrow();
    expect(existsSync(join(r.agent, "config.yml"))).toBe(true);
    expect(readFileSync(r.db, "utf8")).toBe(dbBytes);
  });

  test("gives each process SIGTERM and the grace to exit, then SIGKILLs only the survivors", async () => {
    const graceful = join(dir, "graceful-exited");
    const stubborn = join(dir, "stubborn-killed");
    // One exits on SIGTERM after a short write (a refresh finishing), one ignores SIGTERM. Each says
    // "ready" once its trap is set, and the stop starts only then.
    const a = Bun.spawn(
      [
        "bash",
        "-c",
        `trap 'sleep 0.3; touch ${JSON.stringify(graceful)}; exit 0' TERM; echo ready; while :; do sleep 0.1; done`,
      ],
      { stdout: "pipe" }
    );
    const b = Bun.spawn(
      [
        "bash",
        "-c",
        `trap '' TERM; echo ready; while :; do sleep 0.1; done; touch ${JSON.stringify(stubborn)}`,
      ],
      { stdout: "pipe" }
    );
    await a.stdout.getReader().read();
    await b.stdout.getReader().read();
    const stop = Bun.spawnSync([
      "bash",
      "-c",
      helpers(undefined, `box_login_stop_grace=1; stop_box_login_processes ${a.pid} ${b.pid}`),
    ]);
    expect(stop.exitCode).toBe(0);
    expect(await a.exited).toBe(0);
    expect(existsSync(graceful)).toBe(true);
    await b.exited;
    expect(b.signalCode).toBe("SIGKILL");
    expect(existsSync(stubborn)).toBe(false);
  });
});
