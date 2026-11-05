import type {
  Actor,
  ArtifactBlock,
  Ask,
  Comment,
  IssueCheckResults,
  IssueCoverage,
  IssueDecisions,
  IssueDetails,
  IssueReferences,
  MessageRead,
  ReferenceTarget,
} from "../../api/types";
import type { GapKind, NodeType } from "./page-words";
import { type Draft, treeContext, treeDrafts } from "./tree-drafts";

/**
 * The issue's node tree, built from the page's reads and nothing else
 * (dispatch://INFRA-1196 R4, `dispatch://INFRA-1196/artifact/plan-md#b-t13`).
 *
 * `buildIssueTree` is pure: it takes one value holding the page's reads and answers the issue's
 * root node. It sends no request, reads no document's text and holds no React, so the fold
 * (`tree-fold.ts`), the rows (`IssueTree.tsx`) and the unit tests all read the same model. The
 * placement and the names are that one function with that one input, so the day the chain read
 * lands it is the only thing that changes.
 *
 * ## The spine
 *
 * The `from` edge is the single placement authority: a decision sits under the question it
 * settles, a requirement under the node its `from` names, a check and a plan task under each
 * requirement their `serves` names (edited under the first, transcluded under the rest), and a
 * result under its check. A decision's `shaped` edge is that decision's chip and places nothing;
 * where a requirement's `from` and a decision's `shaped` disagree the node is placed by `from`
 * and the disagreement is drawn as a gap. A node whose spine parent is absent sits under the
 * issue, with the `no_origin` gap on it and never as an error. Every edge that is not the spine
 * is a chip naming its target, and a node is drawn again only under another requirement its own
 * `serves` names.
 *
 * ## What today's reads hold, and what they do not
 *
 * Today's input forces three departures from the rules above, each read at the file and line
 * named, and each one `tree-drafts.ts` says again where it acts:
 *
 * - **No `from` on a requirement.** The `requirement` block carries no attributes at all
 *   (`packages/envoy/internal/dispatch/pmdoc/schema/blocks.json:70-75`, read 2026-11-05), so
 *   every requirement of the issue's spec sits under the issue with the `no_origin` gap, which
 *   is what c-home and c-spine read. A `requirement` block of any other document keeps its place
 *   under that document: the model holds no `from` for it and no coverage row either, so it is a
 *   block of a document and nothing more.
 * - **No `shaped` on a decision.** The decisions read answers `kind` and a boolean `unsure`
 *   (`packages/contracts/src/dispatch-api.ts:2519-2534`), so a decision's links to spec blocks
 *   and the asks its blocks mention are its chips, it keeps its place under its own document,
 *   and no disagreement is found in today's data: a `shaped` edge disagreeing with a `from` is
 *   drawn as the `shaped_disagreement` gap the day both edges exist.
 * - **No identities behind a requirement's coverage.** The coverage read counts the serving
 *   checks and plan tasks without naming them
 *   (`packages/contracts/src/dispatch-api.ts:805-816`), so a requirement draws those two counts
 *   and coverage's own gaps; what serves it is placed from each check's and each plan task's own
 *   `serves` pointer instead, which is the same edge read from the other end.
 *
 * ## The reads
 *
 * The issue read, the coverage read, the decisions read, the results read, the references read,
 * the conversation, each document's blocks, and the one batched reference read. A document's
 * blocks answer block ids, types and attributes and never text (`ArtifactBlock`), which is how
 * the tree holds a `serves` edge without reading a document the reader has not opened; a block's
 * own words come from the batched reference read, whose `name` for a block is that block's
 * stored excerpt. It is one read per document, never one per node.
 */

/** One edge of a node that is not the spine, drawn beside the node as its target's name. */
export interface NodeChip {
  /** Whether nothing answers to this reference now. */
  readonly gone: boolean;
  /** The words shown for the target, from the batched reference read; `""` until it answers. */
  readonly name: string;
  /** The target's own `dispatch://` address. */
  readonly ref: string;
}

/** What the coverage read counts for one requirement, with no identity behind either number. */
export interface NodeCounts {
  readonly checks: number;
  readonly planTasks: number;
}

