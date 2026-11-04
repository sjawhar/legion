// display-read-census.ts - every stored text of production Dispatch, read by Go's display read
// and by the browser's own engine, each reading put in a class.
//
// The pull request this serves moves the reading of stored markdown out of the browser: Go reads
// a stored text for display (pmdoc.ReadForDisplay) and the server sends that tree to a request
// holding `Dispatch-Trees: 1`. The question the census answers is whether that reading says what
// the browser's reading said, over every text Dispatch holds, and above all whether Go's reading
// ever drops text the browser showed.
//
// The recipe is INFRA-172's, recorded in docs/solutions/envoy/a-change-to-how-dispatch-reads-
// stored-markdown-censuses-every-stored-version-and-rewrites-the-hits-before-the-merge-INFRA-172.md:
// a version is read with `GET /api/v1/artifacts/{id}/versions/{n}`, never `/text`, which renders
// the live tree again; each project's issues come from one unpaged `GET /api/v1/issues?project=`,
// because offset paging repeats and skips issues while others write; a network failure or a 5xx
// is retried; the corpus lives under $TMPDIR and is never committed.
//
// ## The two readers
//
// Both readings come from the readers themselves, through the pmdoc differential's own two
// harnesses, so the census never reimplements either:
//
//   - the browser's, `gen/differential.ts`, which runs the fork's headless engine: `--read-only`
//     is the read-only engine, what a reader is shown, and the plain run is the editing engine;
//   - Go's, the `TestDifferentialDump` of `packages/envoy/internal/dispatch/pmdoc`, which dumps
//     `ReadForDisplay(md, DisplayLatest)` and `ReadForDisplay(md, DisplayVersion)` with the block
//     ids dropped (differential_test.go, differential_display_test.go).
//
// A stored text that is shown as it stands now is held to the read-only engine in `DisplayLatest`,
// and a past document version to the editing engine in `DisplayVersion`: the pairing the
// cross-reader gates of `TestDifferentialCompare` hold the head to.
//
// ## The classes
//
// Every text falls in exactly one class. Two trees are compared with the block ids ignored, as the
// differential compares them: a node is its type, its text, its attributes (the block id dropped,
// an attribute whose value is null counting as absent) and its marks (ordered by type and
// attributes, as pmdoc.FromJSON sorts them); a table's short body rows are first padded to the
// header's width, the canonicalisation Parse does and the engine does not. A reading's *visible
// text* is every `text` node's text, in document order, run together.
//
//   - **the two agree** - the two trees are the same tree.
//   - **format differs** - the trees differ and the visible text is the same character for
//     character: the same words, drawn as other blocks or marks.
//   - **the browser lost text** - the browser's visible text is Go's with characters deleted (a
//     proper subsequence of it): Go shows everything the browser showed and more. A directive the
//     browser's tokenizer swallows and the display read draws as its literal fence lines lands
//     here.
//   - **Go loses text** - Go's visible text is the browser's with characters deleted: the reader
//     is shown less than the browser showed them. This class must be empty; a text in it is a
//     defect of the pull request.
//   - **the text differs both ways** - neither visible text is a subsequence of the other, so each
//     reader holds text the other does not. A Go loss hides here too, which is why it is reported
//     beside "Go loses text" and must be empty as well.
//   - **the browser cannot read it** - the engine threw on the text. There is nothing to hold Go
//     to; the throw is the browser's own.
//   - **Go cannot read it** - the display read gave no tree Go's own schema holds. ReadForDisplay
//     never refuses, so this stands for a tree `Node.JSON` would not write, and is a defect too.
//
// A *group* is where the text is stored: a comment's body, an issue message's body, an ask's
// question, an option's label or description, an answer's text, a resolution's reason, a
// document's approval reason, an architecture component's prose, or a document version.
//
// ## Running it
//
//   bun scripts/dispatch/display-read-census.ts                   # read, then class and report
//   bun scripts/dispatch/display-read-census.ts --json            # the same run as one object
//   bun scripts/dispatch/display-read-census.ts --no-fetch        # class the corpus already read
//   bun scripts/dispatch/display-read-census.ts --check-no-stored-text FILE...
//
// The corpus doubles as the resume file: each unit of work (one list, one version) is appended as
// one JSONL line as it lands, and a pass that dies after an hour skips every unit already in it.
// INFRA-172's first pass lost 689 requests to an outage and was discarded; this one continues.
import {
  appendFileSync,
  closeSync,
  mkdirSync,
  openSync,
  readFileSync,
  readSync,
  writeSync,
} from "node:fs";
import { dirname, join } from "node:path";
import { StringDecoder } from "node:string_decoder";
import { parseArgs } from "node:util";
import type { ActiveDispatchConfig } from "../../packages/envoy-client/src/dispatch-config";
import {
  sessionConfigFile,
  sessionDispatchSettings,
} from "../../packages/envoy-client/src/session-settings";

/** The repository this script stands in, whose pmdoc harnesses it runs. */
const REPO = join(import.meta.dir, "..", "..");
/** The browser's reader, and Go's: the differential's own two harnesses. */
const ENGINE_READER = join(REPO, "packages/envoy/internal/dispatch/pmdoc/gen/differential.ts");
const GO_MODULE = join(REPO, "packages/envoy");
const GO_PACKAGE = "./internal/dispatch/pmdoc";
/** At most this many requests a second leave the census, however many are in flight. */
const DEFAULT_RATE = 5;
/** Requests in flight at once: enough to keep the rate up over a slow answer, no more. */
const IN_FLIGHT = 8;
/** A network failure or a 5xx is tried this many times before the unit is recorded as failed. */
const ATTEMPTS = 5;
/** The shortest stored paragraph `--check-no-stored-text` judges, in normalised characters: a
 * shorter one ("Done.", "Approve") says nothing about where a file's words came from. */
