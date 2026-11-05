// The sourcing lint: the one check AskGate makes without a model.
//
// Why it is mechanical. The costliest failure the reflect run of 2026-11-05 measured is a claim
// with no source: 47 findings across 22 sessions. The instruction against it is already in every
// session's prompt and the rate rose anyway. Asking a model "does every factual claim name what it
// was read from?" does not help, because the defect is not a forgotten citation: it is a session
// that cannot tell its own inference from an observation, and such a session answers that question
// "yes" as confidently as it wrote the sentence. So this check asks nothing. It matches text, it
// cannot be argued out of a finding, it costs no call, and — the reason it exists here rather than
// in the charter prompt — it has no provider dependency, so it runs for a session the model review
// skips.
//
// What it does: a sentence that asserts a measurement (a count, a ratio, a percentage, a status, a
// does/does-not, a claim about what a file or symbol does) and carries no citation token (a path, a
// sha, a ref, a backticked command, a URL, a dispatch key) is handed back to the sender to source or
// to label. Nothing is refused and nothing is blocked.
//
// What it does NOT do, stated so no reader concludes the opposite: it says nothing about a claim
// that IS sourced. A confidently wrong sentence that names a command nobody re-ran passes it
// untouched. Only a reader who re-runs the command catches those. This ships the check that cannot
// be fooled, not the one that would catch more.
//
// A leaf module: it imports nothing of the gate, as `text.ts` does not.

/** One sentence the lint flags, with the trigger that put it there. */
export interface SourcingFlag {
  /** The sentence as written, clipped for the notice. */
  sentence: string;
  /** Which assertion kind matched: what the sender has to source. */
  kind: "number" | "status";
}

/** How much of one flagged sentence the notice carries. */
const SENTENCE_MAX = 220;
/** How many flagged sentences the notice names; the count says how many more there are. */
export const SOURCING_FLAG_CAP = 6;
/** Below this a "sentence" is a fragment (a table cell, a heading word), never an assertion. */
const MIN_SENTENCE_CHARS = 25;
/**
 * Above this it is not a sentence either: a pasted log, a base64 blob, a minified line. Nothing a
 * reader reads as one claim runs this long, and the cap is what keeps the lint's cost flat on a
 * write whose body is 70 KiB of one character.
 */
const MAX_SENTENCE_CHARS = 1_000;
/** How much of a write the lint reads at all; a Dispatch body past this is already clipped. */
const MAX_TEXT_CHARS = 512 * 1024;

/**
 * A citation token: something a reader can go and check.
 *
 * Deliberately generous. A false negative (a sentence with a token that sources nothing) costs the
 * reader nothing — the gate said nothing about it, as it says nothing about most text. A false
 * positive costs the sender a sentence they must defend, which is how a mechanical check gets turned
 * off. So anything that even looks checkable counts.
 */
