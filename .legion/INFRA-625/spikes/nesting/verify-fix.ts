// The production module from fix/b, over the same documents: it must cut exactly where the
// measured prototype cuts (boundary-docs/changed.tsv) and change nothing Go accepts.
const WS = "/legion/workspaces/sjawhar/legion/infra-625";
const B = `${WS}/node_modules/.bun`;
const { fromMarkdown } = await import(
  `${B}/mdast-util-from-markdown@2.0.3/node_modules/mdast-util-from-markdown/index.js`
);
const fs = await import("node:fs");

const dirs = ["boundary-docs", "docs"];
const before = new Map<string, string>();
for (const dir of dirs) {
  for (const line of fs.readFileSync(`${dir}/changed.tsv`, "utf8").trim().split("\n")) {
    const [name] = line.split("\t");
    before.set(`${dir}/${name}`, JSON.stringify(fromMarkdown(fs.readFileSync(`${dir}/${name}.md`, "utf8"))));
  }
}

const { boundBlockNesting, maxBlockNesting } = await import(
  "./fix/b/packages/proof-editor/src/block-nesting.ts"
);
boundBlockNesting();

let mismatch = 0;
let changed = 0;
for (const dir of dirs) {
  for (const line of fs.readFileSync(`${dir}/changed.tsv`, "utf8").trim().split("\n")) {
    const [name, verdict] = line.split("\t");
    const body = fs.readFileSync(`${dir}/${name}.md`, "utf8");
    const got = JSON.stringify(fromMarkdown(body)) === before.get(`${dir}/${name}`) ? "same" : "changed";
    if (got === "changed") changed++;
    if (got !== verdict) {
      mismatch++;
      if (mismatch <= 5) console.log(`MISMATCH ${dir}/${name}: prototype=${verdict} production=${got}`);
    }
  }
}
console.log(`production module: bound=${maxBlockNesting} docs=${before.size} changed=${changed} mismatch=${mismatch}`);
