import { afterAll, describe, expect, test } from "bun:test";
import { Window } from "happy-dom";
import { renderMermaidMarkup } from "../src/mermaid-render";
import { mermaidSvgElement, sanitizeMermaidSvg, showMermaidSvg } from "../src/mermaid-svg";
import { MERMAID_EVENTMODELING_DRAWING } from "./fixtures/mermaid-eventmodeling";
import { MERMAID_JOURNEY_DRAWING } from "./fixtures/mermaid-journey";

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
    const svg = mermaidSvgElement(markup, document, "xml");

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
      document,
      "xml"
    );

    const values = attributeValues(svg);
    expect(values.filter((value) => value.includes("evil.example"))).toEqual([]);
    expect(values.some((value) => value.includes("url(#"))).toBe(true);
  });

  test("throws for markup that is not an svg document", () => {
    expect(() => mermaidSvgElement("<div>not a diagram</div>", document, "xml")).toThrow(
      "Invalid SVG output"
    );
  });
});

describe("showMermaidSvg", () => {
  test("keeps one shadow root holding the sheet and the latest diagram only", () => {
    const host = document.createElement("div") as unknown as HTMLElement;
    const attachShadow = host.attachShadow.bind(host);
    let attached = 0;
    host.attachShadow = (init) => {
      attached++;
      return attachShadow(init);
    };
    const diagram = (id: string) =>
      mermaidSvgElement(
        `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect id="${id}" width="4" height="4"/></svg>`,
        document,
        "xml"
      );
    const sheet = "svg{display:block}";
    const second = diagram("second");

    showMermaidSvg(host, diagram("first"), sheet);
    showMermaidSvg(host, second, sheet);

    expect(attached).toBe(1);
    const root = host.shadowRoot;
    expect(root?.mode).toBe("open");
    const children = Array.from(root?.children ?? []);
    expect(children.map((child) => child.localName)).toEqual(["style", "svg"]);
    expect(children[0]?.textContent).toBe(sheet);
    expect(children[1]?.querySelector("rect")?.id).toBe("second");
    expect(children[1]).not.toBe(second);
    expect(host.children).toHaveLength(0);
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
      const svg = sanitizeMermaidSvg(markup, document, "xml");
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
    const svg = sanitizeMermaidSvg(hostile, document, "xml");

    expect(svg.ownerDocument).toBe(document);
    expect(elementNames(svg)).toEqual([
      "svg",
      "style",
      "foreignObject",
      "div",
      "rect",
      "path",
      "text",
    ]);
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
      document,
      "xml"
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
      sanitizeMermaidSvg('<SVG xmlns="http://www.w3.org/2000/svg"><rect/></SVG>', document, "xml")
    ).toThrow("Invalid SVG output");

    const svg = sanitizeMermaidSvg(
      `<svg xmlns="http://www.w3.org/2000/svg">
        <RECT id="upper" width="1" height="1"/>
        <rect id="lower" width="1" height="1"/>
      </svg>`,
      document,
      "xml"
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
      expect(() => sanitizeMermaidSvg(markup, document, "xml")).toThrow("Invalid SVG output");
    }
  });
});

/** The namespace a browser lays a `foreignObject`'s content out in, whatever namespace the parse
 *  that read the markup assigned it. */
const XHTML_NAMESPACE = "http://www.w3.org/1999/xhtml";

/** The first `<foreignObject>` of a drawing, which is where journey and eventmodeling put their
 *  label. Matched by local name: `getElementsByTagName` on an HTML document folds the name it is
 *  given to upper case, which no SVG element carries. */
function labelOf(svg: Element): Element {
  const label = allElements(svg).find((element) => element.localName === "foreignObject");
  if (label === undefined) throw new Error("the drawing holds no foreignObject");
  return label;
}

/** Markup no renderer writes, put into a label by hand: a link, an image and a handler. A label
 *  is the one place an author's own markup reaches the drawing unescaped, which is what makes
 *  the `foreignObject` allowlist load-bearing (`mermaid-svg.ts`). */
const HAND_WRITTEN_LABEL_MARKUP =
  '<a href="https://evil.example/go"><b>link</b></a>' +
  '<img src="https://evil.example/pixel.png" width="1" height="1"/>' +
  '<span onclick="globalThis.hit = 1" class="added">clickable</span>';

