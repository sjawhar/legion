import type { Mark, Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { type EditorState, Plugin, PluginKey, type Transaction } from "@milkdown/kit/prose/state";
import {
  AddMarkStep,
  RemoveMarkStep,
  ReplaceAroundStep,
  ReplaceStep,
} from "@milkdown/kit/prose/transform";
import { Decoration, DecorationSet, type EditorView } from "@milkdown/kit/prose/view";
import { $prose } from "@milkdown/kit/utils";

/** One reference the host's reader finds in a run of the editor's own text: `start` is the index
 *  of its first character, `end` one past its last, and `key` what the host resolves it by
 *  (`EditorReferences.targets`). Two candidates with the same key name the same target. */
export interface ReferenceCandidate {
  readonly start: number;
  readonly end: number;
  readonly key: string;
}

/** What a candidate resolves to: `href` is the `dispatch://` reference the drawn title carries as
 *  `data-dispatch-href` (the host's click handler and hover card read it), `title` the words the
 *  editor draws in place of the stored text, `titleKey` an issue key `title` starts with, drawn
 *  in semibold, and `length` how many of the candidate's characters, from its start, the title
 *  stands for, when that is fewer than all of them: the rest stay drawn as stored text (a `#q2-`
 *  whose question is q2 draws its title for `#q2` and keeps the `-`, so no character the
 *  reference does not name is ever hidden from the author who typed it). */
export interface ReferenceTarget {
  readonly href: string;
  readonly title: string;
  readonly titleKey?: string;
  readonly length?: number;
}

/** The host's side of the editor's reference titles, the whole of it in one value, as
 *  `ChangeDecorations` carries a whole change set: the reader that says what a run of text cites,
 *  what each candidate resolves to, and the callback that asks for the rest.
 *  `ProofEditorHandle.setReferences` replaces the last one. */
export interface EditorReferences {
  /** Every reference candidate in `text`, in order and not overlapping. The text is one text
   *  block's, with code and a link's own text masked out; the same text must answer the same
   *  candidates, since the editor re-reads a block only when an edit touches it. */
  scan(text: string): readonly ReferenceCandidate[];
  /** What each candidate key resolves to; a key absent from the map or mapped to `null` stays
   *  drawn as the words the author wrote. */
  readonly targets: ReadonlyMap<string, ReferenceTarget | null>;
  /** Called with the distinct keys of the document's candidates, sorted, whenever that set
   *  changes, so the host resolves exactly what the document holds. */
  onKeys(keys: readonly string[]): void;
}

/** One candidate in the document. Kept as an undrawn inline decoration so mapping carries its
 *  range through an edit outside its text block. */
interface CandidateSpec {
  readonly key: string;
  /** The marks its title is drawn inside: those of its first character. */
  readonly marks: readonly Mark[];
}

interface ReferenceDecorations {
  /** What the host last set, or `null` while it has set nothing. */
  readonly references: EditorReferences | null;
  readonly candidates: DecorationSet;
  /** The distinct keys of `candidates`, sorted; the same array while that set is unchanged. */
  readonly keys: readonly string[];
  readonly focused: boolean;
  /** The ranges (`from:to`) of the resolved candidates the selection touches, joined by spaces. */
  readonly revealed: string;
  readonly decorations: DecorationSet;
}

type ReferenceChange =
  | { readonly kind: "references"; readonly references: EditorReferences | null }
  | { readonly kind: "focus"; readonly focused: boolean };

const referencesKey = new PluginKey<ReferenceDecorations>("dispatch-reference-decorations");

/** The class that hides a resolved candidate's stored text while its title is drawn. */
const HIDDEN_TEXT_CLASS = "dispatch-reference-text";
/** The class of the drawn title. */
const TITLE_CLASS = "dispatch-reference";
/** Stands in for the characters of a link's text and an inline node other than a hard break: no
 *  reference can start right after it or run through it, and a key right before it ends there, as
 *  the `[` or `)` of the link's stored Markdown would. */
const LINK_MASK = "<";
/** Stands in for code's characters, as its stored backtick does: a reference may start or end
 *  beside it, never inside it. */
const CODE_MASK = "`";

const NO_TARGETS: ReadonlyMap<string, ReferenceTarget | null> = new Map();

/** What stands in for a run of code's text, as its stored backtick does; undefined for text read
 *  as written. A link's text is masked by the walk below, which keeps the run itself. */
function maskFor(marks: readonly Mark[]): string | undefined {
  return marks.some((mark) => mark.type.spec.code === true) ? CODE_MASK : undefined;
}

function linkOf(marks: readonly Mark[]): Mark | undefined {
  return marks.find((mark) => mark.type.name === "link");
}

/** A run of text one link mark covers, in the block's own offsets. */
interface LinkRun {
  readonly mark: Mark;
  readonly start: number;
  end: number;
  text: string;
}

/** The candidates of one text block whose content starts at `start`. The text read is the
 *  block's with code and links masked, so it is as long as the block's own text and a
 *  candidate's offsets are its positions. A link whose text its author wrote keeps those words
 *  and is never drawn over; a link whose text is its own address - the autolink a document
 *  stores as `[dispatch://…](dispatch://…)` - is one candidate of that address. */
function blockCandidates(
  block: ProseMirrorNode,
  start: number,
  references: EditorReferences
): Decoration[] {
  let text = "";
  const runs: LinkRun[] = [];
  block.forEach((child, offset) => {
    if (!child.isText) {
      const leaf = child.type.spec.leafText?.(child);
      text += leaf?.length === child.nodeSize ? leaf : LINK_MASK.repeat(child.nodeSize);
      return;
    }
    const childText = child.text ?? "";
    const link = linkOf(child.marks);
    if (link !== undefined) {
      const last = runs.at(-1);
      if (last !== undefined && last.end === offset && last.mark.eq(link)) {
        last.end += childText.length;
        last.text += childText;
      } else {
        runs.push({ end: offset + childText.length, mark: link, start: offset, text: childText });
      }
      text += LINK_MASK.repeat(childText.length);
      return;
    }
    const mask = maskFor(child.marks);
    text += mask === undefined ? childText : mask.repeat(childText.length);
  });

  const found: Array<{ start: number; end: number; key: string }> = [];
  let reached = 0;
  for (const candidate of references.scan(text)) {
    if (
      candidate.start < reached ||
      candidate.end <= candidate.start ||
      candidate.end > text.length
    ) {
      continue;
    }
    reached = candidate.end;
    found.push(candidate);
  }
  for (const run of runs) {
    const href = run.mark.attrs.href;
    // The whole address and nothing else: the reader must read the link's text, which is its
    // destination, as one reference running end to end.
    if (typeof href !== "string" || run.text !== href) continue;
    const [only, ...others] = references.scan(href);
    if (only !== undefined && others.length === 0 && only.start === 0 && only.end === href.length) {
      found.push({ end: run.end, key: only.key, start: run.start });
    }
  }

  return found.map((candidate) => {
    const spec: CandidateSpec = {
      key: candidate.key,
      // A link's marks but the link itself, so the title stands beside its anchor, never inside
      // it: a link inside a link is no link at all.
      marks:
        block.childAfter(candidate.start).node?.marks.filter((mark) => mark.type.name !== "link") ??
        [],
    };
    return Decoration.inline(start + candidate.start, start + candidate.end, {}, spec);
  });
}

/** Every candidate in the text blocks `from` to `to` touches, code blocks excepted. */
function candidatesBetween(
  doc: ProseMirrorNode,
  from: number,
  to: number,
  references: EditorReferences
): Decoration[] {
  const found: Decoration[] = [];
  doc.nodesBetween(from, to, (node, pos) => {
    if (!node.isTextblock) return true;
    if (node.type.spec.code !== true) found.push(...blockCandidates(node, pos + 1, references));
    return false;
  });
  return found;
}

function allCandidates(doc: ProseMirrorNode, references: EditorReferences): DecorationSet {
  return DecorationSet.create(doc, candidatesBetween(doc, 0, doc.content.size, references));
}

/** The ranges of the document after `transaction` that its steps wrote: replaced content and the
 *  text a mark step changed. `undefined` when a step's reach is unknown, which re-reads it all. */
function changedRanges(transaction: Transaction): Array<[number, number]> | undefined {
  const ranges: Array<[number, number]> = [];
  for (const [index, step] of transaction.steps.entries()) {
    if (
      !(step instanceof ReplaceStep) &&
      !(step instanceof ReplaceAroundStep) &&
      !(step instanceof AddMarkStep) &&
      !(step instanceof RemoveMarkStep)
    ) {
      return undefined;
    }
    const after = transaction.mapping.slice(index);
    ranges.push([after.map(step.from, -1), after.map(step.to, 1)]);
  }
  return ranges;
}

function candidateKeys(candidates: DecorationSet, previous: readonly string[]): readonly string[] {
  const keys = [
    ...new Set(candidates.find().map((candidate) => (candidate.spec as CandidateSpec).key)),
  ].sort();
  const same =
    keys.length === previous.length && keys.every((key, index) => key === previous[index]);
  return same ? previous : keys;
}

/** The candidates after `transaction`: the previous ones mapped through it, with each text block
 *  an edit touched read again. */
function nextCandidates(
  candidates: DecorationSet,
  transaction: Transaction,
  references: EditorReferences
): DecorationSet {
  const { doc } = transaction;
  const ranges = changedRanges(transaction);
  if (ranges === undefined) {
    return allCandidates(doc, references);
  }
  let next = candidates.map(transaction.mapping, doc);
  for (const [rangeFrom, rangeTo] of ranges) {
    // One position wider each way, so a block a deletion joined or split is read whole.
    const from = Math.max(0, Math.min(rangeFrom, rangeTo) - 1);
    const to = Math.min(doc.content.size, Math.max(rangeFrom, rangeTo) + 1);
    const blocks: Array<[number, number]> = [];
    doc.nodesBetween(from, to, (node, pos) => {
      if (!node.isTextblock) return true;
      blocks.push([pos, pos + node.nodeSize]);
      return false;
    });
    for (const [blockFrom, blockTo] of blocks) {
      next = next.remove(next.find(blockFrom, blockTo));
      next = next.add(doc, candidatesBetween(doc, blockFrom, blockTo, references));
    }
  }
  return next;
}

/** Where a resolved candidate's title ends: `length` characters into it, and never past its end,
 *  so a target naming more characters than the candidate holds - its words shortened by an edit
 *  the host has not answered for yet - hides no text beyond the words it was read from. */
function drawnEnd(candidate: Decoration, target: ReferenceTarget): number {
  return target.length === undefined
    ? candidate.to
    : Math.min(candidate.to, candidate.from + target.length);
}

/** The ranges each resolved candidate's title stands for that the selection touches (a caret
 *  inside or at either end, or a range overlapping one), while the editor has focus; empty
 *  otherwise. */
function revealedRanges(
  state: EditorState,
  candidates: DecorationSet,
  targets: ReadonlyMap<string, ReferenceTarget | null>,
  focused: boolean
): string {
  if (!focused) return "";
  const ranges: string[] = [];
  for (const range of state.selection.ranges) {
    for (const candidate of candidates.find(range.$from.pos, range.$to.pos)) {
      const target = targets.get((candidate.spec as CandidateSpec).key);
      if (!target) continue;
      const to = drawnEnd(candidate, target);
      if (to > candidate.from && range.$from.pos <= to) {
        ranges.push(`${candidate.from}:${to}`);
      }
    }
  }
  return ranges.sort().join(" ");
}

function titleWidget(target: ReferenceTarget): (view: EditorView) => HTMLElement {
  return (view) => {
    const document = view.dom.ownerDocument;
    const link = document.createElement("a");
    link.className = TITLE_CLASS;
    link.setAttribute("role", "link");
    link.dataset.dispatchHref = target.href;
    const { title, titleKey } = target;
    if (titleKey !== undefined && titleKey !== "" && title.startsWith(titleKey)) {
      const key = document.createElement("strong");
      key.textContent = titleKey;
      link.append(key, title.slice(titleKey.length));
    } else {
      link.textContent = title;
    }
    // A press on the title neither moves the caret nor focuses the editor, so the title stays
    // drawn until the click lands and the host follows it.
    link.addEventListener("mousedown", (event) => event.preventDefault());
    return link;
  };
}

function drawnDecorations(
  doc: ProseMirrorNode,
  candidates: DecorationSet,
  targets: ReadonlyMap<string, ReferenceTarget | null>,
  revealed: string
): DecorationSet {
  if (targets.size === 0) return DecorationSet.empty;
  const shown = new Set(revealed === "" ? [] : revealed.split(" "));
  const decorations: Decoration[] = [];
  for (const candidate of candidates.find()) {
    const spec = candidate.spec as CandidateSpec;
    const target = targets.get(spec.key);
    if (!target) continue;
    const to = drawnEnd(candidate, target);
    if (to <= candidate.from || shown.has(`${candidate.from}:${to}`)) continue;
    decorations.push(
      Decoration.inline(candidate.from, to, { class: HIDDEN_TEXT_CLASS }),
      Decoration.widget(candidate.from, titleWidget(target), {
        ignoreSelection: true,
        // Widgets with the same key are interchangeable, so ProseMirror keeps the drawn one.
        key: JSON.stringify([target, spec.marks]),
        marks: spec.marks,
        side: 1,
        stopEvent: () => true,
      })
    );
  }
  return DecorationSet.create(doc, decorations);
}

function sameTargets(
  left: ReadonlyMap<string, ReferenceTarget | null>,
  right: ReadonlyMap<string, ReferenceTarget | null>
): boolean {
  if (left.size !== right.size) return false;
  for (const [key, target] of left) {
    const other = right.get(key);
    if (other === undefined) return false;
    if (target === null || other === null) {
      if (target !== other) return false;
    } else if (
      target.href !== other.href ||
      target.title !== other.title ||
      target.titleKey !== other.titleKey ||
      target.length !== other.length
    ) {
      return false;
    }
  }
  return true;
}

/** Whether two sets of references say the same thing, so a host that hands over a fresh value on
 *  every render of its own redraws nothing. */
function sameReferences(left: EditorReferences, right: EditorReferences): boolean {
  return (
    left.scan === right.scan &&
    left.onKeys === right.onKeys &&
    sameTargets(left.targets, right.targets)
  );
}

function dispatchReferenceChange(view: EditorView, change: ReferenceChange): void {
  if (view.isDestroyed) return;
  view.dispatch(view.state.tr.setMeta(referencesKey, change).setMeta("addToHistory", false));
}

/**
 * The editor's reference titles, drawn without a change to the stored text. The host's reader
 * reads each text block (code blocks, code spans and a link's own text left out) and says what
 * each candidate's key resolves to; both reach the plugin through `setReferences`, as a host's
 * change marks reach `./editor-change-decorations.ts`, because a host class written on editor DOM
 * is read back as an edit. A resolved candidate is drawn as its target's title, a link carrying
 * `data-dispatch-href`, with its stored text hidden; while the focused editor's selection touches
 * it, the stored text shows instead, so an author edits what is stored. An edit re-reads only the
 * text blocks it touched; mapping carries the other candidates.
 */
export const referenceDecorationsPlugin = $prose(
  () =>
    new Plugin<ReferenceDecorations>({
      key: referencesKey,
      state: {
        init: () => ({
          candidates: DecorationSet.empty,
          decorations: DecorationSet.empty,
          focused: false,
          keys: [],
          references: null,
          revealed: "",
        }),
        apply(transaction, value, _previous, state) {
          const change = transaction.getMeta(referencesKey) as ReferenceChange | undefined;
          const focused = change?.kind === "focus" ? change.focused : value.focused;
          const references = change?.kind === "references" ? change.references : value.references;
          let candidates = value.candidates;
          if (references === null) {
            candidates = DecorationSet.empty;
          } else if (references.scan !== value.references?.scan) {
            candidates = allCandidates(state.doc, references);
          } else if (transaction.docChanged) {
            candidates = nextCandidates(value.candidates, transaction, references);
          }
          const targets = references?.targets ?? NO_TARGETS;
          const revealed = revealedRanges(state, candidates, targets, focused);
          if (
            candidates === value.candidates &&
            references === value.references &&
            revealed === value.revealed
          ) {
            return focused === value.focused ? value : { ...value, focused };
          }
          return {
            candidates,
            decorations: drawnDecorations(state.doc, candidates, targets, revealed),
            focused,
            keys:
              candidates === value.candidates ? value.keys : candidateKeys(candidates, value.keys),
            references,
            revealed,
          };
        },
      },
      view(view) {
        let keys = referencesKey.getState(view.state)?.keys ?? [];
        return {
          update(updated) {
            const state = referencesKey.getState(updated.state);
            if (state === undefined || state.keys === keys) return;
            keys = state.keys;
            state.references?.onKeys(keys);
          },
        };
      },
      props: {
        decorations: (state: EditorState) => referencesKey.getState(state)?.decorations,
        handleDOMEvents: {
          focus(view) {
            dispatchReferenceChange(view, { focused: true, kind: "focus" });
            return false;
          },
          blur(view) {
            dispatchReferenceChange(view, { focused: false, kind: "focus" });
            return false;
          },
        },
      },
    })
);

/** Draws the references `references` resolves over the document, in place of whatever was drawn
 *  before; `null` draws none. A value saying what the current one says changes nothing. */
export function setReferences(view: EditorView, references: EditorReferences | null): void {
  const current = referencesKey.getState(view.state);
  if (current === undefined || current.references === references) return;
  if (
    current.references !== null &&
    references !== null &&
    sameReferences(current.references, references)
  ) {
    return;
  }
  dispatchReferenceChange(view, { kind: "references", references });
}
