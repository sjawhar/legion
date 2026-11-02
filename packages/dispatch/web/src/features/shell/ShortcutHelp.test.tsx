import { expect, test } from "bun:test";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import type { KeyBindingDescription } from "./keymap";
import { ShortcutHelp } from "./ShortcutHelp";

const snapshot: KeyBindingDescription[] = [
  { enabled: true, id: "next", keys: ["j"], label: "Next component", scope: "architecture" },
  { enabled: true, id: "toggle-view", keys: ["v"], label: "Toggle List / Board", scope: "project" },
  { enabled: true, id: "search", keys: ["$mod+k"], label: "Search", scope: "global" },
];

test("? lists the Architecture section between Project and Board, with its bindings", () => {
  const view = render(
    <MemoryRouter>
      <ShortcutHelp onClose={() => {}} snapshot={snapshot} />
    </MemoryRouter>
  );
  try {
    const dialog = screen.getByRole("dialog", { name: "Keyboard shortcuts" });
    const sections = within(dialog).getAllByRole("region");
    expect(sections.map((section) => section.getAttribute("aria-label"))).toEqual([
      "Global",
      "Project",
      "Architecture",
    ]);
    expect(
      within(screen.getByRole("region", { name: "Architecture" })).getByText("Next component")
    ).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("? offers Report a problem, which closes the panel and opens the report", () => {
  const calls: string[] = [];
  const view = render(
    <MemoryRouter>
      <ShortcutHelp
        onClose={() => calls.push("close")}
        onReport={() => calls.push("report")}
        snapshot={snapshot}
      />
    </MemoryRouter>
  );
  try {
    fireEvent.click(screen.getByRole("button", { name: "Report a problem" }));
    expect(calls).toEqual(["close", "report"]);
  } finally {
    view.unmount();
  }
});
