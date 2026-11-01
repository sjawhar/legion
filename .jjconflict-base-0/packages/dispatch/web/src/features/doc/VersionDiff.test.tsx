import { expect, test } from "bun:test";
import { render, within } from "@testing-library/react";

import { VersionDiff } from "./VersionDiff";

test("VersionDiff marks removed and added lines", () => {
  const view = render(
    <VersionDiff after={"# Decision\nUse Postgres\n"} before={"# Decision\nUse SQLite\n"} />
  );
  // Unmounted by hand: the suite's preload registers no testing-library cleanup, and a diff left
  // in the shared document reaches every later test that looks for one.
  try {
    const { container } = view;
    expect(container.querySelector("del")?.textContent).toBe("Use SQLite\n");
    expect(container.querySelector("ins")?.textContent).toBe("Use Postgres\n");
    expect(container.querySelector("del")?.className).toContain("removed");
    expect(container.querySelector("ins")?.className).toContain("added");
    expect(within(container).getByText("# Decision")).not.toBeNull();
  } finally {
    view.unmount();
  }
});
