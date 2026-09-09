import { expect, test } from "bun:test";
import { fireEvent, render, screen } from "@testing-library/react";

import { DocView, sanitizeMermaidSvg } from "./DocView";

test("DocView renders Mermaid fences as inline SVG", () => {
  const { container } = render(<DocView markdown={"```mermaid\ngraph TD\n  A --> B\n```"} />);

  expect(container.querySelector("[data-testid=mermaid-diagram] svg")).not.toBeNull();
});

test("DocView preserves a Mermaid fence when rendering fails", () => {
  render(<DocView markdown={"```mermaid\nunsupported diagram\n```"} />);

  expect(screen.getByRole("alert").textContent).toContain("Could not render Mermaid");
  expect(screen.getByText("unsupported diagram")).not.toBeNull();
});

test("sanitizeMermaidSvg removes executable SVG content", () => {
  const fragment = sanitizeMermaidSvg(
    '<svg onload="alert(1)"><script>alert(1)</script><circle cx="5" cy="5" r="5" /></svg>'
  );

  expect(fragment.querySelector("script")).toBeNull();
  expect(
    [...fragment.querySelectorAll("*")]
      .flatMap((element) => element.getAttributeNames())
      .some((name) => name.startsWith("on"))
  ).toBe(false);
});

test("DocView explains when a cross-block selection cannot become an anchor", () => {
  const { container } = render(
    <DocView markdown={"First paragraph.\n\nSecond paragraph."} onSelectionChange={() => {}} />
  );
  const [firstParagraph, secondParagraph] = [...container.querySelectorAll("p")];
  const article = container.querySelector("article");
  if (
    firstParagraph?.firstChild === null ||
    firstParagraph?.firstChild === undefined ||
    secondParagraph?.firstChild === null ||
    secondParagraph?.firstChild === undefined ||
    article === null
  ) {
    throw new Error("Expected two paragraph nodes in DocView.");
  }
  const selection = window.getSelection();
  const range = document.createRange();
  range.setStart(firstParagraph.firstChild, 0);
  range.setEnd(secondParagraph.firstChild, 6);
  selection?.removeAllRanges();
  selection?.addRange(range);

  fireEvent.mouseUp(article);

  expect(screen.getByRole("status").textContent).toContain("cannot be anchored");
});
