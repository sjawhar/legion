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

/**
 * A refused directive drawn as its source: the opening fence line, its content (label paragraph
 * dropped, since the opening line already shows the label) and, when the source closes it, the
 * closing fence. The closing fence is the trailing colon run of the directive's last source line,
 * and only counts when it lies past the last kept child: an unclosed directive ends where its
 * content ends, and that content may itself end in a nested directive's own closing fence.
 */
function drawDirective(node: MdastNode, schema: BlockSchema | undefined, source: string) {
  const start = node.position?.start?.offset;
  const end = node.position?.end?.offset;
  if (start === undefined || end === undefined) {
    throw new Error(
      `container directive ${JSON.stringify(node.name ?? "")} has no source position`
    );
  }
  const lineBreak = source.indexOf("\n", start);
  const openingEnd = lineBreak === -1 ? source.length : lineBreak;
  const kept = (node.children ?? []).filter(
    (child) => !(child.type === "paragraph" && child.data?.directiveLabel === true)
  );
  const floor = kept.at(-1)?.position?.end?.offset ?? openingEnd;
  const lastLineStart = source.lastIndexOf("\n", end - 1) + 1;
  const fence = /:+$/.exec(source.slice(lastLineStart, end).trimEnd());
  const drawn: MdastNode[] = [
    {
      type: "paragraph",
      children: [{ type: "text", value: source.slice(start, openingEnd).replaceAll("\r", "") }],
    },
    ...readOnlyChildren(kept, schema, source),
  ];
  if (fence !== null && lastLineStart + fence.index >= floor) {
    drawn.push({ type: "paragraph", children: [{ type: "text", value: fence[0] }] });
  }
  return drawn;
}

function readOnlyChildren(
  nodes: MdastNode[],
  schema: BlockSchema | undefined,
  source: string
): MdastNode[] {
  return nodes.flatMap((node) => {
    if (
      node.type === "containerDirective" &&
      (schema === undefined || typedBlockRefusal(schema, node) !== undefined)
    ) {
      return drawDirective(node, schema, source);
    }
    if (node.children !== undefined) {
      node.children = readOnlyChildren(node.children, schema, source);
    }
    return [node];
  });
}

/**
 * The read-only counterpart of `remarkTypedBlocks`, for bodies that are only displayed. A body
 * the document service would refuse still has to be shown, and refusing it whole sends the
 * reader its raw source; so a container directive the schema refuses (or any, without a schema)
 * is drawn in place as its literal fence lines around its formatted content, and the
 * paragraph-line directive refusals are not applied, so those lines read as the text they are.
 * Accepted typed blocks stay typed, at any depth.
 */
export function remarkReadOnlyDirectives(schema?: BlockSchema) {
  if (schema !== undefined) validateBlockSchema(schema);
  return (tree: MdastNode, file: { toString(): string }): void => {
    if (tree.children !== undefined) {
      tree.children = readOnlyChildren(tree.children, schema, file.toString());
    }
  };
}
