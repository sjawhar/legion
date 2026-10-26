// The two roots a Dispatch document holds, named as the server names them
// (packages/envoy/internal/dispatch/docs/tree.go): a rename on one side alone would leave the
// browser reading an empty root. They live in a module of their own because the pending sync is a
// lazy chunk of its own: importing them from editor.ts would tie that chunk to the editor's module
// and the shell chunk behind it.

/** The editor's tree. */
export const FRAGMENT_NAME = "prosemirror";
/** The margin records the server projects. */
export const MARKS_MAP_NAME = "marks";
