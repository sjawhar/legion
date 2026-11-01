import { afterAll, describe, expect, test } from "bun:test";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { analysePullRequest, classifyFixes, resolveFixes } from "../analyse.ts";
import { GitRepo } from "../git.ts";
import {
  type FixPair,
  type GraphCommit,
  type GraphPullRequest,
  type MergeRecord,
  UsageError,
} from "../types.ts";

const UPSTREAM = "refs/remotes/origin/main";
/** An oid no test repository holds, and that `file:///nonexistent` cannot provide. */
const ABSENT = "0123456789abcdef0123456789abcdef01234567";
const CHANGELOG = "packages/x/CHANGELOG.md";
const directories: string[] = [];

afterAll(async () => {
  await Promise.all(directories.map((dir) => rm(dir, { recursive: true, force: true })));
});

/** A throwaway work tree whose git runs with a fixed identity, no user config, and a clock that
 * advances one minute per command, so commit oids and dates are reproducible. */
class Scratch {
  private tick = 0;

  private constructor(readonly dir: string) {}

  static async create(): Promise<Scratch> {
    const dir = await mkdtemp(join(tmpdir(), "conflicts-analyse-"));
    directories.push(dir);
    const scratch = new Scratch(dir);
    await scratch.git(["init", "-q", "-b", "main"]);
    return scratch;
  }

  async git(args: string[], accept: number[] = [0]): Promise<string> {
    const date = `${1_798_000_000 + 60 * this.tick++} +0000`;
    const proc = Bun.spawn(["git", ...args], {
      cwd: this.dir,
      stdin: "ignore",
      stdout: "pipe",
      stderr: "pipe",
      env: {
        PATH: process.env.PATH,
        GIT_CONFIG_GLOBAL: "/dev/null",
        GIT_CONFIG_NOSYSTEM: "1",
        GIT_AUTHOR_NAME: "Test Author",
        GIT_AUTHOR_EMAIL: "author@example.com",
        GIT_AUTHOR_DATE: date,
        GIT_COMMITTER_NAME: "Test Committer",
        GIT_COMMITTER_EMAIL: "committer@example.com",
        GIT_COMMITTER_DATE: date,
      },
    });
    const [stdout, stderr, code] = await Promise.all([
      new Response(proc.stdout).text(),
      new Response(proc.stderr).text(),
      proc.exited,
    ]);
    if (!accept.includes(code)) throw new Error(`git ${args.join(" ")} (${code}): ${stderr}`);
    return stdout.trim();
  }

  /** Writes `files` and commits them on the current branch; returns the commit's oid. */
  async commit(message: string, files: Record<string, string>): Promise<string> {
    for (const [path, content] of Object.entries(files)) {
      await mkdir(dirname(join(this.dir, path)), { recursive: true });
      await writeFile(join(this.dir, path), content);
    }
    await this.git(["add", "-A"]);
    await this.git(["commit", "-q", "-m", message]);
    return this.git(["rev-parse", "HEAD"]);
  }

  /** Merges `rev` into the current branch, writing `resolution` over any conflicted file. */
  async merge(rev: string, message: string, resolution: Record<string, string> = {}) {
    await this.git(["merge", "-q", "--no-ff", "--no-commit", rev], [0, 1]);
    return this.commit(message, resolution);
  }

  async checkout(branch: string, create?: string): Promise<void> {
    await this.git(create ? ["checkout", "-q", "-b", branch, create] : ["checkout", "-q", branch]);
  }

  /** Points origin/main at local main, as a fetch would. */
  async publishMain(): Promise<void> {
    await this.git(["update-ref", UPSTREAM, "main"]);
  }

  /** The commits of `base..head`, oldest first, as GitHub lists a pull request's commits. */
  async revList(range: string): Promise<string[]> {
    return (await this.git(["rev-list", "--reverse", range])).split("\n");
  }

  async graphCommit(oid: string): Promise<GraphCommit> {
    const [, parents, committedDate, messageHeadline] = (
      await this.git(["log", "-1", "--format=%H%x00%P%x00%cI%x00%s", oid])
    ).split("\0");
    const nodes = parents.split(" ").map((parent) => ({ oid: parent }));
    return { oid, messageHeadline, committedDate, parents: { nodes } };
  }

  async pullRequest(headRefName: string, oids: string[]): Promise<GraphPullRequest> {
    return {
      number: 1,
      title: headRefName,
      state: "MERGED",
      baseRefName: "main",
      headRefName,
      headRefOid: oids[oids.length - 1],
      mergedAt: "2026-12-31T00:00:00Z",
      createdAt: "2026-12-01T00:00:00Z",
      closedAt: "2026-12-31T00:00:00Z",
      commits: await Promise.all(oids.map((oid) => this.graphCommit(oid))),
      forcePushes: [],
    };
  }

