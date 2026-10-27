// fuzz.ts - a seeded corpus of image titles over many lines for the pmdoc differential
// (differential.sh), which always reads it.
//
//   bun fuzz.ts [--seed N] [--count N]
//
// Prints {"id", "md"} JSONL documents, each holding an image whose title, or whose link's title
// (`[![alt](src "title")](href "link title")`), runs over several lines, the shape whose line feeds
// the writer keeps raw only where every line the title creates opens as text in both Go's reader
// and the browser editor's engine (rawTitleLinesHold). A line it gets wrong is a lost link or a
// churned write in one container only, which the in-repo corpora did not hold (INFRA-190), so the
// corpus is built to reach them:
//
//   - systematically, every line start below - each block start of CommonMark, GFM and the editor's
//     extensions, the near-misses around it, every HTML block kind's start and end, and text - as a
//     title's second line, in half the containers each, cycling through the four title fields (an
//     image's title, a linked image's title, its link's title, both), a block start after a no-break
//     or an ideographic space in two rotating containers; then each start again with the title's
//     line feeds written raw, as the base writes them, in a paragraph and a rotating lazy container;
//   - then --count (default 1300) documents mixing one to four line starts per title, with text
//     before the image, a second image, a table before or after the paragraph and emphasis, in a
//     random container and spelling, from a PRNG seeded with --seed (default 20261026).
//
// A title's line feeds are written `&#10;`, so the source reads the title as held, and in a share
// of documents raw, lazily or under the container's prefix. The output is the same for the same
// arguments, byte for byte.

function argument(name: string, fallback: number): number {
  const index = process.argv.indexOf(name);
  if (index < 0) return fallback;
  const value = Number(process.argv[index + 1]);
  if (!Number.isInteger(value) || value < 0) {
    console.error(`fuzz.ts: ${name} takes a non-negative integer`);
    process.exit(2);
  }
  return value;
}
const seed = argument("--seed", 20261026);
const count = argument("--count", 1300);

// mulberry32: a small PRNG, so the corpus depends on nothing but the seed.
let state = seed >>> 0;
function random(): number {
  state = (state + 0x6d2b79f5) >>> 0;
  let t = state;
  t = Math.imul(t ^ (t >>> 15), t | 1);
  t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
  return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
}
const pick = <T>(values: readonly T[]): T => values[Math.floor(random() * values.length)] as T;
const chance = (p: number): boolean => random() < p;

