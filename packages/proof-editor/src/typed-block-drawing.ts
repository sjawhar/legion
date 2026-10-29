import {
  MarkType,
  NodeType,
  type Node as ProseMirrorNode,
  type Schema,
} from "@milkdown/prose/model";

/** The part of an mdast node the typed-block decision reads. */
export type MdastNode = {
  type: string;
  name?: string;
  value?: string;
  attributes?: Record<string, string>;
  data?: { directiveLabel?: boolean };
  children?: MdastNode[];
  position?: {
    start?: { offset?: number };
    end?: { offset?: number };
  };
};

/** Which container directives may stay typed blocks, and what their content is checked against. */
type TypedBlockDecision = {
  /** The ProseMirror schema the parse builds with. */
  schema: Schema;
  /** The parsed text, as `markdownSource` returns it, which the tree's offsets count into. */
  source: string;
  /** Whether `node` may stay a typed block; a refused one is drawn as text. */
  accepts(node: MdastNode): boolean;
};

/** The part of a Milkdown node or mark spec that names the markdown nodes it parses. */
type ParseRule = { parseMarkdown?: { match(node: MdastNode): boolean } };

/** A closing fence line: container prefixes, a run of three or more colons, trailing blanks. */
const CLOSING_FENCE = /^[ \t>]*(:{3,})[ \t]*$/;

/**
 * The text a parse's tree offsets count into: the file without one leading U+FEFF, which
 * micromark drops before it counts the offsets the tree carries.
 */
export function markdownSource(file: { toString(): string }): string {
  const text = file.toString();
  return text.startsWith("\ufeff") ? text.slice(1) : text;
}

/** The paragraph remark makes of a directive's `[label]`, which its opening line already shows. */
function isLabel(node: MdastNode): boolean {
  return node.type === "paragraph" && node.data?.directiveLabel === true;
}

/** Every attribute of `type` at its default, null where it has none. */
function defaultAttrs(type: NodeType): Record<string, unknown> {
  return Object.fromEntries(
    Object.entries(type.spec.attrs ?? {}).map(([key, attr]) => [key, attr.default ?? null])
  );
}

/**
 * The block's children as Milkdown's ParserState would add them: each an empty node of the type
 * it matches, and a mark node's children in its place, since the mark's runner opens the mark,
 * runs its children into the same parent and closes it. Undefined when a child matches no node,
 * or matches text, which this check does not model.
 */
function placeholders(schema: Schema, children: MdastNode[]): ProseMirrorNode[] | undefined {
  const targets = [...Object.values(schema.nodes), ...Object.values(schema.marks)];
  const nodes: ProseMirrorNode[] = [];
  for (const child of children) {
    const target = targets.find((candidate) =>
      (candidate.spec as ParseRule).parseMarkdown?.match(child)
    );
    if (target instanceof MarkType) {
      const inner = placeholders(schema, child.children ?? []);
      if (inner === undefined) return undefined;
      nodes.push(...inner);
      continue;
    }
    if (!(target instanceof NodeType) || target.isText) return undefined;
    nodes.push(target.create(defaultAttrs(target)));
  }
  return nodes;
}

/**
 * Whether Milkdown can build the typed block `name` around `children`. Milkdown builds a node
 * with `NodeType.createAndFill`, and a null there is the `createNodeInParserFail` it logs while
 * it drops the block with all its text. Children that cannot stand in leave the block as it was.
 */
function fitsContentRule(schema: Schema, name: string, children: MdastNode[]): boolean {
  const type = schema.nodes[name];
  if (type === undefined) return true;
  const content = placeholders(schema, children);
  return content === undefined || type.createAndFill(defaultAttrs(type), content) !== null;
}

/**
 * Where `node` ends in the source. A node a remark plugin made without a position, such as a
 * block-level mark span, ends where its last child does.
 */
function sourceEnd(node: MdastNode): number | undefined {
  const end = node.position?.end?.offset;
  if (end !== undefined) return end;
  const last = node.children?.at(-1);
  return last === undefined ? undefined : sourceEnd(last);
}

/**
 * A directive drawn as its source: the opening fence line, its content (label paragraph dropped,
 * since the opening line already shows the label) and, when the source closes it, the closing
 * fence. Lines end at a "\r" or a "\n", as micromark ends them. The closing fence is the
 * directive's last source line when that line holds only container prefixes and a colon run at
 * least as long as the opening one, and only counts when it lies past the end of the last kept
 * child: an unclosed directive ends where its content ends, and that content may itself end in
 * a nested directive's own closing fence.
 */
