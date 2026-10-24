import { afterAll, describe, expect, setDefaultTimeout, test } from "bun:test";
import {
  cpSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
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
function route(dir: string, env: Record<string, string>) {
  const result = Bun.spawnSync(
    [
      "bash",
      "-c",
      `${fn("install_model_route")}\ninstall_model_route "$1" skills-test "$2"`,
      "-",
      root,
      dir,
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

  test("an unknown route fails, naming the two it takes", () => {
    const dir = profileDir();
    const { code, stderr } = route(dir, { LEGION_E2E_MODEL_ROUTE: "bedrock" });
    expect(code).toBe(1);
    expect(stderr).toContain("it is gateway or key-file");
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
    cpSync(
      path.join(root, "scripts/e2e/lib/install-key-file-model-route.sh"),
      path.join(co, "scripts/e2e/lib/install-key-file-model-route.sh")
    );
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
    writeFileSync(
      path.join(bin, "mise"),
      `#!/usr/bin/env bash
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
        `${fn("fail")}\n${fn("install_superpowers")}\nsuperpowers_skills=(writing-plans brainstorming)\ninstall_superpowers "$1" skills-s1 omp@pin`,
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

// rig.sh judge-omp, the whole script, with a stand-in mise that records omp's argv, its cwd and its
// environment, and the real key-file route. The checkout's own skills/asd-ste100 is the ste skill.
describe("judge-omp", () => {
  const bin = freshDir("judge-bin");
  writeFileSync(
    path.join(bin, "mise"),
    `#!/usr/bin/env bash
shift 3
{ printf '%s\\n' "$@"; } >"$PWD/argv.txt"
env >"$PWD/env.txt"
echo "judged"
exit "$(cat "$PWD/exit-status" 2>/dev/null || echo 0)"
`,
    { mode: 0o755 }
  );
  const work = freshDir("judge-work");
  const judge = (kind: string, dir: string, env: Record<string, string> = {}) =>
    Bun.spawnSync(["bash", path.join(import.meta.dir, "rig.sh"), "judge-omp", kind, dir], {
      env: {
        ...childEnv,
        PATH: `${bin}:${childEnv.PATH}`,
        USER: "rig",
        SKILL_SCENARIOS_WORK: work,
        LEGION_E2E_MODEL_ROUTE: "key-file",
        LEGION_E2E_ANTHROPIC_KEY_FILE: keyFile(0o600),
        ...env,
      },
    });
  function promptDir(): string {
    const dir = freshDir("judging");
    writeFileSync(path.join(dir, "prompt.txt"), "Write faults.json.\n");
    return dir;
  }

  // Catches: a judge session with the label's plugins, the operator's skills or the default model,
  // run outside its own directory, or with no record of its model.
  test("ste runs omp in the directory on its prompt, no plugins, asd-ste100 alone, the pinned model", () => {
    const dir = promptDir();
    const result = judge("ste", dir);
    expect({ code: result.exitCode, stderr: result.stderr.toString() }).toEqual({
      code: 0,
      stderr: "",
    });
    expect(readFileSync(path.join(dir, "argv.txt"), "utf8").split("\n")).toEqual([
      "omp",
      "-p",
      "Write faults.json.",
      "--no-extensions",
      "--skills",
      "asd-ste100",
      "--model",
      "anthropic/claude-opus-4-8",
      "--thinking",
      "high",
      "--session-dir",
      `${dir}/sessions`,
      "",
    ]);
    const env = readFileSync(path.join(dir, "env.txt"), "utf8");
    expect(env).toContain(`PWD=${dir}\n`);
    expect(env).toContain("OMP_PROFILE=judge-ste\n");
    expect(env).toContain(`HOME=${work}/judge-profiles/ste/home\n`);
    expect(env).not.toContain("LEGION_E2E_ANTHROPIC_KEY_FILE");
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

  // Catches: a bare judge that still loads skills, or a judge-omp that hides omp's failure.
  test("bare loads no skill, and judge-omp exits with omp's status", () => {
    const dir = promptDir();
    writeFileSync(path.join(dir, "exit-status"), "3\n");
    const result = judge("bare", dir);
    expect(result.exitCode).toBe(3);
    expect(readFileSync(path.join(dir, "argv.txt"), "utf8")).toContain("--no-skills\n");
    expect(readFileSync(path.join(dir, "argv.txt"), "utf8")).not.toContain("--skills\n");
    expect(readFileSync(path.join(dir, "env.txt"), "utf8")).toContain("OMP_PROFILE=judge-bare\n");
    expect(readFileSync(path.join(dir, "out.txt"), "utf8")).toBe("judged\nexit=3\n");
    expect(
      existsSync(path.join(work, "judge-profiles/bare/home/.omp/profiles/judge-bare/agent/skills"))
    ).toBe(false);
  });

  // Catches: a judge kind the rig has no profile for, or a directory with no prompt.
  test("refuses another kind and a directory with no prompt.txt", () => {
    expect(judge("plugins", promptDir()).stderr.toString()).toContain("is ste or bare");
    expect(judge("ste", freshDir("judging-empty")).stderr.toString()).toContain(
      "has no prompt.txt"
    );
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
      ["bash", path.join(scripts, "rig.sh"), "judge-omp", "ste", promptDir()],
      {
        env: {
          ...childEnv,
          PATH: `${bin}:${childEnv.PATH}`,
          USER: "rig",
          SKILL_SCENARIOS_WORK: rootWork,
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
    writeFileSync(path.join(bin, "mise"), '#!/bin/sh\nshift 3\necho "$@"\n', { mode: 0o755 });
    const standIns = `
tmux() { case $1 in new-session) (command sleep 0.3 && bash -c "\${@: -1}") & ;; esac; }
sleep() { command sleep 0.05; }`;
    const result = bash(
      `${fn("launch")}\n${standIns}\nR=$1 tag=t pin=omp@pin\nlaunch spec-writing-s1-1 --continue\ncp "$R/out.txt" "$R/seen"`,
      [run]
    );
    expect(result.code).toBe(0);
    expect(readFileSync(path.join(run, "seen"), "utf8")).toBe(
      `omp -p The person answered your asks on LWEVAL-2. --continue --session-dir ${run}/sessions\nexit=0\n`
    );
  });
});