export const MIN_STORED_PARAGRAPH = 64;

export const GROUPS = [
  "comment",
  "message",
  "ask-question",
  "ask-option-label",
  "ask-option-description",
  "ask-answer",
  "ask-resolution",
  "approval-reason",
  "component-prose",
  "document-version",
] as const;

export type Group = (typeof GROUPS)[number];

export const CLASSES = [
  "the two agree",
  "format differs",
  "the browser lost text",
  "Go loses text",
  "the text differs both ways",
  "the browser cannot read it",
  "Go cannot read it",
] as const;

export type TextClass = (typeof CLASSES)[number];

/** A document version is held to the editing engine in the version mode; every other stored text
 * to the read-only engine in the latest mode. */
export function readingMode(group: Group): "latest" | "version" {
  return group === "document-version" ? "version" : "latest";
}

type DispatchConfig = Pick<ActiveDispatchConfig, "url" | "token">;

/** One stored text, as the corpus holds it. */
export interface StoredText {
  /** Unique over the run, and the key the readers' outputs are joined on. */
  readonly id: string;
  readonly group: Group;
  readonly markdown: string;
}

/** One version of one document, named by a list the census already read. */
interface VersionRef {
  readonly artifact: string;
  readonly number: number;
}

/** One unit of reading: what it found, so a resumed pass needs none of its requests again. */
interface UnitRecord {
  readonly unit: string;
  readonly texts: readonly StoredText[];
  /** The issue keys a project's issue list gave. */
  readonly issues?: readonly string[];
  /** The versions a document list named. */
  readonly versions?: readonly VersionRef[];
  /** Why the unit holds nothing, for a unit the server would not answer. */
  readonly error?: string;
  /** Requests the unit spent, failed attempts included. */
  readonly requests: number;
  /** When the unit landed, and how long it took, so a report covers a run resumed from a pass
   * that died an hour in rather than only the part this process read. */
  readonly at: number;
  readonly ms: number;
}

/** A ProseMirror node, as both readers write one. */
export interface PmNode {
  readonly type: string;
  readonly text?: string;
  readonly attrs?: Record<string, unknown>;
  readonly marks?: readonly PmMark[];
  readonly content?: readonly PmNode[];
}

export interface PmMark {
  readonly type: string;
  readonly attrs?: Record<string, unknown>;
}

/** What the two readers made of one text. A null tree with no message is a reading neither reader
 * was asked for. */
export interface TextReading {
  /** Go's display read, in the mode the text's group takes. */
  readonly go: PmNode | null;
  /** The browser's reading, from the engine the text's group takes. */
  readonly browser: PmNode | null;
  /** The engine's own throw, when it threw. */
  readonly browserError?: string;
}

/** One stored text, where it is stored and the class the census puts it in. */
export interface ClassedText {
  readonly id: string;
  readonly group: Group;
  readonly class: TextClass;
}

/** One class's count, and how it splits by group. */
export interface ClassCount {
  readonly class: TextClass;
  readonly texts: number;
  readonly groups: Readonly<Partial<Record<Group, number>>>;
}

/** Every number one run counts, as both output modes print them. */
export interface CensusReport {
  readonly texts: number;
  readonly requests: number;
  /** Wall-clock seconds spent reading production, over every pass the corpus holds. */
  readonly readSeconds: number;
  /** Wall-clock seconds spent running the two readers and classing what they read. */
  readonly classSeconds: number;
  readonly failedUnits: number;
  readonly classes: readonly ClassCount[];
  readonly groups: readonly { group: Group; texts: number }[];
  /** The id of every text in a class that must be empty, so each can be read again by hand. */
  readonly defects: readonly ClassedText[];
}

interface RunOptions {
  readonly json: boolean;
  readonly fetch: boolean;
  /** Read production and stop, leaving the classing to a later `--no-fetch` run over the corpus:
   * a pass takes hours, and nothing of the comparison should be able to lose it. */
  readonly fetchOnly: boolean;
  readonly projects: readonly string[];
  readonly corpus: string;
  readonly work: string;
  readonly rate: number;
  readonly checkFiles: readonly string[];
}

// --- the stored texts of one answer -------------------------------------------------------

/** A text the census reads, left out when it is empty: an empty text has no tree to compare. */
function text(id: string, group: Group, markdown: unknown): StoredText[] {
  return typeof markdown === "string" && markdown !== "" ? [{ id, group, markdown }] : [];
}

interface ApiComment {
  readonly id: string;
  readonly body?: string;
}

interface ApiMessageThread {
  readonly message?: ApiComment;
  readonly replies?: readonly ApiComment[];
}

interface ApiAsk {
  readonly id: string;
  readonly question?: string;
  readonly options?: readonly { id?: string; label?: string; description?: string }[] | null;
  readonly answer?: { text?: string | null } | null;
  readonly resolution?: { reason?: string } | null;
}

interface ApiArtifact {
  readonly id: string;
  /** `doc` is the markdown document; a `file` or an `image` holds bytes no reader parses, and
   * its version route answers those bytes rather than JSON. */
  readonly kind?: string;
  readonly versions?: readonly { number: number }[] | null;
  readonly approval?: { reason?: string | null } | null;
}

interface ApiComponentTree {
  readonly components?: readonly { id?: string; prose?: string }[] | null;
}

export function commentTexts(comments: readonly ApiComment[]): StoredText[] {
  return comments.flatMap((comment) => text(`comment/${comment.id}`, "comment", comment.body));
}

export function messageTexts(threads: readonly ApiMessageThread[]): StoredText[] {
  return threads.flatMap((thread) =>
    [thread.message, ...(thread.replies ?? [])].flatMap((message) =>
      message === undefined ? [] : text(`message/${message.id}`, "message", message.body)
    )
  );
}

