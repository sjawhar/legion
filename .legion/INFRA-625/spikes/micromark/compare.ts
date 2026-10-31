// Differential test: original vs patched micromark / micromark-core-commonmark /
// mdast-util-from-markdown over spec examples, the pmdoc corpus and a fuzz set.
//
// Every import here is dynamic on purpose: this script runs from outside the workspace and has
// no node_modules of its own, and it loads the same three packages twice, from two roots, so
// each specifier is built at runtime.
const W = "/legion/workspaces/sjawhar/legion/infra-625";
const S = "/legion/scratch/tmp/infra625/spikes/MicromarkSuperlinear";
const roots = { orig: `${W}/node_modules/.bun`, patched: `${S}/overlay` };
// The packages ship a second copy under `dev/`, which a resolver takes on the `development`
// condition (Vite's dev server does). `DEV=1` with `bun --conditions development` checks it.
const dev = process.env.DEV === "1" ? "dev/" : "";

async function load(root: string) {
  const mm = await import(`${root}/micromark@4.0.2/node_modules/micromark/${dev}index.js`);
  const { fromMarkdown } = await import(
    `${root}/mdast-util-from-markdown@2.0.3/node_modules/mdast-util-from-markdown/${dev}index.js`
  );
  const { gfm, gfmHtml } = await import(`${root}/micromark-extension-gfm@3.0.0/node_modules/micromark-extension-gfm/index.js`);
  return { mm, fromMarkdown, gfm: gfm(), gfmHtml: gfmHtml() };
}
const A = await load(roots.orig);
const B = await load(roots.patched);


const { gfmFromMarkdown } = await import(`${W}/node_modules/.bun/mdast-util-gfm@3.1.0/node_modules/mdast-util-gfm/index.js`);
const { directiveContainerSyntax } = await import(`${W}/packages/proof-editor/src/directive-container.ts`);

const gfmMd = gfmFromMarkdown();
const dirExt = directiveContainerSyntax;

type Case = { html: (b: string) => string; tree: (b: string) => string };
const pack = (m: typeof A, ext: unknown[], md: unknown[], html: unknown[]): Case => ({
  html: (b) => m.mm.micromark(b, { extensions: ext, htmlExtensions: html, allowDangerousHtml: true }),
  tree: (b) => JSON.stringify(m.fromMarkdown(b, { extensions: ext, mdastExtensions: md })),
});
const variants: Array<[string, Case, Case]> = [
  ["plain", pack(A, [], [], []), pack(B, [], [], [])],
  ["gfm", pack(A, [A.gfm], [gfmMd], [A.gfmHtml]), pack(B, [B.gfm], [gfmMd], [B.gfmHtml])],
  ["gfm+directive", pack(A, [A.gfm, dirExt], [gfmMd], [A.gfmHtml]), pack(B, [B.gfm, dirExt], [gfmMd], [B.gfmHtml])],
];

let checked = 0;
let diffs = 0;
const shown: string[] = [];
function compare(label: string, body: string) {
  for (const [name, a, b] of variants) {
    for (const key of ["html", "tree"] as const) {
      let x: string;
      let y: string;
      try { x = a[key](body); } catch (error) { x = "ERR " + String(error); }
      try { y = b[key](body); } catch (error) { y = "ERR " + String(error); }
      checked++;
      if (x !== y) {
        diffs++;
        if (shown.length < 10) shown.push(`${label} [${name}/${key}]\n  in:  ${JSON.stringify(body.slice(0, 200))}\n  old: ${JSON.stringify(x.slice(0, 200))}\n  new: ${JSON.stringify(y.slice(0, 200))}`);
      }
    }
  }
}

// 1. CommonMark spec 0.31.2.
const spec = JSON.parse(await Bun.file(`${S}/spec.json`).text()) as Array<{ markdown: string; example: number }>;
for (const example of spec) compare(`spec#${example.example}`, example.markdown);
const afterSpec = { checked, diffs };

// 2. pmdoc corpus + fixtures.
const corpusDir = `${W}/packages/envoy/internal/dispatch/pmdoc/testdata/corpus`;
const { readdirSync } = await import("node:fs");
const corpus = readdirSync(corpusDir).filter((f) => f.endsWith(".md"));
for (const file of corpus) compare(`corpus/${file}`, await Bun.file(`${corpusDir}/${file}`).text());
const fixtures = JSON.parse(
  await Bun.file(`${W}/packages/envoy/internal/dispatch/pmdoc/testdata/fixtures.json`).text()
) as Array<{ name: string; markdown: string }>;
for (const fixture of fixtures) compare(`fixture/${fixture.name}`, fixture.markdown);
const afterCorpus = { checked, diffs };

// 3. Fuzz.
const pieces = [
  "> ", "- ", "1. ", "  ", "    ", "\n", "x\n", "lazy text\n", "[", "]", "[x]: /u\n", "![", "](",
  "```\n", ":::callout{}\n", ":::\n", "> lazy\n", "- [x] t\n", "|a|b|\n", "~~s~~", "www.a.co ",
  "a", " ", "\t", "](u)", "[x]", "&", "<b>", "*", "_", "`", "#", "=", "---\n", "3) ", "+ ",
  "[^1]", "[^1]: note\n", "![^1]", "^", ":::callout[", "]{#a}\n", "- a\n", "> a\n",
];
function rng(seed: number) {
  let s = seed >>> 0;
  return () => {
    s = (s * 1664525 + 1013904223) >>> 0;
    return s / 4294967296;
  };
}
const count = Number(process.env.FUZZ ?? 20000);
const big = Number(process.env.FUZZ_BIG ?? 3000);
for (let i = 0; i < count + big; i++) {
  const random = rng(i + 1);
  // Small documents first, then ones long enough to grow a paragraph, a list and a label run.
  const n = i < count ? 1 + Math.floor(random() * 24) : 40 + Math.floor(random() * 360);
  let body = "";
  for (let k = 0; k < n; k++) body += pieces[Math.floor(random() * pieces.length)];
  compare(`fuzz#${i}`, body);
}

console.log(`spec:    ${spec.length} inputs, ${afterSpec.checked} comparisons, ${afterSpec.diffs} differences`);
console.log(`corpus:  ${corpus.length + fixtures.length} inputs, ${afterCorpus.checked - afterSpec.checked} comparisons, ${afterCorpus.diffs - afterSpec.diffs} differences`);
console.log(`fuzz:    ${count + big} inputs, ${checked - afterCorpus.checked} comparisons, ${diffs - afterCorpus.diffs} differences`);
console.log(`total:   ${checked} comparisons, ${diffs} differences`);
for (const line of shown) console.log("----\n" + line);
