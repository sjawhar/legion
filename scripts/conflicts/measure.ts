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
import { fetchPullRequest, listPullRequests } from "./github.ts";
import { renderComparison, renderSummary } from "./render.ts";
import { applyFixStates, compare, readRawRun, resolveWindow, summarise } from "./summary.ts";
import { type FixSpec, type PullRequestRecord, type RawRun, UsageError } from "./types.ts";

/** Where the run comes from: measured live over a window, or read from earlier raw records. */
type Source = { kind: "live"; since: string; until?: string } | { kind: "json"; path: string };

interface Options {
  source: Source;
  repo: string;
  workdir: string;
  fetch: boolean;
  jobs: number;
  prefix: string;
  fixed: Array<{ glob: string; commit: string }>;
  compareWith?: string;
}

/** A run's raw records, and the clone and resolved specs its fix states need under `--fixed`. */
interface Measured {
  run: RawRun;
  fixes: { git: GitRepo; specs: FixSpec[] } | null;
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
    return {
      source: parseSource(values["from-json"], since, until),
      repo: values.repo,
      workdir: values.workdir ?? join(process.env.TMPDIR || ".", "conflicts"),
      fetch: !values["no-fetch"],
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

/** The run's source: `--from-json`'s file, or the window `--since` (required without it) opens. */
function parseSource(
  fromJson: string | undefined,
  since: string | undefined,
  until: string | undefined
): Source {
  if (fromJson === undefined) {
    if (since === undefined) {
      throw new UsageError("--since is required unless --from-json is given");
    }
    return { kind: "live", since, until };
  }
  if (since !== undefined || until !== undefined) {
    throw new UsageError("--since and --until do not apply to --from-json: its run has its window");
  }
  return { kind: "json", path: fromJson };
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

/** Reads and analyses every pull request updated at or after `sinceInstant`, any state, or open
 * now, whose head branch starts with the prefix; sorted by number. */
async function readPullRequests(
  git: GitRepo,
  options: Options,
  sinceInstant: string
): Promise<PullRequestRecord[]> {
  const listed = (await listPullRequests(options.repo, sinceInstant)).filter((pr) =>
    pr.headRefName.startsWith(options.prefix)
  );
  const prs = await mapLimit(listed, options.jobs, async ({ number }) => {
    const pr = await fetchPullRequest(options.repo, number);
    return analysePullRequest(git, pr, await upstreamRefFor(git, pr.baseRefName));
  });
  return prs.sort((a, b) => a.number - b.number);
}

/** Earlier raw records; with `--fixed`, the clone is opened and the specs resolved after them. */
async function readJsonRun(options: Options, path: string): Promise<Measured> {
  const run = await readRun(path);
  if (options.fixed.length === 0) return { run, fixes: null };
  const git = await openRepo(options);
  return { run, fixes: { git, specs: await resolveFixes(git, options.fixed) } };
}

/** Measures the window live and writes its raw records to the workdir. */
async function measureRun(
  options: Options,
  source: Extract<Source, { kind: "live" }>
): Promise<Measured> {
  const measuredAt = instant(Date.now());
  const { since } = source;
  const until = source.until ?? measuredAt;
  // A bad window, then a bad --fixed commit, is refused before the long part of the run.
  const window = resolveWindow(since, until);
  const git = await openRepo(options);
  const specs = await resolveFixes(git, options.fixed);
  const originMain = await git.revParse(ORIGIN_MAIN);
  const run: RawRun = {
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
  return { run, fixes: specs.length > 0 ? { git, specs } : null };
}

async function main(argv: string[]): Promise<void> {
  const options = parseOptions(argv);
  const before = options.compareWith === undefined ? null : await readRun(options.compareWith);
  const { source } = options;
  const { run, fixes } =
    source.kind === "json"
      ? await readJsonRun(options, source.path)
      : await measureRun(options, source);

  const counted = summarise(run, { prefix: options.prefix, fixes: fixes?.specs });
  const summary =
    fixes === null
      ? counted
      : applyFixStates(counted, await classifyFixes(fixes.git, counted.fixPairs));
  console.log(renderSummary(summary));
  if (before !== null) {
    // The before run's glob totals, without states: its merges need no git.
    const was = summarise(before, { prefix: options.prefix, fixes: fixes?.specs });
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