export function askTexts(asks: readonly ApiAsk[]): StoredText[] {
  return asks.flatMap((ask) => [
    ...text(`ask/${ask.id}/question`, "ask-question", ask.question),
    ...(ask.options ?? []).flatMap((option, index) => {
      const name = option.id ?? String(index);
      return [
        ...text(`ask/${ask.id}/option/${name}/label`, "ask-option-label", option.label),
        ...text(
          `ask/${ask.id}/option/${name}/description`,
          "ask-option-description",
          option.description
        ),
      ];
    }),
    ...text(`ask/${ask.id}/answer`, "ask-answer", ask.answer?.text),
    ...text(`ask/${ask.id}/resolution`, "ask-resolution", ask.resolution?.reason),
  ]);
}

export function approvalTexts(artifacts: readonly ApiArtifact[]): StoredText[] {
  return artifacts
    .filter((artifact) => artifact.kind === "doc")
    .flatMap((artifact) =>
      text(`approval/${artifact.id}`, "approval-reason", artifact.approval?.reason)
    );
}

export function componentTexts(tree: ApiComponentTree, project: string): StoredText[] {
  return (tree.components ?? []).flatMap((component, index) =>
    text(`component/${project}/${component.id ?? index}`, "component-prose", component.prose)
  );
}

export function artifactVersions(artifacts: readonly ApiArtifact[]): VersionRef[] {
  return artifacts
    .filter((artifact) => artifact.kind === "doc")
    .flatMap((artifact) =>
      (artifact.versions ?? []).map((version) => ({
        artifact: artifact.id,
        number: version.number,
      }))
    );
}

// --- reading production ---------------------------------------------------------------------

/** A token bucket: `rate` requests a second over the whole run, however many run at once. */
class Rate {
  private next = 0;
  private readonly gap: number;

  constructor(rate: number) {
    this.gap = 1000 / rate;
  }

  async take(): Promise<void> {
    const now = Date.now();
    const at = Math.max(now, this.next);
    this.next = at + this.gap;
    if (at > now) await Bun.sleep(at - now);
  }
}

/** A 5xx or a network failure is the server's weather and is retried; a 4xx is this run's own
 * answer and is recorded as the unit's error. */
class HttpError extends Error {
  constructor(
    readonly status: number,
    message: string
  ) {
    super(message);
  }
}

/** What one unit spends. Each unit counts its own requests: one counter shared by the eight
 * units in flight would attribute to each of them every request the others made while it ran. */
interface Spend {
  requests: number;
}

async function get<T>(config: DispatchConfig, path: string, rate: Rate, spend: Spend): Promise<T> {
  let last = "";
  for (let attempt = 1; attempt <= ATTEMPTS; attempt++) {
    await rate.take();
    spend.requests += 1;
    try {
      const response = await fetch(`${config.url}${path}`, {
        headers: { Authorization: `Bearer ${config.token}` },
      });
      if (response.ok) return (await response.json()) as T;
      const body = (await response.text()).slice(0, 200);
      if (response.status < 500) throw new HttpError(response.status, `${response.status} ${body}`);
      last = `${response.status} ${body}`;
    } catch (error) {
      if (error instanceof HttpError) throw error;
      last = String(error);
    }
    await Bun.sleep(Math.min(30_000, 500 * 2 ** attempt));
  }
  throw new HttpError(0, `GET ${path}: ${ATTEMPTS} attempts failed: ${last}`);
}

/** Requests this process has sent, for the progress line alone: each unit's own count is what
 * the corpus records and what the report adds up. */
let requests = 0;

/** One unit of work: its name, and the reads that fill it, counting its own requests. */
interface Unit {
  readonly unit: string;
  read(
    config: DispatchConfig,
    rate: Rate,
    spend: Spend
  ): Promise<Omit<UnitRecord, "unit" | "requests" | "at" | "ms">>;
}

function projectUnits(project: string): Unit[] {
  const encoded = encodeURIComponent(project);
  return [
    {
      unit: `project-issues/${project}`,
      async read(config, rate, spend) {
        // Deliberately unpaged: without limit or offset the route answers every issue in one
        // array, where offset paging repeats and skips issues while others write (INFRA-172).
        const issues = await get<{ key: string }[]>(
          config,
          `/api/v1/issues?project=${encoded}`,
          rate,
          spend
        );
        return { texts: [], issues: issues.map((issue) => issue.key) };
      },
    },
    {
      unit: `project-architecture/${project}`,
      async read(config, rate, spend) {
        try {
          const tree = await get<ApiComponentTree>(
            config,
            `/api/v1/projects/${encoded}/architecture`,
            rate,
            spend
          );
          return { texts: componentTexts(tree, project) };
        } catch (error) {
          // A project with no architecture source is 404 SOURCE_NOT_FOUND: it stores no component
          // prose, which is nothing read rather than a read that failed.
          if (error instanceof HttpError && error.status === 404) return { texts: [] };
          throw error;
        }
      },
    },
    {
      unit: `project-artifacts/${project}`,
      async read(config, rate, spend) {
        const artifacts = await get<ApiArtifact[]>(
          config,
          `/api/v1/projects/${encoded}/artifacts`,
          rate,
          spend
        );
        return { texts: approvalTexts(artifacts), versions: artifactVersions(artifacts) };
      },
    },
  ];
}

