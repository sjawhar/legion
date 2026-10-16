import { afterAll, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// Stage 4b's controller checkpoints read the controller's Oh My Pi sessions for the Envoy deliveries
// they hold: controller_deliveries [NEEDLE] prints the session lines that are envoy-message custom
// messages, those holding NEEDLE when one is given, in file order, and controller_received NEEDLE is
// true when there is one. The script runs under set -Eeuo pipefail, where a grep that matches
// nothing exits 1 and would end the run, so a scan that finds no delivery must print nothing and
// exit 0. Both run as the script defines them, under the same options, on session files shaped like
// Oh My Pi's: one compact JSON object per line, so the grep's '"customType":"envoy-message"' is the
// text a delivery's line holds.
const scriptPath = join(import.meta.dir, "..", "stage4b-sandbox-tree.sh");
const fn = scriptFunctions(scriptPath);
const dir = mkdtempSync(join(tmpdir(), "stage4b-controller-deliveries-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

/** A delivery's session line: pi-envoy's custom message, its TOON card as the content. */
const delivery = (id: string, summary: string, kind: string) =>
  JSON.stringify({
    type: "custom_message",
    customType: "envoy-message",
    id,
    content: `Envoy message\n  summary: ${summary}\n  message:\n    kind: ${kind}`,
  });
/** What the agent itself said, holding a needle's text: never a delivery. */
const said = JSON.stringify({
  type: "message",
  message: { role: "assistant", content: [{ type: "text", text: "summary: tick on LEGSMOKE" }] },
});
const aTick = delivery("a-1", "tick on LEGSMOKE", "tick");
const aHeld = delivery("a-2", "held on LEGSMOKE-3", "held");
const bHeld = delivery("b-1", "held on LEGSMOKE-3", "held");
const bTick = delivery("b-2", "tick on LEGSMOKE", "tick");
const profile = join(dir, "profile");
const bare = join(dir, "bare");
mkdirSync(join(profile, "sessions", "a-id"), { recursive: true });
mkdirSync(join(profile, "sessions", "b-id"), { recursive: true });
mkdirSync(bare);
writeFileSync(
  join(profile, "sessions", "a-id", "a.jsonl"),
  `${[aTick, said, aHeld, "not json {"].join("\n")}\n`
);
writeFileSync(
  join(profile, "sessions", "b-id", "b.jsonl"),
  `${[said, bHeld, "not json {", bTick].join("\n")}\n`
);

interface Run {
  readonly code: number;
  readonly out: string;
}
/** Runs BODY after the two functions, under the script's options, with profile_agent exported. */
function run(body: string, profileAgent = profile): Run {
  const result = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
${fn("controller_deliveries")}
${fn("controller_received")}
${body}`,
    ],
    { env: { ...process.env, profile_agent: profileAgent } }
  );
  expect(result.stderr.toString()).toBe("");
  return { code: result.exitCode, out: result.stdout.toString() };
}

describe("controller_deliveries", () => {
  test("prints every delivery line in file order, nothing the agent said or that is not JSON", () => {
    expect(run("controller_deliveries")).toEqual({
      code: 0,
      out: `${[aTick, aHeld, bHeld, bTick].join("\n")}\n`,
    });
  });

  test("with a needle, prints the deliveries holding it", () => {
    expect(run('controller_deliveries "summary: tick on LEGSMOKE"')).toEqual({
      code: 0,
      out: `${[aTick, bTick].join("\n")}\n`,
    });
  });

  test("with a needle no delivery holds, prints nothing and exits 0", () => {
    expect(run('controller_deliveries "summary: todo on LEGSMOKE-9"')).toEqual({
      code: 0,
      out: "",
    });
  });

  test("takes a needle's double quotes literally", () => {
    expect(run(`controller_deliveries '"customType":"envoy-message"'`)).toEqual({
      code: 0,
      out: `${[aTick, aHeld, bHeld, bTick].join("\n")}\n`,
    });
  });

  test("with no session at all, prints nothing and exits 0", () => {
    expect(run("controller_deliveries", bare)).toEqual({ code: 0, out: "" });
  });
});

describe("controller_received", () => {
  test("is true when a delivery holds the needle", () => {
    expect(run('controller_received "summary: held on LEGSMOKE-3"')).toEqual({ code: 0, out: "" });
  });

  test("is false when no delivery holds the needle, and when there is no session", () => {
    expect(run('controller_received "summary: todo on LEGSMOKE-9"')).toEqual({ code: 1, out: "" });
    expect(run('controller_received "summary: tick on LEGSMOKE"', bare)).toEqual({
      code: 1,
      out: "",
    });
  });
});
