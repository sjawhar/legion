import { parseBlockPointerValue } from "@legion/contracts";

import type { Actor, Ask } from "../../api/types";
import { buildDispatchReference, parseNodeReference } from "../refs/routes";
import { checkFacts, resultFacts } from "./check-facts";
import type { IssueTreeReads, NodeChip, NodeCounts, NodeFact } from "./issue-tree";
import { type GapKind, NODE_TYPES, type NodeType } from "./page-words";

/**
 * One draft node per read of the issue page, for `buildIssueTree` to place
 * (dispatch://INFRA-1196 R4, `dispatch://INFRA-1196/artifact/plan-md#b-t13`).
 *
 * Every function here is pure and takes the reads as its input. A builder may enrich a draft an
 * earlier one made - the decisions read names a block of `decisions.md` the document's own blocks
 * already drew, the coverage read names a block of the spec - so the order they run in is the
 * order `treeDrafts` calls them in, and each says what it adds.
 *
 * A node's own words come from the read that carries them: the coverage read for a requirement,
 * the results read for a check and its results, the decisions read for a decision, the
 * conversation for an ask, a comment and a message, and the one batched reference read for every
 * other block, whose `name` for a block is that block's stored excerpt. No document's text is
 * read, and no request is made per node.
 */

/** The id of the row of one block of one document, which is the block's own address inside the
 *  issue: a block id alone is unique only inside its document, and an issue carries several. */
export function blockNodeId(slug: string, blockId: string): string {
  return `${slug}#b-${blockId}`;
}

/** One node as it is being built: a `TreeNode`'s own fields, plus where the model placed it. */
export interface Draft {
  author?: Actor;
  chips: NodeChip[];
  counts?: NodeCounts;
  /** What the node's body draws under its text (`check-facts.ts`). */
  facts: NodeFact[];
  gaps: GapKind[];
  readonly id: string;
  order: number;
  /** The nodes this one sits under, in the order the model placed them: the first is where it is
   *  edited, every other a transclusion. `[]` is "under the issue". */
  parents: string[];
  /** What each `block[]` attribute of the block this node is points at, by the attribute's own
   *  name; empty for a node that is no block. */
  pointers: Readonly<Record<string, readonly string[]>>;
  question: boolean;
  reference?: string;
  replaced: boolean;
  state?: string;
  text: string;
  type: NodeType;
  /** Whether this decision waits for the reader's review (`Decision.waiting_review`). */
  waiting: boolean;
}

/**
 * Where a child of the issue sits among the issue's own children: its documents first, so the
 * spec and the path to the questions inside it open at the top of the tree, then the decisions,
 * then the requirements, then the questions asked of the issue itself, then its children, and
 * the conversation last. A node's order inside a document is the block's own place in it.
 */
export const ROOT_ORDER = {
  ask: 3000,
  child: 4000,
  decision: 1000,
  document: 0,
  other: 6000,
  requirement: 2000,
  turn: 5000,
} as const;

/** What every builder needs of the reads: how this issue addresses a node, and what a reference
 *  points at. Held as one value so the placement and the names are read the same way everywhere. */
export interface TreeContext {
  /** A chip naming what `reference` points at, by the one batched reference read. */
  readonly chipOf: (reference: string) => NodeChip;
  readonly key: string;
  /** The node `reference` names, as this issue addresses it; undefined for anything else. */
  readonly nodeIdOf: (reference: string) => string | undefined;
  /** The slug of the issue's spec, `""` for an issue with none. */
  readonly specSlug: string;
  /** The slug of one document of the issue, by its artifact id. */
  readonly slugOf: (artifactId: string) => string | undefined;
}