function issueUnits(key: string): Unit[] {
  const encoded = encodeURIComponent(key);
  return [
    {
      unit: `issue-comments/${key}`,
      async read(config, rate, spend) {
        const comments = await get<ApiComment[] | null>(
          config,
          `/api/v1/issues/${encoded}/comments`,
          rate,
          spend
        );
        return { texts: commentTexts(comments ?? []) };
      },
    },
    {
      unit: `issue-messages/${key}`,
      async read(config, rate, spend) {
        const threads = await get<ApiMessageThread[] | null>(
          config,
          `/api/v1/issues/${encoded}/messages`,
          rate,
          spend
        );
        return { texts: messageTexts(threads ?? []) };
      },
    },
    {
      unit: `issue-asks/${key}`,
      async read(config, rate, spend) {
        const asks = await get<ApiAsk[] | null>(
          config,
          `/api/v1/issues/${encoded}/asks`,
          rate,
          spend
        );
        return { texts: askTexts(asks ?? []) };
      },
    },
    {
      unit: `issue-artifacts/${key}`,
      async read(config, rate, spend) {
        const artifacts = await get<ApiArtifact[] | null>(
          config,
          `/api/v1/issues/${encoded}/artifacts`,
          rate,
          spend
        );
        return {
          texts: approvalTexts(artifacts ?? []),
          versions: artifactVersions(artifacts ?? []),
        };
      },
    },
  ];
}

function versionUnit(version: VersionRef): Unit {
  return {
    unit: `version/${version.artifact}/${version.number}`,
    async read(config, rate, spend) {
      // /versions/{n} is the stored markdown; /text renders the live tree again (INFRA-172).
      const read = await get<{ markdown?: string }>(
        config,
        `/api/v1/artifacts/${version.artifact}/versions/${version.number}`,
        rate,
        spend
      );
      return {
        texts: text(
          `version/${version.artifact}/${version.number}`,
          "document-version",
          read.markdown
        ),
      };
    },
  };
}

/** What a unit's record says once its texts are on disk: enough to resume, to plan the units it
 * named and to count the run, and no stored text, which a pass of this size should not hold in
 * memory and never needs twice. */
type UnitSummary = Omit<UnitRecord, "texts">;

/**
 * Reads a JSONL file one line at a time, holding one line rather than the file: the corpus and
 * each reader's output run to hundreds of megabytes. `T` is the shape this script itself wrote
 * into the file; nothing else writes one, so each line is read as that shape unchecked, and a
 * file that is not this script's own fails where its fields are used.
 */
function* jsonLines<T>(path: string): Generator<T> {
  const handle = openSync(path, "r");
  try {
    const chunk = Buffer.allocUnsafe(1 << 20);
    const decoder = new StringDecoder("utf8");
    let buffer = "";
    while (true) {
      const read = readSync(handle, chunk, 0, chunk.length, null);
      if (read === 0) break;
      buffer += decoder.write(chunk.subarray(0, read));
      let start = 0;
      let newline = buffer.indexOf("\n", start);
      while (newline !== -1) {
        const line = buffer.slice(start, newline);
        if (line.trim() !== "") yield JSON.parse(line) as T;
        start = newline + 1;
        newline = buffer.indexOf("\n", start);
      }
      buffer = buffer.slice(start);
    }
    buffer += decoder.end();
    if (buffer.trim() !== "") yield JSON.parse(buffer) as T;
  } finally {
    closeSync(handle);
  }
}

/** Every stored text of a corpus file, in the order it was read, each text once. */
export function* storedTexts(path: string): Generator<StoredText> {
  const seen = new Set<string>();
  for (const unit of jsonLines<UnitRecord>(path)) {
    for (const stored of unit.texts) {
      if (seen.has(stored.id)) continue;
      seen.add(stored.id);
      yield stored;
    }
  }
}

/** The corpus file, which is the resume file: one unit a line, appended as it lands. */
class Corpus {
  readonly done = new Map<string, UnitSummary>();

  constructor(readonly path: string) {
    mkdirSync(dirname(path), { recursive: true });
    try {
      for (const line of jsonLines<UnitRecord>(path)) {
        const { texts: _texts, ...summary } = line;
        this.done.set(summary.unit, summary);
      }
    } catch (error) {
      // A corpus not written yet is an empty one; anything else is the operator's to see.
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
    }
  }

  record(unit: UnitRecord): void {
    const { texts: _texts, ...summary } = unit;
    this.done.set(summary.unit, summary);
    appendFileSync(this.path, `${JSON.stringify(unit)}\n`);
  }
}

/** Runs every unit not already in the corpus, `IN_FLIGHT` at a time at the run's rate. */
async function readUnits(
  config: DispatchConfig,
  corpus: Corpus,
  units: readonly Unit[],
  rate: Rate,
  label: string
): Promise<void> {
  const pending = units.filter((unit) => !corpus.done.has(unit.unit));
  console.error(`${label}: ${pending.length} of ${units.length} units left to read`);
  let next = 0;
  let done = 0;
  const started = Date.now();
  const reader = async (): Promise<void> => {
    while (next < pending.length) {
      const unit = pending[next++];
      const spend: Spend = { requests: 0 };
      const opened = Date.now();
      try {
        const read = await unit.read(config, rate, spend);
        corpus.record({
          unit: unit.unit,
          requests: spend.requests,
          at: Date.now(),
          ms: Date.now() - opened,
          ...read,
        });
      } catch (error) {
        corpus.record({
          unit: unit.unit,
          texts: [],
          requests: spend.requests,
          at: Date.now(),
          ms: Date.now() - opened,
          error: String(error),
        });
      }
      done += 1;
      requests += spend.requests;
      if (done % 250 === 0 || done === pending.length) {
        const seconds = (Date.now() - started) / 1000;
        const left = ((pending.length - done) * seconds) / done;
        console.error(
          `${label}: ${done}/${pending.length} units, ${requests} requests, ` +
            `${seconds.toFixed(0)}s elapsed, ~${left.toFixed(0)}s left`
        );
      }
    }
  };
  await Promise.all(Array.from({ length: Math.min(IN_FLIGHT, pending.length) }, reader));
}

function recordedIssues(corpus: Corpus, projects: readonly string[]): string[] {
  return projects.flatMap((project) => [
    ...(corpus.done.get(`project-issues/${project}`)?.issues ?? []),
  ]);
}

