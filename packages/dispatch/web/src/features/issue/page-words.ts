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
 * This module is data and pure functions. It holds no React and touches no DOM, and the only
 * thing it imports is its sibling `page-structure.ts`, whose property and section names and
 * help lines `DECLARED_WORDS` is derived from as well; so a test, a browser spec and a
 * component all read the same list.
 */

import {
  freeze,
  HELP_LINES,
  KEY_REPORTS,
  MARGIN_SECTION_NAMES,
  PROPERTY_LABELS,
} from "./page-structure";

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
 * check's five are `checkState`'s (`features/issue/tree-drafts.ts`), and the page
 * must tell all five apart, so each keeps a word of its own.
 *
 * A decision's three are how the old Decisions tab grouped them, which is what a reader used
 * to find the ones waiting on them: `waiting_review` is what waits for the reader's review,
 * `yours` is a decision a person made or an agent's one the reader kept
 * (`kind === "person" || kept !== null`, and not replaced), and `replaced` is one a later
 * decision replaced (`decisions.go`, `DecisionsPanel.tsx` at
 * 7567aeb606f309f40d4145f3d6a49c19fd452558). A decision in none of the three - an agent's that
 * nobody has reviewed and nothing replaced - stood in no section of that tab either, and draws
 * no state here.
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
  decision: {
    replaced: "Replaced",
    waiting_review: "Waiting for your review",
    yours: "Yours",
  },
} as const);

/** How an ask stands, by the `state` the server writes into its block. */
export type AskState = keyof typeof NODE_STATE_WORDS.ask;

/** How a check stands, from the newest result recorded against it. */
export type CheckState = keyof typeof NODE_STATE_WORDS.check;

/** How a decision stands with the reader, from the decisions read. */
export type DecisionState = keyof typeof NODE_STATE_WORDS.decision;

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

/** The four states the conversation panel can be read in (R12), in the order the panel offers
 *  them: everything the issue recorded, what the reader pinned, the questions alone, and what
 *  people and agents actually said. */
export const CONVERSATION_STATE_ORDER = freeze([
  "everything",
  "pinned",
  "questions",
  "words",
] as const);

/** One state of the conversation panel, by its own token, which is its `data-conversation-state`. */
export type ConversationState = (typeof CONVERSATION_STATE_ORDER)[number];

/** The name of each conversation state. */
export const CONVERSATION_STATE_NAMES = freeze({
  everything: "Everything",
  pinned: "Pinned",
  questions: "Questions",
  words: "What people said",
} as const satisfies Record<ConversationState, string>);

/**
 * How an issue stands, as the page says it: the word a child's row draws beside it (R10). The
 * keys are the server's own status tokens, in the lifecycle order `features/project/board-model.ts`
 * holds them in, and `page-words.test.ts` reads that module, so a status added there fails
 * until it has a word here.
 */
export const ISSUE_STATUS_WORDS = freeze({
  backlog: "Backlog",
  done: "Done",
  icebox: "Icebox",
  in_progress: "In progress",
  needs_review: "Needs review",
  retro: "Retro",
  testing: "Testing",
  todo: "Todo",
  triage: "Triage",
} as const);

/** The word for the issue status `token`. A token the module does not declare reads as itself,
 *  which is what the server sent; `page-words.test.ts` fails until a declared status has a word. */
export function issueStatusWord(token: string): string {
  const words: Readonly<Record<string, string>> = ISSUE_STATUS_WORDS;
  return words[token] ?? token;
}

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
  /** A document node whose own text has not arrived yet (R16). */
  document_reading: "This document has not arrived yet.",
  /** A document node whose own text could not be read. */
  document_unread: "This document could not be read.",
  folded_branch: "This branch holds nothing.",
  plan: "This issue has no plan tasks yet.",
  spec: "This issue has no requirements yet.",
  /** What the requirements panel says of a spec that marks none, which is not the same as an
   *  issue with no spec at all. */
  spec_marks_none: "This spec marks no requirements.",
  spec_summary: "This spec has no summary.",
  tree: "This issue holds nothing yet.",
} as const);

/**
 * What the page says of a gap in the reasoning, one line per kind of gap (R4). A gap is drawn on
 * the node it is about and never as an error: a node nothing explains is a node, and the line
 * says what is absent.
 *
 * `no_check` and `no_plan_task` are the two gaps the coverage read already carries
 * (`RequirementCoverage.gaps`), said in the page's own words rather than in the server's
 * `<block id> has no check`, which names a block by its id. `no_origin` is a node whose spine
 * parent the model does not hold; `shaped_disagreement` is a decision claiming a node the
 * node's own origin does not name, which the page draws rather than picking one of the two.
 */
