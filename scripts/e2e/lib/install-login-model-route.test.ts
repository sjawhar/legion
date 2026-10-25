import { Database } from "bun:sqlite";
import { afterAll, describe, expect, test } from "bun:test";
import {
  existsSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readlinkSync,
  rmSync,
  statSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { SyncSubprocess } from "bun";

// The login route's installer, against two fakes first on PATH, written per run into the run's own
// bin directory, which reads its instructions from files beside it: the preflight runs in a pane's
// environment, so no variable of the test's reaches the fake. `omp` is the operator's own Oh My Pi
// and prints `operator-version`. `mise` stands in for `mise x <spec> -- omp …`: `--version` prints
// `pin-version`; `-p` records its argv, working directory and environment, writes the main session
// Oh My Pi would write under the profile (`session.jsonl`) and, when `sidecar.jsonl` exists, those
// lines a level deeper as a sidecar's session, then exits with `exit`.
const lib = import.meta.dir;
const dir = mkdtempSync(join(tmpdir(), "install-login-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));
const fakeOmp = `#!/bin/sh
echo "omp/$(cat "$(dirname "$0")/operator-version")"
`;
const fakeMise = `#!/usr/bin/env bash
set -eu
here=$(dirname "$0")
[ "$1" = x ] && [ "$3" = -- ] && [ "$4" = omp ] || { echo "fake mise: unexpected $*" >&2; exit 64; }
if [ "$5" = --version ]; then echo "omp/$(cat "$here/pin-version")"; exit 0; fi
printf '%s\\n' "$@" >"$here/ran.argv"
pwd >"$here/ran.cwd"
env >"$here/ran.env"
sessions="$HOME/.omp/profiles/$OMP_PROFILE/agent/sessions/--cwd--"
mkdir -p "$sessions/main-session"
cp "$here/session.jsonl" "$sessions/main-session.jsonl"
[ ! -e "$here/sidecar.jsonl" ] || cp "$here/sidecar.jsonl" "$sessions/main-session/__advisor.jsonl"
echo OK
exit "$(cat "$here/exit")"
`;

// Fake is what the fakes do in one run.
interface Fake {
  operatorVersion?: string;
  pinVersion?: string;
  session?: string[];
  sidecar?: string[];
  exit?: number;
}

type Login = { provider: string; type?: string; disabled?: boolean; minutesLeft?: number };

// operatorDb writes an Oh My Pi agent.db holding the given logins.
function operatorDb(path: string, logins: Login[]) {
  mkdirSync(join(path, ".."), { recursive: true });
  const db = new Database(path, { create: true });
  db.run(
    "CREATE TABLE auth_credentials (provider TEXT, credential_type TEXT, data TEXT, disabled_cause TEXT)"
  );
  for (const login of logins) {
    const expires = Date.now() + (login.minutesLeft ?? 480) * 60_000;
    db.run("INSERT INTO auth_credentials VALUES (?, ?, ?, ?)", [
      login.provider,
      login.type ?? "oauth",
      JSON.stringify({ access: "fixture-access", refresh: "fixture-refresh", expires }),
      login.disabled ? "revoked" : null,
    ]);
  }
  db.close();
}

const anthropicOnly: Login[] = [{ provider: "anthropic" }];
const answer = (provider: string, model: string, stopReason = "stop") =>
  JSON.stringify({ type: "message", message: { role: "assistant", provider, model, stopReason } });

function writeFakes(bin: string, fake: Fake) {
  mkdirSync(bin, { recursive: true });
  writeFileSync(join(bin, "omp"), fakeOmp, { mode: 0o755 });
  writeFileSync(join(bin, "mise"), fakeMise, { mode: 0o755 });
  writeFileSync(join(bin, "operator-version"), fake.operatorVersion ?? "18.12.2");
  writeFileSync(join(bin, "pin-version"), fake.pinVersion ?? "18.12.2");
  const session = fake.session ?? [answer("anthropic", "claude-opus-4-8")];
  writeFileSync(join(bin, "session.jsonl"), `${session.join("\n")}\n`);
  if (fake.sidecar) writeFileSync(join(bin, "sidecar.jsonl"), `${fake.sidecar.join("\n")}\n`);
  writeFileSync(join(bin, "exit"), String(fake.exit ?? 0));
}

// InstallRun is one run of the installer: the run's directories, the process result, its stderr, and
// whether the fake `omp -p` ran.
interface InstallRun {
  operatorHome: string;
  home: string;
  agent: string;
  dest: string;
  fakeRun: string;
  result: SyncSubprocess<"pipe", "pipe">;
  stderr: string;
  ran: boolean;
}

let runs = 0;
function install(
  opts: {
    dbs?: Record<string, Login[]>; // path under the operator's home → its logins
    env?: Record<string, string>;
    fake?: Fake;
    args?: string[];
    profile?: string;
    prepare?: (agent: string, run: string) => void;
    // home names the --home to pass, from the run's directory and the operator's HOME; by default a
    // fresh one with its .omp directory.
    home?: (run: string, operatorHome: string) => string;
  } = {}
): InstallRun {
  const run = join(dir, `run-${++runs}`);
  const operatorHome = join(run, "operator-home");
  const dbs = opts.dbs ?? { ".omp/agent/agent.db": anthropicOnly };
  for (const [path, logins] of Object.entries(dbs)) operatorDb(join(operatorHome, path), logins);
  const home = opts.home ? opts.home(run, operatorHome) : join(run, "omp-home");
  if (!opts.home) mkdirSync(join(home, ".omp"), { recursive: true });
  const profile = opts.profile ?? "legion-e2e-test";
  const agent = join(home, ".omp/profiles", profile, "agent");
  const dest = join(run, "dest");
  opts.prepare?.(agent, run);
  const fakeRun = join(run, "bin", "ran");
  writeFakes(join(run, "bin"), opts.fake ?? {});
  const result = Bun.spawnSync(
    [
      "bash",
      join(lib, "install-login-model-route.sh"),
      "--profile",
      profile,
      "--home",
      home,
      "--dest",
      dest,
      "--omp",
      "github:example/omp@1",
      ...(opts.args ?? ["--run-minutes", "180"]),
    ],
    {
      env: {
        PATH: `${join(run, "bin")}:${process.env.PATH ?? ""}`,
        HOME: operatorHome,
        // Variables of the caller's own session that must not reach the preflight.
        ANTHROPIC_API_KEY: "sk-ant-must-not-reach-the-preflight",
        ANTHROPIC_OAUTH_TOKEN: "must-not-reach-the-preflight",
        PI_CONFIG_FILES: "/must/not/reach/the/preflight.yml",
        CLAUDE_CONFIG_DIR: "/must/not/reach/the/preflight",
        ...opts.env,
      },
    }
  );
  const stderr = result.stderr.toString();
  return {
    operatorHome,
    home,
    agent,
    dest,
    fakeRun,
    result,
    stderr,
    ran: existsSync(`${fakeRun}.argv`),
  };
}

// expectNothingInstalled holds that a refused run linked nothing, wrote no profile file and ran no
// omp -p.
function expectNothingInstalled(r: InstallRun) {
  expect(() => lstatSync(join(r.agent, "agent.db"))).toThrow();
  expect(existsSync(join(r.agent, "config.yml"))).toBe(false);
  expect(r.ran).toBe(false);
}

// Each test runs the installer, and its bun and fakes, one to seven times: seconds each on a loaded box.
const slow = 60_000;

describe("the login model route installer", () => {
  test(
    "links agent.db to the operator's, writes the profile's route, and preflights in a pane's environment",
    () => {
      const r = install({
        dbs: {
          ".omp/agent/agent.db": [
            { provider: "anthropic" },
            { provider: "openai-codex" },
            { provider: "google" },
            { provider: "github-copilot", disabled: true },
            { provider: "mcp_oauth:profile:default:https://mcp.example.com/mcp" },
          ],
        },
      });
      expect(r.stderr).toContain("preflight answered");
      expect(r.result.exitCode).toBe(0);
      const db = join(r.operatorHome, ".omp/agent/agent.db");
      expect(r.result.stdout.toString().trim()).toBe(db);
      expect(lstatSync(join(r.agent, "agent.db")).isSymbolicLink()).toBe(true);
      expect(readlinkSync(join(r.agent, "agent.db"))).toBe(db);
      expect(existsSync(join(r.agent, "models.yml"))).toBe(false);
      const config = Bun.YAML.parse(readFileSync(join(r.agent, "config.yml"), "utf8")) as {
        enabledModels: string[];
        disabledProviders: string[];
        modelRoles: Record<string, string>;
      };
      expect(config.enabledModels).toEqual(["anthropic/claude-opus-4-8"]);
      expect([...config.disabledProviders].sort()).toEqual(
        [
          "amazon-bedrock",
          "bedrock-mantle",
          "google",
          "llama.cpp",
          "lm-studio",
          "ollama",
          "openai-codex",
        ].sort()
      );
      expect(Object.keys(config.modelRoles).length).toBe(12);
      for (const model of Object.values(config.modelRoles))
        expect(model).toBe("anthropic/claude-opus-4-8");
      const argv = readFileSync(`${r.fakeRun}.argv`, "utf8").split("\n");
      expect(argv.slice(0, 9)).toEqual([
        "x",
        "github:example/omp@1",
        "--",
        "omp",
        "-p",
        "--no-extensions",
        "--model",
        "anthropic/claude-opus-4-8",
        "Reply with exactly: OK",
      ]);
      expect(readFileSync(`${r.fakeRun}.cwd`, "utf8").startsWith(r.home)).toBe(false);
      const seen = readFileSync(`${r.fakeRun}.env`, "utf8");
      for (const name of [
        "ANTHROPIC_API_KEY",
        "ANTHROPIC_OAUTH_TOKEN",
        "PI_CONFIG_FILES",
        "CLAUDE_CONFIG_DIR",
      ]) {
        expect(seen).not.toMatch(new RegExp(`^${name}=`, "m"));
      }
      expect(seen).toMatch(/^OMP_PROFILE=legion-e2e-test$/m);
      expect(seen).toMatch(new RegExp(`^HOME=${r.home}$`, "m"));
      expect(readFileSync(join(r.dest, "omp-preflight.log"), "utf8")).toContain("OK");
      expect(statSync(r.dest).mode & 0o777).toBe(0o700);
    },
    slow
  );

  test(
    "links the database of the profile OMP_PROFILE names, and the default one when OMP_PROFILE is set but empty",
    () => {
      const dbs = {
        ".omp/agent/agent.db": anthropicOnly,
        ".omp/profiles/work/agent/agent.db": anthropicOnly,
      };
      const named = install({ dbs, env: { OMP_PROFILE: "work", PI_PROFILE: "other" } });
      expect(named.result.exitCode).toBe(0);
      expect(readlinkSync(join(named.agent, "agent.db"))).toBe(
        join(named.operatorHome, ".omp/profiles/work/agent/agent.db")
      );
      const fromPi = install({ dbs, env: { PI_PROFILE: "work" } });
      expect(readlinkSync(join(fromPi.agent, "agent.db"))).toBe(
        join(fromPi.operatorHome, ".omp/profiles/work/agent/agent.db")
      );
      const empty = install({ dbs, env: { OMP_PROFILE: "", PI_PROFILE: "work" } });
      expect(empty.result.exitCode).toBe(0);
      expect(readlinkSync(join(empty.agent, "agent.db"))).toBe(
        join(empty.operatorHome, ".omp/agent/agent.db")
      );
    },
    slow
  );

  test(
    "finds the database under XDG_DATA_HOME/omp when Oh My Pi's data moved there, and under ~/.omp when it did not",
    () => {
      const migrated = install({
        dbs: {
          "xdg/omp/agent.db": anthropicOnly,
          ".omp/agent/agent.db": [{ provider: "openai-codex" }],
        },
        prepare: (_agent, run) =>
          mkdirSync(join(run, "operator-home/xdg/omp"), { recursive: true }),
        env: { XDG_DATA_HOME: join(dir, `run-${runs + 1}`, "operator-home/xdg") },
      });
      expect(migrated.result.exitCode).toBe(0);
      expect(readlinkSync(join(migrated.agent, "agent.db"))).toBe(
        join(migrated.operatorHome, "xdg/omp/agent.db")
      );
      const unmigrated = install({ env: { XDG_DATA_HOME: join(dir, "no-such-xdg") } });
      expect(unmigrated.result.exitCode).toBe(0);
      expect(readlinkSync(join(unmigrated.agent, "agent.db"))).toBe(
        join(unmigrated.operatorHome, ".omp/agent/agent.db")
      );
    },
    slow
  );

  test(
    "fails, installing nothing, without a database or without a live anthropic login in it",
    () => {
      const none = install({ dbs: {} });
      expect(none.result.exitCode).toBe(1);
      expect(none.stderr).toContain("has no agent.db");
      expectNothingInstalled(none);
      const signedOut = install({
        dbs: {
          ".omp/agent/agent.db": [
            { provider: "anthropic", disabled: true },
            { provider: "openai-codex" },
          ],
        },
      });
      expect(signedOut.result.exitCode).toBe(1);
      expect(signedOut.stderr).toContain("not signed in to anthropic");
      expectNothingInstalled(signedOut);
    },
    slow
  );

  test(
    "fails, installing nothing, when an anthropic login would expire within the run",
    () => {
      const r = install({
        dbs: {
          ".omp/agent/agent.db": [
            { provider: "anthropic" },
            { provider: "anthropic", minutesLeft: 90 },
          ],
        },
      });
      expect(r.result.exitCode).toBe(1);
      expect(r.stderr).toMatch(/expires in 8\d minutes, within the run's 180/);
      expectNothingInstalled(r);
    },
    slow
  );

  test(
    "fails, installing nothing, when --omp differs from the operator's own omp",
    () => {
      const r = install({
        fake: { pinVersion: "18.13.0", operatorVersion: "18.12.2" },
      });
      expect(r.result.exitCode).toBe(1);
      expect(r.stderr).toContain("not the operator's own 18.12.2");
      expectNothingInstalled(r);
    },
    slow
  );

  test(
    "fails when the preflight's main session holds no answer from the route's model on anthropic",
    () => {
      const fellBack = install({
        fake: {
          session: [
            answer("anthropic", "claude-opus-4-8", "error"),
            answer("amazon-bedrock", "claude-opus-4-8"),
          ],
        },
      });
      expect(fellBack.result.exitCode).toBe(1);
      expect(fellBack.stderr).toContain("holds no answer from anthropic/claude-opus-4-8");
      const otherModel = install({ fake: { session: [answer("anthropic", "claude-sonnet-5-5")] } });
      expect(otherModel.result.exitCode).toBe(1);
      const sidecarOnly = install({
        fake: {
          session: [answer("anthropic", "claude-opus-4-8", "aborted")],
          sidecar: [answer("anthropic", "claude-opus-4-8")],
        },
      });
      expect(sidecarOnly.result.exitCode).toBe(1);
    },
    slow
  );

  test(
    "fails when the preflight's omp exits non-zero, naming its status",
    () => {
      const r = install({ fake: { exit: 3 } });
      expect(r.result.exitCode).toBe(1);
      expect(r.stderr).toContain("exited 3");
    },
    slow
  );

  test(
    "refuses a profile that is not fresh, a used --dest, OMP's default profile and a missing --run-minutes",
    () => {
      const leftover = install({
        prepare: (agent) => {
          mkdirSync(agent, { recursive: true });
          writeFileSync(join(agent, "models.yml"), "providers: {}\n");
        },
      });
      expect(leftover.result.exitCode).toBe(2);
      expect(leftover.stderr).toContain("models.yml exists");
      expect(leftover.ran).toBe(false);
      const dangling = install({
        prepare: (agent) => {
          mkdirSync(agent, { recursive: true });
          symlinkSync(join(dir, "no-such-db"), join(agent, "agent.db"));
        },
      });
      expect(dangling.result.exitCode).toBe(2);
      expect(dangling.stderr).toContain("agent.db exists");
      const usedDest = install({
        prepare: (_agent, run) => {
          mkdirSync(join(run, "dest"), { recursive: true });
          writeFileSync(join(run, "dest", "earlier"), "x");
        },
      });
      expect(usedDest.result.exitCode).toBe(2);
      expect(usedDest.stderr).toContain("--dest");
      expectNothingInstalled(usedDest);
      for (const profile of ["default", " default "]) {
        const r = install({ profile });
        expect(r.result.exitCode).toBe(2);
        expect(r.stderr).toContain("OMP's default profile");
      }
      const noMinutes = install({ args: [] });
      expect(noMinutes.result.exitCode).toBe(2);
      expect(noMinutes.stderr).toContain("--run-minutes is required");
    },
    slow
  );

  test(
    "fails, installing nothing, when PI_CONFIG_DIR or PI_CODING_AGENT_DIR moves the operator's database",
    () => {
      for (const env of [{ PI_CONFIG_DIR: ".omp2" }, { PI_CODING_AGENT_DIR: join(dir, "agent") }]) {
        const r = install({ env });
        expect(r.result.exitCode).toBe(1);
        expect(r.stderr).toContain("PI_CONFIG_DIR or PI_CODING_AGENT_DIR is set");
        expectNothingInstalled(r);
      }
    },
    slow
  );

  test(
    "refuses, installing nothing, a --home that is the operator's own HOME, one with no .omp, and an empty HOME",
    () => {
      const own = install({ home: (_run, operatorHome) => operatorHome });
      expect(own.result.exitCode).toBe(2);
      expect(own.stderr).toContain("is your own HOME");
      expect(existsSync(join(own.operatorHome, ".omp/profiles"))).toBe(false);
      expect(own.ran).toBe(false);
      const bare = install({ home: (run) => join(run, "bare-home") });
      expect(bare.result.exitCode).toBe(2);
      expect(bare.stderr).toContain("has no .omp directory");
      expect(bare.ran).toBe(false);
      const noHome = install({ env: { HOME: "" } });
      expect(noHome.result.exitCode).toBe(2);
      expect(noHome.stderr).toContain("HOME is unset or empty");
      expectNothingInstalled(noHome);
    },
    slow
  );
});