function recordedVersions(corpus: Corpus): VersionRef[] {
  const seen = new Set<string>();
  const all: VersionRef[] = [];
  for (const unit of corpus.done.values()) {
    for (const version of unit.versions ?? []) {
      const key = `${version.artifact}/${version.number}`;
      if (seen.has(key)) continue;
      seen.add(key);
      all.push(version);
    }
  }
  return all;
}

/**
 * The order a pass should read versions in: every document's newest version, then every
 * document's next newest, and so on. A pass cut short has then read the version of each document
 * a reader is shown now rather than the whole history of the first few documents, and one
 * document of 4,802 versions cannot eat the pass. INFRA-172's recipe counts the latest version of
 * every document first for the same reason.
 */
export function versionOrder(versions: readonly VersionRef[]): VersionRef[] {
  const byArtifact = new Map<string, number[]>();
  for (const version of versions) {
    const numbers = byArtifact.get(version.artifact);
    if (numbers === undefined) byArtifact.set(version.artifact, [version.number]);
    else numbers.push(version.number);
  }
  const queues = [...byArtifact].map(([artifact, numbers]) => ({
    artifact,
    numbers: [...numbers].sort((left, right) => right - left),
  }));
  const rounds = Math.max(0, ...queues.map((queue) => queue.numbers.length));
  const ordered: VersionRef[] = [];
  for (let round = 0; round < rounds; round++) {
    for (const queue of queues) {
      const number = queue.numbers[round];
      if (number !== undefined) ordered.push({ artifact: queue.artifact, number });
    }
  }
  return ordered;
}

async function readCorpus(
  config: DispatchConfig,
  corpus: Corpus,
  projects: readonly string[],
  rate: Rate
): Promise<void> {
  await readUnits(config, corpus, projects.flatMap(projectUnits), rate, "projects");
  const issues = recordedIssues(corpus, projects);
  await readUnits(config, corpus, issues.flatMap(issueUnits), rate, "issues");
  await readUnits(
    config,
    corpus,
    versionOrder(recordedVersions(corpus)).map(versionUnit),
    rate,
    "versions"
  );
}

// --- comparing the two readings ---------------------------------------------------------------

const BLOCK_ID = "blockId";

/** An attribute set with the block id and every null dropped, keys in order; undefined where
 * nothing is left, so a node carrying only a block id compares as one carrying none. */
function canonicalAttrs(
  attrs: Record<string, unknown> | undefined
): Record<string, unknown> | undefined {
  if (attrs === undefined) return undefined;
  const keys = Object.keys(attrs)
    .filter((key) => key !== BLOCK_ID && attrs[key] !== null && attrs[key] !== undefined)
    .sort();
  if (keys.length === 0) return undefined;
  const out: Record<string, unknown> = {};
  for (const key of keys) out[key] = attrs[key];
  return out;
}

function canonicalMarks(marks: readonly PmMark[] | undefined): PmMark[] | undefined {
  if (marks === undefined || marks.length === 0) return undefined;
  const canonical = marks.map((mark) => ({
    type: mark.type,
    ...(canonicalAttrs(mark.attrs) === undefined ? {} : { attrs: canonicalAttrs(mark.attrs) }),
  }));
  // pmdoc.FromJSON sorts a node's marks before it compares them, so both sides are sorted here.
  canonical.sort((left, right) => JSON.stringify(left).localeCompare(JSON.stringify(right)));
  return canonical;
}

/** Gives every short body row of every table the cells Parse pads it with, each taking its header
 * cell's alignment: the canonicalisation pmdoc does to the engine's reading before comparing
 * (differential_test.go's padShortRows). A row holding no cell stays empty. */
export function padShortRows(node: PmNode): PmNode {
  const content = node.content?.map(padShortRows);
  if (node.type !== "table" || content === undefined || content.length === 0) {
    return content === undefined ? node : { ...node, content };
  }
  const header = content[0];
  const width = header.content?.length ?? 0;
  const rows = content.map((row, index) => {
    const cells = row.content ?? [];
    if (index === 0 || cells.length === 0 || cells.length >= width) return row;
    const padded = [...cells];
    while (padded.length < width) {
      padded.push({
        type: "table_cell",
        attrs: {
          alignment: header.content?.[padded.length]?.attrs?.alignment ?? null,
          colspan: 1,
          colwidth: null,
          rowspan: 1,
        },
        content: [{ type: "paragraph" }],
      });
    }
    return { ...row, content: padded };
  });
  return { ...node, content: rows };
}

function canonicalNode(node: PmNode): unknown {
  const attrs = canonicalAttrs(node.attrs);
  const marks = canonicalMarks(node.marks);
  return {
    type: node.type,
    ...(node.text === undefined || node.text === "" ? {} : { text: node.text }),
    ...(attrs === undefined ? {} : { attrs }),
    ...(marks === undefined ? {} : { marks }),
    ...(node.content === undefined || node.content.length === 0
      ? {}
      : { content: node.content.map(canonicalNode) }),
  };
}

/** The tree as the comparison sees it: block ids dropped, null attributes dropped, marks sorted,
 * short table rows padded. */
export function canonicalTree(node: PmNode): string {
  return JSON.stringify(canonicalNode(padShortRows(node)));
}

/** Every `text` node's text, in document order, run together: what a reader is shown, without the
 * blocks and marks it is shown in. */
export function visibleText(node: PmNode): string {
  const parts: string[] = [];
  const walk = (current: PmNode): void => {
    if (typeof current.text === "string") parts.push(current.text);
    for (const child of current.content ?? []) walk(child);
  };
  walk(node);
  return parts.join("");
}

/** Whether `part` is `whole` with characters deleted, equality included: one pass over each, and
 * no allocation, over texts that run to megabytes. `whole` walks by code point and `part` is
 * matched at the same width, so a surrogate pair is one character on both sides. */