export function treeContext(reads: IssueTreeReads): TreeContext {
  const key = reads.issue.key;
  const slugs = new Map(reads.issue.artifacts.map((artifact) => [artifact.id, artifact.slug]));
  const specSlug = reads.issue.artifacts.find((artifact) => artifact.primary)?.slug ?? "";
  const names = new Map((reads.targets ?? []).map((target) => [target.ref, target]));
  const nodeIdOf = (reference: string): string | undefined => {
    const route = parseNodeReference(reference);
    if (route === undefined || !("key" in route) || route.key !== key) return undefined;
    const block = "block" in route ? route.block : undefined;
    if (route.kind === "spec") {
      return block === undefined ? specSlug : blockNodeId(specSlug, block.id);
    }
    if (route.kind === "artifact") {
      return block === undefined ? route.slug : blockNodeId(route.slug, block.id);
    }
    if (route.kind === "ask" || route.kind === "comment" || route.kind === "message") {
      return route.id;
    }
    return route.kind === "issue" ? key : undefined;
  };
  return {
    chipOf: (reference: string): NodeChip => {
      const target = names.get(reference);
      const gone = target?.gone ?? false;
      return { gone, name: gone ? "" : (target?.name ?? ""), ref: reference };
    },
    key,
    nodeIdOf,
    specSlug,
    slugOf: (artifactId: string) => slugs.get(artifactId),
  };
}

/** Every draft the reads hold, by its node id. */
export function treeDrafts(reads: IssueTreeReads, context: TreeContext): Map<string, Draft> {
  const drafts = new Map<string, Draft>();
  const cited = citations(reads, context);
  addDocuments(drafts, reads, context, cited);
  addDocumentBlocks(drafts, reads, context);
  addChildren(drafts, reads);
  addAsks(drafts, reads, context);
  addTurns(drafts, reads, context);
  addDecisions(drafts, reads, context);
  addRequirements(drafts, reads, context);
  addChecks(drafts, reads, context);
  settleCitations(drafts, cited);
  servePlacements(drafts, context);
  return drafts;
}

/** Where one artifact of the issue is cited: the block whose own words hold its address, and
 *  the document that block belongs to. */
interface Citation {
  /** The slug of the document the citing block belongs to, which is where the cited document
   *  sits when the citation stands in prose that is no node of its own. */
  readonly document: string;
  /** The node id of the citing block, `<slug>#b-<block id>`. */
  readonly id: string;
}

/**
 * Which block of which document cites each artifact of the issue, by the cited artifact's slug:
 * the first block, in document order, whose own text holds that artifact's `dispatch://`
 * address and not an address of one of its blocks, which names the block and not the document.
 *
 * The citation is read from the documents' own text rather than from `via_blocks`, because
 * `GET /issues/{key}/references` answers the closure of artifacts *outside* the issue
 * (`packages/envoy/internal/dispatch/refs/closure.go`'s
 * `where a.issue_key is distinct from $1`, read 2026-11-06), so it never names a document of
 * this issue that this issue's own spec cites - which is the citation the page draws. Both
 * reads this needs, each document's blocks and each document's text, the page already makes.
 *
 * A block's `from` and `to` are byte offsets into the stored markdown, as the server slices it,
 * so the cut is made over the encoded bytes: a document holding anything outside ASCII puts
 * every later block's byte offset past its JavaScript string index.
 */
function citations(reads: IssueTreeReads, context: TreeContext): Map<string, Citation> {
  const found = new Map<string, Citation>();
  const texts = new Map((reads.texts ?? []).map((text) => [text.artifactId, text.markdown]));
  if (texts.size === 0) return found;
  const addresses = reads.issue.artifacts.map((artifact) => ({
    slug: artifact.slug,
    written: [
      buildDispatchReference({ key: context.key, kind: "artifact", slug: artifact.slug }),
      ...(artifact.primary ? [buildDispatchReference({ key: context.key, kind: "spec" })] : []),
    ],
  }));
  const encoder = new TextEncoder();
  const decoder = new TextDecoder();
  for (const document of reads.documents ?? []) {
    const slug = context.slugOf(document.artifactId);
    const markdown = texts.get(document.artifactId);
    if (slug === undefined || markdown === undefined) continue;
    const bytes = encoder.encode(markdown);
    for (const block of document.blocks) {
      const text = decoder.decode(bytes.slice(block.from, block.to));
      for (const address of addresses) {
        if (address.slug === slug || found.has(address.slug)) continue;
        if (!address.written.some((written) => namesTheDocument(text, written))) continue;
        found.set(address.slug, { document: slug, id: blockNodeId(slug, block.id) });
      }
    }
  }
  return found;
}

