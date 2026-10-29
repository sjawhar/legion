// Scaling bench for micromark core + fromMarkdown over a package root.
// usage: node bench.mjs <overlayRoot> <shape:n> ...
const root = process.argv[2];
const mm = await import(`${root}/micromark@4.0.2/node_modules/micromark/index.js`);
const { fromMarkdown } = await import(
  `${root}/mdast-util-from-markdown@2.0.3/node_modules/mdast-util-from-markdown/index.js`
);
const { gen } = await import("/legion/scratch/tmp/infra625/gen.mjs");

const core = (b) =>
  mm.postprocess(mm.parse({}).document().write(mm.preprocess()(b, undefined, true)));
const layers = { mm: core, mdast: (b) => fromMarkdown(b) };

const want = (process.env.LAYERS ?? "mm,mdast").split(",");
const reps = Number(process.env.REPS ?? 3);
for (let i = 0; i < 5; i++) for (const l of want) layers[l]("- a\n> b\n\nx [y](z) *w*\n".repeat(200));

for (const spec of process.argv.slice(3)) {
  const [name, n] = spec.split(":");
  const body = gen[name](Number(n));
  const row = [`${spec}`.padEnd(22), String(body.length).padStart(6)];
  for (const l of want) {
    let best = Infinity, err = "";
    for (let i = 0; i < reps; i++) {
      const t = performance.now();
      try { layers[l](body); } catch (e) { err = "!" + String(e).slice(0, 30); }
      best = Math.min(best, performance.now() - t);
    }
    row.push(`${l}=${best.toFixed(0)}${err}`.padEnd(14));
  }
  console.log(row.join(" "));
}