// Line starts that open a block in some reader, container or position, each also written after a
// leading no-break space and an ideographic space, which some readers take for white space.
const blockStarts = [
  // ATX headings and near-misses.
  "#",
  "# b",
  "#b",
  "## ",
  "###### b",
  "####### b",
  "#\tb",
  // Setext underlines.
  "=",
  "===",
  "= b",
  "-",
  "--",
  "---",
  "- -",
  // Thematic breaks.
  "***",
  "* * *",
  "- - -",
  "___",
  "_ _ _",
  "**",
  "__",
  "***b",
  // Fences.
  "```",
  "```js",
  "``` b",
  "~~~",
  "~~~ x",
  "``",
  "~~",
  "````",
  "``` a`b",
  // Bullet items.
  "- b",
  "+ b",
  "* b",
  "+",
  "*",
  "- ",
  "-b",
  "+b",
  "*b",
  "-\tb",
  // Ordered items.
  "1. b",
  "1) b",
  "2. b",
  "10) b",
  "01. b",
  "0. b",
  "123456789. b",
  "1234567890. b",
  "1.",
  "1)",
  "2.",
  "10)",
  "1. ",
  "1.b",
  "1)b",
  // Task items.
  "[ ] b",
  "[x] b",
  "- [ ] b",
  "- [x] b",
  "1. [ ] b",
  // Quotes.
  ">",
  "> b",
  ">b",
  ">> b",
  // Table rows and delimiter rows.
  "| a |",
  "| a | b |",
  "a | b",
  "| - |",
  "| --- | --- |",
  "| - | - | - |",
  "-|-",
  "|-",
  ":-",
  "-:",
  ":-:",
  "| :- |",
  "| -: |",
  "- | -",
  "--- | ---",
  "| |",
  "|",
  // Link reference and footnote definitions.
  "[l]: /u",
  '[l]: /u "t"',
  "[l]:",
  "[l]",
  "[^1]: x",
  "[^1]:",
  "[^a b]: x",
  "[^1]x",
  // Directives and math.
  ":::",
  ":::name{}",
  ':::callout{kind="note"}',
  ":::callout",
  "::: x",
  "::name",
  "::name{}",
  ":name{x}",
  ":name",
  "::",
  ":",
  "$$",
  "$$ x",
  "$x$",
];
// HTML block starts and ends, of every kind, and the near-misses around them.
const htmlStarts = [
  "<script>",
  "</script>",
  "</SCRIPT>",
  "<script",
  "<SCRIPT>",
  "<pre",
  "<pre>",
  "</pre>",
  "<style>",
  "</style>",
  "<textarea>",
  "</textarea>",
  "</textarea >",
  "</script> b",
  "</pre >",
  "<!-- c",
  "<!--",
  "<!---->",
  "-->",
  "<?x",
  "?>",
  "<!X y",
  "<!x>",
  "<!doctype html>",
  "<![CDATA[",
  "<![CDATA[x]]>",
  "]]>",
  "<div",
  "<div>",
  "</div>",
  '<DIV class="x">',
  "<div/>",
  "<div >",
  "<div\tx>",
  "<p>",
  "</p>",
  "<table>",
  "</table>",
  "<details>",
  "<ul>",
  "<span>",
  "</span>",
  "<b>",
  "</b>",
  "<b >",
  "<b/>",
  '<x-y z="1"/>',
  '<a href="u">',
  "<a href=u>",
  "<b c>",
  '<b\tc="d">',
  "<b>c",
  "</b> c",
  "<span> ",
  // Near-misses.
  "< b",
  "<1>",
  "<=",
  "<",
  "</",
  "<!",
  "<?",
  "<x",
  "</ b>",
  "<b c=>",
  "<b c='d>",
  "<http://x.test>",
  "<a@b.test>",
  "<divx>",
  "<div-x>",
  "<div_x>",
  "<diva b>",
  "<prex>",
  "<pre-x>",
  "<scripts>",
  "<scriptx",
  "<style2>",
  "<textareax>",
  "</divx>",
  "</prex>",
  "<di",
];
// Lines no block opens at, or that only white space before them can open.
const textStarts = [
  "b",
  "B",
  "é",
  "日本",
  "1",
  "12 b",
  "1b",
  '"q"',
  "'q'",
  "&",
  "&amp; b",
  "&#35; b",
  "\\",
  "\\# b",
  "\\- b",
  "$",
  "$ b",
  "{",
  "{x}",
  "%",
  "% b",
  ".",
  ",",
  ";",
  "!",
  "!b",
  "?",
  "@",
  "(",
  ")",
  "]",
  "[",
  "[b",
  "^",
  "`",
  "`c`",
  "~",
  "~b",
  "_b",
  "=b",
  "+-",
  "*-",
  "*e*",
  "/",
  "'",
  "",
  " ",
  "\t",
  " b",
  "  b",
  "   b",
  "    b",
  "\tb",
  " - b",
  "   - b",
  "    - b",
  " # b",
  "   > b",
  "  <div>",
  "   ```",
  " | - |",
  "  1. b",
  " :::",
  "   </script>",
];
const plainStarts = [...blockStarts, ...htmlStarts, ...textStarts];
const spacedStarts = [
  ...blockStarts,
  "<script>",
  "</script>",
  "<div>",
  "<!-- c",
  "<b>",
  "</pre>",
].flatMap((start) => [`\u00a0${start}`, `\u3000${start}`]);
const lineStarts = [...plainStarts, ...spacedStarts];

