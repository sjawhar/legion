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

/** One reference the host's scanner reads in a run of text: `start` is the index of its first
 *  character, `end` one past its last, and `key` what the host resolves it by
 *  (`setReferenceTargets`). Two candidates with the same key name the same target. */
export interface ReferenceCandidate {
  readonly start: number;
  readonly end: number;
  readonly key: string;
}

/** What a candidate resolves to: `href` is the `dispatch://` reference the drawn link carries as
 *  `data-dispatch-href` (the host's click handler and hover card read it), `title` the words the
 *  editor shows in place of the stored text, and `titleKey` an issue key `title` starts with,
 *  drawn in semibold. */
export interface ReferenceTarget {
  readonly href: string;
  readonly title: string;
  readonly titleKey?: string;
}

/** The host's side of the editor's reference titles (`CreateProofEditorOptions.references`). */
export interface EditorReferences {
  /** Every reference candidate in `text`, in order and not overlapping. The text is one text
   *  block's, with code and an author-worded link's text masked out; it must answer the same
   *  candidates for the same text. */
  scan(text: string): readonly ReferenceCandidate[];
  /** Called with the distinct keys of the document's candidates, sorted, whenever that set
   *  changes (the editor starts with none), so the host knows what to resolve. */
  onKeys(keys: readonly string[]): void;
}

/** One candidate in the document. Kept as an undrawn inline decoration so mapping carries its
 *  range through an edit outside its text block. */
interface CandidateSpec {
  readonly key: string;
  /** The marks its title is drawn inside: those of its first character, but a link's. */
  readonly marks: readonly Mark[];
}

interface ReferenceDecorations {
  readonly candidates: DecorationSet;
  /** The distinct keys of `candidates`, sorted; the same array while that set is unchanged. */
  readonly keys: readonly string[];
  readonly targets: ReadonlyMap<string, ReferenceTarget | null>;
  readonly focused: boolean;
  /** The ranges (`from:to`) of the resolved candidates the selection touches, joined by spaces. */
  readonly revealed: string;
  readonly decorations: DecorationSet;
}

type ReferenceChange =
  | { readonly kind: "targets"; readonly targets: ReadonlyMap<string, ReferenceTarget | null> }
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

function isLink(mark: Mark): boolean {
  return mark.type.name === "link";
}

/** A run of text one link mark covers. */
interface LinkRun {
  readonly mark: Mark;
  readonly start: number;
  end: number;
  text: string;
}

