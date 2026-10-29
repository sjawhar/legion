import { chromium } from "/legion/workspaces/sjawhar/legion/infra-625/node_modules/.bun/playwright-core@1.63.0/node_modules/playwright-core/index.mjs";
import http from "node:http";
import fs from "node:fs";
import path from "node:path";

const dir = new URL("./dist/", import.meta.url).pathname;
const types = { ".html": "text/html", ".js": "text/javascript" };
const server = http.createServer((req, res) => {
  const name = req.url === "/" ? "index.html" : decodeURIComponent(req.url.slice(1));
  const file = path.join(dir, name);
  if (!fs.existsSync(file)) {
    res.writeHead(404).end("no");
    return;
  }
  res.writeHead(200, { "content-type": types[path.extname(file)] ?? "text/plain" });
  fs.createReadStream(file).pipe(res);
});
await new Promise((r) => server.listen(0, "127.0.0.1", r));
const port = server.address().port;

const MAIN_SHAPES = (process.env.ONLY ?? "comment2k,mixed32k,list_words,list_plain,para_lines,nest100_blanks").split(",");
const SWEEP = process.env.NOSWEEP === "1" ? [] : [
  ...[1000, 2000, 4000, 8000, 16000, 32000].map((c) => `mixed_${c}`),
  ...[250, 500, 1000, 2000, 4000, 8000].map((n) => `list_${n}`),
];
const REPS = 4; // the first is a warm-up and is dropped

const median = (values) => {
  const sorted = [...values].sort((a, b) => a - b);
  const mid = sorted.length >> 1;
  return sorted.length % 2 === 1 ? sorted[mid] : (sorted[mid - 1] + sorted[mid]) / 2;
};
const summarise = (runs) => {
  const out = {};
  for (const key of Object.keys(runs[0])) {
    const values = runs.map((r) => r[key]);
    out[key] = typeof values[0] === "number" ? Math.round(median(values) * 10) / 10 : values[0];
  }
  return out;
};

const browser = await chromium.launch();
const page = await browser.newPage();
page.on("console", (m) => {
  if (m.type() === "error") console.log("  [console]", m.text().slice(0, 400));
});
page.on("pageerror", (e) => console.log("  [pageerror]", (e.stack ?? String(e)).slice(0, 1500)));
await page.goto(`http://127.0.0.1:${port}/`);
await page.waitForFunction(() => globalThis.ready === true, null, { timeout: 60000 });

console.log("BOOT", JSON.stringify(await page.evaluate(() => globalThis.bootInfo)));
console.log("MAINBOOT", JSON.stringify(await page.evaluate(() => globalThis.mainBoot)));

const repeat = async (fn, args) => {
  const runs = [];
  for (let i = 0; i < REPS; i++) runs.push(await page.evaluate(fn, args));
  if (runs.some((r) => r.error !== undefined)) return runs[runs.length - 1];
  return summarise(runs.slice(1));
};

for (const shape of MAIN_SHAPES) {
  for (const mode of ["obj", "str"]) {
    console.log("W", JSON.stringify(await repeat(([s, m]) => globalThis.runWorker(s, m), [shape, mode])));
  }
  console.log("M", JSON.stringify(await repeat((s) => globalThis.runMain(s), shape)));
}
console.log("--- sweep ---");
for (const shape of SWEEP) {
  console.log("W", JSON.stringify(await repeat(([s, m]) => globalThis.runWorker(s, m), [shape, "str"])));
  console.log("M", JSON.stringify(await repeat((s) => globalThis.runMain(s), shape)));
}
console.log("--- parity ---");
for (const shape of [...MAIN_SHAPES, "mixed_8000", "list_500"]) {
  console.log("P", JSON.stringify(await page.evaluate((s) => globalThis.compare(s), shape)));
}
await browser.close();
server.close();
