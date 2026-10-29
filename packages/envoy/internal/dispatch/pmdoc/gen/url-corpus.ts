// url-corpus.ts - a corpus of bare-URL shapes for the pmdoc differential (differential.sh).
//
//   bun url-corpus.ts <out.jsonl> [count]
//
// Writes count (default 8000) distinct one-block documents as {"id", "md"} lines, each a URL
// built from a scheme, a host, a path and trailing punctuation, with a character before it and
// text after it, placed in a paragraph, a table cell, bold, a quote, a list item or between words;
// then a quarter as many with the URL written as an explicit link to itself (a www URL to
// `http://` and itself), which holds trees a bare URL cannot make, such as a www link right after
// a code span or inline HTML. The same arguments write the same file.
import { writeFileSync } from "node:fs";

const [output, countArgument] = process.argv.slice(2);
if (!output) {
  console.error("usage: bun url-corpus.ts <out.jsonl> [count]");
  process.exit(2);
}
const count = Number(countArgument ?? 8000);

let state = 20261021;
const pick = <T>(values: readonly T[]): T => {
  state = (Math.imul(state, 1103515245) + 12345) >>> 0;
  return values[state % values.length] as T;
};

const befores = [
  "",
  " ",
  "(",
  '"',
  "'",
  "[",
  "*",
  "_",
  "~",
  ":",
  "=",
  "x",
  "1",
  "**",
  "`c` ",
  "<b>",
  "\\",
  ",",
  ".",
  ";",
  "#",
  "&amp;",
  "x;",
  "\\*",
  "\\(",
  "é",
  "—",
  "]",
  "|",
  "!",
];
const schemes = [
  "https://",
  "http://",
  "www.",
  "HTTPS://",
  "Www.",
  "ftp://",
  "https:/",
  "http\\://",
  "www\\.",
];
const hosts = [
  "a.example",
  "api.github.com",
  "a_b.c.d",
  "x.y_z",
  "localhost",
  "-a.example",
  "例え.jp",
  "a.example:8080",
  "x@y.com",
  "a.b.c_d",
  "a'b@c.test",
  "a!b@c.test",
  "a@b.test1",
];
const paths = [
  "",
  "/x",
  "/graphql",
  "/x(y)",
  "/x_y",
  "/a\\b",
  "/a*b",
  "/q?a=1&b=2",
  "/[a]",
  "/x&amp;y",
  "/café",
  "/a~b",
  "/a|b",
  "/x.y",
  "/a`b",
];
const trails = [
  "",
  ")",
  '"',
  ')"',
  "'",
  ".",
  ",",
  ";",
  ":",
  "!",
  "?",
  "_",
  "*",
  "~",
  "]",
  "](",
  ")).",
  "&amp;",
  "&amp;)",
  '".',
  "<b>",
  "'_",
  "]x",
  ")x",
  '"y',
  "\\)",
  '\\"',
];
const afters = ["", " b", " and more", "*", "**"];
const forms = ["{}", "| a |\n| --- |\n| {} |", "**{}**", "> {}", "- {}", "a {} b"];

const seen = new Set<string>();
const lines: string[] = [];
for (let attempt = 0; lines.length < count && attempt < count * 10; attempt++) {
  const text =
    pick(befores) + pick(schemes) + pick(hosts) + pick(paths) + pick(trails) + pick(afters);
  const markdown = `${pick(forms).replace("{}", text)}\n`;
  if (seen.has(markdown)) continue;
  seen.add(markdown);
  lines.push(JSON.stringify({ id: `url-${lines.length}`, md: markdown }));
}
const linkAfters = [...afters, "é", "\\b", "|x", ".x"];
const explicit = lines.length + Math.floor(count / 4);
for (let attempt = 0; lines.length < explicit && attempt < count * 10; attempt++) {
  const url = pick(schemes) + pick(hosts) + pick(paths);
  const href = /^www\\?\./i.test(url) ? `http://${url}` : url;
  const text = `${pick(befores)}[${url}](<${href}>)${pick(linkAfters)}`;
  const markdown = `${pick(forms).replace("{}", text)}\n`;
  if (seen.has(markdown)) continue;
  seen.add(markdown);
  lines.push(JSON.stringify({ id: `link-${lines.length}`, md: markdown }));
}
writeFileSync(output, `${lines.join("\n")}\n`);