const CITATIONS: readonly RegExp[] = [
  /`[^`\n]+`/, // a backticked command, path, symbol or literal
  /\bhttps?:\/\//i, // a URL
  /\b[a-z][a-z0-9+.-]*:\/\//i, // dispatch://, agent://, pr://, issue://, local://
  /\b[A-Z][A-Z0-9]{1,9}-\d+\b/, // a dispatch key (INFRA-515, BET-571)
  /\b[\w.-]+\/[\w.-]+#\d+\b/, // owner/repo#123, the way a cross-repository reference is written
  /(?:^|[\s([])#\d+\b/, // a pull request or issue number
  /\b(?:PR|pull request|issue|commit)\s+#?\d+\b/i,
  // A sha: a whole token of seven to forty hex characters that is not all digits, since a plain
  // number is the thing being claimed, not the source of itself. One bounded quantifier with no
  // alternation inside it: the earlier `[0-9a-f]*[a-f][0-9a-f]*` spelling backtracked over a long
  // run of hex characters, and a 70 KiB body of `b` took the lint past a test's five-second budget.
  /\b(?![0-9]+\b)[0-9a-f]{7,40}\b/,
  /[\w.@-]+\.(?:ts|tsx|js|jsx|json|jsonl|md|mdx|py|rs|go|java|rb|c|h|cpp|yaml|yml|toml|sh|sql|lean|thy|txt|log|csv|kdl|lock)\b/, // a file name
  /(?:^|[\s("'])(?:\.{1,2})?\/[\w.@-]+\/[\w.@/-]+/, // an absolute or relative path of two segments
  /\b[\w-]+\/[\w-]+\/[\w.@/-]+/, // owner/repo/path, packages/x/y
  /\$\s+\S+/, // a shell prompt line
  /§\s*\d|\b(?:section|entry|slice|row|table|figure|appendix|step)\s+\d/i, // a pointer into the document itself
];

/**
 * A sentence the writer has already marked as something other than a measurement they took: an
 * inference, an estimate, a conditional, a requirement, a plan. The lint asks a sender to source a
 * claim or to label it; a sentence that is already labelled has nothing left to do.
 */
const HEDGES: readonly RegExp[] = [
  /\[(?:inference|estimate|estimated|projection|projected|guess|rough|unverified)\b/i,
  /\b(?:i|we)\s+(?:think|believe|suspect|expect|assume|guess)\b/i,
  /\b(?:probably|likely|unlikely|perhaps|maybe|presumably|apparently|seemingly)\b/i,
  /\b(?:appears?|seems?|looks?)\s+(?:to|like|as)\b/i,
  /\b(?:unverified|unmeasured|untested|unchecked|not\s+(?:yet\s+)?(?:verified|measured|tested|checked|confirmed))\b/i,
  /\b(?:my|our)\s+(?:guess|reading|understanding|assumption)\b/i,
  /\b(?:would|could|might|may|should)\b/i, // a conditional or a proposal, not a measurement
  /\b(?:possible|possibly|potential|potentially|hypothetical)\b/i,
  /\b(?:unsure|uncertain|low\s+confidence|chance\s+(?:that|some|of)|we\s+will|i\s+will)\b/i,
  /\b(?:propose|proposal|plan\s+to|intend\s+to|suggest|must|needs?\s+to|requires?)\b/i,
  /\b(?:estimate[ds]?|estimating|projects?\s+to|projecting|forecast|extrapolat)\w*\b/i,
  /\b(?:about|roughly|approximately|around|up\s+to|at\s+most|at\s+least|target)\s*[~<>]?\s*\d/i,
  /[~≈]\s*\d/, // a tilde or an "approximately equal" figure is an approximation
  /^(?:if|when|once|unless|suppose)\b/i, // a conditional, not a report of what happened
  /\bdone\s+means\b/i, // an acceptance criterion
  // An instruction is not a report. "Run the full ladder of seven rungs" names a plan; a sentence
  // that opens with a bare imperative has nothing for a reader to go and re-run yet.
  /^(?:run|add|use|check|write|build|ship|keep|make|do|set|send|start|stop|take|put|read|fix|drop|move|hold|tell|ask|give)\b/i,
];

/**
 * A number that asserts a measurement. A bare year, a version, a time of day, an ordinal or a
 * money figure is not one: those name things rather than count them, and flagging them is how a
 * mechanical check earns its reputation for noise. So the number is read only beside a unit or a
 * counted noun.
 */
const NUMBER_CLAIM =
  /(?:^|[^\w.$-])(?:\d{1,3}(?:,\d{3})+|\d+(?:\.\d+)?)\s*(?:%|percent\b|per\s+cent\b|of\s+(?:the\s+)?\d|\/\s*\d|(?:sessions?|records?|calls?|runs?|files?|tests?|lines?|rows?|commits?|issues?|asks?|comments?|messages?|findings?|cases?|instances?|occurrences?|errors?|failures?|jobs?|agents?|reviewers?|models?|tokens?)\b)/i;
/**
 * A pass, a failure or a state a run reached, asserted of something that is run or built.
 *
 * Two narrowings, each measured against real writes. The subject must be a thing that runs: "the
 * suite passed" is a result a reader can re-run, "the plan works" is an opinion. And the verb must
 * report a result that already happened: present-tense prose ("a target whose proof fails scores
 * 0", "it flags 10% or more") describes a design, and flagging designs was the largest source of
 * false positives when this was measured — seven of the seven status false positives in a
 * hand-read sample of sixty flags were present-tense descriptions.
 */
const STATUS_SUBJECT =
  /\b(?:tests?|suite|checks?|builds?|runs?|jobs?|ci|pipeline|lint|typecheck|compile|proofs?|control|arm|probe|repro|gate|smoke|attempts?|cases?)\b/i;
const STATUS_VERB =
  /\b(?:passed|failed|errored|succeeded|timed\s+out|crashed|hung|flaked|went\s+(?:red|green)|(?:is|are|was|were)\s+(?:green|red|broken|flaky))\b/i;

const KINDS: ReadonlyArray<[SourcingFlag["kind"], (sentence: string) => boolean]> = [
  ["number", (sentence) => NUMBER_CLAIM.test(sentence)],
  ["status", (sentence) => STATUS_VERB.test(sentence) && STATUS_SUBJECT.test(sentence)],
];

/** Whether the sentence carries something a reader can go and check. */
export const hasCitation = (sentence: string) =>
  CITATIONS.some((pattern) => pattern.test(sentence));

/**
 * The text split into candidate sentences, with what no reader reads as prose left out: fenced
 * code, an indented code block, a quoted line (someone else's words), a heading, and a markdown
 * table's rows.
 *
 * Table rows are left out, and it is the lint's largest blind spot: a table is where a report puts
 * its numbers. The carve-out is measured, not assumed. Reading each **cell** as a sentence flagged
 * whole tables at once. Reading each **row** — flag a row holding a measurement when no cell of it
 * cites anything — was built and measured over the same 1,200 real reviewed writes: 5,146 rows held
 * a measurement, 3,451 of them (67%) were silenced by a citation somewhere in the row, and 1,695
 * were flagged, taking the lint from 1.27 to 2.68 flags per call. Sixty of those row flags were
 * hand-read: 26 were false, 43%, against 13% on prose. They fell into three shapes — a row that
 * names its subject in a way no citation pattern recognises (`openssl/punycode | fix104 | 7 PASS`,
 * a job id, `copy#786`), a plan or enumeration table whose numeric cell is a section number or a
 * cost, and a frequency table where one source covers every row and is stated once in the prose
 * beside it. The other end is no better: 1,364 of the 1,695 (80.5%) carry a cell that names what
 * the row measures, so counting that cell as the row's source leaves 331 flags, 0.28 per call,
 * almost all of them pure-number benchmark rows. The claim and its source do not sit at the same
 * granularity in a table, which is why this is a gap and not a bug. A measurement that must be
 * checked belongs in a sentence too.
 */
