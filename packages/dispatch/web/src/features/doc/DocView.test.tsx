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

test("DocView maps rendered heading, bold, and link text to Markdown source offsets", () => {
  const cases = [
    { from: 2, markdown: "# Heading", selector: "h1", text: "Heading", to: 9 },
    { from: 2, markdown: "**bold**", selector: "strong", text: "bold", to: 6 },
    { from: 1, markdown: "[link](https://example.com)", selector: "a", text: "link", to: 5 },
  ];

  for (const testCase of cases) {
    let selection: { from: number; quote: string; to: number } | undefined;
    const { container, unmount } = render(
      <DocView
        markdown={testCase.markdown}
        onSelectionChange={(next) => {
          selection = next;
        }}
      />
    );
    const target = container.querySelector(testCase.selector);
    const article = container.querySelector("article");
    const text = target?.firstChild;
    if (target === null || article === null || text === null || text === undefined) {
      throw new Error(`Expected ${testCase.selector} text in DocView.`);
    }
    const range = document.createRange();
    range.selectNodeContents(text);
    const browserSelection = window.getSelection();
    browserSelection?.removeAllRanges();
    browserSelection?.addRange(range);

    fireEvent.mouseUp(article);

    expect(selection).toMatchObject({
      from: testCase.from,
      quote: target.textContent,
      to: testCase.to,
    });
    unmount();

    const highlighted = render(
      <DocView highlight={{ from: testCase.from, to: testCase.to }} markdown={testCase.markdown} />
    );
    expect(highlighted.container.querySelector(".dispatch-anchor-history")?.textContent).toBe(
      testCase.text
    );
    highlighted.unmount();
  }
});

test("DocView maps a selection beginning after Markdown inline syntax", () => {
  let selection: { from: number; quote: string; to: number } | undefined;
  const { container } = render(
    <DocView
      markdown="**bold** [link](https://example.com)"
      onSelectionChange={(next) => {
        selection = next;
      }}
    />
  );
  const article = container.querySelector("article");
  const link = container.querySelector("a");
  const text = link?.firstChild;
  if (article === null || link === null || text === null || text === undefined) {
    throw new Error("Expected link text in DocView.");
  }
  const range = document.createRange();
  range.selectNodeContents(text);
  const browserSelection = window.getSelection();
  browserSelection?.removeAllRanges();
  browserSelection?.addRange(range);

  fireEvent.mouseUp(article);

  expect(selection).toMatchObject({ from: 10, quote: "link", to: 14 });
});
test("DocView replaces an earlier historical highlight before marking the current anchor", () => {
  const markdown = "First **second**";
  const { container, rerender } = render(
    <DocView highlight={{ from: 0, to: 5 }} markdown={markdown} />
  );

  expect(
    [...container.querySelectorAll(".dispatch-anchor-history")].map((mark) => mark.textContent)
  ).toEqual(["First"]);

  rerender(<DocView highlight={{ from: 8, to: 14 }} markdown={markdown} />);

  expect(container.querySelectorAll(".dispatch-anchor-history")).toHaveLength(1);
  expect(container.querySelector(".dispatch-anchor-history")?.textContent).toBe("second");
});
