import { expect, test } from "bun:test";
import { fireEvent, render } from "@testing-library/react";

import type { ReactNode } from "react";

import { useDialog } from "./useDialog";

function Dialog({
  label,
  onClose,
  open,
  children,
}: {
  label: string;
  onClose: () => void;
  open: boolean;
  children?: ReactNode;
}) {
  const dialog = useDialog<HTMLDivElement>({ onClose, open });
  if (!open) return null;
  return (
    <div aria-label={label} ref={dialog.containerRef} role="dialog">
      <button type="button">{label}</button>
      {children}
    </div>
  );
}

test("Escape pressed outside an open dialog still closes it", () => {
  let closed = 0;
  const view = render(
    <Dialog label="Sheet" onClose={() => (closed += 1)} open>
      <p>body</p>
    </Dialog>
  );
  try {
    fireEvent.keyDown(document.body, { key: "Escape" });
    expect(closed).toBe(1);
  } finally {
    view.unmount();
  }
});

test("Escape closes only the innermost of two nested open dialogs", () => {
  const closed = { picker: 0, sheet: 0 };
  const view = render(
    <Dialog label="Sheet" onClose={() => (closed.sheet += 1)} open>
      <Dialog label="Picker" onClose={() => (closed.picker += 1)} open />
    </Dialog>
  );
  try {
    fireEvent.keyDown(document.body, { key: "Escape" });
    expect(closed).toEqual({ picker: 1, sheet: 0 });
    fireEvent.keyDown(view.getByRole("dialog", { name: "Picker" }), { key: "Escape" });
    expect(closed).toEqual({ picker: 2, sheet: 0 });
  } finally {
    view.unmount();
  }
});
