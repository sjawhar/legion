import { existsSync } from "node:fs";

export const ORIGIN_MAIN = "refs/remotes/origin/main";

interface Exit {
  code: number;
  stdout: string;
}

/** Runs `cmd`; throws naming the command, exit code and stderr when `accept` refuses the code. */
export async function run(cmd: string[], accept: (code: number) => boolean): Promise<Exit> {
  const proc = Bun.spawn(cmd, { stdin: "ignore", stdout: "pipe", stderr: "pipe" });
  const [stdout, stderr, code] = await Promise.all([
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
    proc.exited,
  ]);
  if (!accept(code)) throw new Error(`${cmd.join(" ")} failed (${code}): ${stderr.trim()}`);
  return { code, stdout };
}

export const success = (code: number) => code === 0;
const anyExit = () => true;

/** A local clone of the measured repository, every call `git --git-dir <gitDir> …`. */
export class GitRepo {
  constructor(
    readonly gitDir: string,
    readonly url: string
  ) {}

  private git(args: string[], accept: (code: number) => boolean = success): Promise<Exit> {
    return run(["git", "--git-dir", this.gitDir, ...args], accept);
  }

  async ensure(): Promise<void> {
    if (!existsSync(this.gitDir)) {
      await run(["git", "clone", "--bare", "-q", this.url, this.gitDir], success);
    }
    await this.git([
      "fetch",
      "-q",
      "--prune",
      this.url,
      "+refs/heads/*:refs/remotes/origin/*",
      "+refs/pull/*/head:refs/pull/*/head",
    ]);
  }

  async has(oid: string): Promise<boolean> {
    return (await this.git(["cat-file", "-e", `${oid}^{commit}`], anyExit)).code === 0;
  }

  async fetchOid(oid: string): Promise<boolean> {
    if (await this.has(oid)) return true;
    await this.git(["fetch", "-q", this.url, oid], anyExit);
    return this.has(oid);
  }

  /** Whether `a` is an ancestor of `b`; throws when git cannot tell (e.g. a missing object). */
  async isAncestor(a: string, b: string): Promise<boolean> {
    const { code } = await this.git(
      ["merge-base", "--is-ancestor", a, b],
      (exit) => exit === 0 || exit === 1
    );
    return code === 0;
  }

  /** The paths `git merge-tree` reports conflicted, sorted unique; [] for a clean merge. */
  async mergeTree(ours: string, theirs: string): Promise<string[]> {
    const { stdout } = await this.git(
      ["merge-tree", "--write-tree", "--name-only", "--no-messages", ours, theirs],
      (code) => code === 0 || code === 1
    );
    const paths = stdout.split("\n").slice(1).filter(Boolean);
    return [...new Set(paths)].sort();
  }

  /** origin/main's first-parent tip as it stood at `when`, or null before its first commit. */
  async mainAt(when: string): Promise<string | null> {
    const { stdout } = await this.git([
      "rev-list",
      "-1",
      "--first-parent",
      `--before=${when}`,
      ORIGIN_MAIN,
    ]);
    return stdout.trim() || null;
  }

  async revParse(rev: string): Promise<string> {
    return (await this.git(["rev-parse", rev])).stdout.trim();
  }

  async resolveCommit(rev: string): Promise<string | null> {
    const { code, stdout } = await this.git(
      ["rev-parse", "--verify", "-q", `${rev}^{commit}`],
      anyExit
    );
    return code === 0 ? stdout.trim() : null;
  }
}
