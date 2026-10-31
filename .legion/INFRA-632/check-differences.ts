// Proves that every word-level difference between an old-layout guide and its restructured head
// is one restructure.ts made and recorded (INFRA-632, plan "Proof", Acceptance 2):
//
//   bun .legion/INFRA-632/check-differences.ts [--file <report file>] <base.md> <head.md> <report.json>
//
// Both files are folded: every run of whitespace (tr's [:space:]: space, tab, newline, vertical
// tab, form feed, carriage return) becomes one space, and the ends are trimmed. restructure.ts
// folds JavaScript's `\s`, a superset of these six characters; the two folds agree on the three
// guides, which hold no other whitespace. The report is the JSON array `restructure.ts --report`
// writes, in the order the edits were made: { id, kind, file, old, new }. Its entries for this file
// (`file` is the output path as given on the restructure command line: the entries whose `file` is
// the head path, else the report file whose path shares the most trailing components with the head
// path; --file names it outright) are applied in order to the folded base, each folded `old`
// replaced by its folded `new`. Each `old` must occur exactly once in the text at that point; an
// entry that does not is a failure, named by its index in the report and its id. The result is
// then compared with the folded head, and every word-level hunk left between them (git diff
// --no-index --word-diff=porcelain on scratch files under $TMPDIR) is printed as unexplained.
//
// For evidence, the word-level hunks between the folded base and the folded head are printed one
// per line, each with the report entries whose changed text it overlaps (an entry's changed text is
// its `new` without the words it shares with its `old` at either end, carried through the later
// entries; a later entry that edits it widens it). A hunk no entry overlaps, and no unexplained
// hunk already covers, is printed as unexplained too. Then one line per kind gives how many
// entries applied and how many of those changed only whitespace, and the last line reads
// `<head>: <n> entries applied, <m> unexplained hunks`. The exit status is 1 when an entry fails
// or any hunk is unexplained, 2 on a usage or input error, and 0 otherwise.
import { spawnSync } from "node:child_process";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve, sep } from "node:path";

const USAGE =
  "usage: bun .legion/INFRA-632/check-differences.ts [--file <report file>] <base.md> <head.md> <report.json>\n";

/** tr's [:space:] in the C locale, the class git's word regex below splits on. */
const SPACE_RUN = /[ \t\n\v\f\r]+/g;
const WORD = /[^ \t\n\v\f\r]+/g;
/** A hunk's removed or added words are shown up to this many characters. */
const SHOWN = 70;
/** An unexplained hunk is shown with this many words of the head on either side. */
const CONTEXT = 6;

interface Entry {
  id: string;
  kind: string;
  file: string;
  old: string;
  new: string;
}

/** A word of a folded text: where it starts and where it ends. */
interface Word {
  start: number;
  end: number;
}

/** A run of removed and added words between two words both sides hold. */
interface Hunk {
  removed: string[];
  added: string[];
  /** The index of the first word after the hunk's start, in the old text and in the new text. */
  oldAt: number;
  newAt: number;
}

interface WordDiff {
  hunks: Hunk[];
  /** For each word of the new text, the index of the same word in the old text, or -1. */
  newToOld: Int32Array;
}

/** The span of the folded text after the edits that an entry's changed words occupy. */
interface Region {
  entry: number;
  start: number;
  end: number;
}

class Failure extends Error {
  constructor(
    message: string,
    readonly status: number
  ) {
    super(message);
  }
}

function fail(message: string, status = 2): never {
  throw new Failure(message, status);
}

function fold(text: string): string {
  return text.replace(SPACE_RUN, " ").trim();
}

function words(text: string): Word[] {
  return [...text.matchAll(WORD)].map((match) => ({
    start: match.index,
    end: match.index + match[0].length,
  }));
}

function shown(list: readonly string[]): string {
  const text = list.join(" ");
  return text.length <= SHOWN ? text : `${text.slice(0, SHOWN - 1)}…`;
}

