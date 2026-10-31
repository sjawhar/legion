// mdast (and read-only ProseMirror) output, before vs after the guard, in one process: every
// document is parsed unguarded first, then `installGuard()` runs and they are parsed again.
const WS = "/legion/workspaces/sjawhar/legion/infra-625";
const B = `${WS}/node_modules/.bun`;
// Dynamic import: the guard must be loaded *after* the unguarded pass has run.
const { fromMarkdown } = await import(
  `${B}/mdast-util-from-markdown@2.0.3/node_modules/mdast-util-from-markdown/index.js`
);
const fs = await import("node:fs");
const path = await import("node:path");

const corpusDir = `${WS}/packages/envoy/internal/dispatch/pmdoc/testdata/corpus`;
const docs: { name: string; body: string }[] = fs
  .readdirSync(corpusDir)
  .filter((f: string) => f.endsWith(".md"))
  .map((f: string) => ({ name: f, body: fs.readFileSync(path.join(corpusDir, f), "utf8") }));

// Random documents whose block nesting stays under the bound, plus the shapes that sit right at
// it, so the comparison covers the lines the guard actually looks at.
const rng = (seed: number) => () => ((seed = (seed * 1103515245 + 12345) & 0x7fffffff) / 0x7fffffff);
const pieces = ["x y z", "**b** `c`", "[l](u)", "| a | b |\n| - | - |\n| 1 | 2 |", "```\nc\n```", "# h", "---", "[^f]: note", "x[^f]"];
function randomDoc(seed: number, maxDepth: number): string {
  const r = rng(seed);
  const lines: string[] = [];
  let depth = 0;
  const prefix: string[] = [];
  for (let i = 0; i < 60; i++) {
    const roll = r();
    if (roll < 0.3 && depth < maxDepth) {
      const marker = r() < 0.5 ? "- " : r() < 0.5 ? "> " : "1. ";
      prefix.push(marker);
      depth += marker === "> " ? 1 : 2;
    } else if (roll < 0.45 && prefix.length > 0) {
      const marker = prefix.pop()!;
      depth -= marker === "> " ? 1 : 2;
    }
    const indent = prefix.map((m) => (m === "> " ? "> " : " ".repeat(m.length))).join("");
    const open = prefix.join("");
    const body = pieces[Math.floor(r() * pieces.length)];
    lines.push(
      body
        .split("\n")
        .map((l, k) => (k === 0 ? open : indent) + l)
        .join("\n")
    );
    if (r() < 0.3) lines.push(indent.trimEnd());
  }
  return lines.join("\n") + "\n";
}
for (let seed = 1; seed <= 300; seed++) {
  docs.push({ name: `random-d8-${seed}`, body: randomDoc(seed, 8) });
  docs.push({ name: `random-d40-${seed}`, body: randomDoc(seed + 1000, 40) });
  docs.push({ name: `random-d98-${seed}`, body: randomDoc(seed + 2000, 98) });
}
// Chains that end exactly at the bound and one level below it.
for (const d of [1, 2, 10, 49, 50, 98, 99, 100]) {
  docs.push({ name: `dash-${d}`, body: "- ".repeat(d) + "x\n" });
  docs.push({ name: `quote-${d}`, body: "> ".repeat(d) + "x\n" });
  docs.push({ name: `mix-${d}`, body: "> - ".repeat(d) + "x\n" });
  docs.push({
    name: `ind-${d}`,
    body: Array.from({ length: d }, (_, i) => "  ".repeat(i) + "- a\n").join("") + "\ntail\n",
  });
}

const before = docs.map((d) => {
  try {
    return JSON.stringify(fromMarkdown(d.body));
  } catch (e) {
    return "THROW:" + String(e).slice(0, 40);
  }
});

if (process.env.FASTBLANK === "1") (await import("./fast-blank.ts")).installFastBlank();
// NOBOUND lifts the bound out of reach, so a run measures the fast path on its own.
if (process.env.NOBOUND === "1") Object.assign(globalThis, { MAX_NESTING: "1000000000" });
const { installGuard, maxNesting } = await import("./nesting-guard.ts");
installGuard();

let diffs = 0;
let bytes = 0;
const dumpDir = process.env.DUMP;
if (dumpDir !== undefined) fs.mkdirSync(dumpDir, { recursive: true });
const changed: string[] = [];
for (const [index, doc] of docs.entries()) {
  bytes += doc.body.length;
  let after: string;
  try {
    after = JSON.stringify(fromMarkdown(doc.body));
  } catch (e) {
    after = "THROW:" + String(e).slice(0, 40);
  }
  const same = after === before[index];
  if (!same) {
    diffs++;
    if (diffs <= 5) console.log(`DIFF ${doc.name} (${doc.body.length} bytes)`);
  }
  if (dumpDir !== undefined) {
    fs.writeFileSync(path.join(dumpDir, `${doc.name}.md`), doc.body);
    changed.push(`${doc.name}\t${same ? "same" : "changed"}`);
  }
}
if (dumpDir !== undefined) fs.writeFileSync(path.join(dumpDir, "changed.tsv"), changed.join("\n") + "\n");
console.log(
  `bound=${maxNesting} docs=${docs.length} bytes=${bytes} corpus=100 diffs=${diffs}`
);
