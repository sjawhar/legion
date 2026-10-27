import { existsSync } from "node:fs";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { parseArgs } from "node:util";
import {
  analysePullRequest,
  classifyFixes,
  mapLimit,
  resolveFixes,
  upstreamRefFor,
} from "./analyse.ts";
import { GitRepo, ORIGIN_MAIN } from "./git.ts";
import { ghSource } from "./github.ts";
import { renderComparison, renderSummary } from "./render.ts";
import { compare, conflictingMerges, readRawRun, resolveWindow, summarise } from "./summary.ts";
import { type FixSpec, type PullRequestRecord, type RawRun, UsageError } from "./types.ts";

interface Options {
  since?: string;
  until?: string;
  repo: string;
  workdir: string;
  fetch: boolean;
  fromJson?: string;
  jobs: number;
  prefix: string;
  fixed: Array<{ glob: string; commit: string }>;
  compareWith?: string;
}

/** An RFC 3339 instant truncated to whole seconds. */
const instant = (ms: number) => new Date(ms).toISOString().replace(/\.\d{3}Z$/, "Z");

function parseOptions(argv: string[]): Options {
  try {
    const { values } = parseArgs({
      args: argv,
      options: {
        since: { type: "string" },
        until: { type: "string" },
        repo: { type: "string", default: "sjawhar/legion" },
        workdir: { type: "string" },
        "no-fetch": { type: "boolean", default: false },
        "from-json": { type: "string" },
        jobs: { type: "string", default: "8" },
        "branch-prefix": { type: "string", default: "" },
        fixed: { type: "string", multiple: true },
        "compare-with": { type: "string" },
      },
      strict: true,
      allowPositionals: false,
    });
    const { jobs, since, until } = values;
    if (!/^[1-9]\d*$/.test(jobs))
      throw new UsageError(`--jobs must be a positive integer: ${jobs}`);
    const fixed = (values.fixed ?? []).map((value) => {
      const at = value.lastIndexOf("=");
      if (at <= 0 || at === value.length - 1) {
        throw new UsageError(`--fixed must be '<glob>=<commit>', got "${value}"`);
      }
      return { glob: value.slice(0, at), commit: value.slice(at + 1) };
    });
    const fromJson = values["from-json"];
    if (fromJson === undefined && since === undefined) {
      throw new UsageError("--since is required unless --from-json is given");
    }
    if (fromJson !== undefined && (since !== undefined || until !== undefined)) {
      throw new UsageError(
        "--since and --until do not apply to --from-json: its run has its window"
      );
    }
    return {
      since,
      until,
      repo: values.repo,
      workdir: values.workdir ?? join(process.env.TMPDIR || ".", "conflicts"),
      fetch: !values["no-fetch"],
      fromJson,
      jobs: Number(jobs),
      prefix: values["branch-prefix"],
      fixed,
      compareWith: values["compare-with"],
    };
  } catch (error) {
    // parseArgs throws a TypeError for an unknown flag, a missing value or a positional.
    throw error instanceof UsageError ? error : new UsageError((error as Error).message);
  }
}

/** An earlier run's raw records; an unreadable file is a usage error, as invalid records are. */
async function readRun(path: string): Promise<RawRun> {
  let text: string;
  try {
    text = await readFile(path, "utf8");
  } catch (error) {
    throw new UsageError(`cannot read ${path}: ${(error as Error).message}`);
  }
  return readRawRun(text);
}

/** The bare clone in the workdir, cloned when missing and fetched unless `--no-fetch`. */
async function openRepo(options: Options): Promise<GitRepo> {
  const git = new GitRepo(
    join(options.workdir, "legion.git"),
    `https://github.com/${options.repo}`
  );
  await mkdir(options.workdir, { recursive: true });
  if (options.fetch || !existsSync(git.gitDir)) await git.ensure();
  return git;
}

/** Reads and analyses every pull request updated at or after `sinceInstant`, any state, whose
 * head branch starts with the prefix; sorted by number. */
async function readPullRequests(
  git: GitRepo,
  options: Options,
  sinceInstant: string
): Promise<PullRequestRecord[]> {
  const source = ghSource(options.repo);
  const listed = (await source.list(sinceInstant)).filter((pr) =>
    pr.headRefName.startsWith(options.prefix)
  );
  const graphs = await mapLimit(listed, options.jobs, (pr) => source.fetch(pr.number));
  const prs = await mapLimit(graphs, options.jobs, async (pr) =>
    analysePullRequest(git, pr, await upstreamRefFor(git, pr.baseRefName))
  );
  return prs.sort((a, b) => a.number - b.number);
}

async function main(argv: string[]): Promise<void> {
  const options = parseOptions(argv);
  const before = options.compareWith === undefined ? null : await readRun(options.compareWith);

  let run: RawRun;
  let git: GitRepo | null = null;
  let specs: FixSpec[] = [];
  if (options.fromJson !== undefined) {
    run = await readRun(options.fromJson);
    if (options.fixed.length > 0) {
      git = await openRepo(options);
      specs = await resolveFixes(git, options.fixed);
    }
  } else {
    const measuredAt = instant(Date.now());
    const since = options.since as string;
    const until = options.until ?? measuredAt;
    // A bad window, then a bad --fixed commit, is refused before the long part of the run.
    const window = resolveWindow(since, until);
    git = await openRepo(options);
    specs = await resolveFixes(git, options.fixed);
    const originMain = await git.revParse(ORIGIN_MAIN);
    run = {
      since,
      until,
      origin_main: originMain,
      prs: await readPullRequests(git, options, instant(window.since)),
      measured_at: measuredAt,
      selection: "updated-since",
    };
    const out = join(options.workdir, `conflicts-${since}_${until}.json`);
    await writeFile(out, `${JSON.stringify(run, null, 1)}\n`);
    console.error(`raw records: ${out}`);
  }

  const fixes =
    git !== null && specs.length > 0
      ? { specs, states: await classifyFixes(git, conflictingMerges(run, options.prefix), specs) }
      : undefined;
  const summary = summarise(run, { prefix: options.prefix, fixes });
  console.log(renderSummary(summary));
  if (before !== null) {
    // The before run's glob totals, without states: its merges need no git.
    const beforeFixes = specs.length > 0 ? { specs } : undefined;
    const was = summarise(before, { prefix: options.prefix, fixes: beforeFixes });
    console.log(renderComparison(compare(was, summary)));
  }
}

if (import.meta.main) {
  try {
    await main(Bun.argv.slice(2));
  } catch (error) {
    if (!(error instanceof UsageError)) {
      console.error(error);
      process.exit(1);
    }
    console.error(error.message);
    process.exit(2);
  }
}
