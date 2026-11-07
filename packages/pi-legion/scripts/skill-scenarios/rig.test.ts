import { afterAll, describe, expect, setDefaultTimeout, test } from "bun:test";
import {
  cpSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { homedir, tmpdir } from "node:os";
import path from "node:path";
import { scriptFunctions } from "../../../../scripts/e2e/lib/script-functions";

// Every test here runs rig.sh and its stand-ins as real processes. On a loaded machine one test
// takes longer than bun's 5 s default (the eight-turn conversation took 11 s), so each test gets
// one minute.
setDefaultTimeout(60_000);

// rig.sh's install_model_route, run with the checkout's real installers: the route a label's
// profile reaches Anthropic by, picked by LEGION_E2E_MODEL_ROUTE.
const root = path.resolve(import.meta.dir, "../../../..");
const fn = scriptFunctions(path.join(import.meta.dir, "rig.sh"));
const work = mkdtempSync(path.join(tmpdir(), "skill-scenarios-rig-"));
afterAll(() => rmSync(work, { recursive: true, force: true }));
const operatorHome = path.join(work, "operator-home");
mkdirSync(operatorHome);
/** The variables every child process of this file gets: the caller's PATH, a HOME of the test's
 * own, and the caller's mise data and config directories. A tool on PATH can be a mise shim (jq and
 * jj are, on a devbox). Under a fresh HOME such a shim finds neither its installs nor its global
 * config, so it fails. make_omp_home keeps the operator's two directories for the processes it
 * starts for the same reason, and base_env passes MISE_DATA_DIR to every agent. */
const childEnv = {
  PATH: process.env.PATH ?? "",
  HOME: operatorHome,
  MISE_DATA_DIR:
    process.env.MISE_DATA_DIR ??
    path.join(process.env.XDG_DATA_HOME ?? path.join(homedir(), ".local", "share"), "mise"),
  XDG_CONFIG_HOME: process.env.XDG_CONFIG_HOME ?? path.join(homedir(), ".config"),
};

let profiles = 0;
/** A label's profile directory with an empty Oh My Pi home, as make_omp_home leaves it. */
function profileDir(): string {
  const dir = path.join(work, `profile-${++profiles}`);
  mkdirSync(path.join(dir, "home", ".omp"), { recursive: true });
  return dir;
}
function keyFile(mode: number): string {
  const file = path.join(work, `key-${profiles}-${mode.toString(8)}`);
  writeFileSync(file, "sk-ant-rig-test-0000000000000000\n", { mode });
  return file;
}
function route(dir: string, env: Record<string, string>, model = "") {
  const result = Bun.spawnSync(
    [
      "bash",
      "-c",
      `route_model_id=claude-opus-4-8\n${fn("install_model_route")}\n${fn("install_provider_route")}\ninstall_model_route "$1" skills-test "$2" "$3"`,
      "-",
      root,
      dir,
      model,
    ],
    { env: { ...childEnv, ...env } }
  );
  return { code: result.exitCode, stderr: result.stderr.toString() };
}

describe("install_model_route", () => {
  test("key-file writes the profile's route, its key command under model-key and its record under key", () => {
    const dir = profileDir();
    const { code, stderr } = route(dir, {
      LEGION_E2E_MODEL_ROUTE: "key-file",
      LEGION_E2E_ANTHROPIC_KEY_FILE: keyFile(0o600),
    });
    expect({ code, stderr: code === 0 ? "" : stderr }).toEqual({ code: 0, stderr: "" });
    const models = readFileSync(
      path.join(dir, "home/.omp/profiles/skills-test/agent/models.yml"),
      "utf8"
    );
    expect(models).toContain(`!exec ${path.join(dir, "model-key", "anthropic-key")}`);
    expect(models).toContain("https://api.anthropic.com");
    const served = Bun.spawnSync([path.join(dir, "model-key", "anthropic-key")]);
    expect(served.stdout.toString().trim()).toBe("sk-ant-rig-test-0000000000000000");
    expect(existsSync(path.join(dir, "key", "anthropic-key.log"))).toBe(true);
  });

  test("key-file without LEGION_E2E_ANTHROPIC_KEY_FILE fails, naming the variable", () => {
    const dir = profileDir();
    const { code, stderr } = route(dir, { LEGION_E2E_MODEL_ROUTE: "key-file" });
    expect(code).toBe(1);
    expect(stderr).toContain("LEGION_E2E_ANTHROPIC_KEY_FILE is unset");
    expect(existsSync(path.join(dir, "home/.omp/profiles/skills-test/agent/models.yml"))).toBe(
      false
    );
  });

  test("a key file others can read is refused, and no key command is left behind", () => {
    const dir = profileDir();
    const { code, stderr } = route(dir, {
      LEGION_E2E_MODEL_ROUTE: "key-file",
      LEGION_E2E_ANTHROPIC_KEY_FILE: keyFile(0o644),
    });
    expect(code).not.toBe(0);
    expect(stderr).toContain("group or other can read it");
    expect(existsSync(path.join(dir, "model-key"))).toBe(false);
  });

  /** An operator's models.yml holding the gateway block the provider route names, and another. */
  function providerFile(apiKey: string): string {
    const file = path.join(work, `models-${profiles}.yml`);
    writeFileSync(
      file,
      [
        "providers:",
        "  openai:",
        '    apiKey: "!secrets get --value OPENAI_KEY"',
        "  box-gateway:",
        "    baseUrl: http://gateway.internal.example:4000",
        `    apiKey: "${apiKey}"`,
        "    api: anthropic-messages",
        "    authHeader: true",
        "    models:",
        "      - id: claude-opus-5-5",
        "        thinking:",
        "          mode: anthropic-adaptive",
        "          efforts: [low, high]",
        "",
      ].join("\n")
    );
    return file;
  }

  test("provider copies the named block, its !cat key command as written, with the rig's one model", () => {
    const dir = profileDir();
    const { code, stderr } = route(dir, {
      LEGION_E2E_MODEL_ROUTE: "provider",
      LEGION_E2E_MODEL_PROVIDER_FILE: providerFile("!cat /run/user/1000/box/gateway-key"),
      LEGION_E2E_MODEL_PROVIDER: "box-gateway",
    });
    expect({ code, stderr: code === 0 ? "" : stderr }).toEqual({ code: 0, stderr: "" });
    const agent = path.join(dir, "home/.omp/profiles/skills-test/agent");
    expect(JSON.parse(readFileSync(path.join(agent, "models.yml"), "utf8"))).toEqual({
      providers: {
        "box-gateway": {
          baseUrl: "http://gateway.internal.example:4000",
          apiKey: "!cat /run/user/1000/box/gateway-key",
          api: "anthropic-messages",
          authHeader: true,
          models: [{ id: "claude-opus-4-8" }],
        },
      },
    });
    const config = readFileSync(path.join(agent, "config.yml"), "utf8");
    expect(config).toContain("enabledModels:\n  - box-gateway/claude-opus-4-8\n");
    expect(config).toContain("  - anthropic\n");
    expect(config).toContain("  default: box-gateway/claude-opus-4-8\n");
    expect(config).toContain("  task: box-gateway/claude-opus-4-8\n");
    expect(existsSync(path.join(dir, "model-key"))).toBe(false);
  });

  test("provider with a model the block lists takes the block's own entry for it", () => {
    const dir = profileDir();
    const { code, stderr } = route(
      dir,
      {
        LEGION_E2E_MODEL_ROUTE: "provider",
        LEGION_E2E_MODEL_PROVIDER_FILE: providerFile("!cat /run/user/1000/box/gateway-key"),
        LEGION_E2E_MODEL_PROVIDER: "box-gateway",
      },
      "claude-opus-5-5"
    );
    expect({ code, stderr: code === 0 ? "" : stderr }).toEqual({ code: 0, stderr: "" });
    const agent = path.join(dir, "home/.omp/profiles/skills-test/agent");
    expect(
      JSON.parse(readFileSync(path.join(agent, "models.yml"), "utf8")).providers["box-gateway"]
        .models
    ).toEqual([
      { id: "claude-opus-5-5", thinking: { mode: "anthropic-adaptive", efforts: ["low", "high"] } },
    ]);
    expect(readFileSync(path.join(agent, "config.yml"), "utf8")).toContain(
      "  default: box-gateway/claude-opus-5-5\n"
    );
  });

  // Catches: a profile whose one model is disabled because the operator's block carries a bundled
  // provider's name, which Oh My Pi refuses to start on ("No model available matching
  // enabledModels (anthropic/claude-opus-4-8) with usable credentials").
  test("provider leaves its own name out of the disabled providers", () => {
    const dir = profileDir();
    // The operator's own block, under a bundled provider's name: the deployment's gateway block
    // is often `anthropic`, since that is the API it speaks.
    const file = providerFile("!cat /run/user/1000/box/gateway-key");
    writeFileSync(
      file,
      `${readFileSync(file, "utf8")}  anthropic:
    baseUrl: http://gateway.internal.example:4000
    apiKey: "!cat /run/user/1000/box/gateway-key"
    api: anthropic-messages
`
    );
    const { code, stderr } = route(dir, {
      LEGION_E2E_MODEL_ROUTE: "provider",
      LEGION_E2E_MODEL_PROVIDER_FILE: file,
      LEGION_E2E_MODEL_PROVIDER: "anthropic",
    });
    expect({ code, stderr: code === 0 ? "" : stderr }).toEqual({ code: 0, stderr: "" });
    const agent = path.join(dir, "home/.omp/profiles/skills-test/agent");
    const config = readFileSync(path.join(agent, "config.yml"), "utf8");
    expect(config).toContain("enabledModels:\n  - anthropic/claude-opus-4-8\n");
    expect(config).toContain("disabledProviders:\n  - amazon-bedrock\n");
    expect(config).not.toContain("  - anthropic\n");
  });

  test("provider without LEGION_E2E_MODEL_PROVIDER_FILE fails, naming the variable", () => {
    const dir = profileDir();
    const { code, stderr } = route(dir, {
      LEGION_E2E_MODEL_ROUTE: "provider",
      LEGION_E2E_MODEL_PROVIDER: "box-gateway",
    });
    expect(code).toBe(1);
    expect(stderr).toContain("LEGION_E2E_MODEL_PROVIDER_FILE is unset");
  });

  test("provider refuses a block absent from the file, and one whose key is not read by !cat", () => {
    const missing = route(profileDir(), {
      LEGION_E2E_MODEL_ROUTE: "provider",
      LEGION_E2E_MODEL_PROVIDER_FILE: providerFile("!cat /run/user/1000/box/gateway-key"),
      LEGION_E2E_MODEL_PROVIDER: "elsewhere",
    });
    expect(missing.code).toBe(1);
    expect(missing.stderr).toContain("has no provider block elsewhere");
    const literal = profileDir();
    const inline = route(literal, {
      LEGION_E2E_MODEL_ROUTE: "provider",
      LEGION_E2E_MODEL_PROVIDER_FILE: providerFile("sk-literal-key"),
      LEGION_E2E_MODEL_PROVIDER: "box-gateway",
    });
    expect(inline.code).toBe(1);
    expect(inline.stderr).toContain('does not read its key as "!cat <absolute file>"');
    expect(existsSync(path.join(literal, "home/.omp/profiles/skills-test/agent/models.yml"))).toBe(
      false
    );
  });

  test("an unknown route fails, naming the three it takes", () => {
    const dir = profileDir();
    const { code, stderr } = route(dir, { LEGION_E2E_MODEL_ROUTE: "bedrock" });
    expect(code).toBe(1);
    expect(stderr).toContain("it is gateway, key-file or provider");
  });
});

// The key-file route's key command reads the operator's key for as long as it exists
// (install-key-file-model-route.sh: the caller removes --key-dir on every exit that keeps no
// profile). A setup that stops after the route is installed leaves no profile to use it.
describe("a setup that fails after install_model_route", () => {
  /** A checkout whose plugin installer and home maker do nothing, with the real key-file route. */
  function checkout(): string {
    const co = freshDir("route-checkout");
    mkdirSync(path.join(co, "scripts/e2e/lib"), { recursive: true });
    mkdirSync(path.join(co, "skills/asd-ste100"), { recursive: true });
    writeFileSync(path.join(co, "skills/asd-ste100/SKILL.md"), "# STE\n");
    writeFileSync(path.join(co, ".omp-pin"), "omp@pin\n");
    // The real key-file route, with the lib it sources for its base URL's check.
    for (const file of ["install-key-file-model-route.sh", "model-endpoint.sh"]) {
      cpSync(path.join(root, "scripts/e2e/lib", file), path.join(co, "scripts/e2e/lib", file));
    }
    writeFileSync(
      path.join(co, "scripts/e2e/lib/omp-home.sh"),
      'make_omp_home() { mkdir -p "$1/.omp"; }\n'
    );
    writeFileSync(path.join(co, "scripts/e2e/lib/install-plugin-profile.sh"), "exit 0\n");
    return co;
  }
  const routeEnv = () => ({
    LEGION_E2E_MODEL_ROUTE: "key-file",
    LEGION_E2E_ANTHROPIC_KEY_FILE: keyFile(0o600),
  });

  // Catches: a label profile that fails after its route is installed (here the superpowers probe)
  // and keeps a key command that still reads the operator's key.
  test("cmd_profile removes the label's model-key and marks nothing ready", () => {
    const work = freshDir("route-work");
    const script = [
      "set -euo pipefail",
      ...[
        "fail",
        "label_dir",
        "on_exit",
        "drop_unready_keys",
        "install_model_route",
        "cmd_profile",
      ].map(fn),
      "checkout_commit() { printf 'fixture parents=base\\n'; }",
      "install_superpowers() { fail 'the superpowers probe failed'; }",
      'work=$1; cmd_profile probe "$2"',
    ].join("\n");
    const failed = bash(script, [work, checkout()], {
      ...routeEnv(),
      SKILL_SCENARIOS_SUPERPOWERS: "set",
    });
    expect(failed.code).toBe(1);
    expect(failed.stderr).toContain("the superpowers probe failed");
    const P = path.join(work, "profiles/probe");
    expect(existsSync(path.join(P, "home/.omp/profiles/skills-probe/agent/models.yml"))).toBe(true);
    expect(existsSync(path.join(P, "model-key"))).toBe(false);
    expect(existsSync(path.join(P, "ready"))).toBe(false);

    // The same setup that succeeds keeps its key command: a ready profile uses it.
    const kept = freshDir("route-work");
    const ok = bash(
      script.replace("fail 'the superpowers probe failed'", ":"),
      [kept, checkout()],
      {
        ...routeEnv(),
        SKILL_SCENARIOS_SUPERPOWERS: "set",
      }
    );
    expect({ code: ok.code, stderr: ok.stderr }).toEqual({ code: 0, stderr: "" });
    expect(existsSync(path.join(kept, "profiles/probe/model-key/anthropic-key"))).toBe(true);
    expect(existsSync(path.join(kept, "profiles/probe/ready"))).toBe(true);
  });

  // Catches: a judge-profile build that fails at its second profile and keeps the first one's key
  // command.
  test("judge_profiles removes both profiles' model-key and marks nothing ready", () => {
    const work = freshDir("judge-route-work");
    const co = checkout();
    const script = [
      "set -euo pipefail",
      ...["fail", "on_exit", "drop_unready_keys", "install_model_route", "judge_profiles"].map(fn),
      "checkout_commit() { printf 'fixture parents=base\\n'; }",
      "root=$1; work=$2; judge_model=fixture; judge_thinking=high; judge_profiles",
    ].join("\n");
    // The bare profile's home cannot be made, after the ste profile's route is installed.
    writeFileSync(
      path.join(co, "scripts/e2e/lib/omp-home.sh"),
      'make_omp_home() { [ "$(basename "$(dirname "$1")")" != bare ] || { echo "no home for bare" >&2; return 1; }; mkdir -p "$1/.omp"; }\n'
    );
    const failed = bash(script, [co, work], routeEnv());
    expect(failed.code).toBe(1);
    expect(failed.stderr).toContain("make_omp_home failed for the judge's bare profile");
    const dir = path.join(work, "judge-profiles");
    expect(existsSync(path.join(dir, "ste/home/.omp/profiles/judge-ste/agent/models.yml"))).toBe(
      true
    );
    expect(existsSync(path.join(dir, "ste/model-key"))).toBe(false);
    expect(existsSync(path.join(dir, "bare/model-key"))).toBe(false);
    expect(existsSync(path.join(dir, "ready"))).toBe(false);
  });
});

/** Runs SCRIPT (rig.sh functions and a call of them) under bash with ARGS as $1..., in an
 * environment of PATH and the test's own HOME. */
function bash(script: string, args: string[] = [], env: Record<string, string> = {}) {
  const result = Bun.spawnSync(["bash", "-c", script, "-", ...args], {
    env: { ...childEnv, ...env },
  });
  return {
    code: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
  };
}
let dirs = 0;
/** A new directory under the test's work directory. */
function freshDir(name: string): string {
  const dir = path.join(work, `${name}-${++dirs}`);
  mkdirSync(dir, { recursive: true });
  return dir;
}

// cmd_profile records the label's commit in built-from with checkout_commit. A git worktree holds
// no .jj, and jj alone printed an empty commit line for it.
describe("checkout_commit", () => {
  const git = (dir: string, ...args: string[]) => {
    const result = Bun.spawnSync(
      [
        "git",
        "-C",
        dir,
        "-c",
        "user.name=Rig Test",
        "-c",
        "user.email=rig@example.invalid",
        "-c",
        "commit.gpgsign=false",
        ...args,
      ],
      { env: childEnv }
    );
    if (result.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${result.stderr}`);
    return result.stdout.toString().trim();
  };
  function gitCheckout() {
    const dir = freshDir("git-checkout");
    git(dir, "init", "-q", "-b", "main");
    writeFileSync(path.join(dir, "a.txt"), "a\n");
    git(dir, "add", "-A");
    git(dir, "commit", "-qm", "first");
    const first = git(dir, "rev-parse", "HEAD");
    writeFileSync(path.join(dir, "b.txt"), "b\n");
    git(dir, "add", "-A");
    git(dir, "commit", "-qm", "second");
    return { dir, first, head: git(dir, "rev-parse", "HEAD") };
  }
  const commit = (dir: string) => bash(`${fn("checkout_commit")}\ncheckout_commit "$1"`, [dir]);

  // Catches: a checkout_commit that reads jj alone (an empty line for git), or names no parents.
  test("a clean git checkout prints its HEAD and HEAD's parents", () => {
    const { dir, first, head } = gitCheckout();
    expect(commit(dir)).toEqual({ code: 0, stdout: `${head} parents=${first}\n`, stderr: "" });
  });

  // Catches: a built-from that names a commit the label's files do not match.
  test("a git checkout with a change git status lists is marked dirty", () => {
    const { dir, first, head } = gitCheckout();
    writeFileSync(path.join(dir, "a.txt"), "changed\n");
    expect(commit(dir).stdout).toBe(`${head} parents=${first} dirty\n`);
  });

  // Catches: a checkout_commit that hides a failed read, which cmd_profile would record as an
  // empty commit line.
  test("a directory that is no checkout fails", () => {
    expect(commit(freshDir("no-checkout")).code).not.toBe(0);
  });
});

// cmd_live_read reads every path skill_reads prints. asd-ste100 holds examples/ beside
// references/, and a read of SKILL.md and references/ alone left examples/ unread.
describe("skill_reads", () => {
  function checkout() {
    const co = freshDir("skills-checkout");
    const skill = path.join(co, "skills", "demo");
    for (const file of [
      "SKILL.md",
      "references/rules.md",
      "examples/before-after.md",
      "examples/data.json",
      "LICENSE",
    ]) {
      mkdirSync(path.dirname(path.join(skill, file)), { recursive: true });
      writeFileSync(path.join(skill, file), `${file}\n`);
    }
    return co;
  }
  const reads = (co: string, skill: string) =>
    bash(`${fn("fail")}\n${fn("skill_reads")}\nskill_reads "$1" "$2"`, [co, skill]);

  // Catches: a list that leaves examples/ out, or reads a file that is not markdown.
  test("SKILL.md first, then every other markdown file in byte order", () => {
    expect(reads(checkout(), "demo")).toEqual({
      code: 0,
      stdout: [
        "skill://demo",
        "skill://demo/examples/before-after.md",
        "skill://demo/references/rules.md",
        "",
      ].join("\n"),
      stderr: "",
    });
  });

  // Catches: a live read of a skill the checkout does not have, which reads nothing and passes.
  test("a skill the checkout lacks fails, naming it", () => {
    const co = checkout();
    const { code, stderr } = reads(co, "absent");
    expect(code).toBe(1);
    expect(stderr).toContain(`${co} has no skill absent`);
  });
});

// Oh My Pi walks a run's cwd's ancestors for a project `.omp`, past the run's own HOME. Under a
// work directory below the operator's HOME, a live read loaded the operator's plugins from
// $HOME/.omp in place of the label's packed ones, and nothing scored that as a failure.
describe("omp_ancestor", () => {
  const ancestor = (dir: string) => bash(`${fn("omp_ancestor")}\nomp_ancestor "$1"`, [dir]);

  // Catches: a check that misses a `.omp` above the work directory, the measured case.
  test("names the nearest ancestor that holds .omp", () => {
    const top = freshDir("omp-above");
    mkdirSync(path.join(top, ".omp"));
    const work = path.join(top, "a", "work");
    expect(ancestor(work)).toEqual({ code: 0, stdout: `${top}/.omp\n`, stderr: "" });
  });

  // Catches: a check that starts at the parent and so misses the work directory's own `.omp`.
  test("the work directory's own .omp counts", () => {
    const work = freshDir("omp-self");
    writeFileSync(path.join(work, ".omp"), "");
    expect(ancestor(work).stdout).toBe(`${work}/.omp\n`);
  });

  // Catches: a check that refuses every work directory.
  test("prints nothing and fails when no ancestor holds .omp", () => {
    expect(ancestor(path.join(freshDir("omp-none"), "work"))).toEqual({
      code: 1,
      stdout: "",
      stderr: "",
    });
  });

  // Catches: a rig.sh that defines the check and never runs it.
  test("rig.sh refuses such a work directory before it makes it, naming the .omp", () => {
    const top = freshDir("omp-rig");
    mkdirSync(path.join(top, ".omp"));
    const work = path.join(top, "work");
    const result = Bun.spawnSync(["bash", path.join(import.meta.dir, "rig.sh"), "score"], {
      env: { ...childEnv, SKILL_SCENARIOS_WORK: work },
    });
    expect(result.exitCode).toBe(1);
    expect(result.stderr.toString()).toContain(`${top}/.omp`);
    expect(existsSync(work)).toBe(false);
  });
});

// A spec-writing or spec-rewrite batch takes one run, numbered one above the highest run of its
// scenario and label, so a later batch never removes an earlier run (cmd_run starts from rm -rf).
describe("next_run", () => {
  const next = (work: string, scenario: string, label: string) =>
    bash(`${fn("next_run")}\nwork=$1\nnext_run "$2" "$3"`, [work, scenario, label]);

  // Catches: numbering that starts over at 1 and so overwrites run 1, or that counts directories
  // instead of reading their numbers.
  test("one above the highest number of the scenario and label", () => {
    const work = freshDir("numbering");
    for (const run of [
      "spec-writing-s1-1",
      "spec-writing-s1-10",
      "spec-writing-s1-x-4",
      "spec-writing-s1-2-7",
      "spec-rewrite-s1-30",
      "spec-writing-main-40",
    ])
      mkdirSync(path.join(work, "runs", run), { recursive: true });
    writeFileSync(path.join(work, "runs", "spec-writing-s1-99"), "");
    expect(next(work, "spec-writing", "s1")).toEqual({ code: 0, stdout: "11\n", stderr: "" });
  });

  // Catches: a first batch that fails because $work/runs does not exist yet.
  test("1 when the label has no run of the scenario", () => {
    expect(next(freshDir("numbering-empty"), "spec-rewrite", "s1").stdout).toBe("1\n");
  });
});

// Every worker run's Envoy role topics are its Legion project's and its issue's, and worker_boot
// gives all three worker scenarios the same issue key. So the project is the only thing that keeps
// one run's Envoy messages out of another's session, and `skills<label>r<n>` does not: it names no
// scenario. Measured on 2026-11-06, with the batch's NATS given through SKILL_SCENARIOS_NATS_URL
// and so left running between batches, a run of the scenario threads-closed replaced, numbered 2
// of the label head, was delivered the Envoy message the reviewer run
// reviewer-text-findings-text-only-head-2 published 44 minutes earlier, under the project both
// were given, skillsheadr2: it named the bot thread PRRT_kwDOrig0001, which exists only in the
// reviewer scenario's world, and the agent resolved it. A run that reads another run's
// instructions measures nothing about its own rule.
describe("run_project", () => {
  const project = (scenario: string, runCase: string, label: string, n: string) =>
    bash(`${fn("run_project")}\nrun_project "$1" "$2" "$3" "$4"`, [scenario, runCase, label, n]);

  // Catches: a project built from the label and the run number alone, which two worker scenarios
  // of one label and number share, so Envoy delivers each run's messages to both.
  test("two scenarios of one label and run number get projects of their own", () => {
    const reviewer = project("reviewer-text-findings", "text-only", "head", "2");
    const closed = project("threads-closed", "text-fixes", "head", "2");
    const tester = project("tester-proof", "", "head", "2");
    expect(reviewer.code).toBe(0);
    expect(closed.code).toBe(0);
    expect(tester.code).toBe(0);
    expect(new Set([reviewer.stdout, closed.stdout, tester.stdout]).size).toBe(3);
  });

  // Catches: a project that keeps a hyphen or an upper-case letter from the scenario, the case or
  // the label, which isLegionProjectToken refuses, so no role token can be minted for the run.
  test("each project is a Legion project token", () => {
    for (const [scenario, runCase, label] of [
      ["reviewer-text-findings", "text-only", "head"],
      ["threads-closed", "code-defect", "base"],
      ["tester-proof", "", "ablate-R2"],
    ] as const)
      expect(project(scenario, runCase, label, "3").stdout.trim()).toMatch(/^[a-z0-9]+$/);
  });

  // Catches: a project that drops the case, so the two cases of one scenario, which run as two
  // batches against the same NATS, share their role topics.
  test("two cases of one scenario get projects of their own", () => {
    expect(project("threads-closed", "text-fixes", "head", "1").stdout).not.toBe(
      project("threads-closed", "code-defect", "head", "1").stdout
    );
  });
});

// A cased scenario's case reaches four places that must agree: cmd_batch refuses a case before it
// starts a service, cmd_run puts it in the run's directory name, the run writes it to
// <run>/case.txt, and score.ts reads the name and the file and holds them to each other. All four
// read it through scenario_case.
describe("scenario_case", () => {
  const cases = (scenario: string, env: Record<string, string> = {}) =>
    bash(
      `${fn("fail")}\n${fn("scenario_case")}\n${fn("askgate_case")}\n${fn("reviewer_case")}\n${fn("threads_case")}\nscenario_case "$1"`,
      [scenario],
      env
    );

  // Catches: a reviewer case the rig takes in one place and refuses in another.
  test("reviewer-text-findings takes text-only and defect", () => {
    for (const name of ["text-only", "defect"])
      expect(cases("reviewer-text-findings", { SKILL_SCENARIOS_REVIEWER_CASE: name })).toEqual({
        code: 0,
        stdout: `${name}\n`,
        stderr: "",
      });
  });

  // Catches: a batch that runs the wrong world because the variable is unset or misspelt, which
  // would score as the reviewer's own verdict.
  test("an unset or unknown reviewer case fails, naming the variable and both cases", () => {
    const unset: Record<string, string> = {};
    for (const env of [unset, { SKILL_SCENARIOS_REVIEWER_CASE: "text" }]) {
      const result = cases("reviewer-text-findings", env);
      expect(result.code).toBe(1);
      expect(result.stdout).toBe("");
      expect(result.stderr).toContain("SKILL_SCENARIOS_REVIEWER_CASE");
      expect(result.stderr).toContain("not text-only or defect");
    }
  });

  // Catches: a generalisation that drops askgate's own cases, or that gives an uncased scenario a
  // case and so a directory name no scorer reads.
  test("askgate keeps its cases, and a scenario that takes none prints nothing", () => {
    expect(cases("askgate", { SKILL_SCENARIOS_ASKGATE_CASE: "nochange-block" }).stdout).toBe(
      "nochange-block\n"
    );
    expect(cases("askgate", { SKILL_SCENARIOS_ASKGATE_CASE: "nope" }).code).toBe(1);
    expect(cases("tester-proof", { SKILL_SCENARIOS_REVIEWER_CASE: "defect" })).toEqual({
      code: 0,
      stdout: "",
      stderr: "",
    });
  });

  // Catches: a threads-closed batch that runs no case at all, which the issue's check runs with
  // no variable set, or one that takes a misspelt case for the measured arm.
  test("threads-closed takes both cases, runs text-fixes unset, and fails on any other word", () => {
    for (const name of ["text-fixes", "code-defect"])
      expect(cases("threads-closed", { SKILL_SCENARIOS_THREADS_CASE: name })).toEqual({
        code: 0,
        stdout: `${name}\n`,
        stderr: "",
      });
    expect(cases("threads-closed")).toEqual({ code: 0, stdout: "text-fixes\n", stderr: "" });
    const result = cases("threads-closed", { SKILL_SCENARIOS_THREADS_CASE: "text" });
    expect(result.code).toBe(1);
    expect(result.stdout).toBe("");
    expect(result.stderr).toContain("SKILL_SCENARIOS_THREADS_CASE");
    expect(result.stderr).toContain("not text-fixes or code-defect");
  });

  // Catches: a batch that starts its services and its runs before it reads the case (cmd_batch
  // calls scenario_case before it loads a label or locks the services).
  test("a batch of a cased scenario refuses an unset case before it starts anything", () => {
    const dir = freshDir("reviewer-batch");
    const result = Bun.spawnSync(
      ["bash", path.join(import.meta.dir, "rig.sh"), "batch", "reviewer-text-findings", "1", "x"],
      { env: { ...childEnv, SKILL_SCENARIOS_WORK: dir } }
    );
    expect(result.exitCode).toBe(1);
    expect(result.stderr.toString()).toContain("SKILL_SCENARIOS_REVIEWER_CASE");
    expect(existsSync(path.join(dir, "services"))).toBe(false);
  });
});

// cmd_batch and cmd_run read SKILL_SCENARIOS_SOURCE through source_dir before they start anything.
describe("source_dir", () => {
  function work(files: string[]) {
    const dir = freshDir("sources");
    mkdirSync(path.join(dir, "sources", "acme-1"), { recursive: true });
    for (const file of files) writeFileSync(path.join(dir, "sources", "acme-1", file), "x\n");
    return dir;
  }
  const sourceDir = (dir: string, scenario: string, name: string) =>
    bash(`${fn("fail")}\n${fn("source_dir")}\nwork=$1\nsource_dir "$2" "$3"`, [
      dir,
      scenario,
      name,
    ]);
  const full = ["title.txt", "messages.md", "asks.json"];

  // Catches: a spec-rewrite run seeded from a source with no old spec, which seed.ts would refuse
  // only after the batch started its services.
  test("spec-rewrite refuses a source without old-spec.md, naming it", () => {
    const dir = work(full);
    const { code, stderr } = sourceDir(dir, "spec-rewrite", "acme-1");
    expect(code).toBe(1);
    expect(stderr).toContain(`the source ${dir}/sources/acme-1 has no old-spec.md`);
  });

  // Catches: a spec-writing run refused for want of an old spec it never reads.
  test("spec-writing takes the same source, and prints its directory", () => {
    const dir = work(full);
    expect(sourceDir(dir, "spec-writing", "acme-1")).toEqual({
      code: 0,
      stdout: `${dir}/sources/acme-1\n`,
      stderr: "",
    });
    expect(sourceDir(work([...full, "old-spec.md"]), "spec-rewrite", "acme-1").code).toBe(0);
  });

  // Catches: a run that starts on a source missing a file seed.ts reads, or a name that leaves
  // $work/sources.
  test("refuses an unset name, a path, and a source missing a file", () => {
    const dir = work(["title.txt", "asks.json"]);
    expect(sourceDir(dir, "spec-writing", "").stderr).toContain("SKILL_SCENARIOS_SOURCE is unset");
    expect(sourceDir(dir, "spec-writing", "../sources/acme-1").stderr).toContain(
      "is not a directory name"
    );
    expect(sourceDir(dir, "spec-writing", "acme-1").stderr).toContain("has no messages.md");
  });
});

// The conversation of a spec run: rig.sh's spec_turns with stand-ins for omp (launch), seed.ts
// (seed_ts) and the judge's person stand-in (judge_ts). Each stand-in logs its call, and the
// seed.ts state stand-in hands back the state the test scripted for that turn.
describe("spec_turns", () => {
  interface Turn {
    asks?: number;
    approval?: boolean;
  }
  function conversation(
    turns: Turn[],
    options: { secondTranscript?: boolean; failAt?: number } = {}
  ) {
    const run = freshDir("spec-run");
    const states = freshDir("spec-states");
    mkdirSync(path.join(run, "sessions"));
    turns.forEach((turn, index) => {
      const t = index + 1;
      writeFileSync(
        path.join(states, `state-${t}.json`),
        JSON.stringify({
          turn: t,
          open_asks: Array.from({ length: turn.asks ?? 0 }, (_, k) => ({
            id: `ask-${t}-${k}`,
            question: "Q?",
            options: [],
            multiple: false,
          })),
          approval: turn.approval ? { ask_id: `approval-${t}`, version: t } : null,
        })
      );
    });
    const standIns = `
launch() {
  printf 'launch %s | %s\\n' "$*" "$(<"$R/prompt.txt")" >>"$R/calls"
  [ -n "\${SECOND:-}" ] && [ $# -gt 1 ] && : >"$R/sessions/fresh.jsonl"
  printf x >>"$R/launches"
  status=0
  [ "$(wc -c <"$R/launches")" != "\${FAIL_AT:-0}" ] || status=1
  printf 'output of %s\\nexit=%s\\n' "$*" "$status" >"$R/out.txt"
  printf '{}\\n' >>"$R/sessions/run.jsonl"
}
seed_ts() {
  printf 'seed %s\\n' "$*" >>"$R/calls"
  [ "$1" != state ] || cp "$STATES/state-$3.json" "$R/state-$3.json"
}
judge_ts() {
  printf 'judge %s\\n' "$*" >>"$R/calls"
  jq -c '.open_asks[] | {ask: .id, selected: [], text: "t"}' "$R/state-$3.json" >"$R/answers-$3.jsonl"
}`;
    const result = bash(
      `${fn("note")}\n${fn("fail")}\n${fn("spec_turns")}\n${standIns}\nR=$1\nspec_turns spec-writing-s1-1 LWEVAL-2 "Design it."`,
      [run],
      {
        STATES: states,
        ...(options.secondTranscript ? { SECOND: "1" } : {}),
        ...(options.failAt === undefined ? {} : { FAIL_AT: String(options.failAt) }),
      }
    );
    const read = (name: string) =>
      existsSync(path.join(run, name)) ? readFileSync(path.join(run, name), "utf8") : "";
    const calls = read("calls")
      .trimEnd()
      .split("\n")
      .map((line) => line.replaceAll(run, "<R>"));
    return { ...result, run, read, calls };
  }
  const answered = "The person answered your asks on LWEVAL-2.";
  const approved = "The person approved the spec of LWEVAL-2.";

  // Catches: a loop that answers before it reads the state, continues without --continue (a second
  // session), skips the stand-in, or sends another prompt than the three the rig.sh header gives.
  test("asks, then an approval: answer, continue, approve, continue once more, stop", () => {
    const { code, calls, read } = conversation([{ asks: 2 }, { approval: true }, {}]);
    expect(code).toBe(0);
    expect(calls).toEqual([
      "launch spec-writing-s1-1 | Design it.",
      "seed state <R>/fixture.json 1 <R>",
      "judge answer <R> 1",
      "seed answer <R>/fixture.json <R>/answers-1.jsonl",
      `launch spec-writing-s1-1 --continue | ${answered}`,
      "seed state <R>/fixture.json 2 <R>",
      "seed approve <R>/fixture.json",
      `launch spec-writing-s1-1 --continue | ${approved}`,
      "seed state <R>/fixture.json 3 <R>",
    ]);
    expect(read("turns.txt")).toBe("1 first exit=0\n2 answered exit=0\n3 approved exit=0\n");
    expect(read("answers.jsonl")).toBe(read("answers-1.jsonl"));
    expect(read("out-2.txt")).toBe("output of spec-writing-s1-1 --continue\nexit=0\n");
  });

  // Catches: a loop that answers the open asks when an approval request is open too (the rig.sh
  // header's order puts the approval first).
  test("an open approval request goes before open asks", () => {
    const { calls } = conversation([{ asks: 1, approval: true }, {}]);
    expect(calls.slice(1, 3)).toEqual([
      "seed state <R>/fixture.json 1 <R>",
      "seed approve <R>/fixture.json",
    ]);
    expect(calls.some((call) => call.startsWith("judge"))).toBe(false);
  });

  // Catches: a loop that continues a session with nothing for it to answer.
  test("no open ask and no approval request ends the run after its turn", () => {
    const { code, calls, read } = conversation([{}]);
    expect(code).toBe(0);
    expect(calls).toEqual([
      "launch spec-writing-s1-1 | Design it.",
      "seed state <R>/fixture.json 1 <R>",
    ]);
    expect(read("turns.txt")).toBe("1 first exit=0\n");
  });

  // Catches: a run past 8 turns, or an answer after the eighth turn that no turn reads.
  test("stops after 8 turns, answering nothing after the eighth", () => {
    const { calls, read } = conversation(Array.from({ length: 9 }, () => ({ asks: 1 })));
    expect(calls.filter((call) => call.startsWith("launch"))).toHaveLength(8);
    expect(calls.filter((call) => call.startsWith("judge"))).toHaveLength(7);
    expect(calls.at(-1)).toBe("seed state <R>/fixture.json 8 <R>");
    expect(read("turns.txt").trimEnd().split("\n")).toHaveLength(8);
  });

  // Catches: a --continue that starts a second transcript beside the first, which every reader of
  // the run would then read as one of two.
  test("a later turn that leaves a second transcript fails the run, naming its sessions", () => {
    const { code, stderr, run } = conversation([{ asks: 1 }, {}], { secondTranscript: true });
    expect(code).toBe(1);
    expect(stderr).toContain(`after turn 2, ${run}/sessions holds 2 transcripts`);
  });

  // Catches: a loop that answers and continues a session whose turn failed or timed out.
  test("a turn that does not exit 0 ends the run after its state", () => {
    const { code, calls, read } = conversation([{ asks: 1 }, { asks: 1 }, {}], { failAt: 2 });
    expect(code).toBe(0);
    expect(calls.at(-1)).toBe("seed state <R>/fixture.json 2 <R>");
    expect(calls.filter((call) => call.startsWith("judge"))).toHaveLength(1);
    expect(read("turns.txt")).toBe("1 first exit=0\n2 answered exit=1\n");
  });
});

// cmd_profile's superpowers step, with a stand-in mise: it records the environment omp got and
// writes a transcript whose one tool result is the text in result.txt beside it.
describe("install_superpowers", () => {
  function setup(result: string) {
    const bin = freshDir("superpowers-bin");
    const label = freshDir("superpowers-label");
    const src = freshDir("superpowers-src");
    for (const skill of ["writing-plans", "brainstorming"]) {
      mkdirSync(path.join(src, "skills", skill), { recursive: true });
      writeFileSync(
        path.join(src, "skills", skill, "SKILL.md"),
        `---\nname: ${skill}\n---\n\n# Skill ${skill}\n`
      );
    }
    writeFileSync(path.join(src, "package.json"), '{"version":"9.9.9"}\n');
    writeFileSync(path.join(label, "built-from"), "checkout /x\n");
    // The transcript line the stand-in omp leaves, made here: the stand-in runs under the probe's
    // own `env -i`, where a jq on PATH may be a mise shim that cannot run.
    writeFileSync(
      path.join(bin, "probe.jsonl"),
      `${JSON.stringify({ message: { role: "toolResult", content: [{ type: "text", text: result }] } })}\n`
    );
    // `mise where` names the pin's install, whose bin/omp the probe runs by path; any other call is
    // the probe's `mise x <pin> -- <omp> -p …`.
    mkdirSync(path.join(bin, "pin", "bin"), { recursive: true });
    writeFileSync(path.join(bin, "pin", "bin", "omp"), "#!/bin/sh\n", { mode: 0o755 });
    writeFileSync(
      path.join(bin, "mise"),
      `#!/usr/bin/env bash
if [ "$1" = where ]; then echo "$(dirname "$0")/pin"; exit 0; fi
printf '%s\\n' "$@" >"$(dirname "$0")/argv.txt"
env >"$(dirname "$0")/env.txt"
while [ $# -gt 0 ] && [ "$1" != --session-dir ]; do shift; done
mkdir -p "$2"
cp "$(dirname "$0")/probe.jsonl" "$2/probe.jsonl"
echo done
`,
      { mode: 0o755 }
    );
    const run = () =>
      bash(
        `${fn("fail")}\n${fn("pinned_omp")}\n${fn("install_superpowers")}\nsuperpowers_skills=(writing-plans brainstorming)\ninstall_superpowers "$1" skills-s1 omp@pin`,
        [label],
        { PATH: `${bin}:${childEnv.PATH}`, SKILL_SCENARIOS_SUPERPOWERS: src, USER: "rig" }
      );
    return { bin, label, run };
  }

  // Catches: a profile step that copies the skills somewhere the pinned Oh My Pi does not read, or
  // gives the probe the operator's Envoy (the label's pi-envoy would register with it).
  test("copies both skills into the profile, probes with no Envoy, and marks the label", () => {
    const { bin, label, run } = setup("# Skill writing-plans\n\nbody");
    expect(run()).toMatchObject({ code: 0, stderr: "" });
    const skills = path.join(label, "home/.omp/profiles/skills-s1/agent/skills");
    expect(readFileSync(path.join(skills, "brainstorming/SKILL.md"), "utf8")).toContain(
      "# Skill brainstorming"
    );
    expect(existsSync(path.join(skills, "writing-plans/SKILL.md"))).toBe(true);
    expect(existsSync(path.join(label, "superpowers"))).toBe(true);
    const env = readFileSync(path.join(bin, "env.txt"), "utf8");
    expect(env).toContain("ENVOY_URL=http://127.0.0.1:1\n");
    expect(env).toContain("OMP_PROFILE=skills-s1\n");
    expect(env).toContain(`HOME=${label}/home\n`);
    expect(readFileSync(path.join(label, "built-from"), "utf8")).toContain(
      "superpowers-skill writing-plans "
    );
    // The probe names the pin's own binary, so no omp earlier on PATH stands in for it.
    expect(readFileSync(path.join(bin, "argv.txt"), "utf8")).toStartWith(
      `x\nomp@pin\n--\n${bin}/pin/bin/omp\n-p\n`
    );
  });

  // Catches: a probe that passes when Oh My Pi did not load the skill.
  test("fails when the read answers Unknown skill, and marks nothing", () => {
    const { label, run } = setup("Unknown skill: writing-plans\nAvailable: dispatch");
    const { code, stderr } = run();
    expect(code).toBe(1);
    expect(stderr).toContain("does not load skill://writing-plans (Unknown skill)");
    expect(existsSync(path.join(label, "superpowers"))).toBe(false);
  });

  // Catches: a probe that passes when the agent read nothing at all.
  test("fails when no tool result holds the skill's heading", () => {
    const { label, run } = setup("some other file");
    const { code, stderr } = run();
    expect(code).toBe(1);
    expect(stderr).toContain("read no skill://writing-plans");
    expect(existsSync(path.join(label, "superpowers"))).toBe(false);
  });
});

// rig.sh judge-omp, the whole script, in its Docker container (Docker is on the devbox and on CI's
// ubuntu-24.04 runners), with the real key-file route and a stand-in omp that records its argv and
// environment. SKILL_SCENARIOS_JUDGE_OMP puts the stand-in where the pinned omp's binary is
// mounted, so no test needs a model. The checkout's own skills/asd-ste100 is the ste skill.
describe("judge-omp", () => {
  const work = freshDir("judge-work");
  const outside = path.join(work, "outside.txt");
  writeFileSync(outside, "a file beside the judge's directory\n");
  mkdirSync(path.join(work, "runs"), { recursive: true });
  const omp = path.join(freshDir("judge-omp-bin"), "omp");
  // The stand-in writes everything through the one file the judge may write, answer.json: argv
  // (one word a line), then `env` lines, then one line per probe (`<name> allowed|denied`).
  writeFileSync(
    omp,
    `#!/bin/bash
probe() { if "\${@:2}" >/dev/null 2>&1; then echo "probe $1 allowed"; else echo "probe $1 denied"; fi; }
# DROP: write a marker into every directory this judge can write outside its own directory.
# SEEK: report any marker another judge left anywhere this judge can read.
if [ -n "\${DROP:-}" ]; then
  find / \\( -path /proc -o -path /sys -o -path /dev -o -path "$PWD" \\) -prune -o -type d -writable -print 2>/dev/null |
    while IFS= read -r d; do printf 'private input\\n' >"$d/leak-$DROP" 2>/dev/null; done
fi
{
  printf 'argv %s\\n' "$0" "$@"
  printf 'env %s\\n' "PWD=$PWD" "HOME=$HOME" "OMP_PROFILE=$OMP_PROFILE"
  env | grep -q LEGION_E2E && echo "env LEGION_E2E set"
  probe read-outside cat "$OUTSIDE"
  probe read-home-ssh ls "$OPERATOR_HOME/.ssh"
  probe list-work ls "$WORK/runs"
  probe read-own cat "$PWD/prompt.txt"
  probe write-own-dir touch "$PWD/new.txt"
  probe write-sessions touch "$PWD/sessions/s.jsonl"
  probe read-skill cat "$HOME/.omp/profiles/$OMP_PROFILE/agent/skills/asd-ste100/SKILL.md"
  probe read-models cat "$HOME/.omp/profiles/$OMP_PROFILE/agent/models.yml"
  key=$(sed -n 's/.*!exec \\(.*\\)"$/\\1/p' "$HOME/.omp/profiles/$OMP_PROFILE/agent/models.yml" | head -1)
  probe key-command test -x "$key"
  probe gateway-base-url grep -q 'baseUrl: https://gateway.internal.example/anthropic' "$HOME/.omp/profiles/$OMP_PROFILE/agent/models.yml"
  probe token-is-jwt bash -c '[[ "$("$1")" =~ ^eyJ[A-Za-z0-9_-]*\\.[A-Za-z0-9_-]+\\.[A-Za-z0-9_-]+$ ]]' - "$key"
  probe dbus-set test -n "\${DBUS_SESSION_BUS_ADDRESS:-}"
  [ -z "\${SEEK:-}" ] || echo "seek $(find / \\( -path /proc -o -path /sys -o -path /dev \\) -prune -o -name 'leak-*' -print 2>/dev/null | head -1)"
} >"$PWD/answer.json"
sleep "\${SLEEP:-0}"
echo judged
exit "$(cat "$PWD/exit-status" 2>/dev/null || echo 0)"
`,
    { mode: 0o755 }
  );
  const judge = (
    kind: string,
    dir: string,
    output = "answer.json",
    env: Record<string, string> = {}
  ) =>
    Bun.spawnSync(["bash", path.join(import.meta.dir, "rig.sh"), "judge-omp", kind, dir, output], {
      env: {
        ...childEnv,
        USER: "rig",
        SKILL_SCENARIOS_WORK: work,
        SKILL_SCENARIOS_JUDGE_OMP: omp,
        SKILL_SCENARIOS_JUDGE_ENV: `OUTSIDE=${outside} OPERATOR_HOME=${homedir()} WORK=${work}`,
        LEGION_E2E_MODEL_ROUTE: "key-file",
        LEGION_E2E_ANTHROPIC_KEY_FILE: keyFile(0o600),
        ...env,
      },
    });
  let jobs = 0;
  function judgeDir(): string {
    const dir = path.join(work, "judging", `job-${++jobs}`);
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, "prompt.txt"), "Write answer.json.\n");
    return dir;
  }
  /** The stand-in's record of one kind (`argv`, `env`, `probe`), from the judge's answer file. */
  const recorded = (dir: string, kind: string) =>
    readFileSync(path.join(dir, "answer.json"), "utf8")
      .split("\n")
      .filter((line) => line.startsWith(`${kind} `))
      .map((line) => line.slice(kind.length + 1));

  // Catches: a judge session with the label's plugins, the operator's skills or the default model,
  // run outside its own directory, or with no record of its model.
  test("ste runs omp in the directory on its prompt, no plugins, asd-ste100 alone, the pinned model", () => {
    const dir = judgeDir();
    const result = judge("ste", dir);
    expect({ code: result.exitCode, stderr: result.stderr.toString() }).toEqual({
      code: 0,
      stderr: "",
    });
    expect(recorded(dir, "argv")).toEqual([
      "/opt/omp/omp",
      "-p",
      "Write answer.json.",
      "--no-extensions",
      "--skills",
      "asd-ste100",
      "--model",
      "anthropic/claude-opus-4-8",
      "--thinking",
      "high",
      "--session-dir",
      `${dir}/sessions`,
    ]);
    expect(recorded(dir, "env")).toEqual([
      `PWD=${dir}`,
      "HOME=/home/judge",
      "OMP_PROFILE=judge-ste",
    ]);
    expect(readFileSync(path.join(dir, "out.txt"), "utf8")).toBe("judged\nexit=0\n");
    expect(readFileSync(path.join(dir, "judge-model.txt"), "utf8")).toBe(
      "model anthropic/claude-opus-4-8\nthinking high\n"
    );
    const skill = path.join(work, "judge-profiles/ste/home/.omp/profiles/judge-ste/agent/skills");
    expect(readFileSync(path.join(skill, "asd-ste100/SKILL.md"), "utf8")).toBe(
      readFileSync(path.join(root, "skills/asd-ste100/SKILL.md"), "utf8")
    );
    expect(
      readFileSync(
        path.join(work, "judge-profiles/ste/home/.omp/profiles/judge-ste/agent/models.yml"),
        "utf8"
      )
    ).toContain("https://api.anthropic.com");
  });

  // Catches: a judge that reads the run's transcript, other runs or the operator's files beside its
  // own directory (as a findings judge did, given no check), or that writes anything but its one
  // answer file and its sessions.
  test("a judge sees only its own directory, read-only, with its answer file and sessions writable", () => {
    const dir = judgeDir();
    expect(judge("ste", dir).exitCode).toBe(0);
    expect(Object.fromEntries(recorded(dir, "probe").map((line) => line.split(" ")))).toEqual({
      "read-outside": "denied",
      "read-home-ssh": "denied",
      "list-work": "denied",
      "read-own": "allowed",
      "write-own-dir": "denied",
      "write-sessions": "allowed",
      "read-skill": "allowed",
      "read-models": "allowed",
      "key-command": "allowed",
      "gateway-base-url": "denied",
      "token-is-jwt": "denied",
      "dbus-set": "denied",
    });
    expect(existsSync(path.join(dir, "new.txt"))).toBe(false);
  });

  // Catches: a bare judge that still loads skills, or a judge-omp that hides omp's failure.
  test("bare loads no skill, and judge-omp exits with omp's status", () => {
    const dir = judgeDir();
    writeFileSync(path.join(dir, "exit-status"), "3\n");
    const result = judge("bare", dir);
    expect(result.exitCode).toBe(3);
    expect(recorded(dir, "argv")).toContain("--no-skills");
    expect(recorded(dir, "argv")).not.toContain("--skills");
    expect(recorded(dir, "env")).toContain("OMP_PROFILE=judge-bare");
    expect(recorded(dir, "probe")).toContain("read-skill denied");
    expect(readFileSync(path.join(dir, "out.txt"), "utf8")).toBe("judged\nexit=3\n");
    expect(
      existsSync(path.join(work, "judge-profiles/bare/home/.omp/profiles/judge-bare/agent/skills"))
    ).toBe(false);
  });

  // Catches: a mount two judges share that one can write and the next can read, such as the key
  // command's call record, a path one judge could leave its private input in for another to read.
  // The first judge writes a marker into every directory it can write outside its own directory;
  // the second, of the same kind, looks for one anywhere it can read.
  test("one judge cannot leave a file that a second judge can read", () => {
    const first = judgeDir();
    const env = (word: string) => ({
      SKILL_SCENARIOS_JUDGE_ENV: `OUTSIDE=${outside} OPERATOR_HOME=${homedir()} WORK=${work} ${word}`,
    });
    expect(judge("ste", first, "answer.json", env("DROP=one")).exitCode).toBe(0);
    const second = judgeDir();
    expect(judge("ste", second, "answer.json", env("SEEK=1")).exitCode).toBe(0);
    expect(recorded(second, "seek")).toEqual([""]);
  });

  // Catches: a judge kind the rig has no profile for, a directory with no prompt, and an answer
  // file name that escapes the judge's directory or names another of its files.
  test("refuses another kind, a directory with no prompt.txt and a bad answer name", () => {
    expect(judge("plugins", judgeDir()).stderr.toString()).toContain("is ste or bare");
    expect(judge("ste", freshDir("judging-empty")).stderr.toString()).toContain(
      "has no prompt.txt"
    );
    for (const output of ["../answer.json", "sessions", "prompt.txt", "a/b.json", ""]) {
      const result = judge("ste", judgeDir(), output);
      expect(result.exitCode).not.toBe(0);
      expect(result.stderr.toString()).toContain("is not the name of an answer file");
    }
  });

  // The gateway route with the real installer and key command, and a stand-in hawk-token first on
  // PATH that mints a JWT and counts each mint. The key command needs the operator's home and
  // session bus, so it runs on the host; the container gets only the token it printed.
  describe("on the gateway route", () => {
    const gatewayWork = freshDir("judge-gateway-work");
    mkdirSync(path.join(gatewayWork, "runs"), { recursive: true });
    const fakeBin = freshDir("judge-gateway-bin");
    const mints = path.join(gatewayWork, "mints.log");
    writeFileSync(
      path.join(fakeBin, "hawk-token"),
      `#!/usr/bin/env bash
printf '%s\\n' "$$" >>"$FAKE_MINT_LOG"
payload=$(printf '{"exp":%s}' "$(($(date +%s) + 3600))" | base64 -w0 | tr '+/' '-_' | tr -d '=')
printf 'eyJhbGciOiJub25lIn0.%s.c2ln\\n' "$payload"
`,
      { mode: 0o755 }
    );
    const gatewayJudge = (kind: string, dir: string, env: Record<string, string> = {}) =>
      judge(kind, dir, "answer.json", {
        PATH: `${fakeBin}:${childEnv.PATH}`,
        SKILL_SCENARIOS_WORK: gatewayWork,
        SKILL_SCENARIOS_JUDGE_ENV: `OUTSIDE=${outside} OPERATOR_HOME=${homedir()} WORK=${gatewayWork}`,
        LEGION_E2E_MODEL_ROUTE: "gateway",
        LEGION_E2E_MODEL_GATEWAY_URL: "https://gateway.internal.example/anthropic",
        DBUS_SESSION_BUS_ADDRESS: "unix:path=/nonexistent/bus",
        FAKE_MINT_LOG: mints,
        ...env,
      });
    const mintCount = () =>
      existsSync(mints) ? readFileSync(mints, "utf8").split("\n").filter(Boolean).length : 0;
    function gatewayDir(): string {
      const dir = path.join(gatewayWork, "judging", `job-${++jobs}`);
      mkdirSync(dir, { recursive: true });
      writeFileSync(path.join(dir, "prompt.txt"), "Write answer.json.\n");
      return dir;
    }

    // Catches: a gateway judge that cannot start in its container, one that gets the operator's
    // home or session bus, one whose models.yml still names the host key command (which the
    // container cannot run), one that mints more than once per judge run, a token the judge can
    // read only by a path outside its mounts, and a token left on disk once the judge ends.
    test("a gateway judge starts in the container on a token the host minted once, and the token file is gone after", () => {
      // The first judge-omp call builds both profiles, and each install's preflight mints once.
      // Count only a later call's mints: one judge run is one call of the key command.
      expect(gatewayJudge("bare", gatewayDir()).exitCode).toBe(0);
      const dir = gatewayDir();
      const before = mintCount();
      const result = gatewayJudge("ste", dir);
      expect({ code: result.exitCode, stderr: result.stderr.toString() }).toEqual({
        code: 0,
        stderr: "",
      });
      expect(mintCount() - before).toBeLessThanOrEqual(1);
      expect(recorded(dir, "env")).toEqual([
        `PWD=${dir}`,
        "HOME=/home/judge",
        "OMP_PROFILE=judge-ste",
      ]);
      expect(Object.fromEntries(recorded(dir, "probe").map((line) => line.split(" ")))).toEqual({
        "read-outside": "denied",
        "read-home-ssh": "denied",
        "list-work": "denied",
        "read-own": "allowed",
        "write-own-dir": "denied",
        "write-sessions": "allowed",
        "read-skill": "allowed",
        "read-models": "allowed",
        "key-command": "allowed",
        "gateway-base-url": "allowed",
        "token-is-jwt": "allowed",
        "dbus-set": "denied",
      });
      expect(existsSync(`${dir}.key`)).toBe(false);
      expect(readFileSync(path.join(dir, "answer.json"), "utf8")).not.toMatch(/eyJ[\w-]*\./);
    });

    // Catches: a token directory or file that others can read while the judge runs. The stand-in
    // omp sleeps 2 s, so a watcher sees the files while they exist.
    test("the token file is the owner's alone while the judge runs", () => {
      const dir = gatewayDir();
      const seen = path.join(gatewayWork, `seen-mode-${jobs}`);
      const watcher = Bun.spawn(
        [
          "bash",
          "-c",
          'for _ in $(seq 400); do [ -f "$1/token" ] && { stat -c %a "$1" "$1/token" >"$2"; exit 0; }; sleep 0.05; done',
          "-",
          `${dir}.key`,
          seen,
        ],
        { stdout: "ignore", stderr: "ignore" }
      );
      const result = gatewayJudge("bare", dir, {
        SKILL_SCENARIOS_JUDGE_ENV: `OUTSIDE=${outside} OPERATOR_HOME=${homedir()} WORK=${gatewayWork} SLEEP=2`,
      });
      watcher.kill();
      expect(result.exitCode).toBe(0);
      expect(readFileSync(seen, "utf8").split("\n").filter(Boolean)).toEqual(["700", "600"]);
      expect(existsSync(`${dir}.key`)).toBe(false);
    });

    // Catches: a token file left behind when the judge fails, and a failed mint that starts the
    // judge anyway.
    test("removes the token file when the judge fails, and starts no judge when the key command fails", () => {
      const dir = gatewayDir();
      writeFileSync(path.join(dir, "exit-status"), "3\n");
      expect(gatewayJudge("bare", dir).exitCode).toBe(3);
      expect(existsSync(`${dir}.key`)).toBe(false);
      const refused = gatewayDir();
      const broken = freshDir("judge-gateway-broken");
      writeFileSync(path.join(broken, "hawk-token"), "#!/usr/bin/env bash\nexit 1\n", {
        mode: 0o755,
      });
      rmSync(path.join(gatewayWork, "judge-profiles/cache"), { recursive: true, force: true });
      for (const kind of ["ste", "bare"])
        rmSync(path.join(gatewayWork, `judge-profiles/${kind}/cache`), {
          recursive: true,
          force: true,
        });
      const result = gatewayJudge("ste", refused, { PATH: `${broken}:${childEnv.PATH}` });
      expect(result.exitCode).toBe(1);
      expect(result.stderr.toString()).toContain(
        "judge-omp: the gateway's key command gave no key"
      );
      expect(existsSync(path.join(refused, "answer.json"))).toBe(false);
    });
  });

  // Catches: a judge profile whose built-from records an empty commit line when the rig's root is
  // no checkout, so a judge's model run cannot be traced to the code that built it.
  test("fails, naming the rig's root, when the root is no checkout", () => {
    const top = freshDir("judge-root");
    const scripts = path.join(top, "packages/pi-legion/scripts/skill-scenarios");
    mkdirSync(scripts, { recursive: true });
    writeFileSync(path.join(scripts, "rig.sh"), readFileSync(path.join(import.meta.dir, "rig.sh")));
    for (const file of [".omp-pin", "skills/asd-ste100/SKILL.md"]) {
      mkdirSync(path.dirname(path.join(top, file)), { recursive: true });
      writeFileSync(path.join(top, file), readFileSync(path.join(root, file)));
    }
    mkdirSync(path.join(top, "scripts/e2e"), { recursive: true });
    cpSync(path.join(root, "scripts/e2e/lib"), path.join(top, "scripts/e2e/lib"), {
      recursive: true,
    });
    const rootWork = freshDir("judge-root-work");
    const result = Bun.spawnSync(
      ["bash", path.join(scripts, "rig.sh"), "judge-omp", "ste", judgeDir(), "answer.json"],
      {
        env: {
          ...childEnv,
          USER: "rig",
          SKILL_SCENARIOS_WORK: rootWork,
          SKILL_SCENARIOS_JUDGE_OMP: omp,
          LEGION_E2E_MODEL_ROUTE: "key-file",
          LEGION_E2E_ANTHROPIC_KEY_FILE: keyFile(0o600),
          GIT_CEILING_DIRECTORIES: path.dirname(top),
        },
      }
    );
    expect(result.exitCode).toBe(1);
    expect(result.stderr.toString()).toContain(`cannot read the commit of ${top} with jj or git`);
    expect(existsSync(path.join(rootWork, "judge-profiles/ready"))).toBe(false);
  });
});

