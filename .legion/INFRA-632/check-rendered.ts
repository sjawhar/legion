// Proves that the restructured head of a guide renders every word its old-layout base renders
// (INFRA-632, plan "Proof", Acceptance 2, rendered text):
//
//   bun .legion/INFRA-632/check-rendered.ts <base.md> <head.md>
//
// Both files are parsed with micromark and its GFM extension (mdast-util-from-markdown with
// micromark-extension-gfm and mdast-util-gfm, the root dev dependencies
// scripts/semantic-line-breaks.ts renders with) and reduced to the text they render: text, code
// spans, code blocks and image alt text, with raw HTML's tags and comments dropped (a `<word>` left
// outside a code span is a tag and renders nothing) and link destinations and definitions unseen.
// A word is a run of letters, digits and underscores, so punctuation the layout adds or drops
// (`—`, a fence line's `#`, the `;` a clause split drops, a code span's backticks) is no word.
//
// The base's words must be a subsequence of the head's, the whole file through. git's minimal diff
// (--no-index, one word per line, on scratch files under $TMPDIR) aligns the two; every base word
// it drops is then accounted for, or is missing:
//
//   1. moved: hunks fewer than MOVE_GAP base words apart form a cluster, and a run of at least
//      MOVE_RUN words one hunk drops that one hunk of the cluster adds, in the same order, moved
//      (the longest runs first, each added word matched once), as an enumeration that becomes a
//      list moves a clause from after its items to before them;
//   2. a word of the header row of a table the conversion replaced with one sentence: the word
//      stands in the header row of a base table whose header row (its cells' text joined by
//      ` | `) heads fewer tables in the head than in the base;
//   3. a word ALLOWED below names, by the base words around it, with the report entry or the
//      reason that removes it; each allowance covers one word once.
//
// Every allowance of 3 is printed, and so is each use of 1, 2 and 3. Then, where the base has a
// `## Commands` section (the root guide), every `<…>` placeholder of its fenced code blocks must
// appear literally, as many times, in the rendered text of the head's `## Commands` section.
// The last line reads `<head>: <n> base words, <moved> moved, <allowed> allowed, <missing>
// missing; <p> placeholders, <q> missing`. The exit status is 1 on any missing word or
// placeholder, 2 on a usage or input error, and 0 otherwise.
import { spawnSync } from "node:child_process";
import { existsSync } from "node:fs";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";

const USAGE = "usage: bun .legion/INFRA-632/check-rendered.ts <base.md> <head.md>\n";

const WORD = /[\p{L}\p{N}_]+/gu;
const PLACEHOLDER = /<[^<>\s][^<>]*>/g;
/** Hunks of the word diff fewer than this many base words apart are one cluster (step 1). */
const MOVE_GAP = 80;
/** A moved run holds at least this many words, so no single word counts as moved (step 1). */
const MOVE_RUN = 2;
/** A dropped word is shown with this many base words on either side. */
const CONTEXT = 6;
/** Node types that render as a block of their own, so their text never runs into the next. */
const BLOCKS = new Set([
  "root",
  "paragraph",
  "heading",
  "blockquote",
  "list",
  "listItem",
  "table",
  "tableRow",
  "tableCell",
  "code",
  "thematicBreak",
  "footnoteDefinition",
]);

/**
 * Step 3: words the restructure removes from a guide's rendering, keyed by the guide's first
 * line, as restructure.ts recognises a guide. Each bracketed word is one allowance: the base word
 * at that place may go missing when the words around it in `words` are the base words around it.
 */