export function isSubsequence(part: string, whole: string): boolean {
  let index = 0;
  for (const character of whole) {
    if (index >= part.length) return true;
    if (part.startsWith(character, index)) index += character.length;
  }
  return index >= part.length;
}

export function classify(reading: TextReading): TextClass {
  if (reading.browser === null) return "the browser cannot read it";
  if (reading.go === null) return "Go cannot read it";
  if (canonicalTree(reading.go) === canonicalTree(reading.browser)) return "the two agree";
  const go = visibleText(reading.go);
  const browser = visibleText(reading.browser);
  if (go === browser) return "format differs";
  if (isSubsequence(browser, go)) return "the browser lost text";
  if (isSubsequence(go, browser)) return "Go loses text";
  return "the text differs both ways";
}

// --- the readers' own harnesses ----------------------------------------------------------------

async function run(
  command: readonly string[],
  cwd: string,
  log: string,
  environment: Readonly<Record<string, string>> = {}
): Promise<void> {
  const handle = openSync(log, "w");
  try {
    const child = Bun.spawn([...command], {
      cwd,
      stdout: handle,
      stderr: handle,
      env: { ...process.env, ...environment },
    });
    const code = await child.exited;
    if (code !== 0) {
      throw new Error(`${command.join(" ")} exited ${code}; see ${log}`);
    }
  } finally {
    closeSync(handle);
  }
}
/** Where one stored text stands, without its markdown: all the join needs, and small enough that
 * a hundred thousand of them fit in memory where the texts themselves do not. */
interface TextPlace {
  readonly id: string;
  readonly group: Group;
}

/** Every text of a corpus, in order, by id and group alone. */
function textPlaces(corpus: string): TextPlace[] {
  const places: TextPlace[] = [];
  for (const stored of storedTexts(corpus)) places.push({ id: stored.id, group: stored.group });
  return places;
}

/** Writes the texts `wanted` names as the readers take them: one `{id, md}` a line, in the
 * corpus's own order, which all three readers' outputs then follow line for line. */
function writeReaderCorpus(path: string, corpus: string, wanted: ReadonlySet<string>): void {
  const handle = openSync(path, "w");
  try {
    for (const stored of storedTexts(corpus)) {
      if (!wanted.has(stored.id)) continue;
      const line = `${JSON.stringify({ id: stored.id, md: stored.markdown })}\n`;
      const written = writeSync(handle, line);
      // On a full disk writeSync writes what fits and returns the short count instead of throwing.
      if (written !== Buffer.byteLength(line)) throw new Error(`short write to ${path}`);
    }
  } finally {
    closeSync(handle);
  }
}

/** One line of an engine's output: the reading of the `md` field, `{pm}` or `{err}`, under the
 * name the plain run writes and the name `--read-only` writes. */
interface EngineLine {
  readonly id: string;
  readonly md?: { pm?: PmNode; err?: string } | null;
  readonly md_readonly?: { pm?: PmNode; err?: string } | null;
}

/** One line of the dump test: the display read's two readings, each with its block ids dropped,
 * absent where Node.JSON would not write the tree. */
interface DumpLine {
  readonly id: string;
  readonly display?: PmNode;
  readonly display_version?: PmNode;
}

function engineReading(reading: EngineLine["md"]): PmNode | { error: string } {
  if (reading === null || reading === undefined) return { error: "no reading" };
  if (reading.err !== undefined) return { error: reading.err };
  return reading.pm ?? { error: "no reading" };
}

/**
 * Classes each text of `places` from the three readings one batch directory holds, read in step
 * with it, one line each at a time, so a batch of tens of thousands of documents holds one pair
 * of trees rather than all of them. `visit` is given each text's two readings as they are
 * classed. It runs no reader itself: `classifyCorpus` runs them, and a batch already read can be
 * classed again from its files alone.
 */
export function classifyBatch(
  places: readonly TextPlace[],
  batch: string,
  visit?: (stored: TextPlace, reading: TextReading) => void
): ClassedText[] {
  const editingLines = jsonLines<EngineLine>(join(batch, "engine.jsonl"));
  const readOnlyLines = jsonLines<EngineLine>(join(batch, "engine-readonly.jsonl"));
  const dumpLines = jsonLines<DumpLine>(join(batch, "go-dump.jsonl"));
  const classed: ClassedText[] = [];
  for (const stored of places) {
    const edit = editingLines.next().value;
    const shown = readOnlyLines.next().value;
    const go = dumpLines.next().value;
    if (edit?.id !== stored.id || shown?.id !== stored.id || go?.id !== stored.id) {
      throw new Error(`the readers do not follow the corpus at ${stored.id}`);
    }
    const version = readingMode(stored.group) === "version";
    const browser = engineReading(version ? edit.md : shown.md_readonly);
    const tree = version ? go.display_version : go.display;
    const reading: TextReading = {
      go: tree ?? null,
      browser: "error" in browser ? null : browser,
      ...("error" in browser ? { browserError: browser.error } : {}),
    };
    visit?.(stored, reading);
    classed.push({ ...stored, class: classify(reading) });
  }
  return classed;
}

/** The class each text was put in by a pass before this one. Classing is the long half of a run -
 * hours over a hundred thousand texts on two cores - and a corpus read again has only grown, so a
 * text already decided is not decided twice. Delete the file to decide them all again, which is
 * what a change to either reader, or to the classes, calls for. */
function readVerdicts(path: string): Map<string, TextClass> {
  const verdicts = new Map<string, TextClass>();
  try {
    for (const verdict of jsonLines<ClassedText>(path)) verdicts.set(verdict.id, verdict.class);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
  }
  return verdicts;
}

/**
 * Classes every text of the corpus, running both engines and Go's display read over the texts no
 * pass has classed yet. Each such batch gets a directory of its own under `work`, so the readings
 * of the texts already classed stay where they are and can be read again.
 */
