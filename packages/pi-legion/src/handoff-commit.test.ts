import { afterEach, expect, test } from "bun:test";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import * as path from "node:path";
import { findHandoffCommit, type JjRunner, pathJj } from "./handoff-commit";

const ISSUE = "LEGION-208";
const FILE = ".legion/LEGION-208/implement.json";
const FILESET = `root:"${FILE}"`;
const CARRYING = "210d53a9d1b109df97fbb5dd5041d659dbab1323";
const STANDING = "c0de0000000000000000000000000000000000ff";

const env = {
  LEGION_WORKSPACE: "/workspaces/LEGION-208",
  LEGION_ISSUE: ISSUE,
  JJ_USER: "legion-implementer[bot]",
  JJ_EMAIL: "271566630+legion-implementer[bot]@users.noreply.github.com",
};

/** What the fake jj answers each of the lookup's commands: keyed by the command's shape, with the
 * success path's answers unless a case overrides one. */
interface Answers {
  readonly uncommitted?: string;
  readonly listed?: string;
  readonly carrying?: string;
  readonly author?: string;
  readonly pushed?: string;
}

/** A jj that answers the lookup's commands from `answers` and records each call's args. */
function fakeJj(answers: Answers = {}): { jj: JjRunner; calls: string[][] } {
  const calls: string[][] = [];
  const jj: JjRunner = async (args, cwd) => {
    expect(cwd).toBe(env.LEGION_WORKSPACE);
    calls.push([...args]);
    const [command, sub, revision] = args;
    if (command === "log" && sub === "-r" && revision === "@-") return STANDING;
    if (command === "diff") return answers.uncommitted ?? "";
    if (command === "file") return answers.listed ?? FILE;
    if (command === "log" && revision?.startsWith("latest(")) return answers.carrying ?? CARRYING;
    if (command === "log" && revision === CARRYING) {
      return answers.author ?? `${env.JJ_USER}\n${env.JJ_EMAIL}`;
    }
    if (command === "log" && revision?.includes("remote_bookmarks")) {
      return answers.pushed ?? CARRYING;
    }
    throw new Error(`unexpected jj ${args.join(" ")}`);
  };
  return { jj, calls };
}

test("a file-backed phase its role works reports the pushed commit carrying its handoff", async () => {
  const { jj, calls } = fakeJj();
  await expect(
    findHandoffCommit({ phase: "implementing", role: "implementer", env, jj })
  ).resolves.toBe(CARRYING);
  // The spec's commands, in order: no uncommitted change, the file present, the newest commit on
  // this branch carrying it, that commit's author, and that commit on legion/<issue>@origin.
  expect(calls).toEqual([
    ["diff", "-r", "@", "--name-only", FILESET],
    ["file", "list", "-r", "@", FILESET],
    [
      "log",
      "-r",
      `latest((::@- ~ ::trunk()) & files(${FILESET}))`,
      "--no-graph",
      "-T",
      "commit_id",
    ],
    ["log", "-r", CARRYING, "--no-graph", "-T", 'author.name() ++ "\\n" ++ author.email()'],
    [
      "log",
      "-r",
      `${CARRYING} & ::remote_bookmarks(exact:"legion/LEGION-208", exact:"origin")`,
      "--no-graph",
      "-T",
      "commit_id",
    ],
  ]);
});

test("each phase's word and role mirror the daemon's tables", async () => {
  for (const [phase, role, word] of [
    ["planning", "planner", "plan"],
    ["implementing", "implementer", "implement"],
    ["testing", "tester", "test"],
    ["reviewing", "reviewer", "review"],
  ] as const) {
    const { jj, calls } = fakeJj();
    await findHandoffCommit({ phase, role, env, jj });
    expect(calls[0]).toEqual([
      "diff",
      "-r",
      "@",
      "--name-only",
      `root:".legion/LEGION-208/${word}.json"`,
    ]);
  }
});

test("every other completion reports the commit the workspace stands on", async () => {
  // Retro, the production check and the merger's READY write no file; a role reporting a phase
  // it does not run reports @- too, and the daemon refuses it naming whose phase it is.
  for (const [phase, role] of [
    ["retro", "implementer"],
    ["production_check", "implementer"],
    ["merging", "merger"],
    ["implementing", "tester"],
    ["admitted", "architect"],
  ] as const) {
    const { jj, calls } = fakeJj();
    await expect(findHandoffCommit({ phase, role, env, jj })).resolves.toBe(STANDING);
    expect(calls).toEqual([["log", "-r", "@-", "--no-graph", "-T", "commit_id"]]);
  }
});

test("each refusal names the file and its remedy, and makes no daemon-facing guess", async () => {
  const refusals: ReadonlyArray<readonly [Answers, string]> = [
    [
      { uncommitted: FILE },
      `${FILE} has changes in the working copy that are not committed: commit this phase's handoff before completing`,
    ],
    [
      { listed: "" },
      `${FILE} is missing from the workspace: write this phase's handoff, then commit and push it before completing`,
    ],
    [
      { carrying: "" },
      `${FILE} is not committed on this issue's branch (only the base branch carries it): write and commit this phase's handoff`,
    ],
    [
      { author: "legion-reviewer[bot]\n271566631+legion-reviewer[bot]@users.noreply.github.com" },
      `${FILE} is carried by commit ${CARRYING}, authored by legion-reviewer[bot] <271566631+legion-reviewer[bot]@users.noreply.github.com>, not this pane's legion-implementer[bot]: run jj new, then write and commit this phase's handoff again`,
    ],
    [
      { pushed: "" },
      `${FILE} is carried by commit ${CARRYING}, which is not on legion/LEGION-208@origin: push the issue branch, then complete again`,
    ],
  ];
  for (const [answers, message] of refusals) {
    const { jj } = fakeJj(answers);
    await expect(
      findHandoffCommit({ phase: "implementing", role: "implementer", env, jj })
    ).rejects.toThrow(message);
  }
});

