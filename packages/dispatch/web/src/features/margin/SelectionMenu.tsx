import type { ReactNode } from "react";

import type { ComposerKind } from "./Composer";
import type { MarginSelection } from "./Margin";

interface SelectionMenuProps {
  onAction: (kind: ComposerKind) => void;
  selection: MarginSelection;
}

export function SelectionMenu({ onAction, selection }: SelectionMenuProps): ReactNode {
  const left = Math.max(8, Math.min(selection.rect.left, window.innerWidth - 232));
  const top = Math.max(8, selection.rect.top - 44);
  const actions: ComposerKind[] = selection.canSuggest
    ? ["comment", "ask", "suggestion"]
    : ["comment", "ask"];

  return (
    <div
      aria-label="Selection actions"
      className="fixed z-20 flex gap-1 rounded-lg border border-slate-300 bg-white p-1 shadow-lg"
      role="toolbar"
      style={{ left, top }}
    >
      {actions.map((kind) => (
        <button
          className="rounded px-2 py-1 text-sm font-medium text-sky-800 hover:bg-sky-50"
          key={kind}
          onClick={() => onAction(kind)}
          type="button"
        >
          {kind === "comment" ? "Comment" : kind === "ask" ? "Ask" : "Suggest"}
        </button>
      ))}
    </div>
  );
}