const ALLOWED: Record<string, { words: string; reason: string }[]> = {
  "# Legion": [
    { words: "server the work needs [row] a tool", reason: "reword-agents-run-database-row" },
    { words: "work needs into the pod [row] a service", reason: "reword-agents-get-tool-row" },
  ],
  "# Live proofs": [
    {
      words: "through scripts e2e [shellcheckrc] Every docker run",
      reason:
        "enum-e2e-sourced-libs moves the clause it ends before the list; git aligns its `scripts e2e` with other words",
    },
    { words: "curl python3 [and] tmux It builds", reason: "enum-e2e-stage1-needs" },
    { words: "no YubiKey touch [and] LEGION_E2E_MODEL_ROUTE", reason: "enum-e2e-stage2-needs" },
    { words: "another deployment s [and] LEGION_E2E_MODEL_ROUTE", reason: "enum-e2e-stage3-needs" },
    { words: "aws eks get token [and] the secrets broker", reason: "enum-e2e-stage4a-needs" },
    {
      words: "the daemon s [imageProbehands] it and no issue",
      reason:
        "the bridge table's image-probe row holds an unescaped `|` in a code span (restructure.ts warns), so GFM cuts the cell there and the base renders `imageProbe` and `hands` as one word; the head's item keeps both",
    },
    {
      words: "daemon controller liveness [rows] below",
      reason: "reword-e2e-controller-rows-below",
    },
    {
      words: "curl python3 [and] base64 [and] credentials for",
      reason: "enum-e2e-verifiers-needs",
    },
    {
      words: "select repo cache [readyistrue] negative pr in x",
      reason:
        "the multi-repo-acceptance table's cache row holds an unescaped `|` in a code span (restructure.ts warns), so GFM cuts the cell there and the base renders `ready`, `is` and `true` as one word; the head's item keeps all three",
    },
    {
      words: "two call baseline protocol [below] before requires",
      reason: "reword-e2e-baseline-pr-in-x",
    },
    {
      words: "two call baseline protocol [below] before captures",
      reason: "reword-e2e-baseline-repo-edit",
    },
    { words: "1 31 or later [and] kind LEGION_E2E_KIND", reason: "enum-e2e-burstable-needs" },
  ],
  "# Legion on Kubernetes": [
    { words: "the paragraph after [this] list says", reason: "reword-k8s-cgo-paragraph" },
    {
      words: "default is kept [and] chromium for",
      reason: "enum-k8s-capability-tools (`chromium`)",
    },
    { words: "the extraction [row] below and what", reason: "reword-k8s-extraction-row-paren" },
    { words: "the extraction [row] below Still", reason: "reword-k8s-extraction-row-verdict" },
    { words: "which is the runner [row] and for", reason: "reword-k8s-runner-row" },
    {
      words: "thermonuclear deep review [and] thermonuclear code quality [and] the planner",
      reason: "enum-k8s-shipped-agents-skills",
    },
    {
      words: "plan reviewer in agents [and] [the] pair s rubrics [and] ce simplify",
      reason: "enum-k8s-shipped-agents-skills",
    },
    {
      words: "the Envoy plugin ships [the] asd ste100",
      reason: "enum-k8s-shipped-agents-skills",
    },
    {
      words: "dispatch writing specs [and] envoy skills",
      reason: "enum-k8s-shipped-agents-skills",
    },
    {
      words: "sqlite3 nc [and] [dig] on PATH with go version running",
      reason:
        "enum-k8s-probe-path-tools: `and` dropped; `dig` moved into the list as its last item, one word after the dropped `and`, so no run of MOVE_RUN takes it",
    },
    {
      words: "installed version on PATH [and] prints the table",
      reason: "enum-k8s-probe-path-tools",
    },
    { words: "chromium version [and] socat V", reason: "enum-k8s-toolchain-step-commands" },
    {
      words:
        "The image is probed before it publishes [the] [image] [is] [probed] [before] [it] [publishes] is refused",
      reason:
        "html-block-k8s-volume-mount-refusal: in the base a line starting with `<dir>` opens a raw HTML block, whose text holds the link's destination `#the-image-is-probed-before-it-publishes` as words; the head renders that paragraph as Markdown, where a link destination is unseen",
    },
  ],
};

interface MdNode {
  type: string;
  value?: string;
  alt?: string;
  depth?: number;
  children?: MdNode[];
}

type Parse = (text: string) => MdNode;

/** The text a node renders, and where in it each table's header row stands. */
interface Rendering {
  text: string;
  headerRows: { header: string; start: number; end: number }[];
}