/** Whether `text` names the document `written` addresses rather than one of its blocks: a
 *  `#` after the address names a block of that document, which cites the block and not the
 *  document it stands in. */
function namesTheDocument(text: string, written: string): boolean {
  for (let at = text.indexOf(written); at !== -1; at = text.indexOf(written, at + 1)) {
    if (text[at + written.length] !== "#") return true;
  }
  return false;
}

/**
 * Where each cited document finally sits. A document a block cites is a node under that block
 * (R16), which is what `addDocuments` wrote; where the citation stands in prose that is no node
 * of its own - a paragraph is a document's own text and no node (`addDocumentBlocks`) - the
 * document is a node under the document that cites it instead, never under an invented parent.
 */
function settleCitations(drafts: Map<string, Draft>, cited: ReadonlyMap<string, Citation>): void {
  for (const [slug, citation] of cited) {
    const draft = drafts.get(slug);
    if (draft === undefined || drafts.has(citation.id)) continue;
    draft.parents = [citation.document];
  }
}

/** Puts `draft` in, over whatever draft held its id. */
function add(drafts: Map<string, Draft>, draft: Draft): Draft {
  drafts.set(draft.id, draft);
  return draft;
}

/** A draft with nothing on it but its id and its type, for a builder to fill in. */
function blank(id: string, type: NodeType, order: number): Draft {
  return {
    chips: [],
    facts: [],
    gaps: [],
    id,
    order,
    parents: [],
    pointers: {},
    question: false,
    replaced: false,
    text: "",
    type,
    waiting: false,
  };
}

/**
 * The documents and images the issue carries. A document a block cites sits under that block
 * (R16) - the block `citations` found, and the one the references read names for a document of
 * another issue; one nothing cites sits under the issue, which is where a node whose spine
 * parent is absent goes.
 */
function addDocuments(
  drafts: Map<string, Draft>,
  reads: IssueTreeReads,
  context: TreeContext,
  cited: ReadonlyMap<string, Citation>
): void {
  const citedBy = new Map<string, string>();
  for (const member of reads.references?.members ?? []) {
    const block = member.via_blocks?.[0];
    const document = context.slugOf(member.via.id);
    if (block === undefined || document === undefined || member.via.kind !== "artifact") continue;
    citedBy.set(member.artifact.slug, blockNodeId(document, block));
  }
  for (const [slug, citation] of cited) citedBy.set(slug, citation.id);
  reads.issue.artifacts.forEach((artifact, index) => {
    if (artifact.kind !== "doc" && artifact.kind !== "image") return;
    const cites = citedBy.get(artifact.slug);
    const draft = add(
      drafts,
      blank(
        artifact.slug,
        artifact.kind === "doc" ? "document" : "image",
        ROOT_ORDER.document + (artifact.primary ? 0 : index + 1)
      )
    );
    draft.author = artifact.created_by;
    draft.parents = cites === undefined ? [] : [cites];
    draft.reference = buildDispatchReference({
      key: context.key,
      kind: "artifact",
      slug: artifact.slug,
    });
    draft.text = artifact.name;
  });
}

/**
 * Each document's typed blocks, in document order. A block of no declared node type - a
 * paragraph, a heading, a table - is the document's own text and no node of the tree. A block's
 * `block[]` attributes are kept as its pointers, which is where `serves` and `needs` are read
 * from; its words come from the batched reference read, since nothing reads the document's text.
 */
