/**
 * The upstream boundary carries real types.
 *
 * proof-sdk ships sources no consumer's tsc program may open (AGENTS.md § The upstream
 * boundary). Before `upstream/` existed, every name crossing that boundary resolved to `any`:
 * the opaque declaration they were mapped to was a script, not a module, so each import
 * silently became `any` under the copied files' `@ts-nocheck` — 49 bindings, and with them
 * `ProofEditorHandle.applyRemoteMarks`, whose mark records Dispatch builds.
 *
 * This file has no runtime: `bun run typecheck` is what runs it. `Assert<NotAny<T>>` resolves
 * to `never` when `T` is `any`, which no `true` satisfies, and an `@ts-expect-error` case fails
 * as an unused directive the moment the shape it rejects stops being rejected.
 * `tests/upstream-pin.test.ts` is the other half — declarations that no longer match the pinned
 * sources are stale, and stale declarations are a lie tsc cannot catch.
 */

import type { setCurrentActor } from "proof-sdk-upstream/src/editor/actor";
import type {
  agentCursorCtx,
  agentCursorPlugin,
} from "proof-sdk-upstream/src/editor/plugins/agent-cursor";
import type { arrowCommentPlugin } from "proof-sdk-upstream/src/editor/plugins/arrow-comment";
import type { authoredTrackerPlugin } from "proof-sdk-upstream/src/editor/plugins/authored-tracker";
import type {
  collabCursorBuilder,
  collabSelectionBuilder,
} from "proof-sdk-upstream/src/editor/plugins/collab-cursors";
import type { findHighlightsPlugin } from "proof-sdk-upstream/src/editor/plugins/find-highlights";
import type {
  HeatMapMode,
  heatmapCtx,
  heatmapPlugin,
} from "proof-sdk-upstream/src/editor/plugins/heatmap-decorations";
import type { markPopoverPlugin } from "proof-sdk-upstream/src/editor/plugins/mark-popover";
import type { markdownLinkClickPlugin } from "proof-sdk-upstream/src/editor/plugins/markdown-link-click";
import type {
  accept,
  applyRemoteMarks,
  comment,
  deleteMark,
  getMarks,
  MarkRange,
  marksPlugins,
  reject,
  reply,
  resolve,
  StoredMark,
  setDefaultMarkdownParser,
  suggestReplace,
  unresolve,
} from "proof-sdk-upstream/src/editor/plugins/marks";
import type { mermaidDiagramsPlugin } from "proof-sdk-upstream/src/editor/plugins/mermaid-diagrams";
import type { placeholderPlugin } from "proof-sdk-upstream/src/editor/plugins/placeholder";
import type { suggestionsPlugins } from "proof-sdk-upstream/src/editor/plugins/suggestions";
import type { tableKeyboardPlugin } from "proof-sdk-upstream/src/editor/plugins/table-keyboard";
import type { taskCheckboxesPlugin } from "proof-sdk-upstream/src/editor/plugins/task-checkboxes";
import type {
  codeBlockExtPlugins,
  codeBlockSchemaExt,
} from "proof-sdk-upstream/src/editor/schema/code-block-ext";
import type { frontmatterSchema } from "proof-sdk-upstream/src/editor/schema/frontmatter";
import type { proofMarkPlugins } from "proof-sdk-upstream/src/editor/schema/proof-marks";
import type { remarkProofMarksPlugin } from "proof-sdk-upstream/src/editor/schema/remark-proof-marks-plugin";
import type { generateMarkId } from "proof-sdk-upstream/src/formats/marks.js";
import type {
  proofMarkHandler,
  remarkProofMarks,
} from "proof-sdk-upstream/src/formats/remark-proof-marks.js";

import type { CreateProofEditorOptions, ProofEditorHandle } from "../src/lib";

type NotAny<T> = 0 extends 1 & T ? false : true;
type Assert<T extends true> = T;

/** Every name this package imports across the upstream boundary, and the two the public API
 *  re-exports. A row resolves to `never` — which fails to compile — when its name is `any`. */
