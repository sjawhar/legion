#!/usr/bin/env bun
// Stand-in for `gh`, and for every `legion` subcommand that talks to GitHub, on a skill scenario's
// PATH. Each call is recorded to $SKILL_SCENARIO_RUN/calls.jsonl (argv, and the contents of any
// file or stdin it hands GitHub a body through), then answered from the first route in
// $SKILL_SCENARIO_RUN/fixtures.json whose regex matches "<as> <argv...>". A call no route matches
// answers `gh: Not Found (HTTP 404)` and exits 1. Nothing is sent anywhere. `--json a,b` picks
// fields and `--jq`/`-q` runs jq, as gh does.
//   gh-standin.ts <gh|legion> <args...>
import { appendFileSync, readFileSync } from "node:fs";

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
for (let index = 0; index < args.length; index += 1) {
  const arg = args[index] ?? "";
  const next = args[index + 1];
  if (!next) continue;
  if (arg === "--input" || arg === "--body-file") files[next] = await readSource(next);
  else if (arg === "-F" || arg === "--field") {
    const value = next.split("=").slice(1).join("=");
    if (value.startsWith("@")) files[value.slice(1)] = await readSource(value.slice(1));
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