export const GAP_LINES = freeze({
  no_check: "No check serves this requirement.",
  no_origin: "Nothing explains this node: it names nothing it came from.",
  no_plan_task: "No plan task serves this requirement.",
  no_read_back: "Nobody is recorded as having read these sentences back.",
  no_summary_author: "Nobody is recorded as having written these sentences.",
  shaped_disagreement: "A decision says it shaped this node, which came from another one.",
} as const);

/** One kind of gap the page names, which is a row's `data-gap-kind`. */
export type GapKind = keyof typeof GAP_LINES;

/**
 * The same gap in the fewest words, which is what a row of the tree draws beside the node (R1:
 * a row is one line). The sentence above is drawn in the node's body, where there is room for
 * it, so a reader sees that something is absent on the row and reads what it is by focusing it.
 */
export const GAP_WORDS = freeze({
  no_check: "No check",
  no_origin: "Nothing explains it",
  no_plan_task: "No plan task",
  no_read_back: "Not read back",
  no_summary_author: "No author",
  shaped_disagreement: "Two origins",
} as const satisfies Record<GapKind, string>);

/**
 * Which gap one line of the coverage read names. The server writes `<block id> has no check`
 * and `<block id> has no plan task` (`RequirementCoverage.gaps`), which name a block by its id;
 * every surface of the page reads the line through this and draws `GAP_WORDS` or `GAP_LINES`
 * instead, so no identifier and no server phrasing is drawn where a name belongs (R21).
 */
export function coverageGapKind(line: string): GapKind {
  return line.endsWith("has no check") ? "no_check" : "no_plan_task";
}

/** The noun a progress count is written with: `1/1 task`, `3/7 tasks`, `0/1 child`,
 *  `2/5 children`. The count itself is the issue's own and carries `AUTHOR_TEXT_ATTRIBUTE`. */
export const PROGRESS_NOUNS = freeze({
  child: "child",
  children: "children",
  task: "task",
  tasks: "tasks",
} as const);

/** What a panel says while a read of its own is out, and when that read failed. Neither is an
 *  empty state - the place may hold plenty - so neither belongs in `EMPTY_STATE_LINES`. */
