#!/usr/bin/env bun
// Stand-in for `gh`, and for every `legion` subcommand that talks to GitHub, on a skill scenario's
// PATH. Each call is recorded to $SKILL_SCENARIO_RUN/calls.jsonl (argv, and the contents of any
// file or stdin it hands GitHub a body through), then answered from the first route in
// $SKILL_SCENARIO_RUN/fixtures.json whose regex matches "<as> <argv...>". A call no route matches
// answers `gh: Not Found (HTTP 404)` and exits 1. Nothing is sent anywhere. `--json a,b` picks
// fields and `--jq`/`-q` runs jq, as gh does. A pull request body edit (`gh pr edit --body`,
// `--body-file`, or a PATCH through `gh api`) is kept: every route answering with an object that
// has a `body` then answers with the new one, as GitHub's next read would.
//   gh-standin.ts <gh|legion> <args...>
import { appendFileSync, readFileSync, writeFileSync } from "node:fs";

interface Route {
  readonly match: string;
  readonly stdout?: unknown;
  readonly stderr?: string;
  readonly exit?: number;
}

const run = process.env.SKILL_SCENARIO_RUN;
if (!run) {
  console.error("gh-standin: SKILL_SCENARIO_RUN is unset");
  process.exit(2);
}
const [as = "gh", ...args] = Bun.argv.slice(2);
const fixtures: unknown = JSON.parse(readFileSync(`${run}/fixtures.json`, "utf8"));
if (
  typeof fixtures !== "object" ||
  fixtures === null ||
  !("routes" in fixtures) ||
  !Array.isArray(fixtures.routes)
) {
  console.error(`gh-standin: ${run}/fixtures.json has no routes array`);
  process.exit(2);
}
// fixture.sh writes each route as {match, stdout?, stderr?, exit?}.
const routes: readonly Route[] = fixtures.routes;

let stdin: string | undefined;
const readStdin = async () => {
  stdin ??= await new Response(Bun.stdin.stream()).text();
  return stdin;
};
const files: Record<string, string> = {};
const readSource = async (source: string) =>
  source === "-" ? readStdin() : readFileSync(source, "utf8");
// A pull request body edit: `gh pr edit` takes it from `-b/--body` or `-F/--body-file`, a
// `gh api -X PATCH repos/<o>/<r>/pulls/<n>` from a `body` field or an `--input` JSON object.
const prEdit = as === "gh" && args[0] === "pr" && args[1] === "edit";
const prPatch =
  as === "gh" &&
  args[0] === "api" &&
  /(^|\s)(-X|--method)\s?PATCH(\s|$)/.test(args.join(" ")) &&
  args.some((arg) => /^\/?repos\/[^/]+\/[^/]+\/pulls\/\d+$/.test(arg));
let body: string | undefined;
for (let index = 0; index < args.length; index += 1) {
  const arg = args[index] ?? "";
  const next = args[index + 1];
  if (!next) continue;
  if (prEdit && (arg === "-b" || arg === "--body")) body = next;
  else if (prEdit && (arg === "-F" || arg === "--body-file")) {
    files[next] = await readSource(next);
    body = files[next];
  } else if (arg === "--input" || arg === "--body-file") files[next] = await readSource(next);
  else if (arg === "-F" || arg === "--field" || arg === "-f" || arg === "--raw-field") {
    const [key = "", ...rest] = next.split("=");
    let value = rest.join("=");
    if ((arg === "-F" || arg === "--field") && value.startsWith("@")) {
      files[value.slice(1)] = await readSource(value.slice(1));
      value = files[value.slice(1)] ?? "";
    }
    if (prPatch && key === "body") body = value;
  }
}
const input = args[args.indexOf("--input") + 1];
if (prPatch && args.includes("--input") && input !== undefined) {
  const parsed: unknown = JSON.parse(files[input] ?? "null");
  if (typeof parsed === "object" && parsed !== null && "body" in parsed) {
    if (typeof parsed.body === "string") body = parsed.body;
  }
}
const joined = `${as} ${args.join(" ")}`;
const route = routes.find((candidate) => new RegExp(candidate.match, "s").test(joined));
appendFileSync(
  `${run}/calls.jsonl`,
  `${JSON.stringify({ at: new Date().toISOString(), as, argv: args, files, stdin, route: route?.match ?? null })}\n`
);
if (!route) {
  console.error(as === "gh" ? "gh: Not Found (HTTP 404)" : `unknown command: ${args.join(" ")}`);
  process.exit(1);
}
if (body !== undefined) {
  const edited = routes.map((candidate) =>
    typeof candidate.stdout === "object" && candidate.stdout !== null && "body" in candidate.stdout
      ? { ...candidate, stdout: { ...candidate.stdout, body } }
      : candidate
  );
  writeFileSync(`${run}/fixtures.json`, JSON.stringify({ ...fixtures, routes: edited }));
}
let output: unknown = route.stdout ?? "";
const jsonIndex = args.indexOf("--json");
if (jsonIndex >= 0 && typeof output === "object" && output !== null && !Array.isArray(output)) {
  const record: Record<string, unknown> = { ...output };
  const fields = (args[jsonIndex + 1] ?? "").split(",");
  output = Object.fromEntries(
    fields.filter((field) => field in record).map((field) => [field, record[field]])
  );
}
let text = typeof output === "string" ? output : JSON.stringify(output);
const jqIndex = args.findIndex((arg) => arg === "--jq" || arg === "-q");
const filter = jqIndex >= 0 ? args[jqIndex + 1] : undefined;
if (filter !== undefined) {
  const jq = Bun.spawnSync(["jq", "-r", filter], { stdin: new TextEncoder().encode(text) });
  text = jq.stdout.toString();
  if (jq.exitCode !== 0) {
    process.stderr.write(jq.stderr.toString());
    process.exit(1);
  }
}
if (text !== "") process.stdout.write(text.endsWith("\n") ? text : `${text}\n`);
if (route.stderr) process.stderr.write(route.stderr);
process.exit(route.exit ?? 0);
