import { afterEach, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";

// GitHub answers the pull request's head as the commit its branch points at, and the worker skill
// has every pushing role read that head after its own push and stop when it is not its commit. A
// stand-in that answers the fixture's first commit for ever stalls every such role: measured on
// 2026-11-06, two runs of the loop that follows an approval pushed their text fixes, read the
// head, found the fixture's
// commit and ended with a WAITING line instead of completing. So the stand-in reads the tip of the
// run's own bare remote at the call, and serves the pull request's diff from it too.
const STANDIN = path.join(import.meta.dir, "gh-standin.ts");
const made: string[] = [];

afterEach(() => {
  for (const dir of made.splice(0)) rmSync(dir, { recursive: true, force: true });
});

function git(dir: string, ...args: string[]): string {
  const result = Bun.spawnSync(
    ["git", "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-C", dir, ...args],
    { env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null" } }
  );
  if (result.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${result.stderr.toString()}`);
  return result.stdout.toString().trim();
}

/** A run directory with the pull-request route of rig.sh's worker_fixture, and, unless `remote` is
 * false, a bare remote of its own whose issue branch holds two commits. */
function runDir(options: { remote: boolean }): { dir: string; first: string; second: string } {
  const dir = mkdtempSync(path.join(tmpdir(), "gh-standin-"));
  made.push(dir);
  const work = path.join(dir, "work");
  mkdirSync(work);
  git(work, "init", "-q", "-b", "legion/LWEVAL-1");
  writeFileSync(path.join(work, "greet.ts"), "export const greet = () => 1;\n");
  git(work, "add", "-A");
  git(work, "commit", "-qm", "the approved head");
  const first = git(work, "rev-parse", "HEAD");
  writeFileSync(path.join(work, "docs.md"), "the text fix\n");
  git(work, "add", "-A");
  git(work, "commit", "-qm", "docs: the text fix");
  const second = git(work, "rev-parse", "HEAD");
  git(work, "update-ref", "refs/heads/legion/LWEVAL-1", first);
  if (options.remote) {
    git(dir, "clone", "-q", "--bare", work, path.join(dir, "remote.git"));
    git(path.join(dir, "remote.git"), "update-ref", "refs/heads/legion/LWEVAL-1", first);
  }
  writeFileSync(
    path.join(dir, "world.json"),
    JSON.stringify({ key: "LWEVAL-1", branch: "legion/LWEVAL-1", base: "main", head: first })
  );
  writeFileSync(
    path.join(dir, "fixtures.json"),
    JSON.stringify({
      routes: [
        {
          match: "^gh pr view",
          stdout: { number: 1001, headRefOid: first, head: { sha: first }, body: "a body" },
        },
        { match: "^gh pr diff", stdout: "the frozen diff\n" },
      ],
    })
  );
  return { dir, first, second };
}

const headOf = (dir: string) =>
  Bun.spawnSync(
    ["bun", STANDIN, "gh", "pr", "view", "1001", "--json", "headRefOid", "--jq", ".headRefOid"],
    {
      env: { ...process.env, SKILL_SCENARIO_RUN: dir },
    }
  )
    .stdout.toString()
    .trim();

// Catches: a stand-in that answers the fixture's commit after the role pushed, so every pushing
// role reads a head that is not its own and stops.
test("the pull request's head is the tip of the run's own remote", () => {
  const { dir, first, second } = runDir({ remote: true });
  expect(headOf(dir)).toBe(first);
  git(path.join(dir, "remote.git"), "update-ref", "refs/heads/legion/LWEVAL-1", second);
  expect(headOf(dir)).toBe(second);
});

// Catches: a stand-in that refuses or empties the answer for a run with no remote of its own,
// which every scenario without a worker fixture is.
test("a run with no remote keeps the route's own head", () => {
  const { dir, first } = runDir({ remote: false });
  expect(headOf(dir)).toBe(first);
});

/** What the stand-in answers for `gh <args...>` in run directory `dir`, as `by` if the run names
 * an agent. */
const answer = (dir: string, args: string[], by?: string) =>
  Bun.spawnSync(["bun", STANDIN, "gh", ...args], {
    env: {
      ...process.env,
      SKILL_SCENARIO_RUN: dir,
      ...(by === undefined ? {} : { SKILL_SCENARIO_AGENT: by }),
    },
  })
    .stdout.toString()
    .trim();

// Catches: a stand-in that answers the diff the fixture froze after another role pushed to the
// pull request, so a reviewer reading the fix the implementer pushed sees the change without it
// and resolves nothing. The run's own remote is the fact, as it is for the head.
test("the pull request's diff is the diff of the run's own remote", () => {
  const { dir, second } = runDir({ remote: true });
  const remote = path.join(dir, "remote.git");
  git(
    remote,
    "update-ref",
    "refs/heads/main",
    git(remote, "rev-parse", "refs/heads/legion/LWEVAL-1")
  );
  expect(answer(dir, ["pr", "diff", "1001"])).toBe("");
  git(remote, "update-ref", "refs/heads/legion/LWEVAL-1", second);
  expect(answer(dir, ["pr", "diff", "1001"])).toContain("+the text fix");
  expect(answer(dir, ["pr", "diff", "1001", "--name-only"])).toBe("docs.md");
});

// Catches: a stand-in that drops the run's world for a scenario that builds none, which would
// crash every `gh pr diff` of a run with no world.json instead of answering its route.
test("a run with no world keeps the route's own diff", () => {
  const { dir } = runDir({ remote: false });
  rmSync(path.join(dir, "world.json"));
  expect(answer(dir, ["pr", "diff", "1001"])).toBe("the frozen diff");
});

// Catches: a stand-in that records no agent, so a run that boots one agent per role cannot tell
// which role made a call and the score reads one role's work as the other's.
test("each call records the agent the run named, and none when it names one", () => {
  const { dir } = runDir({ remote: true });
  answer(dir, ["pr", "view", "1001"], "implementer");
  answer(dir, ["pr", "view", "1001"]);
  const recorded = readFileSync(path.join(dir, "calls.jsonl"), "utf8")
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line).by);
  expect(recorded).toEqual(["implementer", undefined]);
});