export async function classifyCorpus(corpus: string, work: string): Promise<ClassedText[]> {
  const verdictsPath = join(work, "verdicts.jsonl");
  const verdicts = readVerdicts(verdictsPath);
  const places = textPlaces(corpus);
  const pending = places.filter((place) => !verdicts.has(place.id));
  console.error(
    `readers: ${places.length} texts, ${verdicts.size} already classed, ${pending.length} to read`
  );
  if (pending.length > 0) {
    const batch = join(work, `batch-${new Date().toISOString().replace(/[:.]/gu, "-")}`);
    mkdirSync(batch, { recursive: true });
    const readerCorpus = join(batch, "reader-corpus.jsonl");
    const editing = join(batch, "engine.jsonl");
    writeReaderCorpus(readerCorpus, corpus, new Set(pending.map((place) => place.id)));
    await run(["bun", ENGINE_READER, readerCorpus, editing, "md"], REPO, join(batch, "engine.log"));
    console.error("readers: the editing engine is done");
    await run(
      [
        "bun",
        ENGINE_READER,
        "--read-only",
        readerCorpus,
        join(batch, "engine-readonly.jsonl"),
        "md",
      ],
      REPO,
      join(batch, "engine-readonly.log")
    );
    console.error("readers: the read-only engine is done");
    // The display read is only reachable from inside its own module, so it is run through the
    // differential's dump test, which writes ReadForDisplay's two readings with the block ids
    // dropped. It is one package, never ./... : packages/daemon holds a test that pushes this
    // pod's workspace.
    await run(
      ["go", "test", GO_PACKAGE, "-run", "^TestDifferentialDump$", "-count=1", "-timeout", "0"],
      GO_MODULE,
      join(batch, "go.log"),
      {
        PMDOC_DIFF_CORPUS: readerCorpus,
        PMDOC_DIFF_ENGINE: editing,
        PMDOC_DIFF_OUT: join(batch, "go-dump.jsonl"),
      }
    );
    console.error("readers: the display read is done");
    const read = classifyBatch(pending, batch);
    appendFileSync(verdictsPath, read.map((one) => `${JSON.stringify(one)}\n`).join(""));
    for (const one of read) verdicts.set(one.id, one.class);
  }
  return places.map((place) => ({
    ...place,
    // Every text is either in the verdicts already or in the batch just classed.
    class: verdicts.get(place.id) as TextClass,
  }));
}

// --- the report ---------------------------------------------------------------------------------

/** Each class's count and its split by group, in the order the classes are declared, so a run
 * with nothing in a class still prints that class as zero. */
export function censusReport(
  classed: readonly { id: string; group: Group; class: TextClass }[],
  spent: readonly UnitSummary[],
  classSeconds: number
): CensusReport {
  const classes = CLASSES.map((name) => {
    const rows = classed.filter((row) => row.class === name);
    const groups: Partial<Record<Group, number>> = {};
    for (const row of rows) groups[row.group] = (groups[row.group] ?? 0) + 1;
    return { class: name, texts: rows.length, groups };
  });
  const empty: readonly TextClass[] = [
    "Go loses text",
    "the text differs both ways",
    "Go cannot read it",
  ];
  const ends = spent.map((unit) => unit.at);
  return {
    texts: classed.length,
    requests: spent.reduce((total, unit) => total + unit.requests, 0),
    readSeconds:
      spent.length === 0
        ? 0
        : (Math.max(...ends) - Math.min(...spent.map((unit) => unit.at - unit.ms))) / 1000,
    classSeconds,
    failedUnits: spent.filter((unit) => unit.error !== undefined).length,
    classes,
    groups: GROUPS.map((group) => ({
      group,
      texts: classed.filter((row) => row.group === group).length,
    })),
    defects: classed.filter((row) => empty.includes(row.class)),
  };
}

function printReport(report: CensusReport): void {
  console.log(
    `texts: ${report.texts}  requests: ${report.requests}  ` +
      `reading: ${report.readSeconds.toFixed(0)}s  classing: ${report.classSeconds.toFixed(0)}s  ` +
      `units the server would not answer: ${report.failedUnits}`
  );
  console.log("Classes");
  console.table(
    report.classes.map((count) => ({
      class: count.class,
      texts: count.texts,
      ...count.groups,
    }))
  );
  console.log("Groups");
  console.table(report.groups);
  console.log("Texts in a class that must be empty");
  console.table(report.defects);
}

// --- the guard: no stored text in the repository -------------------------------------------------

/** A paragraph as the guard compares it: every run of whitespace one space, the ends trimmed. A
 * file that re-wrapped a stored paragraph still holds it. */
export function normalizeParagraph(paragraph: string): string {
  return paragraph.replace(/\s+/gu, " ").trim();
}

export function paragraphs(markdown: string): string[] {
  return markdown
    .split(/\n[ \t]*\n/u)
    .map(normalizeParagraph)
    .filter((paragraph) => paragraph.length >= MIN_STORED_PARAGRAPH);
}

/** Where one stored paragraph came from. The paragraph itself is not kept: for a corpus of this
 * size that would be the whole of production's prose in memory, and the head it is indexed by
 * already names it. */
export interface StoredParagraph {
  readonly id: string;
  readonly group: Group;
}

/**
 * Every stored paragraph by its first MIN_STORED_PARAGRAPH characters, so one file is searched in
 * one pass over its own characters rather than once for each of a hundred thousand paragraphs.
 * That head is what a file is held to: that many characters of production's prose, run together,
 * is a quotation however the file goes on after them.
 */
