import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { Plugin, PluginKey } from "@milkdown/kit/prose/state";
import { Decoration, DecorationSet, type EditorView } from "@milkdown/kit/prose/view";

import type { Decision, DecisionLink } from "../../api/types";
import { loadedEngine } from "../refs/markdown-engine";
import { buildReferencePath, parseDispatchReference } from "../refs/routes";

interface Notes {
  readonly artifactId: string;
  readonly decisions: readonly Decision[];
  readonly historical: boolean;
}
const key = new PluginKey<{ notes: Notes; decorations: DecorationSet }>("dispatch-decision-notes");

export interface DecisionNoteMatch {
  readonly decision: Decision;
  readonly link: DecisionLink;
  readonly node: ProseMirrorNode;
  readonly pos: number;
  readonly candidates: number;
}

function contentKey(node: ProseMirrorNode): string {
  return JSON.stringify(node.toJSON(), (name, value: unknown) => name === "blockId" ? undefined : value);
}

/** Historical untyped blocks lost their IDs in stored Markdown. Compare their parsed stored
 * text, and explicitly return every identical candidate rather than inventing a position. */
export function decisionNoteMatches(doc: ProseMirrorNode, notes: Notes, parse = loadedEngine()?.parse): DecisionNoteMatch[] {
  const nodes: { node: ProseMirrorNode; pos: number }[] = [];
  doc.descendants((node, pos) => { if (node.isBlock) nodes.push({ node, pos }); });
  const matches: DecisionNoteMatch[] = [];
  for (const decision of notes.decisions) {
    if (decision.replaced) continue;
    for (const link of decision.links) {
      if (link.to.parent?.id !== notes.artifactId || link.to.block_id === undefined) continue;
      let candidates = nodes.filter(({ node }) => node.attrs.blockId === link.to.block_id);
      if (candidates.length === 0 && notes.historical && link.to.pin !== undefined && parse !== undefined) {
        const target = parse(link.to.pin.text)?.firstChild;
        if (target !== null && target !== undefined) {
          const text = contentKey(target);
          candidates = nodes.filter(({ node }) => contentKey(node) === text);
        }
      }
      for (const candidate of candidates) matches.push({ ...candidate, decision, link, candidates: candidates.length });
    }
  }
  return matches;
}

function decorations(doc: ProseMirrorNode, notes: Notes): DecorationSet {
  const widgets: Decoration[] = [];
  for (const { node, pos, decision, link, candidates } of decisionNoteMatches(doc, notes)) {
      widgets.push(Decoration.widget(pos + node.nodeSize, () => {
        const aside = document.createElement("aside");
        aside.className = "not-prose my-2 rounded border-l-2 border-sky-500 bg-sky-50 p-3 text-sm text-slate-900 dark:bg-slate-800 dark:text-slate-100";
        aside.dataset.decisionShaped = decision.ref;
        aside.contentEditable = "false";
        const label = document.createElement("p");
        label.textContent = decision.kind === "person" ? "Shaped by this person's decision" : "Shaped by this agent's decision";
        aside.append(label);
        if (candidates > 1) {
          const ambiguous = document.createElement("p");
          ambiguous.textContent = `One of ${candidates} identical passages; the stored version records no position.`;
          aside.append(ambiguous);
        }
        const title = document.createElement("a");
        const route = parseDispatchReference(decision.ref);
        if (route !== undefined) title.href = buildReferencePath(route);
        title.textContent = decision.heading;
        title.className = "font-medium underline";
        aside.append(title);
        const text = document.createElement("div");
        text.className = "whitespace-pre-wrap";
        text.textContent = decision.text;
        aside.append(text);
        if (link?.stale || link?.to.pin?.state === "changed") {
          const changed = document.createElement("p");
          changed.textContent = link.to.pin?.state === "changed" ? "This passage changed since the decision." : "The decision changed after this passage.";
          aside.append(changed);
        }
        return aside;
      }, { key: `${decision.ref}:${pos}:${decision.text_sha256}:${link.stale}:${link.to.pin?.state}:${candidates}`, side: 1 }));
  }
  return DecorationSet.create(doc, widgets);
}

/** Decorations are not document content. They cannot become a second copy of an answer in
 * Yjs, an editor paste, a document version or the block index. */
export function setDecisionNotes(view: EditorView, artifactId: string, decisions: readonly Decision[], historical = false): void {
  const notes = { artifactId, decisions, historical };
  if (key.getState(view.state) === undefined) {
    const plugin = new Plugin({
      key,
      state: {
        init: (_, state) => ({ notes, decorations: decorations(state.doc, notes) }),
        apply: (tr, prior) => {
          const next: Notes = tr.getMeta(key) ?? prior.notes;
          return tr.docChanged || next !== prior.notes
            ? { notes: next, decorations: decorations(tr.doc, next) }
            : prior;
        },
      },
      props: { decorations: (state) => key.getState(state)?.decorations ?? null },
    });
    view.updateState(view.state.reconfigure({ plugins: [...view.state.plugins, plugin] }));
  } else {
    view.dispatch(view.state.tr.setMeta(key, notes));
  }
}
