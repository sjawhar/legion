import type { ReactNode } from "react";

import { CopyButton } from "../../components/CopyButton";
import { buildDispatchReference, type DispatchReferenceRoute } from "./routes";

/** Matches the copy-reference control inside a row or card, for a keymap that clicks it. */
export const COPY_REF_SELECTOR = "[data-copy-ref] button";

const MODIFIER =
  typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform) ? "⌘" : "Ctrl";

/** The one copy control for a node's `dispatch://` reference, shown wherever the node is. A click
 *  copies the reference. When the surface has a shorter identifier worth keeping one click away
 *  — the issue header's key — `primary` names it: a click copies `primary.value` and a
 *  Ctrl/Cmd-click copies the reference, and the button's name says so. */
export function CopyRefButton({
  className,
  primary,
  route,
}: {
  className?: string;
  primary?: { label: string; value: string };
  route: DispatchReferenceRoute;
}): ReactNode {
  const reference = buildDispatchReference(route);
  return (
    <span className="contents" data-copy-ref="">
      {primary === undefined ? (
        <CopyButton className={className} value={reference} what="reference" />
      ) : (
        <CopyButton
          className={className}
          label={`Copy ${primary.label} · ${MODIFIER}-click copies the reference`}
          value={(event) => (event.metaKey || event.ctrlKey ? reference : primary.value)}
          what="reference"
        />
      )}
    </span>
  );
}