function addDocumentBlocks(
  drafts: Map<string, Draft>,
  reads: IssueTreeReads,
  context: TreeContext
): void {
  const types: readonly string[] = NODE_TYPES;
  for (const document of reads.documents ?? []) {
    const slug = context.slugOf(document.artifactId);
    if (slug === undefined) continue;
    document.blocks.forEach((block, index) => {
      if (!types.includes(block.type)) return;
      const reference = buildDispatchReference({
        block: { id: block.id },
        key: context.key,
        kind: "artifact",
        slug,
      });
      const pointers: Record<string, readonly string[]> = {};
      for (const attribute of block.attributes ?? []) {
        if (attribute.kind === "block[]") {
          pointers[attribute.name] = parseBlockPointerValue(attribute.value) ?? [];
        }
      }
      const draft = add(drafts, blank(blockNodeId(slug, block.id), block.type as NodeType, index));
      draft.parents = [slug];
      draft.pointers = pointers;
      draft.reference = reference;
      draft.text = context.chipOf(reference).name;
      // A check with no result recorded against it is in no state the results read would name,
      // and `no-result` is that state (`NODE_STATE_WORDS.check`), so a check drawn before the
      // results read answers says so rather than drawing a state nothing recorded.
      if (block.type === "check") draft.state = "no-result";
      if (block.type === "ask") draft.state = "open";
    });
  }
}

/** The issue's children, each one node under the issue: a child's own tree is its own page. */
function addChildren(drafts: Map<string, Draft>, reads: IssueTreeReads): void {
  reads.issue.children.forEach((child, index) => {
    const draft = add(drafts, blank(child.key, "child_issue", ROOT_ORDER.child + index));
    draft.text = child.title;
    draft.reference = `dispatch://${child.key}`;
    draft.chips = child.blocked_by.map((blocked) => ({
      gone: false,
      name: blocked,
      ref: `dispatch://${blocked}`,
    }));
  });
}

/**
 * Every question asked of the issue: the block the server indexed it from, where it has one,
 * else a node of its own under the issue. An approval request belongs to the document it reviews
 * rather than to the reasoning, so the tree draws none.
 */
function addAsks(drafts: Map<string, Draft>, reads: IssueTreeReads, context: TreeContext): void {
  const asks = new Map<string, Ask>();
  for (const ask of [...reads.issue.open_asks, ...(reads.asks ?? [])]) {
    if (ask.kind === "question") asks.set(ask.id, ask);
  }
  for (const ask of asks.values()) {
    // The document holding the typed `ask` block is `block_artifact`; `artifact_id` is the
    // document an anchored ask is anchored in, which a block ask does not carry.
    const slug = ask.block_artifact?.slug ?? context.slugOf(ask.artifact_id ?? "");
    const block = ask.block_id ?? undefined;
    const id = slug !== undefined && block !== undefined ? blockNodeId(slug, block) : ask.id;
    const draft = drafts.get(id) ?? add(drafts, blank(id, "ask", ROOT_ORDER.ask));
    draft.author = ask.author;
    draft.reference = buildDispatchReference({ id: ask.id, key: context.key, kind: "ask" });
    draft.state = ask.state;
    draft.text = ask.question;
    // An open question is what the fold unfolds the path to, whatever its depth (R6).
    draft.question = ask.state === "open";
  }
}

/**
 * The conversation's own turns: each comment and each message a node of this tree, a root turn
 * under the issue and a reply under the turn it answers (R1, R19). A thread head a page never
 * read is no node, so a reply whose parent is missing falls back to the issue, as every node
 * whose parent is absent does (`assemble`).
 *
 * The page draws these rows *and* the panel draws the same conversation (R12): the tree is the
 * issue's one ordered body, and the panel is where a reader answers. An address naming a
 * comment or a message (`/comments/<id>`, `/messages/<id>`, `?comment=`) is therefore an
 * address of a row of this tree.
 */