/** A run of base words the word diff drops and of head words it adds at one place. */
interface Hunk {
  /** The index of the first dropped base word, or of the base word the added words precede. */
  oldStart: number;
  removed: string[];
  added: string[];
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

/**
 * micromark with GFM, imported only once a node_modules directory holding it is in the script's
 * directory or above, so Bun never installs an unpinned copy itself (as semantic-line-breaks.ts's
 * loadParser does).
 */
async function loadParser(): Promise<Parse> {
  let installed = false;
  for (let directory = import.meta.dir; !installed; directory = dirname(directory)) {
    installed = existsSync(join(directory, "node_modules", "mdast-util-from-markdown"));
    if (dirname(directory) === directory) break;
  }
  if (!installed) {
    fail(
      "check-rendered.ts needs mdast-util-from-markdown, mdast-util-gfm and micromark-extension-gfm; run `bun install` in the repository root\n"
    );
  }
  const [markdown, gfmMarkdown, gfmSyntax] = await Promise.all([
    import("mdast-util-from-markdown"),
    import("mdast-util-gfm"),
    import("micromark-extension-gfm"),
  ]);
  const options = {
    extensions: [gfmSyntax.gfm()],
    mdastExtensions: [gfmMarkdown.gfmFromMarkdown()],
  };
  return (text) => markdown.fromMarkdown(text, options) as unknown as MdNode;
}

/** What a node renders (see the header). */
function render(node: MdNode): Rendering {
  const parts: string[] = [];
  const headerRows: Rendering["headerRows"] = [];
  let length = 0;
  const push = (part: string): void => {
    parts.push(part);
    length += part.length;
  };
  const walk = (current: MdNode, headerRow: boolean): void => {
    if (current.type === "definition" || current.type === "yaml") return;
    const start = length;
    if (current.type === "html") {
      push((current.value ?? "").replace(/<!--[\s\S]*?-->/g, " ").replace(/<[^>]*>/g, " "));
    } else if (current.type === "break") {
      push(" ");
    } else if (current.value !== undefined) {
      push(current.value);
    } else if (current.alt) {
      push(current.alt);
    }
    for (const [index, child] of (current.children ?? []).entries()) {
      walk(child, current.type === "table" && index === 0);
    }
    if (BLOCKS.has(current.type)) push("\n");
    if (headerRow) {
      const header = (current.children ?? [])
        .map((cell) => render(cell).text.replace(/\s+/g, " ").trim())
        .join(" | ");
      headerRows.push({ header, start, end: length });
    }
  };
  walk(node, false);
  return { text: parts.join(""), headerRows };
}

/** The top-level blocks of the `## Commands` section, or undefined when there is none. */
function commandsSection(root: MdNode): MdNode[] | undefined {
  const blocks = root.children ?? [];
  const start = blocks.findIndex(
    (block) =>
      block.type === "heading" && block.depth === 2 && render(block).text.trim() === "Commands"
  );
  if (start === -1) return undefined;
  const end = blocks.findIndex(
    (block, index) => index > start && block.type === "heading" && (block.depth ?? 7) <= 2
  );
  return blocks.slice(start + 1, end === -1 ? undefined : end);
}

/** git's minimal diff of two word lists, one word per line. */
async function wordHunks(
  scratch: string,
  before: readonly string[],
  after: readonly string[]
): Promise<Hunk[]> {
  const oldPath = join(scratch, "base.words");
  const newPath = join(scratch, "head.words");
  await writeFile(oldPath, before.map((word) => `${word}\n`).join(""));
  await writeFile(newPath, after.map((word) => `${word}\n`).join(""));
  const result = spawnSync(
    "git",
    [
      "diff",
      "--no-index",
      "--no-ext-diff",
      "--no-textconv",
      "--no-color",
      "--minimal",
      "-U0",
      oldPath,
      newPath,
    ],
    { cwd: scratch, encoding: "utf8", maxBuffer: 1 << 30 }
  );
  if (result.status !== 0 && result.status !== 1) {
    fail(`git diff exited ${result.status ?? result.signal}\n${result.stderr}`);
  }
  const hunks: Hunk[] = [];
  let open: Hunk | undefined;
  for (const line of result.stdout.split("\n")) {
    const header = /^@@ -(\d+)(?:,(\d+))? \+\d+(?:,\d+)? @@/.exec(line);
    if (header) {
      const start = Number(header[1]);
      open = { oldStart: header[2] === "0" ? start : start - 1, removed: [], added: [] };
      hunks.push(open);
    } else if (open && line.startsWith("-")) {
      open.removed.push(line.slice(1));
    } else if (open && line.startsWith("+")) {
      open.added.push(line.slice(1));
    }
  }
  return hunks;
}

/**
 * For each dropped word, whether it lies in a moved run (step 1). `null` separates two hunks'
 * words, so no run spans two hunks.
 */
function movedRuns(
  dropped: readonly (string | null)[],
  added: readonly (string | null)[]
): boolean[] {
  const moved = dropped.map(() => false);
  const taken = added.map(() => false);
  for (;;) {
    let best = 0;
    let bestEnd: [number, number] = [0, 0];
    let previous = new Uint32Array(added.length + 1);
    for (let i = 1; i <= dropped.length; i++) {
      const row = new Uint32Array(added.length + 1);
      for (let j = 1; j <= added.length; j++) {
        const word = dropped[i - 1];
        if (word === null || moved[i - 1] || taken[j - 1] || word !== added[j - 1]) continue;
        const run = (previous[j - 1] ?? 0) + 1;
        row[j] = run;
        if (run > best) {
          best = run;
          bestEnd = [i, j];
        }
      }
      previous = row;
    }
    if (best < MOVE_RUN) return moved;
    for (let offset = 1; offset <= best; offset++) {
      moved[bestEnd[0] - offset] = true;
      taken[bestEnd[1] - offset] = true;
    }
  }
}

function count(list: readonly string[]): Map<string, number> {
  const counts = new Map<string, number>();
  for (const item of list) counts.set(item, (counts.get(item) ?? 0) + 1);
  return counts;
}

async function main(argv: string[]): Promise<number> {
  const [basePath, headPath] = argv;
  if (argv.length !== 2 || !basePath || !headPath || basePath.startsWith("-")) fail(USAGE);
  const [baseText, headText] = await Promise.all(
    [basePath, headPath].map((path) =>
      readFile(path, "utf8").catch((error: unknown) =>
        fail(`${path}: ${error instanceof Error ? error.message : String(error)}\n`)
      )
    )
  );
  const title = (baseText as string).split("\n", 1)[0] ?? "";
  const explicit = ALLOWED[title];
  if (explicit === undefined) {
    fail(`${basePath}: its first line, ${JSON.stringify(title)}, names none of the three guides\n`);
  }
  const parse = await loadParser();
  const baseRoot = parse(baseText as string);
  const headRoot = parse(headText as string);
  const base = render(baseRoot);
  const head = render(headRoot);
  const baseMatches = [...base.text.matchAll(WORD)];
  const baseWords = baseMatches.map((match) => match[0]);
  const headWords = head.text.match(WORD) ?? [];
  const out: string[] = [
    `${headPath}: the rendered words of ${basePath} (${baseWords.length}) against the head's (${headWords.length})`,
  ];

  // Step 2: the base words in the header row of a converted table.
  const headHeaders = count(head.headerRows.map((row) => row.header));
  const baseHeaders = count(base.headerRows.map((row) => row.header));
  const headerWord = new Map<number, string>();
  for (const row of base.headerRows) {
    if ((baseHeaders.get(row.header) ?? 0) <= (headHeaders.get(row.header) ?? 0)) continue;
    for (const [index, match] of baseMatches.entries()) {
      if (match.index >= row.start && match.index < row.end) headerWord.set(index, row.header);
    }
  }
  // Step 3: each bracketed word of ALLOWED, with the words around it.
  const allowances = explicit.flatMap(({ words, reason }) => {
    const list = words.split(" ");
    out.push(`  allowed: ${words} (${reason})`);
    return list.flatMap((word, index) =>
      word.startsWith("[") ? [{ list, index, reason, used: false }] : []
    );
  });

  const scratch = await mkdtemp(join(tmpdir(), "check-rendered-"));
  let moved = 0;
  let allowed = 0;
  let missing = 0;
  try {
    const hunks = await wordHunks(scratch, baseWords, headWords);
    const clusters: Hunk[][] = [];
    let end = Number.NEGATIVE_INFINITY;
    for (const hunk of hunks) {
      if (clusters.length === 0 || hunk.oldStart - end >= MOVE_GAP) clusters.push([]);
      clusters.at(-1)?.push(hunk);
      end = hunk.oldStart + hunk.removed.length;
    }
    for (const cluster of clusters) {
      const dropped = cluster.flatMap((hunk) => [
        ...hunk.removed.map((word, offset) => ({ word, at: hunk.oldStart + offset })),
        { word: null, at: -1 },
      ]);
      const runs = movedRuns(
        dropped.map((item) => item.word),
        cluster.flatMap((hunk) => [...hunk.added, null])
      );
      const movedWords = dropped.filter((_, index) => runs[index]).map((item) => item.word);
      if (movedWords.length > 0) {
        moved += movedWords.length;
        out.push(`  moved, near base word ${cluster[0]?.oldStart}: ${movedWords.join(" ")}`);
      }
      for (const [index, { word, at }] of dropped.entries()) {
        if (word === null || runs[index]) continue;
        const context = `…${baseWords.slice(Math.max(0, at - CONTEXT), at).join(" ")} [${word}] ${baseWords.slice(at + 1, at + 1 + CONTEXT).join(" ")}…`;
        const header = headerWord.get(at);
        if (header !== undefined) {
          allowed++;
          out.push(
            `  removed, base word ${at}: ${context} (the header row "${header}" of a converted table)`
          );
          continue;
        }
        const allowance = allowances.find(
          (candidate) =>
            !candidate.used &&
            candidate.list.every(
              (listed, offset) =>
                listed.replace(/^\[|\]$/g, "") === baseWords[at - candidate.index + offset]
            )
        );
        if (allowance !== undefined) {
          allowance.used = true;
          allowed++;
          out.push(`  removed, base word ${at}: ${context} (${allowance.reason})`);
        } else {
          missing++;
          out.push(`  MISSING base word ${at}: ${context}`);
        }
      }
    }
  } finally {
    await rm(scratch, { recursive: true, force: true });
  }
  for (const allowance of allowances) {
    if (!allowance.used) {
      out.push(`  note: the allowance "${allowance.list.join(" ")}" went unused`);
    }
  }

  let placeholders = 0;
  let placeholdersMissing = 0;
  const baseCommands = commandsSection(baseRoot);
  if (baseCommands !== undefined) {
    const wanted = count(
      baseCommands
        .filter((block) => block.type === "code")
        .flatMap((block) => (block.value ?? "").replace(/\s+/g, " ").match(PLACEHOLDER) ?? [])
    );
    const headCommands = (commandsSection(headRoot) ?? [])
      .map((block) => render(block).text)
      .join("\n")
      .replace(/\s+/g, " ");
    for (const [placeholder, times] of wanted) {
      placeholders += times;
      const found = headCommands.split(placeholder).length - 1;
      if (found < times) {
        placeholdersMissing += times - found;
        out.push(
          `  MISSING placeholder ${placeholder}: ${times} in the base's ## Commands fences, ${found} in the head's ## Commands rendering`
        );
      }
    }
    out.push(
      `  placeholders: ${wanted.size} distinct, ${placeholders} in the base's ## Commands fences; ${placeholders - placeholdersMissing} found in the head's ## Commands rendering`
    );
  }
  out.push(
    `${headPath}: ${baseWords.length} base words, ${moved} moved, ${allowed} allowed, ${missing} missing; ${placeholders} placeholders, ${placeholdersMissing} missing`
  );
  process.stdout.write(`${out.join("\n")}\n`);
  return missing > 0 || placeholdersMissing > 0 ? 1 : 0;
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