export function storedParagraphs(texts: Iterable<StoredText>): Map<string, StoredParagraph[]> {
  const index = new Map<string, StoredParagraph[]>();
  for (const stored of texts) {
    for (const paragraph of paragraphs(stored.markdown)) {
      const head = paragraph.slice(0, MIN_STORED_PARAGRAPH);
      const holders = index.get(head);
      if (holders === undefined) index.set(head, [{ id: stored.id, group: stored.group }]);
      else if (!holders.some((holder) => holder.id === stored.id)) {
        holders.push({ id: stored.id, group: stored.group });
      }
    }
  }
  return index;
}

export interface StoredTextHit {
  readonly path: string;
  /** The stored text the paragraph came from. */
  readonly id: string;
  readonly group: Group;
  /** Where in the file the match begins, in normalised characters. */
  readonly offset: number;
}

/** Every stored paragraph the file opens, wherever it stands: each window of the file's
 * normalised text is looked up, so a paragraph quoted inside a longer one is found too. */
export function storedTextIn(
  path: string,
  content: string,
  index: ReadonlyMap<string, readonly StoredParagraph[]>
): StoredTextHit[] {
  const normalized = normalizeParagraph(content);
  const hits: StoredTextHit[] = [];
  const seen = new Set<string>();
  for (let offset = 0; offset + MIN_STORED_PARAGRAPH <= normalized.length; offset++) {
    const holders = index.get(normalized.slice(offset, offset + MIN_STORED_PARAGRAPH));
    if (holders === undefined) continue;
    for (const stored of holders) {
      if (seen.has(stored.id)) continue;
      seen.add(stored.id);
      hits.push({ path, id: stored.id, group: stored.group, offset });
    }
  }
  return hits;
}

function checkNoStoredText(files: readonly string[], corpus: string): number {
  const index = storedParagraphs(storedTexts(corpus));
  const hits = files.flatMap((path) => storedTextIn(path, readFileSync(path, "utf8"), index));
  for (const hit of hits) {
    console.error(
      `${hit.path}: holds a paragraph of the stored text ${hit.id} (${hit.group}), ` +
        `at character ${hit.offset} of the file's text`
    );
  }
  console.log(
    `--check-no-stored-text: ${files.length} file${files.length === 1 ? "" : "s"} against ` +
      `${index.size} stored paragraphs ` +
      `of at least ${MIN_STORED_PARAGRAPH} characters, from ${corpus}: ` +
      `${hits.length === 0 ? "no stored text" : `${hits.length} hits`}`
  );
  return hits.length === 0 ? 0 : 1;
}

// --- the run ---------------------------------------------------------------------------------

function parseArguments(argv: readonly string[]): RunOptions {
  const { values } = parseArgs({
    args: [...argv],
    options: {
      project: { type: "string", multiple: true },
      corpus: { type: "string" },
      work: { type: "string" },
      rate: { type: "string" },
      json: { type: "boolean" },
      "no-fetch": { type: "boolean" },
      "fetch-only": { type: "boolean" },
      "check-no-stored-text": { type: "string", multiple: true },
    },
    strict: true,
  });
  const work = values.work ?? join(process.env.TMPDIR ?? "/tmp", "display-read-census");
  const rate = values.rate === undefined ? DEFAULT_RATE : Number(values.rate);
  if (!Number.isFinite(rate) || rate <= 0 || rate > DEFAULT_RATE) {
    throw new Error(
      `--rate must be a positive number of requests a second, at most ${DEFAULT_RATE}`
    );
  }
  const checkFiles = values["check-no-stored-text"] ?? [];
  if (checkFiles.includes("")) throw new Error("--check-no-stored-text requires a file path");
  return {
    json: values.json === true,
    fetch: values["no-fetch"] !== true && checkFiles.length === 0,
    fetchOnly: values["fetch-only"] === true,
    projects: values.project ?? [],
    corpus: values.corpus ?? join(work, "corpus.jsonl"),
    work,
    rate,
    checkFiles,
  };
}

function dispatchConfig(): DispatchConfig {
  const { dispatch: resolution, warnings } = sessionDispatchSettings(
    sessionConfigFile("command", process.env),
    process.env,
    { cwd: process.cwd() }
  );
  // A checkout's OpenCode-era file, which is not read, is said on every run while it is there.
  for (const warning of warnings) console.error(`dispatch: ${warning}`);
  if (!resolution.enabled) {
    throw new Error(
      resolution.error === null
        ? `Dispatch is not configured: ${resolution.notice}`
        : `dispatch config: ${resolution.error}`
    );
  }
  return resolution;
}

async function main(options: RunOptions): Promise<number> {
  mkdirSync(options.work, { recursive: true });
  const corpus = new Corpus(options.corpus);
  if (options.checkFiles.length > 0) {
    return checkNoStoredText(options.checkFiles, options.corpus);
  }
  if (options.fetch) {
    const config = dispatchConfig();
    const rate = new Rate(options.rate);
    const projects =
      options.projects.length > 0
        ? options.projects
        : (await get<{ key: string }[]>(config, "/api/v1/projects", rate, { requests: 0 })).map(
            (project) => project.key
          );
    console.error(`census: ${projects.length} projects into ${options.corpus}`);
    await readCorpus(config, corpus, projects, rate);
  }
  if (options.fetchOnly) {
    const spent = [...corpus.done.values()];
    console.error(
      `census: ${spent.length} units, ` +
        `${spent.reduce((total, unit) => total + unit.requests, 0)} requests, in ${options.corpus}`
    );
    return 0;
  }
  const started = Date.now();
  const classed = await classifyCorpus(options.corpus, options.work);
  const report = censusReport(classed, [...corpus.done.values()], (Date.now() - started) / 1000);
  // One object and nothing else on stdout under --json, so a program can read the whole of it.
  if (options.json) console.log(JSON.stringify(report));
  else printReport(report);
  return 0;
}

if (import.meta.main) process.exit(await main(parseArguments(Bun.argv.slice(2))));
