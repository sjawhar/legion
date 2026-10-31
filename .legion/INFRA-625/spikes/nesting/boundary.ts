// Where Go stops accepting vs where the guard stops opening, over the chains, one level at a time.
const WS = "/legion/workspaces/sjawhar/legion/infra-625";
const B = `${WS}/node_modules/.bun`;
// Dynamic import: the unguarded pass must run before the guard module is loaded.
const { fromMarkdown } = await import(
  `${B}/mdast-util-from-markdown@2.0.3/node_modules/mdast-util-from-markdown/index.js`
);
const fs = await import("node:fs");
const path = await import("node:path");

const shapes: Record<string, (d: number) => string> = {
  dash: (d) => "- ".repeat(d) + "x\n",
  ord: (d) => "1. ".repeat(d) + "x\n",
  quote: (d) => "> ".repeat(d) + "x\n",
  mix: (d) => "> - ".repeat(d) + "x\n",
  qlist: (d) => "> ".repeat(d) + "- x\n",
  ind: (d) => Array.from({ length: d }, (_, i) => "  ".repeat(i) + "- a\n").join(""),
  qind: (d) => Array.from({ length: d }, (_, i) => "> ".repeat(i + 1) + "a\n").join(""),
  // a list opened inside quotes, then one more item: the later item takes no level
  qlist2: (d) => "> ".repeat(d) + "- x\n" + "> ".repeat(d) + "- y\n",
};
const docs: { name: string; body: string }[] = [];
for (const [shape, make] of Object.entries(shapes)) {
  const top = shape === "quote" || shape === "qind" ? 110 : shape === "mix" ? 45 : 60;
  for (let d = 1; d <= top; d++) docs.push({ name: `${shape}-${String(d).padStart(3, "0")}`, body: make(d) });
}

const before = docs.map((d) => JSON.stringify(fromMarkdown(d.body)));
const { installGuard } = await import("./nesting-guard.ts");
installGuard();

const dumpDir = process.env.DUMP ?? "boundary-docs";
fs.mkdirSync(dumpDir, { recursive: true });
const rows: string[] = [];
for (const [index, doc] of docs.entries()) {
  fs.writeFileSync(path.join(dumpDir, `${doc.name}.md`), doc.body);
  rows.push(`${doc.name}\t${JSON.stringify(fromMarkdown(doc.body)) === before[index] ? "same" : "changed"}`);
}
fs.writeFileSync(path.join(dumpDir, "changed.tsv"), rows.join("\n") + "\n");
console.log(`docs=${docs.length}`);
