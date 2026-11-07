import { expect, test } from "bun:test";
import { render } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import type { ReactNode } from "react";

import { KeymapProvider } from "./KeymapProvider";
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

test("Escape pressed outside an open dialog still closes it", async () => {
  const user = userEvent.setup();
  let closed = 0;
  const view = render(
    <KeymapProvider>
      <Dialog label="Sheet" onClose={() => (closed += 1)} open>
        <p>body</p>
      </Dialog>
    </KeymapProvider>
  );
  try {
    await user.keyboard("{Escape}");
    expect(closed).toBe(1);
  } finally {
    view.unmount();
  }
});

test("Escape closes only the innermost of two nested open dialogs", async () => {
  const user = userEvent.setup();
  const closed = { picker: 0, sheet: 0 };
  const view = render(
    <KeymapProvider>
      <Dialog label="Sheet" onClose={() => (closed.sheet += 1)} open>
        <Dialog label="Picker" onClose={() => (closed.picker += 1)} open />
      </Dialog>
    </KeymapProvider>
  );
  try {
    await user.keyboard("{Escape}");
    expect(closed).toEqual({ picker: 1, sheet: 0 });
    await user.click(view.getByRole("dialog", { name: "Picker" }));
    await user.keyboard("{Escape}");
    expect(closed).toEqual({ picker: 2, sheet: 0 });
  } finally {
    view.unmount();
  }
});

test("Escape closes the most recently opened of two sibling (non-nested) open dialogs", async () => {
  // Mirrors the phone nav sheet and the Inbox drawer its Peek button opens on top of it
  // (LEGION-547): neither contains the other, so containment alone can't say which is topmost.
  const user = userEvent.setup();
  const closed = { drawer: 0, nav: 0 };
  const view = render(
    <KeymapProvider>
      <Dialog label="Nav" onClose={() => (closed.nav += 1)} open />
      <Dialog label="Drawer" onClose={() => (closed.drawer += 1)} open />
    </KeymapProvider>
  );
  try {
    await user.keyboard("{Escape}");
    expect(closed).toEqual({ drawer: 1, nav: 0 });
  } finally {
    view.unmount();
  }
});
