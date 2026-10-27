import { afterAll, describe, expect, test } from "bun:test";
import { Window } from "happy-dom";
import { renderMermaidMarkup } from "../src/mermaid-render";
import { mermaidSvgElement, sanitizeMermaidSvg } from "../src/mermaid-svg";

// The document is passed in and never installed as `globalThis.document`: ELK, which lays out
// flowcharts and state diagrams, fails to build while a global `document` exists. That is also
// what shows `renderMermaidMarkup` needs no DOM, as it must to run in a Web Worker.
const window = new Window();
const document = window.document as unknown as Document;

afterAll(async () => {
  await window.happyDOM.close();
});

function allElements(root: Element): Element[] {
  return [root, ...Array.from(root.getElementsByTagName("*"))];
}

function elementNames(root: Element): string[] {
  return allElements(root).map((element) => element.localName);
}

function attributeValues(root: Element): string[] {
  return allElements(root).flatMap((element) =>
    Array.from(element.attributes, (attribute) => attribute.value)
  );
}

function parseRaw(markup: string): Element {
  return new window.DOMParser().parseFromString(markup, "image/svg+xml")
    .documentElement as unknown as Element;
}

describe("renderMermaidMarkup", () => {
  test("returns the editor's svg markup for a source", () => {
    const markup = renderMermaidMarkup("sequenceDiagram\n  Alice->>Bob: Hi");

    expect(parseRaw(markup).localName).toBe("svg");
    expect(markup).toContain("Alice");
  });

  test("throws upstream's reason for a source it refuses", () => {
    expect(() => renderMermaidMarkup("graph TD\n  A -->")).toThrow(/^Dangling Mermaid edge/);
  });
});

describe("mermaidSvgElement", () => {
  test("draws a sequence diagram as an svg of the document with the editor's accessible name", () => {
    const markup = renderMermaidMarkup("sequenceDiagram\n  Alice->>Bob: Hi");
    const svg = mermaidSvgElement(markup, document);

    expect(svg.localName).toBe("svg");
    expect(svg.ownerDocument).toBe(document);
    expect(svg.getAttribute("role")).toBe("img");
    expect(svg.getAttribute("aria-label")).toBe("Rendered Mermaid diagram");
    expect(svg.textContent).toContain("Alice");
    expect(svg.textContent).toContain("Hi");
    const rawStyle = parseRaw(markup).getElementsByTagName("style")[0]?.textContent ?? "";
    expect(rawStyle).toContain("@import");
    const style = svg.getElementsByTagName("style")[0]?.textContent ?? "";
    expect(style.trim()).not.toBe("");
    expect(style).not.toContain("@import");
    expect(style).toBe(rawStyle.replace(/@import[^\n]*;/g, ""));
  });

  test("drops an author's classDef and style url() paint and keeps the arrowhead markers", () => {
    const svg = mermaidSvgElement(
      renderMermaidMarkup(
        "graph TD\n  A --> B\n  classDef evil fill:url(https://evil.example/x.png)\n  class A evil\n  style B fill:url(https://evil.example/y.png)\n  linkStyle 0 stroke:\\75rl(https://evil.example/q)"
      ),
      document
    );

    const values = attributeValues(svg);
    expect(values.filter((value) => value.includes("evil.example"))).toEqual([]);
    expect(values.some((value) => value.includes("url(#"))).toBe(true);
  });

  test("throws for markup that is not an svg document", () => {
    expect(() => mermaidSvgElement("<div>not a diagram</div>", document)).toThrow(
      "Invalid SVG output"
    );
  });
});