function drawDirective(node: MdastNode, children: MdastNode[], source: string): MdastNode[] {
  const start = node.position?.start?.offset;
  const end = node.position?.end?.offset;
  if (start === undefined || end === undefined) {
    throw new Error(
      `container directive ${JSON.stringify(node.name ?? "")} has no source position`
    );
  }
  let openingEnd = start;
  while (openingEnd < source.length && source[openingEnd] !== "\n" && source[openingEnd] !== "\r") {
    openingEnd++;
  }
  let lastLineStart = end;
  while (
    lastLineStart > 0 &&
    source[lastLineStart - 1] !== "\n" &&
    source[lastLineStart - 1] !== "\r"
  ) {
    lastLineStart--;
  }
  const opening = source.slice(start, openingEnd);
  let openingRun = 0;
  while (opening[openingRun] === ":") openingRun++;
  const kept = node.children?.filter((child) => !isLabel(child)).at(-1);
  const floor = (kept === undefined ? undefined : sourceEnd(kept)) ?? openingEnd;
  const lastLine = source.slice(lastLineStart, end);
  const fence = CLOSING_FENCE.exec(lastLine);
  const drawn: MdastNode[] = [
    { type: "paragraph", children: [{ type: "text", value: opening }] },
    ...children.filter((child) => !isLabel(child)),
  ];
  if (
    fence !== null &&
    fence[1].length >= openingRun &&
    lastLineStart + lastLine.indexOf(":") >= floor
  ) {
    drawn.push({ type: "paragraph", children: [{ type: "text", value: fence[1] }] });
  }
  return drawn;
}

/**
 * A container directive, its content first: a nested directive drawn as text may leave content
 * this block's rule refuses, so the block is kept typed only when `accepts` takes it and its
 * processed content fits, and drawn otherwise.
 */
function decideDirective(node: MdastNode, decision: TypedBlockDecision): MdastNode[] {
  const children = decideTypedBlocks(node.children ?? [], decision);
  if (decision.accepts(node) && fitsContentRule(decision.schema, node.name ?? "", children)) {
    return [{ ...node, children }];
  }
  return drawDirective(node, children, decision.source);
}

/**
 * `nodes` with each container directive, at any depth, decided innermost first: kept a typed
 * block when `accepts` takes it and its processed content fits its ProseMirror content rule, or
 * drawn in place as its literal fence lines around its formatted content, so a drawn block's
 * lines are checked against its parent's rule in turn. The parse's nodes are left as they are,
 * so a directive drawn later reads its own children rather than the paragraphs drawn in their
 * place.
 */
function decideTypedBlocks(nodes: MdastNode[], decision: TypedBlockDecision): MdastNode[] {
  return nodes.flatMap((node) => {
    if (node.type === "containerDirective") return decideDirective(node, decision);
    if (node.children === undefined) return [node];
    return [{ ...node, children: decideTypedBlocks(node.children, decision) }];
  });
}

/**
 * The remark transformer that runs `decideTypedBlocks` over the whole tree, checked against the
 * ProseMirror `schema` the parse builds with, keeping a container directive typed only when
 * `accepts` takes it.
 */
export function remarkDecideTypedBlocks(schema: Schema, accepts: (node: MdastNode) => boolean) {
  return (tree: MdastNode, file: { toString(): string }): void => {
    if (tree.children !== undefined) {
      tree.children = decideTypedBlocks(tree.children, {
        schema,
        source: markdownSource(file),
        accepts,
      });
    }
  };
}

/**
 * The headless engine's default-mode content-rule step, run after `remarkTypedBlocks` (which has
 * already thrown for every directive the block schema refuses) and after the mark plugins, so a
 * block-level mark span is a mark node by the time its blocks are checked. Milkdown drops a
 * typed block whose content breaks its content rule, silently and with all its text; checked
 * against the ProseMirror `schema` the parse builds with, such a block is drawn instead as its
 * literal fence lines around its formatted content, so every word of it stays in the document.
 */
export function remarkUnfitTypedBlocks(schema: Schema) {
  return remarkDecideTypedBlocks(schema, () => true);
}
