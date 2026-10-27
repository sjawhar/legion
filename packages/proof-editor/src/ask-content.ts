import type { Fragment, Schema } from "@milkdown/prose/model";
import type { BlockSchema } from "./block-schema.js";

/**
 * The block node types the document service accepts as a block's child, besides the block
 * schema's typed blocks: the list in pmdoc's `isBlockNodeType`
 * (packages/envoy/internal/dispatch/pmdoc/node.go), which a change to either side must keep equal.
 */
export const SERVER_BLOCK_TYPES: Readonly<Record<string, true>> = {
  paragraph: true,
  heading: true,
  blockquote: true,
  bullet_list: true,
  ordered_list: true,
  code_block: true,
  hr: true,
  table: true,
  footnote_definition: true,
  frontmatter: true,
};

/**
 * Makes `schema`'s `ask` node (when it has one) accept, as a node's content, what the document
 * service accepts in it: at least one block, each of a type the service knows
 * (`SERVER_BLOCK_TYPES` or a typed block of `blockSchema`), and no mark the ask disallows.
 *
 * An ask's content rule is `paragraph+ bullet_list?`, but two collaborators' concurrent edits
 * (two lists, a paragraph after the list) can leave a shared document holding an ask that rule
 * refuses. y-prosemirror builds each node through `createChecked`, and when that throws it deletes
 * the whole element from the shared Yjs document, so every editor and the server lose the ask's
 * text (INFRA-624). Only `validContent` changes, which is what `createChecked`, a step's own
 * check and `Node.check` read; content matching (`contentMatch`, `canReplace`, `createAndFill`)
 * keeps the rule, so every command, input rule, paste and markdown parse still holds a local edit
 * to it, and the typed-block drawing's fit check is untouched.
 */
export function relaxAskContentCheck(schema: Schema, blockSchema: BlockSchema | undefined): void {
  const ask = schema.nodes.ask;
  if (ask === undefined) return;
  const typedBlocks = new Set(blockSchema?.types.map((type) => type.name));
  ask.validContent = (content: Fragment): boolean => {
    if (content.childCount === 0) return false;
    for (let index = 0; index < content.childCount; index++) {
      const child = content.child(index);
      const name = child.type.name;
      if (SERVER_BLOCK_TYPES[name] !== true && !typedBlocks.has(name)) return false;
      if (!ask.allowsMarks(child.marks)) return false;
    }
    return true;
  };
}