function addTurns(drafts: Map<string, Draft>, reads: IssueTreeReads, context: TreeContext): void {
  (reads.comments ?? []).forEach((comment, index) => {
    const draft = add(drafts, blank(comment.id, "comment", ROOT_ORDER.turn + index));
    draft.author = comment.author;
    draft.parents = comment.reply_to === null ? [] : [comment.reply_to];
    draft.reference = buildDispatchReference({
      id: comment.id,
      key: context.key,
      kind: "comment",
    });
    draft.text = comment.body;
  });
  const messages = (reads.messages ?? []).flatMap((thread) => [thread.message, ...thread.replies]);
  messages.forEach((message, index) => {
    const draft = add(drafts, blank(message.id, "message", ROOT_ORDER.turn + index));
    draft.author = message.author;
    draft.parents = message.in_reply_to === null ? [] : [message.in_reply_to];
    draft.reference = buildDispatchReference({
      id: message.id,
      key: context.key,
      kind: "message",
    });
    draft.text = message.body;
  });
}

/**
 * Each decision of the issue's `decisions.md`. A decision sits under the question it settles;
 * the model holds no such edge today (`packages/contracts/src/dispatch-api.ts:2519-2534`: the
 * read answers `kind` and a boolean `unsure`, and no `shaped`), so every decision's spine parent
 * is absent and it sits under the issue with the gap that says so, as any other node whose spine
 * parent is absent does (R4) - never parked inside `decisions.md`, where the fold would close
 * the reasoning away behind a document. It names every spec block it links and every ask its
 * blocks mention as its chips, and the day the edge lands the question it settles is read from
 * the decision itself.
 */
function addDecisions(
  drafts: Map<string, Draft>,
  reads: IssueTreeReads,
  context: TreeContext
): void {
  (reads.decisions?.decisions ?? []).forEach((decision, index) => {
    const id = context.nodeIdOf(decision.ref);
    if (id === undefined) return;
    const held = drafts.get(id);
    const draft = add(drafts, blank(id, "decision", held?.order ?? index));
    // A target with no address of its own is named by nothing, so there is no chip to draw for
    // it (`GraphNode.ref` is optional).
    draft.chips = [
      ...decision.links.flatMap((link) =>
        link.to.ref === undefined ? [] : [{ ...context.chipOf(link.to.ref), stale: link.stale }]
      ),
      ...decision.asks.map((ask) =>
        context.chipOf(buildDispatchReference({ id: ask, key: context.key, kind: "ask" }))
      ),
    ];
    // Under the issue: the question it settles is what would place it, and the model holds none.
    draft.gaps = ["no_origin"];
    draft.order = ROOT_ORDER.decision + index;
    draft.parents = [];
    draft.pointers = held?.pointers ?? {};
    draft.reference = decision.ref;
    draft.replaced = decision.replaced;
    draft.text = decision.text;
    // What waits for the reader: its node is where Keep and Change are drawn, and the one the
    // Decisions preset opens on (R7, `DecisionActions.tsx`).
    draft.waiting = decision.waiting_review;
  });
}

/**
 * Every requirement of the issue's spec sits under the node its `from` names. No requirement
 * carries one - the `requirement` block declares no attributes at all
 * (`packages/envoy/internal/dispatch/pmdoc/schema/blocks.json:70-75`, read 2026-11-05) - so each
 * sits under the issue with the gap that says so, beside the two counts the coverage read holds
 * for it and the gaps that read already carries. Where a decision claims to have shaped a
 * requirement whose own origin names another node, the requirement is placed by its `from` and
 * the disagreement is drawn as a gap (R4), never placed under the decision.
 */
function addRequirements(
  drafts: Map<string, Draft>,
  reads: IssueTreeReads,
  context: TreeContext
): void {
  const shapedBy = new Map<string, number>();
  for (const decision of reads.decisions?.decisions ?? []) {
    for (const link of decision.links) {
      const shaped = link.to.ref === undefined ? undefined : context.nodeIdOf(link.to.ref);
      if (shaped === undefined) continue;
      shapedBy.set(shaped, (shapedBy.get(shaped) ?? 0) + 1);
    }
  }
  (reads.coverage?.requirements ?? []).forEach((requirement, index) => {
    const id = blockNodeId(context.specSlug, requirement.block_id);
    const held = drafts.get(id);
    // The `from` edge is the single placement authority (R4): the requirement's own pointer,
    // never the decision that claims to have shaped it.
    const origin = context.nodeIdOf(held?.pointers.from?.[0] ?? "");
    const draft = add(drafts, blank(id, "requirement", ROOT_ORDER.requirement + index));
    draft.chips = held?.chips ?? [];
    draft.counts = { checks: requirement.checks, planTasks: requirement.plan_tasks };
    draft.gaps = requirementGaps(requirement.gaps, origin, shapedBy.get(id) ?? 0);
    draft.parents = origin === undefined ? [] : [origin];
    draft.pointers = held?.pointers ?? {};
    draft.reference = requirement.ref;
    draft.text = requirement.text;
  });
}

