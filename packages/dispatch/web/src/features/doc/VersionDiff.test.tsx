import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";

import { VersionDiff } from "./VersionDiff";

test("VersionDiff marks removed and added lines", () => {
  const { container } = render(
    <VersionDiff after={"# Decision\nUse Postgres\n"} before={"# Decision\nUse SQLite\n"} />
  );

  expect(container.querySelector("del")?.textContent).toBe("Use SQLite\n");
  expect(container.querySelector("ins")?.textContent).toBe("Use Postgres\n");
  expect(container.querySelector("del")?.className).toContain("removed");
  expect(container.querySelector("ins")?.className).toContain("added");
  expect(screen.getByText("# Decision")).not.toBeNull();
});
