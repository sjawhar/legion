const WS = "/legion/workspaces/sjawhar/legion/infra-625";
const B = `${WS}/node_modules/.bun`;
const { createHeadlessProof } = await import(`${WS}/packages/proof-editor/src/lib-headless.ts`);
const { parse, preprocess, postprocess } = await import(`${B}/micromark@4.0.2/node_modules/micromark/index.js`);
const { fromMarkdown } = await import(`${B}/mdast-util-from-markdown@2.0.3/node_modules/mdast-util-from-markdown/index.js`);
const { gen } = await import("./gen.mjs");
const fs = await import("node:fs");
const blockSchema = JSON.parse(fs.readFileSync(`${WS}/packages/envoy/internal/dispatch/pmdoc/schema/blocks.json`, "utf8"));
const ro = await createHeadlessProof({ blockSchema, readOnly: true });
const layers: Record<string, (b: string) => unknown> = {
  mm: (b) => postprocess(parse({}).document().write(preprocess()(b, undefined, true))),
  mdast: (b) => fromMarkdown(b),
  ro: (b) => ro.parseMarkdown(b),
};
const want = (process.env.LAYERS ?? "mm,mdast,ro").split(",");
for (const l of want) layers[l]("- a\n> b\n\nx [y](z) *w*\n".repeat(300));
for (const spec of process.argv.slice(2)) {
  const [name, n] = spec.split(":");
  const body = gen[name](Number(n));
  const row = [`${spec}(${body.length})`.padEnd(26)];
  for (const l of want) {
    let best = Infinity, err = "";
    for (let i = 0; i < Number(process.env.REPS ?? 2); i++) { const t = performance.now(); try { layers[l](body); } catch (e) { err = "!" + String(e).slice(0, 40); } best = Math.min(best, performance.now() - t); }
    row.push(`${l}=${best.toFixed(0)}${err}`);
  }
  console.log(row.join("  "));
}