function showHunk(hunk: Hunk): string {
  const removed = hunk.removed.length > 0 ? `[-${shown(hunk.removed)}-]` : "";
  const added = hunk.added.length > 0 ? `{+${shown(hunk.added)}+}` : "";
  return `${removed}${added}`;
}

function parseReport(text: string, path: string): Entry[] {
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch (error) {
    fail(`${path}: not JSON: ${error instanceof Error ? error.message : String(error)}\n`);
  }
  if (!Array.isArray(parsed)) fail(`${path}: not a JSON array\n`);
  return parsed.map((entry: unknown, index) => {
    const record = entry as Record<string, unknown> | null;
    for (const key of ["id", "kind", "file", "old", "new"]) {
      if (typeof record?.[key] !== "string") {
        fail(`${path}: entry ${index} has no string field "${key}"\n`);
      }
    }
    return record as unknown as Entry;
  });
}

/** The report file whose entries describe the head (see the header). */
function reportFile(entries: readonly Entry[], head: string, named: string | undefined): string {
  const files = [...new Set(entries.map((entry) => entry.file))];
  if (named !== undefined) {
    if (!files.includes(named)) fail(`--file ${named}: no report entry names that file\n`);
    return named;
  }
  const exact = files.filter((file) => file === head || resolve(file) === resolve(head));
  if (exact.length === 1) return exact[0] as string;
  const headParts = resolve(head).split(sep).filter(Boolean).reverse();
  let best: string[] = [];
  let bestShared = 0;
  for (const file of files) {
    const parts = resolve(file).split(sep).filter(Boolean).reverse();
    let shared = 0;
    while (shared < parts.length && parts[shared] === headParts[shared]) shared++;
    if (shared > bestShared) {
      best = [file];
      bestShared = shared;
    } else if (shared === bestShared && shared > 0) {
      best.push(file);
    }
  }
  if (best.length !== 1) {
    fail(
      `${head}: ${best.length === 0 ? "no" : "more than one"} report file matches its path; name it with --file (report files: ${files.join(", ")})\n`
    );
  }
  return best[0] as string;
}

/** git's word-level diff of two folded texts, words being runs of non-space. */
async function wordDiff(scratch: string, before: string, after: string): Promise<WordDiff> {
  if (before === after) {
    return { hunks: [], newToOld: Int32Array.from(words(after), (_, index) => index) };
  }
  const oldPath = join(scratch, "old");
  const newPath = join(scratch, "new");
  await writeFile(oldPath, before);
  await writeFile(newPath, after);
  const result = spawnSync(
    "git",
    [
      "diff",
      "--no-index",
      "--no-ext-diff",
      "--no-textconv",
      "--no-color",
      "--minimal",
      "--word-diff=porcelain",
      "--word-diff-regex=[^[:space:]]+",
      oldPath,
      newPath,
    ],
    { cwd: scratch, encoding: "utf8", maxBuffer: 1 << 30 }
  );
  if (result.status !== 0 && result.status !== 1) {
    fail(`git diff exited ${result.status ?? result.signal}\n${result.stderr}`);
  }
  const newCount = words(after).length;
  const newToOld = new Int32Array(newCount).fill(-1);
  const hunks: Hunk[] = [];
  let oldAt = 0;
  let newAt = 0;
  let open: Hunk | undefined;
  let body = false;
  for (const line of result.stdout.split("\n")) {
    if (!body) {
      body = line.startsWith("@@");
      continue;
    }
    const payload = line.slice(1).match(WORD) ?? [];
    if (line.startsWith(" ")) {
      for (let index = 0; index < payload.length; index++) newToOld[newAt + index] = oldAt + index;
      oldAt += payload.length;
      newAt += payload.length;
      open = undefined;
    } else if ((line.startsWith("-") || line.startsWith("+")) && payload.length > 0) {
      if (open === undefined) {
        open = { removed: [], added: [], oldAt, newAt };
        hunks.push(open);
      }
      if (line.startsWith("-")) {
        open.removed.push(...payload);
        oldAt += payload.length;
      } else {
        open.added.push(...payload);
        newAt += payload.length;
      }
    }
  }
  if (newAt !== newCount || oldAt !== words(before).length) {
    fail(
      `git's word diff accounts for ${oldAt} and ${newAt} words, not ${words(before).length} and ${newCount}\n`
    );
  }
  return { hunks, newToOld };
}