// A container holds the paragraph: its first line opens with open, a line after it under cont,
// and close follows it. lazy says a raw line feed can leave its lines unprefixed.
interface Container {
  name: string;
  open: string;
  cont: string;
  close: string;
  lazy: boolean;
}
const callout = ':::callout{#c1 kind="note" title="T"}\n';
// Alternately top-level and lazy, so the even and the odd half each hold both.
const containers: readonly Container[] = [
  { name: "paragraph", open: "", cont: "", close: "", lazy: false },
  { name: "bullet", open: "- ", cont: "  ", close: "", lazy: true },
  { name: "typed", open: callout, cont: "", close: "\n:::", lazy: false },
  { name: "ordered", open: "1. ", cont: "   ", close: "", lazy: true },
  { name: "after-paragraph", open: "p\n\n", cont: "", close: "", lazy: false },
  { name: "quote", open: "> ", cont: "> ", close: "", lazy: true },
  { name: "ordered-10", open: "10. ", cont: "    ", close: "", lazy: true },
  { name: "footnote", open: "x[^1]\n\n[^1]: ", cont: "    ", close: "", lazy: true },
  { name: "task", open: "- [ ] ", cont: "  ", close: "", lazy: true },
  { name: "nested", open: "- a\n  - ", cont: "    ", close: "", lazy: true },
  { name: "quote-in-list", open: "- > ", cont: "  > ", close: "", lazy: true },
  { name: "list-in-quote", open: "> - ", cont: ">   ", close: "", lazy: true },
  { name: "typed-list", open: `${callout}- `, cont: "  ", close: "\n:::", lazy: true },
  { name: "ordered-paren", open: "1) ", cont: "   ", close: "", lazy: true },
];
const lazyContainers = containers.filter((container) => container.lazy);
const topLevelContainers = containers.filter((container) => !container.lazy);

type Field = "title" | "linked-title" | "link-title" | "both";
const fields: readonly Field[] = ["title", "linked-title", "link-title", "both"];

// quoted writes a title value as a markdown title, its line feeds as `&#10;` unless raw.
function quoted(value: string, raw: boolean): string {
  const escaped = value.replaceAll("\\", "\\\\").replaceAll('"', '\\"');
  return `"${raw ? escaped : escaped.replaceAll("\n", "&#10;")}"`;
}

function image(field: Field, title: string, raw: boolean, alt = "alt", src = "/i.png"): string {
  const linkTitle = field === "both" ? `${title}\nl` : title;
  switch (field) {
    case "title":
      return `![${alt}](${src} ${quoted(title, raw)})`;
    case "linked-title":
      return `[![${alt}](${src} ${quoted(title, raw)})](https://h.test/p)`;
    case "link-title":
      return `[![${alt}](${src})](https://h.test/p ${quoted(title, raw)})`;
    case "both":
      return `[![${alt}](${src} ${quoted(title, raw)})](https://h.test/p ${quoted(linkTitle, raw)})`;
  }
}

// document places a paragraph's text in a container; raw line feeds in it are prefixed with the
// container's continuation unless lazy.
function document(container: Container, text: string, lazy: boolean): string {
  const body = lazy ? text : text.replaceAll("\n", `\n${container.cont}`);
  return `${container.open}${body}${container.close}\n`;
}

const seen = new Set<string>();
const lines: string[] = [];
function emit(markdown: string): void {
  if (seen.has(markdown)) return;
  seen.add(markdown);
  lines.push(JSON.stringify({ id: String(lines.length), md: markdown }));
}

// Each line start as a title's second line, in half the containers, the fields cycling; one opening
// with a no-break or ideographic space in a top-level and a lazy container, both rotating.
let cycle = 0;
const nextField = () => fields[cycle++ % fields.length] as Field;
plainStarts.forEach((start, startIndex) => {
  containers.forEach((container, containerIndex) => {
    if ((startIndex + containerIndex) % 2 !== 0) return;
    emit(document(container, image(nextField(), `t\n${start}`, false), true));
  });
});
spacedStarts.forEach((start, startIndex) => {
  for (const group of [topLevelContainers, lazyContainers]) {
    const container = group[startIndex % group.length] as Container;
    emit(document(container, image(nextField(), `t\n${start}`, false), true));
  }
});
// Each line start again with the title's line feeds raw, the base's own spelling: in a paragraph
// after text and in a rotating lazy container, unprefixed.
plainStarts.forEach((start, startIndex) => {
  emit(document(containers[0] as Container, `a ${image(nextField(), `t\n${start}`, true)}`, true));
  const container = lazyContainers[startIndex % lazyContainers.length] as Container;
  emit(document(container, image(nextField(), `t\n${start}`, true), true));
});

