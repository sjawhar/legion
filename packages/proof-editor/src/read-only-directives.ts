import type { Schema } from "@milkdown/prose/model";
import { type BlockSchema, typedBlockRefusal, validateBlockSchema } from "./block-schema.js";
import { remarkDecideTypedBlocks } from "./typed-block-drawing.js";

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
  return remarkDecideTypedBlocks(
    schema,
    (node) => blockSchema !== undefined && typedBlockRefusal(blockSchema, node) === undefined
  );
}