// Upstream Mermaid's own markup, which the page reads with the HTML parser (`mermaid-diagrams.ts`
// picks the parse from the renderer the worker names). Each drawing is a recording of mermaid
// 12.3.0 under the worker's configuration, with its `<style>` element taken out, because
// happy-dom's HTML parser loses an svg `<style>`'s text and every element after it; each
// fixture's header says how it was recorded (`tests/fixtures/`).
describe("sanitizeMermaidSvg, over upstream Mermaid's drawings", () => {
  test("keeps a journey's label: its text and the class and style of each node of it", () => {
    const svg = sanitizeMermaidSvg(MERMAID_JOURNEY_DRAWING, document, "html");

    const label = labelOf(svg);
    expect(label.namespaceURI).toBe("http://www.w3.org/2000/svg");
    const kept = allElements(label).slice(1);
    expect(kept.map((element) => [element.localName, element.namespaceURI])).toEqual([
      ["div", XHTML_NAMESPACE],
      ["div", XHTML_NAMESPACE],
    ]);
    expect(kept[0]?.getAttribute("class")).toBe("journey-section section-type-0");
    expect(kept[0]?.getAttribute("style")).toBe("display:table;height:100%;width:100%");
    expect(kept[1]?.getAttribute("class")).toBe("label");
    expect(label.textContent).toBe("Draw");
    // The drawing's other label, and the `<switch>` each one sits in, are kept with it.
    expect(svg.textContent).toContain("Render");
    expect(elementNames(svg)).toContain("switch");
  });

  test("keeps an eventmodeling label whole: its b, its two line breaks and its code", () => {
    const svg = sanitizeMermaidSvg(MERMAID_EVENTMODELING_DRAWING, document, "html");

    const label = labelOf(svg);
    const kept = allElements(label).slice(1);
    expect(kept.map((element) => [element.localName, element.namespaceURI])).toEqual([
      ["div", XHTML_NAMESPACE],
      ["span", XHTML_NAMESPACE],
      ["b", XHTML_NAMESPACE],
      ["br", XHTML_NAMESPACE],
      ["br", XHTML_NAMESPACE],
      ["code", XHTML_NAMESPACE],
    ]);
    expect(kept[1]?.getAttribute("style")).toBe(
      "display:table-cell;text-align:center;vertical-align:middle"
    );
    expect(kept[5]?.getAttribute("style")).toBe("text-align: left; display: block;max-width:430px");
    // The frame's name, then its inline data, whose spaces mermaid writes as no-break spaces.
    expect(label.textContent).toBe("Dispatchitem:\u00a0apple,\u00a0count:");
  });

  test("removes an image, a link and a handler an author wrote into a label", () => {
    for (const drawing of [MERMAID_JOURNEY_DRAWING, MERMAID_EVENTMODELING_DRAWING]) {
      const svg = sanitizeMermaidSvg(
        drawing.replace("</foreignObject>", `${HAND_WRITTEN_LABEL_MARKUP}</foreignObject>`),
        document,
        "html"
      );

      const label = labelOf(svg);
      expect(elementNames(label)).not.toContain("a");
      expect(elementNames(label)).not.toContain("img");
      expect(label.textContent).not.toContain("link");
      expect(attributeValues(svg).filter((value) => value.includes("evil.example"))).toEqual([]);
      // The `<span>` the handler sat on is a label's own element, so it stays, without it: the
      // allowlist removes what an author added, not everything beside Mermaid's own markup.
      const added = allElements(label).find((element) => element.getAttribute("class") === "added");
      expect(added?.localName).toBe("span");
      expect(added?.getAttribute("onclick")).toBeNull();
      expect(label.textContent).toContain("clickable");
    }
  });

  // mermaid rewrites every void `<br>` of a label to `<br/>` (`cleanUpSvgCode`) and hands the
  // markup to DOMPurify, whose serializer writes it back as the void `<br>` in a browser and
  // leaves it self-closed under linkedom, the DOM the worker's shim builds and the fixture was
  // recorded against. Measured in Google Chrome for Testing 158.0.8117.2 against mermaid 12.3.0:
  // `<b>Dispatch</b><br><br><code ...>`. So this row writes the browser's spelling back, which
  // is the whole reason Mermaid's markup is read with the HTML parser.
  test("a label's void br survives the html parse, which the xml parse refuses the drawing for", () => {
    const asABrowserWritesIt = MERMAID_EVENTMODELING_DRAWING.replaceAll("<br/>", "<br>");

    const label = labelOf(sanitizeMermaidSvg(asABrowserWritesIt, document, "html"));
    expect(elementNames(label).filter((name) => name === "br")).toHaveLength(2);
    expect(label.textContent).toBe("Dispatchitem:\u00a0apple,\u00a0count:");

    expect(() => sanitizeMermaidSvg(asABrowserWritesIt, document, "xml")).toThrow(
      "Invalid SVG output"
    );
  });
});