// A spec run's plan row reads skill://writing-plans, which only a label built with
// SKILL_SCENARIOS_SUPERPOWERS holds (cmd_profile marks it with a `superpowers` file).
describe("require_superpowers", () => {
  const check = (label: string) =>
    bash(`${fn("fail")}\n${fn("require_superpowers")}\nP=$1\nrequire_superpowers spec-writing`, [
      label,
    ]);

  // Catches: a spec run on a label with no writing-plans, whose plan row would then fail for the
  // rig's fault and not the skill's.
  test("refuses a label built without SKILL_SCENARIOS_SUPERPOWERS, naming the variable", () => {
    const label = freshDir("label");
    const { code, stderr } = check(label);
    expect(code).toBe(1);
    expect(stderr).toContain("SKILL_SCENARIOS_SUPERPOWERS");
    writeFileSync(path.join(label, "superpowers"), "");
    expect(check(label).code).toBe(0);
  });
});

// launch with a stand-in tmux that starts the pane's command a moment later, as a real tmux server
// does, and a stand-in mise that prints the omp command line it was given.
describe("launch", () => {
  // Catches: a later turn that reads the previous turn's `exit=` line as its own and returns at
  // once, and extra omp arguments placed anywhere but after the prompt.
  test("a later turn passes its arguments to omp and waits for its own exit line", () => {
    const run = freshDir("launch-run");
    const bin = freshDir("launch-bin");
    for (const dir of ["cwd", "sessions"]) mkdirSync(path.join(run, dir));
    writeFileSync(path.join(run, "prompt.txt"), "The person answered your asks on LWEVAL-2.\n");
    writeFileSync(path.join(run, "pane.env"), `PATH=${bin}:${process.env.PATH ?? ""}\n`);
    writeFileSync(path.join(run, "out.txt"), "the first turn\nexit=0\n");
    mkdirSync(path.join(bin, "pin", "bin"), { recursive: true });
    writeFileSync(path.join(bin, "pin", "bin", "omp"), "#!/bin/sh\n", { mode: 0o755 });
    writeFileSync(
      path.join(bin, "mise"),
      `#!/bin/sh\nif [ "$1" = where ]; then echo "${bin}/pin"; exit 0; fi\nshift 3\necho "$@"\n`,
      { mode: 0o755 }
    );
    const standIns = `
tmux() { case $1 in new-session) (command sleep 0.3 && bash -c "\${@: -1}") & ;; esac; }
sleep() { command sleep 0.05; }`;
    const result = bash(
      `${fn("fail")}\n${fn("pinned_omp")}\n${fn("launch")}\n${standIns}\nR=$1 tag=t pin=omp@pin\nlaunch spec-writing-s1-1 --continue\ncp "$R/out.txt" "$R/seen"`,
      [run],
      { PATH: `${bin}:${childEnv.PATH}` }
    );
    expect(result.code).toBe(0);
    expect(readFileSync(path.join(run, "seen"), "utf8")).toBe(
      `${bin}/pin/bin/omp -p The person answered your asks on LWEVAL-2. --continue --session-dir ${run}/sessions\nexit=0\n`
    );
  });
});