/**
 * The characters of the old text a hunk's span of new-text words stands at: the old words its new
 * words are, or else the gap between the old words its nearest unchanged neighbours are.
 */
function oldSpan(
  first: number,
  count: number,
  newToOld: Int32Array,
  oldWords: readonly Word[],
  oldLength: number
): [number, number] {
  let start = Number.POSITIVE_INFINITY;
  let end = Number.NEGATIVE_INFINITY;
  for (let index = first; index < first + count; index++) {
    const mapped = newToOld[index] ?? -1;
    const word = oldWords[mapped];
    if (word === undefined) continue;
    start = Math.min(start, word.start);
    end = Math.max(end, word.end);
  }
  if (start <= end) return [start, end];
  let left = first - 1;
  while (left >= 0 && (newToOld[left] ?? -1) === -1) left--;
  let right = first + count;
  while (right < newToOld.length && (newToOld[right] ?? -1) === -1) right++;
  const leftWord = left >= 0 ? oldWords[newToOld[left] ?? -1] : undefined;
  const rightWord = right < newToOld.length ? oldWords[newToOld[right] ?? -1] : undefined;
  return [leftWord?.end ?? 0, rightWord?.start ?? oldLength];
}

async function main(argv: string[]): Promise<number> {
  let named: string | undefined;
  const paths: string[] = [];
  for (let index = 0; index < argv.length; index++) {
    const arg = argv[index] as string;
    if (arg === "--file") {
      named = argv[++index];
      if (named === undefined) fail(USAGE);
    } else if (arg.startsWith("-")) {
      fail(USAGE);
    } else {
      paths.push(arg);
    }
  }
  const [basePath, headPath, reportPath] = paths;
  if (paths.length !== 3 || !basePath || !headPath || !reportPath) fail(USAGE);
  const [baseText, headText, reportText] = await Promise.all(
    [basePath, headPath, reportPath].map((path) =>
      readFile(path, "utf8").catch((error: unknown) =>
        fail(`${path}: ${error instanceof Error ? error.message : String(error)}\n`)
      )
    )
  );
  const report = parseReport(reportText as string, reportPath);
  const file = reportFile(report, headPath, named);
  const base = fold(baseText as string);
  const head = fold(headText as string);

  const out: string[] = [];
  out.push(`${headPath}: report entries of ${file}, applied in order to the folded ${basePath}`);
  let text = base;
  let failures = 0;
  const regions: Region[] = [];
  const applied = new Map<string, { applied: number; whitespace: number }>();
  for (const [index, entry] of report.entries()) {
    if (entry.file !== file) continue;
    const old = fold(entry.old);
    const next = fold(entry.new);
    const at = old === "" ? -1 : text.indexOf(old);
    const again = at === -1 ? -1 : text.indexOf(old, at + 1);
    if (at === -1 || again !== -1) {
      failures++;
      out.push(
        `  FAILED #${index} ${entry.id} (${entry.kind}): its old text occurs ${at === -1 ? "nowhere" : "more than once"} in the text: ${shown([old])}`
      );
      continue;
    }
    const count = applied.get(entry.kind) ?? { applied: 0, whitespace: 0 };
    count.applied++;
    applied.set(entry.kind, count);
    text = text.slice(0, at) + next + text.slice(at + old.length);
    if (old === next) {
      count.whitespace++;
      continue;
    }
    let prefix = 0;
    while (prefix < old.length && prefix < next.length && old[prefix] === next[prefix]) prefix++;
    let suffix = 0;
    const room = Math.min(old.length, next.length) - prefix;
    while (suffix < room && old[old.length - 1 - suffix] === next[next.length - 1 - suffix]) {
      suffix++;
    }
    const start = at + prefix;
    const oldEnd = at + old.length - suffix;
    const newEnd = at + next.length - suffix;
    const delta = next.length - old.length;
    for (const region of regions) {
      if (region.end <= start) continue;
      if (region.start >= oldEnd) {
        region.start += delta;
        region.end += delta;
      } else {
        region.start = Math.min(region.start, start);
        region.end = region.end >= oldEnd ? region.end + delta : newEnd;
      }
    }
    regions.push({ entry: index, start, end: newEnd });
  }

  const scratch = await mkdtemp(join(tmpdir(), "check-differences-"));
  try {
    const textWords = words(text);
    const headWords = words(head);
    const residual = await wordDiff(scratch, text, head);
    let unexplained = 0;
    const residualSpans: [number, number][] = [];
    for (const hunk of residual.hunks) {
      unexplained++;
      residualSpans.push(
        oldSpan(hunk.newAt, hunk.added.length, residual.newToOld, textWords, text.length)
      );
      const end = hunk.newAt + hunk.added.length;
      const [before, after] = [
        headWords.slice(Math.max(0, hunk.newAt - CONTEXT), hunk.newAt),
        headWords.slice(end, end + CONTEXT),
      ].map((list) => list.map((word) => head.slice(word.start, word.end)).join(" "));
      out.push(
        `  UNEXPLAINED at head word ${hunk.newAt}, the edited base against the head: …${before} ${showHunk(hunk)} ${after}…`
      );
    }

    const changes = await wordDiff(scratch, base, head);
    const explaining = new Set<number>();
    for (const [number, hunk] of changes.hunks.entries()) {
      const span = oldSpan(
        hunk.newAt,
        hunk.added.length,
        residual.newToOld,
        textWords,
        text.length
      );
      const entries = regions
        .filter((region) => span[0] <= region.end && span[1] >= region.start)
        .map((region) => region.entry)
        .sort((a, b) => a - b);
      for (const entry of entries) explaining.add(entry);
      const covered = residualSpans.some(([start, end]) => span[0] <= end && span[1] >= start);
      let verdict: string;
      if (entries.length > 0) {
        verdict = entries
          .map((entry) => `#${entry} ${report[entry]?.id} (${report[entry]?.kind})`)
          .join(", ");
        if (covered) verdict += "; partly UNEXPLAINED, above";
      } else if (covered) {
        verdict = "UNEXPLAINED, above";
      } else {
        unexplained++;
        verdict = "UNEXPLAINED: no report entry changed these words";
      }
      out.push(`  hunk ${number + 1} at head word ${hunk.newAt}: ${showHunk(hunk)} <- ${verdict}`);
    }

    const silent = regions.filter((region) => !explaining.has(region.entry));
    for (const region of silent) {
      const entry = report[region.entry];
      out.push(
        `  note: #${region.entry} ${entry?.id} (${entry?.kind}) changed words no hunk of git's alignment overlaps`
      );
    }
    let total = 0;
    for (const [kind, count] of [...applied].sort(([a], [b]) => a.localeCompare(b))) {
      total += count.applied;
      out.push(
        `  ${kind}: ${count.applied} applied${count.whitespace > 0 ? `, ${count.whitespace} of them whitespace only` : ""}`
      );
    }
    out.push(
      `  ${changes.hunks.length} hunks between the folded base and the folded head; the edited base ${text === head ? "equals" : "differs from"} the folded head`
    );
    if (failures > 0) out.push(`  ${failures} report entries FAILED to apply`);
    out.push(`${headPath}: ${total} entries applied, ${unexplained} unexplained hunks`);
    process.stdout.write(`${out.join("\n")}\n`);
    return failures > 0 || unexplained > 0 ? 1 : 0;
  } finally {
    await rm(scratch, { recursive: true, force: true });
  }
}

if (import.meta.main) {
  try {
    process.exit(await main(process.argv.slice(2)));
  } catch (error) {
    if (!(error instanceof Failure)) throw error;
    process.stderr.write(error.message);
    process.exit(error.status);
  }
}
