import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";

import { PriorityBadge } from "./Badge";

test.each([
  [0, "P0"],
  [1, "P1"],
  [2, "P2"],
  [3, "P3"],
] as const)("PriorityBadge labels priority %i as %s", (priority, label) => {
  render(<PriorityBadge priority={priority} />);

  expect(screen.getByText(label)).toBeTruthy();
});

test("PriorityBadge is absent when an issue has no priority", () => {
  const { container } = render(<PriorityBadge priority={null} />);

  expect(container.firstChild).toBeNull();
});