export type UpstreamBoundaryIsTyped = [
  // editor/actor
  Assert<NotAny<typeof setCurrentActor>>,
  // editor/plugins/agent-cursor
  Assert<NotAny<typeof agentCursorCtx>>,
  Assert<NotAny<typeof agentCursorPlugin>>,
  // editor/plugins/arrow-comment
  Assert<NotAny<typeof arrowCommentPlugin>>,
  // editor/plugins/authored-tracker
  Assert<NotAny<typeof authoredTrackerPlugin>>,
  // editor/plugins/collab-cursors
  Assert<NotAny<typeof collabCursorBuilder>>,
  Assert<NotAny<typeof collabSelectionBuilder>>,
  // editor/plugins/find-highlights
  Assert<NotAny<typeof findHighlightsPlugin>>,
  // editor/plugins/heatmap-decorations
  Assert<NotAny<HeatMapMode>>,
  Assert<NotAny<typeof heatmapCtx>>,
  Assert<NotAny<typeof heatmapPlugin>>,
  // editor/plugins/mark-popover
  Assert<NotAny<typeof markPopoverPlugin>>,
  // editor/plugins/markdown-link-click
  Assert<NotAny<typeof markdownLinkClickPlugin>>,
  // editor/plugins/marks
  Assert<NotAny<typeof accept>>,
  Assert<NotAny<typeof applyRemoteMarks>>,
  Assert<NotAny<typeof comment>>,
  Assert<NotAny<typeof deleteMark>>,
  Assert<NotAny<typeof getMarks>>,
  Assert<NotAny<MarkRange>>,
  Assert<NotAny<typeof marksPlugins>>,
  Assert<NotAny<typeof reject>>,
  Assert<NotAny<typeof reply>>,
  Assert<NotAny<typeof resolve>>,
  Assert<NotAny<typeof setDefaultMarkdownParser>>,
  Assert<NotAny<StoredMark>>,
  Assert<NotAny<typeof suggestReplace>>,
  Assert<NotAny<typeof unresolve>>,
  // editor/plugins/mermaid-diagrams
  Assert<NotAny<typeof mermaidDiagramsPlugin>>,
  // editor/plugins/placeholder
  Assert<NotAny<typeof placeholderPlugin>>,
  // editor/plugins/suggestions
  Assert<NotAny<typeof suggestionsPlugins>>,
  // editor/plugins/table-keyboard
  Assert<NotAny<typeof tableKeyboardPlugin>>,
  // editor/plugins/task-checkboxes
  Assert<NotAny<typeof taskCheckboxesPlugin>>,
  // editor/schema/code-block-ext
  Assert<NotAny<typeof codeBlockExtPlugins>>,
  Assert<NotAny<typeof codeBlockSchemaExt>>,
  // editor/schema/frontmatter
  Assert<NotAny<typeof frontmatterSchema>>,
  // editor/schema/proof-marks
  Assert<NotAny<typeof proofMarkPlugins>>,
  // editor/schema/remark-proof-marks-plugin
  Assert<NotAny<typeof remarkProofMarksPlugin>>,
  // formats/marks
  Assert<NotAny<typeof generateMarkId>>,
  // formats/remark-proof-marks
  Assert<NotAny<typeof proofMarkHandler>>,
  Assert<NotAny<typeof remarkProofMarks>>,
  // The public API, as Dispatch reads it.
  Assert<NotAny<Parameters<ProofEditorHandle["applyRemoteMarks"]>[0][string]>>,
  Assert<NotAny<NonNullable<CreateProofEditorOptions["heatMapMode"]>>>,
];

declare const handle: ProofEditorHandle;
declare const options: CreateProofEditorOptions;

/** The shapes the boundary has to reject. Each one compiles when its type is `any`, and an
 *  `@ts-expect-error` that stops catching anything is itself an error. */
export function rejectedShapes(): void {
  // @ts-expect-error a mark record's `kind` is a MarkKind, never an arbitrary string
  handle.applyRemoteMarks({ "mark-1": { kind: "replaced" } });
  // @ts-expect-error `resolved` is a boolean
  handle.applyRemoteMarks({ "mark-1": { resolved: "yes" } });
  // @ts-expect-error a heat map mode is one of four literals
  options.heatMapMode = "hiden";
  const mark: StoredMark = { kind: "replace", status: "pending" };
  // @ts-expect-error a suggestion status is pending | accepted | rejected
  mark.status = "resolved";
}