/** The gaps on one requirement: its own origin, the two the coverage read already carries, and a
 *  decision claiming to have shaped a requirement that came from elsewhere. */
function requirementGaps(
  coverage: readonly string[],
  origin: string | undefined,
  shaped: number
): GapKind[] {
  const gaps: GapKind[] = [];
  if (origin === undefined) gaps.push("no_origin");
  for (const gap of coverage) {
    gaps.push(gap.endsWith("has no check") ? "no_check" : "no_plan_task");
  }
  if (origin !== undefined && shaped > 0) gaps.push("shaped_disagreement");
  return gaps;
}

/**
 * Each check of the issue's Verification document with the results recorded against it: the
 * check's own words, how it stands and the four attributes its body draws, and one node per
 * result under the check it was recorded against (R4, "a result under its check"), each with
 * what that result recorded (`check-facts.ts`). The check's `serves` is no part of this read
 * (`CheckBlock` carries none), so its placement is the block's own pointer, below.
 */
function addChecks(drafts: Map<string, Draft>, reads: IssueTreeReads, context: TreeContext): void {
  for (const check of reads.results?.checks ?? []) {
    const slug = context.slugOf(check.artifact_id);
    if (slug === undefined) continue;
    const id = blockNodeId(slug, check.block_id);
    const held = drafts.get(id);
    const draft = add(drafts, blank(id, "check", held?.order ?? 0));
    draft.facts = checkFacts(check);
    draft.parents = held?.parents ?? [slug];
    draft.pointers = held?.pointers ?? {};
    draft.reference = check.ref;
    draft.state = checkState(check.results);
    draft.text = check.text;
    check.results.forEach((result, index) => {
      const row = add(drafts, blank(result.id, "result", index));
      row.author = result.actor;
      row.facts = resultFacts(result);
      row.parents = [id];
      row.text = result.observed;
    });
  }
}

/**
 * How a check stands, from the newest result recorded against it, in the states
 * `NODE_STATE_WORDS.check` names: `older` and `unmarked` are about the result's currency rather
 * than its verdict, and a currency the server left out reads as `unmarked` because nothing
 * judged it. It is the one reading of a check's state the page has: the Checks preset draws
 * the node, and the five states are told apart by a word and a shape (R15, R21).
 */
function checkState(results: readonly { currency?: string; verdict: string }[]): string {
  const latest = results[0];
  if (latest === undefined) return "no-result";
  if (latest.currency === undefined || latest.currency === "unknown") return "unmarked";
  if (latest.currency === "older") return "older";
  return latest.verdict === "pass" ? "passed" : "failed";
}

/**
 * A check and a plan task sit under each requirement their `serves` names, edited under the
 * first and transcluded under the rest (R4); what their `needs` names is a chip, as every edge
 * that is not the spine is. A `serves` naming no requirement of this issue places nothing, so
 * the block keeps its place under its own document.
 */
function servePlacements(drafts: Map<string, Draft>, context: TreeContext): void {
  for (const draft of drafts.values()) {
    if (draft.type !== "check" && draft.type !== "plan_task") continue;
    const serves = (draft.pointers.serves ?? [])
      .map(context.nodeIdOf)
      .filter((id): id is string => id !== undefined && drafts.get(id)?.type === "requirement");
    if (serves.length > 0) draft.parents = serves;
    draft.chips = [...draft.chips, ...(draft.pointers.needs ?? []).map(context.chipOf)];
  }
}
