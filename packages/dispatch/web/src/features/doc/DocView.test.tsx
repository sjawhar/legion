import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";

import { DocView } from "./DocView";

test("DocView renders Mermaid fences as inline SVG", () => {
  const { container } = render(<DocView markdown={"```mermaid\ngraph TD\n  A --> B\n```"} />);

  expect(container.querySelector("[data-testid=mermaid-diagram] svg")).not.toBeNull();
});

test("DocView preserves a Mermaid fence when rendering fails", () => {
  render(<DocView markdown={"```mermaid\nunsupported diagram\n```"} />);

  expect(screen.getByRole("alert").textContent).toContain("Could not render Mermaid");
  expect(screen.getByText("unsupported diagram")).not.toBeNull();
});
