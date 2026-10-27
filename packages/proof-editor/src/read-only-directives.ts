import {
  MarkType,
  NodeType,
  type Node as ProseMirrorNode,
  type Schema,
} from "@milkdown/prose/model";
import { type BlockSchema, typedBlockRefusal, validateBlockSchema } from "./block-schema.js";

type MdastNode = {
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

type Context = { blockSchema: BlockSchema | undefined; schema: Schema; source: string };

/** The part of a Milkdown node or mark spec that names the markdown nodes it parses. */
type ParseRule = { parseMarkdown?: { match(node: MdastNode): boolean } };

/** A closing fence line: container prefixes, a run of three or more colons, trailing blanks. */
const CLOSING_FENCE = /^[ \t>]*(:{3,})[ \t]*$/;

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
 * this block's rule refuses, so the block is kept typed only when the schema accepts it and its
 * processed content fits, and drawn otherwise.
 */
function readOnlyDirective(node: MdastNode, context: Context): MdastNode[] {
  const { blockSchema, schema } = context;
  const refused = blockSchema === undefined || typedBlockRefusal(blockSchema, node) !== undefined;
  const children = readOnlyChildren(node.children ?? [], context);
  if (!refused && fitsContentRule(schema, node.name ?? "", children)) {
    return [{ ...node, children }];
  }
  return drawDirective(node, children, context.source);
}

/**
 * `nodes` with each container directive decided. The parse's nodes are left as they are, so a
 * directive drawn later reads its own children rather than the paragraphs drawn in their place.
 */
function readOnlyChildren(nodes: MdastNode[], context: Context): MdastNode[] {
  return nodes.flatMap((node) => {
    if (node.type === "containerDirective") return readOnlyDirective(node, context);
    if (node.children === undefined) return [node];
    return [{ ...node, children: readOnlyChildren(node.children, context) }];
  });
}

/**
 * The read-only counterpart of `remarkTypedBlocks`, for bodies that are only displayed. A body
 * the document service would refuse still has to be shown, and refusing it whole sends the
 * reader its raw source; so a container directive the block schema refuses (or any, without a
 * block schema) is drawn in place as its literal fence lines around its formatted content, and
 * the paragraph-line directive refusals are not applied, so those lines read as the text they
 * are. Accepted typed blocks stay typed, at any depth, as long as their content fits their
 * content rule: Milkdown drops a node whose content breaks its rule, silently and with all its
 * text, so checked against the ProseMirror `schema` the parse builds with, a typed block whose
 * content would not fit (a heading in a `paragraph+` block, or a drawn directive's paragraphs
 * after its list) is drawn as text too. Blocks are decided innermost first, so a drawn block's
 * lines are checked against its parent's rule in turn. The lines are cut from the file without
 * one leading U+FEFF, which micromark drops before it counts the offsets the tree carries.
 */
export function remarkReadOnlyDirectives(blockSchema: BlockSchema | undefined, schema: Schema) {
  if (blockSchema !== undefined) validateBlockSchema(blockSchema);
  return (tree: MdastNode, file: { toString(): string }): void => {
    if (tree.children !== undefined) {
      const text = file.toString();
      tree.children = readOnlyChildren(tree.children, {
        blockSchema,
        schema,
        source: text.startsWith("\ufeff") ? text.slice(1) : text,
      });
    }
  };
}