// A batch's Postgres and its NATS are the servers SKILL_SCENARIOS_POSTGRES_URL and
// SKILL_SCENARIOS_NATS_URL name when both are set, and otherwise two containers it starts. The
// harness gives services_up a docker that records its command line and answers `docker port`, so
// the container branch runs where there is no docker, and a pg_sql that answers every query with
// 100, the max_connections of a server that admits 100.
describe("services_up and services_down", () => {
  const stubs = `
until_true() { shift 2; "$@"; }
pg_sql() { printf '%s\\n' 100; }
nats_ready() { :; }
start_listener() { envoy_port=4100; }
go() { printf 'go %s\\n' "$*" >>"$S/go-calls"; }
docker() {
  printf 'docker %s\\n' "$*" >>"$S/docker-calls"
  case $1 in port) case $2 in *-pg) echo 127.0.0.1:55432 ;; *) echo 127.0.0.1:54222 ;; esac ;; esac
}`;
  const given = {
    SKILL_SCENARIOS_POSTGRES_URL: "postgres://rig:secret@127.0.0.1:5432/postgres?sslmode=disable",
    SKILL_SCENARIOS_NATS_URL: "nats://127.0.0.1:4222",
  };
  function up(env: Record<string, string>) {
    const S = freshDir("services-up");
    const result = bash(
      `${fn("note")}\n${fn("fail")}\n${fn("given_services")}\n${fn("safe_url")}\n${fn("services_up")}\n${stubs}\nS=$1 root=$2 tag=rig-test\nservices_env=$S/env\nservices_postgres=$S/postgres-url\nservices_up`,
      [S, root],
      env
    );
    return { S, ...result };
  }

  // Catches: a batch that starts a container although the operator gave it both servers, which is
  // every batch in a Legion pod and in a builds Job, where there is no docker at all.
  test("both URLs set: no container, the Postgres recorded 0600 for each run, the given NATS in the services env", () => {
    const { S, code, stdout, stderr } = up({ ...given, SKILL_SCENARIOS_PARALLEL: "2" });
    expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
    expect(existsSync(path.join(S, "docker-calls"))).toBe(false);
    expect(readFileSync(path.join(S, "postgres-url"), "utf8")).toBe(
      `${given.SKILL_SCENARIOS_POSTGRES_URL}\n`
    );
    expect(statSync(path.join(S, "postgres-url")).mode & 0o777).toBe(0o600);
    expect(readFileSync(path.join(S, "env"), "utf8")).toContain(
      "ENVOY_NATS_URL=nats://127.0.0.1:4222\n"
    );
    // The listener is still built from the checkout.
    expect(readFileSync(path.join(S, "go-calls"), "utf8")).toContain("build -o");
    // No message prints the password.
    expect(stdout).toContain("Postgres postgres://rig@127.0.0.1:5432/postgres?sslmode=disable");
    expect(stdout).not.toContain("secret");
  });

  // Catches: a batch that starts its runs against a given server with too few connections, where
  // each run's Dispatch server takes 30 and the run that cannot get them fails for the rig's fault.
  test("a parallelism the given Postgres cannot serve is refused, naming both numbers", () => {
    const { S, code, stderr } = up({ ...given, SKILL_SCENARIOS_PARALLEL: "3" });
    expect(code).toBe(1);
    expect(stderr).toContain("needs 110 connections");
    expect(stderr).toContain("which admits 100");
    expect(stderr).toContain("set SKILL_SCENARIOS_PARALLEL to 2 or lower");
    // It refuses before it builds anything.
    expect(existsSync(path.join(S, "go-calls"))).toBe(false);
  });

  // Catches: a batch on a box with docker that takes one of the two URLs, or none, and stops
  // starting its containers.
  test("neither URL, or one without the other: the two containers, and no Postgres recorded", () => {
    const none: Record<string, string> = {};
    for (const env of [none, { SKILL_SCENARIOS_NATS_URL: given.SKILL_SCENARIOS_NATS_URL }]) {
      const { S, code, stdout } = up({ ...env, SKILL_SCENARIOS_PARALLEL: "1" });
      expect(code).toBe(0);
      const calls = readFileSync(path.join(S, "docker-calls"), "utf8");
      expect(calls).toContain("--name rig-test-pg");
      expect(calls).toContain("max_connections=50");
      expect(calls).toContain("--name rig-test-nats");
      expect(existsSync(path.join(S, "postgres-url"))).toBe(false);
      expect(readFileSync(path.join(S, "env"), "utf8")).toContain(
        "ENVOY_NATS_URL=nats://127.0.0.1:54222\n"
      );
      expect(stdout).toContain(
        "Postgres postgres://postgres@127.0.0.1:55432/postgres?sslmode=disable"
      );
    }
  });

  function down(env: Record<string, string>) {
    const S = freshDir("services-down");
    for (const file of ["env", "postgres-url", "up"]) writeFileSync(path.join(S, file), "");
    const result = bash(
      `${fn("given_services")}\n${fn("services_down")}\nstop_tree() { printf 'stop_tree %s\\n' "$*" >>"$S/stopped"; }\ndocker() { printf 'docker %s\\n' "$*" >>"$S/docker-calls"; }\nS=$1 tag=rig-test\nservices_env=$S/env\nservices_postgres=$S/postgres-url\nlistener_pid=4242\nservices_down`,
      [S],
      env
    );
    return { S, ...result };
  }

  // Catches: a teardown that removes a server it was given, or the databases on it.
  test("both URLs set: the listener stops, the batch's own files go, and no docker runs", () => {
    const { S, code } = down(given);
    expect(code).toBe(0);
    expect(existsSync(path.join(S, "docker-calls"))).toBe(false);
    expect(readFileSync(path.join(S, "stopped"), "utf8")).toBe("stop_tree 4242\n");
    for (const file of ["env", "postgres-url", "up"]) {
      expect(existsSync(path.join(S, file))).toBe(false);
    }
  });

  // Catches: a teardown that leaves the batch's own containers running.
  test("neither URL: the batch's two containers are removed", () => {
    const { S, code } = down({});
    expect(code).toBe(0);
    expect(readFileSync(path.join(S, "docker-calls"), "utf8")).toBe(
      "docker rm -fv rig-test-nats rig-test-pg\n"
    );
  });
});