/** One node of the issue's tree. A node drawn under more than one parent is one node per
 *  placement, each naming the others in `alsoUnder`. */
export interface TreeNode {
  /** The other requirements this node is also drawn under (R23); empty for a node drawn once. */
  readonly alsoUnder: readonly NodeChip[];
  /** Who made it, where the model holds an author (R7). */
  readonly author?: Actor;
  readonly children: readonly TreeNode[];
  readonly chips: readonly NodeChip[];
  /** What the coverage read counts for a requirement. */
  readonly counts?: NodeCounts;
  readonly gaps: readonly GapKind[];
  /** How many nodes this one holds, at any depth: what a folded row says it holds. */
  readonly held: number;
  /** The node's own address inside the issue: the issue key, an artifact slug,
   *  `<slug>#b-<block id>`, else the id the model gives it. */
  readonly id: string;
  /** Whether this node is an open question that waits on the reader, which the fold unfolds the
   *  path to whatever its depth (R6). */
  readonly question: boolean;
  /** The node's own `dispatch://` address, where it has one. */
  readonly reference?: string;
  /** Whether the decision this node holds was replaced, which folds its branch to one line
   *  (R1). */
  readonly replaced: boolean;
  /** How it stands, as the model's own token, for a type the model gives a state. */
  readonly state?: string;
  /** The node's own whole text; `""` where the model holds none. */
  readonly text: string;
  /** Whether this placement is a transclusion rather than the node's edited place. */
  readonly transcluded: boolean;
  readonly type: NodeType;
}

/** One document's blocks, as `GET /api/v1/artifacts/{id}/blocks` answers them. */
export interface DocumentBlocks {
  readonly artifactId: string;
  readonly blocks: readonly ArtifactBlock[];
}

/** The page's reads, as one value. Every field but the issue is optional, because the tree draws
 *  from the reads that have answered rather than waiting for the last of them. */
export interface IssueTreeReads {
  /** The issue's asks, from the conversation; merged with the issue read's open asks by id. */
  readonly asks?: readonly Ask[];
  readonly coverage?: IssueCoverage;
  readonly decisions?: IssueDecisions;
  /** The issue's comments, each a node of the tree (`addTurns`). */
  readonly comments?: readonly Comment[];
  /** One entry per document of the issue whose blocks the page has read. */
  readonly documents?: readonly DocumentBlocks[];
  readonly issue: IssueDetails;
  /** The issue's messages with their replies, each a node of the tree (`addTurns`). */
  readonly messages?: readonly MessageRead[];
  /** Which block cites each document the issue references (`via_blocks`). */
  readonly references?: IssueReferences;
  /** Each check of the Verification document with the results recorded against it. */
  readonly results?: IssueCheckResults;
  /** What every `dispatch://` address the tree holds points at, in one read. */
  readonly targets?: readonly ReferenceTarget[];
}

/** How many characters a row's first line may hold (R1). */
export const FIRST_LINE_LIMIT = 100;

/**
 * The node's own text to the first sentence end or `FIRST_LINE_LIMIT` characters, whichever
 * comes first, cut at a word and never inside one. `cut` says whether the text carries on past
 * the line, which is what the row draws its ellipsis for.
 */
export function firstLineOf(text: string): { readonly cut: boolean; readonly line: string } {
  const whole = text.replace(/\s+/gu, " ").trim();
  const sentence = /^.*?[.!?](?=\s|$)/u.exec(whole)?.[0];
  if (sentence !== undefined && sentence.length <= FIRST_LINE_LIMIT) {
    return { cut: sentence.length < whole.length, line: sentence };
  }
  if (whole.length <= FIRST_LINE_LIMIT) {
    return { cut: false, line: whole };
  }
  const window = whole.slice(0, FIRST_LINE_LIMIT + 1);
  const lastSpace = window.lastIndexOf(" ");
  const line = lastSpace <= 0 ? window.slice(0, FIRST_LINE_LIMIT) : window.slice(0, lastSpace);
  return { cut: true, line };
}