test("a pane without an identity skips the author check", async () => {
  const { jj, calls } = fakeJj({ author: "someone\nelse@example.invalid" });
  await expect(
    findHandoffCommit({
      phase: "implementing",
      role: "implementer",
      env: { ...env, JJ_USER: undefined, JJ_EMAIL: undefined },
      jj,
    })
  ).resolves.toBe(CARRYING);
  expect(calls.map((call) => call[0])).toEqual(["diff", "file", "log", "log"]);
});

test("the workspace and a well-formed issue key are required before any jj runs", async () => {
  const { jj, calls } = fakeJj();
  await expect(
    findHandoffCommit({
      phase: "implementing",
      role: "implementer",
      env: { ...env, LEGION_WORKSPACE: undefined },
      jj,
    })
  ).rejects.toThrow("LEGION_WORKSPACE is not set");
  await expect(
    findHandoffCommit({
      phase: "implementing",
      role: "implementer",
      env: { ...env, LEGION_ISSUE: "../x" },
      jj,
    })
  ).rejects.toThrow('LEGION_ISSUE "../x" is not a Dispatch issue key like LEGION-1');
  expect(calls).toEqual([]);
});

test("a failing jj surfaces its command and stderr", async () => {
  const jj: JjRunner = async (args) => {
    throw new Error(`jj ${args.join(" ")} failed: Error: There is no jj repo in "."`);
  };
  await expect(findHandoffCommit({ phase: "retro", role: "implementer", env, jj })).rejects.toThrow(
    'jj log -r @- --no-graph -T commit_id failed: Error: There is no jj repo in "."'
  );
});

// The revsets are jj's, so a fake cannot prove them: one real repository with a bare origin, the
// handoff committed and the branch pushed, run through PATH's jj, as a pane is.
const jjOnPath = Bun.which("jj");
const scratch: string[] = [];
afterEach(async () => {
  for (const directory of scratch.splice(0)) await rm(directory, { recursive: true, force: true });
});

/** Runs jj in `cwd` as the pane's identity, failing on a non-zero exit. */
async function jj(cwd: string, identity: NodeJS.ProcessEnv, ...args: string[]): Promise<string> {
  const child = Bun.spawn([jjOnPath as string, ...args], {
    cwd,
    env: { ...process.env, ...identity },
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  if (code !== 0) throw new Error(`jj ${args.join(" ")} exited ${code}:\n${stderr}`);
  return stdout.trim();
}

test.skipIf(jjOnPath === null)(
  "against a real jj workspace, the lookup refuses an unpushed handoff and reports it once pushed",
  async () => {
    const root = await mkdtemp(path.join(tmpdir(), "legion-handoff-commit-"));
    scratch.push(root);
    const workspace = path.join(root, "workspace");
    const identity = { JJ_USER: "Legion Test", JJ_EMAIL: "legion-test@example.invalid" };
    const paneEnv = { ...identity, LEGION_WORKSPACE: workspace, LEGION_ISSUE: ISSUE };
    await Bun.spawn(["git", "init", "--bare", "--quiet", path.join(root, "origin.git")]).exited;
    await jj(root, identity, "git", "init", workspace);
    await jj(workspace, identity, "git", "remote", "add", "origin", path.join(root, "origin.git"));
    await writeFile(path.join(workspace, "main.py"), 'print("hi")\n');
    await jj(workspace, identity, "describe", "-m", "implement: the change");
    await jj(workspace, identity, "new");

    // The handoff written but not yet committed: it sits in the working copy, @.
    await Bun.write(path.join(workspace, FILE), '{"schemaVersion":1,"phase":"implement"}\n');
    const lookup = () =>
      findHandoffCommit({ phase: "implementing", role: "implementer", env: paneEnv, jj: pathJj });
    await expect(lookup()).rejects.toThrow(`${FILE} has changes in the working copy`);

    // Committed but not pushed.
    await jj(workspace, identity, "split", "-m", "implement: record handoff", FILE);
    const carrying = await jj(
      workspace,
      identity,
      "log",
      "-r",
      `latest((::@- ~ ::trunk()) & files(${FILESET}))`,
      "--no-graph",
      "-T",
      "commit_id"
    );
    expect(carrying).toMatch(/^[0-9a-f]{40}$/);
    await expect(lookup()).rejects.toThrow(
      `${FILE} is carried by commit ${carrying}, which is not on legion/${ISSUE}@origin: push the issue branch, then complete again`
    );

    // Pushed: the carrying commit is reported.
    await jj(workspace, identity, "bookmark", "set", `legion/${ISSUE}`, "-r", "@-");
    await jj(workspace, identity, "git", "push", "--bookmark", `legion/${ISSUE}`);
    await expect(lookup()).resolves.toBe(carrying);

    // Another App's commit carrying the file is refused naming the author.
    await expect(
      findHandoffCommit({
        phase: "implementing",
        role: "implementer",
        env: { ...paneEnv, JJ_USER: "legion-reviewer[bot]", JJ_EMAIL: "r@example.invalid" },
        jj: pathJj,
      })
    ).rejects.toThrow(
      `${FILE} is carried by commit ${carrying}, authored by Legion Test <legion-test@example.invalid>, not this pane's legion-reviewer[bot]: run jj new, then write and commit this phase's handoff again`
    );

    // A phase that writes no file reports the commit the workspace stands on.
    await expect(
      findHandoffCommit({ phase: "retro", role: "implementer", env: paneEnv, jj: pathJj })
    ).resolves.toBe(carrying);
  },
  60_000
);