// The probes services_up waits on: one for a given server as for a started container.
describe("nats_ready and the URL helpers", () => {
  /** A server on 127.0.0.1 that writes TEXT to every connection, as a process of its own: bash()
   * runs synchronously, so a listener in this process would never reach its accept. */
  async function greeting(text: string) {
    const child = Bun.spawn(
      [
        "bun",
        "-e",
        'const server = Bun.listen({ hostname: "127.0.0.1", port: 0, socket: { open: (socket) => void socket.write(process.env.GREETING ?? ""), data: () => {} } });\nconsole.log(server.port);',
      ],
      { env: { ...childEnv, GREETING: text }, stdout: "pipe", stderr: "inherit" }
    );
    const reader = child.stdout.getReader();
    const { value } = await reader.read();
    reader.releaseLock();
    return {
      port: Number(new TextDecoder().decode(value).trim()),
      stop: async () => {
        child.kill();
        await child.exited;
      },
    };
  }

  // Catches: a probe that takes any process holding the port for a NATS, so a batch starts against
  // a dead one.
  test("the INFO line a NATS sends on connect, and nothing else, is ready", async () => {
    const probe = (port: number) =>
      bash(`${fn("nats_ready")}\nnats_ready "nats://127.0.0.1:$1"`, [String(port)]).code;
    const nats = await greeting('INFO {"server_id":"rig"}\r\n');
    const other = await greeting("+OK\r\n");
    try {
      expect(probe(nats.port)).toBe(0);
      expect(probe(other.port)).toBe(1);
    } finally {
      await nats.stop();
      await other.stop();
    }
    // Port 1, where nothing listens, is the rig's own address for that.
    expect(probe(1)).toBe(1);
  });

  // Catches: a message that prints the password of a server the operator gave, and a run's
  // database URL that drops the query a given URL carries (sslmode, say).
  test("safe_url drops the password, pg_database_url swaps the database and keeps the query", () => {
    const { stdout } = bash(
      `${fn("safe_url")}\n${fn("pg_database_url")}\nsafe_url "$1"\npg_database_url "$1" run_x\npg_database_url "$2" run_x`,
      [
        "postgres://rig:secret@db.example:5432/postgres?sslmode=disable",
        "postgres://127.0.0.1:5432",
      ]
    );
    expect(stdout).toBe(
      "postgres://rig@db.example:5432/postgres?sslmode=disable\n" +
        "postgres://rig:secret@db.example:5432/run_x?sslmode=disable\n" +
        "postgres://127.0.0.1:5432/run_x\n"
    );
  });
});
