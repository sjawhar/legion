import type { Schema } from "@milkdown/prose/model";
import { type BlockSchema, typedBlockRefusal, validateBlockSchema } from "./block-schema.js";
import { type MdastNode, remarkDecideTypedBlocks } from "./typed-block-drawing.js";

/**
 * `node`'s attributes without those its block type marks `server` in `blockSchema`, so the parse
 * fills each with its schema default. The block id and the client-owned attributes are kept.
 * Throws when `blockSchema` has no type of `node`'s name, which `typedBlockRefusal` refuses first.
 */
function clientAttributes(
  blockSchema: BlockSchema,
  node: MdastNode
): Record<string, string> | undefined {
  const type = blockSchema.types.find((candidate) => candidate.name === node.name);
  if (type === undefined) {
    throw new Error(`no block type for the accepted directive ":::${node.name ?? ""}"`);
  }
  if (node.attributes === undefined) return undefined;
  return Object.fromEntries(
    Object.entries(node.attributes).filter(([name]) => type.attributes[name]?.server !== true)
  );
}

/**
 * Drops, at any depth, every attribute the block schema marks `server` from each container
 * directive `remarkDecideTypedBlocks` kept a typed block (it draws every other one as
 * paragraphs), so each reads at its schema default: outside a document no server wrote them, so
 * a body's `:::ask{state="answered" …}` reads as the open decision its author wrote, never as an
 * answer nobody recorded. It runs after the decision, so a forged server attribute with a value
 * the schema refuses (`:::ask{state="bogus"}`) still has the block drawn as its fence lines.
 */
function dropServerAttributes(nodes: MdastNode[], blockSchema: BlockSchema): void {
  for (const node of nodes) {
    if (node.type === "containerDirective") node.attributes = clientAttributes(blockSchema, node);
    if (node.children !== undefined) dropServerAttributes(node.children, blockSchema);
  }
}

/**
 * The read-only counterpart of `remarkTypedBlocks`, for bodies that are only displayed. A body
 * the document service would refuse still has to be shown, and refusing it whole sends the
 * reader its raw source; so a container directive the block schema refuses (or any, without a
 * block schema) is drawn in place as its literal fence lines around its formatted content, and
 * the paragraph-line directive refusals are not applied, so those lines read as the text they
 * are. Accepted typed blocks stay typed, at any depth the tokenizer opens, as long as their
 * content fits their content rule: Milkdown drops a node whose content breaks its rule, silently
 * and with all its text, so checked against the ProseMirror `schema` the parse builds with, a
 * typed block whose content would not fit (a heading in a `paragraph+` block, or a drawn
 * directive's paragraphs after its list) is drawn as text too. Blocks are decided innermost first,
 * so a drawn block's lines are checked against its parent's rule in turn. Directives nest at most
 * `maxDirectiveNesting` (32) deep: an opener inside that many open directive containers is
 * already text from the tokenizer (./directive-container), so it reaches this transform as a line
 * of the paragraph it stands in and is drawn as that line. A typed block kept typed loses its
 * server attributes (`dropServerAttributes` says why). The lines are cut from the file without
 * one leading U+FEFF, which micromark drops before it counts the offsets the tree carries.
 */
export function remarkReadOnlyDirectives(blockSchema: BlockSchema | undefined, schema: Schema) {
  if (blockSchema !== undefined) validateBlockSchema(blockSchema);
  const decide = remarkDecideTypedBlocks(
    schema,
    (node) => blockSchema !== undefined && typedBlockRefusal(blockSchema, node) === undefined
  );
  return (tree: MdastNode, file: { toString(): string }): void => {
    decide(tree, file);
    if (blockSchema !== undefined && tree.children !== undefined) {
      dropServerAttributes(tree.children, blockSchema);
    }
  };
}