/** The candidates of one text block whose content starts at `start`. The text scanned is the
 *  block's with code and links masked; a link whose text is its own address is one candidate,
 *  when the scanner reads that address as one reference. */
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
    const link = child.marks.find(isLink);
    if (link !== undefined) {
      const last = runs.at(-1);
      if (last !== undefined && last.end === offset && last.mark.eq(link)) {
        last.end += childText.length;
        last.text += childText;
      } else {
        runs.push({ end: offset + childText.length, mark: link, start: offset, text: childText });
      }
      text += LINK_MASK.repeat(childText.length);
    } else {
      const code = child.marks.some((mark) => mark.type.spec.code === true);
      text += code ? CODE_MASK.repeat(childText.length) : childText;
    }
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
    found.push(candidate);
    reached = candidate.end;
  }
  for (const run of runs) {
    const href = run.mark.attrs.href;
    if (typeof href !== "string" || run.text !== href) continue;
    const [only, ...others] = references.scan(href);
    if (only !== undefined && others.length === 0 && only.start === 0 && only.end === href.length) {
      found.push({ end: run.end, key: only.key, start: run.start });
    }
  }

  return found.map((candidate) => {
    const marks = block.childAfter(candidate.start).node?.marks.filter((mark) => !isLink(mark));
    const spec: CandidateSpec = { key: candidate.key, marks: marks ?? [] };
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

/** The ranges of the document after `transaction` that its steps wrote: replaced content and the
 *  text a mark step changed. `undefined` when a step's reach is unknown, which rescans it all. */
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
 *  an edit touched scanned again. */
function nextCandidates(
  candidates: DecorationSet,
  transaction: Transaction,
  references: EditorReferences
): DecorationSet {
  const { doc } = transaction;
  const ranges = changedRanges(transaction);
  if (ranges === undefined) {
    return DecorationSet.create(doc, candidatesBetween(doc, 0, doc.content.size, references));
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

/** The ranges of the resolved candidates the selection touches (a caret inside or at either end,
 *  or a range overlapping one), while the editor has focus; empty otherwise. */
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
      if (targets.get((candidate.spec as CandidateSpec).key)) {
        ranges.push(`${candidate.from}:${candidate.to}`);
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
    if (!target || shown.has(`${candidate.from}:${candidate.to}`)) continue;
    decorations.push(
      Decoration.inline(candidate.from, candidate.to, { class: HIDDEN_TEXT_CLASS }),
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
      target.titleKey !== other.titleKey
    ) {
      return false;
    }
  }
  return true;
}

function dispatchReferenceChange(view: EditorView, change: ReferenceChange): void {
  if (view.isDestroyed) return;
  view.dispatch(view.state.tr.setMeta(referencesKey, change).setMeta("addToHistory", false));
}

/**
 * The editor's reference titles, drawn without a change to the stored text. The host's scanner
 * reads each text block (code blocks, code spans and a link whose author wrote its text left out;
 * a link whose text is its own address read as that address), and the host says what each
 * candidate's key resolves to (`setReferenceTargets`). A resolved candidate is drawn as its
 * target's title, a link carrying `data-dispatch-href`, with its stored text hidden; while the
 * focused editor's selection touches it, the stored text shows instead, so an author edits what is
 * stored. An edit rescans only the text blocks it touched; mapping carries the other candidates.
 */
export const referenceDecorationsPlugin = (references: EditorReferences) =>
  $prose(
    () =>
      new Plugin<ReferenceDecorations>({
        key: referencesKey,
        state: {
          init: (_config, state) => {
            const candidates = DecorationSet.create(
              state.doc,
              candidatesBetween(state.doc, 0, state.doc.content.size, references)
            );
            return {
              candidates,
              decorations: DecorationSet.empty,
              focused: false,
              keys: candidateKeys(candidates, []),
              revealed: "",
              targets: new Map(),
            };
          },
          apply(transaction, value, _previous, state) {
            const change = transaction.getMeta(referencesKey) as ReferenceChange | undefined;
            const targets = change?.kind === "targets" ? change.targets : value.targets;
            const focused = change?.kind === "focus" ? change.focused : value.focused;
            const candidates = transaction.docChanged
              ? nextCandidates(value.candidates, transaction, references)
              : value.candidates;
            const revealed = revealedRanges(state, candidates, targets, focused);
            if (
              candidates === value.candidates &&
              targets === value.targets &&
              revealed === value.revealed
            ) {
              return focused === value.focused ? value : { ...value, focused };
            }
            return {
              candidates,
              decorations: drawnDecorations(state.doc, candidates, targets, revealed),
              focused,
              keys:
                candidates === value.candidates
                  ? value.keys
                  : candidateKeys(candidates, value.keys),
              revealed,
              targets,
            };
          },
        },
        view(view) {
          let keys = referencesKey.getState(view.state)?.keys ?? [];
          if (keys.length > 0) references.onKeys(keys);
          return {
            update(updated) {
              const next = referencesKey.getState(updated.state)?.keys ?? [];
              if (next !== keys) {
                keys = next;
                references.onKeys(keys);
              }
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

/** Says what each candidate key resolves to; a key absent from `targets` or mapped to `null` is
 *  drawn as its stored text. A map with the same entries as the current one changes nothing. */
export function setReferenceTargets(
  view: EditorView,
  targets: ReadonlyMap<string, ReferenceTarget | null>
): void {
  const current = referencesKey.getState(view.state);
  if (current === undefined || sameTargets(current.targets, targets)) return;
  dispatchReferenceChange(view, { kind: "targets", targets: new Map(targets) });
}
