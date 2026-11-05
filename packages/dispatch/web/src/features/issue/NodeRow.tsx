import { type ReactNode, useLayoutEffect, useRef } from "react";

import {
  borderDefault,
  borderTransparent,
  quoteAccentBorder,
  surfaceMutedHoverBg,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { IDENTITY_HEIGHT_PROPERTY } from "./identity-block";
import { firstLineOf, type NodeChip, type TreeNode } from "./issue-tree";
import { NodeGlyph } from "./NodeGlyph";
import { authorText, pageChrome } from "./page-structure";
import {
  CONTROL_LABELS,
  GAP_LINES,
  GAP_WORDS,
  GLYPH_TOKENS,
  NODE_TYPE_NAMES,
  nodeStateWord,
} from "./page-words";

/**
 * One row of the issue's tree, and the body it opens on focus (dispatch://INFRA-1196 R1, R2,
 * `dispatch://INFRA-1196/artifact/plan-md#b-t14`).
 *
 * A row is the twisty, the type with its shape, the author where the model holds one, the node's
 * first line, its chips, the gaps on it, and its state with its shape where the model holds one;
 * a folded row says how many nodes it holds. The first line is cut at a word (`firstLineOf`) and
 * the whole text is in the body, so nothing is lost and no row truncates a word.
 *
 * The body is drawn for the focused row and for a row pinned open, and for no other row: it is
 * the focus that opens it, never the pointer, and no element of the row carries a `title`, since
 * a reader on a phone or on the keyboard never sees one (R2).
 *
 * The row is a `treeitem` of the tree's one tab stop: the focused row is the only one that takes
 * the focus, and the twisty and the pin inside it take none, so Tab leaves the tree rather than
 * walking its rows. The key bindings are `dispatch://INFRA-1196/artifact/plan-md#b-t15`'s.
 */
export function NodeRow({
  body,
  expanded,
  focused,
  folded,
  indent,
  level,
  node,
  oneLine,
  onFocus,
  onPin,
  onToggle,
  pinned,
  tabStop,
}: {
  /** Whether this row draws its body: it has the focus, or the reader pinned it open. */
  body: boolean;
  /** `true` when its children are drawn, `false` when it holds children and they are not, and
   *  undefined for a node that holds none. */
  expanded?: boolean;
  focused: boolean;
  /** How many nodes it holds while it is folded. */
  folded?: number;
  /** How far in the row is drawn, in pixels. */
  indent: number;
  level: number;
  node: TreeNode;
  /** Whether the row is drawn on one line, which is how the zoomed tree draws it at 1024 px and
   *  below: the row stays one line, cut with an ellipsis, and its body says the rest. */
  oneLine: boolean;
  onFocus: () => void;
  onPin: () => void;
  onToggle: () => void;
  pinned: boolean;
  /** Whether this row is the tree's one tab stop: the focused row, or the tree's first row
   *  before the reader has touched it. */
  tabStop: boolean;
}): ReactNode {
  const row = useRef<HTMLDivElement | null>(null);
  const first = firstLineOf(node.text);
  const state = node.state === undefined ? undefined : nodeStateWord(node.type, node.state);
  const stateGlyphs: Readonly<Record<string, Readonly<Record<string, string>>>> =
    GLYPH_TOKENS.states;
  const stateGlyph = node.state === undefined ? undefined : stateGlyphs[node.type]?.[node.state];

  // The focused row is brought inside the viewport and clear of the sticky identity block after
  // every move, which is the one number the block publishes rather than a guess at its height
  // (R9, `identity-block.ts`).
  useLayoutEffect(() => {
    const element = row.current;
    if (!focused || element === null) return;
    const header = Number.parseFloat(
      getComputedStyle(document.documentElement).getPropertyValue(IDENTITY_HEIGHT_PROPERTY)
    );
    const clear = Number.isFinite(header) ? header : 0;
    const box = element.getBoundingClientRect();
    if (box.top >= clear && box.bottom <= window.innerHeight) return;
    window.scrollBy({ top: box.top - clear - 8 });
  }, [focused]);

  return (
    <div
      aria-expanded={expanded}
      aria-level={level}
      className={`flex min-w-0 flex-col border-l-2 py-1 pr-2 ${
        focused ? quoteAccentBorder : borderTransparent
      } ${surfaceMutedHoverBg}`}
      data-folded-count={folded}
      data-node-id={node.id}
      data-node-row=""
      data-node-state={node.state}
      data-node-type={node.type}
      onFocus={onFocus}
      ref={row}
      role="treeitem"
      style={{ paddingLeft: indent }}
      tabIndex={tabStop ? 0 : -1}
    >
      <div
        className={`flex min-w-0 items-baseline gap-x-2 text-sm ${
          oneLine ? "flex-nowrap overflow-hidden whitespace-nowrap" : "flex-wrap gap-y-1"
        }`}
      >
        <Twisty expanded={expanded} onToggle={onToggle} />
        <span className="flex items-center gap-1" {...pageChrome}>
          <NodeGlyph token={GLYPH_TOKENS.types[node.type]} />
          <span className={textMutedOnCanvas}>{NODE_TYPE_NAMES[node.type]}</span>
        </span>
        {node.author === undefined ? null : (
          <span className={textMutedOnCanvas} data-node-author="" {...authorText}>
            {node.author.id}
          </span>
        )}
        <span
          className={`min-w-0 ${textSecondaryOnCanvas} ${oneLine ? "truncate" : "break-words"}`}
          data-node-first-line=""
          {...authorText}
        >
          {first.cut ? `${first.line}…` : first.line}
        </span>
        {node.alsoUnder.map((chip) => (
          <Chip chip={chip} key={`also:${chip.ref}`} label={CONTROL_LABELS.also_under} />
        ))}
        {node.chips.map((chip) => (
          <Chip chip={chip} key={chip.ref} />
        ))}
        {node.gaps.map((gap) => (
          <span
            className={`rounded border px-1 text-xs ${borderDefault} ${textMutedOnCanvas}`}
            data-gap=""
            data-gap-kind={gap}
            key={gap}
            {...pageChrome}
          >
            {GAP_WORDS[gap]}
          </span>
        ))}
        {folded === undefined ? null : (
          <span className={`text-xs ${textMutedOnCanvas}`} {...pageChrome}>
            <span {...authorText}>{folded}</span> {CONTROL_LABELS.folded_nodes}
          </span>
        )}
        {state === undefined || node.state === undefined ? null : (
          <span className="ml-auto flex items-center gap-1 text-xs" {...pageChrome}>
            {stateGlyph === undefined ? null : <NodeGlyph token={stateGlyph} />}
            <span className={textMutedOnCanvas} data-node-state={node.state}>
              {state}
            </span>
          </span>
        )}
      </div>
      {body ? <NodeBody node={node} onPin={onPin} pinned={pinned} /> : null}
    </div>
  );
}

/** The control that opens and closes a branch. It draws a shape with no name of its own - the
 *  tree's named shapes are its types' and its states' - and carries the page's word for what it
 *  does as its accessible name, never as a tooltip. */
function Twisty({ expanded, onToggle }: { expanded?: boolean; onToggle: () => void }): ReactNode {
  if (expanded === undefined) {
    return <span className="inline-block w-4 shrink-0" />;
  }
  return (
    <button
      aria-label={expanded ? CONTROL_LABELS.fold : CONTROL_LABELS.unfold}
      className={`shrink-0 ${textMutedOnCanvas}`}
      onClick={onToggle}
      tabIndex={-1}
      type="button"
    >
      <svg
        aria-hidden="true"
        className={`h-4 w-4 transition-transform ${expanded ? "rotate-90" : ""}`}
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
        viewBox="0 0 16 16"
      >
        <path d="M6 3 l5 5 l-5 5" />
      </svg>
    </button>
  );
}

/**
 * One edge of the node that is not the spine, naming its target: the page's word for the edge
 * where it has one, the target's own name from the batched reference read, and the target's
 * address, which is how a reader cites it and the only thing that tells two blocks of one
 * document apart. A target nothing answers to now says so instead of naming nothing.
 */
function Chip({ chip, label }: { chip: NodeChip; label?: string }): ReactNode {
  return (
    <span
      className={`rounded border px-1 text-xs ${borderDefault} ${textMutedOnCanvas}`}
      data-node-chip=""
    >
      {label === undefined ? null : <span {...pageChrome}>{label} </span>}
      {chip.gone ? (
        <span {...pageChrome}>{CONTROL_LABELS.target_gone} </span>
      ) : (
        <span {...authorText}>{chip.name === "" ? "" : `${chip.name} `}</span>
      )}
      <span className="break-all" {...authorText}>
        {chip.ref}
      </span>
    </span>
  );
}

/** How wide a node's own text is drawn, in pixels: the measure R1 names for the focused node's
 *  body, which is a line length a person reads rather than the column's whole width. */
const BODY_MEASURE = 660;

/**
 * The node's whole text, with what the model holds about it that a row has no line for: the
 * gaps said in full, the two counts the coverage read carries for a requirement, and the control
 * that keeps the body open once the focus leaves it.
 */
function NodeBody({
  node,
  onPin,
  pinned,
}: {
  node: TreeNode;
  onPin: () => void;
  pinned: boolean;
}): ReactNode {
  return (
    <div
      className="mt-1 ml-6 flex min-w-0 flex-col gap-1 text-sm"
      data-node-body=""
      data-pinned={pinned ? "" : undefined}
      style={{ maxWidth: BODY_MEASURE }}
    >
      <p className={`whitespace-pre-wrap break-words ${textSecondaryOnCanvas}`} {...authorText}>
        {node.text}
      </p>
      {node.gaps.map((gap) => (
        <p className={`text-xs ${textMutedOnCanvas}`} key={gap} {...pageChrome}>
          {GAP_LINES[gap]}
        </p>
      ))}
      {node.counts === undefined ? null : (
        <p className={`text-xs ${textMutedOnCanvas}`} {...pageChrome}>
          <span>{CONTROL_LABELS.serving_checks}: </span>
          <span {...authorText}>{node.counts.checks}</span>
          <span>, {CONTROL_LABELS.serving_plan_tasks}: </span>
          <span {...authorText}>{node.counts.planTasks}</span>
        </p>
      )}
      <p>
        <button
          className={`text-xs underline ${textMutedOnCanvas}`}
          onClick={onPin}
          tabIndex={-1}
          type="button"
          {...pageChrome}
        >
          {pinned ? CONTROL_LABELS.let_close : CONTROL_LABELS.keep_open}
        </button>
      </p>
    </div>
  );
}
