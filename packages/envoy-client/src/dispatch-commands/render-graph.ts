import type {
  Actor,
  Ask,
  GraphEdge,
  GraphNode,
  GraphReferences,
  IssueReferences,
} from "@legion/contracts";
import { actorText } from "../actor-text";
import { textHead } from "../ask-answer";
import type { DispatchClient, GraphReferencesQuery } from "../dispatch-http";
import { ITEM_KINDS, type ItemKind, type ParsedDispatchRef } from "../dispatch-owner";
import { messageFor } from "../errors";
import { dispatchAnswered } from "./failures";

/** A graph read's edges as a read shows them: edge type, the other node and its address,
 *  excerpt, when, and on a document's, ask's, comment's or message's read its block at the
 *  queried end. */
function referenceLines(read: GraphReferences | string): string[] {
  if (typeof read === "string") return [`- ${read}`];
  if (read.edges.length === 0) return ["- none"];
  return read.edges.map(edgeLine);
}

/** Whether `kind`, a graph node's, is one of the items whose blocks are its own (`ITEM_KINDS`):
 *  the parents a block node naming no document has. One set serves every read of such a node
 *  (`edgeLine`, `nodeLine`), so a part of an ask reads as its ask, as a block of a comment reads
 *  as its comment. */
function isItemKind(kind: GraphNode["kind"] | undefined): kind is ItemKind {
  return ITEM_KINDS.some((item) => item === kind);
}

/** One edge of a graph read. A part of an ask, or a block of a comment or message (a block node
 *  naming no document), shows as its item, then the block: `ask <item ref> block <part id>`,
 *  `comment <item ref> block <id>`. */
export function edgeLine(edge: GraphEdge): string {
  const { node } = edge;
  const excerpt = edge.excerpt === undefined ? "" : `${textHead(edge.excerpt.text)} · `;
  const block = edge.block === undefined ? "" : ` (block ${edge.block.ref ?? edge.block.id})`;
  const parent = node.parent ?? undefined;
  const item =
    node.kind === "block" && node.document === undefined && isItemKind(parent?.kind)
      ? parent
      : undefined;
  const source =
    item === undefined
      ? `${node.kind} ${node.ref ?? node.id}`
      : `${item.kind} ${item.ref ?? item.id} block ${node.block_id ?? node.id}`;
  return `- ${edge.kind} ${source} (${excerpt}${edge.created_at})${block}`;
}

/**
 * How a read degrades a graph or closure section the server cannot serve. Dispatch's own 404 is a
 * server without the route; any other failure, a gateway's 404 page included, is named.
 */
function unavailableReason(error: unknown): string {
  return dispatchAnswered(error, 404) ? "unavailable" : `unavailable: ${messageFor(error)}`;
}

/** One direction of a node's reference graph: the answer's node and its edges, or why the server
 *  could not serve it. */
export async function graphReferences(
  client: DispatchClient,
  query: GraphReferencesQuery
): Promise<GraphReferences | string> {
  try {
    return await client.getReferences(query);
  } catch (error) {
    return unavailableReason(error);
  }
}

export async function issueReferencesOrUnavailable(
  client: DispatchClient,
  key: string
): Promise<IssueReferences | string> {
  try {
    return await client.getIssueReferences(key);
  } catch (error) {
    return unavailableReason(error);
  }
}

/**
 * The sections every read ends with: the `Node:` line (the read node's maker, last change and
 * parent), `Referenced by:` (edges pointing at the node, mentions and structure alike) and
 * `Links:` (edges it writes), read from the reference graph in both directions. A graph the
 * server cannot serve degrades to one "unavailable" row, like the closure section, so the read
 * itself still answers.
 */
export async function graphSections(client: DispatchClient, ref: string): Promise<string[]> {
  const [incoming, outgoing] = await Promise.all([
    graphReferences(client, { to: ref }),
    graphReferences(client, { from: ref }),
  ]);
  return graphSectionLines(incoming, outgoing);
}

export function graphSectionLines(
  incoming: GraphReferences | string,
  outgoing: GraphReferences | string
): string[] {
  return [
    ...(typeof incoming === "string" ? [] : nodeLine(incoming.node)),
    "Referenced by:",
    ...referenceLines(incoming),
    "Links:",
    ...referenceLines(outgoing),
  ];
}

