import {
  Fragment,
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

type Context = { blockSchema?: BlockSchema; schema?: Schema; source: string };

/** The part of a Milkdown node or mark spec that names the markdown nodes it parses. */
type ParseRule = { parseMarkdown?: { match(node: MdastNode): boolean } };

/**
 * Whether Milkdown can build the typed block `name` around `children`. Each child stands in as an
 * empty node of the type Milkdown's ParserState would match it to, and the block's content rule
 * is checked the way ProseMirror's `NodeType.createAndFill` checks it (prosemirror-model 1.25,
 * `createAndFill`: fill before the content, match it, fill to the end); a null there is the
 * `createNodeInParserFail` Milkdown logs while it drops the block. A child Milkdown matches to no
 * node, or to a mark, or one that cannot stand in as an empty node, leaves the block as it was.
 */
function fitsContentRule(schema: Schema, name: string, children: MdastNode[]): boolean {
  const type = schema.nodes[name];
  if (type === undefined) return true;
  const targets = Object.values({ ...schema.nodes, ...schema.marks });
  const placeholders: ProseMirrorNode[] = [];
  for (const child of children) {
    const target = targets.find((candidate) =>
      (candidate.spec as ParseRule).parseMarkdown?.match(child)
    );
    if (!(target instanceof NodeType)) return true;
    const attrs = Object.fromEntries(
      Object.entries(target.spec.attrs ?? {}).map(([key, attr]) => [key, attr.default ?? null])
    );
    try {
      placeholders.push(target.create(attrs));
    } catch {
      return true;
    }
  }
  let content = Fragment.from(placeholders);
  if (content.size) {
    const before = type.contentMatch.fillBefore(content);
    if (!before) return false;
    content = before.append(content);
  }
  const matched = type.contentMatch.matchFragment(content);
  return Boolean(matched?.fillBefore(Fragment.empty, true));
}

/**
 * A directive drawn as its source: the opening fence line, its content (label paragraph dropped,
 * since the opening line already shows the label) and, when the source closes it, the closing
 * fence. The closing fence is the trailing colon run of the directive's last source line, and
 * only counts when it lies past `keptEnd`, where the last kept child ended in the source: an
 * unclosed directive ends where its content ends, and that content may itself end in a nested
 * directive's own closing fence.
 */
function drawDirective(
  node: MdastNode,
  children: MdastNode[],
  keptEnd: number | undefined,
  source: string
): MdastNode[] {
  const start = node.position?.start?.offset;
  const end = node.position?.end?.offset;
  if (start === undefined || end === undefined) {
    throw new Error(
      `container directive ${JSON.stringify(node.name ?? "")} has no source position`
    );
  }
  const lineBreak = source.indexOf("\n", start);
  const openingEnd = lineBreak === -1 ? source.length : lineBreak;
  const floor = keptEnd ?? openingEnd;
  const lastLineStart = source.lastIndexOf("\n", end - 1) + 1;
  const fence = /:+$/.exec(source.slice(lastLineStart, end).trimEnd());
  const drawn: MdastNode[] = [
    {
      type: "paragraph",
      children: [{ type: "text", value: source.slice(start, openingEnd).replaceAll("\r", "") }],
    },
    ...children.filter(
      (child) => !(child.type === "paragraph" && child.data?.directiveLabel === true)
    ),
  ];
  if (fence !== null && lastLineStart + fence.index >= floor) {
    drawn.push({ type: "paragraph", children: [{ type: "text", value: fence[0] }] });
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
  const keptEnd = (node.children ?? [])
    .filter((child) => !(child.type === "paragraph" && child.data?.directiveLabel === true))
    .at(-1)?.position?.end?.offset;
  const children = readOnlyChildren(node.children ?? [], context);
  if (!refused && (schema === undefined || fitsContentRule(schema, node.name ?? "", children))) {
    node.children = children;
    return [node];
  }
  return drawDirective(node, children, keptEnd, context.source);
}

function readOnlyChildren(nodes: MdastNode[], context: Context): MdastNode[] {
  return nodes.flatMap((node) => {
    if (node.type === "containerDirective") return readOnlyDirective(node, context);
    if (node.children !== undefined) {
      node.children = readOnlyChildren(node.children, context);
    }
    return [node];
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
 * text, so given the ProseMirror `schema` the parse builds with, a typed block whose content
 * would not fit (a heading in a `paragraph+` block, or a drawn directive's paragraphs after
 * its list) is drawn as text too. Blocks are decided innermost first, so a drawn block's lines
 * are checked against its parent's rule in turn.
 */
export function remarkReadOnlyDirectives(blockSchema?: BlockSchema, schema?: Schema) {
  if (blockSchema !== undefined) validateBlockSchema(blockSchema);
  return (tree: MdastNode, file: { toString(): string }): void => {
    if (tree.children !== undefined) {
      tree.children = readOnlyChildren(tree.children, {
        blockSchema,
        schema,
        source: file.toString(),
      });
    }
  };
}