describe("sanitizeMermaidSvg", () => {
  test("keeps every element beautiful-mermaid draws for each diagram type", () => {
    const diagrams = [
      "graph TD\n  A[Start] --> B{Choice}\n  B -->|Yes| C((Done))\n  B -.-> D[(Store)]",
      "stateDiagram-v2\n  [*] --> Idle\n  Idle --> Running : start\n  Running --> [*]",
      "sequenceDiagram\n  Alice->>Bob: Hi\n  Bob-->>Alice: Hello\n  Note over Alice: think",
      "classDiagram\n  Animal <|-- Duck\n  class Duck {\n    +String beak\n    +swim() void\n  }",
      "erDiagram\n  CUSTOMER ||--o{ ORDER : places\n  CUSTOMER {\n    string name PK\n  }",
      'xychart-beta\n  title "Sales"\n  x-axis [a, b, c]\n  bar [1, 2, 3]\n  line [3, 2, 1]',
    ];
    for (const source of diagrams) {
      const markup = renderMermaidMarkup(source);
      const svg = sanitizeMermaidSvg(markup, document);
      expect(elementNames(svg)).toEqual(elementNames(parseRaw(markup)));
    }
  });

  const hostile = `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"
      viewBox="0 0 10 10" onload="window.hit = 1">
    <style>
      @import url('https://fonts.googleapis.com/css2?family=Inter:wght@400;500&amp;display=swap');
      @IMPORT "https://evil.example/sheet.css";
      @import url(https://evil.example/a;b.css) screen;
      text { fill: red; }
    </style>
    <script>window.hit = 1</script>
    <foreignObject><div xmlns="http://www.w3.org/1999/xhtml">html</div></foreignObject>
    <image href="https://evil.example/pixel.png" width="1" height="1"/>
    <a href="javascript:alert(1)"><rect id="linked" width="1" height="1"/></a>
    <use href="#shape" xlink:href="#shape"/>
    <rect id="shape" width="2" height="2" onclick="alert(1)"/>
    <path d="M0 0L1 1"/>
    <text xlink:href="https://evil.example/">label</text>
  </svg>`;

  test("keeps the shapes, text and styling and drops every script, link and fetch", () => {
    const svg = sanitizeMermaidSvg(hostile, document);

    expect(svg.ownerDocument).toBe(document);
    expect(elementNames(svg)).toEqual(["svg", "style", "rect", "path", "text"]);
    expect(svg.getAttribute("onload")).toBeNull();
    expect(svg.getAttribute("viewBox")).toBe("0 0 10 10");
    const rect = svg.getElementsByTagName("rect")[0];
    expect(rect?.getAttribute("id")).toBe("shape");
    expect(rect?.getAttribute("onclick")).toBeNull();
    expect(svg.getElementsByTagName("path")[0]?.getAttribute("d")).toBe("M0 0L1 1");
    const text = svg.getElementsByTagName("text")[0];
    expect(text?.textContent).toBe("label");
    expect(text?.attributes).toHaveLength(0);
    const style = svg.getElementsByTagName("style")[0]?.textContent ?? "";
    expect(style).toContain("text { fill: red; }");
    expect(style).not.toMatch(/@import|googleapis|evil\.example/i);
  });

  test("drops every attribute that can fetch: a non-local url(), an image function or an escape", () => {
    const svg = sanitizeMermaidSvg(
      `<svg xmlns="http://www.w3.org/2000/svg">
        <rect id="styled" style="fill:url(https://x.example/a)"/>
        <rect id="spaced" fill="url( 'https://x.example/b' )"/>
        <rect id="mixed" fill="url(#grad) url(https://x.example/c)"/>
        <rect id="escaped" fill="\\75rl(https://x.example/d)" stroke="u\\rl(https://x.example/e)"/>
        <rect id="imaged" style="background:image-set('https://x.example/f' 1x)"/>
        <rect id="local" fill="url(#grad)" marker-end='url( "#arrow" )' data-label="a\\b (x.example)"/>
      </svg>`,
      document
    );

    expect(
      attributeValues(svg).filter(
        (value) => value.includes("x.example") && value !== "a\\b (x.example)"
      )
    ).toEqual([]);
    const rects = Array.from(svg.getElementsByTagName("rect"));
    expect(rects.map((rect) => rect.getAttribute("id"))).toEqual([
      "styled",
      "spaced",
      "mixed",
      "escaped",
      "imaged",
      "local",
    ]);
    const local = rects[5];
    expect(local?.getAttribute("fill")).toBe("url(#grad)");
    expect(local?.getAttribute("marker-end")).toBe('url( "#arrow" )');
    // A data-* attribute is no CSS, so beautiful-mermaid's copy of a label keeps its characters.
    expect(local?.getAttribute("data-label")).toBe("a\\b (x.example)");
  });

  test("compares element names exactly, since XML names are case-sensitive", () => {
    expect(() =>
      sanitizeMermaidSvg('<SVG xmlns="http://www.w3.org/2000/svg"><rect/></SVG>', document)
    ).toThrow("Invalid SVG output");

    const svg = sanitizeMermaidSvg(
      `<svg xmlns="http://www.w3.org/2000/svg">
        <RECT id="upper" width="1" height="1"/>
        <rect id="lower" width="1" height="1"/>
      </svg>`,
      document
    );
    expect(elementNames(svg)).toEqual(["svg", "rect"]);
    expect(svg.getElementsByTagName("rect")[0]?.getAttribute("id")).toBe("lower");
  });

  test("throws for markup that is not an svg document", () => {
    for (const markup of [
      "<div>not a diagram</div>",
      "<svg><rect></svg>",
      "<svg><rect/></svg>",
      "not markup at all",
    ]) {
      expect(() => sanitizeMermaidSvg(markup, document)).toThrow("Invalid SVG output");
    }
  });
});
