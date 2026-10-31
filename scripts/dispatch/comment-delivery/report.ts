import { readFile, rename, rm, writeFile } from "node:fs/promises";
import { basename, dirname, join as joinPath } from "node:path";
import { parseArgs } from "node:util";
import { judge } from "./classes.ts";
import { type NamedText, renderPairs, renderReport } from "./render.ts";

// Turns the three files of the comment delivery audit (#2366) into a document for people,
// and a TSV file of its pairs: comments.csv from
// packages/envoy/internal/dispatch/deliveryaudit/comments.sql, and receipts.tsv and
// receipts-counts.txt from `tq receipts` (theorem-labs/agent-infra transcript-store).
// scripts/dispatch/README.md ("Comment delivery") gives the run. Input that does not agree with
// itself is refused (exit 1, every problem on stderr, nothing on stdout or in the pairs file),
// never counted.

interface Options {
  readonly comments: string;
  readonly receipts: string;
  readonly counts: string;
  readonly pairs: string;
  readonly findings: readonly string[];
  readonly traces: readonly string[];
}

function parseArguments(argv: readonly string[]): Options {
  const { values } = parseArgs({
    args: [...argv],
    options: {
      comments: { type: "string" },
      receipts: { type: "string" },
      counts: { type: "string" },
      pairs: { type: "string" },
      findings: { type: "string", multiple: true },
      trace: { type: "string", multiple: true },
    },
    strict: true,
  });
  const { comments, receipts, counts, pairs } = values;
  if (
    comments === undefined ||
    receipts === undefined ||
    counts === undefined ||
    pairs === undefined
  ) {
    throw new Error("--comments, --receipts, --counts and --pairs are required");
  }
  return {
    comments,
    receipts,
    counts,
    pairs,
    findings: values.findings ?? [],
    traces: values.trace ?? [],
  };
}

/** Each file's name and text, in the order given. A file that cannot be read rejects. */
function readNamed(paths: readonly string[]): Promise<NamedText[]> {
  return Promise.all(
    paths.map(async (path) => ({ name: basename(path), text: await readFile(path, "utf8") }))
  );
}

/** Writes `text` to `path` through a temporary file in the same directory, so that `path` is
 *  either absent or the whole text. */
async function writeWhole(path: string, text: string): Promise<void> {
  const temporary = joinPath(dirname(path), `.${basename(path)}.${process.pid}.tmp`);
  try {
    await writeFile(temporary, text);
    await rename(temporary, path);
  } catch (error) {
    await rm(temporary, { force: true });
    throw error;
  }
}

/** Judges the files, writes the pairs file, and gives the document. Writes nothing when it
 *  refuses. */
async function run(argv: readonly string[]): Promise<string> {
  const options = parseArguments(argv);
  const [[comments, receipts, counts], findings, traces] = await Promise.all([
    Promise.all(
      [options.comments, options.receipts, options.counts].map((path) => readFile(path, "utf8"))
    ),
    readNamed(options.findings),
    readNamed(options.traces),
  ]);
  const report = judge({
    comments: comments ?? "",
    receipts: receipts ?? "",
    counts: counts ?? "",
  });
  const pairs = renderPairs(report);
  const document = renderReport(report, findings, traces, {
    name: basename(options.pairs),
    rows: pairs.rows,
  });
  await writeWhole(options.pairs, pairs.text);
  return document;
}

if (import.meta.main) {
  try {
    process.stdout.write(await run(Bun.argv.slice(2)));
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`);
    process.exit(1);
  }
}
