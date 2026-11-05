/**
 * Every word the issue page says of its own accord, and every shape it draws beside one
 * (INFRA-1196 R21, `dispatch://INFRA-1196/spec#b-r-plain-words`). Nothing the page draws names
 * itself in an internal word: a type name, a state word, a preset name, a control label, an
 * empty-state line and an attribute's own `when` value all come from here, so a word a reader
 * sees is a word somebody chose rather than a schema token, a slug or an enumeration case.
 *
 * The page divides its own words from the author's with two attributes:
 *
 * - every element whose visible text is the page's own vocabulary carries
 *   `PAGE_CHROME_ATTRIBUTE`;
 * - every element whose visible text came from a person or an agent carries
 *   `AUTHOR_TEXT_ATTRIBUTE`: a node's own text, an issue or child title, a check's `run`
 *   command, a label value, an author's name.
 *
 * A reader of the page's words therefore takes each `[data-page-chrome]` element's text with
 * every `[data-author-text]` descendant's text removed, and every word that survives is in
 * `DECLARED_WORDS`.
 *
 * A shape is an inline `<svg>` carrying its `GLYPH_TOKENS` token in `data-glyph` and its own
 * accessible name from `GLYPH_NAMES`, drawn beside the word rather than in place of it: a state
 * and a type are told apart by a word and a shape, never by colour alone.
 *
 * This module is data and pure functions. It holds no React, touches no DOM and imports
 * nothing, so a test, a browser spec and a component all read the same list.
 */

/**
 * Freezes `value` and everything it holds. Each declaration below is `as const`, which is the
 * compiler's half of the promise; this is the runtime half, so no caller can write a word into
 * the page's vocabulary after the module loads.
 */
function freeze<T>(value: T): T {
  for (const held of Object.values(value as Record<string, unknown>)) {
    if (typeof held === "object" && held !== null) {
      freeze(held);
    }
  }
  return Object.freeze(value);
}

/** The keys of `value`, as its own key type rather than as plain strings. */
function keysOf<T extends object>(value: T): readonly (keyof T)[] {
  return Object.keys(value) as (keyof T)[];
}

/**
 * A plain name for every kind of node the page draws, which is every kind the model holds: the
 * issue, the documents and images it carries, the five typed blocks of those documents, the
 * decisions and asks, the conversation, the results recorded against a check, and the child
 * issues.
 *
 * A key is the model's own token for the kind, so the page needs no second table to read a
 * node: `callout`, `ask`, `requirement`, `plan_task` and `check` are the block type names in
 * `packages/envoy/internal/dispatch/pmdoc/schema/blocks.json`, and `ask`, `comment` and
 * `message` are the item kinds of a `dispatch://` reference. A name is lowercase because the
 * page draws it inside its own sentences and rows, where a capital would read as a proper noun;
 * `sentenceCase` supplies the capital where a name opens a sentence or a heading.
 */
export const NODE_TYPE_NAMES = freeze({
  ask: "ask",
  callout: "callout",
  check: "check",
  child_issue: "child issue",
  comment: "comment",
  decision: "decision",
  document: "document",
  handoff: "handoff",
  image: "image",
  issue: "issue",
  message: "message",
  plan_task: "plan task",
  requirement: "requirement",
  result: "result",
} as const);

/** One kind of node the page draws, by the model's own token for it. */
export type NodeType = keyof typeof NODE_TYPE_NAMES;

/**
 * The state words of each type whose state the model holds, and no entry at all for a type it
 * gives no state: a comment is not in a state, so the page has no word to draw for one, and an
 * entry holding an empty table would invite a row to draw a blank badge.
 *
 * The ask's three states are the `state` choices of the `ask` block (`blocks.json`). The
 * check's five are `checkState`'s (`features/verification/VerificationTab.tsx`), and the page
 * must tell all five apart, so each keeps a word of its own.
 */
export const NODE_STATE_WORDS = freeze({
  ask: {
    answered: "Answered",
    open: "Open",
    resolved: "Resolved",
  },
  check: {
    failed: "Failed",
    "no-result": "No result",
    older: "Older",
    passed: "Passed",
    unmarked: "Unmarked",
  },
} as const);

