/** The attribute `@legion/proof-editor` stamps a block's id onto (`BLOCK_ID_DOM_ATTR`), written
 * out rather than imported: a value import from the editor library would pull it out of
 * `editor.ts`'s lazy chunk into every page that loads this module, as `ask-block.ts` explains.
 * Exported, so a reader of the drawn document names the attribute the same way. It lives in a
 * module of its own, importing nothing, so naming it costs no caller the document chunk's own
 * dependencies. */
export const BLOCK_ID_ATTRIBUTE = "data-block-id";
