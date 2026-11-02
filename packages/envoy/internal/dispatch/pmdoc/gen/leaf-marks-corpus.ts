// leaf-marks-corpus.ts - a corpus of marks over inline leaves, of inline HTML declarations and of
// autolinks inside a link label, for the pmdoc differential (differential.sh).
//
//   bun leaf-marks-corpus.ts <out.jsonl>
//
// Writes a fixed, enumerated set of documents as {"id", "md"} lines, deduplicated by their
// markdown and numbered leaf-0 upwards in output order. It takes no count and no seed and picks
// nothing, so two runs write byte-identical files. Every document is one content form put in one
// mark context put in one container, and ends in a single line ending.
//
// The forms come in four families, each written with the mark contexts that decide its reading:
//
//   - A leaf form is an inline node that carries no text of its own - inline HTML, an image, a
//     hard break, a footnote reference - together with a code span and an autolink, which do carry
//     text and so must keep the mark either reader puts on them. Neither reader puts an emphasis,
//     strong, strikethrough or link mark on such a node: a mark lives on text alone, as the
//     browser editor's store holds it. All eleven mark contexts are written for these, since a
//     mark of each kind, two marks nested, and a mark that opens or closes beside the form each
//     reach the node differently.
//   - A declaration form opens with `<!`. Both readers open an inline declaration at `<!` and any
//     ASCII letter, lowercase as well as uppercase; the controls (`<!A>`, `<!DOCTYPE html>`,
//     `<!>`, `<!1>`, `<! a>`, unclosed, escaped, in a code span) must read the same in both. A
//     declaration is one raw HTML node or it is text, so only whether a mark covers it tells the
//     two readings apart: these take the four contexts that settle that, bare, strong, emphasis
//     and a link label.
//   - A label form is a link label holding an autolink. Both readers give the autolink its own
//     link, in place of the label's, on its own text, and leave the label's link on the runs
//     around it. These take bare, strong and strikethrough; a form that is itself a link label
//     wrapped in `[...](u)` is a different shape, so the link context is written only for the
//     image control, which is no link.
//   - An image-label form is an image whose label holds raw HTML: a tag, a lowercase declaration,
//     an uppercase one, a comment, a processing instruction, CDATA and an anchor span with its
//     `</span>`, each with text around it and each alone in the label, with and without a title.
//     The engine's alt text is mdast-util-to-string of the label, which writes a literal node's
//     own value and so keeps the raw HTML; the anchor span is in the alt too, since the engine
//     builds the alt as the label closes and turns spans into marks only afterwards, and Go reads
//     a span in a label as the raw HTML it is for that reason. Only the label decides the alt,
//     which no mark around the image reaches, so these take the bare context alone, in every
//     container.
//
// The containers are a paragraph, a heading, a quote, a bullet item, an ordered item, a table
// cell, a callout, a footnote definition and a quoted bullet item. A form holding a line ending -
// the two-line declaration and the two hard breaks - is written only in the two containers whose
// body runs over lines at column 0, the paragraph and the callout; every other container would
// need a line prefix on the second line or cannot hold a line ending at all. No form holds a `|`,
// so no table cell needs an escape.
import { writeFileSync } from "node:fs";

const [output] = process.argv.slice(2);
if (!output) {
  console.error("usage: bun leaf-marks-corpus.ts <out.jsonl>");
  process.exit(2);
}

// Inline nodes a mark can reach, `[^d]` carrying its definition below and the last two carrying
// text of their own.
const leafForms = [
  "<b>",
  "</b>",
  "<!-- c -->",
  '<a href="u">',
  "<br/>",
  "![x](i.png)",
  '![x](i.png "t")',
  "[^d]",
  "a\\\nb",
  "a  \nb",
  "<https://u.test>",
  "`c`",
] as const;
// A lowercase declaration, the adjacency that decides where it ends, and the controls.
const declarationForms = [
  "<!a>",
  "<!a b>",
  '<!a "x">',
  "<!a>q",
  "<!a>*b*",
  "<!a\nb>",
  "<!A>",
  "<!DOCTYPE html>",
  "<!>",
  "<!1>",
  "<! a>",
  "<!a",
  "\\<!a>",
  "`<!a>`",
] as const;
// A link label holding an autolink, then the controls: a nested link, two labels that only look
// like an address, and the same autolink in an image label.
const labelForms = [
  "[<https://u.test>](u)",
  "[<mailto:a@b.test>](u)",
  "[<a@b.test>](u)",
  "[a <https://u.test> b](u)",
  "[a <https://u.test> b <mailto:c@d.test> e](u)",
  "[foo [bar](/uri)](/uri2)",
  "[www.a.test](u)",
  "[https://u.test](u)",
  "![<https://u.test>](u)",
] as const;
// Raw HTML in an image's label, one form for each kind a label can hold.
const imageLabelHTML = [
  "<b>",
  "<!a>",
  "<!A>",
  "<!-- c -->",
  "<?x?>",
  "<![CDATA[x]]>",
  '<span data-proof="comment" data-id="c1" data-by="user:x">b</span>',
] as const;
const imageLabelForms = imageLabelHTML.flatMap((html) =>
  ["i.png", 'i.png "t"'].flatMap((destination) => [
    `![r ${html} s](${destination})`,
    `![${html}](${destination})`,
  ])
);

const linkContext = "[{}](u)";
const leafContexts = [
  "**{}**",
  "*{}*",
  "~~{}~~",
  linkContext,
  "***{}***",
  "**~~{}~~**",
  "**a {} b**",
  "**a**{}**b**",
  "__{}__",
  "_{}_",
  "{}",
] as const;
const declarationContexts = ["{}", "**{}**", "*{}*", linkContext] as const;
const labelContexts = ["{}", "**{}**", "~~{}~~"] as const;
const imageLabelContexts = ["{}"] as const;

const paragraph = "{}";
const callout = ':::callout{#c1 kind="note" title="T"}\n{}\n:::';
const containers = [
  paragraph,
  "# {}",
  "> {}",
  "- {}",
  "1. {}",
  "| a |\n| --- |\n| {} |",
  callout,
  "Text[^z].\n\n[^z]: {}",
  "> - {}",
] as const;
const lineEndingContainers = [paragraph, callout] as const;

const forms: { md: string; contexts: readonly string[] }[] = [
  ...leafForms.map((md) => ({ md, contexts: leafContexts as readonly string[] })),
  ...declarationForms.map((md) => ({ md, contexts: declarationContexts as readonly string[] })),
  ...labelForms.map((md) => ({
    md,
    // An image label is the one label form that is not itself a link.
    contexts: md.startsWith("![") ? [...labelContexts, linkContext] : labelContexts,
  })),
  ...imageLabelForms.map((md) => ({ md, contexts: imageLabelContexts as readonly string[] })),
];

const seen = new Set<string>();
const lines: string[] = [];
for (const { md, contexts } of forms) {
  for (const context of contexts) {
    const text = context.replace("{}", () => md);
    for (const container of text.includes("\n") ? lineEndingContainers : containers) {
      const definition = text.includes("[^d]") ? "\n\n[^d]: note" : "";
      const markdown = `${container.replace("{}", () => text)}${definition}\n`;
      if (seen.has(markdown)) continue;
      seen.add(markdown);
      lines.push(JSON.stringify({ id: `leaf-${lines.length}`, md: markdown }));
    }
  }
}
writeFileSync(output, `${lines.join("\n")}\n`);