/** How an ask stands, by the `state` the server writes into its block. */
export type AskState = keyof typeof NODE_STATE_WORDS.ask;

/** How a check stands, from the newest result recorded against it. */
export type CheckState = keyof typeof NODE_STATE_WORDS.check;

/** Every node kind, in one list, for a caller that walks them all. */
export const NODE_TYPES: readonly NodeType[] = freeze(keysOf(NODE_TYPE_NAMES));

/**
 * The state words of `type`, or `undefined` when the model gives that type no state. The page
 * draws no badge for an answer of `undefined`: it is the difference between a node whose state
 * nobody recorded and a node that has no state to record.
 */
export function nodeStateWords(type: NodeType): Readonly<Record<string, string>> | undefined {
  const byType: Partial<Record<NodeType, Readonly<Record<string, string>>>> = NODE_STATE_WORDS;
  return byType[type];
}

/**
 * The word for `state` of a node of `type`, or `undefined` when the type has no state or the
 * state is not one the module declares.
 */
export function nodeStateWord(type: NodeType, state: string): string | undefined {
  return nodeStateWords(type)?.[state];
}

/**
 * The name of the node kind `token`, which is `NODE_TYPE_NAMES`' word for a kind the module
 * declares. A kind it does not declare reads as its token with each underscore as a space:
 * a person can read that, and it is the fallback rather than the answer, because
 * `page-words.test.ts` reads the block schema and fails until the kind has a word here.
 */
export function nodeTypeName(token: string): string {
  const names: Readonly<Record<string, string>> = NODE_TYPE_NAMES;
  return names[token] ?? token.replaceAll("_", " ");
}

/** The name at the start of a sentence or a heading: its first letter capitalised, nothing else
 *  touched, so "child issue" opens a heading as "Child issue". */
export function sentenceCase(name: string): string {
  return name === "" ? name : `${name[0]?.toUpperCase() ?? ""}${name.slice(1)}`;
}

/** The five presets in the order the row draws them (R3). The row is a query over one tree, so
 *  the order is the reader's reading order: the whole tree, then what was decided, then what is
 *  checked, then the spec and the plan the tree carries. */
export const PRESET_ORDER = freeze(["tree", "decisions", "checks", "spec", "plan"] as const);

/** One preset of the issue page, by its own token, which is also its address. */
export type Preset = (typeof PRESET_ORDER)[number];

/** The name of each preset. "Checks" rather than "Verification", because verification names an
 *  activity and the preset draws a thing (R15). */
export const PRESET_NAMES = freeze({
  checks: "Checks",
  decisions: "Decisions",
  plan: "Plan",
  spec: "Spec",
  tree: "Tree",
} as const satisfies Record<Preset, string>);

/**
 * Every line the page draws where a place holds nothing, one per situation: a preset whose
 * query matches nothing, an issue with no children, a spec with no summary, a check with no
 * result, and a folded branch that turns out to hold nothing. Each is one sentence, in the
 * simple present, saying what is absent rather than what the reader should do about it.
 */
export const EMPTY_STATE_LINES = freeze({
  check_result: "No result recorded against this check.",
  checks: "This issue has no checks yet.",
  children: "This issue has no child issues.",
  decisions: "This issue has no decisions yet.",
  folded_branch: "This branch holds nothing.",
  plan: "This issue has no plan tasks yet.",
  spec: "This issue has no requirements yet.",
  spec_summary: "This spec has no summary.",
  tree: "This issue holds nothing yet.",
} as const);

/**
 * Each `when` value of a check block as words (R15). The values are the `when` choices of the
 * `check` block in `blocks.json`, which `CheckWhen` in `packages/contracts/src/dispatch-api.ts`
 * mirrors; `page-words.test.ts` reads that schema, so a new choice fails until it has words
 * here.
 */
export const CHECK_WHEN_WORDS = freeze({
  after_merge: "after the merge",
  before_merge: "before the merge",
  default_branch: "on the default branch",
} as const);