/** The issue's whole tree, from the reads it was given. */
export function buildIssueTree(reads: IssueTreeReads): TreeNode {
  const context = treeContext(reads);
  return assemble(reads.issue, [...treeDrafts(reads, context).values()], context.chipOf);
}

/**
 * Every `dispatch://` address the tree holds: each node's own, so a block's words can be read,
 * and each chip's, so a chip can name its target. This is what the one batched reference read is
 * given, which is why the page draws every chip in one request rather than one per node.
 */
export function treeReferences(root: TreeNode): string[] {
  const refs = new Set<string>();
  const walk = (node: TreeNode): void => {
    if (node.reference !== undefined) refs.add(node.reference);
    for (const chip of [...node.chips, ...node.alsoUnder]) refs.add(chip.ref);
    for (const child of node.children) walk(child);
  };
  walk(root);
  return [...refs];
}

/**
 * The drafts as one tree under the issue: one node per placement, each naming the parents it is
 * also drawn under and counting the nodes it holds. A placement that would sit under itself, or
 * under a node it already holds, is dropped, so a `from` edge that ever points in a circle still
 * draws a tree.
 *
 * A parent no draft answers to is no placement at all: a document cited by a paragraph, which
 * is the document's own text and no node, and a reply to a turn the page never read. Rather
 * than dropping the node with its missing parent, it sits under the issue, which is where
 * every node whose parent is absent goes (R4).
 */
function assemble(
  issue: IssueDetails,
  drafts: readonly Draft[],
  chipOf: (reference: string) => NodeChip
): TreeNode {
  const key = issue.key;
  const byParent = new Map<string, Draft[]>();
  const references = new Map<string, string>();
  const known = new Set(drafts.map((draft) => draft.id));
  /** Where each draft is actually drawn: the parents a draft of its own answers for, and the
   *  issue itself for a draft whose every named parent is absent. */
  const placements = new Map<string, readonly string[]>();
  for (const draft of drafts) {
    references.set(draft.id, draft.reference ?? draft.id);
    const named = draft.parents.filter((parent) => known.has(parent));
    const placed = named.length === 0 ? [key] : named;
    placements.set(draft.id, placed);
    for (const parent of placed) {
      byParent.set(parent, [...(byParent.get(parent) ?? []), draft]);
    }
  }
  const childrenOf = (id: string, above: readonly string[]): readonly TreeNode[] =>
    (byParent.get(id) ?? [])
      .filter((child) => child.id !== id && !above.includes(child.id))
      .sort((one, other) => one.order - other.order || one.id.localeCompare(other.id))
      .map((child) => build(child, id, [...above, id]));
  const build = (draft: Draft, parent: string, above: readonly string[]): TreeNode => {
    const children = childrenOf(draft.id, above);
    const parents = placements.get(draft.id) ?? [];
    const others = parents.filter((one) => one !== parent);
    return {
      alsoUnder: parents.length > 1 ? others.map((one) => chipOf(references.get(one) ?? one)) : [],
      ...(draft.author === undefined ? {} : { author: draft.author }),
      children,
      chips: draft.chips,
      ...(draft.counts === undefined ? {} : { counts: draft.counts }),
      gaps: draft.gaps,
      held: children.reduce((total, child) => total + child.held + 1, 0),
      id: draft.id,
      question: draft.question,
      ...(draft.reference === undefined ? {} : { reference: draft.reference }),
      replaced: draft.replaced,
      ...(draft.state === undefined ? {} : { state: draft.state }),
      text: draft.text,
      transcluded: parents.indexOf(parent) > 0,
      type: draft.type,
    };
  };
  const children = childrenOf(key, [key]);
  return {
    alsoUnder: [],
    children,
    chips: [],
    gaps: [],
    held: children.reduce((total, child) => total + child.held + 1, 0),
    id: key,
    question: false,
    replaced: false,
    // How Dispatch names an issue everywhere: its key and then its title, which is also what
    // tells this row from the title the identity block draws directly above it.
    text: `${key} ${issue.title}`,
    transcluded: false,
    type: "issue",
  };
}
