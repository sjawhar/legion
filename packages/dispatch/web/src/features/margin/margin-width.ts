/** How wide the margin opens where its reader has set no width of their own.
 *
 *  Its own module because the shell reads it for the stored preference it hands `Margin`
 *  (`app.tsx`), while `Margin` itself is a chunk the shell loads only once it is up
 *  (`features/shell/deferred-shell.ts`): one number must not pull the margin's whole column,
 *  its sheet and its composer into the entry chunk. */
export const DEFAULT_MARGIN_WIDTH = 384;