/** What a recorded result says about itself beside its verdict: whether it still speaks for the
 *  work as it stands. A result whose currency Dispatch could not judge draws no word at all, so
 *  `unknown` has none. */
export const RESULT_CURRENCY_WORDS = freeze({
  current: "Current",
  older: "Older",
} as const);

/**
 * Every label the page's own controls and headings carry. They live here rather than inside a
 * component so `DECLARED_WORDS` stays derived: a label a component writes inline would be a
 * word the page draws and the list does not hold.
 *
 * Each label names what the control does to this page, in a plain verb or a plain noun. The
 * values a control acts on - a title, a label, an author's name - are the author's words and
 * carry `AUTHOR_TEXT_ATTRIBUTE` instead.
 */
export const CONTROL_LABELS = freeze({
  /** The one control that reaches a property with no value yet (R11, which bans "more"). */
  add: "Add",
  /** The other requirements a transcluded node is also drawn under (R23). */
  also_under: "Also under",
  /** The collapsible section of child issues at the top of every preset (R10). */
  children: "Children",
  /** The direct children done of total, labelled on the collapsed header (R10). */
  children_done: "Children done",
  /** Copies a check's `run` command, which renders as code beside it (R15). */
  copy_command: "Copy the command",
  /** Takes a version of an artifact down (R16). */
  download: "Download",
  /** Closes a branch of the tree (R1). */
  fold: "Fold",
  /** Moves to the next open question that waits on the reader (R6). */
  next_question: "Next open question",
  /** Marks a document or an artifact in the node menu that no block cites (R16). */
  nothing_cites_it: "Cited by nothing",
  /** The node menu beside the presets, holding what no block cites and the upload (R16). */
  other_nodes: "Files and documents",
  /** The whole subtree done of total, labelled beside the direct count (R10). */
  subtree_done: "Whole tree done",
  /** Opens a branch of the tree (R1). */
  unfold: "Unfold",
  /** Adds an artifact from the node menu (R16). */
  upload: "Upload",
  /** Every version an artifact has (R16). */
  versions: "Versions",
  /** Names the author of a node whose author the model holds (R7); the name itself is the
   *  author's own text. */
  written_by: "Written by",
} as const);

/**
 * A plain label for every attribute a typed block can draw in its header, keyed by the
 * attribute's own name in `blocks.json`. The server-owned ones are here too, because a block
 * whose type the schema no longer describes draws every attribute it carries
 * (`renderTypedBlock`, `features/doc/typed-block-drawing.ts`).
 *
 * A label is lowercase, like the block name it sits under, and the stylesheet adds the colon.
 */
export const BLOCK_ATTRIBUTE_LABELS = freeze({
  answer: "answer",
  answered_at: "answered at",
  answered_by: "answered by",
  copied_from: "copied from",
  copied_from_document: "copied from document",
  /** Why the block cannot be answered as written; "invalid" names a verdict, not the reason. */
  invalid: "problem",
  kind: "kind",
  /** Whether the reader may choose more than one option. */
  multiple: "more than one answer",
  needs: "needs",
  negative_control: "negative control",
  run: "run",
  selected: "selected",
  /** The requirements a check or a plan task is there for; the spec's own verb for the edge. */
  serves: "serves",
  state: "state",
  surface: "surface",
  title: "title",
  urgency: "urgency",
  when: "when",
} as const);

/**
 * The label of the block attribute `name`. An attribute the module does not declare reads as
 * its own name with each underscore as a space, which a person can read; it is the fallback
 * rather than the answer, because `page-words.test.ts` reads the block schema and fails until
 * the attribute has a label here.
 */
export function blockAttributeLabel(name: string): string {
  const labels: Readonly<Record<string, string>> = BLOCK_ATTRIBUTE_LABELS;
  return labels[name] ?? name.replaceAll("_", " ");
}

/**
 * The `data-glyph` token of each shape the page draws: one per node type, and one per state of
 * the two types that have states. No two types share a token and no two states of one type's
 * surface share one, so a reader who tells shapes apart tells the nodes apart; a state's token
 * carries its type, which keeps the check's five states distinct from the ask's three as well.
 */