export const READING_LINES = freeze({
  coverage_failed: "Could not load the requirement coverage.",
  coverage_loading: "Loading requirements…",
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

/** What a recorded result says it saw, in the page's own words rather than the wire's `pass`
 *  and `fail`: the verdict is drawn as a fact of the result's own node (R15). */
export const RESULT_VERDICT_WORDS = freeze({
  fail: "Fail",
  pass: "Pass",
} as const);

/**
 * One line per read the page draws from that Dispatch does not serve yet (R20). A preset says
 * what it does not hold rather than drawing a count with nothing behind it: the coverage read
 * counts the checks that serve a requirement and names none of them, and the chain read that
 * would name them (dispatch://INFRA-1098 item 14) has no route.
 */
export const WAITING_LINES = freeze({
  chain:
    "Each requirement counts the checks that serve it, and naming them waits on a read Dispatch does not answer yet.",
} as const);

/**
 * How a document of the issue stands with its reviewers, in words (R5): the head's "How it gets
 * done" row says this of the plan. The keys are `ArtifactApproval.state`; the version a review
 * covers is no part of the word, because the head is one line and the document's own toolbar
 * carries the version.
 */
export const APPROVAL_STATE_WORDS = freeze({
  approved: "Approved",
  awaiting: "Awaiting approval",
  changes_requested: "Changes requested",
  draft: "Draft",
  stale: "Approved, and changed since",
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
  /** Leaves an editor with the value the issue had, in every editor the page opens. */
  cancel: "Cancel",
  /** Answers an agent's decision with a change, on the decision's own node (R7). It opens the
   *  composer below, whose comment is anchored to the decision's own words. */
  change_decision: "Change",
  /** The collapsible section of child issues at the top of every preset (R10). */
  children: "Children",
  /** The direct children done of total, labelled on the collapsed header (R10). */
  children_done: "Children done",
  /** Closes the issue, from the identity block's one actions control (R11). */
  close_issue: "Close issue",
  /** What the components line says of an issue whose components come from an ancestor. */
  components_inherited_from: "inherited from",
  /** What it says of an issue the project's component tree reaches no component for, before
   *  the reason the server gives for that. */
  components_none: "None",
  /** What it says of an issue attached to no component at all. */
  components_not_attached: "Not attached",
  /** What it says of a component id the project's tree no longer holds, before that id. */
  components_retired: "retired",
  /** The control that attaches an issue with no components of its own to some. */
  components_set: "Set components",
  /** The heading of the conversation inside the one right-hand panel (R12). */
  conversation: "Conversation",
  /** The word that control carries, beside the command it copies. */
  copy: "Copy",
  /** Copies a check's `run` command, which renders as code beside it (R15). */
  copy_command: "Copy the command",
  /** What it says once the command is on the clipboard, and when the browser refused it. */
  copied: "Copied",
  copy_failed: "Copy failed",
  /** Why a copy the browser refused can still be made: the toolbar says what to do instead. */
  copy_failed_hint: "Copy failed - select the text",
  /** Copies the address of the block the caret is in, in the open document's toolbar (R16). */
  copy_link: "Copy link",
  /** Draws a pinned version of the open document against the document as it stands (R16). */
  diff_vs_current: "Diff vs current",
  /** The one row a branch below the three drawn levels folds to, which opens it in place: the
   *  count before it is the branch's own and carries `AUTHOR_TEXT_ATTRIBUTE` (R1). */
  deeper_nodes: "nodes deeper in this branch",
  /** Takes a version of an artifact down (R16). */
  download: "Download",
  /** Gives an issue that has a parent a different one (R11, row one). */
  edit_parent: "Edit parent issue",
  /** What an external link says of an address the browser will not open. */
  external_link_unsafe: "unsafe external link",
  /** The box that narrows the preset to the rows whose words hold what is typed (R3). */
  filter: "Filter",
  /** Closes a branch of the tree (R1). */
  fold: "Fold",
  /** What a folded branch says it holds, after the count of the nodes under it (R1). */
  folded_nodes: "nodes folded",
  /** How many checks a pull request has, before the count GitHub gives. */
  github_checks: "checks",
  /** What an external link says when Dispatch holds no token for its repository: the line is
   *  drawn beside the link rather than kept in a tooltip (R21). */
  github_unavailable: "GitHub details are unavailable for this repository",
  /** The head's first row: where the plan says how the issue gets done (R5). */
  how_it_gets_done: "How it gets done",
  /** The one control row one keeps the actions behind: close, reopen and setting the parent.
   *  It names actions, never a property (R11 bans "more"). */
  issue_actions: "Issue actions",
  /** Agrees with an agent's decision as its node draws it, which takes it out of what waits
   *  for the reader's review (R7). */
  keep_decision: "Keep",
  /** Keeps a node's body open once the focus leaves it (R2); the focused node's body is open
   *  without it. */
  keep_open: "Keep open",
  /** Lets a node kept open close again (R2). */
  let_close: "Let it close",
  /** The word the name-a-version control shows; `name_version` is its accessible name (R16). */
  name: "Name",
  /** Gives the open document's current text a version of its own (R16). */
  name_version: "Name version",
  /** Moves to the next open question that waits on the reader (R6). */
  next_question: "Next open question",
  /** What the repository control says while neither the issue nor its project names one. */
  no_repository: "No repository",
  /** What the route control says while the issue has none to show. */
  no_route: "No route",
  /** What one side of a plan task's pointers says while it names nothing (R4). */
  none: "none",
  /** Marks a document or an artifact in the node menu that no block cites (R16). */
  nothing_cites_it: "Cited by nothing",
  /** What the claim says of a holder the agent registry no longer lists (R11, row four). */
  not_running: "not running",
  /** The node menu beside the presets, holding what no block cites and the upload (R16). */
  other_nodes: "Files and documents",
  /** The row of five queries over the one tree, as a reader of it hears it named (R3). */
  presets: "Issue presets",
  /** Hands a claimed issue back, so another agent can take it (R11, row four). */
  release_claim: "Release",
  /** Reopens a closed issue, from the actions control or the closed issue's own line (R11). */
  reopen_issue: "Reopen issue",
  /** What reopening does, as the control's own words rather than a tooltip (R11). */
  reopen_into_backlog: "Reopen into Backlog",
  /** Why the repository shown may not be the issue's own, said beside the control rather than
   *  in a tooltip (R11, R21). */
  repository_inherited: "inherited or the project's default",
  /** The heading of the requirements the spec marks, under the document itself (R14). */
  requirements: "Requirements",
  /** Each editor's save, naming the field it writes. */
  save_blocked_by: "Save blocked by",
  save_parent: "Save parent",
  save_route: "Save route",
  /** Sends the change a reader wrote against a decision, named apart from the conversation's
   *  own Send so a page drawing both offers two controls a reader can tell apart (R7). */
  send_change: "Send change",
  /** What a control says while its own write is out: Dispatch has not answered, so none of
   *  these says the thing is done. */
  saving_close: "Closing…",
  saving_field: "Saving…",
  saving_labels: "Saving labels…",
  saving_release: "Releasing…",
  saving_reopen: "Reopening…",
  /** How many checks serve a requirement, which the coverage read counts without naming them
   *  (R4, until the chain read lands). */
  serving_checks: "Checks serving it",
  /** How many plan tasks serve a requirement, counted the same way (R4). */
  serving_plan_tasks: "Plan tasks serving it",
  /** Gives an issue with no parent one (R11, row one). */
  set_parent: "Set parent issue",
  /** Ends that comparison and draws the pinned version itself (R16). */
  show_version: "Show version",
  /** The whole subtree done of total, labelled beside the direct count (R10). */
  subtree_done: "Whole tree done",
  /** What a chip says of a target nothing answers to now (`ReferenceTarget.gone`). */
  target_gone: "Deleted",
  /** What a chip says of a block a decision shaped and changed after: the decision changed
   *  since it named the block, and the block has not changed since (`DecisionLink.stale`). */
  target_stale: "Out of date",
  /** The body of the page, as a reader of the tree hears it named (R1). */
  tree: "Issue tree",
  /** What the assignee control says while nobody answers the issue's asks (R11). */
  unassigned: "Unassigned",
  /** Opens a branch of the tree (R1). */
  unfold: "Unfold",
  /** Adds an artifact from the node menu (R16). */
  upload: "Upload",
  /** The file chooser that control opens, named apart from the Artifacts tab's own so a page
   *  drawing both offers two controls a reader can tell apart. */
  upload_file: "Upload a file",
  /** Which version of the open document the preset draws (R16). */
  version: "Version",
  /** Every version an artifact has (R16). */
  versions: "Versions",
  /** The head's second row: where the checks say what the reader will be shown (R5). */
  what_you_will_be_shown: "What you will be shown",
  /** Names the author of a node whose author the model holds (R7); the name itself is the
   *  author's own text. */
  written_by: "Written by",
  /** The box that composer writes in: what the reader would change about the decision. */
  your_change: "Your change",
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
 * A plain label for every fact a node's body draws that is no attribute of a block: what a
 * recorded result says of itself (`features/issue/check-facts.ts`). A check's own attributes
 * keep the labels their document's header draws them under, so a check reads the same in the
 * tree and in the document it lives in, and only what the schema has no name for is here.
 *
 * A label is lowercase, like the attribute labels it sits beside, and the row draws the colon.
 */
export const NODE_FACT_LABELS = freeze({
  commit: "commit",
  excluded: "excluded",
  failed: "failed",
  role: "role",
  screenshot: "screenshot",
  skipped: "skipped",
  /** Whether the result still speaks for the work as it stands (`RESULT_CURRENCY_WORDS`). */
  stands: "how it stands",
  verdict: "verdict",
} as const);

/** The label of the fact `name`: this module's own where it has one, else the label its block
 *  attribute carries (`blockAttributeLabel`). */
export function nodeFactLabel(name: string): string {
  const labels: Readonly<Record<string, string>> = NODE_FACT_LABELS;
  return labels[name] ?? blockAttributeLabel(name);
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
    decision: {
      replaced: "decision-replaced",
      waiting_review: "decision-waiting-review",
      yours: "decision-yours",
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
    readonly decision: Record<DecisionState, string>;
  };
  readonly types: Record<NodeType, string>;
});

/** Every type whose state the page draws, which is every key of `NODE_STATE_WORDS`. */
const STATE_TYPES = freeze(keysOf(NODE_STATE_WORDS));

function glyphNames(): Readonly<Record<string, string>> {
  const names: Record<string, string> = {};
  for (const type of NODE_TYPES) {
    names[GLYPH_TOKENS.types[type]] = NODE_TYPE_NAMES[type];
  }
  for (const type of STATE_TYPES) {
    const words: Readonly<Record<string, string>> = NODE_STATE_WORDS[type];
    const tokens: Readonly<Record<string, string>> = GLYPH_TOKENS.states[type];
    for (const state of Object.keys(words)) {
      const token = tokens[state];
      if (token !== undefined) names[token] = `${words[state]} ${NODE_TYPE_NAMES[type]}`;
    }
  }
  return names;
}

/**
 * The accessible name of each glyph, by its token: a shape carries its own name, because a
 * reader who cannot see the shape still has to hear what it says. A type's shape is named by
 * the type; a state's shape by its state word and the type it stands on, as in "Passed check".
 */
export const GLYPH_NAMES: Readonly<Record<string, string>> = freeze(glyphNames());

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
  CONVERSATION_STATE_NAMES,
  ISSUE_STATUS_WORDS,
  EMPTY_STATE_LINES,
  KEY_REPORTS,
  GAP_LINES,
  GAP_WORDS,
  PROGRESS_NOUNS,
  READING_LINES,
  CHECK_WHEN_WORDS,
  RESULT_CURRENCY_WORDS,
  RESULT_VERDICT_WORDS,
  WAITING_LINES,
  APPROVAL_STATE_WORDS,
  CONTROL_LABELS,
  PROPERTY_LABELS,
  MARGIN_SECTION_NAMES,
  HELP_LINES,
  BLOCK_ATTRIBUTE_LABELS,
  NODE_FACT_LABELS,
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