  repo(): GitRepo {
    return new GitRepo(join(this.dir, ".git"), "file:///nonexistent");
  }
}

function changelog(line3: string): string {
  return `# x\n\n${line3}\n\nline 5\n`;
}

/** Test 1's history: main A, B (changelog line 3), C (README only); `legion/T-1` from A changes
 * changelog line 3 (X), merges main at B resolving the conflict (M1), then merges main at C (M2). */
async function changelogHistory() {
  const scratch = await Scratch.create();
  const a = await scratch.commit("A", {
    [CHANGELOG]: changelog("line 3"),
    "README.md": "readme\n",
  });
  await scratch.checkout("legion/T-1", "main");
  const x = await scratch.commit("X", { [CHANGELOG]: changelog("branch") });
  await scratch.checkout("main");
  const b = await scratch.commit("B", { [CHANGELOG]: changelog("main") });
  await scratch.checkout("legion/T-1");
  const m1 = await scratch.merge("main", "M1", { [CHANGELOG]: changelog("branch and main") });
  await scratch.checkout("main");
  const c = await scratch.commit("C", { "README.md": "readme, revised\n" });
  await scratch.checkout("legion/T-1");
  const m2 = await scratch.merge("main", "M2");
  await scratch.publishMain();
  const pr = await scratch.pullRequest("legion/T-1", await scratch.revList("main..legion/T-1"));
  return { git: scratch.repo(), pr, a, b, c, x, m1, m2 };
}

function mergeOf(merges: MergeRecord[], oid: string): MergeRecord {
  const merge = merges.find((m) => m.oid === oid);
  if (!merge) throw new Error(`no merge ${oid}`);
  return merge;
}

/** `merge` counted against the fix `commit` under `glob`, as `summarise` pairs them. */
function pairOf(merge: MergeRecord, glob: string, commit: string): FixPair {
  const { oid, branch_parent, upstream_parent, conflicts } = merge;
  return { oid, glob, commit, branch_parent, upstream_parent, paths: conflicts ?? [] };
}

/** `pr` with each commit's parent `from` renamed `to`, as GitHub lists a commit the clone lacks. */
function withParent(pr: GraphPullRequest, from: string, to: string): GraphPullRequest {
  const commits = pr.commits.map((c) => ({
    ...c,
    parents: { nodes: c.parents.nodes.map((p) => ({ oid: p.oid === from ? to : p.oid })) },
  }));
  return { ...pr, commits };
}

describe("analysePullRequest", () => {
  test("re-runs a conflicting forward merge and lists its conflicted file", async () => {
    const { git, pr, b, c, x, m1, m2 } = await changelogHistory();

    const record = await analysePullRequest(git, pr, UPSTREAM);

    expect(record.commit_count).toBe(3);
    expect(Object.keys(record)).toEqual([
      "number",
      "title",
      "state",
      "baseRefName",
      "headRefName",
      "headRefOid",
      "mergedAt",
      "createdAt",
      "closedAt",
      "commit_count",
      "merges",
      "force_pushes",
    ]);
    expect(record.closedAt).toBe(pr.closedAt);
    expect(record.merges.map((m) => m.oid)).toEqual([m1, m2]);
    expect(mergeOf(record.merges, m1)).toMatchObject({
      kind: "forward",
      branch_parent: x,
      upstream_parent: b,
      upstream_on_main: true,
      upstream_on_base: true,
      conflicts: [CHANGELOG],
    });
    expect(mergeOf(record.merges, m1).error).toBeUndefined();
    expect(mergeOf(record.merges, m2)).toMatchObject({
      kind: "forward",
      branch_parent: m1,
      upstream_parent: c,
      conflicts: [],
    });
  });

  test("lists every conflicted file, sorted", async () => {
    const scratch = await Scratch.create();
    await scratch.commit("A", { "z.md": "z\n", "packages/a.ts": "a\n" });
    await scratch.checkout("legion/T-2", "main");
    await scratch.commit("X", { "z.md": "branch z\n", "packages/a.ts": "branch a\n" });
    await scratch.checkout("main");
    await scratch.commit("B", { "z.md": "main z\n", "packages/a.ts": "main a\n" });
    await scratch.checkout("legion/T-2");
    const m = await scratch.merge("main", "M", { "z.md": "z\n", "packages/a.ts": "a\n" });
    await scratch.publishMain();
    const pr = await scratch.pullRequest("legion/T-2", await scratch.revList("main..legion/T-2"));

    const record = await analysePullRequest(scratch.repo(), pr, UPSTREAM);

    expect(mergeOf(record.merges, m)).toMatchObject({
      kind: "forward",
      conflicts: ["packages/a.ts", "z.md"],
    });
  });

  test("a merge of a branch that is neither main nor the base is other-branch", async () => {
    const scratch = await Scratch.create();
    await scratch.commit("A", { "README.md": "readme\n" });
    await scratch.checkout("feature/other", "main");
    const y = await scratch.commit("Y", { "other.txt": "other\n" });
    await scratch.checkout("legion/T-3", "main");
    const x = await scratch.commit("X", { "mine.txt": "mine\n" });
    const m = await scratch.merge("feature/other", "M");
    await scratch.publishMain();
    // The pull request's list holds its own line only, so the other branch's Y is outside it.
    const pr = await scratch.pullRequest("legion/T-3", [x, m]);

    const record = await analysePullRequest(scratch.repo(), pr, UPSTREAM);

    expect(mergeOf(record.merges, m)).toMatchObject({
      kind: "other-branch",
      branch_parent: x,
      upstream_parent: y,
      upstream_on_main: false,
      upstream_on_base: false,
      conflicts: [],
    });
  });

  test("a merge whose upstream parent cannot be fetched is an undeterminable forward merge", async () => {
    const { git, pr, b, c, x, m1, m2 } = await changelogHistory();

    const record = await analysePullRequest(git, withParent(pr, b, ABSENT), UPSTREAM);

    const merge = mergeOf(record.merges, m1);
    expect(merge).toEqual({
      oid: m1,
      date: pr.commits[1].committedDate, // pr lists X, M1, M2
      headline: "M1",
      parents: [x, ABSENT],
      branch_parent: x,
      upstream_parent: ABSENT,
      kind: "forward",
      conflicts: null,
      error: "parent object missing",
    });
    expect(mergeOf(record.merges, m2)).toMatchObject({
      kind: "forward",
      branch_parent: m1,
      upstream_parent: c,
      conflicts: [],
    });
  });
});