export const GLYPH_TOKENS = freeze({
  states: {
    ask: {
      answered: "ask-answered",
      open: "ask-open",
      resolved: "ask-resolved",
    },
    check: {
      failed: "check-failed",
      "no-result": "check-no-result",
      older: "check-older",
      passed: "check-passed",
      unmarked: "check-unmarked",
    },
  },
  types: {
    ask: "ask",
    callout: "callout",
    check: "check",
    child_issue: "child-issue",
    comment: "comment",
    decision: "decision",
    document: "document",
    handoff: "handoff",
    image: "image",
    issue: "issue",
    message: "message",
    plan_task: "plan-task",
    requirement: "requirement",
    result: "result",
  },
} as const satisfies {
  readonly states: {
    readonly ask: Record<AskState, string>;
    readonly check: Record<CheckState, string>;
  };
  readonly types: Record<NodeType, string>;
});

function glyphNames(): Readonly<Record<string, string>> {
  const names: Record<string, string> = {};
  for (const type of NODE_TYPES) {
    names[GLYPH_TOKENS.types[type]] = NODE_TYPE_NAMES[type];
  }
  for (const state of keysOf(NODE_STATE_WORDS.ask)) {
    names[GLYPH_TOKENS.states.ask[state]] = `${NODE_STATE_WORDS.ask[state]} ${NODE_TYPE_NAMES.ask}`;
  }
  for (const state of keysOf(NODE_STATE_WORDS.check)) {
    names[GLYPH_TOKENS.states.check[state]] =
      `${NODE_STATE_WORDS.check[state]} ${NODE_TYPE_NAMES.check}`;
  }
  return names;
}

/**
 * The accessible name of each glyph, by its token: a shape carries its own name, because a
 * reader who cannot see the shape still has to hear what it says. A type's shape is named by
 * the type; a state's shape by its state word and the type it stands on, as in "Passed check".
 */
export const GLYPH_NAMES: Readonly<Record<string, string>> = freeze(glyphNames());

/** The attribute every element carries whose visible text is the page's own vocabulary. */
export const PAGE_CHROME_ATTRIBUTE = "data-page-chrome";

/** The attribute every element carries whose visible text came from a person or an agent. */
export const AUTHOR_TEXT_ATTRIBUTE = "data-author-text";

/**
 * The words of `text`, as the page's vocabulary counts them: lowercase, each run of letters and
 * digits a word, an apostrophe inside a word kept, and every other character a break. A reader
 * of the page splits drawn text the same way, so the two sides compare word for word.
 */
export function wordsIn(text: string): readonly string[] {
  return text.toLowerCase().match(/[\p{L}\p{N}]+(?:'[\p{L}\p{N}]+)*/gu) ?? [];
}

/** Every string `value` holds, however deeply, in the order it holds them. */
function phrasesIn(value: unknown): readonly string[] {
  if (typeof value === "string") {
    return [value];
  }
  if (typeof value === "object" && value !== null) {
    return Object.values(value).flatMap(phrasesIn);
  }
  return [];
}

/**
 * Everything the page's own chrome may say, as the declarations above say it. `DECLARED_WORDS`
 * is derived from this list and from nothing else, so a word added to a name, a label or a line
 * joins the set by itself, and a word added nowhere is not in it.
 */
const WORD_SOURCES: readonly unknown[] = [
  NODE_TYPE_NAMES,
  NODE_STATE_WORDS,
  PRESET_NAMES,
  EMPTY_STATE_LINES,
  CHECK_WHEN_WORDS,
  RESULT_CURRENCY_WORDS,
  CONTROL_LABELS,
  BLOCK_ATTRIBUTE_LABELS,
];

/**
 * Every word the page's own chrome may show, lowercase and sorted, with no word that appears in
 * none of the declarations above. A reader of the page checks the text of each
 * `[data-page-chrome]` element against this list, so an internal word drawn where a name
 * belongs has nowhere to hide.
 */
export const DECLARED_WORDS: readonly string[] = freeze(
  [...new Set(WORD_SOURCES.flatMap(phrasesIn).flatMap(wordsIn))].sort()
);