// Every tag name either reader's HTML block rules name - CommonMark's block tags (micromark's
// htmlBlockNames), goldmark's allowedBlockTags, which add `meta`, the raw elements and a few inline
// tags - in each form the two readers read differently: case, a closing tag with spaces after `</`,
// self-closing, white space after the name (a tab, a form feed, which Go's `\s` holds and
// CommonMark's does not), attributes and text after the tag. Each form once with `&#10;` in a
// rotating container and once raw in a rotating lazy container, the fields cycling.
const tagNames = [
  "address",
  "article",
  "aside",
  "base",
  "basefont",
  "blockquote",
  "body",
  "caption",
  "center",
  "col",
  "colgroup",
  "dd",
  "details",
  "dialog",
  "dir",
  "div",
  "dl",
  "dt",
  "fieldset",
  "figcaption",
  "figure",
  "footer",
  "form",
  "frame",
  "frameset",
  "h1",
  "h2",
  "h3",
  "h4",
  "h5",
  "h6",
  "head",
  "header",
  "hr",
  "html",
  "iframe",
  "legend",
  "li",
  "link",
  "main",
  "menu",
  "menuitem",
  "meta",
  "nav",
  "noframes",
  "ol",
  "optgroup",
  "option",
  "p",
  "param",
  "search",
  "section",
  "summary",
  "table",
  "tbody",
  "td",
  "tfoot",
  "th",
  "thead",
  "title",
  "tr",
  "track",
  "ul",
  "pre",
  "script",
  "style",
  "textarea",
  "b",
  "span",
  "x-y",
  "a",
];
const tagForms = (name: string): string[] => {
  const upper = name.toUpperCase();
  return [
    `<${name}`,
    `<${upper}>`,
    `</${name}>`,
    `</ ${name}>`,
    `</  ${upper}`,
    `<${name}/>`,
    `<${name} />`,
    `<${name}/>x`,
    `<${name}\tx>`,
    `<${name}\fx`,
    `<${name} x="1">`,
    `<${name}>c`,
  ];
};
tagNames.flatMap(tagForms).forEach((start, startIndex) => {
  const container = containers[startIndex % containers.length] as Container;
  emit(document(container, image(nextField(), `t\n${start}\nz`, false), true));
  const lazy = lazyContainers[startIndex % lazyContainers.length] as Container;
  emit(document(lazy, image(nextField(), `t\n${start}\nz`, true), true));
});

// A title's delimiter row under a paragraph line over 4 KiB, its header, with one to three cells,
// so the row is no wider than the header or is, each title field, raw and `&#10;`.
const longText = "x".repeat(5000);
for (const header of ["", "p | ", "p | q | "]) {
  for (const delimiter of ["|-|", "-|-", "| - | - |", ":-|-:"]) {
    for (const raw of [false, true]) {
      emit(
        document(
          containers[0] as Container,
          `${header}${longText} ${image(nextField(), `a\n${delimiter}\nb`, raw)}`,
          true
        )
      );
    }
  }
}

// Seeded mixing: one to four lines per title, each line a start and a tail.
const firsts = ["t", "a b", "x", "T", "", "a |", "*e*", "1."];
const tails = ["", "", " b", "x", " | c |", " *e*", "-", " <b>", "&#10;"];
const befores = ["", "", "", "a | b ", "text ", "*em* ", "| a | ", "**b** ", "1. ", "# "];
const afters = ["", "", " b", " | c", " ![y](/j.png)", "  ", "\\"];
const table = "| a | b |\n| - | - |\n| c | d |";
function randomTitle(): string {
  const parts = [pick(firsts)];
  const more = Math.floor(random() * 4);
  for (let index = 0; index < more; index++) {
    parts.push(pick(lineStarts) + (chance(0.4) ? pick(tails) : ""));
  }
  return parts.join("\n");
}
const target = lines.length + count;
for (let attempt = 0; lines.length < target && attempt < count * 10; attempt++) {
  const container = pick(containers);
  const raw = chance(0.35);
  let text = image(pick(fields), randomTitle(), raw);
  if (chance(0.1)) text = `*${text}*`;
  text = pick(befores) + text;
  if (chance(0.2)) text += ` ${image(pick(fields), randomTitle(), raw, "two", "/k.png")}`;
  else text += pick(afters);
  let markdown = document(container, text, !raw || chance(0.5));
  if (chance(0.15)) markdown += `\n${table}\n`;
  if (chance(0.15)) markdown = `${table}\n\n${markdown}`;
  emit(markdown);
}

process.stdout.write(`${lines.join("\n")}\n`);