export function sentences(text: string): string[] {
  const out: string[] = [];
  // A paragraph of prose, gathered across the lines a writer wrapped it over. Splitting on the
  // line was the first thing tried and it is wrong: a report hard-wrapped at about 95 columns
  // turns every long claim into fragments, and a fragment carrying a number but not the clause
  // that sourced it is a false positive the lint cannot defend.
  let para: string[] = [];
  const flush = () => {
    if (para.length === 0) return;
    const joined = para.join(" ");
    para = [];
    for (const piece of joined.split(/(?<=[.!?])\s+(?=["'`([]?[A-Z0-9])/)) {
      const sentence = piece.trim();
      if (sentence.length >= MIN_SENTENCE_CHARS && sentence.length <= MAX_SENTENCE_CHARS)
        out.push(sentence);
    }
  };
  let fenced = false;
  // A real newline, and the two characters `\n` as well: a write assembled from a JSON string
  // carries its line breaks escaped, and a line left joined runs two claims into one flag.
  for (const rawLine of text.slice(0, MAX_TEXT_CHARS).split(/\n|\\n/)) {
    const line = rawLine.trim();
    if (/^(?:```|~~~)/.test(line)) {
      flush();
      fenced = !fenced;
      continue;
    }
    // Everything a reader does not read as this writer's own prose ends the paragraph and is
    // dropped: a blank line, fenced or indented code, a quotation, a heading, a table row (and a
    // line of pipe-separated cells a renderer lays out as one).
    if (
      fenced ||
      line === "" ||
      /^(?: {4,}|\t)/.test(rawLine) ||
      line.startsWith(">") ||
      /^#{1,6}\s/.test(line) ||
      line.startsWith("|") ||
      /\S\s*\|\s*\S.*\S\s*\|\s*\S/.test(line)
    ) {
      flush();
      continue;
    }
    // A list item starts its own paragraph and its marker is not part of the sentence.
    const item = /^(?:[-*+]|\d+[.)])\s+/.exec(line);
    if (item !== null) {
      flush();
      para.push(line.slice(item[0].length));
      continue;
    }
    para.push(line);
  }
  flush();
  return out;
}

/**
 * Every sentence of `text` that asserts a measurement and names nothing a reader can check.
 * A question is left alone (it asks rather than claims), and so is a sentence the writer already
 * marked unverified.
 */
export function sourcingLint(text: string): SourcingFlag[] {
  const flags: SourcingFlag[] = [];
  for (const sentence of sentences(text)) {
    if (sentence.endsWith("?")) continue;
    // Matched without markdown emphasis, kept with it: `roughly **167 attempts**` is hedged, and
    // the asterisks stand between the hedge and its number in every pattern that reads one.
    const plain = sentence.replace(/[*_]{1,3}/g, "");
    if (hasCitation(plain)) continue;
    if (HEDGES.some((pattern) => pattern.test(plain))) continue;
    const match = KINDS.find(([, asserts]) => asserts(plain));
    if (match !== undefined) flags.push({ sentence, kind: match[0] });
  }
  return flags;
}

/** Every string in a tool call's arguments, in the order the arguments hold them. */
export function proseOf(input: unknown): string {
  const parts: string[] = [];
  const walk = (value: unknown): void => {
    if (typeof value === "string") parts.push(value);
    else if (Array.isArray(value)) for (const item of value) walk(item);
    else if (value !== null && typeof value === "object")
      for (const item of Object.values(value)) walk(item);
  };
  walk(input);
  return parts.join("\n");
}

const WHAT: Record<SourcingFlag["kind"], string> = {
  number: "a count or a ratio",
  status: "a pass, a failure or a state",
};

/**
 * The note handed back to the sender, or undefined when nothing was flagged. It reports; it never
 * refuses, so the write goes out either way and the sender decides.
 */
export function renderSourcingNotice(flags: readonly SourcingFlag[]): string | undefined {
  if (flags.length === 0) return undefined;
  const shown = flags.slice(0, SOURCING_FLAG_CAP);
  const lines = shown.map(
    ({ sentence, kind }) =>
      `- ${WHAT[kind]}: ${sentence.length <= SENTENCE_MAX ? sentence : `${sentence.slice(0, SENTENCE_MAX - 1)}…`}`
  );
  const more =
    flags.length > shown.length ? `\n…and ${flags.length - shown.length} more like them.` : "";
  return [
    `<advisor-gate advisor="AskGate" check="sourcing">`,
    `${flags.length} sentence${flags.length === 1 ? "" : "s"} in this write assert${flags.length === 1 ? "s" : ""} something measured and name nothing a reader can check — no path, sha, ref, command, URL or dispatch key:`,
    ...lines,
    `${more}`,
    "This blocks nothing; the write went as you sent it. For each one, either name what you read it from, or mark it as what it is — an inference, not an observation. The gate cannot tell the two apart, which is why it asks rather than refuses, and it says nothing at all about a wrong claim that is sourced.",
    "</advisor-gate>",
  ]
    .filter((line) => line !== "")
    .join("\n");
}
