// Replay real conflicting merges under candidate layouts.
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import * as prettier from "prettier";

const repo = "/legion/scratch/tmp/p631/repo";
const git = (args, input) =>
  execFileSync("git", ["-C", repo, ...args], { input, maxBuffer: 1 << 28 }).toString();
const gitStatus = (args) => {
  try {
    return { code: 0, out: git(args) };
  } catch (e) {
    return { code: e.status, out: e.stdout.toString() };
  }
};

const ABBR = /(?:^|[^\p{L}\p{N}])(?:e\.g|i\.e|etc|vs|ex|eg|ie|cf|Fig|No)\.$/u;

function prettierFmt(text) {
  return prettier.format(text, {
    parser: "markdown",
    proseWrap: "never",
    plugins: ["prettier-plugin-sentences-per-line"],
  });
}

// Split a line at sentence ends followed by a code span, bold, digit, quote or link.
function extraSplit(text) {
  const out = [];
  let fence = false;
  for (const line of text.split("\n")) {
    if (/^\s*(```|~~~)/.test(line)) fence = !fence;
    if (fence || /^\s*(\||#)/.test(line)) {
      out.push(line);
      continue;
    }
    const indent = /^(\s*(?:[-*+]|\d+\.)\s+|\s*)/.exec(line)[0];
    const contIndent = " ".repeat(indent.length);
    let parts = [];
    let start = 0;
    let inCode = false;
    for (let i = 0; i < line.length; i++) {
      const c = line[i];
      if (c === "`") {
        let j = i;
        while (line[j] === "`") j++;
        const ticks = line.slice(i, j);
        if (!inCode) {
          const close = line.indexOf(ticks, j);
          if (close === -1) break;
          i = close + ticks.length - 1;
          continue;
        }
      }
      if (/[.!?]/.test(c) && i >= indent.length) {
        let k = i + 1;
        while (k < line.length && /[)\]"'*_”’]/.test(line[k])) k++;
        if (line[k] === " " && /[`*\d"“\[(A-Z]/.test(line[k + 1] ?? "")) {
          const before = line.slice(start, i + 1);
          if (!ABBR.test(line.slice(0, i + 1)) && !/^\s*\d+\.$/.test(line.slice(0, i + 1))) {
            parts.push(line.slice(start, k));
            start = k + 1;
          }
        }
      }
    }
    parts.push(line.slice(start));
    out.push(parts.map((p, n) => (n === 0 ? p : contIndent + p)).join("\n"));
  }
  return out.join("\n");
}

// Blank line between consecutive prose lines of one paragraph (each sentence a paragraph).
function blankBetween(text) {
  const lines = text.split("\n");
  const out = [];
  let fence = false;
  const prose = (l) => l.trim() !== "" && !/^\s*(\||#|```|~~~)/.test(l);
  for (let i = 0; i < lines.length; i++) {
    const l = lines[i];
    if (/^\s*(```|~~~)/.test(l)) fence = !fence;
    out.push(l);
    const n = lines[i + 1];
    if (!fence && n !== undefined && prose(l) && prose(n) && !/^\s*([-*+]|\d+\.)\s/.test(n)) out.push("");
  }
  return out.join("\n");
}


// Break long lines after clause punctuation (; : —) outside code spans.
function clauseSplit(text, limit = 160) {
  const out = [];
  let fence = false;
  for (const line of text.split("\n")) {
    if (/^\s*(```|~~~)/.test(line)) fence = !fence;
    if (fence || line.length <= limit || /^\s*(\||#)/.test(line)) { out.push(line); continue; }
    const indent = /^(\s*(?:[-*+]|\d+\.)\s+|\s*)/.exec(line)[0];
    const cont = " ".repeat(indent.length);
    const parts = []; let start = 0;
    for (let i = indent.length; i < line.length; i++) {
      const c = line[i];
      if (c === "`") { let j = i; while (line[j] === "`") j++; const close = line.indexOf(line.slice(i, j), j); if (close === -1) break; i = close + (j - i) - 1; continue; }
      if ((c === ";" || c === ":") && line[i + 1] === " " && i - start > 40) { parts.push(line.slice(start, i + 1)); start = i + 2; }
      else if (c === "—" && line[i - 1] === " " && line[i + 1] === " " && i - start > 40) { parts.push(line.slice(start, i - 1)); start = i; }
    }
    parts.push(line.slice(start));
    out.push(parts.map((p, n) => (n === 0 ? p : cont + p)).join("\n"));
  }
  return out.join("\n");
}
const transforms = {
  T4_plus_clause_split: async (t) => clauseSplit(extraSplit(await prettierFmt(t))),
  T0_identity: async (t) => t,
  T1_prettier_sembr: async (t) => prettierFmt(t),
  T2_plus_codespan_split: async (t) => extraSplit(await prettierFmt(t)),
  T3_plus_blank_between_sentences: async (t) => blankBetween(extraSplit(await prettierFmt(t))),
};

const events = readFileSync("/legion/scratch/tmp/p631/events.txt", "utf8").trim().split("\n");
const results = [];
const only = process.env.ONLY; const dump = process.env.DUMP; import("node:fs").then(()=>{});
const emptyTree = git(["hash-object", "-t", "tree", "--stdin"], "").trim();
for (const ev of events) {
  const [pr, m, p1, p2, file] = ev.split(" ");
  const base = git(["merge-base", p1, p2]).trim();
  const show = (c) => {
    const r = gitStatus(["show", `${c}:${file}`]);
    return r.code === 0 ? r.out : "";
  };
  const v = { base: show(base), ours: show(p1), theirs: show(p2) };
  const row = { pr, merge: m.slice(0, 10), file: file.endsWith("AGENTS.md") ? "AGENTS" : "README" };
  for (const [name, fn] of Object.entries(transforms)) {
    if (only && name !== only) continue;
    const commit = async (text, parent) => {
      const blob = git(["hash-object", "-w", "--stdin"], await fn(text)).trim();
      const tree = git(["mktree"], `100644 blob ${blob}\tf.md\n`).trim();
      return git(["commit-tree", tree, ...(parent ? ["-p", parent] : []), "-m", "x"]).trim();
    };
    const b = await commit(v.base);
    const o = await commit(v.ours, b);
    const t = await commit(v.theirs, b);
    const r = gitStatus(["merge-tree", "--write-tree", "--messages", o, t]);
    let hunks = 0;
    if (r.code !== 0) {
      const tree = r.out.split("\n")[0];
      const merged = git(["show", `${tree}:f.md`]);
      hunks = (merged.match(/^<<<<<<< /gm) || []).length;
      if (dump) (await import('node:fs')).writeFileSync(`${dump}/${pr}-${m.slice(0,8)}-${row.file}.md`, merged);
    }
    row[name] = r.code === 0 ? 0 : hunks;
  }
  results.push(row);
  console.log(JSON.stringify(row));
}
const sum = {};
for (const name of Object.keys(transforms)) {
  sum[name] = { conflictedEvents: results.filter((r) => r[name] > 0).length, hunks: results.reduce((a, r) => a + r[name], 0) };
}
console.log("SUMMARY", JSON.stringify(sum));