/**
 * The read node's facts on one line: `Node: <kind> <ref> · made by <actors> at <time> · changed
 * by <actors> at <time> · part of <parent> (version <N>)`, the parent's version a revision when
 * the parent is an item of its own blocks (`ITEM_KINDS`) rather than a document. An empty actor
 * list is "nobody recorded"; a null time, version or parent leaves its part out. A server older
 * than the facts sends no `made_by`, and the read prints no line.
 */
function nodeLine(node: GraphNode): string[] {
  if (node.made_by === undefined) return [];
  const fact = (by: Actor[] | undefined, time: string | null | undefined) =>
    `${by === undefined || by.length === 0 ? "nobody recorded" : by.map(actorText).join(", ")}${time == null ? "" : ` at ${time}`}`;
  const made = fact(node.made_by, node.made_at);
  const changed = fact(node.changed_by, node.changed_at);
  const { parent } = node;
  const partOf =
    parent == null
      ? ""
      : ` · part of ${parent.ref ?? `${parent.kind} ${parent.id}`}${node.parent_version == null ? "" : ` (${isItemKind(parent.kind) ? "revision" : "version"} ${node.parent_version})`}`;
  return [
    `Node: ${node.kind} ${node.ref ?? node.id} · made by ${made} · changed by ${changed}${partOf}`,
  ];
}

/**
 * A block read's head, from the node its reference names. A document's block: the block, the
 * document it is in (`ref.document`, the reference before the `#b-`), the version its text last
 * changed in and that text. A comment's or message's block: one line naming the block, its item
 * and the comment revision its text last changed in (`Block b2 of comment <id> (changed in
 * revision 1):`), then its text. A part of an ask reads the same way, under the name `noun`
 * gives it (`Option b2 of ask <id> (changed in revision 1):`), the revision being the ask's
 * parts'. A block no current version or revision holds has neither a changed version nor text.
 * Then for a pinned ref how the block stands against the version or revision it pins and its
 * text there.
 */
export function blockLines(node: GraphNode, ref: ParsedDispatchRef, noun = "Block"): string[] {
  const unit = ref.item === undefined ? "version" : "revision";
  const blockID = node.block_id ?? ref.block;
  const head =
    ref.item === undefined
      ? [
          `Block: ${blockID}`,
          `Document: ${node.document ?? ref.document}`,
          ...(node.changed_version === undefined
            ? []
            : [`Changed in version: ${node.changed_version}`]),
          ...(node.text === undefined ? [] : ["Text:", node.text]),
        ]
      : [
          `${noun} ${blockID} of ${ref.item.kind} ${ref.item.id}${node.changed_version === undefined ? "" : ` (changed in revision ${node.changed_version})`}:`,
          ...(node.text === undefined ? [] : [node.text]),
        ];
  const { pin } = node;
  if (pin === undefined) return head;
  const state =
    pin.state === "unchanged"
      ? `Unchanged since ${unit} ${pin.version}.`
      : pin.state === "changed"
        ? `Changed since ${unit} ${pin.version}; last changed in ${unit} ${node.changed_version}.`
        : `Removed in ${unit} ${pin.removed_version}.`;
  return [
    ...head,
    `Pinned to ${unit}: ${pin.version}`,
    state,
    `Text at ${unit} ${pin.version}:`,
    pin.text,
  ];
}

/**
 * What a block read's head calls a part of an ask: its answer, one of its options, or, where the
 * ask places every part it holds, its question — no node carries a part's type, so the ask's own
 * read is what says which part an id names (`AskOption.id`, `AskAnswer.id`, `Ask.revision`). A
 * part the ask does not place is a `Part`: its options read with no id (the parts are not
 * indexed yet, or the index stands on other options than the row now holds), or the reference is
 * pinned to a part the ask has since removed.
 */
export function askPartNoun(ask: Ask, node: GraphNode, blockID: string): string {
  if (ask.answer?.id === blockID) return "Answer";
  if (ask.options.some((option) => option.id === blockID)) return "Option";
  const placed =
    (ask.revision ?? 0) > 0 &&
    ask.options.every((option) => option.id !== undefined) &&
    (ask.answer === null || ask.answer.id !== undefined);
  return placed && node.pin?.state !== "removed" ? "Question" : "Part";
}