describe("fix states", () => {
  test("classify M1 against a fix after, at, and before its upstream parent", async () => {
    const { git, pr, a, b, c, m1 } = await changelogHistory();
    const merge = mergeOf((await analysePullRequest(git, pr, UPSTREAM)).merges, m1);

    const stateFor = async (commit: string) => {
      const [fix] = await resolveFixes(git, [{ glob: "**/CHANGELOG.md", commit }]);
      const pair = pairOf(merge, fix.glob, fix.commit);
      return (await classifyFixes(git, [pair])).get(m1)?.get("**/CHANGELOG.md");
    };

    expect(await stateFor(c)).toBe("before");
    expect(await stateFor(b)).toBe("transition");
    expect(await stateFor(a)).toBe("after");
  });

  test("resolveFixes resolves a short oid and refuses commits off origin/main", async () => {
    const { git, c, x } = await changelogHistory();

    expect(await resolveFixes(git, [{ glob: "*.md", commit: c.slice(0, 10) }])).toEqual([
      { glob: "*.md", commit: c },
    ]);
    await expect(resolveFixes(git, [{ glob: "*.md", commit: x }])).rejects.toBeInstanceOf(
      UsageError
    );
    await expect(
      resolveFixes(git, [{ glob: "*.md", commit: "refs/heads/no-such-branch" }])
    ).rejects.toBeInstanceOf(UsageError);
  });

  test("classifyFixes refuses a merge whose parent cannot be fetched", async () => {
    const { git, pr, c, m1 } = await changelogHistory();
    const merge = mergeOf((await analysePullRequest(git, pr, UPSTREAM)).merges, m1);
    const [fix] = await resolveFixes(git, [{ glob: "**/CHANGELOG.md", commit: c }]);

    for (const event of [
      { ...merge, upstream_parent: ABSENT },
      { ...merge, branch_parent: ABSENT },
    ]) {
      const pair = pairOf(event, fix.glob, fix.commit);
      const error = await classifyFixes(git, [pair]).catch((e: unknown) => e);
      expect(error).toBeInstanceOf(Error);
      expect(error).not.toBeInstanceOf(UsageError);
    }
    const states = await classifyFixes(git, [pairOf(merge, fix.glob, fix.commit)]);
    expect(states.get(m1)?.get("**/CHANGELOG.md")).toBe("before");
  });
});

describe("GitRepo", () => {
  test("isAncestor answers for present commits and throws for a missing one", async () => {
    const { git, a, c, x } = await changelogHistory();

    expect(await git.isAncestor(a, c)).toBe(true);
    expect(await git.isAncestor(c, x)).toBe(false);
    await expect(git.isAncestor(ABSENT, UPSTREAM)).rejects.toBeInstanceOf(Error);
    await expect(git.isAncestor(a, ABSENT)).rejects.toBeInstanceOf(Error);
  });
});
